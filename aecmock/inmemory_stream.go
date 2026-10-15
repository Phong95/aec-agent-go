package aecmock

import (
	"context"
	"fmt"
	"io"
	"path"
	"strings"

	"github.com/Phong95/aec-agent-go/components/sandboxhub"
	"github.com/Phong95/aec-agent-go/kernel"
)

// Byte streams (A-124). "fs.stream" is opt-in (WithCapabilities): the defaults stay
// what the existing goldens expect. File contents are Go strings, which hold any
// bytes, so binary files round-trip as they are.

// Compile-time proof of the stream contract.
var _ sandboxhub.StreamView = (*inMemoryView)(nil)

// ReadStream returns the byte range of a file. The bytes are copied when the call
// is made, so a later write does not change what Body yields.
func (v *inMemoryView) ReadStream(ctx context.Context, p string, opts sandboxhub.ReadStreamOptions) (*sandboxhub.ByteStream, error) {
	v.sb.mu.Lock()
	defer v.sb.mu.Unlock()
	if err := v.begin(ctx, sandboxhub.CapabilityFSStream); err != nil {
		return nil, err
	}
	if opts.Offset < 0 || (opts.Length != nil && *opts.Length < 0) {
		return nil, v.fail("offset and length must not be negative", kernel.SandboxCodeInvalidArgument)
	}
	rp, err := v.resolve(p)
	if err != nil {
		return nil, err
	}
	node, ok := v.sb.store.get(rp)
	if !ok {
		return nil, v.fail(fmt.Sprintf("\"%s\" does not exist", p), kernel.SandboxCodeNotFound)
	}
	if node.kind != sandboxhub.EntryFile {
		return nil, v.fail(fmt.Sprintf("\"%s\" is a %s, not a file", p, node.kind), kernel.SandboxCodeInvalidPath)
	}
	size := int64(len(node.content))
	offset := min(opts.Offset, size)
	length := size - offset
	if opts.Length != nil {
		length = min(*opts.Length, length)
	}
	slice := node.content[offset : offset+length]
	return &sandboxhub.ByteStream{
		Path: rp, Size: size, ModifiedAt: node.mtime, Offset: offset, Length: length,
		Body: io.NopCloser(strings.NewReader(slice)),
	}, nil
}

// WriteStream reads body to the end (outside the sandbox mutex), then stores it like
// WriteFile; a body over MaxBytes stores nothing.
func (v *inMemoryView) WriteStream(ctx context.Context, p string, body io.Reader, opts sandboxhub.WriteStreamOptions) (sandboxhub.WriteResult, error) {
	v.sb.mu.Lock()
	err := v.begin(ctx, sandboxhub.CapabilityFSStream)
	v.sb.mu.Unlock()
	if err != nil {
		return sandboxhub.WriteResult{}, err
	}
	if opts.MaxBytes <= 0 {
		return sandboxhub.WriteResult{}, v.fail("maxBytes must be a positive number", kernel.SandboxCodeInvalidArgument)
	}
	data, err := io.ReadAll(io.LimitReader(body, opts.MaxBytes+1))
	if err != nil {
		return sandboxhub.WriteResult{}, v.fail("Could not read the upload: "+err.Error(), kernel.SandboxCodeCancelled)
	}
	if int64(len(data)) > opts.MaxBytes {
		return sandboxhub.WriteResult{}, v.fail(fmt.Sprintf("\"%s\" is larger than the limit of %d bytes", p, opts.MaxBytes), kernel.SandboxCodeTooLarge)
	}
	if ctx.Err() != nil {
		return sandboxhub.WriteResult{}, v.fail("Operation cancelled", kernel.SandboxCodeCancelled)
	}

	v.sb.mu.Lock()
	defer v.sb.mu.Unlock()
	rp, err := v.resolve(p)
	if err != nil {
		return sandboxhub.WriteResult{}, err
	}
	existing, exists := v.sb.store.get(rp)
	if exists && existing.kind != sandboxhub.EntryFile {
		return sandboxhub.WriteResult{}, v.fail(fmt.Sprintf("\"%s\" is a %s", p, existing.kind), kernel.SandboxCodeInvalidPath)
	}
	if exists && opts.Exclusive {
		return sandboxhub.WriteResult{}, v.fail(fmt.Sprintf("\"%s\" already exists", p), kernel.SandboxCodeConflict)
	}
	parent := path.Dir(rp)
	parentNode, parentExists := v.sb.store.get(parent)
	switch {
	case !parentExists:
		if opts.CreateDirs != nil && !*opts.CreateDirs {
			return sandboxhub.WriteResult{}, v.fail(fmt.Sprintf("Directory \"%s\" does not exist", parent), kernel.SandboxCodeNotFound)
		}
		v.sb.mkdirp(parent)
	case parentNode.kind != sandboxhub.EntryDirectory:
		return sandboxhub.WriteResult{}, v.fail(fmt.Sprintf("\"%s\" is not a directory", parent), kernel.SandboxCodeInvalidPath)
	}
	content := string(data)
	v.sb.store.set(rp, memNode{kind: sandboxhub.EntryFile, content: content, mtime: v.sb.tick()})
	return sandboxhub.WriteResult{Path: rp, Bytes: len(data), SHA256: sha256Hex(content), Created: !exists}, nil
}
