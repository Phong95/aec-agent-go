package orchestration_test

import (
	"context"
	"errors"
	"testing"

	"github.com/Phong95/aec-agent-go/components/agent"
	"github.com/Phong95/aec-agent-go/components/agent/profile"
	"github.com/Phong95/aec-agent-go/components/events"
	"github.com/Phong95/aec-agent-go/components/llmhub"
	"github.com/Phong95/aec-agent-go/components/orchestration"
	"github.com/Phong95/aec-agent-go/components/persistence"
	"github.com/Phong95/aec-agent-go/components/toolhub"
)

// newOrchestration returns an Orchestration whose factory builds real agents over
// fresh hubs.
func newOrchestration(t *testing.T) *orchestration.Orchestration {
	t.Helper()
	deps := agent.Deps{LLMHub: llmhub.NewLLMHub(), ToolHub: toolhub.NewToolHub(), PersistenceHub: persistence.NewPersistenceHub(), Events: events.New()}
	return orchestration.New(func(ctx context.Context, p profile.AgentProfile) (*agent.Agent, error) {
		return agent.New(ctx, deps, p)
	})
}

func spawn(t *testing.T, o *orchestration.Orchestration, id string) *agent.Agent {
	t.Helper()
	a, err := o.Spawn(t.Context(), profile.AgentProfile{ID: id, Name: id})
	if err != nil {
		t.Fatalf("Spawn(%s): %v", id, err)
	}
	return a
}

// A duplicate id has the expected error text, matches ErrAgentExists, and leaves
// the first agent registered.
func TestOrchestration_SpawnDuplicate(t *testing.T) {
	o := newOrchestration(t)
	first := spawn(t, o, "a1")

	_, err := o.Spawn(t.Context(), profile.AgentProfile{ID: "a1", Name: "again"})
	if err == nil {
		t.Fatal("a duplicate id was spawned")
	}
	if want := `Agent "a1" already exists`; err.Error() != want {
		t.Fatalf("error text = %q, want %q", err.Error(), want)
	}
	if !errors.Is(err, orchestration.ErrAgentExists) {
		t.Fatalf("errors.Is(err, ErrAgentExists) = false for %v", err)
	}
	got, ok := o.Get("a1")
	if !ok || got != first {
		t.Fatalf("Get(a1) = %v, %v, want the first agent to stay registered", got, ok)
	}
	if n := len(o.List()); n != 1 {
		t.Fatalf("List has %d agents, want 1", n)
	}
}

// List returns the agents in insertion order, as a copy; Dispose removes one and
// keeps the order of the rest, and an unknown id is a no-op.
func TestOrchestration_ListOrder(t *testing.T) {
	o := newOrchestration(t)
	for _, id := range []string{"c", "a", "b"} {
		spawn(t, o, id)
	}
	ids := func() []string {
		var out []string
		for _, a := range o.List() {
			out = append(out, a.ID())
		}
		return out
	}
	if got := ids(); len(got) != 3 || got[0] != "c" || got[1] != "a" || got[2] != "b" {
		t.Fatalf("List() = %v, want [c a b] (insertion order)", got)
	}

	// The returned slice is a copy.
	list := o.List()
	list[0] = nil
	if o.List()[0] == nil {
		t.Fatal("List() hands out the registry's own slice")
	}

	if err := o.Dispose(t.Context(), "a"); err != nil {
		t.Fatalf("Dispose(a): %v", err)
	}
	if got := ids(); len(got) != 2 || got[0] != "c" || got[1] != "b" {
		t.Fatalf("List() after Dispose(a) = %v, want [c b]", got)
	}
	if _, ok := o.Get("a"); ok {
		t.Fatal("a is still registered after Dispose")
	}
	if err := o.Dispose(t.Context(), "unknown"); err != nil {
		t.Fatalf("Dispose(unknown) = %v, want nil", err)
	}

	// A spawn after the dispose appends at the end, and the freed id can be reused.
	spawn(t, o, "a")
	if got := ids(); len(got) != 3 || got[2] != "a" {
		t.Fatalf("List() after a re-spawn = %v, want a last", got)
	}
}

// A factory error registers nothing.
func TestOrchestration_FactoryError(t *testing.T) {
	boom := errors.New("factory failed")
	o := orchestration.New(func(context.Context, profile.AgentProfile) (*agent.Agent, error) { return nil, boom })
	if _, err := o.Spawn(t.Context(), profile.AgentProfile{ID: "a1"}); !errors.Is(err, boom) {
		t.Fatalf("Spawn error = %v, want the factory error", err)
	}
	if _, ok := o.Get("a1"); ok || len(o.List()) != 0 {
		t.Fatal("a failed spawn left an agent registered")
	}
}

