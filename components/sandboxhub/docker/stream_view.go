package docker

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"

	"github.com/Phong95/aec-agent-go/components/sandboxhub"
	"github.com/Phong95/aec-agent-go/internal/jsonx"
	"github.com/Phong95/aec-agent-go/kernel"
)

// ---------------------------------------------------------------------------
// Byte streams (A-124) - the helper's readraw and writeraw modes.
//
// readraw: one JSON request on stdin; stdout is "AEC-OK {json}\n" (path, size,
// modifiedAt, offset, length) followed by exactly length file bytes, or
// "AEC-ERR {json}\n". writeraw: a JSON header line on stdin, then the file's bytes as
// frames (a 4-byte big-endian length, then the bytes); a zero-length frame ends the
// upload and puts the file in place, stdin closing before it discards the bytes; the
// answer is one JSON line, like a file operation.
// ---------------------------------------------------------------------------

// Compile-time proof of the stream contract.
var _ sandboxhub.StreamView = (*view)(nil)

// rawHeaderMaxBytes bounds the helper's header line before the raw bytes.
const rawHeaderMaxBytes = 64 * 1024

// rawOutputMaxBytes bounds what is kept of docker's stderr (and writeraw's answer).
const rawOutputMaxBytes = 64 * 1024

type readRawRequest struct {
	Path   string   `json:"path"`
	Roots  []string `json:"roots"`
	Offset int64    `json:"offset"`
	Length *int64   `json:"length,omitempty"`
}

type writeRawHeader struct {
	Path       string   `json:"path"`
	Roots      []string `json:"roots"`
	MaxBytes   int64    `json:"maxBytes"`
	CreateDirs bool     `json:"createDirs"`
	Exclusive  bool     `json:"exclusive"`
}

// rawMeta is the JSON of readraw's AEC-OK line.
type rawMeta struct {
	Path       string  `json:"path"`
	Size       int64   `json:"size"`
	ModifiedAt float64 `json:"modifiedAt"`
	Offset     int64   `json:"offset"`
	Length     int64   `json:"length"`
}

// spawnRaw starts the helper in a raw-bytes mode. A start failure carries this
// sandbox's id.
func (s *Sandbox) spawnRaw(ctx context.Context, mode string) (Process, error) {
	if err := s.assertUsable(); err != nil {
		return nil, err
	}
	args := []string{"exec", "--interactive", "--user", s.p.settings.User, s.containerName, "node", helperPath, mode}
	proc, err := s.p.runner.Start(ctx, args)
	if err != nil {
		var se *kernel.SandboxError
		if errors.As(err, &se) && se.SandboxID == "" {
			return nil, kernel.NewSandboxError(se.Message, se.Code, s.id)
		}
		return nil, err
	}
	return proc, nil
}

// cappedBuffer keeps the first max bytes written to it and drops the rest.
type cappedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
	max int
}

func (c *cappedBuffer) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if room := c.max - c.buf.Len(); room > 0 {
		c.buf.Write(p[:min(len(p), room)])
	}
	return len(p), nil
}

func (c *cappedBuffer) String() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.buf.String()
}

// drainInto copies r into w in the background; the channel closes at EOF.
func drainInto(w io.Writer, r io.Reader) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = io.Copy(w, r)
	}()
	return done
}

// ReadStream reads a file as raw bytes; a container that is down is restarted once.
func (v *view) ReadStream(ctx context.Context, path string, opts sandboxhub.ReadStreamOptions) (*sandboxhub.ByteStream, error) {
	if err := v.begin(ctx, sandboxhub.CapabilityFSStream); err != nil {
		return nil, err
	}
	if opts.Offset < 0 || (opts.Length != nil && *opts.Length < 0) {
		return nil, v.fail("offset and length must not be negative", kernel.SandboxCodeInvalidArgument)
	}
	logical, err := v.logical(path)
	if err != nil {
		return nil, err
	}
	req := readRawRequest{Path: logical, Roots: v.roots, Offset: opts.Offset, Length: opts.Length}
	stream, err := v.openRaw(ctx, req)
	if errors.Is(err, errContainerDown) {
		if err := v.sb.recover(ctx); err != nil {
			return nil, err
		}
		stream, err = v.openRaw(ctx, req)
		if errors.Is(err, errContainerDown) {
			return nil, v.containerNotRunning(err)
		}
	}
	return stream, err
}

