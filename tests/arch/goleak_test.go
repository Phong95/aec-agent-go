package arch_test

import (
	"bytes"
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// goleakMainProblem checks the _test.go files of one package (file name to
// source) and returns "" when the package has exactly one TestMain whose body
// calls VerifyTestMain of go.uber.org/goleak, or one problem text otherwise
// (Phase 12, REL-04). Counting only receiver-less functions named exactly
// TestMain keeps a test such as TestMainContent from counting. A helper-process
// exit before the goleak call is allowed: the call only has to appear in the
// body.
func goleakMainProblem(files map[string][]byte) string {
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)
	mains := 0
	called := false
	for _, name := range names {
		f, err := parser.ParseFile(token.NewFileSet(), name, files[name], parser.SkipObjectResolution)
		if err != nil {
			return "cannot parse " + name + ": " + err.Error()
		}
		alias := ""
		for _, imp := range f.Imports {
			p, err := strconv.Unquote(imp.Path.Value)
			if err != nil || p != "go.uber.org/goleak" {
				continue
			}
			alias = "goleak"
			if imp.Name != nil {
				alias = imp.Name.Name
			}
		}
		if alias == "_" || alias == "." {
			alias = ""
		}
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Recv != nil || fn.Name.Name != "TestMain" {
				continue
			}
			mains++
			if alias == "" || fn.Body == nil {
				continue
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok || sel.Sel.Name != "VerifyTestMain" {
					return true
				}
				if id, ok := sel.X.(*ast.Ident); ok && id.Name == alias {
					called = true
				}
				return true
			})
		}
	}
	switch {
	case mains == 0:
		return "no TestMain"
	case mains > 1:
		return "more than one TestMain"
	case !called:
		return "TestMain does not call goleak.VerifyTestMain"
	}
	return ""
}

// nestedModuleDirs returns the directories (relative to the root module, slash
// form) of every nested module: a go.mod below the root. A nested module is not
// walked into a second time, and testdata, node_modules and hidden directories
// are skipped.
func nestedModuleDirs(t *testing.T) []string {
	t.Helper()
	var dirs []string
	err := filepath.WalkDir(".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			return nil
		}
		name := d.Name()
		if path != "." && (name == "testdata" || name == "node_modules" || strings.HasPrefix(name, ".")) {
			return filepath.SkipDir
		}
		if path == "." {
			return nil
		}
		if _, statErr := os.Stat(filepath.Join(path, "go.mod")); statErr == nil {
			dirs = append(dirs, filepath.ToSlash(path))
			return filepath.SkipDir
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk for nested modules: %v", err)
	}
	sort.Strings(dirs)
	return dirs
}

// testPackageInfo is the subset of `go list -json` output the goleak check needs.
type testPackageInfo struct {
	ImportPath   string
	Dir          string
	TestGoFiles  []string
	XTestGoFiles []string
}

// listTestPackages runs go list in dir (a module root) and returns every package
// with at least one test file.
func listTestPackages(t *testing.T, dir string) []testPackageInfo {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "go", "list", "-json=ImportPath,Dir,TestGoFiles,XTestGoFiles", "./...")
	cmd.Dir = dir
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("go list in %s: %v\n%s", dir, err, stderr.String())
	}
	var pkgs []testPackageInfo
	for dec := json.NewDecoder(bytes.NewReader(out)); dec.More(); {
		var p testPackageInfo
		if err := dec.Decode(&p); err != nil {
			t.Fatalf("decode go list output of %s: %v", dir, err)
		}
		if len(p.TestGoFiles)+len(p.XTestGoFiles) > 0 {
			pkgs = append(pkgs, p)
		}
	}
	return pkgs
}

