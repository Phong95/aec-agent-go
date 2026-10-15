package sandboxhub_test

import (
	"context"
	"slices"
	"sync"
	"testing"

	"github.com/Phong95/aec-agent-go/aecmock"
	"github.com/Phong95/aec-agent-go/components/events"
	"github.com/Phong95/aec-agent-go/components/sandboxhub"
)

type eventsCtxKey struct{}

// recordingProvider records the shared-folder removals the hub makes, with the
// eventsCtxKey value of their ctx.
type recordingProvider struct {
	*aecmock.InMemoryProvider
	mu    sync.Mutex
	calls []string
}

func (p *recordingProvider) RemoveData(ctx context.Context, spec sandboxhub.Spec, cc sandboxhub.CreateContext, agentID string) error {
	p.mu.Lock()
	tag, _ := ctx.Value(eventsCtxKey{}).(string)
	p.calls = append(p.calls, agentID+"@"+tag)
	p.mu.Unlock()
	return p.InMemoryProvider.RemoveData(ctx, spec, cc, agentID)
}

func (p *recordingProvider) list() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.calls)
}

// TestSandboxHubWithEventsReleasesRemovedAgents: with WithEvents the hub releases a
// removed agent's shared-sandbox folder itself, with the publisher's ctx, until Dispose.
func TestSandboxHubWithEventsReleasesRemovedAgents(t *testing.T) {
	bus := events.New()
	hub := sandboxhub.NewSandboxHub(sandboxhub.WithEvents(bus))
	provider := &recordingProvider{InMemoryProvider: aecmock.NewInMemoryProvider()}
	if err := hub.Register(provider); err != nil {
		t.Fatal(err)
	}
	if err := hub.ConfigureShared(sandboxhub.Spec{Kind: sandboxhub.KindDocker}); err != nil {
		t.Fatal(err)
	}

	ctx := context.WithValue(t.Context(), eventsCtxKey{}, "pub")
	events.Publish(ctx, bus, events.AgentRemoved, "", events.AgentRemovedPayload{AgentID: "a1"})
	if got := provider.list(); !slices.Equal(got, []string{"a1@pub"}) {
		t.Fatalf("removals = %v, want a1 with the publisher's ctx", got)
	}

	if err := hub.Dispose(t.Context()); err != nil {
		t.Fatal(err)
	}
	events.Publish(t.Context(), bus, events.AgentRemoved, "", events.AgentRemovedPayload{AgentID: "a2"})
	if got := provider.list(); len(got) != 1 {
		t.Fatalf("removals after Dispose = %v, want none added", got)
	}
}

// TestSandboxHubWithoutEventsDoesNotSubscribe: a hub built without WithEvents ignores the bus.
func TestSandboxHubWithoutEventsDoesNotSubscribe(t *testing.T) {
	bus := events.New()
	hub := sandboxhub.NewSandboxHub()
	provider := &recordingProvider{InMemoryProvider: aecmock.NewInMemoryProvider()}
	if err := hub.Register(provider); err != nil {
		t.Fatal(err)
	}
	if err := hub.ConfigureShared(sandboxhub.Spec{Kind: sandboxhub.KindDocker}); err != nil {
		t.Fatal(err)
	}
	events.Publish(t.Context(), bus, events.AgentRemoved, "", events.AgentRemovedPayload{AgentID: "a1"})
	if got := provider.list(); len(got) != 0 {
		t.Fatalf("removals = %v, want none", got)
	}
}
