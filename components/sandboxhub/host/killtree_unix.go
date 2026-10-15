//go:build unix

package host

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
)

// ---------------------------------------------------------------------------
// Process trees on Unix (CONTEXT B2): a command is started in its own process group
// (Setpgid), and killing it sends SIGKILL to the negative
// process group id, which reaches every process that group still holds - a
// grandchild whose parent already exited included. A process that moved itself to
// another group or session (setsid) escapes (RESEARCH F-U1).
// ---------------------------------------------------------------------------

// tree is the handle on everything one command started: the process group, whose id
// is the pid of its leader.
type tree struct {
	pid  int
	proc *os.Process
}

// startInTree starts cmd as the leader of a new process group.
func startInTree(cmd *exec.Cmd) (*tree, error) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return &tree{pid: cmd.Process.Pid, proc: cmd.Process}, nil
}

// kill sends SIGKILL to the whole process group, also when its leader already
// exited and only descendants are left. A group that does not exist any more is not
// an error; any other failure falls back to killing the leader alone.
func (t *tree) kill() error {
	err := syscall.Kill(-t.pid, syscall.SIGKILL)
	if err == nil || errors.Is(err, syscall.ESRCH) {
		return nil
	}
	if killErr := t.proc.Kill(); killErr != nil && !errors.Is(killErr, os.ErrProcessDone) {
		return killErr
	}
	return nil
}

// release does nothing on Unix: nothing is held open for the group.
func (t *tree) release() {}
