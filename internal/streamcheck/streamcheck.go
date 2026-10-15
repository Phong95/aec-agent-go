// Package streamcheck is the shared suite for the optional "fs.stream" part of a
// sandbox view (A-124, sandboxhub.StreamView): raw bytes, byte ranges, the upload
// limit, exclusive writes and the path checks. Every provider that offers the
// capability runs it (in-memory, host, and the opt-in real Docker tests), as core/ts
// runs tests/unit/sandbox-streams.test.ts against its sandboxes.
//
// It is a non-test file that imports "testing" so that tests in any package can use it.
package streamcheck

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/Phong95/aec-agent-go/components/sandboxhub"
	"github.com/Phong95/aec-agent-go/kernel"
)

// binary has NUL, high bytes and both line endings: none may be changed in transit.
var binary = []byte{0, 1, 2, 0xff, 0xfe, 0x80, 10, 13, 0, 65}

// Run runs the suite; newView returns a fresh view of one agent (shared tenancy or
// own) for each case.
func Run(t *testing.T, newView func(t *testing.T) sandboxhub.View) {
	t.Helper()
	cases := []struct {
		name string
		run  func(t *testing.T, v sandboxhub.View, s sandboxhub.StreamView)
	}{
		{"round trip of binary bytes", roundTrip},
		{"one byte range", byteRange},
		{"an upload over maxBytes leaves nothing", tooLarge},
		{"an upload that breaks off leaves nothing", brokenUpload},
		{"exclusive refuses an existing file", exclusive},
		{"paths stay inside the roots", escapes},
		{"a missing file and a folder", missing},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			v := newView(t)
			s, ok := sandboxhub.AsStreamView(v)
			if !ok {
				t.Fatalf("view %s has no fs.stream (capabilities %v)", v.ID(), v.Capabilities())
			}
			c.run(t, v, s)
		})
	}
}