// openRaw is one docker exec of the helper in readraw mode; it returns once the
// header line arrived, with Body reading the rest of the helper's stdout.
func (v *view) openRaw(ctx context.Context, req readRawRequest) (*sandboxhub.ByteStream, error) {
	proc, err := v.sb.spawnRaw(ctx, "readraw")
	if err != nil {
		return nil, err
	}
	stderr := &cappedBuffer{max: rawOutputMaxBytes}
	stderrDone := drainInto(stderr, proc.Stderr())
	stop := context.AfterFunc(ctx, func() { _ = proc.Kill() })
	input, _ := jsonx.Marshal(req)
	_, _ = proc.Stdin().Write(input)
	_ = proc.Stdin().Close()

	out := bufio.NewReaderSize(proc.Stdout(), rawHeaderMaxBytes)
	line, readErr := out.ReadSlice('\n')
	abandon := func() {
		stop()
		_ = proc.Kill()
		_, _ = io.Copy(io.Discard, out)
		<-stderrDone
		_, _ = proc.Wait()
	}
	if readErr != nil {
		abandon()
		if ctx.Err() != nil {
			return nil, v.fail("Operation cancelled", kernel.SandboxCodeCancelled)
		}
		text := stderr.String()
		if containerDown.MatchString(text) {
			return nil, &containerDownError{message: firstLines(text)}
		}
		detail := firstLines(text)
		if detail == "" {
			detail = "no answer from the sandbox helper"
		}
		return nil, v.fail("The sandbox did not answer: "+detail, kernel.SandboxCodeEnvironmentUnavailable)
	}
	header := strings.TrimSuffix(string(line), "\n")
	switch {
	case strings.HasPrefix(header, "AEC-OK "):
		var meta rawMeta
		if err := json.Unmarshal([]byte(strings.TrimPrefix(header, "AEC-OK ")), &meta); err != nil {
			abandon()
			return nil, v.fail("The sandbox helper sent an unreadable header", kernel.SandboxCodeEnvironmentUnavailable)
		}
		return &sandboxhub.ByteStream{
			Path: meta.Path, Size: meta.Size, ModifiedAt: int64(meta.ModifiedAt),
			Offset: meta.Offset, Length: meta.Length,
			Body: &rawBody{ctx: ctx, r: out, proc: proc, stop: stop, stderrDone: stderrDone, left: meta.Length, length: meta.Length, sandboxID: v.sb.id},
		}, nil
	case strings.HasPrefix(header, "AEC-ERR "):
		abandon()
		return nil, helperError(strings.TrimPrefix(header, "AEC-ERR "), v.sb.id)
	default:
		abandon()
		return nil, v.fail("Unexpected output from the sandbox helper: "+truncateText(header, 200), kernel.SandboxCodeEnvironmentUnavailable)
	}
}

// rawBody is the Body of a readraw stream: exactly length bytes of the helper's
// stdout. Close kills the docker client when the reader stopped early.
type rawBody struct {
	ctx        context.Context //nolint:containedctx // the stream is read under the ctx of ReadStream, after it returned
	r          *bufio.Reader
	proc       Process
	stop       func() bool
	stderrDone <-chan struct{}
	left       int64
	length     int64
	sandboxID  string
	closeOnce  sync.Once
}

func (b *rawBody) Read(p []byte) (int, error) {
	if b.left <= 0 {
		return 0, io.EOF
	}
	if int64(len(p)) > b.left {
		p = p[:b.left]
	}
	n, err := b.r.Read(p)
	b.left -= int64(n)
	if err == nil {
		return n, nil
	}
	if b.ctx.Err() != nil {
		return n, kernel.NewSandboxError("Operation cancelled", kernel.SandboxCodeCancelled, b.sandboxID)
	}
	if errors.Is(err, io.EOF) && b.left > 0 {
		return n, kernel.NewSandboxError(
			fmt.Sprintf("The file ended after %d of %d bytes (it changed while being read)", b.length-b.left, b.length),
			kernel.SandboxCodeEnvironmentUnavailable, b.sandboxID)
	}
	if errors.Is(err, io.EOF) {
		return n, io.EOF
	}
	return n, err
}

func (b *rawBody) Close() error {
	b.closeOnce.Do(func() {
		b.stop()
		if b.left > 0 {
			_ = b.proc.Kill()
		}
		_, _ = io.Copy(io.Discard, b.r)
		<-b.stderrDone
		_, _ = b.proc.Wait()
	})
	return nil
}

