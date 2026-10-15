package host

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"

	"github.com/Phong95/aec-agent-go/components/sandboxhub"
	"github.com/Phong95/aec-agent-go/kernel"
)

// ---------------------------------------------------------------------------
// Byte streams (A-124) - ReadStream and WriteStream on the host, through the same
// two checks as every other operation (resolve, then an os.Root at the real root).
// ---------------------------------------------------------------------------

// Compile-time proof of the stream contract.
var _ sandboxhub.StreamView = (*view)(nil)

// streamChunkBytes is how much WriteStream copies between cancellation checks.
const streamChunkBytes = 1024 * 1024

// fileBody is the Body of a host ReadStream: exactly length bytes of f, from the
// offset ReadStream seeked to.
type fileBody struct {
	ctx       context.Context //nolint:containedctx // the stream is read under the ctx of ReadStream, after it returned
	f         *os.File
	left      int64
	length    int64
	sandboxID string
}

func (b *fileBody) Read(p []byte) (int, error) {
	if b.left <= 0 {
		return 0, io.EOF
	}
	if b.ctx.Err() != nil {
		return 0, kernel.NewSandboxError("Operation cancelled", kernel.SandboxCodeCancelled, b.sandboxID)
	}
	if int64(len(p)) > b.left {
		p = p[:b.left]
	}
	n, err := b.f.Read(p)
	b.left -= int64(n)
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

func (b *fileBody) Close() error { return b.f.Close() }

// ReadStream opens the file for raw reading at opts.Offset; the handle outlives the
// os.Root it was opened through (closing a Root does not close its files).
func (v *view) ReadStream(ctx context.Context, input string, opts sandboxhub.ReadStreamOptions) (*sandboxhub.ByteStream, error) {
	if err := v.begin(ctx, sandboxhub.CapabilityFSStream); err != nil {
		return nil, err
	}
	if opts.Offset < 0 || (opts.Length != nil && *opts.Length < 0) {
		return nil, v.fail("offset and length must not be negative", kernel.SandboxCodeInvalidArgument)
	}
	res, err := v.resolve(input)
	if err != nil {
		return nil, err
	}
	var out *sandboxhub.ByteStream
	err = inRoot(res.realRoot, res.realPath, func(root *os.Root, rel string) error {
		f, err := root.Open(rel)
		if err != nil {
			return err
		}
		st, err := f.Stat()
		if err != nil {
			_ = f.Close()
			return err
		}
		if !st.Mode().IsRegular() {
			_ = f.Close()
			return v.fail(fmt.Sprintf("\"%s\" is a %s, not a file", input, entryTypeOf(st.Mode())), kernel.SandboxCodeInvalidPath)
		}
		size := st.Size()
		offset := min(opts.Offset, size)
		length := size - offset
		if opts.Length != nil {
			length = min(*opts.Length, length)
		}
		if _, err := f.Seek(offset, io.SeekStart); err != nil {
			_ = f.Close()
			return err
		}
		out = &sandboxhub.ByteStream{
			Path: res.logical, Size: size, ModifiedAt: st.ModTime().UnixMilli(), Offset: offset, Length: length,
			Body: &fileBody{ctx: ctx, f: f, left: length, length: length, sandboxID: v.sb.id},
		}
		return nil
	})
	if err != nil {
		return nil, mapFSError(err, input, v.sb.id)
	}
	return out, nil
}

// WriteStream copies body to a temporary file next to the target and renames it
// into place; any failure removes the temporary file.
func (v *view) WriteStream(ctx context.Context, input string, body io.Reader, opts sandboxhub.WriteStreamOptions) (sandboxhub.WriteResult, error) {
	if err := v.begin(ctx, sandboxhub.CapabilityFSStream); err != nil {
		return sandboxhub.WriteResult{}, err
	}
	if opts.MaxBytes <= 0 {
		return sandboxhub.WriteResult{}, v.fail("maxBytes must be a positive number", kernel.SandboxCodeInvalidArgument)
	}
	res, err := v.resolve(input)
	if err != nil {
		return sandboxhub.WriteResult{}, err
	}
	var (
		created bool
		written int64
		sum     string
	)
	err = inRoot(res.realRoot, res.realPath, func(root *os.Root, rel string) error {
		existing, err := root.Lstat(rel)
		if err != nil && isPathEscape(err) {
			return err
		}
		exists := err == nil
		if exists && !existing.Mode().IsRegular() {
			return v.fail(fmt.Sprintf("\"%s\" is a %s", input, entryTypeOf(existing.Mode())), kernel.SandboxCodeInvalidPath)
		}
		if exists && opts.Exclusive {
			return v.fail(fmt.Sprintf("\"%s\" already exists", input), kernel.SandboxCodeConflict)
		}
		parent := filepath.Dir(rel)
		parentStat, err := root.Stat(parent)
		switch {
		case err != nil && isPathEscape(err):
			return err
		case err != nil:
			if opts.CreateDirs != nil && !*opts.CreateDirs {
				return v.fail(fmt.Sprintf("Directory \"%s\" does not exist", path.Dir(res.logical)), kernel.SandboxCodeNotFound)
			}
			if err := root.MkdirAll(parent, dirPerm); err != nil {
				return err
			}
		case !parentStat.IsDir():
			return v.fail(fmt.Sprintf("\"%s\" is not a directory", path.Dir(res.logical)), kernel.SandboxCodeInvalidPath)
		}

		// Write to a temp file and rename: readers never see a half-written file.
		var tag [4]byte
		_, _ = rand.Read(tag[:])
		tmp := filepath.Join(parent, fmt.Sprintf(".aec-write-%d-%s", os.Getpid(), hex.EncodeToString(tag[:])))
		f, err := root.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, filePerm)
		if err != nil {
			return err
		}
		done := false
		defer func() {
			if !done {
				_ = f.Close()
				_ = root.Remove(tmp)
			}
		}()
		hash := sha256.New()
		buf := make([]byte, streamChunkBytes)
		for {
			if ctx.Err() != nil {
				return v.fail("Operation cancelled", kernel.SandboxCodeCancelled)
			}
			n, rerr := body.Read(buf)
			if n > 0 {
				written += int64(n)
				if written > opts.MaxBytes {
					return v.fail(fmt.Sprintf("\"%s\" is larger than the limit of %d bytes", input, opts.MaxBytes), kernel.SandboxCodeTooLarge)
				}
				hash.Write(buf[:n])
				if _, err := f.Write(buf[:n]); err != nil {
					return err
				}
			}
			if errors.Is(rerr, io.EOF) {
				break
			}
			if rerr != nil {
				return v.fail("Could not read the upload: "+rerr.Error(), kernel.SandboxCodeCancelled)
			}
		}
		if err := f.Close(); err != nil {
			_ = root.Remove(tmp)
			done = true
			return err
		}
		done = true
		if opts.Exclusive {
			if _, err := root.Lstat(rel); err == nil {
				_ = root.Remove(tmp)
				return v.fail(fmt.Sprintf("\"%s\" already exists", input), kernel.SandboxCodeConflict)
			}
		}
		if err := root.Rename(tmp, rel); err != nil {
			_ = root.Remove(tmp)
			return err
		}
		created = !exists
		sum = hex.EncodeToString(hash.Sum(nil))
		return nil
	})
	if err != nil {
		return sandboxhub.WriteResult{}, mapFSError(err, input, v.sb.id)
	}
	return sandboxhub.WriteResult{Path: res.logical, Bytes: int(written), SHA256: sum, Created: created}, nil
}
