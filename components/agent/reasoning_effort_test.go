package agent_test

import (
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/Phong95/aec-agent-go/components/agent"
	"github.com/Phong95/aec-agent-go/components/events"
	"github.com/Phong95/aec-agent-go/kernel"
)

// A-164: reasoning effort per session - Session.ReasoningEffort reaches
// llmhub.ModelOptions.ReasoningEffort from the next turn.

func lastEffort(t *testing.T, e *env) string {
	t.Helper()
	calls := e.mocks["primary"].Calls()
	if len(calls) == 0 {
		t.Fatal("no model call")
	}
	return calls[len(calls)-1].Options.ReasoningEffort
}

func TestReasoningEffortDefaultsToUnset(t *testing.T) {
	e := newEnv(t)
	a := e.spawn(t, testProfile("effort"))
	_, err := a.StartSession(t.Context())
	mustOK(t, err)
	_, err = a.Chat(t.Context(), "hi")
	mustOK(t, err)
	if got := lastEffort(t, e); got != "" {
		t.Fatalf("effort = %q, want none", got)
	}
}

func TestReasoningEffortIsStoredAnnouncedAndSent(t *testing.T) {
	e := newEnv(t)
	var mu sync.Mutex
	var fields [][]string
	e.bus.OnAny(func(ev kernel.Event) {
		if ev.Type != events.SessionUpdated.Name {
			return
		}
		if p, ok := ev.Payload.(events.SessionUpdatedPayload); ok {
			mu.Lock()
			fields = append(fields, p.Fields)
			mu.Unlock()
		}
	})
	a := e.spawn(t, testProfile("effort"))
	s, err := a.StartSession(t.Context())
	mustOK(t, err)

	saved, ok, err := a.SetReasoningEffort(t.Context(), s.ID, "high")
	mustOK(t, err)
	if !ok || saved.ReasoningEffort != "high" {
		t.Fatalf("saved = %+v, ok = %v", saved, ok)
	}
	stored, err := e.hub.Sessions.Get(t.Context(), s.ID)
	mustOK(t, err)
	if stored.ReasoningEffort != "high" {
		t.Fatalf("stored effort = %q", stored.ReasoningEffort)
	}
	mu.Lock()
	announced := slices.ContainsFunc(fields, func(f []string) bool { return slices.Equal(f, []string{"reasoningEffort"}) })
	mu.Unlock()
	if !announced {
		t.Fatalf("no SessionUpdated [reasoningEffort], got %v", fields)
	}

	_, err = a.Chat(t.Context(), "hi")
	mustOK(t, err)
	if got := lastEffort(t, e); got != "high" {
		t.Fatalf("effort = %q, want high", got)
	}

	// The call's own effort wins over the session's.
	_, err = a.Chat(t.Context(), "hi", agent.WithReasoningEffort("low"))
	mustOK(t, err)
	if got := lastEffort(t, e); got != "low" {
		t.Fatalf("effort = %q, want low", got)
	}

	// "" clears it.
	_, _, err = a.SetReasoningEffort(t.Context(), s.ID, "")
	mustOK(t, err)
	_, err = a.Chat(t.Context(), "hi")
	mustOK(t, err)
	if got := lastEffort(t, e); got != "" {
		t.Fatalf("effort = %q after clearing", got)
	}
}

func TestReasoningEffortRejectsMalformedLevels(t *testing.T) {
	e := newEnv(t)
	a := e.spawn(t, testProfile("effort"))
	s, err := a.StartSession(t.Context())
	mustOK(t, err)
	for _, bad := range []string{"High", "very high", strings.Repeat("x", 40)} {
		_, _, err := a.SetReasoningEffort(t.Context(), s.ID, bad)
		var se *kernel.SessionError
		if !errors.As(err, &se) || se.Code != kernel.SessionCodeInvalid {
			t.Fatalf("%q: err = %v, want SessionCodeInvalid", bad, err)
		}
	}
	_, ok, err := a.SetReasoningEffort(t.Context(), "no-such-session", "low")
	if err != nil || ok {
		t.Fatalf("unknown session: ok = %v, err = %v", ok, err)
	}
}
