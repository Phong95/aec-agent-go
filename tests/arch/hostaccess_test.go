package arch_test

import (
	"go/parser"
	"go/token"
	"os"
	"strconv"
	"strings"
	"testing"
)

// hostAccessGuarded are the packages that must never reach the host: the stores,
// the skill and artifact hubs and their contracts, the sandbox contracts and hub,
// the test doubles of aecmock, the agent loop (agentloop, Phase 9) and the agent
// runtime packages (agent, orchestration, memorysystem and builtin/skills, Phase 10),
// and the Phase 11 packages that hold built-in tools (builtin/clarify, 11-01,
// builtin/workspace, 11-02, builtin/artifacts, 11-03, and builtin/web, 11-04) or share
// their plumbing (internal/toolkit, 11-01).
// They touch files only through sandboxhub.View (T-05-08 carried forward: every path and
// every command goes through the sandbox, never the host). The two backends
// sandbox/host and sandbox/docker are the only exceptions (T-06-26); see
// hostAccessExempt.
//
// An entry that contains a slash (builtin/skills, builtin/clarify, builtin/workspace,
// builtin/artifacts, builtin/web, internal/toolkit) names a nested package and matches the module-relative prefix entry + "/"; every
// other entry matches the first path element. builtin/workspace and builtin/artifacts
// are guarded with no exception: every file and command of their tools goes through
// sandboxhub.View, and the artifacts only through toolhub.Context.GetArtifacts (T-05-08,
// T-11-07, T-11-13). builtin/web, the package of the web tools, is guarded too, with one
// per-package exception for exactly net and net/http (hostAccessNetwork).
var hostAccessGuarded = []string{"components/persistence", "components/skillhub/skill", "components/skillhub", "components/artifacthub/artifact", "components/artifacthub", "components/sandboxhub", "aecmock", "components/agent/agentloop", "components/agent", "components/orchestration", "components/agent/memorysystem", "components/toolhub/builtin/skills", "internal/toolkit", "components/toolhub/builtin/clarify", "components/toolhub/builtin/workspace", "components/toolhub/builtin/artifacts", "components/toolhub/builtin/web", "components/toolhub/builtin/jobs", "components/toolhub/builtin/subagents", "components/secrethub", "components/toolhub/builtin/secrets", "components/toolhub/builtin/preview"}

// hostAccessNetwork are the per-package exceptions to the host-access ban: the import
// paths a guarded package may use although forbiddenHostImports names them. Only
// builtin/web has one, and only for net and net/http: the web tools need sockets and HTTP,
// and the dial-time SSRF check lives there. crypto/tls is not needed (the default client
// TLS configuration is used), and the dial-time check is a generic function, so the
// package never imports syscall. os, os/exec, path/filepath, syscall, crypto/tls and the
// rest stay forbidden in builtin/web, and every other guarded package stays forbidden
// from net and net/http. A path is matched exactly, never by prefix.
var hostAccessNetwork = map[string][]string{"components/toolhub/builtin/web": {"net", "net/http"}}

// hostAccessExempt are the module-relative directory prefixes inside a guarded
// package that may reach the host: the host and Docker sandbox backends, the only
// code of the SDK that touches files, processes and the docker client.
var hostAccessExempt = []string{"components/sandboxhub/host/", "components/sandboxhub/docker/"}

