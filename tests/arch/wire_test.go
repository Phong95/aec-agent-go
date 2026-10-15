package arch_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

// stdlibMarshalCalls reports every call to encoding/json's top-level Marshal or
// MarshalIndent, and every json.NewEncoder(...) call (whose Encode would bypass
// jsonx the same way), in src, whatever local name the package is imported under.
func stdlibMarshalCalls(filename string, src []byte) ([]string, error) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, filename, src, parser.SkipObjectResolution)
	if err != nil {
		return nil, err
	}
	local := ""
	for _, imp := range f.Imports {
		path, err := strconv.Unquote(imp.Path.Value)
		if err != nil || path != "encoding/json" {
			continue
		}
		local = "json"
		if imp.Name != nil {
			local = imp.Name.Name
		}
	}
	if local == "" || local == "_" || local == "." {
		return nil, nil
	}
	var hits []string
	ast.Inspect(f, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		id, ok := sel.X.(*ast.Ident)
		if !ok || id.Name != local {
			return true
		}
		if sel.Sel.Name == "Marshal" || sel.Sel.Name == "MarshalIndent" || sel.Sel.Name == "NewEncoder" {
			pos := fset.Position(call.Pos())
			hits = append(hits, filename+":"+strconv.Itoa(pos.Line)+": json."+sel.Sel.Name)
		}
		return true
	})
	return hits, nil
}

func TestNoStdlibMarshalOnWirePaths(t *testing.T) {
	var all []string
	for _, file := range goFiles(t) {
		if strings.HasPrefix(file, "internal/jsonx/") {
			continue
		}
		src, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		hits, err := stdlibMarshalCalls(file, src)
		if err != nil {
			t.Fatalf("parse %s: %v", file, err)
		}
		all = append(all, hits...)
	}
	if len(all) > 0 {
		t.Fatalf("encoding/json Marshal on a wire path (use jsonx.Marshal / kernel.EncodeJSON):\n%s", strings.Join(all, "\n"))
	}
}

func TestStdlibMarshalCheckCatchesAliasedImport(t *testing.T) {
	src := []byte("package p\n\nimport stdjson \"encoding/json\"\n\nfunc f() { _, _ = stdjson.Marshal(1) }\n")
	hits, err := stdlibMarshalCalls("p.go", src)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || !strings.Contains(hits[0], "p.go:5") {
		t.Fatalf("aliased Marshal call not caught: %v", hits)
	}
	enc := []byte("package p\n\nimport \"encoding/json\"\n\nfunc f() { _ = json.NewEncoder(nil) }\n")
	if hits, err := stdlibMarshalCalls("e.go", enc); err != nil || len(hits) != 1 {
		t.Fatalf("json.NewEncoder call not caught: %v %v", hits, err)
	}
	clean := []byte("package p\n\nimport \"encoding/json\"\n\nfunc f() { _ = json.NewEncoder }\n")
	if hits, err := stdlibMarshalCalls("q.go", clean); err != nil || len(hits) != 0 {
		t.Fatalf("clean source flagged: %v %v", hits, err)
	}
}

// wirePackages are the packages whose exported struct fields are wire data
// (ARCHITECTURE section 8 rule 1: every wire field carries a json tag).
// A package is identified by its full directory (see wirePackageDir), so the
// sub-packages sandbox/host and sandbox/docker, which hold backends and no wire
// data, are not wire packages.
var wirePackages = []string{"kernel", "components/events", "components/trace", "components/sandboxhub", "components/agent/memorysystem/memory", "components/agent/profile", "components/toolhub", "components/llmhub", "components/skillhub/skill", "components/artifacthub/artifact", "components/persistence"}

// nonWireTypes are exported structs that are not wire data, or that marshal
// through custom JSON, keyed "package.Type".
var nonWireTypes = map[string]bool{
	"kernel.SDKError":                 true,
	"kernel.ModelError":               true,
	"kernel.ToolError":                true,
	"kernel.PolicyError":              true,
	"kernel.SessionError":             true,
	"kernel.SkillError":               true,
	"kernel.SandboxError":             true,
	"kernel.ArtifactError":            true,
	"job.Error":                       true,
	"events.Kind":                     true,
	"events.Log":                      true,
	"events.Dispatcher":               true,
	"trace.Service":                   true,
	"trace.MutableContext":            true,
	"trace.ConsoleSink":               true,
	"sandboxhub.Selection":            true,
	"sandboxhub.ExecOptions":          true,
	"sandboxhub.SandboxHub":           true,
	"sandboxhub.PolicyUpdate":         true,
	"sandboxhub.TraceScope":           true,
	"toolhub.Context":                 true,
	"toolhub.ToolHub":                 true,
	"toolhub.Authorized":              true,
	"toolhub.DefaultPolicy":           true,
	"toolhub.DefaultPolicyOptions":    true,
	"toolhub.Properties":              true,
	"toolhub.Property":                true,
	"persistence.MemorySessionStore":  true,
	"persistence.SessionBacking":      true,
	"persistence.SessionOption":       true,
	"persistence.MemoryMessageStore":  true,
	"persistence.MemoryMemoryStore":   true,
	"persistence.MemoryAgentStore":    true,
	"persistence.MemoryProviderStore": true,
	"persistence.MemorySkillStore":    true,
	"persistence.MemoryArtifactStore": true,
	"persistence.PersistenceHub":      true,
	// A runtime value (the resolved loop limits of one run), not wire data.
	"profile.ResolvedLoopConfig": true,
}

