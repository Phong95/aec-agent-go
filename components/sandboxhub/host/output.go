package host

import (
	"io"
	"sync"

	"github.com/Phong95/aec-agent-go/components/sandboxhub"
	"github.com/Phong95/aec-agent-go/internal/viewkit"
)

// collector gathers the output of one command: stdout and stderr share one byte cap
// (maxOutput). The raw bytes of each stream are
// kept and decoded once at the end, and OnOutput gets the text as it arrives through
// one viewkit.Decoder per stream, so a character split across two pipe reads reaches
// OnOutput and the result whole, never as U+FFFD (H8).
//
// A chunk that does not fit is cut at the byte cap and then back to a character
// boundary, so no partial character decodes to U+FFFD. The collector then ignores
// everything, reports the cut through onCut and never counts a chunk that
// fits exactly as truncated: the next byte is what overflows.
//
// The two copy goroutines of os/exec call Write concurrently; one small mutex
// guards all state. OnOutput and onCut run after the mutex is released.
type collector struct {
	mu      sync.Mutex
	max     int
	written int
	halted  bool
	streams [2]outStream

	onOutput func(sandboxhub.OutputStream, string)
	onCut    func()
}

// outStream is the state of one of the two streams.
type outStream struct {
	name sandboxhub.OutputStream
	raw  []byte
	dec  viewkit.Decoder
}

const (
	stdoutIndex = 0
	stderrIndex = 1
)

// newCollector returns a collector with the byte cap max. onOutput may be nil;
// onCut is called once, after the chunk that overflowed was cut.
func newCollector(maxBytes int, onOutput func(sandboxhub.OutputStream, string), onCut func()) *collector {
	c := &collector{max: maxBytes, onOutput: onOutput, onCut: onCut}
	c.streams[stdoutIndex].name = sandboxhub.StreamStdout
	c.streams[stderrIndex].name = sandboxhub.StreamStderr
	return c
}

// streamWriter is the io.Writer of one stream. Write always reports the whole chunk
// as written, so the pipe keeps draining after the command was killed.
type streamWriter struct {
	c   *collector
	idx int
}

// stdout and stderr return the writers to hand to os/exec.
func (c *collector) stdout() io.Writer { return streamWriter{c, stdoutIndex} }
func (c *collector) stderr() io.Writer { return streamWriter{c, stderrIndex} }

func (w streamWriter) Write(p []byte) (int, error) {
	c := w.c
	c.mu.Lock()
	if c.halted {
		c.mu.Unlock()
		return len(p), nil
	}
	s := &c.streams[w.idx]
	room := max(c.max-c.written, 0)
	piece := p
	cut := len(p) > room
	if cut {
		piece = p[:room]
		c.halted = true
	}
	s.raw = append(s.raw, piece...)
	c.written += len(piece)
	text := s.dec.Write(piece)
	if cut {
		// Back off to a character boundary. The bytes dropped here are exactly the
		// ones the decoder holds back, so OnOutput and the result still agree.
		tail := viewkit.IncompleteTail(s.raw)
		s.raw = s.raw[:len(s.raw)-tail]
		c.written -= tail
	}
	c.mu.Unlock()

	if text != "" && c.onOutput != nil {
		c.onOutput(s.name, text)
	}
	if cut && c.onCut != nil {
		c.onCut()
	}
	return len(p), nil
}

// halt makes the collector ignore everything from now on (the command was killed).
func (c *collector) halt() {
	c.mu.Lock()
	c.halted = true
	c.mu.Unlock()
}

// finish flushes what the decoders still hold (one U+FFFD for a character cut off by
// the end of the stream) to OnOutput. It does nothing after a kill or a cut.
func (c *collector) finish() {
	type flush struct {
		name sandboxhub.OutputStream
		text string
	}
	var flushes []flush
	c.mu.Lock()
	if !c.halted {
		for i := range c.streams {
			if text := c.streams[i].dec.End(); text != "" {
				flushes = append(flushes, flush{c.streams[i].name, text})
			}
		}
	}
	c.mu.Unlock()
	if c.onOutput == nil {
		return
	}
	for _, f := range flushes {
		c.onOutput(f.name, f.text)
	}
}

// result returns the final stdout and stderr: the kept bytes decoded the way
// JavaScript does.
func (c *collector) result() (stdout, stderr string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return viewkit.LossyUTF8(c.streams[stdoutIndex].raw), viewkit.LossyUTF8(c.streams[stderrIndex].raw)
}
