package sandboxhub

import (
	"context"
	"io"
)

// ---------------------------------------------------------------------------
// Byte streams (A-124) - the optional "fs.stream" part of a View.
//
// ReadFile and WriteFile carry UTF-8 text with size caps, which is right for tools.
// A preview server needs raw bytes (images, fonts, wasm, large models), byte ranges
// (HTTP Range) and files far bigger than MaxReadBytes, and a chat upload needs to
// write them. A view with CapabilityFSStream implements StreamView; a caller checks
// with AsStreamView.
// ---------------------------------------------------------------------------

// CapabilityFSStream is ReadStream / WriteStream (StreamView).
const CapabilityFSStream Capability = "fs.stream"

// ReadStreamOptions picks the byte range of a ReadStream. Offset is the first byte
// (0 = the start); Length is the bytes to read at most (nil = to the end of the
// file). A range past the end is cut to the file.
type ReadStreamOptions struct {
	Offset int64  `json:"offset,omitempty"`
	Length *int64 `json:"length,omitempty"`
}

// ByteStream is an open file read as raw bytes. The caller must Close Body - that
// releases the file or the process behind it, also when it stops early. A Body that
// ends before Length bytes (the file shrank while read) returns an error, not EOF.
// Body is read under the ctx given to ReadStream: cancelling it stops the read.
type ByteStream struct {
	// Path is the canonical logical path.
	Path string `json:"path"`
	// Size is the size of the whole file in bytes.
	Size int64 `json:"size"`
	// ModifiedAt is epoch milliseconds.
	ModifiedAt int64 `json:"modifiedAt"`
	// Offset and Length are the range Body yields (after cutting it to the file).
	Offset int64         `json:"offset"`
	Length int64         `json:"length"`
	Body   io.ReadCloser `json:"-"`
}

// WriteStreamOptions are the options of a WriteStream.
type WriteStreamOptions struct {
	// MaxBytes is required: a body longer than this fails with "too_large" and
	// nothing is written.
	MaxBytes int64 `json:"maxBytes"`
	// CreateDirs creates missing parent directories. nil = true.
	CreateDirs *bool `json:"createDirs,omitempty"`
	// Exclusive fails with "conflict" when the file already exists (default: replace it).
	Exclusive bool `json:"exclusive,omitempty"`
}

// StreamView is the optional "fs.stream" part of a View.
type StreamView interface {
	// ReadStream reads a file as raw bytes, optionally one byte range.
	ReadStream(ctx context.Context, path string, opts ReadStreamOptions) (*ByteStream, error)
	// WriteStream writes raw bytes from body to a temporary name and renames it into
	// place, so readers never see a partial file; on any error (including too_large)
	// nothing is left behind.
	WriteStream(ctx context.Context, path string, body io.Reader, opts WriteStreamOptions) (WriteResult, error)
}

// AsStreamView returns v as a StreamView when it has the "fs.stream" capability.
func AsStreamView(v View) (StreamView, bool) {
	s, ok := v.(StreamView)
	if !ok {
		return nil, false
	}
	for _, c := range v.Capabilities() {
		if c == CapabilityFSStream {
			return s, true
		}
	}
	return nil, false
}
