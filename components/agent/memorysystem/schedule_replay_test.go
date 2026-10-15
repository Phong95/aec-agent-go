package memorysystem

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/Phong95/aec-agent-go/components/agent/memorysystem/memory"
	"github.com/Phong95/aec-agent-go/components/events"
	"github.com/Phong95/aec-agent-go/components/persistence"
	"github.com/Phong95/aec-agent-go/kernel"
	"github.com/Phong95/aec-agent-go/kernel/ordered"
)

// ---------------------------------------------------------------------------
// The recorded schedule transcripts
//
// memorysystem-schedule.json is recorded by tools/wiregen/memory/schedule.cases.mjs:
// each scenario drove a real memory system and consolidation scheduler over the SDK's
// in-memory PersistenceHub, a real event dispatcher and a scripted model, with a manual
// fake of setInterval and a pinned clock, and recorded every consolidate call as
// [offset ms, session id], the user text of every extraction call, the records and the
// checkpoints. The Go test replays the same steps in its own synctest bubble against
// the real System: the actor goroutine, the real dispatcher and the in-memory stores
// run on the bubble's fake clock, which is mapped to the recorded clock, so the records
// carry the same timestamps. It requires the same call offsets, the same chat inputs,
// the same records byte for byte (ids restart at 1 per scenario, as the recorder
// normalizes them) and the same checkpoints.
// ---------------------------------------------------------------------------

// scheduleCall is one recorded consolidate call: the offset in virtual milliseconds
// and the session. In the fixture it is the array [ms, "session"].
type scheduleCall struct {
	Ms      int64
	Session string
}

func (c *scheduleCall) UnmarshalJSON(b []byte) error {
	var raw [2]json.RawMessage
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	if err := json.Unmarshal(raw[0], &c.Ms); err != nil {
		return err
	}
	return json.Unmarshal(raw[1], &c.Session)
}

type scheduleStep struct {
	AtMs            int64    `json:"atMs"`
	Action          string   `json:"action"`
	SessionID       string   `json:"sessionId"`
	Event           string   `json:"event"`
	AgentID         string   `json:"agentId"`
	IntervalMinutes *float64 `json:"intervalMinutes"`
	Enabled         *bool    `json:"enabled"`
}

type scheduleMemory struct {
	IntervalMinutes *float64 `json:"intervalMinutes"`
	Enabled         *bool    `json:"enabled"`
}

// config is the memory config of the scenario, the Go form of the recorder's
// memoryConfig(intervalMinutes, enabled).
func (m scheduleMemory) config() memory.Config {
	enabled := true
	if m.Enabled != nil {
		enabled = *m.Enabled
	}
	return memory.Config{Enabled: enabled, UpdateUser: true, Consolidation: &memory.ConsolidationTrigger{IntervalMinutes: m.IntervalMinutes}}
}

type scheduleScenario struct {
	Name         string          `json:"name"`
	AgentID      string          `json:"agentId"`
	ClockMs      int64           `json:"clockMs"`
	Memory       scheduleMemory  `json:"memory"`
	Sessions     []string        `json:"sessions"`
	Replies      []reply         `json:"replies"`
	Steps        []scheduleStep  `json:"steps"`
	EndMs        int64           `json:"endMs"`
	Calls        []scheduleCall  `json:"calls"`
	ChatInputs   []string        `json:"chatInputs"`
	ModelCalls   int             `json:"modelCalls"`
	Records      []memory.Record `json:"records"`
	Checkpoints  map[string]*int `json:"checkpoints"`
	DisposedAtMs *int64          `json:"disposedAtMs"`
}

func TestScheduleMatchesGolden(t *testing.T) {
	var scenarios []scheduleScenario
	loadFixture(t, "memorysystem-schedule", &scenarios)
	if len(scenarios) < 7 {
		t.Fatalf("only %d scenarios in the fixture, want at least 7", len(scenarios))
	}
	for _, sc := range scenarios {
		t.Run(sc.Name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) { replaySchedule(t, sc) })
		})
	}
}

