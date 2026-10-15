package arch_test

// Dependency guards (FND-01, threat T-04-15). The module targets Go 1.25.0; a
// dependency that declares a newer go directive would force a toolchain switch
// or a build failure for every consumer.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"go/version"
	"os/exec"
	"slices"
	"strings"
	"testing"
)

// goFloor is the newest go directive any module in the build list may declare.
const goFloor = "go1.25.0"

// exceedsFloor reports whether a go directive (for example "1.25.1") is newer
// than the floor.
func exceedsFloor(goVersion string) bool {
	return version.Compare("go"+goVersion, goFloor) > 0
}

func TestGoVersionFloorCompare(t *testing.T) {
	cases := []struct {
		in   string
		over bool
	}{
		{"1.13", false},
		{"1.20", false},
		{"1.25", false},
		{"1.25.0", false},
		{"1.25.1", true},
		{"1.26.0", true},
	}
	for _, c := range cases {
		if got := exceedsFloor(c.in); got != c.over {
			t.Errorf("exceedsFloor(%q) = %v, want %v", c.in, got, c.over)
		}
	}
}

// runGo runs the go tool in the module root (the working directory of this
// root-package test) and returns stdout.
func runGo(t *testing.T, args ...string) []byte {
	t.Helper()
	var stderr bytes.Buffer
	cmd := exec.CommandContext(t.Context(), "go", args...)
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("go %v: %v\n%s", args, err, stderr.String())
	}
	return out
}

// TestDependencyGoFloor fails when any module in the selected build list
// declares a go directive newer than 1.25.0. The selected build list is what
// the Go 1.25 toolchain must satisfy, so this is the go mod graph floor.
// If it ever fires, pin the offending golang.org/x/* module to an older release.
// A go list failure fails the test: CI has network access and locally the
// module cache is populated by go mod tidy.
func TestDependencyGoFloor(t *testing.T) {
	out := runGo(t, "list", "-m", "-json", "all")
	var offenders []string
	checked := 0
	for dec := json.NewDecoder(bytes.NewReader(out)); dec.More(); {
		var m struct {
			Path      string
			Version   string
			GoVersion string
		}
		if err := dec.Decode(&m); err != nil {
			t.Fatalf("decode go list -m output: %v", err)
		}
		checked++
		if m.GoVersion == "" {
			continue
		}
		if exceedsFloor(m.GoVersion) {
			offenders = append(offenders, fmt.Sprintf("%s@%s declares go %s", m.Path, m.Version, m.GoVersion))
		}
	}
	if checked == 0 {
		t.Fatal("go list -m all returned no modules")
	}
	for _, o := range offenders {
		t.Error(o)
	}
}

func TestGoModDirective(t *testing.T) {
	out := runGo(t, "mod", "edit", "-json")
	var mod struct {
		Go        string
		Toolchain string
	}
	if err := json.Unmarshal(out, &mod); err != nil {
		t.Fatalf("decode go mod edit -json: %v", err)
	}
	if mod.Go != "1.25.0" {
		t.Errorf("go.mod go directive = %q, want 1.25.0", mod.Go)
	}
	if mod.Toolchain != "" {
		t.Errorf("go.mod has a toolchain directive %q; it must not", mod.Toolchain)
	}
}

// vendorSDKPrefixes are the module path prefixes of the vendor SDKs of Phase 8:
// Anthropic, OpenAI and AWS (which includes smithy-go). Only the provider leaves
// llm/anthropic, llm/openaicompat and llm/bedrock may link them (vendor_test.go
// vendorAllowed).
var vendorSDKPrefixes = []string{"github.com/anthropics/", "github.com/openai/", "github.com/aws/"}

// vendorDeps returns the entries of deps (import paths, as `go list -deps`
// prints them) that belong to a vendor SDK module, in their original order.
func vendorDeps(deps []string) []string {
	var found []string
	for _, d := range deps {
		for _, prefix := range vendorSDKPrefixes {
			if strings.HasPrefix(d, prefix) {
				found = append(found, d)
				break
			}
		}
	}
	return found
}

func TestVendorDepsDetectsSDKs(t *testing.T) {
	deps := []string{
		"fmt",
		"net/http",
		"github.com/Phong95/aec-agent-go/kernel",
		"github.com/anthropics/anthropic-sdk-go/option",
		"github.com/openai/openai-go/v3",
		"github.com/aws/aws-sdk-go-v2/service/bedrockruntime",
		"github.com/aws/smithy-go",
		"golang.org/x/sync/errgroup",
		"go.uber.org/goleak",
		"github.com/awsome/other",
	}
	want := []string{
		"github.com/anthropics/anthropic-sdk-go/option",
		"github.com/openai/openai-go/v3",
		"github.com/aws/aws-sdk-go-v2/service/bedrockruntime",
		"github.com/aws/smithy-go",
	}
	got := vendorDeps(deps)
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("vendorDeps = %v, want %v", got, want)
	}
	if got := vendorDeps([]string{"fmt", "github.com/Phong95/aec-agent-go/components/llmhub"}); len(got) != 0 {
		t.Fatalf("a vendor-free list reported %v", got)
	}
	if got := vendorDeps(nil); len(got) != 0 {
		t.Fatalf("nil list reported %v", got)
	}
}

