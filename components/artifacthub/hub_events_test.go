package artifacthub_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/Phong95/aec-agent-go/components/artifacthub"
	"github.com/Phong95/aec-agent-go/components/events"
	"github.com/Phong95/aec-agent-go/components/persistence"
	"github.com/Phong95/aec-agent-go/kernel"
)

type eventsCtxKey struct{}

// deleteRecorder records DeleteByAgent with the eventsCtxKey value of its ctx, and can fail.
type deleteRecorder struct {
	persistence.ArtifactStore
	mu    sync.Mutex
	calls []string
	err   error
}

func (s *deleteRecorder) DeleteByAgent(ctx context.Context, agentID string) error {
	s.mu.Lock()
	tag, _ := ctx.Value(eventsCtxKey{}).(string)
	s.calls = append(s.calls, agentID+"@"+tag)
	err := s.err
	s.mu.Unlock()
	if err != nil {
		return err
	}
	return s.ArtifactStore.DeleteByAgent(ctx, agentID)
}

func (s *deleteRecorder) list() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.calls)
}

// TestArtifactHubWithEventsDeletesRemovedAgents: with WithEvents the hub deletes a
// removed agent's artifacts itself, with the publisher's ctx, until Dispose.
func TestArtifactHubWithEventsDeletesRemovedAgents(t *testing.T) {
	bus := events.New()
	store := &deleteRecorder{ArtifactStore: persistence.NewMemoryArtifactStore()}
	hub := artifacthub.NewArtifactHub(func() persistence.ArtifactStore { return store }, artifacthub.WithEvents(bus))

	ctx := context.WithValue(t.Context(), eventsCtxKey{}, "pub")
	events.Publish(ctx, bus, events.AgentRemoved, "", events.AgentRemovedPayload{AgentID: "a1"})
	if got := store.list(); !slices.Equal(got, []string{"a1@pub"}) {
		t.Fatalf("deletes = %v, want a1 with the publisher's ctx", got)
	}

	if err := hub.Dispose(t.Context()); err != nil {
		t.Fatal(err)
	}
	events.Publish(t.Context(), bus, events.AgentRemoved, "", events.AgentRemovedPayload{AgentID: "a2"})
	if got := store.list(); len(got) != 1 {
		t.Fatalf("deletes after Dispose = %v, want none added", got)
	}
}

// TestArtifactHubWithEventsLogsAFailedRelease: a failing delete is logged with the
// agent id and the error, through WithLogger, and does not stop the publish.
func TestArtifactHubWithEventsLogsAFailedRelease(t *testing.T) {
	bus := events.New()
	var logs bytes.Buffer
	store := &deleteRecorder{ArtifactStore: persistence.NewMemoryArtifactStore(), err: errors.New("db is down")}
	artifacthub.NewArtifactHub(func() persistence.ArtifactStore { return store },
		artifacthub.WithEvents(bus), artifacthub.WithLogger(slog.New(slog.NewTextHandler(&logs, nil))))
	var after int
	bus.On(events.AgentRemoved.Name, func(kernel.Event) { after++ })

	events.Publish(t.Context(), bus, events.AgentRemoved, "", events.AgentRemovedPayload{AgentID: "a1"})
	if after != 1 {
		t.Errorf("subscriber after the failing release ran %d times, want 1", after)
	}

	for _, frag := range []string{"a1", "db is down", "artifacts: could not delete"} {
		if !strings.Contains(logs.String(), frag) {
			t.Errorf("log = %q, want it to contain %q", logs.String(), frag)
		}
	}
}
