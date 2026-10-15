//go:build !windows

package host

import (
	"errors"
	"syscall"

	"github.com/Phong95/aec-agent-go/kernel"
)

// classifyPlatformError maps the Unix errnos of the classifyFSError table that
// package io/fs has no sentinel for: ENOTDIR, EISDIR, ELOOP, ENAMETOOLONG, EROFS
// and ENOSPC.
func classifyPlatformError(err error) (fsKind, bool) {
	switch {
	case errors.Is(err, syscall.ENOTDIR):
		return fsKind{kernel.SandboxCodeInvalidPath, "a parent is not a directory"}, true
	case errors.Is(err, syscall.EISDIR):
		return fsKind{kernel.SandboxCodeInvalidPath, "is a directory"}, true
	case errors.Is(err, syscall.ELOOP):
		return fsKind{kernel.SandboxCodeInvalidPath, "too many symlinks"}, true
	case errors.Is(err, syscall.ENAMETOOLONG):
		return fsKind{kernel.SandboxCodeInvalidPath, "name too long"}, true
	case errors.Is(err, syscall.EROFS):
		return fsKind{kernel.SandboxCodePolicyDenied, "read-only file system"}, true
	case errors.Is(err, syscall.ENOSPC):
		return fsKind{kernel.SandboxCodeTooLarge, "no space left on the disk"}, true
	}
	return fsKind{}, false
}

// isNotDir reports whether err says that a path component is not a directory (a
// regular file where a directory was expected).
func isNotDir(err error) bool { return errors.Is(err, syscall.ENOTDIR) }
