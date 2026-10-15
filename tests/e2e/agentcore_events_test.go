package e2e_test

import (
	"context"
	"encoding/json"
	"slices"
	"sync"
	"testing"

	aec "github.com/Phong95/aec-agent-go"
	"github.com/Phong95/aec-agent-go/components/agent/profile"
	"github.com/Phong95/aec-agent-go/components/events"
	"github.com/Phong95/aec-agent-go/components/skillhub"
	"github.com/Phong95/aec-agent-go/components/skillhub/skill"
	"github.com/Phong95/aec-agent-go/kernel"
)

// lifecycleRecorder collects the lifecycle events a host hears through OnAny.
type lifecycleRecorder struct {
	mu     sync.Mutex
	events []kernel.Event
}

func recordLifecycle(core *aec.AgentCore) *lifecycleRecorder {
	r := &lifecycleRecorder{}
	lifecycle := []string{events.AgentSpawned.Name, events.AgentRemoved.Name, events.AgentProfileChanged.Name, events.SkillDeleted.Name}
	core.Events().OnAny(func(e kernel.Event) {
		if !slices.Contains(lifecycle, e.Type) {
			return
		}
		r.mu.Lock()
		defer r.mu.Unlock()
		r.events = append(r.events, e)
	})
	return r
}

func (r *lifecycleRecorder) types() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, 0, len(r.events))
	for _, e := range r.events {
		out = append(out, e.Type)
	}
	return out
}

func (r *lifecycleRecorder) all() []kernel.Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.events)
}

// TestAgentCore_LifecycleEvents: the AgentCore emits agent:spawned, agent:removed,
// agent:profile-changed and skill:deleted (SessionID "") for hosts, on top of the
// built-in work that stays direct calls.
func TestAgentCore_LifecycleEvents(t *testing.T) {
	t.Run("spawn and remove emit agent:spawned and agent:removed", func(t *testing.T) {
		core, err := aec.NewAgentCore()
		if err != nil {
			t.Fatal(err)
		}
		rec := recordLifecycle(core)

		if _, err := core.SpawnAgent(t.Context(), testProfile("a1")); err != nil {
			t.Fatal(err)
		}
		if err := core.RemoveAgent(t.Context(), "a1"); err != nil {
			t.Fatal(err)
		}

		want := []string{events.AgentSpawned.Name, events.AgentRemoved.Name}
		if got := rec.types(); !slices.Equal(got, want) {
			t.Fatalf("lifecycle events = %v, want %v", got, want)
		}
		for _, e := range rec.all() {
			if e.SessionID != "" {
				t.Errorf("%s has SessionID %q, want \"\"", e.Type, e.SessionID)
			}
		}
		removed, err := events.PayloadAs[events.AgentRemovedPayload](rec.all()[1])
		if err != nil || removed.AgentID != "a1" {
			t.Errorf("agent:removed payload = %+v, %v, want agentId a1", removed, err)
		}
	})

	t.Run("a skill delete emits agent:profile-changed per changed agent, then skill:deleted", func(t *testing.T) {
		core, err := aec.NewAgentCore()
		if err != nil {
			t.Fatal(err)
		}
		for id, skills := range map[string][]string{"a1": {"site-diary", "other"}, "a2": {"other"}} {
			p := testProfile(id)
			p.Skills = skills
			if _, err := core.SpawnAgent(t.Context(), p); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := core.SkillHub().Create(t.Context(), skill.Package{Content: skillMD("site-diary")}, skillhub.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
		rec := recordLifecycle(core)

		if err := core.SkillHub().Delete(t.Context(), "site-diary"); err != nil {
			t.Fatal(err)
		}

		want := []string{events.AgentProfileChanged.Name, events.SkillDeleted.Name}
		if got := rec.types(); !slices.Equal(got, want) {
			t.Fatalf("lifecycle events = %v, want %v (a2 never had the skill)", got, want)
		}
		changed, err := events.PayloadAs[events.AgentProfileChangedPayload](rec.all()[0])
		if err != nil || changed.AgentID != "a1" {
			t.Fatalf("agent:profile-changed payload = %+v, %v, want agentId a1", changed, err)
		}
		var p profile.AgentProfile
		if err := json.Unmarshal(changed.Profile, &p); err != nil || p.ID != "a1" || !slices.Equal(p.Skills, []string{"other"}) {
			t.Errorf("agent:profile-changed profile = %+v, %v, want a1 with skills [other]", p, err)
		}
		deleted, err := events.PayloadAs[events.SkillDeletedPayload](rec.all()[1])
		if err != nil || deleted.Name != "site-diary" {
			t.Errorf("skill:deleted payload = %+v, %v, want name site-diary", deleted, err)
		}
	})
}

// TestAgentCore_RemoveAgentReleasesThroughHubSubscribers: the sandbox hub and the
// artifact hub release the agent's data as subscribers of agent:removed, with the ctx
// RemoveAgent was called with, before a host's subscriber runs.
func TestAgentCore_RemoveAgentReleasesThroughHubSubscribers(t *testing.T) {
	r := newRemoveRig(t)
	var hostSaw []string
	r.core.Events().On(events.AgentRemoved.Name, func(kernel.Event) { hostSaw = r.log.list() })

	ctx := context.WithValue(t.Context(), rigCtxKey{}, "remove-call")
	if err := r.core.RemoveAgent(ctx, "a1"); err != nil {
		t.Fatalf("RemoveAgent: %v", err)
	}
	if got := r.log.ctxList(); !slices.Equal(got, []any{"remove-call", "remove-call"}) {
		t.Fatalf("ctx seen by the two releases = %v, want the RemoveAgent ctx twice", got)
	}
	want := []string{"agent.own-data", "agents.delete", "sandbox.shared", "artifacts.release"}
	if !slices.Equal(hostSaw, want) {
		t.Fatalf("steps when the host subscriber ran = %v, want %v (hubs first)", hostSaw, want)
	}
}
