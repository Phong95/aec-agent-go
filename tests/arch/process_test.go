package arch_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strconv"
	"strings"
	"testing"
)

// processStartImports are the standard library packages that start a process or
// load host code: os/exec, syscall (ForkExec, StartProcess and Exec start a
// process without os/exec) and plugin. Only the two sandbox backends may import
// them. os.StartProcess needs no extra import, so processExecImports also scans
// for that selector (osStartProcessCalls).
var processStartImports = map[string]bool{
	"os/exec": true,
	"syscall": true,
	"plugin":  true,
}

// osStartProcessCalls reports every use of os.StartProcess in src, whatever local
// name the os package is imported under. A dot or blank import of os yields none.
func osStartProcessCalls(filename string, src []byte) ([]string, error) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, filename, src, parser.SkipObjectResolution)
	if err != nil {
		return nil, err
	}
	local := ""
	for _, imp := range f.Imports {
		path, err := strconv.Unquote(imp.Path.Value)
		if err != nil || path != "os" {
			continue
		}
		local = "os"
		if imp.Name != nil {
			local = imp.Name.Name
		}
	}
	if local == "" || local == "_" || local == "." {
		return nil, nil
	}
	var hits []string
	ast.Inspect(f, func(n ast.Node) bool {
		sel, ok := n.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		id, ok := sel.X.(*ast.Ident)
		if ok && id.Name == local && sel.Sel.Name == "StartProcess" {
			hits = append(hits, filename+":"+strconv.Itoa(fset.Position(sel.Pos()).Line)+": uses os.StartProcess")
		}
		return true
	})
	return hits, nil
}

// processExecImports reports an os/exec, syscall or plugin import, and a use of
// os.StartProcess, in src, unless the file belongs to one of the two sandbox
// backends (hostAccessExempt), the only code of the SDK that may start a process
// (T-05-08 carried forward). syscall.ForkExec and syscall.Exec start a process
// without os/exec, so the import ban alone would leave that route open (re-review
// RR-02 of Phase 5).
func processExecImports(file string, src []byte) ([]string, error) {
	for _, prefix := range hostAccessExempt {
		if strings.HasPrefix(file, prefix) {
			return nil, nil
		}
	}
	hits, err := forbiddenImports(file, src, processStartImports)
	if err != nil {
		return nil, err
	}
	calls, err := osStartProcessCalls(file, src)
	if err != nil {
		return nil, err
	}
	return append(hits, calls...), nil
}

// TestNoProcessExecOutsideSandboxBackends fails when any non-test file of the
// module outside sandbox/host and sandbox/docker imports os/exec.
func TestNoProcessExecOutsideSandboxBackends(t *testing.T) {
	files := goFiles(t)
	if len(files) == 0 {
		t.Fatal("no non-test Go file was checked")
	}
	var all []string
	for _, file := range files {
		src, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		hits, err := processExecImports(file, src)
		if err != nil {
			t.Fatalf("parse %s: %v", file, err)
		}
		all = append(all, hits...)
	}
	if len(all) > 0 {
		t.Fatalf("os/exec outside sandbox/host and sandbox/docker (T-05-08):\n%s", strings.Join(all, "\n"))
	}
}

// TestNoProcessExecOutsideSandboxBackendsCatchesImport proves the rule can fail:
// os/exec is reported in a provider leaf, in llm, in the root package and under an
// alias, and is accepted in the two sandbox backends.
func TestNoProcessExecOutsideSandboxBackendsCatchesImport(t *testing.T) {
	src := []byte("package p\n\nimport run \"os/exec\"\n\nvar _ = run.Command\n")
	for _, file := range []string{"components/llmhub/providers/openaisub/p.go", "components/llmhub/p.go", "aec.go", "components/sandboxhub/p.go", "components/sandboxhub/hostile/p.go", "components/toolhub/p.go"} {
		if hits, err := processExecImports(file, src); err != nil || len(hits) != 1 || !strings.Contains(hits[0], "imports os/exec") {
			t.Errorf("%s importing os/exec not caught: %v %v", file, hits, err)
		}
	}
	for _, file := range []string{"components/sandboxhub/host/p.go", "components/sandboxhub/docker/p.go", "components/sandboxhub/docker/sub/p.go"} {
		if hits, err := processExecImports(file, src); err != nil || len(hits) != 0 {
			t.Errorf("%s must be allowed to import os/exec: %v %v", file, hits, err)
		}
	}
	// syscall (ForkExec, StartProcess, Exec) and plugin start a process or load host
	// code without os/exec, and os.StartProcess needs no extra import: all three are
	// reported outside the backends, accepted inside them.
	for _, imp := range []string{"os/exec", "syscall", "plugin"} {
		if !processStartImports[imp] {
			t.Errorf("%s is not in processStartImports", imp)
		}
		extra := []byte("package p\n\nimport x " + strconv.Quote(imp) + "\n\nvar _ = x.Anything\n")
		if hits, err := processExecImports("components/toolhub/p.go", extra); err != nil || len(hits) != 1 || !strings.Contains(hits[0], "imports "+imp) {
			t.Errorf("%s not caught outside the backends: %v %v", imp, hits, err)
		}
		if hits, err := processExecImports("components/sandboxhub/host/p.go", extra); err != nil || len(hits) != 0 {
			t.Errorf("%s must be allowed in a backend: %v %v", imp, hits, err)
		}
	}
	fork := []byte("package p\n\nimport sys \"syscall\"\n\nfunc f() { _, _ = sys.ForkExec(\"x\", nil, nil) }\n")
	if hits, err := processExecImports("kernel/p.go", fork); err != nil || len(hits) != 1 || !strings.Contains(hits[0], "imports syscall") {
		t.Errorf("syscall.ForkExec not caught: %v %v", hits, err)
	}
	start := []byte("package p\n\nimport stdos \"os\"\n\nfunc f() { _, _ = stdos.StartProcess(\"x\", nil, nil) }\n")
	if hits, err := processExecImports("conformance/p.go", start); err != nil || len(hits) != 1 || !strings.Contains(hits[0], "os.StartProcess") {
		t.Errorf("os.StartProcess not caught: %v %v", hits, err)
	}
	if hits, err := processExecImports("components/sandboxhub/host/p.go", start); err != nil || len(hits) != 0 {
		t.Errorf("os.StartProcess must be allowed in a backend: %v %v", hits, err)
	}
	osClean := []byte("package p\n\nimport \"os\"\n\nvar _ = os.Args\n")
	if hits, err := processExecImports("kernel/p.go", osClean); err != nil || len(hits) != 0 {
		t.Errorf("os.Args flagged as a process start: %v %v", hits, err)
	}
	clean := []byte("package p\n\n// os/exec is only named in this comment\nimport \"strings\"\n\nvar _ = strings.ToUpper\n")
	if hits, err := processExecImports("kernel/p.go", clean); err != nil || len(hits) != 0 {
		t.Errorf("clean source flagged: %v %v", hits, err)
	}
}
