package artifacts

import (
	"context"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/Phong95/aec-agent-go/aecmock"
	"github.com/Phong95/aec-agent-go/components/artifacthub"
	"github.com/Phong95/aec-agent-go/components/artifacthub/artifact"
	"github.com/Phong95/aec-agent-go/components/events"
	"github.com/Phong95/aec-agent-go/components/persistence"
	"github.com/Phong95/aec-agent-go/components/sandboxhub"
	"github.com/Phong95/aec-agent-go/components/toolhub"
	"github.com/Phong95/aec-agent-go/kernel"
)

// This file holds the 29 tool-layer cases of the artifact tools: the tools over a real
// artifact hub (a persistence hub with the in-memory artifact store) and a real in-memory
// sandbox. The hub-level cases (agent ownership twice, and ArtifactHub fails closed with
// no store) live with the hub (Phase 6). The recorded corpus (wire_test.go) compares the
// same tools byte for byte over many more cases; these cases keep readable test titles
// and a narrative.

// sandboxOf builds a SandboxHub with the in-memory provider
// of the kind as the shared sandbox, and the view the agent gets from it.
func sandboxOf(t *testing.T, agentID string, kind sandboxhub.Kind) sandboxhub.View {
	t.Helper()
	hub := sandboxhub.NewSandboxHub()
	if err := hub.Register(aecmock.NewInMemoryProvider(aecmock.WithKind(kind))); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if err := hub.ConfigureShared(sandboxhub.Spec{Kind: kind}); err != nil {
		t.Fatalf("ConfigureShared: %v", err)
	}
	env, err := hub.Resolve(t.Context(), agentID, sandboxhub.Selection{})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	t.Cleanup(func() {
		if err := hub.Dispose(context.WithoutCancel(t.Context())); err != nil {
			t.Errorf("Dispose: %v", err)
		}
	})
	return env
}

// emitted is one event a tool published through toolhub.Context.Emit.
type emitted struct {
	Type    string
	Payload any
}

// harness is the test setup: a persistence hub with the in-memory artifact store, an
// artifact hub over it, the agent's sandbox and a tool context that records its events.
type harness struct {
	hub    *artifacthub.ArtifactHub
	tc     toolhub.Context
	events *[]emitted
	env    sandboxhub.View
}

func setup(t *testing.T, sessionID string) harness {
	t.Helper()
	ph := persistence.NewPersistenceHub()
	hub := artifacthub.NewArtifactHub(func() persistence.ArtifactStore { return ph.Artifacts })
	env := sandboxOf(t, "a1", sandboxhub.KindDocker)
	log := &[]emitted{}
	return harness{hub: hub, env: env, events: log, tc: contextOn(t, hub, env, sessionID, log)}
}

// contextOn is a tool context of agent a1 in a session, over the hub and the sandbox.
func contextOn(t *testing.T, hub *artifacthub.ArtifactHub, env sandboxhub.View, sessionID string, log *[]emitted) toolhub.Context {
	t.Helper()
	return toolhub.Context{
		SessionID: sessionID,
		AgentID:   "a1",
		GetArtifacts: func() artifact.View {
			view, err := hub.ForAgent("a1", sessionID)
			if err != nil {
				t.Errorf("ForAgent: %v", err)
			}
			return view
		},
		GetSandbox: func(context.Context) (sandboxhub.View, error) { return env, nil },
		Emit: func(eventType string, payload any) {
			if log != nil {
				*log = append(*log, emitted{Type: eventType, Payload: payload})
			}
		},
	}
}

// execute runs a handler with the JSON arguments and fails the test on a returned error:
// an artifact tool reports every problem as an output.
func execute(t *testing.T, h toolhub.Handler, tc toolhub.Context, args string) toolhub.Output {
	t.Helper()
	a, err := kernel.NewArguments([]byte(args))
	if err != nil {
		t.Fatalf("arguments %s: %v", args, err)
	}
	out, err := h.Execute(t.Context(), a, tc)
	if err != nil {
		t.Fatalf("Execute returned an error (every failure must be an output): %v", err)
	}
	return out
}

