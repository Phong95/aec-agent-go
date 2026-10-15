//go:build windows

package conformance_test

import (
	"os"
	"os/exec"
	"testing"
)

// makeLink creates a link at link that points to target (both absolute host
// paths; the target directory must exist). It tries os.Symlink first and falls back
// to a junction (cmd /c mklink /J), because an unprivileged Windows account cannot
// create symlinks but can create junctions (CONTEXT A3). It skips the test only when
// both fail, and logs which mode ran. The link is removed when the test ends, before
// the temporary folders are, so removing a folder never meets a junction to a folder
// outside it. It duplicates the helper of package sandbox/host because test files
// cannot be shared across packages.
func makeLink(t *testing.T, link, target string) {
	t.Helper()
	if err := os.Symlink(target, link); err == nil {
		t.Logf("link mode: symlink")
		t.Cleanup(func() { _ = os.Remove(link) })
		return
	}
	out, err := exec.CommandContext(t.Context(), "cmd", "/c", "mklink", "/J", link, target).CombinedOutput()
	if err != nil {
		t.Skipf("neither os.Symlink nor mklink /J can create a link here: %v: %s", err, out)
	}
	t.Logf("link mode: junction")
	t.Cleanup(func() { _ = os.Remove(link) })
}
