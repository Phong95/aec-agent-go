package bedrock

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

// awsClientPackages are the AWS packages whose New and NewFromConfig build a client:
// the runtime client of Converse and the control-plane client of ListFoundationModels.
// Both constructors read AWS_BEARER_TOKEN_BEDROCK and switch to bearer
// authentication unless the option function of client.go undoes it, so a client
// built anywhere else would be exposed to the variable.
var awsClientPackages = map[string]string{
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime": "bedrockruntime",
	"github.com/aws/aws-sdk-go-v2/service/bedrock":        "bedrock",
}

// awsClientConstructors are the functions of those packages that build a client.
var awsClientConstructors = []string{"New", "NewFromConfig"}

// awsClientUses reports every use of a client constructor in src: a call, or a
// reference that is not called (a function value), through the package under any
// local name; and a dot import of either package, which cannot be followed and is
// refused outright. Comments and strings are not code, so they never match: the
// check works on the syntax tree.
func awsClientUses(filename string, src []byte) ([]string, error) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, filename, src, parser.SkipObjectResolution)
	if err != nil {
		return nil, err
	}
	var hits []string
	report := func(pos token.Pos, what string) {
		hits = append(hits, filename+":"+strconv.Itoa(fset.Position(pos).Line)+": "+what)
	}
	locals := map[string]bool{}
	for _, imp := range f.Imports {
		path, err := strconv.Unquote(imp.Path.Value)
		if err != nil {
			continue
		}
		def, ok := awsClientPackages[path]
		if !ok {
			continue
		}
		switch {
		case imp.Name == nil:
			locals[def] = true
		case imp.Name.Name == ".":
			report(imp.Pos(), "dot import of "+path)
		case imp.Name.Name != "_":
			locals[imp.Name.Name] = true
		}
	}
	if len(locals) == 0 {
		return hits, nil
	}
	ast.Inspect(f, func(n ast.Node) bool {
		sel, ok := n.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		if id, ok := sel.X.(*ast.Ident); ok && locals[id.Name] && slices.Contains(awsClientConstructors, sel.Sel.Name) {
			report(sel.Pos(), id.Name+"."+sel.Sel.Name)
		}
		return true
	})
	return hits, nil
}

// TestAWSClientsOnlyBuiltByTheHelper parses every non-test Go file of the package
// and fails when an AWS client constructor is used anywhere but client.go, the one
// place that clears the bearer token of the environment (T-08-21). It also requires
// client.go to build both clients, so the guard cannot pass because the helper moved.
func TestAWSClientsOnlyBuiltByTheHelper(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	built := 0
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, err := os.ReadFile(filepath.Clean(name))
		if err != nil {
			t.Fatal(err)
		}
		hits, err := awsClientUses(name, src)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		if name == "client.go" {
			built = len(hits)
			continue
		}
		for _, h := range hits {
			t.Errorf("an AWS client is built outside client.go: %s", h)
		}
	}
	if built != 2 {
		t.Errorf("client.go builds %d AWS clients, want 2 (runtime and control plane)", built)
	}
}

// TestAWSClientGuardCatchesUses proves the guard can fail: a call, a call through an
// alias, a function value, a NewFromConfig call and a dot import are reported, and a
// comment, a string, another package's New and a constructor-free use pass.
func TestAWSClientGuardCatchesUses(t *testing.T) {
	const header = "package x\n\nimport (\n"
	catches := map[string]string{
		"a call":             header + "\t\"github.com/aws/aws-sdk-go-v2/service/bedrockruntime\"\n)\n\nvar _ = bedrockruntime.New(bedrockruntime.Options{})\n",
		"a control client":   header + "\t\"github.com/aws/aws-sdk-go-v2/service/bedrock\"\n)\n\nvar _ = bedrock.New(bedrock.Options{})\n",
		"an alias":           header + "\tbr \"github.com/aws/aws-sdk-go-v2/service/bedrockruntime\"\n)\n\nvar _ = br.New(br.Options{})\n",
		"a function value":   header + "\t\"github.com/aws/aws-sdk-go-v2/service/bedrockruntime\"\n)\n\nvar f = bedrockruntime.New\n",
		"NewFromConfig":      header + "\t\"github.com/aws/aws-sdk-go-v2/service/bedrock\"\n)\n\nvar f = bedrock.NewFromConfig\n",
		"a dot import":       header + "\t. \"github.com/aws/aws-sdk-go-v2/service/bedrockruntime\"\n)\n\nvar _ = Options{}\n",
		"inside a function":  header + "\t\"github.com/aws/aws-sdk-go-v2/service/bedrock\"\n)\n\nfunc f() { _ = bedrock.New(bedrock.Options{}) }\n",
		"two uses, two hits": header + "\t\"github.com/aws/aws-sdk-go-v2/service/bedrock\"\n)\n\nvar a, b = bedrock.New, bedrock.New\n",
	}
	for name, src := range catches {
		hits, err := awsClientUses("x.go", []byte(src))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		want := 1
		if name == "two uses, two hits" {
			want = 2
		}
		if len(hits) != want {
			t.Errorf("%s: %d hits %v, want %d", name, len(hits), hits, want)
		}
	}
	passes := map[string]string{
		"a comment":              header + "\t\"github.com/aws/aws-sdk-go-v2/service/bedrockruntime\"\n)\n\n// bedrockruntime.New is built in client.go\nvar _ bedrockruntime.Options\n",
		"a string":               header + "\t\"github.com/aws/aws-sdk-go-v2/service/bedrockruntime\"\n)\n\nvar _ = \"bedrockruntime.New\"\nvar _ bedrockruntime.Options\n",
		"another package's New":  header + "\t\"errors\"\n)\n\nvar _ = errors.New(\"x\")\n",
		"a type use":             header + "\t\"github.com/aws/aws-sdk-go-v2/service/bedrock\"\n)\n\nvar _ *bedrock.Client\n",
		"an unrelated local New": "package x\n\nfunc New() int { return 1 }\n\nvar _ = New()\n",
	}
	for name, src := range passes {
		hits, err := awsClientUses("x.go", []byte(src))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if len(hits) != 0 {
			t.Errorf("%s: reported %v, want nothing", name, hits)
		}
	}
}
