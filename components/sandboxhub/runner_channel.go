package sandboxhub

import (
	"encoding/base64"
	"io"
	"sync"

	"github.com/Phong95/aec-agent-go/kernel"
)

// ---------------------------------------------------------------------------
// A-160: the bytes of one runner request (readStream, writeStream, connect), used on both
// ends of the connection. A ChannelWriter sends "data" frames and waits once
// RunnerStreamWindow of them are unanswered; a ChannelReader hands the frames it got to
// its consumer and answers each one with an "ack" when the consumer asks for the next, so
// a slow consumer slows the sender instead of piling bytes up in memory.
// ---------------------------------------------------------------------------

// ChannelSend (A-160) sends one frame of a channel (Type data, ack or end; Data for data);
// the channel's owner adds the request's ID.
type ChannelSend func(m RunnerMessage)

// ChannelReader (A-160) is the incoming bytes of a channel, as an io.ReadCloser. It is
// safe for concurrent use: frames arrive on one goroutine, the consumer reads on another.
type ChannelReader struct {
	ack     func()
	abandon func()

	mu    sync.Mutex
	wake  chan struct{}
	queue [][]byte
	cur   []byte
	// owed: a frame was handed out; it is answered when the consumer asks for more.
	owed   bool
	ended  bool
	err    error
	closed bool
}

// NewChannelReader returns a reader that calls ack to answer one frame (when the
// consumer takes the next one) and abandon when the consumer stops before the end
// (Close before EOF, or a failure).
func NewChannelReader(ack, abandon func()) *ChannelReader {
	if abandon == nil {
		abandon = func() {}
	}
	return &ChannelReader{ack: ack, abandon: abandon, wake: make(chan struct{}, 1)}
}

// Push takes a "data" frame (base64). Ignored after the end; a malformed frame fails
// the channel.
func (r *ChannelReader) Push(data string) {
	b, err := base64.StdEncoding.DecodeString(data)
	if err != nil {
		r.Fail(kernel.NewSandboxError("The runner sent malformed bytes", kernel.SandboxCodeEnvironmentUnavailable, ""))
		return
	}
	r.mu.Lock()
	if r.ended || r.err != nil || r.closed {
		r.mu.Unlock()
		return
	}
	r.queue = append(r.queue, b)
	r.mu.Unlock()
	r.notify()
}

// End takes an "end" frame: the consumer gets what is queued, then io.EOF.
func (r *ChannelReader) End() {
	r.mu.Lock()
	r.ended = true
	r.mu.Unlock()
	r.notify()
}

// Fail fails the channel: the consumer gets err after what is queued.
func (r *ChannelReader) Fail(err error) {
	r.mu.Lock()
	if !r.ended && r.err == nil {
		r.err = err
	}
	r.mu.Unlock()
	r.notify()
}

func (r *ChannelReader) notify() {
	select {
	case r.wake <- struct{}{}:
	default:
	}
}

// Read reads the next bytes; io.EOF after the end.
func (r *ChannelReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	for {
		r.mu.Lock()
		if r.closed {
			r.mu.Unlock()
			return 0, io.ErrClosedPipe
		}
		if len(r.cur) > 0 {
			n := copy(p, r.cur)
			r.cur = r.cur[n:]
			r.mu.Unlock()
			return n, nil
		}
		// The consumer asked for more: the sender may send another frame.
		if r.owed {
			r.owed = false
			r.mu.Unlock()
			r.ack()
			continue
		}
		if len(r.queue) > 0 {
			r.cur = r.queue[0]
			r.queue = r.queue[1:]
			r.owed = true
			r.mu.Unlock()
			continue
		}
		if r.err != nil {
			err := r.err
			r.closed = true
			r.mu.Unlock()
			r.abandon()
			return 0, err
		}
		if r.ended {
			r.mu.Unlock()
			return 0, io.EOF
		}
		r.mu.Unlock()
		<-r.wake
	}
}