func replaySchedule(t *testing.T, sc scheduleScenario) {
	start := time.Now()
	elapsed := func() time.Duration { return time.Since(start) }
	// The bubble's clock starts at a fixed instant of its own; the recorded scenario
	// started at ClockMs, so the pipeline clock is ClockMs plus the virtual time that
	// has passed.
	clock := func() time.Time { return time.UnixMilli(sc.ClockMs).Add(elapsed()) }
	var idCounter int
	ids := func() string {
		idCounter++
		return fmt.Sprintf("mem_%d_%d", clock().UnixMilli(), idCounter)
	}

	w := newWorld(t)
	model := newFakeModel(sc.Replies...)
	var (
		holdNext atomic.Bool
		held     atomic.Pointer[gate]
	)
	model.hold = func(ctx context.Context, msgs []kernel.Message) error {
		if isExtractionCall(msgs) && holdNext.CompareAndSwap(true, false) {
			return held.Load().hold(ctx, msgs)
		}
		return nil
	}
	sys := w.system(sc.AgentID, sc.Memory.config(), model, WithClock(clock), WithIDs(ids), WithLogger(slog.New(slog.DiscardHandler)))

	var (
		callsMu sync.Mutex
		calls   []scheduleCall
	)
	inner := sys.sched.consolidate
	sys.sched.consolidate = func(ctx context.Context, sessionID string) ([]memory.ConsolidationResult, error) {
		callsMu.Lock()
		calls = append(calls, scheduleCall{Ms: elapsed().Milliseconds(), Session: sessionID})
		callsMu.Unlock()
		return inner(ctx, sessionID)
	}

	for _, sid := range sc.Sessions {
		err := w.hub.Sessions.Update(t.Context(), kernel.Session{ID: sid, Metadata: ordered.Object{}, CreatedAt: 1000, UpdatedAt: 2000})
		if err != nil {
			t.Fatal(err)
		}
	}

	counter := 0
	appendMessages := func(sessionID string) {
		for _, role := range []kernel.Role{kernel.RoleUser, kernel.RoleAssistant} {
			counter++
			if err := w.hub.Messages.Append(t.Context(), sessionID, kernel.Message{Role: role, Content: fmt.Sprintf("message %d", counter)}); err != nil {
				t.Fatal(err)
			}
		}
	}

	sys.Start(t.Context())
	disposed := make(chan int64, 1)
	disposing := false
	sleepUntil := func(atMs int64) {
		time.Sleep(time.Duration(atMs)*time.Millisecond - elapsed())
		synctest.Wait()
	}
	for _, step := range sc.Steps {
		sleepUntil(step.AtMs)
		switch step.Action {
		case "turn", "append":
			appendMessages(step.SessionID)
			if step.Action != "turn" {
				break
			}
			agent := sc.AgentID
			if step.AgentID != "" {
				agent = step.AgentID
			}
			switch step.Event {
			case "max-iterations":
				events.Emit(w.bus, events.MaxIterationsReached, step.SessionID, events.MaxIterationsPayload{AgentID: agent, Iterations: 1})
			case "aborted":
				events.Emit(w.bus, events.RunCompleted, step.SessionID, events.RunCompletedPayload{AgentID: agent, Iterations: 1, Aborted: true})
			default:
				events.Emit(w.bus, events.RunCompleted, step.SessionID, events.RunCompletedPayload{AgentID: agent, Iterations: 1})
			}
		case "updateConfig":
			m := sc.Memory
			if step.IntervalMinutes != nil {
				m.IntervalMinutes = step.IntervalMinutes
			}
			if step.Enabled != nil {
				m.Enabled = step.Enabled
			}
			sys.UpdateConfig(m.config())
		case "hold":
			held.Store(newGate())
			holdNext.Store(true)
		case "release":
			held.Load().release()
		case "dispose":
			disposing = true
			go func() {
				if err := sys.Dispose(context.Background()); err != nil {
					t.Errorf("Dispose: %v", err)
				}
				disposed <- elapsed().Milliseconds()
			}()
		default:
			t.Fatalf("unknown step action %q", step.Action)
		}
		synctest.Wait()
	}
	sleepUntil(sc.EndMs)
	var disposedAt *int64
	if disposing {
		ms := <-disposed
		disposedAt = &ms
	} else {
		dispose(t, sys)
	}

	// --- compare with the recorded transcript -----------------------------------
	callsMu.Lock()
	gotCalls := append([]scheduleCall{}, calls...)
	callsMu.Unlock()
	if fmt.Sprint(gotCalls) != fmt.Sprint(append([]scheduleCall{}, sc.Calls...)) {
		t.Errorf("consolidate calls differ\n got: %v\nwant: %v", gotCalls, sc.Calls)
	}

	var chatInputs []string
	for _, c := range model.extractionCalls() {
		chatInputs = append(chatInputs, c[1].Content)
	}
	if fmt.Sprint(chatInputs) != fmt.Sprint(sc.ChatInputs) {
		t.Errorf("extraction inputs differ\n got: %q\nwant: %q", chatInputs, sc.ChatInputs)
	}
	if n := len(model.extractionCalls()); n != sc.ModelCalls {
		t.Errorf("%d extraction calls, want %d", n, sc.ModelCalls)
	}

	records, err := w.hub.Memory.Query(t.Context(), persistence.MemoryFilter{})
	if err != nil {
		t.Fatal(err)
	}
	// supersededBy and sourceTrust differ from the recording: see goStoreRecords (the
	// scenarios seed no record).
	if got, want := encode(t, append([]memory.Record{}, records...)), encode(t, goStoreRecords(t, sc.Records, nil)); got != want {
		t.Errorf("records differ\n got: %s\nwant: %s", got, want)
	}

	for _, sid := range sc.Sessions {
		want := sc.Checkpoints[sid]
		s, err := w.hub.Sessions.Get(t.Context(), sid)
		if err != nil || s == nil {
			t.Fatalf("session %s: %v %v", sid, s, err)
		}
		switch {
		case want == nil && s.ConsolidatedMessageCount != nil:
			t.Errorf("session %s: checkpoint %d, want none", sid, *s.ConsolidatedMessageCount)
		case want != nil && (s.ConsolidatedMessageCount == nil || *s.ConsolidatedMessageCount != *want):
			t.Errorf("session %s: checkpoint %v, want %d", sid, s.ConsolidatedMessageCount, *want)
		}
	}

	switch {
	case (disposedAt == nil) != (sc.DisposedAtMs == nil):
		t.Errorf("Dispose resolved at %v, want %v", disposedAt, sc.DisposedAtMs)
	case disposedAt != nil && *disposedAt != *sc.DisposedAtMs:
		t.Errorf("Dispose resolved at %d ms, want %d ms", *disposedAt, *sc.DisposedAtMs)
	}
}

