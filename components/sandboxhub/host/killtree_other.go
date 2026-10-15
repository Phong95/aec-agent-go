//go:build !unix && !windows

package host

import (
	"errors"
	"os"
	"os/exec"
)

// tree is the handle on a command on a system with neither process groups nor job
// objects: only the process itself can be killed, so a descendant of a cancelled
// command is not ended there.
type tree struct {
	proc *os.Process
}

// startInTree starts cmd.
func startInTree(cmd *exec.Cmd) (*tree, error) {
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return &tree{proc: cmd.Process}, nil
}

// kill kills the process; one that is already gone is not an error.
func (t *tree) kill() error {
	if err := t.proc.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
		return err
	}
	return nil
}

// release does nothing: nothing is held open.
func (t *tree) release() {}
