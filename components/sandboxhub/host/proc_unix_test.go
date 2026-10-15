//go:build unix

package host

import (
	"errors"
	"os"
	"strconv"
	"strings"
	"syscall"
)

// processAlive reports whether the process with this id is still running. A killed
// process whose parent does not reap it (PID 1 of a container) lingers as a zombie,
// which /proc/<pid>/stat shows as state Z: that counts as dead (RESEARCH Pitfall 7).
// Without /proc (macOS) kill(pid, 0) decides: ESRCH means gone.
func processAlive(pid int) bool {
	if data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat"); err == nil {
		// "<pid> (<name>) <state> ...": the state follows the last closing bracket.
		text := string(data)
		if i := strings.LastIndexByte(text, ')'); i >= 0 && i+2 < len(text) {
			return text[i+2] != 'Z'
		}
	}
	return !errors.Is(syscall.Kill(pid, 0), syscall.ESRCH)
}
