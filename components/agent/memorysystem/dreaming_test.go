package memorysystem

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/Phong95/aec-agent-go/components/agent/memorysystem/memory"
	"github.com/Phong95/aec-agent-go/components/persistence"
	"github.com/Phong95/aec-agent-go/kernel"
)

// ---------------------------------------------------------------------------
// Consolidation against the recorded fixture.
//
// memorysystem-consolidate.json is recorded by tools/wiregen/memory/pipeline.cases.mjs:
// each scenario ran a consolidation pass over the SDK's in-memory message, session and
// memory stores and a scripted model, under a pinned clock and a fresh id counter. The
// Go test replays the steps and requires the same ConsolidationResults, the same
// message ranges read, the same chat input, the same checkpoints and the same records.
// ---------------------------------------------------------------------------

// sessionSummary is the part of a session the fixture records.
type sessionSummary struct {
	UpdatedAt                int64 `json:"updatedAt"`
	ConsolidatedMessageCount *int  `json:"consolidatedMessageCount"`
}

type conStep struct {
	SessionID string           `json:"sessionId"`
	Reply     reply            `json:"reply"`
	AdvanceMs int64            `json:"advanceMs"`
	Append    []kernel.Message `json:"append"`
}

type conScenario struct {
	Name     string                      `json:"name"`
	AgentID  string                      `json:"agentId"`
	ClockMs  int64                       `json:"clockMs"`
	Config   *memory.DreamingConfig      `json:"config"`
	Seed     []memory.Record             `json:"seed"`
	Sessions []conSession                `json:"sessions"`
	Messages map[string][]kernel.Message `json:"messages"`
	Steps    []conStep                   `json:"steps"`
	Result   struct {
		Steps []struct {
			Results  json.RawMessage `json:"results"`
			Throws   *string         `json:"throws"`
			GetRange json.RawMessage `json:"getRange"`
			Chat     json.RawMessage `json:"chat"`
			Session  json.RawMessage `json:"session"`
		} `json:"steps"`
		Store    json.RawMessage            `json:"store"`
		Sessions map[string]json.RawMessage `json:"sessions"`
	} `json:"result"`
}

type conSession struct {
	ID                       string `json:"id"`
	UpdatedAt                *int64 `json:"updatedAt"`
	ConsolidatedMessageCount *int   `json:"consolidatedMessageCount"`
}

