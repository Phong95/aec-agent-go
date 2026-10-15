package arch_test

import (
	"os"
	"strconv"
	"strings"
	"testing"
)

// llmEnvImports are the standard library packages that let code read the process
// environment, run a process or touch the file system. No non-test file under
// llm/ (nor under internal/llmtransport/, the HTTP code the leaves share) may import
// them, so SDK code can never pick up a credential from the environment (T-08-03):
// providers take credentials only from llmhub.ProviderConfig. syscall (syscall.Getenv),
// os/user (home directory and user lookups) and os/signal are on the list since the
// review (WR-04): a name check on "os" alone left them open. The guard is still an
// import check of this module's own code; what a vendor package reads is held by the
// exact vendor allow-list and by TestBedrockLinksNoCredentialChain. Live tests read
// environment variables in test files, which this guard does not scan.
// io/ioutil (ReadFile of a credentials file with no os import), plugin (loads host
// code) and cgo (import "C") are on the list too (re-review RR-03/RR-02): they reach
// the file system or the host as directly as the names above.
var llmEnvImports = map[string]bool{
	"os":            true,
	"os/exec":       true,
	"os/signal":     true,
	"os/user":       true,
	"io/ioutil":     true,
	"path/filepath": true,
	"plugin":        true,
	"syscall":       true,
	"C":             true,
}

// llmEnvDenied is the explicit expectation behind llmEnvImports, iterated by the
// synthetic test so that a name dropped from the map fails a test.
var llmEnvDenied = []string{"os", "os/exec", "os/signal", "os/user", "io/ioutil", "path/filepath", "plugin", "syscall", "C"}

// llmCoreNetDenied is the explicit expectation behind llmCoreNetImports.
var llmCoreNetDenied = []string{"net", "net/http", "net/rpc", "net/smtp", "net/http/httputil", "crypto/tls"}

// llmTransportDir is the shared HTTP code of the provider leaves. It is checked
// like a leaf of the llm tree.
const llmTransportDir = "internal/llmtransport/"

// llmCoreNetImports are the network packages the files directly in llm/ (the
// contracts, not the provider leaves) may not import either: the leaves open
// connections, the shared package does not.
// net/rpc, net/smtp, net/http/httputil and crypto/tls dial or proxy without the file
// importing net itself, so they are on the list with it (re-review RR-03).
var llmCoreNetImports = map[string]bool{
	"net":               true,
	"net/http":          true,
	"net/rpc":           true,
	"net/smtp":          true,
	"net/http/httputil": true,
	"crypto/tls":        true,
}

// llmTreeImports reports the imports of src a non-test file of the llm tree may
// not have: os, os/exec and path/filepath anywhere under llm/, and net and
// net/http additionally for a file directly in llm/ (not in a leaf package). It
// returns checked=false for a file outside llm/.
func llmTreeImports(file string, src []byte) (hits []string, checked bool, err error) {
	if strings.HasPrefix(file, llmTransportDir) {
		hits, err = forbiddenImports(file, src, llmEnvImports)
		return hits, true, err
	}
	if !strings.HasPrefix(file, "components/llmhub/") {
		return nil, false, nil
	}
	forbidden := llmEnvImports
	if !strings.Contains(strings.TrimPrefix(file, "components/llmhub/"), "/") {
		forbidden = map[string]bool{}
		for k := range llmEnvImports {
			forbidden[k] = true
		}
		for k := range llmCoreNetImports {
			forbidden[k] = true
		}
	}
	hits, err = forbiddenImports(file, src, forbidden)
	return hits, true, err
}

// TestLLMTreeHasNoHostOrEnvAccess fails when a non-test file under llm/ imports
// os, os/exec or path/filepath, or when a file directly in llm/ imports net or
// net/http. At least one file must have been checked, so an emptied or renamed
// directory cannot make the rule pass vacuously.
func TestLLMTreeHasNoHostOrEnvAccess(t *testing.T) {
	checked := 0
	var all []string
	for _, file := range goFiles(t) {
		src, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		hits, ok, err := llmTreeImports(file, src)
		if err != nil {
			t.Fatalf("parse %s: %v", file, err)
		}
		if ok {
			checked++
		}
		all = append(all, hits...)
	}
	if checked == 0 {
		t.Fatal("no non-test Go file under llm/ was checked")
	}
	if len(all) > 0 {
		t.Fatalf("host or environment access in the llm tree (take credentials from llmhub.ProviderConfig):\n%s", strings.Join(all, "\n"))
	}
}