func isError(o toolhub.Output) bool { return o.IsError != nil && *o.IsError }

// notRetryable reports an error output that is marked not retryable.
func notRetryable(o toolhub.Output) bool { return isError(o) && o.Retryable != nil && !*o.Retryable }

// hasMeta checks a subset of the metadata: every key has the value.
func hasMeta(t *testing.T, o toolhub.Output, want map[string]any) {
	t.Helper()
	for key, w := range want {
		got, ok := o.Metadata.Get(key)
		if !ok {
			t.Errorf("metadata has no %q: %+v", key, o.Metadata)
			continue
		}
		if !reflect.DeepEqual(got, w) {
			t.Errorf("metadata %q = %#v, want %#v", key, got, w)
		}
	}
}

func metaHas(o toolhub.Output, key string) bool {
	_, ok := o.Metadata.Get(key)
	return ok
}

func mustWrite(t *testing.T, env sandboxhub.View, path, content string) {
	t.Helper()
	if _, err := env.WriteFile(t.Context(), path, content, sandboxhub.WriteOptions{}); err != nil {
		t.Fatalf("WriteFile(%s): %v", path, err)
	}
}

func mustRead(t *testing.T, env sandboxhub.View, path string) string {
	t.Helper()
	fc, err := env.ReadFile(t.Context(), path, sandboxhub.ReadOptions{})
	if err != nil {
		t.Fatalf("ReadFile(%s): %v", path, err)
	}
	return fc.Content
}

// agentView is the hub's view for agent "a1".
func agentView(t *testing.T, h harness) artifact.View {
	t.Helper()
	view, err := h.hub.ForAgent("a1", "")
	if err != nil {
		t.Fatalf("ForAgent: %v", err)
	}
	return view
}

func readArtifact(t *testing.T, h harness, name string, env sandboxhub.View) artifact.Read {
	t.Helper()
	got, err := agentView(t, h).Read(t.Context(), artifact.Ref{Name: name}, env, artifact.ReadOptions{})
	if err != nil {
		t.Fatalf("Read %s: %v", name, err)
	}
	return got
}

func eventTypes(h harness) []string {
	out := make([]string, 0, len(*h.events))
	for _, e := range *h.events {
		out = append(out, e.Type)
	}
	return out
}

