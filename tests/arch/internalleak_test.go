package arch_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

// internalLeaks returns "Owner -> importpath.Type" for every exported
// declaration of src whose signature, exported field, exported interface method
// or untyped var initialiser names a type of an internal package (CONTEXT EC-2).
// An outside module cannot import an internal package, so it could not name such
// a type, and the exported test surface of conformance and aecmock would be
// unusable from server/go. Function bodies, including the body of a function
// literal used as an initialiser, are never inspected: only the literal's own
// signature is.
//
// The rule is purely syntactic (one file, no type information), so it has limits.
// A dot import of an internal package cannot be traced to the identifiers it
// brings in, so the import itself is reported. It does not follow an exported
// signature through an unexported alias or named type declared in the file
// (type local = x.T, then func F(l local)), and it does not see an internal type
// that an untyped var gets from a same-package helper (var V = helper()). Only a
// selector written in the exported declaration itself is found. The conformance
// and aecmock packages keep to the direct form, and go vet or go build of the
// external consumer module is the semantic backstop: it fails when an exported
// identifier needs a type it cannot import.
func internalLeaks(filename string, src []byte) ([]string, error) {
	f, err := parser.ParseFile(token.NewFileSet(), filename, src, parser.SkipObjectResolution)
	if err != nil {
		return nil, err
	}
	internalByName := map[string]string{} // local import name -> import path
	var dotImports []string
	for _, imp := range f.Imports {
		p, err := strconv.Unquote(imp.Path.Value)
		if err != nil || !strings.Contains(p+"/", "/internal/") {
			continue
		}
		name := path.Base(p)
		if imp.Name != nil {
			name = imp.Name.Name
		}
		if name == "_" {
			continue
		}
		if name == "." {
			dotImports = append(dotImports, p)
			continue
		}
		internalByName[name] = p
	}
	// A dot import hides which identifiers come from the internal package, so it is
	// reported as a whole.
	var out []string
	for _, p := range dotImports {
		out = append(out, "(dot import) -> "+p)
	}
	if len(internalByName) == 0 {
		return out, nil
	}
	var mentions func(owner string, n ast.Node)
	mentions = func(owner string, n ast.Node) {
		ast.Inspect(n, func(x ast.Node) bool {
			if lit, ok := x.(*ast.FuncLit); ok {
				// The signature of a function literal is part of the declaration, its
				// body is not.
				mentions(owner, lit.Type)
				return false
			}
			sel, ok := x.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			id, ok := sel.X.(*ast.Ident)
			if !ok {
				return true
			}
			if p, ok := internalByName[id.Name]; ok {
				out = append(out, owner+" -> "+p+"."+sel.Sel.Name)
			}
			return true
		})
	}
	for _, decl := range f.Decls {
		switch d := decl.(type) {
		case *ast.FuncDecl:
			owner := d.Name.Name
			if d.Recv != nil && len(d.Recv.List) == 1 {
				recv := recvTypeName(d.Recv.List[0].Type)
				if !ast.IsExported(recv) {
					continue
				}
				owner = recv + "." + owner
			}
			if !ast.IsExported(d.Name.Name) {
				continue
			}
			mentions(owner, d.Type)
		case *ast.GenDecl:
			for _, spec := range d.Specs {
				switch s := spec.(type) {
				case *ast.TypeSpec:
					if !ast.IsExported(s.Name.Name) {
						continue
					}
					owner := s.Name.Name
					if s.TypeParams != nil {
						mentions(owner, s.TypeParams)
					}
					switch tt := s.Type.(type) {
					case *ast.StructType:
						for _, fld := range tt.Fields.List {
							if len(fld.Names) == 0 || anyExported(fld.Names) {
								mentions(owner, fld.Type)
							}
						}
					case *ast.InterfaceType:
						for _, m := range tt.Methods.List {
							if len(m.Names) == 0 || anyExported(m.Names) {
								mentions(owner, m.Type)
							}
						}
					default:
						mentions(owner, s.Type)
					}
				case *ast.ValueSpec:
					if !anyExported(s.Names) {
						continue
					}
					owner := s.Names[0].Name
					if s.Type != nil {
						mentions(owner, s.Type)
						continue
					}
					for _, v := range s.Values {
						mentions(owner, v)
					}
				}
			}
		}
	}
	return out, nil
}

// recvTypeName returns the base type name of a method receiver expression.
func recvTypeName(e ast.Expr) string {
	for {
		switch x := e.(type) {
		case *ast.StarExpr:
			e = x.X
		case *ast.ParenExpr:
			e = x.X
		case *ast.IndexExpr:
			e = x.X
		case *ast.IndexListExpr:
			e = x.X
		case *ast.Ident:
			return x.Name
		default:
			return ""
		}
	}
}

func anyExported(names []*ast.Ident) bool {
	for _, n := range names {
		if ast.IsExported(n.Name) {
			return true
		}
	}
	return false
}