func readAll(t *testing.T, bs *sandboxhub.ByteStream) []byte {
	t.Helper()
	defer func() { _ = bs.Body.Close() }()
	data, err := io.ReadAll(bs.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return data
}

func wantCode(t *testing.T, err error, code kernel.SandboxErrorCode) {
	t.Helper()
	var se *kernel.SandboxError
	if !errors.As(err, &se) || se.Code != code {
		t.Fatalf("error = %v, want code %s", err, code)
	}
}

func ptr(n int64) *int64 { return &n }

func roundTrip(t *testing.T, v sandboxhub.View, s sandboxhub.StreamView) {
	ctx := context.Background()
	// Two reads of the body (a MultiReader) - the write must not depend on one chunk.
	body := io.MultiReader(bytes.NewReader(binary[:4]), bytes.NewReader(binary[4:]))
	res, err := s.WriteStream(ctx, "files/model.bin", body, sandboxhub.WriteStreamOptions{MaxBytes: 100})
	if err != nil {
		t.Fatal(err)
	}
	if res.Bytes != len(binary) || !res.Created || res.Path != v.WorkingDir()+"/files/model.bin" {
		t.Fatalf("write = %+v", res)
	}
	bs, err := s.ReadStream(ctx, "files/model.bin", sandboxhub.ReadStreamOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if bs.Size != int64(len(binary)) || bs.Offset != 0 || bs.Length != int64(len(binary)) {
		t.Fatalf("stream = %+v", bs)
	}
	if got := readAll(t, bs); !bytes.Equal(got, binary) {
		t.Fatalf("bytes = %v, want %v", got, binary)
	}
	st, err := v.Stat(ctx, "files/model.bin")
	if err != nil || st == nil || st.Size != len(binary) {
		t.Fatalf("stat = %+v, %v", st, err)
	}
}

func byteRange(t *testing.T, _ sandboxhub.View, s sandboxhub.StreamView) {
	ctx := context.Background()
	if _, err := s.WriteStream(ctx, "r.bin", bytes.NewReader(binary), sandboxhub.WriteStreamOptions{MaxBytes: 100}); err != nil {
		t.Fatal(err)
	}
	bs, err := s.ReadStream(ctx, "r.bin", sandboxhub.ReadStreamOptions{Offset: 3, Length: ptr(4)})
	if err != nil {
		t.Fatal(err)
	}
	if bs.Offset != 3 || bs.Length != 4 || bs.Size != int64(len(binary)) {
		t.Fatalf("stream = %+v", bs)
	}
	if got := readAll(t, bs); !bytes.Equal(got, binary[3:7]) {
		t.Fatalf("range = %v", got)
	}
	tail, err := s.ReadStream(ctx, "r.bin", sandboxhub.ReadStreamOptions{Offset: 8, Length: ptr(100)})
	if err != nil {
		t.Fatal(err)
	}
	if tail.Length != 2 {
		t.Fatalf("tail length = %d, want 2 (cut to the file)", tail.Length)
	}
	if got := readAll(t, tail); !bytes.Equal(got, binary[8:]) {
		t.Fatalf("tail = %v", got)
	}
}

func tooLarge(t *testing.T, v sandboxhub.View, s sandboxhub.StreamView) {
	ctx := context.Background()
	_, err := s.WriteStream(ctx, "big.bin", bytes.NewReader(append(binary, binary...)), sandboxhub.WriteStreamOptions{MaxBytes: 15})
	wantCode(t, err, kernel.SandboxCodeTooLarge)
	if st, _ := v.Stat(ctx, "big.bin"); st != nil {
		t.Fatalf("big.bin exists after a refused upload: %+v", st)
	}
	noTempFiles(t, v)
}

// noTempFiles fails when a write's temp file was left in the working directory.
func noTempFiles(t *testing.T, v sandboxhub.View) {
	t.Helper()
	hidden := true
	listing, err := v.List(context.Background(), ".", sandboxhub.ListOptions{IncludeHidden: &hidden})
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range listing.Entries {
		if strings.HasPrefix(e.Name, ".aec-write-") {
			t.Fatalf("a temp file was left behind: %s", e.Path)
		}
	}
}

// brokenReader fails after its bytes: an upload whose connection dropped.
type brokenReader struct{ r io.Reader }

func (b brokenReader) Read(p []byte) (int, error) {
	n, err := b.r.Read(p)
	if errors.Is(err, io.EOF) {
		return n, errors.New("the connection dropped")
	}
	return n, err
}

// brokenUpload: a body that fails must not leave a truncated file (or its temp file)
// behind - the Docker helper puts a file in place only after the end frame.
func brokenUpload(t *testing.T, v sandboxhub.View, s sandboxhub.StreamView) {
	ctx := context.Background()
	_, err := s.WriteStream(ctx, "broken.bin", brokenReader{bytes.NewReader(binary)}, sandboxhub.WriteStreamOptions{MaxBytes: 100})
	if err == nil {
		t.Fatal("an upload that broke off was accepted")
	}
	if st, _ := v.Stat(ctx, "broken.bin"); st != nil {
		t.Fatalf("broken.bin exists after an upload that broke off: %+v", st)
	}
	noTempFiles(t, v)
}

func exclusive(t *testing.T, v sandboxhub.View, s sandboxhub.StreamView) {
	ctx := context.Background()
	if _, err := v.WriteFile(ctx, "a.txt", "one", sandboxhub.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	_, err := s.WriteStream(ctx, "a.txt", strings.NewReader("two"), sandboxhub.WriteStreamOptions{MaxBytes: 10, Exclusive: true})
	wantCode(t, err, kernel.SandboxCodeConflict)
	if fc, _ := v.ReadFile(ctx, "a.txt", sandboxhub.ReadOptions{}); fc.Content != "one" {
		t.Fatalf("content = %q after a refused exclusive write", fc.Content)
	}
	res, err := s.WriteStream(ctx, "a.txt", strings.NewReader("two"), sandboxhub.WriteStreamOptions{MaxBytes: 10})
	if err != nil || res.Created {
		t.Fatalf("replace = %+v, %v", res, err)
	}
	if fc, _ := v.ReadFile(ctx, "a.txt", sandboxhub.ReadOptions{}); fc.Content != "two" {
		t.Fatalf("content = %q after the replace", fc.Content)
	}
}

func escapes(t *testing.T, _ sandboxhub.View, s sandboxhub.StreamView) {
	ctx := context.Background()
	_, err := s.ReadStream(ctx, "../../../etc/passwd", sandboxhub.ReadStreamOptions{})
	wantCode(t, err, kernel.SandboxCodePathEscape)
	_, err = s.WriteStream(ctx, "../escape.bin", bytes.NewReader(binary), sandboxhub.WriteStreamOptions{MaxBytes: 100})
	wantCode(t, err, kernel.SandboxCodePathEscape)
}

func missing(t *testing.T, v sandboxhub.View, s sandboxhub.StreamView) {
	ctx := context.Background()
	_, err := s.ReadStream(ctx, "nope.bin", sandboxhub.ReadStreamOptions{})
	wantCode(t, err, kernel.SandboxCodeNotFound)
	if _, err := v.WriteFile(ctx, "dir/x.txt", "x", sandboxhub.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	_, err = s.ReadStream(ctx, "dir", sandboxhub.ReadStreamOptions{})
	wantCode(t, err, kernel.SandboxCodeInvalidPath)
}
