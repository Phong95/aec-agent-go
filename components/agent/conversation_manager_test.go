package agent

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/Phong95/aec-agent-go/components/persistence"
	"github.com/Phong95/aec-agent-go/kernel"
	"github.com/Phong95/aec-agent-go/kernel/ordered"
)

// flakyMessages is a message store whose Append fails while fail is set, and whose
// Get blocks on hold (when set) after it entered, so a test can run a write in the
// middle of a LoadSession.
type flakyMessages struct {
	persistence.MessageStore
	mu      sync.Mutex
	fail    bool
	hold    chan struct{}
	entered chan struct{}
}

func (f *flakyMessages) Append(ctx context.Context, id string, m kernel.Message) error {
	f.mu.Lock()
	fail := f.fail
	f.mu.Unlock()
	if fail {
		return errors.New("store down")
	}
	return f.MessageStore.Append(ctx, id, m)
}

func (f *flakyMessages) Get(ctx context.Context, id string) ([]kernel.Message, error) {
	f.mu.Lock()
	hold, entered := f.hold, f.entered
	f.mu.Unlock()
	if hold != nil {
		close(entered)
		<-hold
	}
	return f.MessageStore.Get(ctx, id)
}

// The cache follows the store (store first, cache second): a history append the store
// refuses leaves the in-memory history unchanged, so the cache is never ahead of the
// store.
// (A-97: the cache is the per-turn Conversation's.)
func TestConversationManager_StoreFirst(t *testing.T) {
	msgs := &flakyMessages{MessageStore: persistence.NewMemoryMessageStore()}
	hub := persistence.NewPersistenceHub(persistence.WithMessages(msgs))
	m := NewConversationManager(hub)
	ctx := t.Context()
	s, err := m.StartSession(ctx, ordered.Object{}, "")
	if err != nil {
		t.Fatal(err)
	}
	c := mustOpen(t, m, s.ID)
	if err := c.AddMessage(ctx, kernel.Message{Role: kernel.RoleUser, Content: "kept"}); err != nil {
		t.Fatal(err)
	}
	msgs.mu.Lock()
	msgs.fail = true
	msgs.mu.Unlock()
	if err := c.AddMessage(ctx, kernel.Message{Role: kernel.RoleUser, Content: "lost"}); err == nil {
		t.Fatal("AddMessage with a failing store = nil, want the store error")
	}
	stored, err := msgs.MessageStore.Get(ctx, s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := c.Messages(); len(got) != 1 || len(stored) != 1 || got[0].Content != "kept" {
		t.Fatalf("cache %v, store %v: want both to hold only the kept message", got, stored)
	}
}

// LoadSession of an unknown id leaves the active session and its caches alone.
// (A-97: the default session; there are no caches any more.)
func TestConversationManager_LoadUnknownKeepsActive(t *testing.T) {
	c := NewConversationManager(persistence.NewPersistenceHub())
	ctx := t.Context()
	s, err := c.StartSession(ctx, ordered.Object{}, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok, err := c.LoadSession(ctx, "nope"); ok || err != nil {
		t.Fatalf("LoadSession(nope) = %v, %v", ok, err)
	}
	active, ok := c.DefaultSessionID()
	if !ok || active != s.ID {
		t.Fatalf("after a miss: default session %q %v; want %q", active, ok, s.ID)
	}
	if _, ok, err := c.Open(ctx, "nope"); ok || err != nil {
		t.Fatalf("Open(nope) = %v, %v, want false and no error", ok, err)
	}
}

// mustOpen opens a session's per-turn Conversation (A-97).
func mustOpen(t *testing.T, m *ConversationManager, sessionID string) *Conversation {
	t.Helper()
	c, ok, err := m.Open(t.Context(), sessionID)
	if err != nil || !ok {
		t.Fatalf("Open(%s) = %v, %v", sessionID, ok, err)
	}
	return c
}

// A LoadSession and an AddMessage that run at the same time never lose the message: the
// write waits for the load (it takes the write mutex), then appends to the history the
// load installed. The readers of the cache do not wait for the store while the load is
// held in it. Real time, not synctest: a goroutine blocked on a sync.Mutex is not
// durably blocked, so synctest.Wait would never return.
//
// -> Superseded (A-97): LoadSession loads no history and there is no cache to race, so
// the test above is gone (with its note "The cache readers are not blocked by the store
// I/O of the load."). Each turn opens its own Conversation from the stores, so
// what the host wrote between two turns (here straight into the stores) is in the next
// turn's history, and two sessions' Conversations never share state.
func TestConversationManager_OpenReadsTheStores(t *testing.T) {
	hub := persistence.NewPersistenceHub()
	m := NewConversationManager(hub)
	ctx := t.Context()
	s, err := m.StartSession(ctx, ordered.Object{}, "")
	if err != nil {
		t.Fatal(err)
	}
	first := mustOpen(t, m, s.ID)
	if err := first.AddMessage(ctx, kernel.Message{Role: kernel.RoleUser, Content: "one"}); err != nil {
		t.Fatal(err)
	}
	// A host write between two turns.
	if err := hub.Messages.Append(ctx, s.ID, kernel.Message{Role: kernel.RoleAssistant, Content: "host"}); err != nil {
		t.Fatal(err)
	}
	second := mustOpen(t, m, s.ID)
	got := second.Messages()
	if len(got) != 2 || got[0].Content != "one" || got[1].Content != "host" {
		t.Fatalf("history of the next turn = %v, want the turn's message and the host's", got)
	}
	if stored, err := m.Messages(ctx, s.ID); err != nil || len(stored) != 2 {
		t.Fatalf("Messages = %v, %v, want both messages", stored, err)
	}
}

// Unexported-level regression test of the phase 10 code review (WR-A04).

// A write whose ctx is bound to a session that is no longer the active one is refused
// and touches nothing; a ctx without the binding writes to the active session as before.
//
// -> Superseded (A-97): there is no binding to check (nor its "No binding: the active
// session, as before." case). A turn's Conversation is bound to
// its session by construction, so a StartSession while it is open moves none of its
// writes into the new session.
func TestConversationManager_ConversationBoundToSession(t *testing.T) {
	hub := persistence.NewPersistenceHub()
	m := NewConversationManager(hub)
	ctx := t.Context()

	s1, err := m.StartSession(ctx, ordered.Object{}, "")
	if err != nil {
		t.Fatal(err)
	}
	c1 := mustOpen(t, m, s1.ID)
	s2, err := m.StartSession(ctx, ordered.Object{}, "")
	if err != nil {
		t.Fatal(err)
	}
	usage := &kernel.TokenUsage{InputTokens: 5}
	if err := c1.AddMessage(ctx, kernel.Message{Role: kernel.RoleAssistant, Content: "two"}); err != nil {
		t.Fatal(err)
	}
	if err := c1.UpdateSession(ctx, kernel.Session{CurrentTokenUsage: usage}); err != nil {
		t.Fatal(err)
	}
	if msgs, _ := hub.Messages.Get(ctx, s2.ID); len(msgs) != 0 {
		t.Fatalf("the second session holds %v, want nothing from the first session's turn", msgs)
	}
	if stored, _ := hub.Sessions.Get(ctx, s2.ID); stored == nil || stored.CurrentTokenUsage != nil {
		t.Fatalf("second session = %+v, want no usage from the first session's turn", stored)
	}
	if stored, _ := hub.Sessions.Get(ctx, s1.ID); stored == nil || stored.CurrentTokenUsage == nil || stored.CurrentTokenUsage.InputTokens != 5 {
		t.Fatalf("first session = %+v, want the turn's usage", stored)
	}
	if id, _ := m.DefaultSessionID(); id != s2.ID {
		t.Fatalf("default session = %q, want the second one", id)
	}
}
