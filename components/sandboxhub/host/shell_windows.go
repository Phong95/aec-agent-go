//go:build windows

package host

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"syscall"
)

// shellCommand is the shell form of a command: the
// command interpreter named by the ComSpec variable of the SERVER environment (Node
// uses process.env.comspec, falling back to cmd.exe) run as
//
//	<comspec> /d /s /c "<argv joined by single spaces>"
//
// The line is set verbatim through SysProcAttr.CmdLine. The default quoting of
// os/exec would escape every quote in the line as \" and cmd.exe does not read that,
// so `echo "a  b" & echo c` would not print what Node prints (RESEARCH F-W5, Pitfall
// 4). With /s and the outer quotes cmd.exe strips exactly one pair and runs the rest
// as typed.
func shellCommand(ctx context.Context, argv []string) *exec.Cmd {
	comspec := os.Getenv("ComSpec")
	if comspec == "" {
		comspec = "cmd.exe"
	}
	// libuv quotes the program in the command line when its path has a space.
	program := comspec
	if strings.ContainsAny(comspec, " \t") {
		program = `"` + comspec + `"`
	}
	cmd := exec.CommandContext(ctx, comspec) //nolint:gosec // G204: the shell is the point of this mode; the sandbox's approval policy gates it
	cmd.SysProcAttr = &syscall.SysProcAttr{CmdLine: program + ` /d /s /c "` + strings.Join(argv, " ") + `"`}
	return cmd
}