// hostSandboxPackages are the import paths of the two sandbox backends that reach
// the machine the program runs on: sandbox/host (real folders and processes, not
// isolated) and sandbox/docker (a Docker daemon). Both are opt-in packages; the root
// never links them.
var hostSandboxPackages = []string{archModule + "/components/sandboxhub/host", archModule + "/components/sandboxhub/docker"}

// hostSandboxDeps returns the entries of deps (import paths, as `go list -deps`
// prints them) that are one of the sandbox backends, in their original order. It
// compares whole paths, so a package such as sandbox/hostile or a package below a
// backend never matches.
func hostSandboxDeps(deps []string) []string {
	var found []string
	for _, d := range deps {
		if slices.Contains(hostSandboxPackages, d) {
			found = append(found, d)
		}
	}
	return found
}

func TestHostSandboxDepsDetectsBackends(t *testing.T) {
	deps := []string{
		"fmt",
		archModule + "/components/sandboxhub",
		archModule + "/components/sandboxhub/host",
		archModule + "/components/sandboxhub/hostile",
		archModule + "/components/sandboxhub/host/internal/x",
		archModule + "/components/sandboxhub/docker",
		"github.com/other/sandbox/host",
	}
	want := []string{archModule + "/components/sandboxhub/host", archModule + "/components/sandboxhub/docker"}
	if got := hostSandboxDeps(deps); !slices.Equal(got, want) {
		t.Fatalf("hostSandboxDeps = %v, want %v", got, want)
	}
	if got := hostSandboxDeps([]string{"fmt", archModule + "/components/sandboxhub"}); len(got) != 0 {
		t.Fatalf("a backend-free list reported %v", got)
	}
	if got := hostSandboxDeps(nil); len(got) != 0 {
		t.Fatalf("nil list reported %v", got)
	}
}

// TestRootDoesNotLinkVendorSDKs runs go list -deps over the root package and
// every package a consumer imports without a provider (kernel, llm, tool,
// persistence, aecmock, conformance) and fails when a vendor SDK or one of the
// two sandbox backends (sandbox/host, sandbox/docker) is among their non-test
// dependencies. This is the guard behind the rule that the heavy providers and the
// host and Docker sandboxes are opt-in packages: importing the root costs no vendor
// code and gives no access to the host machine. An empty package list fails the test, so a
// broken go list cannot pass it.
func TestRootDoesNotLinkVendorSDKs(t *testing.T) {
	out := runGo(t, "list", "-deps", ".", "./kernel", "./components/llmhub", "./components/toolhub", "./components/persistence", "./aecmock", "./conformance")
	deps := strings.Fields(string(out))
	if len(deps) == 0 {
		t.Fatal("go list -deps returned no packages")
	}
	if !slices.Contains(deps, archModule+"/kernel") {
		t.Fatalf("go list -deps did not list the kernel package: %v", deps[:min(len(deps), 5)])
	}
	if found := vendorDeps(deps); len(found) > 0 {
		t.Fatalf("a vendor SDK is linked by the root, kernel, llm, tool, persistence, aecmock or conformance package:\n%s", strings.Join(found, "\n"))
	}
	if found := hostSandboxDeps(deps); len(found) > 0 {
		t.Fatalf("a sandbox backend is linked by the root, kernel, llm, tool, persistence, aecmock or conformance package (they are opt-in packages):\n%s", strings.Join(found, "\n"))
	}
}

// TestHostSandboxGuardSeesBackends is the positive control of the sandbox half of
// TestRootDoesNotLinkVendorSDKs: sandbox/host and sandbox/docker import their way
// into a program only by an explicit import, and go list -deps over the two
// packages must make hostSandboxDeps report both paths. Without it the root guard
// would pass just as well if hostSandboxDeps or go list silently saw nothing.
func TestHostSandboxGuardSeesBackends(t *testing.T) {
	out := runGo(t, "list", "-deps", "./components/sandboxhub/host", "./components/sandboxhub/docker")
	deps := strings.Fields(string(out))
	found := hostSandboxDeps(deps)
	for _, p := range hostSandboxPackages {
		if !slices.Contains(found, p) {
			t.Errorf("go list -deps over the backends does not report %s; the sandbox guard would be vacuous (got %v)", p, found)
		}
	}
}

