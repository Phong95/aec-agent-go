package artifact_test

import (
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/Phong95/aec-agent-go/components/artifacthub/artifact"
	"github.com/Phong95/aec-agent-go/internal/jsonx"
	"github.com/Phong95/aec-agent-go/internal/wiretest"
)

// artifactWire mirrors the object artifact-wire.json holds, fields in fixture
// order. Everything except query, readOptions, ref, saveInput and updateInput
// was produced by a real ArtifactHub over a real MemoryArtifactStore and a real
// in-memory sandbox.
type artifactWire struct {
	Write       artifact.Write       `json:"write"`
	ReadCurrent artifact.Read        `json:"readCurrent"`
	ReadChanged artifact.Read        `json:"readChanged"`
	ReadEmpty   artifact.Read        `json:"readEmpty"`
	ReadMissing artifact.Read        `json:"readMissing"`
	List        []artifact.Artifact  `json:"list"`
	Versions    []artifact.Version   `json:"versions"`
	Query       artifact.Query       `json:"query"`
	ReadOptions artifact.ReadOptions `json:"readOptions"`
	Ref         artifact.Ref         `json:"ref"`
	SaveInput   artifact.SaveInput   `json:"saveInput"`
	UpdateInput artifact.UpdateInput `json:"updateInput"`
	// UpdateInputClearDescription carries an explicit empty description, which
	// clears the stored one and so must not collapse into an absent key.
	UpdateInputClearDescription artifact.UpdateInput `json:"updateInputClearDescription"`
}

func TestArtifactWireFixture(t *testing.T) {
	var w artifactWire
	wiretest.AssertRoundTrip(t, "artifact-wire.json", &w)

	if w.ReadCurrent.File.Status != artifact.FileCurrent ||
		w.ReadChanged.File.Status != artifact.FileChanged ||
		w.ReadEmpty.File.Status != artifact.FileCurrent ||
		w.ReadMissing.File.Status != artifact.FileMissing {
		t.Fatalf("file statuses: %q %q %q %q", w.ReadCurrent.File.Status, w.ReadChanged.File.Status, w.ReadEmpty.File.Status, w.ReadMissing.File.Status)
	}
	if w.ReadMissing.File.Reason == "" || w.ReadMissing.File.Content != "" {
		t.Errorf("missing file must carry a reason and no content: %+v", w.ReadMissing.File)
	}
	if w.Write.Version.Origin.Tool != "save_artifact" || w.Write.Version.Origin.SessionID != "session-1" {
		t.Errorf("origin lost: %+v", w.Write.Version.Origin)
	}
	if w.Versions[0].Origin.Tool != "" || w.Versions[0].Origin.SessionID == "" {
		t.Errorf("an omitted origin tool must stay omitted: %+v", w.Versions[0].Origin)
	}
	if w.List[0].Description != "" || w.List[0].Tags != nil {
		t.Errorf("an artifact without description and tags must keep them unset: %+v", w.List[0])
	}

	// Explicit zero values survive as present.
	if w.ReadOptions.MaxBytes == nil || *w.ReadOptions.MaxBytes != 0 {
		t.Error("readOptions maxBytes 0 must survive")
	}
	if w.SaveInput.Content == nil || *w.SaveInput.Content != "" {
		t.Error("saveInput content \"\" (an empty file) must survive")
	}
	if w.UpdateInput.Content == nil || *w.UpdateInput.Content != "" {
		t.Error("updateInput content \"\" must survive")
	}
	if w.UpdateInput.ExpectedVersion == nil || *w.UpdateInput.ExpectedVersion != 0 {
		t.Error("updateInput expectedVersion 0 must survive")
	}
	if w.UpdateInput.Tags == nil || len(w.UpdateInput.Tags) != 0 {
		t.Error("updateInput tags [] (clear the tags) must survive")
	}
	if d := w.UpdateInputClearDescription.Description; d == nil || *d != "" {
		t.Errorf("updateInput description \"\" (clear it) must survive, got %v", d)
	}
	if w.UpdateInput.Description == nil || *w.UpdateInput.Description != "d" {
		t.Errorf("updateInput description \"d\" must survive, got %v", w.UpdateInput.Description)
	}
	if w.UpdateInput.ID == "" || w.UpdateInput.Name == "" {
		t.Errorf("the embedded Ref must encode inline: %+v", w.UpdateInput.Ref)
	}
}