// forbiddenHostImports are the standard library packages that give host file,
// process or network access. net/url is not among them: it only parses text.
// The list is a deny list, so it names every route known to reach the host
// without importing os: io/ioutil (ReadFile, WriteFile, ReadDir), syscall (Open,
// ForkExec, Getenv), os/user and os/signal, plugin (loads host code), the network
// packages that dial without the file importing net itself (net/rpc, net/smtp,
// net/http/httputil, crypto/tls) and cgo (import "C"; its path is "C", which
// isStdlib would classify as standard library). No guarded package imports any of
// them today: the only users of syscall are the two sandbox backends, which are
// exempt (hostAccessExempt). hostAccessDenied lists the names the synthetic test
// expects, so a name dropped from this map fails a test.
var forbiddenHostImports = map[string]bool{
	"os":                true,
	"os/exec":           true,
	"os/signal":         true,
	"os/user":           true,
	"io/ioutil":         true,
	"path/filepath":     true,
	"syscall":           true,
	"plugin":            true,
	"C":                 true,
	"net":               true,
	"net/http":          true,
	"net/rpc":           true,
	"net/smtp":          true,
	"net/http/httputil": true,
	"crypto/tls":        true,
	// Routes that dial, listen, exec or read host files without importing a name above
	// (review WR-B04): textproto.Dial opens TCP connections, cgi execs a process, fcgi,
	// httptest and pprof open listeners or serve host internals, syslog dials, runtime/debug
	// writes heap dumps and crash output to a file descriptor, and the debug/* parsers open
	// host files by name. builtin/web gets only net and net/http (hostAccessNetwork), exact
	// match, so these stay hits there too.
	"net/textproto":     true,
	"net/http/cgi":      true,
	"net/http/fcgi":     true,
	"net/http/httptest": true,
	"net/http/pprof":    true,
	"log/syslog":        true,
	"runtime/debug":     true,
	"debug/elf":         true,
	"debug/pe":          true,
	"debug/macho":       true,
	"debug/buildinfo":   true,
}

// hostAccessDenied is the explicit expectation behind forbiddenHostImports. The
// synthetic tests iterate it (not the map), because iterating the map could never
// notice a name that is missing from it.
var hostAccessDenied = []string{
	"os", "os/exec", "os/signal", "os/user", "io/ioutil", "path/filepath", "syscall", "plugin", "C",
	"net", "net/http", "net/rpc", "net/smtp", "net/http/httputil", "crypto/tls",
	"net/textproto", "net/http/cgi", "net/http/fcgi", "net/http/httptest", "net/http/pprof",
	"log/syslog", "runtime/debug", "debug/elf", "debug/pe", "debug/macho", "debug/buildinfo",
}

// hostAccessImports reports every forbidden import in src, whatever name or alias
// it is imported under (a blank or dot import counts too). It reads the import
// block only, so a comment or a string that names a package is not a hit.
func hostAccessImports(filename string, src []byte) ([]string, error) {
	return forbiddenImports(filename, src, forbiddenHostImports)
}

// hostAccessImportsIn is hostAccessImports for a file of the guarded package pkg: the
// import paths that pkg has an exception for (hostAccessNetwork) are not hits, every
// other forbidden path is. A package without an exception entry gets the full list.
func hostAccessImportsIn(pkg, filename string, src []byte) ([]string, error) {
	allowed := hostAccessNetwork[pkg]
	if len(allowed) == 0 {
		return hostAccessImports(filename, src)
	}
	forbidden := make(map[string]bool, len(forbiddenHostImports))
	for path := range forbiddenHostImports {
		if !contains(allowed, path) {
			forbidden[path] = true
		}
	}
	return forbiddenImports(filename, src, forbidden)
}

// forbiddenImports reports every import in src whose path is a key of forbidden,
// as "file:line: imports path", reading the import block only. It is the shared
// scanner of the host-access, llm-tree and process-exec guards.
func forbiddenImports(filename string, src []byte, forbidden map[string]bool) ([]string, error) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, filename, src, parser.ImportsOnly)
	if err != nil {
		return nil, err
	}
	var hits []string
	for _, imp := range f.Imports {
		path, err := strconv.Unquote(imp.Path.Value)
		if err != nil {
			return nil, err
		}
		if forbidden[path] {
			hits = append(hits, filename+":"+strconv.Itoa(fset.Position(imp.Pos()).Line)+": imports "+path)
		}
	}
	return hits, nil
}

