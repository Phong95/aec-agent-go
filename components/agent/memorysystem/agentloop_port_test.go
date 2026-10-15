package memorysystem_test

import (
	"context"
	"testing"

	"github.com/Phong95/aec-agent-go/components/agent/agentloop"
	"github.com/Phong95/aec-agent-go/components/agent/memorysystem"
	"github.com/Phong95/aec-agent-go/components/agent/memorysystem/memory"
	"github.com/Phong95/aec-agent-go/components/events"
	"github.com/Phong95/aec-agent-go/components/llmhub"
	"github.com/Phong95/aec-agent-go/components/persistence"
	"github.com/Phong95/aec-agent-go/kernel"
)

// The System is the memory port of the agent loop. The assertion lives in a test file
// of the external test package because memorysystem must not import agentloop in
// non-test code (the layer table); a test may.
var _ agentloop.Memory = (*memorysystem.System)(nil)

// idleModel is a Model that must never be called: this test only recalls.
type idleModel struct{}

func (idleModel) Chat(context.Context, []kernel.Message, llmhub.ModelOptions) (kernel.ModelResponse, error) {
	return kernel.ModelResponse{}, context.Canceled
}

// TestSystem_SatisfiesAgentloopMemory calls a real System through the loop's port.
func TestSystem_SatisfiesAgentloopMemory(t *testing.T) {
	sys, err := memorysystem.New(memorysystem.Deps{
		AgentID:     "a1",
		Persistence: persistence.NewPersistenceHub(),
		Events:      events.New(),
		Model:       idleModel{},
	}, memory.Config{})
	if err != nil {
		t.Fatal(err)
	}
	var port agentloop.Memory = sys
	out, err := port.RecallForTurn(t.Context(), "hello", "s1")
	if err != nil {
		t.Fatal(err)
	}
	if out.Skipped != memory.SkippedMemoryDisabled || out.Content != nil {
		t.Fatalf("RecallForTurn through the port = %+v, want skipped %q", out, memory.SkippedMemoryDisabled)
	}
}
