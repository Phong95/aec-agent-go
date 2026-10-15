package agent

import (
	"context"
	"log/slog"
	"sync"

	"github.com/Phong95/aec-agent-go/components/artifacthub"
	"github.com/Phong95/aec-agent-go/components/artifacthub/artifact"
	"github.com/Phong95/aec-agent-go/components/sandboxhub"
)

// ArtifactManager is the per-agent artifact access (A-62). Artifacts are global and
// durable (the hub owns the store), so the manager only binds the agent's identity,
// and the session doing the writing, to a view. It has no lifecycle and caches
// nothing: a view is a thin scope object, and creating one per tool call keeps the
// session stamp on each version correct.
type ArtifactManager struct {
	hub     *artifacthub.ArtifactHub
	agentID string
	logger  *slog.Logger

	// logOnce makes the failure of View be logged once per manager, so a tool loop
	// that asks on every call does not flood the log.
	logOnce sync.Once
}

// NewArtifactManager returns the manager of the agent agentID over the hub. A nil
// logger means slog.Default().
func NewArtifactManager(hub *artifacthub.ArtifactHub, agentID string, logger *slog.Logger) *ArtifactManager {
	if logger == nil {
		logger = slog.Default()
	}
	return &ArtifactManager{hub: hub, agentID: agentID, logger: logger}
}

// Artifacts returns the agent's artifact view. sessionID is stamped on
// the versions written through the view; "" leaves it unscoped. It returns the
// error of artifacthub.ArtifactHub.ForAgent as it is, which today is a blank agent id.
func (m *ArtifactManager) Artifacts(sessionID string) (artifact.View, error) {
	return m.hub.ForAgent(m.agentID, sessionID)
}

// View is Artifacts shaped as the loop's artifact port (agentloop.Deps.Artifacts),
// which has no error result. When ForAgent fails it returns a view whose every
// method returns that error and logs the failure once, so the error surfaces at use
// (when a tool asks for the artifacts), not at spawn.
func (m *ArtifactManager) View(sessionID string) artifact.View {
	v, err := m.Artifacts(sessionID)
	if err != nil {
		m.logOnce.Do(func() {
			m.logger.Warn("agent: artifact view unavailable", "agentId", m.agentID, "error", err.Error())
		})
		return &errorView{agentID: m.agentID, sessionID: sessionID, err: err}
	}
	return v
}

// errorView is the artifact.View of an agent whose ForAgent failed: it holds the
// agent and session ids and returns the one error from every operation.
type errorView struct {
	agentID   string
	sessionID string
	err       error
}

var _ artifact.View = (*errorView)(nil)

func (v *errorView) AgentID() string   { return v.agentID }
func (v *errorView) SessionID() string { return v.sessionID }

func (v *errorView) Save(context.Context, artifact.SaveInput, sandboxhub.View) (artifact.Write, error) {
	return artifact.Write{}, v.err
}

func (v *errorView) Update(context.Context, artifact.UpdateInput, sandboxhub.View) (artifact.Write, error) {
	return artifact.Write{}, v.err
}

func (v *errorView) Get(context.Context, artifact.Ref) (artifact.Artifact, error) {
	return artifact.Artifact{}, v.err
}

func (v *errorView) Read(context.Context, artifact.Ref, sandboxhub.View, artifact.ReadOptions) (artifact.Read, error) {
	return artifact.Read{}, v.err
}

func (v *errorView) List(context.Context, artifact.Query) ([]artifact.Artifact, error) {
	return nil, v.err
}

func (v *errorView) Versions(context.Context, artifact.Ref) ([]artifact.Version, error) {
	return nil, v.err
}

func (v *errorView) Delete(context.Context, artifact.Ref) error { return v.err }