func TestConsolidateMatchesGolden(t *testing.T) {
	var scenarios []conScenario
	loadFixture(t, "memorysystem-consolidate", &scenarios)
	if len(scenarios) < 15 {
		t.Fatalf("the fixture holds %d scenarios, want at least 15", len(scenarios))
	}
	for _, sc := range scenarios {
		t.Run(sc.Name, func(t *testing.T) {
			ctx := t.Context()
			ms := sc.ClockMs
			clock := func() time.Time { return time.UnixMilli(ms) }

			store := seedStore(t, sc.Seed...)
			sessions := persistence.NewMemorySessionStore()
			for _, s := range sc.Sessions {
				updated := int64(2000)
				if s.UpdatedAt != nil {
					updated = *s.UpdatedAt
				}
				if err := sessions.Update(ctx, kernel.Session{ID: s.ID, CreatedAt: 1000, UpdatedAt: updated, ConsolidatedMessageCount: s.ConsolidatedMessageCount}); err != nil {
					t.Fatal(err)
				}
			}
			messages := &countingMessages{MessageStore: persistence.NewMemoryMessageStore()}
			for sid, list := range sc.Messages {
				if err := messages.Set(ctx, sid, list); err != nil {
					t.Fatal(err)
				}
			}

			replies := make([]reply, len(sc.Steps))
			for i, s := range sc.Steps {
				replies[i] = s.Reply
			}
			model := newFakeModel(replies...)
			d := newTestDeps(sc.AgentID, WithClock(clock), WithIDs(tsIDs(sc.ClockMs)), WithLocation(time.UTC))
			dr := newDreaming(d, messages, sessions, store, newMemory(d, model, store), sc.Config)

			if len(sc.Steps) != len(sc.Result.Steps) {
				t.Fatalf("%d steps, the fixture has %d results", len(sc.Steps), len(sc.Result.Steps))
			}
			for i, step := range sc.Steps {
				ms += step.AdvanceMs
				for _, m := range step.Append {
					if err := messages.Append(ctx, step.SessionID, m); err != nil {
						t.Fatal(err)
					}
				}
				messages.take()
				callsBefore := len(model.extractionCalls())

				got, err := dr.consolidate(ctx, step.SessionID)

				want := sc.Result.Steps[i]
				if want.Throws != nil {
					if err == nil || err.Error() != *want.Throws {
						t.Errorf("step %d: error = %v, want %q", i, err, *want.Throws)
					}
				} else if err != nil {
					t.Fatalf("step %d: %v", i, err)
				} else if gotJSON := encode(t, got); gotJSON != string(want.Results) {
					t.Errorf("step %d: results differ\n got: %s\nwant: %s", i, gotJSON, want.Results)
				}
				if gotJSON := encode(t, messages.take()); gotJSON != string(want.GetRange) {
					t.Errorf("step %d: getRange calls differ\n got: %s\nwant: %s", i, gotJSON, want.GetRange)
				}
				var chat []string
				for _, call := range model.extractionCalls()[callsBefore:] {
					chat = append(chat, call[1].Content)
				}
				if chat == nil {
					chat = []string{}
				}
				if gotJSON := encode(t, chat); gotJSON != string(want.Chat) {
					t.Errorf("step %d: chat input differs\n got: %s\nwant: %s", i, gotJSON, want.Chat)
				}
				if gotJSON := encode(t, summarize(t, sessions, step.SessionID)); gotJSON != string(want.Session) {
					t.Errorf("step %d: session differs\n got: %s\nwant: %s", i, gotJSON, want.Session)
				}
			}

			// supersededBy and sourceTrust differ from the recording: see goStoreJSON.
			if gotStore, wantStore := encode(t, storeRecords(t, store)), goStoreJSON(t, sc.Result.Store, sc.Seed); gotStore != wantStore {
				t.Errorf("store differs\n got: %s\nwant: %s", gotStore, sc.Result.Store)
			}
			for sid, want := range sc.Result.Sessions {
				if gotJSON := encode(t, summarize(t, sessions, sid)); gotJSON != string(want) {
					t.Errorf("session %s differs\n got: %s\nwant: %s", sid, gotJSON, want)
				}
			}
		})
	}
}

