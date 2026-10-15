package docker

import (
	"bytes"
	"io"
	"strings"
	"sync"
	"time"
)

// fakeProcess is a scripted docker client for the exec stream. Its stdout and stderr
// readers are channel-based, so a read blocks durably (inside a testing/synctest bubble
// too: the channels must be created inside the bubble, which newFakeProcess does when a
// test calls it from there). It records what the host did to it: the bytes written to
// stdin, whether stdin was closed, and how often Kill was called. Like a real client,
// Kill (and finish) ends both streams and lets Wait return.
type fakeProcess struct {
	out, err chan []byte
	done     chan struct{}
	finished sync.Once
	exit     *int

	stdin *fakeStdin

	mu     sync.Mutex
	kills  int
	onKill func()
}

func newFakeProcess() *fakeProcess {
	p := &fakeProcess{out: make(chan []byte), err: make(chan []byte), done: make(chan struct{})}
	p.stdin = &fakeStdin{closed: make(chan struct{})}
	return p
}

// emitStdout delivers one read of stdout; it returns when a reader took it (or the
// process finished).
func (p *fakeProcess) emitStdout(data []byte) {
	select {
	case p.out <- data:
	case <-p.done:
	}
}

// emitStderr delivers one read of stderr.
func (p *fakeProcess) emitStderr(data []byte) {
	select {
	case p.err <- data:
	case <-p.done:
	}
}

// finish ends both streams and lets Wait return with code (nil is a client killed by a
// signal). It is safe to call more than once; the first call wins.
func (p *fakeProcess) finish(code *int) {
	p.finished.Do(func() {
		p.exit = code
		close(p.done)
	})
}

// closeStreamsOnStdinClose makes the fake behave like the helper: when the host closes
// stdin the command is killed and the client exits, so the streams end at once.
func (p *fakeProcess) closeStreamsOnStdinClose(code int) {
	p.stdin.mu.Lock()
	defer p.stdin.mu.Unlock()
	p.stdin.onClose = func() { p.finish(&code) }
}

func (p *fakeProcess) Stdin() io.WriteCloser { return p.stdin }
func (p *fakeProcess) Stdout() io.Reader     { return &chanReader{ch: p.out, done: p.done} }
func (p *fakeProcess) Stderr() io.Reader     { return &chanReader{ch: p.err, done: p.done} }

func (p *fakeProcess) Wait() (*int, error) {
	<-p.done
	return p.exit, nil
}

func (p *fakeProcess) Kill() error {
	p.mu.Lock()
	p.kills++
	hook := p.onKill
	p.mu.Unlock()
	if hook != nil {
		hook()
	}
	p.finish(nil)
	return nil
}

func (p *fakeProcess) killCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.kills
}

// chanReader reads the chunks of a channel one Read at a time; a chunk larger than the
// buffer is returned over several reads. It reports EOF once the process finished.
type chanReader struct {
	ch   chan []byte
	done chan struct{}
	rest []byte
}

func (r *chanReader) Read(buf []byte) (int, error) {
	if len(r.rest) == 0 {
		select {
		case data := <-r.ch:
			r.rest = data
		case <-r.done:
			return 0, io.EOF
		}
	}
	n := copy(buf, r.rest)
	r.rest = r.rest[n:]
	return n, nil
}

// fakeStdin records writes and the close of the helper's stdin.
type fakeStdin struct {
	mu      sync.Mutex
	writes  bytes.Buffer
	isClose bool
	closed  chan struct{}
	onClose func()
}

func (s *fakeStdin) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.isClose {
		return 0, io.ErrClosedPipe
	}
	return s.writes.Write(p)
}

func (s *fakeStdin) Close() error {
	s.mu.Lock()
	if s.isClose {
		s.mu.Unlock()
		return nil
	}
	s.isClose = true
	hook := s.onClose
	close(s.closed)
	s.mu.Unlock()
	if hook != nil {
		hook()
	}
	return nil
}

// written is everything the host wrote to stdin so far.
func (s *fakeStdin) written() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.writes.String()
}

// wasClosed reports whether the host closed stdin.
func (s *fakeStdin) wasClosed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.isClose
}

// headerLine is the first stdin line (the helper's header) without its newline.
func (s *fakeStdin) headerLine() string {
	line, _, _ := strings.Cut(s.written(), "\n")
	return line
}

// replayTimeout bounds every wait of a replay, so a Go exec stream that never stops the
// command fails the test instead of hanging it.
const replayTimeout = 10 * time.Second
