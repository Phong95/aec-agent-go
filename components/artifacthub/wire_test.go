package artifacthub_test

import (
	"encoding/json"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/Phong95/aec-agent-go/aecmock"
	"github.com/Phong95/aec-agent-go/components/artifacthub"
	"github.com/Phong95/aec-agent-go/components/artifacthub/artifact"
	"github.com/Phong95/aec-agent-go/components/persistence"
	"github.com/Phong95/aec-agent-go/components/sandboxhub"
	"github.com/Phong95/aec-agent-go/internal/jsonx"
	"github.com/Phong95/aec-agent-go/internal/wiretest"
	"github.com/Phong95/aec-agent-go/kernel"
)

// artifactWireFixture is the part of artifact-wire.json the hub test replays: the
// recorded results of the view, kept raw so the comparison is on the exact bytes.
type artifactWireFixture struct {
	Write       json.RawMessage `json:"write"`
	ReadCurrent json.RawMessage `json:"readCurrent"`
	ReadChanged json.RawMessage `json:"readChanged"`
	ReadEmpty   json.RawMessage `json:"readEmpty"`
	ReadMissing json.RawMessage `json:"readMissing"`
	List        json.RawMessage `json:"list"`
	Versions    json.RawMessage `json:"versions"`
}

// encode returns the wire bytes of v.
func encode(t *testing.T, v any) []byte {
	t.Helper()
	b, err := kernel.EncodeJSON(v)
	if err != nil {
		t.Fatalf("encode %T: %v", v, err)
	}
	return b
}

// requireWire fails unless the wire bytes of got equal the fixture field.
func requireWire(t *testing.T, label string, got any, want json.RawMessage) {
	t.Helper()
	if b := encode(t, got); string(b) != string(want) {
		t.Errorf("%s differs\n got: %s\nwant: %s", label, b, want)
	}
}

