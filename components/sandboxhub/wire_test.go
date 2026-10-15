package sandboxhub_test

import (
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/Phong95/aec-agent-go/components/sandboxhub"
	"github.com/Phong95/aec-agent-go/internal/jsonx"
	"github.com/Phong95/aec-agent-go/internal/wiretest"
	"github.com/Phong95/aec-agent-go/kernel/ordered"
)

// sandboxWire mirrors the object sandbox-wire.json holds, one typed field per
// key in fixture order. The values were produced by a real in-memory sandbox
// (traits, limits, capabilities and the five results) or authored in declaration
// order (the rest).
type sandboxWire struct {
	Traits           sandboxhub.Traits        `json:"traits"`
	Limits           sandboxhub.Limits        `json:"limits"`
	Capabilities     []sandboxhub.Capability  `json:"capabilities"`
	WriteResult      sandboxhub.WriteResult   `json:"writeResult"`
	FileContent      sandboxhub.FileContent   `json:"fileContent"`
	FileStat         sandboxhub.FileStat      `json:"fileStat"`
	ListResult       sandboxhub.ListResult    `json:"listResult"`
	SearchResult     sandboxhub.SearchResult  `json:"searchResult"`
	ExecResult       sandboxhub.ExecResult    `json:"execResult"`
	ExecResultKilled sandboxhub.ExecResult    `json:"execResultKilled"`
	ReadOptions      sandboxhub.ReadOptions   `json:"readOptions"`
	WriteOptions     sandboxhub.WriteOptions  `json:"writeOptions"`
	ListOptions      sandboxhub.ListOptions   `json:"listOptions"`
	SearchQuery      sandboxhub.SearchQuery   `json:"searchQuery"`
	ExecRequest      sandboxhub.ExecRequest   `json:"execRequest"`
	Spec             sandboxhub.Spec          `json:"spec"`
	CreateContext    sandboxhub.CreateContext `json:"createContext"`
	HubPolicy        sandboxhub.HubPolicy     `json:"hubPolicy"`
}

func TestSandboxWireFixture(t *testing.T) {
	var w sandboxWire
	wiretest.AssertRoundTrip(t, "sandbox-wire.json", &w)

	if w.Traits.Kind != sandboxhub.KindDocker || w.Traits.Tenancy != sandboxhub.TenancyShared || w.Traits.OS != sandboxhub.OSLinux || !w.Traits.Isolated {
		t.Errorf("traits decoded wrongly: %+v", w.Traits)
	}
	wantCaps := []sandboxhub.Capability{sandboxhub.CapabilityFSRead, sandboxhub.CapabilityFSWrite, sandboxhub.CapabilityFSSearch, sandboxhub.CapabilityExec}
	if !slices.Equal(w.Capabilities, wantCaps) {
		t.Errorf("capabilities = %v, want %v", w.Capabilities, wantCaps)
	}

	// Explicit zero values must survive as present, not become absent.
	if w.ReadOptions.MaxBytes == nil || *w.ReadOptions.MaxBytes != 0 {
		t.Error("readOptions maxBytes 0 must survive")
	}
	if w.ListOptions.Recursive == nil || *w.ListOptions.Recursive {
		t.Error("listOptions recursive false must survive")
	}
	if w.SearchQuery.CaseSensitive == nil || *w.SearchQuery.CaseSensitive {
		t.Error("searchQuery caseSensitive false must survive")
	}
	if w.ExecRequest.Shell == nil || *w.ExecRequest.Shell {
		t.Error("execRequest shell false must survive")
	}
	if w.WriteOptions.CreateDirs == nil || *w.WriteOptions.CreateDirs {
		t.Error("writeOptions createDirs false must survive")
	}
	if w.Spec.Limits == nil || w.Spec.Limits.Pids == nil || *w.Spec.Limits.Pids != 0 {
		t.Error("spec limits pids 0 must survive")
	}
	// Options keep the authored (non-sorted) key order at every nesting level.
	if got := w.Spec.Options.Keys(); !slices.Equal(got, []string{"retries", "folder", "nested"}) {
		t.Errorf("spec options keys = %v, want [retries folder nested]", got)
	}
	if v, ok := w.Spec.Options.Get("nested"); !ok {
		t.Error("nested spec option missing")
	} else if n, ok := v.(ordered.Object); !ok || !slices.Equal(n.Keys(), []string{"b", "a"}) {
		t.Errorf("nested spec option = %#v, want an ordered.Object with keys [b a]", v)
	}
	if w.ExecResult.ExitCode == nil || *w.ExecResult.ExitCode != 0 {
		t.Error("an exit code 0 must decode as a non-nil zero")
	}
	if w.ExecResultKilled.ExitCode != nil {
		t.Error("an exit code null must decode as nil")
	}
	if !strings.Contains(w.FileContent.Content, "<&>") {
		t.Error("raw <&> must survive in file content")
	}
}