// TestEveryTestPackageVerifiesGoleak makes a goleak TestMain mandatory in every
// test package of the root module and of every nested module (CONTEXT RL-1), so
// "goleak reports no leak across the suite" cannot erode when a package is added.
func TestEveryTestPackageVerifiesGoleak(t *testing.T) {
	modules := append([]string{"."}, nestedModuleDirs(t)...)
	var problems []string
	for _, mod := range modules {
		pkgs := listTestPackages(t, mod)
		for _, p := range pkgs {
			files := map[string][]byte{}
			for _, name := range append(append([]string{}, p.TestGoFiles...), p.XTestGoFiles...) {
				src, err := os.ReadFile(filepath.Join(p.Dir, name))
				if err != nil {
					t.Fatal(err)
				}
				files[name] = src
			}
			if problem := goleakMainProblem(files); problem != "" {
				problems = append(problems, p.ImportPath+": "+problem)
			}
		}
		t.Logf("module %s: %d test packages checked", mod, len(pkgs))
	}
	sort.Strings(problems)
	if len(problems) > 0 {
		t.Fatalf("test packages without a goleak TestMain (add TestMain calling goleak.VerifyTestMain):\n%s", strings.Join(problems, "\n"))
	}
}

func TestGoleakMainCheckCatchesMissingCalls(t *testing.T) {
	const head = "package p\n\nimport (\n\t\"testing\"\n\n\t\"go.uber.org/goleak\"\n)\n\n"
	cases := []struct {
		name  string
		files map[string]string
		want  string
	}{
		{"no TestMain", map[string]string{"a_test.go": "package p\n\nimport \"testing\"\n\nfunc TestA(t *testing.T) {}\n\nfunc TestMainContent(t *testing.T) {}\n"}, "no TestMain"},
		{"TestMain without the call", map[string]string{"a_test.go": "package p\n\nimport \"testing\"\n\nfunc TestMain(m *testing.M) { m.Run() }\n"}, "does not call"},
		{"two TestMain functions", map[string]string{
			"a_test.go": head + "func TestMain(m *testing.M) { goleak.VerifyTestMain(m) }\n",
			"b_test.go": strings.Replace(head, "package p", "package p_test", 1) + "func TestMain(m *testing.M) { goleak.VerifyTestMain(m) }\n",
		}, "more than one"},
		{"call under the wrong alias", map[string]string{"a_test.go": "package p\n\nimport (\n\t\"testing\"\n\n\tleak \"go.uber.org/goleak\"\n)\n\nfunc TestMain(m *testing.M) { goleak.VerifyTestMain(m) }\n\nvar _ = leak.IgnoreCurrent\n"}, "does not call"},
		{"blank goleak import", map[string]string{"a_test.go": "package p\n\nimport (\n\t\"testing\"\n\n\t_ \"go.uber.org/goleak\"\n)\n\nfunc TestMain(m *testing.M) { goleak.VerifyTestMain(m) }\n"}, "does not call"},
		{"method named TestMain", map[string]string{"a_test.go": head + "type h struct{}\n\nfunc (h) TestMain(m *testing.M) { goleak.VerifyTestMain(m) }\n"}, "no TestMain"},
	}
	for _, c := range cases {
		files := map[string][]byte{}
		for name, src := range c.files {
			files[name] = []byte(src)
		}
		if got := goleakMainProblem(files); !strings.Contains(got, c.want) {
			t.Errorf("%s: want a problem containing %q, got %q", c.name, c.want, got)
		}
	}
	ok := map[string]map[string]string{
		"clean one-file package":              {"a_test.go": head + "func TestMain(m *testing.M) { goleak.VerifyTestMain(m) }\n"},
		"aliased import used under its alias": {"a_test.go": "package p\n\nimport (\n\t\"testing\"\n\n\tleak \"go.uber.org/goleak\"\n)\n\nfunc TestMain(m *testing.M) { leak.VerifyTestMain(m) }\n"},
		"helper exit before the call":         {"a_test.go": "package p\n\nimport (\n\t\"os\"\n\t\"testing\"\n\n\t\"go.uber.org/goleak\"\n)\n\nfunc TestMain(m *testing.M) {\n\tif os.Getenv(\"HELPER\") != \"\" {\n\t\tos.Exit(0)\n\t}\n\tgoleak.VerifyTestMain(m)\n}\n"},
	}
	for name, srcs := range ok {
		files := map[string][]byte{}
		for n, src := range srcs {
			files[n] = []byte(src)
		}
		if got := goleakMainProblem(files); got != "" {
			t.Errorf("%s flagged: %q", name, got)
		}
	}
}