// TestPublicPackagesNameNoInternalType checks the non-test files of conformance
// (outside conformance/internal) and aecmock: no exported identifier may mention
// a type of an internal package (CONTEXT EC-2, T-12-10).
func TestPublicPackagesNameNoInternalType(t *testing.T) {
	var all []string
	checked := 0
	for _, file := range goFiles(t) {
		inConformance := strings.HasPrefix(file, "conformance/") && !strings.HasPrefix(file, "conformance/internal/")
		if !inConformance && !strings.HasPrefix(file, "aecmock/") {
			continue
		}
		src, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		hits, err := internalLeaks(file, src)
		if err != nil {
			t.Fatalf("parse %s: %v", file, err)
		}
		all = append(all, hits...)
		checked++
	}
	if checked == 0 {
		t.Fatal("no conformance or aecmock file was checked; the rule would pass vacuously")
	}
	if len(all) > 0 {
		t.Fatalf("exported identifier names an internal type (an outside module cannot use it):\n%s", strings.Join(all, "\n"))
	}
	t.Logf("%d files of conformance and aecmock checked", checked)
}

func TestInternalLeaksCatchSyntheticShapes(t *testing.T) {
	const imp = "example.com/m/internal/x"
	const head = "package p\n\nimport \"" + imp + "\"\n\n"
	cases := []struct {
		name string
		src  string
		want []string
	}{
		{"exported func parameter", head + "func F(a x.T) {}\n", []string{"F -> " + imp + ".T"}},
		{"exported func result", head + "func G() x.T { return x.T{} }\n", []string{"G -> " + imp + ".T"}},
		{"method on an exported type", head + "type S struct{}\n\nfunc (S) M(a x.T) {}\n", []string{"S.M -> " + imp + ".T"}},
		{"method on a pointer receiver", head + "type S struct{}\n\nfunc (*S) M() x.T { return x.T{} }\n", []string{"S.M -> " + imp + ".T"}},
		{"exported struct field", head + "type S struct{ F x.T }\n", []string{"S -> " + imp + ".T"}},
		{"embedded field", head + "type S struct{ x.T }\n", []string{"S -> " + imp + ".T"}},
		{"exported interface method", head + "type I interface{ M(x.T) }\n", []string{"I -> " + imp + ".T"}},
		{"type alias", head + "type A = x.T\n", []string{"A -> " + imp + ".T"}},
		{"func type", head + "type Fn func(x.T)\n", []string{"Fn -> " + imp + ".T"}},
		{"exported var with a type", head + "var V x.T\n", []string{"V -> " + imp + ".T"}},
		{"untyped var whose initialiser names the type", head + "var V = x.New()\n", []string{"V -> " + imp + ".New"}},
		{"generic constraint", head + "type G[T x.C] struct{}\n", []string{"G -> " + imp + ".C"}},
		{"dot import", "package p\n\nimport . \"" + imp + "\"\n\nfunc F(a T) {}\n", []string{"(dot import) -> " + imp}},
		{"func literal signature", head + "var V = func(a x.T) {}\n", []string{"V -> " + imp + ".T"}},
		{"renamed import", "package p\n\nimport ix \"" + imp + "\"\n\nvar V ix.T\n", []string{"V -> " + imp + ".T"}},
	}
	for _, c := range cases {
		got, err := internalLeaks("p.go", []byte(c.src))
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: internalLeaks = %v, want %v", c.name, got, c.want)
		}
	}
	// Source that must not be flagged. The generic type without constraints also
	// guards the nil TypeParams panic of ast.Inspect.
	clean := map[string]string{
		"unexported func":         head + "func f(a x.T) {}\n",
		"unexported receiver":     head + "type s struct{}\n\nfunc (s) M(a x.T) {}\n",
		"unexported field":        head + "type S struct{ f x.T }\n",
		"unexported iface method": head + "type I interface{ m(x.T) }\n",
		"unexported var":          head + "var v x.T\n",
		"body only":               head + "func F() int { var t x.T; _ = t; return 1 }\n",
		"func literal body only":  head + "var V = func() int { return x.N() }\n",
		"nested literal body":     head + "var V = wrap(func() { x.Do() })\n",
		"blank import":            "package p\n\nimport _ \"" + imp + "\"\n\nfunc F() {}\n",
		"no type parameters":      head + "type P struct{ N int }\n",
		"no internal import":      "package p\n\nimport \"strings\"\n\nfunc F(b strings.Builder) {}\n",
		"non-internal selector":   "package p\n\nimport \"example.com/m/x\"\n\nfunc F(a x.T) {}\n",
	}
	for name, src := range clean {
		if got, err := internalLeaks("c.go", []byte(src)); err != nil || len(got) != 0 {
			t.Errorf("%s flagged: %v %v", name, got, err)
		}
	}
}