// TestArtifactWireReproduced replays the steps of the wiregen case artifact-wire
// (save, read current, changed, empty and missing, update, list, versions) over
// the real aecmock in-memory sandbox, and requires the same bytes.
func TestArtifactWireReproduced(t *testing.T) {
	ctx := t.Context()
	var fx artifactWireFixture
	if err := json.Unmarshal(wiretest.Load(t, "artifact-wire.json"), &fx); err != nil {
		t.Fatalf("decode artifact-wire.json: %v", err)
	}

	// The fixture's timestamps are .005Z, .007Z and .009Z, not .000Z, .001Z and
	// .002Z: the recording's in-memory sandbox also consumed a tick of the clock
	// on each of its own writes between the hub's reads. The real Go in-memory
	// sandbox consumes the same ticks (two when the sandbox is created,
	// one for the agent folder, one per missing directory and one for each file it
	// writes), so ONE deterministic clock injected into both the sandbox and the hub
	// reproduces the recorded interleaving without scripting any timestamp. A scripted
	// clock that failed the test on an extra read, as the Phase 6 stop-gap needed,
	// is not needed any more: a different tick order shows up as a different
	// timestamp in the fixture bytes.
	clock := wiretest.Clock(1700000000000, 1)
	store := persistence.NewMemoryArtifactStore()
	hub := artifacthub.NewArtifactHub(
		func() persistence.ArtifactStore { return store },
		artifacthub.WithClock(clock),
		artifacthub.WithIDs(wiretest.IDs()),
	)
	view, err := hub.ForAgent("agent-1", "session-1")
	if err != nil {
		t.Fatal(err)
	}
	provider := aecmock.NewInMemoryProvider(aecmock.WithClock(clock))
	sb, err := provider.Create(ctx, sandboxhub.Spec{Kind: sandboxhub.KindDocker},
		sandboxhub.CreateContext{ID: "docker:shared", Tenancy: sandboxhub.TenancyShared})
	if err != nil {
		t.Fatal(err)
	}
	fv, err := sb.ForAgent(ctx, "agent-1")
	if err != nil {
		t.Fatal(err)
	}

	body := "# Clash report\n<&>\n"
	write, err := view.Save(ctx, artifact.SaveInput{
		Name: "clash-report.md", Type: "text/markdown", Content: &body,
		Description: "Clashes found in the model", Tags: []string{"clash", "qa"},
		Note: "first cut", Tool: "save_artifact",
	}, fv)
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	requireWire(t, "write", write, fx.Write)

	readCurrent, err := view.Read(ctx, artifact.Ref{Name: "clash-report.md"}, fv, artifact.ReadOptions{})
	if err != nil {
		t.Fatal(err)
	}
	requireWire(t, "readCurrent", readCurrent, fx.ReadCurrent)

	// The file changes behind the artifact's back: a "changed" read.
	if _, err := fv.WriteFile(ctx, write.Artifact.SandboxPath, "# Clash report\nrevised\n", sandboxhub.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	readChanged, err := view.Read(ctx, artifact.Ref{Name: "clash-report.md"}, fv, artifact.ReadOptions{})
	if err != nil {
		t.Fatal(err)
	}
	requireWire(t, "readChanged", readChanged, fx.ReadChanged)

	// Re-track the new content: version 2.
	one := 1
	if _, err := view.Update(ctx, artifact.UpdateInput{
		Ref: artifact.Ref{Name: "clash-report.md"}, Note: "re-tracked", ExpectedVersion: &one,
	}, fv); err != nil {
		t.Fatalf("Update: %v", err)
	}

	// An empty file keeps "content":"".
	empty := ""
	if _, err := view.Save(ctx, artifact.SaveInput{Name: "empty.txt", Type: "text/plain", Content: &empty}, fv); err != nil {
		t.Fatalf("Save empty: %v", err)
	}
	readEmpty, err := view.Read(ctx, artifact.Ref{Name: "empty.txt"}, fv, artifact.ReadOptions{})
	if err != nil {
		t.Fatal(err)
	}
	requireWire(t, "readEmpty", readEmpty, fx.ReadEmpty)

	// Delete the file with the sandbox's own rm, as the recorded scenario does (it takes
	// two clock ticks): a "missing" read produced by the real read path.
	rm, err := fv.Exec(ctx, sandboxhub.ExecRequest{
		Command: []string{"rm", write.Artifact.SandboxPath}, TimeoutMs: 5000, MaxOutputBytes: 1000,
	}, sandboxhub.ExecOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if rm.ExitCode == nil || *rm.ExitCode != 0 {
		t.Fatalf("rm = %+v, want exit code 0", rm)
	}
	readMissing, err := view.Read(ctx, artifact.Ref{Name: "clash-report.md"}, fv, artifact.ReadOptions{})
	if err != nil {
		t.Fatal(err)
	}
	requireWire(t, "readMissing", readMissing, fx.ReadMissing)

	list, err := view.List(ctx, artifact.Query{})
	if err != nil {
		t.Fatal(err)
	}
	requireWire(t, "list", list, fx.List)

	versions, err := view.Versions(ctx, artifact.Ref{Name: "clash-report.md"})
	if err != nil {
		t.Fatal(err)
	}
	requireWire(t, "versions", versions, fx.Versions)

	if readCurrent.File.Status != artifact.FileCurrent || readChanged.File.Status != artifact.FileChanged ||
		readEmpty.File.Status != artifact.FileCurrent || readMissing.File.Status != artifact.FileMissing {
		t.Errorf("file statuses = %s %s %s %s", readCurrent.File.Status, readChanged.File.Status, readEmpty.File.Status, readMissing.File.Status)
	}
}

// artifacthubFixtures is every golden fixture owned by package artifacthub.
var artifacthubFixtures = []string{
	"artifacthub-errors.json",
}

// TestArtifacthubFixturesCovered fails when a fixture with the prefix artifacthub-
// has no entry above, and when an entry has no file, so a fixture added to wiregen
// cannot go unchecked.
func TestArtifacthubFixturesCovered(t *testing.T) {
	entries, err := os.ReadDir(wiretest.Dir(t))
	if err != nil {
		t.Fatalf("read fixture dir: %v", err)
	}
	present := map[string]bool{}
	for _, e := range entries {
		present[e.Name()] = true
		if strings.HasPrefix(e.Name(), "artifacthub-") && !slices.Contains(artifacthubFixtures, e.Name()) {
			t.Errorf("fixture %s has no entry in artifacthubFixtures", e.Name())
		}
	}
	for _, f := range artifacthubFixtures {
		if !present[f] {
			t.Errorf("artifacthubFixtures entry %s has no fixture file", f)
		}
	}
}

// unmarshalStrict is a small alias used by the golden tests.
func unmarshalStrict(t *testing.T, data []byte, v any) {
	t.Helper()
	if err := jsonx.UnmarshalStrict(data, v); err != nil {
		t.Fatalf("decode: %v", err)
	}
}
