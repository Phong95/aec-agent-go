//go:build windows

package docker

import (
	"os/exec"
	"syscall"
)

// createNoWindow is CREATE_NO_WINDOW: no console window for the docker client.
const createNoWindow = 0x08000000

// hideWindow makes the docker client run without a console window, so a server on
// Windows does not flash a window for every docker call.
func hideWindow(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: createNoWindow}
}
