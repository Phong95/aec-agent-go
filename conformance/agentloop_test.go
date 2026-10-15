package conformance_test

import (
	"testing"

	"github.com/Phong95/aec-agent-go/aecmock"
	"github.com/Phong95/aec-agent-go/components/agent/agentloop"
	"github.com/Phong95/aec-agent-go/components/agent/profile"
	"github.com/Phong95/aec-agent-go/components/events"
	"github.com/Phong95/aec-agent-go/components/persistence"
	"github.com/Phong95/aec-agent-go/components/toolhub"
	"github.com/Phong95/aec-agent-go/components/trace"
	"github.com/Phong95/aec-agent-go/conformance"
	"github.com/Phong95/aec-agent-go/kernel/ordered"
)

// defaultCompactor is the compactor config the suite expects. A zero config has a
// threshold of 0, which would compact on every turn.
func defaultCompactor() profile.CompactorConfig {
	return profile.CompactorConfig{
		ContextWindowTokens:      200_000,
		Threshold:                0.8,
		MinTailUserMessages:      4,
		ProtectFirstN:            2,
		ProactivePruneTokens:     50_000,
		OldToolResultMinTokens:   2_000,
		TailTargetRatio:          0.2,
		ProtectLastN:             10,
		SummarizeMaxOutputTokens: 4_096,
	}
}

// newScriptedHarness builds a loop over the aecmock fakes from a scenario: the
// scripted model, an in-memory conversation with a started session, a tool hub
// holding the scenario's tools, the passthrough context (a stub context
// manager), a dispatcher and a trace service. Everything is created with the
// subtest's t, inside its synctest bubble.
func newScriptedHarness(t *testing.T, s conformance.AgentLoopScenario) conformance.AgentLoopHarness {
	t.Helper()

	// The session starts under the scenario's session id: the loop refuses a sessionID
	// that is not the active session's id (IN-05), and the suite runs every case with
	// s.SessionID.
	hub := persistence.NewPersistenceHub(persistence.WithSessions(persistence.NewMemorySessionStore(
		persistence.WithSessionIDs(func() string { return s.SessionID }))))
	conv := aecmock.NewConversation(hub)
	if _, err := conv.StartSession(t.Context(), ordered.Object{}); err != nil {
		t.Fatalf("start session: %v", err)
	}

	turns := make([]aecmock.ScriptedTurn, len(s.Turns))
	for i, tr := range s.Turns {
		turns[i] = aecmock.ScriptedTurn{Content: tr.Content, ToolCalls: tr.ToolCalls, StopReason: tr.StopReason}
	}
	models := aecmock.NewScriptedModels(turns...)

	toolHub := toolhub.NewToolHub()
	names := make([]string, 0, len(s.Tools))
	for _, h := range s.Tools {
		if err := toolHub.Register(h); err != nil {
			t.Fatalf("register tool %s: %v", h.Name(), err)
		}
		names = append(names, h.Definition().Name)
	}

	bus := events.New()
	svc := trace.NewService()
	prof := profile.AgentProfile{
		ID:           s.AgentID,
		Name:         s.AgentID,
		SystemPrompt: "You are a test agent.",
		Compactor:    defaultCompactor(),
		LoopConfig:   s.LoopConfig,
	}

	deps := agentloop.Deps{
		Models:       models,
		Tools:        aecmock.NewHubTools(toolHub, names),
		Context:      aecmock.PassthroughContext{},
		Conversation: conv,
		Events:       bus,
		Profile:      func() profile.AgentProfile { return prof },
		Trace:        svc,
	}
	if len(s.SkillTools) > 0 {
		deps.Skills = aecmock.NewSkillTools(s.SkillTools...)
	}
	if s.Interaction != nil {
		deps.Interaction = func(string) toolhub.InteractionHandler { return s.Interaction }
	}
	var opts []agentloop.Option
	if s.MaxParallelTools > 0 {
		opts = append(opts, agentloop.WithMaxParallelTools(s.MaxParallelTools))
	}
	loop, err := agentloop.New(deps, opts...)
	if err != nil {
		t.Fatalf("agentloop.New: %v", err)
	}

	return conformance.AgentLoopHarness{
		Loop:       loop,
		Bus:        bus,
		Trace:      svc,
		Messages:   conv.Messages,
		ModelCalls: func() int { return len(models.Calls()) },
	}
}

// TestAgentLoopScripted runs the agent-loop conformance suite over the aecmock
// scripted fakes, which is also the proof that the suite's expectations hold
// for the loop.
func TestAgentLoopScripted(t *testing.T) {
	conformance.AgentLoop(t, newScriptedHarness)
}