// TestVendorGuardSeesLLMAll is the positive control of TestRootDoesNotLinkVendorSDKs:
// llm/all links the three vendor SDK modules, so vendorDeps must find a path under
// each of them in its go list -deps output. Without it the root guard would pass
// just as well if vendorDeps or go list silently saw nothing.
func TestVendorGuardSeesLLMAll(t *testing.T) {
	out := runGo(t, "list", "-deps", "./components/llmhub/providers/all")
	deps := strings.Fields(string(out))
	if !slices.Contains(deps, archModule+"/components/llmhub/providers/all") {
		t.Fatalf("go list -deps did not list llm/all: %v", deps[max(0, len(deps)-5):])
	}
	found := vendorDeps(deps)
	for _, prefix := range vendorSDKPrefixes {
		if !slices.ContainsFunc(found, func(d string) bool { return strings.HasPrefix(d, prefix) }) {
			t.Errorf("components/llmhub/providers/all links no package under %s; the vendor guard would be vacuous", prefix)
		}
	}
}

// credentialChainPackages are the AWS SDK packages that read the environment, shared
// credential files or the instance metadata service, or that run a process: the
// default credential chain (config), its providers, and the token services they
// call. The Bedrock provider takes only the static keys of llmhub.ProviderConfig, so
// none of them may be linked into llm/bedrock, directly or through another package
// (review WR-04). The exact import allow-list in vendor_test.go covers the direct
// imports; this covers what the packages it admits would pull in.
var credentialChainPackages = []string{
	"github.com/aws/aws-sdk-go-v2/config",
	"github.com/aws/aws-sdk-go-v2/credentials/processcreds",
	"github.com/aws/aws-sdk-go-v2/credentials/ssocreds",
	"github.com/aws/aws-sdk-go-v2/credentials/stscreds",
	"github.com/aws/aws-sdk-go-v2/credentials/endpointcreds",
	"github.com/aws/aws-sdk-go-v2/feature/ec2/imds",
	"github.com/aws/aws-sdk-go-v2/service/sso",
	"github.com/aws/aws-sdk-go-v2/service/ssooidc",
	"github.com/aws/aws-sdk-go-v2/service/sts",
	"github.com/aws/aws-sdk-go-v2/service/signin",
}

// linkedFrom returns the entries of deps that are one of the packages, or lie below
// one of them.
func linkedFrom(deps, packages []string) []string {
	var found []string
	for _, d := range deps {
		for _, p := range packages {
			if d == p || strings.HasPrefix(d, p+"/") {
				found = append(found, d)
				break
			}
		}
	}
	return found
}

// TestLinkedFromDetectsCredentialChain proves the matcher can fail: a dependency
// list that holds the config package, a package below a provider and the IMDS client
// is reported, and a list of the packages llm/bedrock really links is not.
func TestLinkedFromDetectsCredentialChain(t *testing.T) {
	deps := []string{
		"github.com/aws/aws-sdk-go-v2/credentials",
		"github.com/aws/aws-sdk-go-v2/config",
		"github.com/aws/aws-sdk-go-v2/credentials/ssocreds/internal/x",
		"github.com/aws/aws-sdk-go-v2/feature/ec2/imds",
		"github.com/aws/aws-sdk-go-v2/service/bedrockruntime",
		"github.com/aws/aws-sdk-go-v2/configsources",
	}
	got := linkedFrom(deps, credentialChainPackages)
	want := []string{deps[1], deps[2], deps[3]}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("linkedFrom = %v, want %v", got, want)
	}
	if got := linkedFrom([]string{"github.com/aws/aws-sdk-go-v2/credentials", "github.com/aws/aws-sdk-go-v2/service/bedrock"}, credentialChainPackages); len(got) != 0 {
		t.Fatalf("the packages llm/bedrock needs were reported: %v", got)
	}
}

// TestBedrockLinksNoCredentialChain runs go list -deps over llm/bedrock and fails
// when a package of the default credential chain is among its dependencies. The
// positive control is the static credentials package, which must be there, so a
// broken go list cannot make the test pass.
func TestBedrockLinksNoCredentialChain(t *testing.T) {
	out := runGo(t, "list", "-deps", "./components/llmhub/providers/bedrock")
	deps := strings.Fields(string(out))
	if !slices.Contains(deps, "github.com/aws/aws-sdk-go-v2/credentials") || !slices.Contains(deps, archModule+"/components/llmhub/providers/bedrock") {
		t.Fatalf("go list -deps did not list the static credentials package and llm/bedrock: %v", deps[max(0, len(deps)-5):])
	}
	if found := linkedFrom(deps, credentialChainPackages); len(found) > 0 {
		t.Fatalf("components/llmhub/providers/bedrock links the AWS default credential chain (environment, shared files, IMDS or a process):\n%s", strings.Join(found, "\n"))
	}
}
