package agent_test

import (
	"bytes"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/Phong95/aec-agent-go/components/agent"
	"github.com/Phong95/aec-agent-go/components/artifacthub"
	"github.com/Phong95/aec-agent-go/components/artifacthub/artifact"
	"github.com/Phong95/aec-agent-go/components/persistence"
	"github.com/Phong95/aec-agent-go/components/sandboxhub"
	"github.com/Phong95/aec-agent-go/kernel"
)

// TestAgent_ArtifactsView: the agent's artifact view is the hub's ForAgent view bound
// to the agent and to the session that is stamped on new versions, and an agent built
// without an artifact hub reports not_configured.
func TestAgent_ArtifactsView(t *testing.T) {
	a, _ := newTestAgent(t, testProfile("artist"))
	v, err := a.Artifacts("s-1")
	if err != nil {
		t.Fatalf("Artifacts: %v", err)
	}
	if v.AgentID() != "artist" || v.SessionID() != "s-1" {
		t.Fatalf("view = agent %q session %q, want artist and s-1", v.AgentID(), v.SessionID())
	}
	unscoped, err := a.Artifacts("")
	if err != nil || unscoped.SessionID() != "" {
		t.Fatalf("Artifacts(\"\") = %v, %v, want an unscoped view", unscoped, err)
	}
	// Nothing saved yet: List works through the env's persistence store.
	list, err := v.List(t.Context(), artifact.Query{})
	if err != nil || len(list) != 0 {
		t.Fatalf("List = %v, %v, want an empty list", list, err)
	}

	e := newEnv(t)
	deps := e.deps()
	deps.ArtifactHub = nil
	bare, err := agent.New(t.Context(), deps, testProfile("bare"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = bare.Dispose(t.Context()) })
	_, err = bare.Artifacts("s-1")
	var ae *kernel.ArtifactError
	if !errors.As(err, &ae) || ae.Code != kernel.ArtifactCodeNotConfigured {
		t.Fatalf("Artifacts without a hub = %v, want a not_configured ArtifactError", err)
	}
}

// TestArtifactManager_ErrorViewLogsOnce: when ForAgent fails (a blank agent id) the
// manager's view for the loop returns that error from every method and logs the
// failure once, while Artifacts returns the error directly.
func TestArtifactManager_ErrorViewLogsOnce(t *testing.T) {
	var logs bytes.Buffer
	hub := artifacthub.NewArtifactHub(func() persistence.ArtifactStore { return persistence.NewMemoryArtifactStore() })
	m := agent.NewArtifactManager(hub, "  ", slog.New(slog.NewTextHandler(&logs, nil)))

	_, direct := m.Artifacts("s-1")
	var ae *kernel.ArtifactError
	if !errors.As(direct, &ae) || ae.Code != kernel.ArtifactCodeInvalidArgument {
		t.Fatalf("Artifacts = %v, want an invalid_argument ArtifactError", direct)
	}

	ctx := t.Context()
	var sb sandboxhub.View
	for range 3 {
		v := m.View("s-1")
		if v.SessionID() != "s-1" {
			t.Fatalf("error view session = %q, want s-1", v.SessionID())
		}
		checks := map[string]error{}
		_, checks["Save"] = v.Save(ctx, artifact.SaveInput{}, sb)
		_, checks["Update"] = v.Update(ctx, artifact.UpdateInput{}, sb)
		_, checks["Get"] = v.Get(ctx, artifact.Ref{})
		_, checks["Read"] = v.Read(ctx, artifact.Ref{}, sb, artifact.ReadOptions{})
		_, checks["List"] = v.List(ctx, artifact.Query{})
		_, checks["Versions"] = v.Versions(ctx, artifact.Ref{})
		checks["Delete"] = v.Delete(ctx, artifact.Ref{})
		for method, err := range checks {
			if !errors.Is(err, direct) && err.Error() != direct.Error() {
				t.Errorf("%s error = %v, want the ForAgent error %v", method, err, direct)
			}
		}
	}
	if n := strings.Count(logs.String(), "artifact view unavailable"); n != 1 {
		t.Fatalf("the failure was logged %d times, want once:\n%s", n, logs.String())
	}
	if !strings.Contains(logs.String(), "agentId") {
		t.Fatalf("the log line does not name the agent:\n%s", logs.String())
	}
}