// Close stops reading; before the end it abandons the channel.
func (r *ChannelReader) Close() error {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return nil
	}
	r.closed = true
	done := r.ended && len(r.queue) == 0 && len(r.cur) == 0
	r.mu.Unlock()
	r.notify()
	if !done {
		r.abandon()
	}
	return nil
}

// ChannelWriter (A-160) is the outgoing bytes of a channel, as an io.Writer. It is safe
// for concurrent use.
type ChannelWriter struct {
	send ChannelSend

	mu         sync.Mutex
	cond       *sync.Cond
	unanswered int
	err        error
	ended      bool
}

// NewChannelWriter returns a writer that sends its frames with send.
func NewChannelWriter(send ChannelSend) *ChannelWriter {
	w := &ChannelWriter{send: send}
	w.cond = sync.NewCond(&w.mu)
	return w
}

// Write sends p in frames, waiting for acks once the window is full.
func (w *ChannelWriter) Write(p []byte) (int, error) {
	written := 0
	for written < len(p) {
		end := min(written+RunnerStreamChunkBytes, len(p))
		w.mu.Lock()
		for w.unanswered >= RunnerStreamWindow && w.err == nil {
			w.cond.Wait()
		}
		if w.err != nil {
			err := w.err
			w.mu.Unlock()
			return written, err
		}
		if w.ended {
			w.mu.Unlock()
			return written, kernel.NewSandboxError("The channel already ended", kernel.SandboxCodeInvalidArgument, "")
		}
		w.unanswered++
		w.mu.Unlock()
		w.send(RunnerMessage{Type: RunnerMessageData, Data: base64.StdEncoding.EncodeToString(p[written:end])})
		written = end
	}
	return written, nil
}

// OnAck takes an "ack": the other end took a frame.
func (w *ChannelWriter) OnAck() {
	w.mu.Lock()
	if w.unanswered > 0 {
		w.unanswered--
	}
	w.mu.Unlock()
	w.cond.Broadcast()
}

// End sends "end": no more bytes.
func (w *ChannelWriter) End() {
	w.mu.Lock()
	if w.ended || w.err != nil {
		w.mu.Unlock()
		return
	}
	w.ended = true
	w.mu.Unlock()
	w.send(RunnerMessage{Type: RunnerMessageEnd})
}

// Fail stops the writer: waiting and later writes fail with err.
func (w *ChannelWriter) Fail(err error) {
	w.mu.Lock()
	if w.err == nil {
		w.err = err
	}
	w.mu.Unlock()
	w.cond.Broadcast()
}

// channelConn (A-160) is a "connect" tunnel: what is written goes out as frames,
// incoming frames are read. Close tells the far end (close) and stops both sides.
type channelConn struct {
	reader *ChannelReader
	writer *ChannelWriter
	once   sync.Once
	close  func()
}

func newChannelConn(send ChannelSend, closeFn func()) *channelConn {
	c := &channelConn{writer: NewChannelWriter(send)}
	c.reader = NewChannelReader(func() { send(RunnerMessage{Type: RunnerMessageAck}) }, nil)
	c.close = closeFn
	return c
}

func (c *channelConn) Read(p []byte) (int, error)  { return c.reader.Read(p) }
func (c *channelConn) Write(p []byte) (int, error) { return c.writer.Write(p) }

// Close closes the tunnel from this side.
func (c *channelConn) Close() error {
	c.once.Do(func() {
		c.writer.Fail(kernel.NewSandboxError("The connection closed", kernel.SandboxCodeCancelled, ""))
		_ = c.reader.Close()
		c.close()
	})
	return nil
}

// Push, End and Fail take the far end's frames (End: it closed: no more bytes, and
// nothing more can be sent).
func (c *channelConn) Push(data string) { c.reader.Push(data) }

func (c *channelConn) End() {
	c.reader.End()
	c.writer.Fail(kernel.NewSandboxError("The connection closed", kernel.SandboxCodeCancelled, ""))
}

func (c *channelConn) Fail(err error) {
	c.reader.Fail(err)
	c.writer.Fail(err)
}