// artifactFixtures is every golden fixture owned by package artifact.
var artifactFixtures = []string{
	"artifact-wire.json",
}

// TestArtifactFixturesCovered fails when a fixture with the artifact- prefix has
// no entry above, and when an entry has no file, so a fixture added to wiregen
// cannot go unchecked.
func TestArtifactFixturesCovered(t *testing.T) {
	entries, err := os.ReadDir(wiretest.Dir(t))
	if err != nil {
		t.Fatalf("read fixture dir: %v", err)
	}
	present := map[string]bool{}
	for _, e := range entries {
		present[e.Name()] = true
		if strings.HasPrefix(e.Name(), "artifact-") && !slices.Contains(artifactFixtures, e.Name()) {
			t.Errorf("fixture %s has no entry in artifactFixtures", e.Name())
		}
	}
	for _, f := range artifactFixtures {
		if !present[f] {
			t.Errorf("artifactFixtures entry %s has no fixture file", f)
		}
	}
}

func TestFileUnionRejectsMalformed(t *testing.T) {
	const current = `{"status":"current","path":"p","sandboxId":"s","content":"c","size":1,"sha256":"h","truncated":false}`
	const missing = `{"status":"missing","path":"p","sandboxId":"s","reason":"r"}`

	// The two valid shapes decode (a baseline so the rejections below prove something).
	for _, ok := range []string{current, strings.Replace(current, "current", "changed", 1), missing} {
		var f artifact.File
		if err := jsonx.UnmarshalStrict([]byte(ok), &f); err != nil {
			t.Fatalf("valid shape %s rejected: %v", ok, err)
		}
	}

	bad := map[string]string{
		"unknown status":            `{"status":"deleted","path":"p","sandboxId":"s","reason":"r"}`,
		"empty status":              `{"status":"","path":"p","sandboxId":"s","reason":"r"}`,
		"no status":                 `{"path":"p","sandboxId":"s","reason":"r"}`,
		"numeric status":            `{"status":1,"path":"p","sandboxId":"s","reason":"r"}`,
		"null status":               `{"status":null,"path":"p","sandboxId":"s","reason":"r"}`,
		"missing with content":      `{"status":"missing","path":"p","sandboxId":"s","reason":"r","content":"c"}`,
		"missing with size":         `{"status":"missing","path":"p","sandboxId":"s","reason":"r","size":1}`,
		"missing without reason":    `{"status":"missing","path":"p","sandboxId":"s"}`,
		"missing with null reason":  `{"status":"missing","path":"p","sandboxId":"s","reason":null}`,
		"current with reason":       `{"status":"current","path":"p","sandboxId":"s","content":"c","size":1,"sha256":"h","truncated":false,"reason":"r"}`,
		"current without content":   `{"status":"current","path":"p","sandboxId":"s","size":1,"sha256":"h","truncated":false}`,
		"current with null content": `{"status":"current","path":"p","sandboxId":"s","content":null,"size":1,"sha256":"h","truncated":false}`,
		"current without size":      `{"status":"current","path":"p","sandboxId":"s","content":"c","sha256":"h","truncated":false}`,
		"current without sha256":    `{"status":"current","path":"p","sandboxId":"s","content":"c","size":1,"truncated":false}`,
		"current without truncated": `{"status":"current","path":"p","sandboxId":"s","content":"c","size":1,"sha256":"h"}`,
		"changed without path":      `{"status":"changed","sandboxId":"s","content":"c","size":1,"sha256":"h","truncated":false}`,
		"unknown key":               `{"status":"missing","path":"p","sandboxId":"s","reason":"r","extra":true}`,
		"wrong size type":           `{"status":"current","path":"p","sandboxId":"s","content":"c","size":"1","sha256":"h","truncated":false}`,
		"json array":                `[]`,
		"json null":                 `null`,
		"json number":               `1`,
		"json string":               `"current"`,
		"empty object":              `{}`,
		"truncated input":           `{"status":"missing"`,
		"empty input":               ``,
	}
	for name, in := range bad {
		t.Run(name, func(t *testing.T) {
			f := artifact.File{Status: artifact.FileCurrent, Path: "keep"}
			before := f
			if err := f.UnmarshalJSON([]byte(in)); err == nil {
				t.Errorf("%s must be rejected: %s", name, in)
			}
			if f != before {
				t.Errorf("a rejected input must leave the value unchanged: %+v", f)
			}
			var viaStrict artifact.File
			if err := jsonx.UnmarshalStrict([]byte(in), &viaStrict); err == nil {
				t.Errorf("%s must be rejected through UnmarshalStrict too", name)
			}
		})
	}
}

