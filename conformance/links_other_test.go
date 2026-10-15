//go:build !windows

package conformance_test

import (
	"os"
	"testing"
)

// makeLink creates a symlink at link that points to target (both absolute host
// paths). It skips the test when the machine does not allow symlinks, and logs the
// mode that ran, like the Windows helper (which falls back to a junction). It
// duplicates the helper of package sandbox/host because test files cannot be shared
// across packages.
func makeLink(t *testing.T, link, target string) {
	t.Helper()
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("os.Symlink cannot create a link here: %v", err)
	}
	t.Logf("link mode: symlink")
	t.Cleanup(func() { _ = os.Remove(link) })
}