// untaggedWireFields reports every exported field of every exported struct in
// src that has no json tag, except the types in skip.
func untaggedWireFields(filename string, src []byte, skip map[string]bool) ([]string, error) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, filename, src, parser.SkipObjectResolution)
	if err != nil {
		return nil, err
	}
	var missing []string
	for _, decl := range f.Decls {
		gd, ok := decl.(*ast.GenDecl)
		if !ok || gd.Tok != token.TYPE {
			continue
		}
		for _, spec := range gd.Specs {
			ts, ok := spec.(*ast.TypeSpec)
			if !ok || !ts.Name.IsExported() {
				continue
			}
			st, ok := ts.Type.(*ast.StructType)
			if !ok || skip[f.Name.Name+"."+ts.Name.Name] {
				continue
			}
			for _, field := range st.Fields.List {
				for _, name := range field.Names {
					if !name.IsExported() {
						continue
					}
					tag := ""
					if field.Tag != nil {
						if unq, err := strconv.Unquote(field.Tag.Value); err == nil {
							tag = reflect.StructTag(unq).Get("json")
						}
					}
					if tag == "" {
						missing = append(missing, filename+": "+ts.Name.Name+"."+name.Name+" has no json tag")
					}
				}
			}
		}
	}
	return missing, nil
}

// wirePackageDir returns the full package directory of a module-relative file
// path (slash form), or "" for a file of the root package. A file is classified by
// its whole directory, not by the first path element, so a sub-package such as
// sandbox/host (and later sandbox/docker) is not a wire package merely because its
// parent sandbox is one, while every package listed in wirePackages is checked
// exactly as before.
func wirePackageDir(file string) string {
	i := strings.LastIndex(file, "/")
	if i < 0 {
		return ""
	}
	return file[:i]
}

func TestWireStructsHaveJSONTags(t *testing.T) {
	var missing []string
	for _, file := range goFiles(t) {
		if dir := wirePackageDir(file); dir == "" || !contains(wirePackages, dir) {
			continue
		}
		src, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		m, err := untaggedWireFields(file, src, nonWireTypes)
		if err != nil {
			t.Fatalf("parse %s: %v", file, err)
		}
		missing = append(missing, m...)
	}
	if len(missing) > 0 {
		t.Fatalf("wire struct fields without a json tag:\n%s", strings.Join(missing, "\n"))
	}
}

// TestWireTagCheckClassifiesByPackageDirectory proves a file is classified by its
// whole package directory: the wire package sandbox is checked, its sub-package
// sandbox/host is not, and a file of the root package has no directory.
func TestWireTagCheckClassifiesByPackageDirectory(t *testing.T) {
	for file, want := range map[string]string{
		"components/sandboxhub/hub.go":             "components/sandboxhub",
		"components/sandboxhub/host/provider.go":   "components/sandboxhub/host",
		"components/sandboxhub/docker/provider.go": "components/sandboxhub/docker",
		"conformance/internal/x.go":                "conformance/internal",
		"aec.go":                                   "",
	} {
		if got := wirePackageDir(file); got != want {
			t.Errorf("wirePackageDir(%q) = %q, want %q", file, got, want)
		}
	}
	if !contains(wirePackages, wirePackageDir("components/sandboxhub/hub.go")) {
		t.Error("sandbox must stay a wire package")
	}
	for _, backend := range []string{"components/sandboxhub/host/provider.go", "components/sandboxhub/docker/provider.go"} {
		if contains(wirePackages, wirePackageDir(backend)) {
			t.Errorf("%s must not be checked as a wire package", backend)
		}
	}
}

func TestWireTagCheckCatchesUntaggedField(t *testing.T) {
	src := []byte("package kernel\n\ntype Thing struct {\n\tOK string `json:\"ok\"`\n\tBad string\n\thidden string\n}\n")
	m, err := untaggedWireFields("kernel/thing.go", src, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(m) != 1 || !strings.Contains(m[0], "Thing.Bad") {
		t.Fatalf("untagged field not caught exactly once: %v", m)
	}
	if m, _ := untaggedWireFields("kernel/thing.go", src, map[string]bool{"kernel.Thing": true}); len(m) != 0 {
		t.Fatalf("skip list ignored: %v", m)
	}
}