// ---------------------------------------------------------------------------
// The recorded recallForTurn results
//
// memorysystem-recall-for-turn.json holds, per scenario, the RecalledMemories of each
// call (content text, record ids, traces with the four scores, maxResults, skipped,
// indexRebuilt, indexRebuildMs and indexedCount) and the store afterwards (the
// lastRecalledAt stamps). performance.now was pinned to the case clock and every query
// of the memory store advanced it by queryDelayMs, so indexRebuildMs is a known number.
// ---------------------------------------------------------------------------

type recallForTurnCall struct {
	UserMessage string `json:"userMessage"`
	AdvanceMs   int64  `json:"advanceMs"`
}

type recallForTurnScenario struct {
	Name         string              `json:"name"`
	AgentID      string              `json:"agentId"`
	ClockMs      int64               `json:"clockMs"`
	Memory       memory.Config       `json:"memory"`
	Seed         []memory.Record     `json:"seed"`
	QueryDelayMs int64               `json:"queryDelayMs"`
	Calls        []recallForTurnCall `json:"calls"`
	Result       struct {
		Results []memory.RecalledMemories `json:"results"`
		Store   []memory.Record           `json:"store"`
	} `json:"result"`
}

// queryHook wraps a MemoryStore and runs hook on every Query: the fixture recorder
// advanced its clock there, which is what makes indexRebuildMs a known number.
type queryHook struct {
	persistence.MemoryStore
	hook func()
}

func (q *queryHook) Query(ctx context.Context, f persistence.MemoryFilter) ([]memory.Record, error) {
	q.hook()
	return q.MemoryStore.Query(ctx, f)
}

func TestRecallForTurnMatchesGolden(t *testing.T) {
	var scenarios []recallForTurnScenario
	loadFixture(t, "memorysystem-recall-for-turn", &scenarios)
	if len(scenarios) < 8 {
		t.Fatalf("only %d scenarios in the fixture, want at least 8", len(scenarios))
	}
	for _, sc := range scenarios {
		t.Run(sc.Name, func(t *testing.T) {
			var nowMs atomic.Int64
			nowMs.Store(sc.ClockMs)
			clock := func() time.Time { return time.UnixMilli(nowMs.Load()) }

			store := seedStore(t, sc.Seed...)
			delayed := &queryHook{MemoryStore: store, hook: func() { nowMs.Add(sc.QueryDelayMs) }}
			sys, err := New(Deps{
				AgentID:     sc.AgentID,
				Persistence: persistence.NewPersistenceHub(persistence.WithMemory(delayed)),
				Events:      events.New(),
				Model:       newFakeModel(),
			}, sc.Memory, WithClock(clock), WithLogger(slog.New(slog.DiscardHandler)))
			if err != nil {
				t.Fatal(err)
			}

			if len(sc.Result.Results) != len(sc.Calls) {
				t.Fatalf("fixture has %d results for %d calls", len(sc.Result.Results), len(sc.Calls))
			}
			for i, call := range sc.Calls {
				nowMs.Add(call.AdvanceMs)
				got, err := sys.RecallForTurn(t.Context(), call.UserMessage, "session-1")
				if err != nil {
					t.Fatalf("call %d: %v", i, err)
				}
				if g, want := encode(t, got), encode(t, sc.Result.Results[i]); g != want {
					t.Errorf("call %d (%q) differs\n got: %s\nwant: %s", i, call.UserMessage, g, want)
				}
			}
			if g, want := encode(t, storeRecords(t, store)), encode(t, sc.Result.Store); g != want {
				t.Errorf("store after the calls differs\n got: %s\nwant: %s", g, want)
			}
		})
	}
}