// summarize reads a session as the fixture records it; an unknown session is nil.
func summarize(t *testing.T, s persistence.SessionStore, id string) *sessionSummary {
	t.Helper()
	sess, err := s.Get(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	if sess == nil {
		return nil
	}
	return &sessionSummary{UpdatedAt: sess.UpdatedAt, ConsolidatedMessageCount: sess.ConsolidatedMessageCount}
}

// ---------------------------------------------------------------------------
// Tests for the consolidation-trigger contracts on the Dreaming level (the
// scheduler cases arrive with plan 10-03).
// ---------------------------------------------------------------------------

type dreamRig struct {
	store    *persistence.MemoryMemoryStore
	sessions *persistence.MemorySessionStore
	messages *countingMessages
	model    *fakeModel
	dr       *dreaming
}

func newDreamRig(t *testing.T, replies ...reply) *dreamRig {
	t.Helper()
	store := persistence.NewMemoryMemoryStore()
	sessions := persistence.NewMemorySessionStore()
	messages := &countingMessages{MessageStore: persistence.NewMemoryMessageStore()}
	model := newFakeModel(replies...)
	d := newTestDeps("agent-1", WithClock(fixedClock(1700000000000)), WithLocation(time.UTC))
	if err := sessions.Update(t.Context(), kernel.Session{ID: "s1", CreatedAt: 1, UpdatedAt: 2}); err != nil {
		t.Fatal(err)
	}
	return &dreamRig{store: store, sessions: sessions, messages: messages, model: model, dr: newDreaming(d, messages, sessions, store, newMemory(d, model, store), nil)}
}

func (r *dreamRig) appendMessages(t *testing.T, texts ...string) {
	t.Helper()
	for i, text := range texts {
		role := kernel.RoleUser
		if i%2 == 1 {
			role = kernel.RoleAssistant
		}
		if err := r.messages.Append(t.Context(), "s1", kernel.Message{Role: role, Content: text}); err != nil {
			t.Fatal(err)
		}
	}
}

func (r *dreamRig) checkpoint(t *testing.T) int {
	t.Helper()
	n, err := r.dr.consolidatedMessageCount(t.Context(), "s1")
	if err != nil {
		t.Fatal(err)
	}
	return n
}

const oneMemory = `[{"kind":"fact","scope":"project","subject":"site","key":"site_name","value":"Tower A","confidence":"explicit"}]`

func TestConsolidation_ReadsOnlyNewMessages(t *testing.T) {
	r := newDreamRig(t, reply{Content: oneMemory}, reply{Content: "[]"})
	r.appendMessages(t, "one", "two")
	if _, err := r.dr.consolidate(t.Context(), "s1"); err != nil {
		t.Fatal(err)
	}
	if got := r.messages.take(); len(got) != 1 || got[0] != [3]any{"s1", 2, 2} {
		t.Fatalf("first pass read %v, want [s1 2 2]", got)
	}
	if r.checkpoint(t) != 2 {
		t.Fatalf("checkpoint = %d, want 2", r.checkpoint(t))
	}

	r.appendMessages(t, "three", "four")
	if _, err := r.dr.consolidate(t.Context(), "s1"); err != nil {
		t.Fatal(err)
	}
	// The range read is (session.id, 4, 2): only the two new messages are read.
	if got := r.messages.take(); len(got) != 1 || got[0] != [3]any{"s1", 4, 2} {
		t.Fatalf("second pass read %v, want [s1 4 2]", got)
	}
	calls := r.model.extractionCalls()
	if len(calls) != 2 || calls[1][1].Content == calls[0][1].Content {
		t.Fatalf("the second pass must extract from the new messages only: %v", calls)
	}
}

func TestConsolidation_TruncatedOutputKeepsCheckpoint(t *testing.T) {
	for name, bad := range map[string]reply{
		"truncated":    {Content: "[]", StopReason: kernel.StopMaxTokens, Usage: &kernel.TokenUsage{OutputTokens: 4096}},
		"empty":        {Content: ""},
		"not an array": {Content: `{"a":1}`},
		"model error":  {Throws: strPtr("provider exploded")},
	} {
		t.Run(name, func(t *testing.T) {
			r := newDreamRig(t, bad, reply{Content: oneMemory})
			r.appendMessages(t, "one", "two", "three")

			if _, err := r.dr.consolidate(t.Context(), "s1"); err == nil {
				t.Fatal("the pass must fail")
			}
			if r.checkpoint(t) != 0 {
				t.Fatalf("a failed pass moved the checkpoint to %d", r.checkpoint(t))
			}
			if n := len(storeRecords(t, r.store)); n != 0 {
				t.Fatalf("a failed pass stored %d records", n)
			}

			// The next pass retries the same messages and advances the checkpoint.
			got, err := r.dr.consolidate(t.Context(), "s1")
			if err != nil || len(got) != 1 || got[0].Action != memory.ActionPromote {
				t.Fatalf("retry = %v, %v", got, err)
			}
			if r.checkpoint(t) != 3 {
				t.Fatalf("checkpoint = %d after the retry, want 3", r.checkpoint(t))
			}
			calls := r.messages.take()
			if len(calls) != 2 || calls[0] != calls[1] {
				t.Fatalf("both passes must read the same range, got %v", calls)
			}
		})
	}
}

func TestConsolidation_CheckpointKeepsUpdatedAt(t *testing.T) {
	r := newDreamRig(t, reply{Content: oneMemory})
	r.appendMessages(t, "one", "two")
	if _, err := r.dr.consolidate(t.Context(), "s1"); err != nil {
		t.Fatal(err)
	}
	sess, err := r.sessions.Get(t.Context(), "s1")
	if err != nil || sess == nil {
		t.Fatalf("session: %v %v", sess, err)
	}
	if sess.UpdatedAt != 2 || sess.ConsolidatedMessageCount == nil || *sess.ConsolidatedMessageCount != 2 {
		t.Fatalf("session = %+v, want updatedAt 2 (untouched) and checkpoint 2", sess)
	}
}

func TestConsolidation_StaleWriteBackKeepsCheckpoint(t *testing.T) {
	r := newDreamRig(t, reply{Content: oneMemory})
	r.appendMessages(t, "one", "two")
	stale, err := r.sessions.Get(t.Context(), "s1")
	if err != nil || stale == nil {
		t.Fatal(err)
	}
	if _, err := r.dr.consolidate(t.Context(), "s1"); err != nil {
		t.Fatal(err)
	}
	// A conversation manager that cached the session before the pass writes it back.
	if err := r.sessions.Update(t.Context(), *stale); err != nil {
		t.Fatal(err)
	}
	if r.checkpoint(t) != 2 {
		t.Fatalf("a stale write-back lowered the checkpoint to %d", r.checkpoint(t))
	}
}

func TestConsolidation_StampsAgentID(t *testing.T) {
	r := newDreamRig(t, reply{Content: oneMemory})
	r.appendMessages(t, "one", "two")
	if _, err := r.dr.consolidate(t.Context(), "s1"); err != nil {
		t.Fatal(err)
	}
	recs := storeRecords(t, r.store)
	if len(recs) != 1 || recs[0].AgentID != "agent-1" {
		t.Fatalf("records = %+v, want one stamped with agent-1", recs)
	}
}

func TestConsolidation_AgentIsolation(t *testing.T) {
	r := newDreamRig(t, reply{Content: oneMemory})
	foreign := memory.Record{
		ID: "foreign", AgentID: "other-agent", Kind: memory.KindFact, Scope: memory.ScopeProject, Subject: "site", Key: "site_name",
		Value: "Tower B", Status: memory.StatusActive, Confidence: memory.ConfidenceExplicit, Source: "conversation",
		SourceTrust: memory.TrustUserApproved, ObservedAt: "2023-11-01T00:00:00.000Z",
	}
	if err := r.store.Save(t.Context(), foreign); err != nil {
		t.Fatal(err)
	}
	r.appendMessages(t, "one", "two")
	if _, err := r.dr.consolidate(t.Context(), "s1"); err != nil {
		t.Fatal(err)
	}
	got, err := r.store.Get(t.Context(), "foreign")
	if err != nil || got == nil || got.Status != memory.StatusActive || got.Value != "Tower B" || got.UpdatedAt != "" {
		t.Fatalf("another agent's memory was touched: %+v %v", got, err)
	}
	if n := len(storeRecords(t, r.store)); n != 2 {
		t.Fatalf("store holds %d records, want the foreign one and a new own one", n)
	}
}

func TestDreaming_ExpiryDaysZeroIsTheDefault(t *testing.T) {
	d := newTestDeps("agent-1")
	dr := newDreaming(d, nil, nil, nil, nil, &memory.DreamingConfig{})
	if got := dr.expiryDaysNow(); got != 30 {
		t.Fatalf("0 days = %v, want the 30 day default", got)
	}
	dr.updateConfig(memory.DreamingConfig{MemoryExpiryDays: 7})
	if got := dr.expiryDaysNow(); got != 7 {
		t.Fatalf("updated days = %v, want 7", got)
	}
	dr.updateConfig(memory.DreamingConfig{})
	if got := dr.expiryDaysNow(); got != 30 {
		t.Fatalf("reset days = %v, want 30", got)
	}
}

func TestDreaming_ExpiryOutOfRangeIsTheRangeError(t *testing.T) {
	d := newTestDeps("agent-1", WithClock(fixedClock(1700000000000)))
	dr := newDreaming(d, nil, nil, seedStore(t), nil, &memory.DreamingConfig{MemoryExpiryDays: 1e12})
	if _, err := dr.expireStale(t.Context()); err == nil || err.Error() != "Invalid time value" {
		t.Fatalf("error = %v, want the toISOString RangeError text", err)
	}
}

func strPtr(s string) *string { return &s }