// hostAccessPackage returns the guarded package a module-relative file path (slash
// form) belongs to, or "" when it belongs to none or to an exempt backend. A nested
// guarded package (an entry with a slash) is matched by its directory prefix, so
// builtin/skills/x.go and builtin/skills/sub/x.go are guarded while builtin/skillsx/x.go
// and builtin/x.go are not; the others are matched by their first element.
func hostAccessPackage(file string) string {
	for _, prefix := range hostAccessExempt {
		if strings.HasPrefix(file, prefix) {
			return ""
		}
	}
	// Guarded packages nest (components/agent holds agentloop and
	// memorysystem), so the longest matching prefix names the owning package.
	best := ""
	for _, pkg := range hostAccessGuarded {
		if strings.Contains(pkg, "/") && strings.HasPrefix(file, pkg+"/") && len(pkg) > len(best) {
			best = pkg
		}
	}
	if best != "" {
		return best
	}
	dir, _, found := strings.Cut(file, "/")
	if found && contains(hostAccessGuarded, dir) {
		return dir
	}
	return ""
}

// TestNoHostAccessInStoresAndHubs fails when non-test code of persistence, skill,
// skillhub, artifact, artifacthub, sandbox, aecmock, agentloop, agent,
// orchestration, memorysystem, builtin/skills, internal/toolkit, builtin/clarify,
// builtin/workspace, builtin/artifacts or builtin/web imports os, os/exec,
// path/filepath, net or net/http (sandbox/host and sandbox/docker excepted, and builtin/web
// excepted for net and net/http only, hostAccessNetwork). A
// package without a single checked file fails too, so a renamed or emptied
// directory cannot make the rule pass vacuously. The full ban list is
// forbiddenHostImports (it also names io/ioutil, syscall, os/user, os/signal,
// plugin, cgo's "C", net/rpc, net/smtp, net/http/httputil and crypto/tls).
func TestNoHostAccessInStoresAndHubs(t *testing.T) {
	checked := map[string]int{}
	var hits []string
	for _, file := range goFiles(t) {
		pkg := hostAccessPackage(file)
		if pkg == "" {
			continue
		}
		src, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		h, err := hostAccessImportsIn(pkg, file, src)
		if err != nil {
			t.Fatalf("parse %s: %v", file, err)
		}
		hits = append(hits, h...)
		checked[pkg]++
	}
	for _, pkg := range hostAccessGuarded {
		if checked[pkg] == 0 {
			t.Errorf("no non-test Go file of %s was checked", pkg)
		}
	}
	if len(hits) > 0 {
		t.Fatalf("host access from a store or hub package (use sandboxhub.View):\n%s", strings.Join(hits, "\n"))
	}
}

