package openaicompat

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// vendorModule is the import path of the vendor SDK this package wraps.
const vendorModule = "github.com/openai/openai-go/v3"

// envReadingFunctions are the functions of the vendor SDK that apply its
// environment defaults: the client constructor, which reads OPENAI_API_KEY,
// OPENAI_BASE_URL, OPENAI_ORG_ID, OPENAI_PROJECT_ID, OPENAI_CUSTOM_HEADERS and more,
// and the function that lists those defaults. The SDK has no public switch to turn
// them off, so the package must never use either (see the package comment).
var envReadingFunctions = []string{"NewClient", "DefaultClientOptions"}

// envReadingClientUses reports every use of an environment-reading function of the
// vendor SDK in src: a call, or a reference that is not called (a function value),
// through the package under any local name, or through a dot import. Comments and
// strings are not code, so they never match; the check works on the syntax tree.
func envReadingClientUses(filename string, src []byte) ([]string, error) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, filename, src, parser.SkipObjectResolution)
	if err != nil {
		return nil, err
	}
	locals := map[string]bool{}
	dot := false
	for _, imp := range f.Imports {
		path, err := strconv.Unquote(imp.Path.Value)
		if err != nil || path != vendorModule {
			continue
		}
		switch {
		case imp.Name == nil:
			locals["openai"] = true
		case imp.Name.Name == ".":
			dot = true
		case imp.Name.Name != "_":
			locals[imp.Name.Name] = true
		}
	}
	if len(locals) == 0 && !dot {
		return nil, nil
	}
	var hits []string
	report := func(pos token.Pos, name string) {
		hits = append(hits, filename+":"+strconv.Itoa(fset.Position(pos).Line)+": "+name)
	}
	ast.Inspect(f, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.SelectorExpr:
			id, ok := x.X.(*ast.Ident)
			if ok && locals[id.Name] && slices.Contains(envReadingFunctions, x.Sel.Name) {
				report(x.Pos(), id.Name+"."+x.Sel.Name)
			}
		case *ast.CallExpr:
			if id, ok := x.Fun.(*ast.Ident); ok && dot && slices.Contains(envReadingFunctions, id.Name) {
				report(x.Pos(), id.Name)
			}
		}
		return true
	})
	return hits, nil
}

// TestNoEnvironmentReadingClientConstructor parses every non-test file of the
// package and fails on any use of the SDK's client constructor or of its default
// options (T-08-15): the package builds the chat and model services directly, so
// OPENAI_* variables never reach a request.
func TestNoEnvironmentReadingClientConstructor(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	checked := 0
	for _, file := range files {
		if strings.HasSuffix(file, "_test.go") {
			continue
		}
		src, err := os.ReadFile(file)
		if err != nil {
			t.Fatalf("read %s: %v", file, err)
		}
		checked++
		hits, err := envReadingClientUses(file, src)
		if err != nil {
			t.Fatalf("parse %s: %v", file, err)
		}
		for _, h := range hits {
			t.Errorf("the vendor SDK's environment-reading constructor is used: %s", h)
		}
	}
	if checked < 6 {
		t.Fatalf("only %d non-test files were checked, the guard is not looking at the package", checked)
	}
}

// TestEnvironmentConstructorGuardCatchesAUse proves the guard can fail: a call, an
// aliased call, a dot-import call, a function value and the default options are
// found, and a comment, a string, another package's NewClient and the service
// constructors are not.
func TestEnvironmentConstructorGuardCatchesAUse(t *testing.T) {
	const header = "package p\n\nimport (\n"
	caught := map[string]string{
		"call":            header + "\t\"github.com/openai/openai-go/v3\"\n)\n\nfunc f() { _ = openai.NewClient() }\n",
		"aliased call":    header + "\tsdk \"github.com/openai/openai-go/v3\"\n)\n\nfunc f() { _ = sdk.NewClient() }\n",
		"dot import":      header + "\t. \"github.com/openai/openai-go/v3\"\n)\n\nfunc f() { _ = NewClient() }\n",
		"function value":  header + "\t\"github.com/openai/openai-go/v3\"\n)\n\nvar f = openai.NewClient\n",
		"default options": header + "\t\"github.com/openai/openai-go/v3\"\n)\n\nfunc f() { _ = openai.DefaultClientOptions() }\n",
	}
	for name, src := range caught {
		hits, err := envReadingClientUses(name+".go", []byte(src))
		if err != nil || len(hits) != 1 {
			t.Errorf("%s: want one hit, got %v (%v)", name, hits, err)
		}
	}
	clean := map[string]string{
		"comment":        header + "\t\"github.com/openai/openai-go/v3\"\n)\n\n// openai.NewClient() is not used\nvar _ = openai.NewChatCompletionService\n",
		"string":         header + "\t\"github.com/openai/openai-go/v3\"\n)\n\nvar _ = \"openai.NewClient()\"\nvar _ = openai.NewModelService\n",
		"other package":  header + "\t\"example.com/other\"\n)\n\nfunc f() { _ = other.NewClient() }\n",
		"no vendor":      "package p\n\nfunc NewClient() {}\n\nfunc f() { NewClient() }\n",
		"service ctor":   header + "\t\"github.com/openai/openai-go/v3\"\n)\n\nvar _ = openai.NewChatCompletionService\n",
		"local variable": header + "\t\"github.com/openai/openai-go/v3\"\n)\n\nfunc f() { openai := struct{ X int }{}; _ = openai.X }\n",
	}
	for name, src := range clean {
		hits, err := envReadingClientUses(name+".go", []byte(src))
		if err != nil || len(hits) != 0 {
			t.Errorf("%s: want no hit, got %v (%v)", name, hits, err)
		}
	}
}
