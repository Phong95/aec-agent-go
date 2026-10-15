package arch_test

import (
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// goFiles returns the non-test .go files of the module (relative, slash form),
// skipping testdata and hidden directories. Nested modules (a directory below
// the root that holds its own go.mod, such as examples/externalconsumer) are
// skipped too: their files are not root-module code (Phase 12, RESEARCH Pattern 4).
func goFiles(t *testing.T) []string {
	t.Helper()
	files, err := goFilesUnder(".")
	if err != nil {
		t.Fatalf("walk module: %v", err)
	}
	return files
}

// goFilesUnder is goFiles over any root: it returns the non-test .go files below
// root as paths relative to root, in slash form. It skips testdata, hidden
// directories and any directory other than root that contains a go.mod.
func goFilesUnder(root string) ([]string, error) {
	var files []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			name := d.Name()
			if path != root && (name == "testdata" || strings.HasPrefix(name, ".")) {
				return filepath.SkipDir
			}
			if path != root {
				if _, statErr := os.Stat(filepath.Join(path, "go.mod")); statErr == nil {
					return filepath.SkipDir
				}
			}
			return nil
		}
		if strings.HasSuffix(path, ".go") && !strings.HasSuffix(path, "_test.go") {
			rel, relErr := filepath.Rel(root, path)
			if relErr != nil {
				return relErr
			}
			files = append(files, filepath.ToSlash(rel))
		}
		return nil
	})
	return files, err
}

func TestGoFilesSkipsNestedModules(t *testing.T) {
	root := t.TempDir()
	write := func(rel, body string) {
		t.Helper()
		full := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("go.mod", "module example.com/root\n")
	write("a.go", "package a\n")
	write("sub/b.go", "package sub\n")
	write("sub/b_test.go", "package sub\n")
	write("nested/go.mod", "module example.com/nested\n")
	write("nested/x.go", "package nested\n")
	write("nested/deeper/y.go", "package deeper\n")
	write("testdata/t.go", "package t\n")
	write(".hidden/h.go", "package h\n")
	got, err := goFilesUnder(root)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"a.go", "sub/b.go"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("goFilesUnder = %v, want %v (nested module, testdata, hidden and test files must be skipped)", got, want)
	}
}