// TestLLMTreeGuardCatchesForbiddenImport proves the llm-tree rule can fail: each
// forbidden import is reported under an alias, as a blank import and as a dot
// import; net and net/http are reported directly in llm/ but not in a leaf; a
// clean source, a comment naming a package and a file outside llm/ are not.
func TestLLMTreeGuardCatchesForbiddenImport(t *testing.T) {
	// The names the review added are on the list, and the transport package is checked.
	for _, imp := range llmEnvDenied {
		if !llmEnvImports[imp] {
			t.Errorf("%s is not in llmEnvImports", imp)
		}
	}
	for _, imp := range llmCoreNetDenied {
		if !llmCoreNetImports[imp] {
			t.Errorf("%s is not in llmCoreNetImports", imp)
		}
	}
	if len(llmEnvImports) != len(llmEnvDenied) || len(llmCoreNetImports) != len(llmCoreNetDenied) {
		t.Errorf("llmEnvImports/llmCoreNetImports and their expectation lists differ in size: keep them in step")
	}
	for _, imp := range llmEnvDenied {
		for _, file := range []string{"components/llmhub/x.go", "components/llmhub/providers/anthropic/x.go", "components/llmhub/providers/openaisub/deep/x.go", "internal/llmtransport/x.go"} {
			src := []byte("package p\n\nimport (\n\tx " + strconv.Quote(imp) + "\n)\n\nvar _ = x.Anything\n")
			hits, ok, err := llmTreeImports(file, src)
			if err != nil || !ok || len(hits) != 1 || !strings.Contains(hits[0], "imports "+imp) {
				t.Errorf("%s importing %s not caught: %v %v %v", file, imp, hits, ok, err)
			}
		}
	}
	for _, form := range []string{"_ \"os\"", ". \"os/exec\"", "\"path/filepath\"", "stdos \"os\""} {
		src := []byte("package p\n\nimport " + form + "\n")
		if hits, _, err := llmTreeImports("components/llmhub/providers/openaicompat/p.go", src); err != nil || len(hits) != 1 {
			t.Errorf("import form %s not caught: %v %v", form, hits, err)
		}
	}
	for _, imp := range llmCoreNetDenied {
		src := []byte("package p\n\nimport x " + strconv.Quote(imp) + "\n\nvar _ = x.Anything\n")
		if hits, ok, err := llmTreeImports("components/llmhub/types.go", src); err != nil || !ok || len(hits) != 1 {
			t.Errorf("%s directly in llm/ not caught: %v %v %v", imp, hits, ok, err)
		}
		if hits, ok, err := llmTreeImports("components/llmhub/providers/openaisub/p.go", src); err != nil || !ok || len(hits) != 0 {
			t.Errorf("%s in a leaf must be allowed: %v %v %v", imp, hits, ok, err)
		}
	}
	clean := []byte("package p\n\n// os and net/http are only named in this comment\nimport (\n\t\"net/url\"\n\t\"strings\"\n)\n\nvar _ = \"os/exec\"\nvar _ = url.Parse\nvar _ = strings.ToUpper\n")
	if hits, ok, err := llmTreeImports("components/llmhub/p.go", clean); err != nil || !ok || len(hits) != 0 {
		t.Errorf("clean source flagged: %v %v %v", hits, ok, err)
	}
	outside := []byte("package p\n\nimport \"os\"\n\nvar _ = os.Args\n")
	for _, file := range []string{"kernel/p.go", "internal/llmtest/p.go", "llmx/p.go", "aec.go"} {
		if hits, ok, err := llmTreeImports(file, outside); err != nil || ok || len(hits) != 0 {
			t.Errorf("%s is outside the llm tree but was checked: %v %v %v", file, hits, ok, err)
		}
	}
}