// sandboxFixtures is every golden fixture owned by package sandbox.
var sandboxFixtures = []string{
	"sandbox-approval.json",
	"sandbox-hub.json",
	"sandbox-logical-path.json",
	"sandbox-traced.json",
	"sandbox-wire.json",
}

// TestSandboxFixturesCovered fails when a fixture with the sandbox- prefix has
// no entry above, and when an entry has no file, so a fixture added to wiregen
// cannot go unchecked.
func TestSandboxFixturesCovered(t *testing.T) {
	entries, err := os.ReadDir(wiretest.Dir(t))
	if err != nil {
		t.Fatalf("read fixture dir: %v", err)
	}
	present := map[string]bool{}
	for _, e := range entries {
		present[e.Name()] = true
		if strings.HasPrefix(e.Name(), "sandbox-") && !slices.Contains(sandboxFixtures, e.Name()) {
			t.Errorf("fixture %s has no entry in sandboxFixtures", e.Name())
		}
	}
	for _, f := range sandboxFixtures {
		if !present[f] {
			t.Errorf("sandboxFixtures entry %s has no fixture file", f)
		}
	}
}

func encode(t *testing.T, v any) string {
	t.Helper()
	b, err := jsonx.Marshal(v)
	if err != nil {
		t.Fatalf("encode %T: %v", v, err)
	}
	return string(b)
}

func TestExecResultExitCodeNull(t *testing.T) {
	killed := sandboxhub.ExecResult{Stdout: "x", TimedOut: true}
	got := encode(t, killed)
	want := `{"exitCode":null,"stdout":"x","stderr":"","truncated":false,"timedOut":true,"cancelled":false,"durationMs":0}`
	if got != want {
		t.Errorf("killed result:\n got %s\nwant %s", got, want)
	}

	zero := 0
	ok := sandboxhub.ExecResult{ExitCode: &zero, DurationMs: 7}
	if got := encode(t, ok); !strings.HasPrefix(got, `{"exitCode":0,`) {
		t.Errorf("exit code 0 must be written as 0: %s", got)
	}
}

func TestSandboxNilCollections(t *testing.T) {
	if got := encode(t, sandboxhub.ListResult{}); got != `{"entries":[],"truncated":false}` {
		t.Errorf("nil entries: %s", got)
	}
	if got := encode(t, sandboxhub.SearchResult{}); got != `{"matches":[],"truncated":false}` {
		t.Errorf("nil matches: %s", got)
	}
	if got := encode(t, sandboxhub.ExecRequest{TimeoutMs: 1, MaxOutputBytes: 2}); got != `{"command":[],"timeoutMs":1,"maxOutputBytes":2}` {
		t.Errorf("nil command: %s", got)
	}
	// A pointer must encode the same way as a value.
	if got := encode(t, &sandboxhub.ListResult{}); got != `{"entries":[],"truncated":false}` {
		t.Errorf("pointer to nil entries: %s", got)
	}
	// The caller's value is not changed by the encoding.
	lr := sandboxhub.ListResult{}
	_ = encode(t, lr)
	if lr.Entries != nil {
		t.Error("encoding must not write the normalised slice back")
	}
}

func TestApprovalPolicyAndOperations(t *testing.T) {
	var policy sandboxhub.ApprovalPolicy = func(traits sandboxhub.Traits, op sandboxhub.Operation) bool {
		return !traits.Isolated && op == sandboxhub.OperationExec
	}
	host := sandboxhub.Traits{Kind: sandboxhub.KindHost}
	if !policy(host, sandboxhub.OperationExec) || policy(host, sandboxhub.OperationFSRead) {
		t.Error("approval policy must receive the traits and the operation")
	}
	ops := []sandboxhub.Operation{sandboxhub.OperationFSRead, sandboxhub.OperationFSWrite, sandboxhub.OperationFSSearch, sandboxhub.OperationExec}
	for _, op := range ops {
		if string(op) == string(sandboxhub.CapabilityExecShell) {
			t.Errorf("exec.shell is a capability, not an operation: %s", op)
		}
	}
}

// TestHubPolicyNilSlicesEncodeAsEmptyArrays pins IN-03: the three arrays are
// required on the wire, so a zero HubPolicy must not write null.
func TestHubPolicyNilSlicesEncodeAsEmptyArrays(t *testing.T) {
	got, err := jsonx.Marshal(sandboxhub.HubPolicy{})
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{`"allowedImages":[]`, `"allowedNetwork":[]`, `"ownKinds":[]`} {
		if !strings.Contains(string(got), key) {
			t.Errorf("zero HubPolicy encodes %s, want %s", got, key)
		}
	}
	if strings.Contains(string(got), "null") {
		t.Errorf("zero HubPolicy encodes null: %s", got)
	}
}