func TestFileEmptyContentKept(t *testing.T) {
	for _, status := range []artifact.FileStatus{artifact.FileCurrent, artifact.FileChanged} {
		b, err := jsonx.Marshal(artifact.File{Status: status, Path: "p", SandboxID: "s", SHA256: "h"})
		if err != nil {
			t.Fatalf("%s: %v", status, err)
		}
		want := `{"status":"` + string(status) + `","path":"p","sandboxId":"s","content":"","size":0,"sha256":"h","truncated":false}`
		if string(b) != want {
			t.Errorf("empty content must stay present:\n got %s\nwant %s", b, want)
		}
	}
	b, err := jsonx.Marshal(artifact.File{Status: artifact.FileMissing, Path: "p", SandboxID: "s", Reason: "r"})
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != `{"status":"missing","path":"p","sandboxId":"s","reason":"r"}` {
		t.Errorf("missing shape: %s", b)
	}
	// A pointer encodes the same way as a value.
	pb, err := jsonx.Marshal(&artifact.File{Status: artifact.FileMissing, Path: "p", SandboxID: "s", Reason: "r"})
	if err != nil || string(pb) != string(b) {
		t.Errorf("pointer encoding differs: %s %v", pb, err)
	}
}

func TestFileMarshalZeroIsError(t *testing.T) {
	cases := map[string]artifact.File{
		"zero value":                {},
		"unknown status":            {Status: "deleted", Path: "p"},
		"missing carrying content":  {Status: artifact.FileMissing, Reason: "r", Content: "c"},
		"missing carrying size":     {Status: artifact.FileMissing, Reason: "r", Size: 1},
		"missing carrying sha256":   {Status: artifact.FileMissing, Reason: "r", SHA256: "h"},
		"missing carrying truncate": {Status: artifact.FileMissing, Reason: "r", Truncated: true},
		"current carrying a reason": {Status: artifact.FileCurrent, Reason: "r"},
		"changed carrying a reason": {Status: artifact.FileChanged, Reason: "r"},
	}
	for name, f := range cases {
		t.Run(name, func(t *testing.T) {
			if b, err := jsonx.Marshal(f); err == nil {
				t.Errorf("must return an error, got %s", b)
			}
		})
	}
	// An error inside a larger value surfaces too (the value is never half written).
	if _, err := jsonx.Marshal(artifact.Read{}); err == nil {
		t.Error("a Read holding a zero File must fail to encode")
	}
}

func TestArtifactOptionalFieldsOmitted(t *testing.T) {
	b, err := jsonx.Marshal(artifact.Origin{AgentID: "a", SandboxPath: "/p", SandboxID: "s"})
	if err != nil || string(b) != `{"agentId":"a","sandboxPath":"/p","sandboxId":"s"}` {
		t.Errorf("origin without sessionId and tool: %s %v", b, err)
	}
	b, err = jsonx.Marshal(artifact.Artifact{ID: "i", AgentID: "a", Name: "n", Type: "t", Version: 1, SandboxPath: "/p", SandboxID: "s", CreatedAt: "c", UpdatedAt: "u"})
	want := `{"id":"i","agentId":"a","name":"n","type":"t","version":1,"sandboxPath":"/p","sandboxId":"s","bytes":0,"sha256":"","createdAt":"c","updatedAt":"u"}`
	if err != nil || string(b) != want {
		t.Errorf("artifact without optional fields:\n got %s\nwant %s (%v)", b, want, err)
	}
	// The embedded Ref must encode its keys first and inline.
	b, err = jsonx.Marshal(artifact.UpdateInput{Ref: artifact.Ref{Name: "n"}, Note: "x"})
	if err != nil || string(b) != `{"name":"n","note":"x"}` {
		t.Errorf("update input: %s %v", b, err)
	}
}
