//go:build !windows

package host

import (
	"context"
	"os/exec"
	"strings"
)

// shellCommand is the shell form of a command:
// /bin/sh -c with the argv joined by single spaces, as Node does.
func shellCommand(ctx context.Context, argv []string) *exec.Cmd {
	return exec.CommandContext(ctx, "/bin/sh", "-c", strings.Join(argv, " ")) //nolint:gosec // G204: the shell is the point of this mode; the sandbox's approval policy gates it
}