// TestNoHostAccessCatchesForbiddenImport proves the rule can fail: a synthetic
// source with each forbidden import is reported (also under an alias, blank and dot
// import), a clean source and net/url are not, and the guarded set covers exactly
// the seventeen packages (the twelve of Phases 5 to 10 plus internal/toolkit,
// builtin/clarify, builtin/workspace, builtin/artifacts and builtin/web of Phase 11), with sandbox/host and sandbox/docker as the only
// exemptions (and the net and net/http exception of builtin/web, proven by
// TestNoHostAccessNetworkException).
func TestNoHostAccessCatchesForbiddenImport(t *testing.T) {
	// Iterate the explicit expectation, so a name missing from the map fails here.
	for _, imp := range hostAccessDenied {
		if !forbiddenHostImports[imp] {
			t.Errorf("%s is not in forbiddenHostImports", imp)
		}
		src := []byte("package p\n\nimport (\n\tx " + strconv.Quote(imp) + "\n)\n\nvar _ = x.Anything\n")
		hits, err := hostAccessImports("components/artifacthub/p.go", src)
		if err != nil || len(hits) != 1 || !strings.Contains(hits[0], "imports "+imp) {
			t.Errorf("import %s not caught: %v %v", imp, hits, err)
		}
	}
	if len(forbiddenHostImports) != len(hostAccessDenied) {
		t.Errorf("forbiddenHostImports has %d names, hostAccessDenied %d: keep them in step", len(forbiddenHostImports), len(hostAccessDenied))
	}
	// cgo: a bare import "C" (and the cgo preamble form) is a hit.
	for _, src := range []string{
		"package p\n\nimport \"C\"\n",
		"package p\n\n/*\n#include <stdlib.h>\n*/\nimport \"C\"\n",
	} {
		if hits, err := hostAccessImports("components/sandboxhub/p.go", []byte(src)); err != nil || len(hits) != 1 || !strings.Contains(hits[0], "imports C") {
			t.Errorf("cgo import not caught: %v %v", hits, err)
		}
	}
	for _, form := range []string{"_ \"os\"", ". \"os/exec\"", "\"path/filepath\"", "stdnet \"net/http\"", "\"io/ioutil\"", "_ \"syscall\""} {
		src := []byte("package p\n\nimport " + form + "\n")
		if hits, err := hostAccessImports("components/persistence/p.go", src); err != nil || len(hits) != 1 {
			t.Errorf("import form %s not caught: %v %v", form, hits, err)
		}
	}
	clean := []byte("package p\n\n// os and net/http are only named in this comment\nimport (\n\t\"net/url\"\n\t\"strings\"\n)\n\nvar _ = \"os/exec\"\nvar _ = url.Parse\nvar _ = strings.ToUpper\n")
	if hits, err := hostAccessImports("components/skillhub/skill/p.go", clean); err != nil || len(hits) != 0 {
		t.Errorf("clean source flagged: %v %v", hits, err)
	}
	for _, pkg := range []string{"components/persistence", "components/skillhub/skill", "components/skillhub", "components/artifacthub/artifact", "components/artifacthub", "components/sandboxhub", "aecmock", "components/agent/agentloop", "components/agent", "components/orchestration", "components/agent/memorysystem", "components/toolhub/builtin/skills", "internal/toolkit", "components/toolhub/builtin/clarify", "components/toolhub/builtin/workspace", "components/toolhub/builtin/artifacts", "components/toolhub/builtin/web", "components/toolhub/builtin/jobs", "components/toolhub/builtin/subagents", "components/secrethub", "components/toolhub/builtin/secrets", "components/toolhub/builtin/preview"} {
		if hostAccessPackage(pkg+"/x.go") != pkg {
			t.Errorf("%s is not guarded", pkg)
		}
	}
	if len(hostAccessGuarded) != 22 { // Update (A-126): 18 - promptjob and builtin/scheduling merged into job and builtin/jobs; Update (A-105): 19 - builtin/subagents; Update (A-101): 21 - secrethub (with secrethub/secret) and builtin/secrets; Update (A-124): 22 - builtin/preview
		t.Errorf("the guarded set has %d packages, want exactly 22: %v", len(hostAccessGuarded), hostAccessGuarded)
	}
	// A nested guarded package is matched by its directory prefix: its sub-directories
	// are guarded (builtin/web too, which has its own exception, not an exemption), a
	// package whose name merely starts with the same letters and the builtin directory
	// itself are not.
	if hostAccessPackage("components/toolhub/builtin/skills/sub/x.go") != "components/toolhub/builtin/skills" {
		t.Errorf("components/toolhub/builtin/skills/sub/x.go must be guarded")
	}
	for _, file := range []string{"components/toolhub/builtin/web/x.go", "components/toolhub/builtin/web/sub/x.go"} {
		if hostAccessPackage(file) != "components/toolhub/builtin/web" {
			t.Errorf("%s must be guarded as builtin/web", file)
		}
	}
	for _, file := range []string{"components/toolhub/builtin/skillsx/x.go", "components/toolhub/builtin/webx/x.go", "components/toolhub/builtin/x.go"} {
		if hostAccessPackage(file) != "" {
			t.Errorf("%s must not be guarded", file)
		}
	}
	// The two backends are the only exempt directories of a guarded package.
	for _, file := range []string{"components/sandboxhub/host/x.go", "components/sandboxhub/docker/x.go", "components/sandboxhub/host/sub/x.go"} {
		if hostAccessPackage(file) != "" {
			t.Errorf("%s must be exempt", file)
		}
	}
	for _, file := range []string{"components/sandboxhub/hostile/x.go", "components/sandboxhub/dockerx.go", "components/sandboxhub/x.go", "aecmock/host/x.go"} {
		if hostAccessPackage(file) == "" {
			t.Errorf("%s must stay guarded", file)
		}
	}
	for _, file := range []string{"conformance/x.go", "kernel/x.go", "aec.go", "skillhubx/x.go"} {
		if hostAccessPackage(file) != "" {
			t.Errorf("%s must not be guarded", file)
		}
	}
}

