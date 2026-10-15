package arch_test

import (
	"os"
	"path/filepath"
	"testing"

	"go.uber.org/goleak"
)

// TestMain runs the guards from the module root, because they walk the module's
// files and call go list ./... with module-relative paths, and fails the package if
// any test leaves a goroutine running.
func TestMain(m *testing.M) {
	if err := os.Chdir(filepath.Join("..", "..")); err != nil {
		panic("arch: chdir to the module root: " + err.Error())
	}
	if _, err := os.Stat("go.mod"); err != nil {
		panic("arch: the module root has no go.mod: " + err.Error())
	}
	goleak.VerifyTestMain(m)
}
