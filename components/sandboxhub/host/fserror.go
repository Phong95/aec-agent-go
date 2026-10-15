package host

import (
	"errors"
	"io/fs"

	"github.com/Phong95/aec-agent-go/kernel"
)

// pathEscapesMessage is the text of the error os.Root returns when a path leaves
// the root (through "..", an absolute or escaping symlink, a Windows junction, or a
// Windows reserved device name). The error value is unexported in package os
// (errPathEscapes) and there is no sentinel or typed error to compare with, so
// isPathEscape recognises it by this text - and only there (threat T-07-19).
const pathEscapesMessage = "path escapes from parent"

// isPathEscape reports whether err is the os.Root escape error. It is the single
// detector in the package. A changed message in a later Go release would make this
// return false, which fails closed (the operation fails with another error code,
// never succeeds), and escape_test.go provokes the error on every operating system
// and Go version of the CI matrix so a changed message fails CI.
//
// Only the underlying error is compared, never the full text: a *fs.PathError renders
// as "op path: reason", and the path is caller-controlled (a file literally named
// "path escapes from parent" must not look like an escape). An error that is not a
// *fs.PathError must equal the message exactly, so it cannot carry a path either.
func isPathEscape(err error) bool {
	if err == nil {
		return false
	}
	var pe *fs.PathError
	if errors.As(err, &pe) {
		return pe.Err != nil && pe.Err.Error() == pathEscapesMessage
	}
	return err.Error() == pathEscapesMessage
}

// fsKind is the SandboxError code and message tail of one file system failure.
type fsKind struct {
	code kernel.SandboxErrorCode
	text string
}

// classifyFSError is the errno table of the file system errors: not_found for a
// missing path, invalid_path for a parent that is not a directory, a directory
// where a file was expected, too many symlinks and a name that is too long,
// policy_denied for a permission or read-only failure, too_large for a full disk.
// The operating system specific errnos are in classifyPlatformError.
func classifyFSError(err error) (fsKind, bool) {
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return fsKind{kernel.SandboxCodeNotFound, "does not exist"}, true
	case errors.Is(err, fs.ErrPermission):
		return fsKind{kernel.SandboxCodePolicyDenied, "permission denied"}, true
	}
	return classifyPlatformError(err)
}

// mapFSError turns a file system error into its SandboxError
// (CONTEXT A1): a *kernel.SandboxError is returned unchanged, the os.Root escape
// error becomes path_escape with the symlink message, a known errno becomes its
// code with the message `"<label>" <text>`, and anything else is
// environment_unavailable with `"<label>": <error text>`. label is the path the
// caller supplied.
func mapFSError(err error, label, sandboxID string) error {
	var se *kernel.SandboxError
	if errors.As(err, &se) {
		return err
	}
	if isPathEscape(err) {
		return kernel.NewSandboxError(
			`"`+label+`" resolves through a symlink to a location outside the workspace`, kernel.SandboxCodePathEscape, sandboxID)
	}
	if k, ok := classifyFSError(err); ok {
		return kernel.NewSandboxError(`"`+label+`" `+k.text, k.code, sandboxID)
	}
	return kernel.NewSandboxError(`"`+label+`": `+err.Error(), kernel.SandboxCodeEnvironmentUnavailable, sandboxID)
}