// TestArtifactTools runs the 29 tool-layer cases of the artifact tools under their
// titles. A title that holds an em dash builds it from the code point, so this source
// stays ASCII.
func TestArtifactTools(t *testing.T) {
	save, update, read, list := NewSaveTool(Options{}), NewUpdateTool(Options{}), NewReadTool(Options{}), NewListTool(Options{})

	// --- artifact tool definitions ---------------------------------------------------------

	t.Run("exposes the four A-62 tools with the right risk and sandboxOperation", func(t *testing.T) {
		tools := NewTools(Options{})
		names := make([]string, len(tools))
		byName := map[string]toolhub.Definition{}
		for i, h := range tools {
			d := h.Definition()
			names[i] = d.Name
			byName[d.Name] = d
		}
		if !slices.Equal(names, ToolNames()) {
			t.Fatalf("names = %v, want %v", names, ToolNames())
		}
		want := map[string][2]string{
			"save_artifact":   {string(toolhub.RiskDraft), string(sandboxhub.OperationFSWrite)},
			"update_artifact": {string(toolhub.RiskDraft), string(sandboxhub.OperationFSWrite)},
			"read_artifact":   {string(toolhub.RiskReadOnly), string(sandboxhub.OperationFSRead)},
			"list_artifacts":  {string(toolhub.RiskReadOnly), ""},
		}
		for name, w := range want {
			d := byName[name]
			if string(d.Risk) != w[0] || string(d.SandboxOperation) != w[1] {
				t.Errorf("%s: risk %q sandboxOperation %q, want %q %q", name, d.Risk, d.SandboxOperation, w[0], w[1])
			}
		}
	})

	t.Run("fails without retry when the host wired no artifact store", func(t *testing.T) {
		out := execute(t, save, toolhub.Context{SessionID: "s", Emit: func(string, any) {}}, `{"name":"a.md","type":"text/markdown","content":"hi"}`)
		if !notRetryable(out) {
			t.Errorf("output = %+v, want a non-retryable error", out)
		}
		if !strings.Contains(out.Content, "no artifact store") {
			t.Errorf("content = %q, want it to say there is no artifact store", out.Content)
		}
	})

	// --- save_artifact ----------------------------------------------------------------------

	t.Run("writes inline content to the sandbox, tracks it, stamps the session and emits artifact:saved", func(t *testing.T) {
		h := setup(t, "sess-7")
		out := execute(t, save, h.tc, `{"name":"reports/clash.md","type":"text/markdown","content":"# Clashes\n","description":"Level 3 clashes","tags":["clash","clash"],"note":"first pass"}`)

		if isError(out) {
			t.Fatalf("error output: %s", out.Content)
		}
		for _, part := range []string{`Saved artifact "reports/clash.md"`, "v1"} {
			if !strings.Contains(out.Content, part) {
				t.Errorf("content = %q, want it to contain %q", out.Content, part)
			}
		}
		hasMeta(t, out, map[string]any{"name": "reports/clash.md", "version": 1, "bytes": 10})
		if !metaHas(out, "sandboxPath") {
			t.Error("metadata has no sandboxPath")
		}

		if got := mustRead(t, h.env, "artifacts/reports/clash.md"); got != "# Clashes\n" {
			t.Errorf("the written file = %q", got)
		}
		got := readArtifact(t, h, "reports/clash.md", h.env)
		a := got.Artifact
		if a.Type != "text/markdown" || a.Description != "Level 3 clashes" || a.SessionID != "sess-7" || a.Version != 1 || a.SandboxID != h.env.ID() {
			t.Errorf("artifact = %+v", a)
		}
		if !slices.Equal(a.Tags, []string{"clash"}) { // duplicates collapsed
			t.Errorf("tags = %v, want [clash]", a.Tags)
		}
		if got.File.Status != artifact.FileCurrent || got.File.Content != "# Clashes\n" {
			t.Errorf("file = %+v", got.File)
		}

		versions, err := agentView(t, h).Versions(t.Context(), artifact.Ref{Name: "reports/clash.md"})
		if err != nil || len(versions) != 1 {
			t.Fatalf("Versions = %+v, %v; want one version", versions, err)
		}
		v := versions[0]
		if v.Note != "first pass" {
			t.Errorf("note = %q", v.Note)
		}
		wantOrigin := artifact.Origin{AgentID: "a1", SessionID: "sess-7", Tool: "save_artifact", SandboxPath: a.SandboxPath, SandboxID: h.env.ID()}
		if v.Origin != wantOrigin {
			t.Errorf("origin = %+v, want %+v", v.Origin, wantOrigin)
		}

		if !slices.Equal(eventTypes(h), []string{events.ArtifactSaved.Name}) {
			t.Fatalf("events = %v, want [artifact:saved]", eventTypes(h))
		}
		payload, ok := (*h.events)[0].Payload.(events.ArtifactWrittenPayload)
		if !ok {
			t.Fatalf("payload is %T", (*h.events)[0].Payload)
		}
		want := events.ArtifactWrittenPayload{
			AgentID: "a1", ArtifactID: a.ID, Name: "reports/clash.md", Type: "text/markdown", Version: 1, Bytes: 10,
			SandboxPath: a.SandboxPath, SandboxID: h.env.ID(),
		}
		if payload != want {
			t.Errorf("payload = %+v, want %+v", payload, want)
		}
	})

	t.Run("tracks an existing file with from_path without creating a file under artifacts/", func(t *testing.T) {
		h := setup(t, "s1")
		mustWrite(t, h.env, "out/takeoff.csv", "item,qty\nbeam,12\n")

		out := execute(t, save, h.tc, `{"name":"takeoff.csv","type":"text/csv","from_path":"out/takeoff.csv","note":"tracked"}`)

		if isError(out) {
			t.Fatalf("error output: %s", out.Content)
		}
		got := readArtifact(t, h, "takeoff.csv", h.env)
		if got.Artifact.SandboxPath != "/workspace/agents/a1/out/takeoff.csv" {
			t.Errorf("sandboxPath = %s", got.Artifact.SandboxPath)
		}
		if got.File.Status != artifact.FileCurrent || got.File.Content != "item,qty\nbeam,12\n" {
			t.Errorf("file = %+v", got.File)
		}
		stat, err := h.env.Stat(t.Context(), "artifacts/takeoff.csv")
		if err != nil || stat != nil {
			t.Errorf("Stat(artifacts/takeoff.csv) = %v, %v; want no file", stat, err)
		}
	})

	t.Run("from_path to a nonexistent file gives a non-retryable file_missing error", func(t *testing.T) {
		h := setup(t, "s1")
		out := execute(t, save, h.tc, `{"name":"missing.csv","type":"text/csv","from_path":"nope.csv"}`)
		if !notRetryable(out) {
			t.Errorf("output = %+v, want a non-retryable error", out)
		}
		hasMeta(t, out, map[string]any{"code": "file_missing"})
	})

	t.Run("refuses a name the agent already used, leaving the existing file untouched", func(t *testing.T) {
		h := setup(t, "s1")
		execute(t, save, h.tc, `{"name":"a.md","type":"text/markdown","content":"one"}`)

		again := execute(t, save, h.tc, `{"name":"a.md","type":"text/markdown","content":"two"}`)

		if !notRetryable(again) {
			t.Errorf("output = %+v, want a non-retryable error", again)
		}
		hasMeta(t, again, map[string]any{"code": "name_conflict"})
		if !strings.Contains(again.Content, "update_artifact") {
			t.Errorf("content = %q, want it to point at update_artifact", again.Content)
		}
		if got := mustRead(t, h.env, "artifacts/a.md"); got != "one" {
			t.Errorf("the file = %q, want it untouched", got)
		}
	})

	t.Run("rejects a bad name and a bad type", func(t *testing.T) {
		h := setup(t, "s1")
		for _, args := range []string{
			`{"name":"../escape.md","type":"text/markdown","content":"x"}`,
			`{"name":"ok.md","type":"not a type!","content":"x"}`,
		} {
			out := execute(t, save, h.tc, args)
			if !notRetryable(out) {
				t.Errorf("%s: output = %+v, want a non-retryable error", args, out)
			}
			hasMeta(t, out, map[string]any{"code": "invalid_argument"})
		}
	})

	t.Run("needs content or from_path "+emDash+" having neither is the only error", func(t *testing.T) {
		h := setup(t, "s1")
		out := execute(t, save, h.tc, `{"name":"a.md","type":"text/markdown"}`)
		if !notRetryable(out) {
			t.Errorf("output = %+v, want a non-retryable error", out)
		}
		hasMeta(t, out, map[string]any{"code": "invalid_argument"})
	})

	// Models routinely fill every property a schema offers. Both given must not be a dead
	// end: the inline content is saved and the dropped from_path is reported.
	t.Run("saves the inline content and says so when the model also passed from_path", func(t *testing.T) {
		h := setup(t, "s1")
		mustWrite(t, h.env, "out/draft.md", "from the file")

		out := execute(t, save, h.tc, `{"name":"a.md","type":"text/markdown","content":"inline wins","from_path":"out/draft.md"}`)

		if isError(out) {
			t.Fatalf("error output: %s", out.Content)
		}
		if !strings.Contains(out.Content, "you passed both") {
			t.Errorf("content = %q, want it to say both were passed", out.Content)
		}
		hasMeta(t, out, map[string]any{"ignoredFromPath": "out/draft.md"})
		if got := readArtifact(t, h, "a.md", h.env).File; got.Content != "inline wins" {
			t.Errorf("file = %+v", got)
		}
	})

	t.Run("tracks the file when content is blank next to a from_path", func(t *testing.T) {
		h := setup(t, "s1")
		mustWrite(t, h.env, "out/draft.md", "from the file")

		out := execute(t, save, h.tc, `{"name":"a.md","type":"text/markdown","content":"","from_path":"out/draft.md"}`)

		if isError(out) {
			t.Fatalf("error output: %s", out.Content)
		}
		if metaHas(out, "ignoredFromPath") {
			t.Error("metadata has ignoredFromPath")
		}
		if got := readArtifact(t, h, "a.md", h.env).File; got.Content != "from the file" {
			t.Errorf("file = %+v", got)
		}
	})

	t.Run("still saves an intentionally empty artifact when no from_path is given", func(t *testing.T) {
		h := setup(t, "s1")
		out := execute(t, save, h.tc, `{"name":"empty.md","type":"text/markdown","content":""}`)
		if isError(out) {
			t.Fatalf("error output: %s", out.Content)
		}
		if got := readArtifact(t, h, "empty.md", h.env).File; got.Status != artifact.FileCurrent || got.Content != "" {
			t.Errorf("file = %+v, want the current empty file", got)
		}
	})

	t.Run("gives a clear, non-retryable error when the agent has no sandbox", func(t *testing.T) {
		h := setup(t, "s1")
		tc := h.tc
		tc.GetSandbox = nil
		out := execute(t, save, tc, `{"name":"a.md","type":"text/markdown","content":"hi"}`)
		if !notRetryable(out) {
			t.Errorf("output = %+v, want a non-retryable error", out)
		}
		if !strings.Contains(out.Content, "no sandbox") {
			t.Errorf("content = %q, want it to say there is no sandbox", out.Content)
		}
	})

	// --- update_artifact --------------------------------------------------------------------

	t.Run("rewrites the tracked file, moves the head to v2 and keeps a content-less version log", func(t *testing.T) {
		h := setup(t, "s1")
		execute(t, save, h.tc, `{"name":"a.md","type":"text/markdown","content":"v1"}`)

		out := execute(t, update, h.tc, `{"name":"a.md","content":"v2","note":"added totals"}`)

		if isError(out) {
			t.Fatalf("error output: %s", out.Content)
		}
		hasMeta(t, out, map[string]any{"version": 2, "previousVersion": 1})
		if got := readArtifact(t, h, "a.md", h.env).File; got.Content != "v2" {
			t.Errorf("file = %+v", got)
		}
		versions, err := agentView(t, h).Versions(t.Context(), artifact.Ref{Name: "a.md"})
		if err != nil {
			t.Fatal(err)
		}
		var numbers []int
		for _, v := range versions {
			numbers = append(numbers, v.Version)
		}
		if !slices.Equal(numbers, []int{2, 1}) {
			t.Errorf("versions = %v, want [2 1]", numbers)
		}
		if want := []string{events.ArtifactSaved.Name, events.ArtifactUpdated.Name}; !slices.Equal(eventTypes(h), want) {
			t.Errorf("events = %v, want %v", eventTypes(h), want)
		}
	})

	t.Run("re-tracks the same path with a new sha256 when only the name is given", func(t *testing.T) {
		h := setup(t, "s1")
		execute(t, save, h.tc, `{"name":"a.md","type":"text/markdown","content":"v1"}`)
		mustWrite(t, h.env, "artifacts/a.md", "edited outside the tool")

		out := execute(t, update, h.tc, `{"name":"a.md"}`)

		if isError(out) {
			t.Fatalf("error output: %s", out.Content)
		}
		hasMeta(t, out, map[string]any{"version": 2})
		got := readArtifact(t, h, "a.md", h.env)
		if got.Artifact.SandboxPath != "/workspace/agents/a1/artifacts/a.md" {
			t.Errorf("sandboxPath = %s", got.Artifact.SandboxPath)
		}
		if got.File.Status != artifact.FileCurrent || got.File.Content != "edited outside the tool" {
			t.Errorf("file = %+v", got.File)
		}
	})

	t.Run("switches the tracked path with from_path", func(t *testing.T) {
		h := setup(t, "s1")
		execute(t, save, h.tc, `{"name":"a.md","type":"text/markdown","content":"v1"}`)
		mustWrite(t, h.env, "out/new.md", "moved")

		out := execute(t, update, h.tc, `{"name":"a.md","from_path":"out/new.md"}`)

		if isError(out) {
			t.Fatalf("error output: %s", out.Content)
		}
		got := readArtifact(t, h, "a.md", h.env)
		if got.Artifact.SandboxPath != "/workspace/agents/a1/out/new.md" {
			t.Errorf("sandboxPath = %s", got.Artifact.SandboxPath)
		}
		if got.File.Content != "moved" {
			t.Errorf("file = %+v", got.File)
		}
	})

	t.Run("honours expected_version so a stale edit fails and does not rewrite the file", func(t *testing.T) {
		h := setup(t, "s1")
		execute(t, save, h.tc, `{"name":"a.md","type":"text/markdown","content":"v1"}`)
		execute(t, update, h.tc, `{"name":"a.md","content":"v2"}`)

		stale := execute(t, update, h.tc, `{"name":"a.md","content":"v3","expected_version":1}`)

		if !notRetryable(stale) {
			t.Errorf("output = %+v, want a non-retryable error", stale)
		}
		hasMeta(t, stale, map[string]any{"code": "conflict"})
		if got := mustRead(t, h.env, "artifacts/a.md"); got != "v2" {
			t.Errorf("the file = %q, want v2", got)
		}
	})

	t.Run("replaces description and tags only when they are given", func(t *testing.T) {
		h := setup(t, "s1")
		execute(t, save, h.tc, `{"name":"a.md","type":"text/markdown","content":"v1","description":"first","tags":["keep"]}`)

		execute(t, update, h.tc, `{"name":"a.md","content":"v2"}`)
		if a := readArtifact(t, h, "a.md", h.env).Artifact; a.Description != "first" || !slices.Equal(a.Tags, []string{"keep"}) {
			t.Errorf("after an update with neither: description %q tags %v, want first [keep]", a.Description, a.Tags)
		}

		execute(t, update, h.tc, `{"name":"a.md","content":"v3","description":"second","tags":["new"]}`)
		if a := readArtifact(t, h, "a.md", h.env).Artifact; a.Description != "second" || !slices.Equal(a.Tags, []string{"new"}) {
			t.Errorf("after an update with both: description %q tags %v, want second [new]", a.Description, a.Tags)
		}
	})

	t.Run("keeps working when the model passes both content and from_path", func(t *testing.T) {
		h := setup(t, "s1")
		mustWrite(t, h.env, "out/draft.md", "file text")
		execute(t, save, h.tc, `{"name":"a.md","type":"text/markdown","content":"v1"}`)

		out := execute(t, update, h.tc, `{"name":"a.md","content":"v2 inline","from_path":"out/draft.md"}`)

		if isError(out) {
			t.Fatalf("error output: %s", out.Content)
		}
		hasMeta(t, out, map[string]any{"version": 2, "ignoredFromPath": "out/draft.md"})
		if got := readArtifact(t, h, "a.md", h.env).File; got.Content != "v2 inline" {
			t.Errorf("file = %+v", got)
		}
	})

	t.Run("reports not_found for an artifact that does not exist", func(t *testing.T) {
		h := setup(t, "s1")
		out := execute(t, update, h.tc, `{"name":"ghost.md","content":"x"}`)
		if !notRetryable(out) {
			t.Errorf("output = %+v, want a non-retryable error", out)
		}
		hasMeta(t, out, map[string]any{"code": "not_found"})
	})

	t.Run("gives a clear file_missing error re-tracking in a different sandbox with no content or path", func(t *testing.T) {
		h := setup(t, "s1")
		execute(t, save, h.tc, `{"name":"a.md","type":"text/markdown","content":"v1"}`)
		other := sandboxOf(t, "a1", sandboxhub.KindHost)
		tc := h.tc
		tc.GetSandbox = func(context.Context) (sandboxhub.View, error) { return other, nil }

		out := execute(t, update, tc, `{"name":"a.md"}`)

		if !notRetryable(out) {
			t.Errorf("output = %+v, want a non-retryable error", out)
		}
		hasMeta(t, out, map[string]any{"code": "file_missing"})
		if !strings.Contains(out.Content, other.ID()) {
			t.Errorf("content = %q, want it to name the sandbox %s", out.Content, other.ID())
		}
		got, err := agentView(t, h).Get(t.Context(), artifact.Ref{Name: "a.md"})
		if err != nil {
			t.Fatal(err)
		}
		if got.Version != 1 { // no write happened
			t.Errorf("version = %d, want 1", got.Version)
		}
	})

	// --- read_artifact ----------------------------------------------------------------------

	t.Run("returns the tracked file's content with a header naming the tracked path", func(t *testing.T) {
		h := setup(t, "s1")
		execute(t, save, h.tc, `{"name":"a.md","type":"text/markdown","content":"hello"}`)

		out := execute(t, read, h.tc, `{"name":"a.md"}`)

		if isError(out) {
			t.Fatalf("error output: %s", out.Content)
		}
		for _, part := range []string{"a.md (text/markdown) v1", "hello"} {
			if !strings.Contains(out.Content, part) {
				t.Errorf("content = %q, want it to contain %q", out.Content, part)
			}
		}
		hasMeta(t, out, map[string]any{"version": 1, "status": "current", "truncated": false})
		if !metaHas(out, "sandboxPath") {
			t.Error("metadata has no sandboxPath")
		}
	})

	t.Run("bounds long content without touching the tracked file", func(t *testing.T) {
		h := setup(t, "s1")
		execute(t, save, h.tc, `{"name":"a.md","type":"text/markdown","content":"`+strings.Repeat("z", 500)+`"}`)

		out := execute(t, read, h.tc, `{"name":"a.md","max_bytes":100}`)

		hasMeta(t, out, map[string]any{"truncated": true, "bytes": 500})
		if !strings.Contains(out.Content, "raise max_bytes") {
			t.Errorf("content = %q, want the truncation note", out.Content)
		}
		if got := readArtifact(t, h, "a.md", h.env).File; got.Content != strings.Repeat("z", 500) {
			t.Errorf("the tracked file was cut: %d bytes", len(got.Content))
		}
	})

	t.Run("is not an error when the file changed since it was tracked, and suggests re-tracking", func(t *testing.T) {
		h := setup(t, "s1")
		execute(t, save, h.tc, `{"name":"a.md","type":"text/markdown","content":"v1"}`)
		mustWrite(t, h.env, "artifacts/a.md", "edited outside the tool")

		out := execute(t, read, h.tc, `{"name":"a.md"}`)

		if isError(out) {
			t.Errorf("error output: %s", out.Content)
		}
		hasMeta(t, out, map[string]any{"status": "changed"})
		for _, part := range []string{"changed since", "update_artifact"} {
			if !strings.Contains(out.Content, part) {
				t.Errorf("content = %q, want it to contain %q", out.Content, part)
			}
		}
	})

	t.Run("gives a non-retryable file_missing error when the tracked file is gone", func(t *testing.T) {
		h := setup(t, "s1")
		execute(t, save, h.tc, `{"name":"a.md","type":"text/markdown","content":"v1"}`)
		fresh := sandboxOf(t, "a1", sandboxhub.KindDocker) // same id, empty store: the file is no longer there
		tc := h.tc
		tc.GetSandbox = func(context.Context) (sandboxhub.View, error) { return fresh, nil }

		out := execute(t, read, tc, `{"name":"a.md"}`)

		if !notRetryable(out) {
			t.Errorf("output = %+v, want a non-retryable error", out)
		}
		hasMeta(t, out, map[string]any{"code": "file_missing"})
	})

	t.Run("reports not_found for an artifact that was never saved", func(t *testing.T) {
		h := setup(t, "s1")
		out := execute(t, read, h.tc, `{"name":"ghost.md"}`)
		if !notRetryable(out) {
			t.Errorf("output = %+v, want a non-retryable error", out)
		}
		hasMeta(t, out, map[string]any{"code": "not_found"})
	})

	t.Run("gives a missing file naming both ids when read from a different sandbox", func(t *testing.T) {
		h := setup(t, "s1")
		execute(t, save, h.tc, `{"name":"a.md","type":"text/markdown","content":"v1"}`)
		other := sandboxOf(t, "a1", sandboxhub.KindHost)

		got := readArtifact(t, h, "a.md", other)

		if got.File.Status != artifact.FileMissing {
			t.Fatalf("status = %s, want missing", got.File.Status)
		}
		for _, id := range []string{got.Artifact.SandboxID, other.ID()} {
			if !strings.Contains(got.File.Reason, id) {
				t.Errorf("reason = %q, want it to contain %s", got.File.Reason, id)
			}
		}
	})

	// --- list_artifacts ---------------------------------------------------------------------

	t.Run("lists metadata newest first, filters by type and tag, and shows the tracked path", func(t *testing.T) {
		h := setup(t, "s1")
		execute(t, save, h.tc, `{"name":"a.md","type":"text/markdown","content":"a","tags":["report"]}`)
		execute(t, save, h.tc, `{"name":"b.csv","type":"text/csv","content":"b","tags":["takeoff"],"description":"Quantities"}`)

		all := execute(t, list, h.tc, `{}`)
		hasMeta(t, all, map[string]any{"count": 2})
		for _, part := range []string{"b.csv (text/csv) v1", "Quantities", "tracks /workspace/agents/a1/artifacts/b.csv"} {
			if !strings.Contains(all.Content, part) {
				t.Errorf("content = %q, want it to contain %q", all.Content, part)
			}
		}

		hasMeta(t, execute(t, list, h.tc, `{"type":"text/csv"}`), map[string]any{"count": 1})
		hasMeta(t, execute(t, list, h.tc, `{"tags":["report"]}`), map[string]any{"count": 1})
	})

	t.Run("only counts this session's artifacts when asked", func(t *testing.T) {
		ph := persistence.NewPersistenceHub()
		hub := artifacthub.NewArtifactHub(func() persistence.ArtifactStore { return ph.Artifacts })
		env := sandboxOf(t, "a1", sandboxhub.KindDocker)
		execute(t, save, contextOn(t, hub, env, "s1", nil), `{"name":"old.md","type":"text/markdown","content":"old"}`)
		execute(t, save, contextOn(t, hub, env, "s2", nil), `{"name":"new.md","type":"text/markdown","content":"new"}`)

		tc := contextOn(t, hub, env, "s2", nil)
		hasMeta(t, execute(t, list, tc, `{"this_session_only":true}`), map[string]any{"count": 1})
		hasMeta(t, execute(t, list, tc, `{}`), map[string]any{"count": 2})
	})

	t.Run("tells the model what to do when there is nothing yet", func(t *testing.T) {
		h := setup(t, "s1")
		out := execute(t, list, h.tc, `{}`)
		hasMeta(t, out, map[string]any{"count": 0})
		if !strings.Contains(out.Content, "save_artifact") {
			t.Errorf("content = %q, want it to point at save_artifact", out.Content)
		}
	})
}