// countingReader records the error of the body it reads, so a failing upload (the
// client went away) is told apart from the helper refusing more bytes.
type countingReader struct {
	r   io.Reader
	err error
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	if err != nil && !errors.Is(err, io.EOF) {
		c.err = err
	}
	return n, err
}

// Frames of a writeraw body (A-124): a 4-byte big-endian length, then the bytes; a
// zero-length frame ends the upload. The helper puts the file in place only after that
// end frame, so an upload that breaks off (the docker client killed on cancel, a body
// that fails) never leaves a truncated file behind.
const maxFrameBytes = 1 << 30

var endFrame [4]byte

// frameWriter writes each Write as frames (an empty one is skipped: it would read as the
// end frame).
type frameWriter struct{ w io.Writer }

func (f frameWriter) Write(p []byte) (int, error) {
	done := 0
	for done < len(p) {
		n := min(len(p)-done, maxFrameBytes)
		var prefix [4]byte
		binary.BigEndian.PutUint32(prefix[:], uint32(n)) //nolint:gosec // n <= maxFrameBytes
		if _, err := f.w.Write(prefix[:]); err != nil {
			return done, err
		}
		if _, err := f.w.Write(p[done : done+n]); err != nil {
			return done, err
		}
		done += n
	}
	return done, nil
}

// WriteStream writes body through the helper's writeraw mode. There is no restart and
// retry: the body can only be read once. A body that fails closes stdin without the end
// frame: the helper discards what it received and answers, and the body's error is
// what the caller gets.
func (v *view) WriteStream(ctx context.Context, path string, body io.Reader, opts sandboxhub.WriteStreamOptions) (sandboxhub.WriteResult, error) {
	if err := v.begin(ctx, sandboxhub.CapabilityFSStream); err != nil {
		return sandboxhub.WriteResult{}, err
	}
	if opts.MaxBytes <= 0 {
		return sandboxhub.WriteResult{}, v.fail("maxBytes must be a positive number", kernel.SandboxCodeInvalidArgument)
	}
	logical, err := v.logical(path)
	if err != nil {
		return sandboxhub.WriteResult{}, err
	}
	proc, err := v.sb.spawnRaw(ctx, "writeraw")
	if err != nil {
		return sandboxhub.WriteResult{}, err
	}
	stop := context.AfterFunc(ctx, func() { _ = proc.Kill() })
	defer stop()

	stdout := &cappedBuffer{max: rawOutputMaxBytes}
	stderr := &cappedBuffer{max: rawOutputMaxBytes}
	stdoutDone := drainInto(stdout, proc.Stdout())
	stderrDone := drainInto(stderr, proc.Stderr())

	source := &countingReader{r: body}
	pumped := make(chan struct{})
	go func() {
		defer close(pumped)
		w := proc.Stdin()
		defer func() { _ = w.Close() }()
		header, _ := jsonx.Marshal(writeRawHeader{
			Path: logical, Roots: v.roots, MaxBytes: opts.MaxBytes,
			CreateDirs: opts.CreateDirs == nil || *opts.CreateDirs, Exclusive: opts.Exclusive,
		})
		if _, err := w.Write(append(header, '\n')); err != nil {
			return // the helper is gone; its answer (or docker's stderr) says why
		}
		if _, err := io.Copy(frameWriter{w}, source); err != nil {
			return // no end frame: the helper discards the bytes (a failed body) or is gone
		}
		// The end frame: only now may the helper put the file in place.
		_, _ = w.Write(endFrame[:])
	}()

	<-stdoutDone
	<-stderrDone
	code, _ := proc.Wait()
	<-pumped

	if ctx.Err() != nil {
		return sandboxhub.WriteResult{}, v.fail("Operation cancelled", kernel.SandboxCodeCancelled)
	}
	if source.err != nil {
		return sandboxhub.WriteResult{}, v.fail("Could not read the upload: "+source.err.Error(), kernel.SandboxCodeCancelled)
	}
	raw, err := parseHelperResponse(RunResult{Code: code, Stdout: stdout.String(), Stderr: stderr.String()}, v.sb.id)
	if err != nil {
		return sandboxhub.WriteResult{}, err
	}
	var out sandboxhub.WriteResult
	err = v.decode(raw, &out)
	return out, err
}

func truncateText(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
