//go:build windows

package host

import (
	"errors"
	"syscall"

	"github.com/Phong95/aec-agent-go/kernel"
)

// The Windows error numbers of the classifyFSError table (the Windows equivalents of
// ENOTDIR, EISDIR, ELOOP, ENAMETOOLONG, EROFS and ENOSPC). package syscall does not
// export all of them.
const (
	errorWriteProtect         syscall.Errno = 19   // ERROR_WRITE_PROTECT
	errorHandleDiskFull       syscall.Errno = 39   // ERROR_HANDLE_DISK_FULL
	errorDiskFull             syscall.Errno = 112  // ERROR_DISK_FULL
	errorFilenameExceedsRange syscall.Errno = 206  // ERROR_FILENAME_EXCED_RANGE
	errorDirectory            syscall.Errno = 267  // ERROR_DIRECTORY
	errorCantResolveFilename  syscall.Errno = 1921 // ERROR_CANT_RESOLVE_FILENAME (a symlink loop)
)

// classifyPlatformError maps the Windows errnos that package io/fs has no sentinel
// for. A full disk (ERROR_DISK_FULL) is the Windows ENOSPC; a missing path
// (ERROR_PATH_NOT_FOUND) is already fs.ErrNotExist.
func classifyPlatformError(err error) (fsKind, bool) {
	switch {
	case errors.Is(err, errorDirectory):
		return fsKind{kernel.SandboxCodeInvalidPath, "a parent is not a directory"}, true
	case errors.Is(err, errorCantResolveFilename):
		return fsKind{kernel.SandboxCodeInvalidPath, "too many symlinks"}, true
	case errors.Is(err, errorFilenameExceedsRange):
		return fsKind{kernel.SandboxCodeInvalidPath, "name too long"}, true
	case errors.Is(err, errorWriteProtect):
		return fsKind{kernel.SandboxCodePolicyDenied, "read-only file system"}, true
	case errors.Is(err, errorDiskFull), errors.Is(err, errorHandleDiskFull):
		return fsKind{kernel.SandboxCodeTooLarge, "no space left on the disk"}, true
	}
	return fsKind{}, false
}

// isNotDir reports whether err says that a path component is not a directory (a
// regular file where a directory was expected). Windows reports a missing path for
// "file\child" (fs.ErrNotExist); ERROR_DIRECTORY is its other spelling.
func isNotDir(err error) bool { return errors.Is(err, errorDirectory) }