// TestNoHostAccessNetworkException proves the one exception to the host-access ban can
// fail in both directions: builtin/web may import net and net/http and nothing else of
// the forbidden list (the match is exact, so net/http/httputil and net/rpc stay hits),
// and no other guarded package gets the exception. The map itself names exactly one
// package with exactly two paths, both of them forbidden elsewhere.
func TestNoHostAccessNetworkException(t *testing.T) {
	webSrc := func(imports ...string) []byte {
		src := "package web\n\nimport (\n"
		for _, imp := range imports {
			src += "\t_ " + strconv.Quote(imp) + "\n"
		}
		return []byte(src + ")\n")
	}
	if hits, err := hostAccessImportsIn("components/toolhub/builtin/web", "components/toolhub/builtin/web/x.go", webSrc("net/http", "net")); err != nil || len(hits) != 0 {
		t.Errorf("net and net/http must be clean in builtin/web: %v %v", hits, err)
	}
	for _, imp := range []string{
		"os", "os/exec", "path/filepath", "syscall", "crypto/tls", "net/rpc", "net/http/httputil", "io/ioutil", "plugin",
		// the listener, exec and dialing routes of review WR-B04: the exception is exactly net and net/http
		"net/textproto", "net/http/cgi", "net/http/fcgi", "net/http/httptest", "net/http/pprof", "log/syslog", "runtime/debug",
	} {
		hits, err := hostAccessImportsIn("components/toolhub/builtin/web", "components/toolhub/builtin/web/x.go", webSrc(imp))
		if err != nil || len(hits) != 1 || !strings.Contains(hits[0], "imports "+imp) {
			t.Errorf("%s must be a hit in builtin/web: %v %v", imp, hits, err)
		}
	}
	// A clean network import next to a forbidden one: only the forbidden one is a hit.
	if hits, err := hostAccessImportsIn("components/toolhub/builtin/web", "components/toolhub/builtin/web/x.go", webSrc("net/http", "os")); err != nil || len(hits) != 1 || !strings.Contains(hits[0], "imports os") {
		t.Errorf("os next to net/http must be the only hit: %v %v", hits, err)
	}
	// No other guarded package has the exception.
	for _, pkg := range hostAccessGuarded {
		if pkg == "components/toolhub/builtin/web" {
			continue
		}
		for _, imp := range []string{"net/http", "net"} {
			hits, err := hostAccessImportsIn(pkg, pkg+"/x.go", webSrc(imp))
			if err != nil || len(hits) != 1 || !strings.Contains(hits[0], "imports "+imp) {
				t.Errorf("%s must be a hit in %s: %v %v", imp, pkg, hits, err)
			}
		}
	}
	// The map names exactly one package with exactly two paths.
	if len(hostAccessNetwork) != 1 {
		t.Fatalf("hostAccessNetwork names %d packages, want exactly 1: %v", len(hostAccessNetwork), hostAccessNetwork)
	}
	paths, ok := hostAccessNetwork["components/toolhub/builtin/web"]
	if !ok || len(paths) != 2 || !contains(paths, "net") || !contains(paths, "net/http") {
		t.Errorf("hostAccessNetwork must allow exactly net and net/http in builtin/web: %v", hostAccessNetwork)
	}
	for pkg, allowed := range hostAccessNetwork {
		if !contains(hostAccessGuarded, pkg) {
			t.Errorf("%s has an exception but is not guarded", pkg)
		}
		for _, path := range allowed {
			if !forbiddenHostImports[path] {
				t.Errorf("%s: exception for %s, which is not forbidden anyway", pkg, path)
			}
		}
	}
}
