package orchestration

import (
	"context"
	"errors"
	"testing"

	"github.com/Phong95/aec-agent-go/components/agent"
	"github.com/Phong95/aec-agent-go/components/agent/profile"
	"github.com/Phong95/aec-agent-go/components/events"
	"github.com/Phong95/aec-agent-go/components/llmhub"
	"github.com/Phong95/aec-agent-go/components/persistence"
	"github.com/Phong95/aec-agent-go/components/toolhub"
)

// An agent whose stop fails is removed anyway (in a deferred call), and the stop error
// is returned. Agent.Dispose cannot fail yet, so the failing stop is passed to the
// helper Dispose is built on.
func TestOrchestration_DisposeRemovesEvenOnError(t *testing.T) {
	deps := agent.Deps{LLMHub: llmhub.NewLLMHub(), ToolHub: toolhub.NewToolHub(), PersistenceHub: persistence.NewPersistenceHub(), Events: events.New()}
	o := New(func(ctx context.Context, p profile.AgentProfile) (*agent.Agent, error) {
		return agent.New(ctx, deps, p)
	})
	for _, id := range []string{"a1", "a2", "a3"} {
		if _, err := o.Spawn(t.Context(), profile.AgentProfile{ID: id, Name: id}); err != nil {
			t.Fatalf("Spawn(%s): %v", id, err)
		}
	}

	boom := errors.New("stop failed")
	if err := o.disposeWith("a2", func(*agent.Agent) error { return boom }); !errors.Is(err, boom) {
		t.Fatalf("disposeWith error = %v, want the stop error", err)
	}
	if _, ok := o.Get("a2"); ok {
		t.Fatal("a2 is still registered after its stop failed")
	}
	var ids []string
	for _, a := range o.List() {
		ids = append(ids, a.ID())
	}
	if len(ids) != 2 || ids[0] != "a1" || ids[1] != "a3" {
		t.Fatalf("List() = %v, want [a1 a3] in insertion order", ids)
	}

	// A stop that panics removes the entry too (the deferred removal still runs).
	func() {
		defer func() { _ = recover() }()
		_ = o.disposeWith("a1", func(*agent.Agent) error { panic("stop panicked") })
	}()
	if _, ok := o.Get("a1"); ok {
		t.Fatal("a1 is still registered after its stop panicked")
	}

	// An unknown id is a no-op and the stop is not called.
	called := false
	if err := o.disposeWith("nope", func(*agent.Agent) error { called = true; return boom }); err != nil || called {
		t.Fatalf("disposeWith(unknown) = %v, called = %v, want nil and not called", err, called)
	}
}
