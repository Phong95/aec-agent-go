//go:build windows

package host

import (
	"errors"
	"syscall"
)

const (
	// queryLimitedInformation is PROCESS_QUERY_LIMITED_INFORMATION.
	queryLimitedInformation = 0x1000
	// stillActive is the exit code GetExitCodeProcess reports for a running process.
	stillActive = 259
	// errInvalidParameter is what OpenProcess reports for a process id that is gone.
	errInvalidParameter = syscall.Errno(87)
)

// processAlive reports whether the process with this id is still running: it opens
// the process and asks for its exit code, which is 259 (STILL_ACTIVE) only while it
// runs. A process id that no longer exists cannot be opened. Any other failure counts
// as alive, so a probe that cannot tell never lets a test pass.
func processAlive(pid int) bool {
	h, err := syscall.OpenProcess(queryLimitedInformation, false, uint32(pid)) //nolint:gosec // G115: a process id fits a uint32
	if err != nil {
		return !errors.Is(err, errInvalidParameter)
	}
	defer func() { _ = syscall.CloseHandle(h) }()
	var code uint32
	if err := syscall.GetExitCodeProcess(h, &code); err != nil {
		return true
	}
	return code == stillActive
}