// TestOrchestration_RemoveThenRespawn: an id that was removed (disposed with its data,
// the way AgentCore.RemoveAgent does it) can be spawned again, and the new agent is a
// different, working one: the registry keeps no trace of the removed agent.
func TestOrchestration_RemoveThenRespawn(t *testing.T) {
	o := newOrchestration(t)
	first := spawn(t, o, "a1")
	spawn(t, o, "a2")

	if err := o.Delete(t.Context(), "a1"); err != nil {
		t.Fatalf("Delete(a1): %v", err)
	}
	if _, ok := o.Get("a1"); ok {
		t.Fatal("a1 is still registered after it was removed")
	}

	second, err := o.Spawn(t.Context(), profile.AgentProfile{ID: "a1", Name: "second life"})
	if err != nil {
		t.Fatalf("Spawn(a1) after the removal: %v", err)
	}
	if second == first {
		t.Fatal("the respawned agent is the removed one")
	}
	if got := second.Profile().Name; got != "second life" {
		t.Errorf("respawned agent profile name = %q, want the new profile", got)
	}
	if got, ok := o.Get("a1"); !ok || got != second {
		t.Errorf("Get(a1) = %v, %v, want the respawned agent", got, ok)
	}
	// The removed agent was really stopped: it cannot start jobs any more.
	// Update (A-126): scheduled tasks removed; a stopped agent refuses a chat instead.
	if _, err := first.Chat(t.Context(), "anything"); !errors.Is(err, agent.ErrAgentDisposed) {
		t.Errorf("Chat on the removed agent = %v, want ErrAgentDisposed", err)
	}
	var ids []string
	for _, a := range o.List() {
		ids = append(ids, a.ID())
	}
	if len(ids) != 2 || ids[0] != "a2" || ids[1] != "a1" {
		t.Errorf("List() = %v, want [a2 a1] (the respawn goes last)", ids)
	}
	_ = o.Dispose(t.Context(), "a1")
	_ = o.Dispose(t.Context(), "a2")
}

// The factory runs outside the registry lock: while one agent is being built, Get and
// List answer, the factory itself may read the registry, a Spawn of the same id fails
// with ErrAgentExists at once, and a factory failure releases the id.
func TestOrchestration_SpawnBuildsOutsideTheLock(t *testing.T) {
	deps := agent.Deps{LLMHub: llmhub.NewLLMHub(), ToolHub: toolhub.NewToolHub(), PersistenceHub: persistence.NewPersistenceHub(), Events: events.New()}
	entered, gate := make(chan struct{}), make(chan struct{})
	fail := true
	var o *orchestration.Orchestration
	o = orchestration.New(func(ctx context.Context, p profile.AgentProfile) (*agent.Agent, error) {
		if p.ID == "slow" && fail {
			_ = o.List() // the factory may read the registry
			close(entered)
			<-gate
			return nil, errors.New("build failed")
		}
		return agent.New(ctx, deps, p)
	})
	t.Cleanup(func() {
		for _, a := range o.List() {
			_ = a.Dispose(context.WithoutCancel(t.Context()))
		}
	})

	done := make(chan error, 1)
	go func() {
		_, err := o.Spawn(t.Context(), profile.AgentProfile{ID: "slow", Name: "slow"})
		done <- err
	}()
	<-entered
	if _, ok := o.Get("slow"); ok {
		t.Error("an agent that is still being built is visible")
	}
	if n := len(o.List()); n != 0 {
		t.Errorf("List = %d agents while building, want 0", n)
	}
	spawn(t, o, "other") // another id does not wait
	if _, err := o.Spawn(t.Context(), profile.AgentProfile{ID: "slow"}); !errors.Is(err, orchestration.ErrAgentExists) {
		t.Errorf("Spawn of an id being built = %v, want ErrAgentExists", err)
	}
	close(gate)
	if err := <-done; err == nil {
		t.Fatal("the failing build registered an agent")
	}
	if _, ok := o.Get("slow"); ok {
		t.Fatal("a failed build left the agent registered")
	}
	fail = false
	spawn(t, o, "slow") // the reservation was rolled back, so the id is free again
}
