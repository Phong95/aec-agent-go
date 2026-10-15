package memorysystem

import (
	"context"
	"encoding/json"
	"math"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/Phong95/aec-agent-go/components/agent/memorysystem/memory"
	"github.com/Phong95/aec-agent-go/components/events"
	"github.com/Phong95/aec-agent-go/components/persistence"
	"github.com/Phong95/aec-agent-go/kernel"
)

// TestSystem_ConsolidatesOneTurnOnTick is the tracer of the scheduler: a turn that
// finished is consolidated exactly at the interval, on virtual time, by a real actor
// goroutine that the real dispatcher feeds, and Dispose leaves nothing behind.
func TestSystem_ConsolidatesOneTurnOnTick(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w := newWorld(t)
		model := newFakeModel(extractionOf(t, "English"))
		sys := w.system("a1", memCfg(2), model)
		sys.Start(t.Context())

		s1 := w.session()
		w.chat("a1", s1, "hello")
		synctest.Wait()

		// One millisecond short of the interval: no pass.
		advance(2*minute - time.Millisecond)
		if n := len(model.extractionCalls()); n != 0 {
			t.Fatalf("%d extraction calls before the tick, want 0", n)
		}

		// The tick: one pass, one record stamped with the agent, checkpoint 2.
		advance(time.Millisecond)
		if n := len(model.extractionCalls()); n != 1 {
			t.Fatalf("%d extraction calls at the tick, want 1", n)
		}
		recs := w.records("a1")
		if len(recs) != 1 || recs[0].AgentID != "a1" || recs[0].Key != sharedKey || recs[0].Value != "English" {
			t.Fatalf("records = %+v, want one response_language record stamped a1", recs)
		}
		if got := w.checkpoint(s1); got != 2 {
			t.Fatalf("checkpoint = %d, want 2", got)
		}

		if err := sys.Dispose(t.Context()); err != nil {
			t.Fatalf("Dispose = %v, want nil", err)
		}
	})
}

// TestIntervalOf is the interval table: the interval is exact, disabled for 0, NaN,
// infinity, negatives, a missing trigger or disabled memory, otherwise clamped to [1
// minute, 2^31-1 ms].
func TestIntervalOf(t *testing.T) {
	f := func(v float64) *float64 { return &v }
	enabled := func(m *float64) memory.Config {
		return memory.Config{Enabled: true, Consolidation: &memory.ConsolidationTrigger{IntervalMinutes: m}}
	}
	tests := []struct {
		name string
		cfg  memory.Config
		want time.Duration
	}{
		{"2 minutes", enabled(f(2)), 2 * time.Minute},
		{"1 minute", enabled(f(1)), time.Minute},
		{"sub-minute clamps to 60 s", enabled(f(0.01)), time.Minute},
		{"just under a minute clamps", enabled(f(0.99999)), time.Minute},
		{"overflow-sized caps at 2^31-1 ms", enabled(f(1e9)), 2147483647 * time.Millisecond},
		{"the cap boundary", enabled(f(2147483647.0 / 60000)), 2147483647 * time.Millisecond},
		{"fractional minutes keep whole milliseconds", enabled(f(1.5)), 90 * time.Second},
		{"zero is disabled", enabled(f(0)), 0},
		{"negative is disabled", enabled(f(-1)), 0},
		{"NaN is disabled", enabled(f(math.NaN())), 0},
		{"+Inf is disabled", enabled(f(math.Inf(1))), 0},
		{"-Inf is disabled", enabled(f(math.Inf(-1))), 0},
		{"nil interval is disabled", enabled(nil), 0},
		{"nil trigger is disabled", memory.Config{Enabled: true}, 0},
		{"memory disabled is disabled", memory.Config{Enabled: false, Consolidation: &memory.ConsolidationTrigger{IntervalMinutes: f(5)}}, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := IntervalOf(tc.cfg); got != tc.want {
				t.Errorf("IntervalOf = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestNew_ValidatesItsDeps: every missing dependency is reported by name.
func TestNew_ValidatesItsDeps(t *testing.T) {
	good := Deps{AgentID: "a1", Persistence: persistence.NewPersistenceHub(), Events: events.New(), Model: newFakeModel()}
	for name, mutate := range map[string]func(*Deps){
		"AgentID":     func(d *Deps) { d.AgentID = "" },
		"Persistence": func(d *Deps) { d.Persistence = nil },
		"Events":      func(d *Deps) { d.Events = nil },
		"Model":       func(d *Deps) { d.Model = nil },
	} {
		t.Run(name, func(t *testing.T) {
			d := good
			mutate(&d)
			sys, err := New(d, memory.Config{})
			if err == nil || sys != nil {
				t.Fatalf("New = %v, %v, want an error", sys, err)
			}
			if !strings.Contains(err.Error(), name) {
				t.Errorf("error %q does not name %s", err, name)
			}
		})
	}
	if _, err := New(good, memory.Config{}); err != nil {
		t.Fatalf("New with every dependency: %v", err)
	}
}

// TestNew_DoesNotAliasTheConfig: the System deep-clones the config it is given, so a
// caller that edits its own interval later changes nothing.
func TestNew_DoesNotAliasTheConfig(t *testing.T) {
	cfg := memCfg(5)
	sys, err := New(Deps{AgentID: "a1", Persistence: persistence.NewPersistenceHub(), Events: events.New(), Model: newFakeModel()}, cfg)
	if err != nil {
		t.Fatal(err)
	}
	*cfg.Consolidation.IntervalMinutes = 99
	if got := IntervalOf(sys.config()); got != 5*time.Minute {
		t.Fatalf("interval = %v after the caller edited its config, want 5m", got)
	}
}

// TestSystem_IndexRebuildMsRounds: indexRebuildMs is Math.round(elapsed ms), so a half
// rounds up. The recorded fixtures only use whole-millisecond delays, so the rounding
// of a fraction is pinned here.
func TestSystem_IndexRebuildMsRounds(t *testing.T) {
	for _, tc := range []struct {
		elapsed time.Duration
		want    float64
	}{
		{0, 0},
		{400 * time.Microsecond, 0},
		{500 * time.Microsecond, 1},
		{1400 * time.Microsecond, 1},
		{1500 * time.Microsecond, 2},
		{2500 * time.Microsecond, 3},
		{40 * time.Millisecond, 40},
	} {
		t.Run(tc.elapsed.String(), func(t *testing.T) {
			base := time.UnixMilli(1700000000000)
			calls := 0
			clock := func() time.Time {
				calls++
				if calls == 1 {
					return base
				}
				return base.Add(tc.elapsed)
			}
			sys, err := New(Deps{AgentID: "a1", Persistence: persistence.NewPersistenceHub(), Events: events.New(), Model: newFakeModel()}, memory.Config{Enabled: true}, WithClock(clock))
			if err != nil {
				t.Fatal(err)
			}
			out, err := sys.RecallForTurn(t.Context(), "hello", "s1")
			if err != nil {
				t.Fatal(err)
			}
			if !out.IndexRebuilt || out.IndexRebuildMs == nil || *out.IndexRebuildMs != tc.want {
				t.Fatalf("IndexRebuildMs = %v (rebuilt %v), want %v", out.IndexRebuildMs, out.IndexRebuilt, tc.want)
			}
		})
	}
}

// TestSystem_EnsureIndexIsSingleFlight: of many concurrent first recalls exactly one
// builds the index and reports indexRebuilt (RESEARCH Pitfall 3).
func TestSystem_EnsureIndexIsSingleFlight(t *testing.T) {
	sys, err := New(Deps{AgentID: "a1", Persistence: persistence.NewPersistenceHub(), Events: events.New(), Model: newFakeModel()}, memory.Config{Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	var (
		wg      sync.WaitGroup
		rebuilt atomic.Int32
	)
	for range 16 {
		wg.Go(func() {
			out, err := sys.RecallForTurn(t.Context(), "hello world", "s1")
			if err != nil {
				t.Error(err)
				return
			}
			if out.IndexRebuilt {
				rebuilt.Add(1)
			}
		})
	}
	wg.Wait()
	if n := rebuilt.Load(); n != 1 {
		t.Fatalf("%d callers reported indexRebuilt, want exactly 1", n)
	}
}

// Regression tests of the phase 10 code review warning WR-B04 (and the info items
// IN-B02 and IN-B01's memory side).

// --- WR-B04: a NaN score does not break the recall trace ----------------------------------

func TestRecallForTurn_UnparsableTimestampKeepsTheTraceEncodable(t *testing.T) {
	hub := persistence.NewPersistenceHub()
	bad := memory.Record{
		ID: "bad-1", AgentID: "a1", Kind: memory.KindFact, Scope: memory.ScopeUser, Subject: "user",
		Key: "response_language", Value: "Reply in English language", Status: memory.StatusActive,
		Confidence: memory.ConfidenceExplicit, Source: "conversation", SourceTrust: memory.TrustUserApproved,
		ObservedAt: "garbage", UpdatedAt: "garbage",
	}
	if err := hub.Memory.Save(t.Context(), bad); err != nil {
		t.Fatal(err)
	}
	sys, err := New(Deps{AgentID: "a1", Persistence: hub, Events: events.New(), Model: newFakeModel()}, memory.Config{Enabled: true}, WithLocation(time.UTC))
	if err != nil {
		t.Fatal(err)
	}

	// The raw recall result keeps the NaN (as JavaScript would); the trace does not.
	raw, err := sys.Recall(t.Context(), memory.RetrievalQuery{AgentID: "a1", CurrentContext: "language"})
	if err != nil || len(raw) != 1 || !math.IsNaN(raw[0].RecencyScore) {
		t.Fatalf("Recall = %+v, %v, want one result with a NaN recency score", raw, err)
	}

	out, err := sys.RecallForTurn(t.Context(), "language", "s1")
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Memories) != 1 {
		t.Fatalf("trace = %+v, want the bad record recalled", out.Memories)
	}
	m := out.Memories[0]
	for name, v := range map[string]float64{"combined": m.CombinedScore, "relevance": m.RelevanceScore, "recency": m.RecencyScore, "importance": m.ImportanceScore} {
		if math.IsNaN(v) || math.IsInf(v, 0) {
			t.Errorf("%s score = %v, want a finite number", name, v)
		}
	}
	if _, err := kernel.EncodeJSON(out.Memories); err != nil {
		t.Fatalf("the trace does not encode: %v", err)
	}
	if _, err := json.Marshal(out.Memories); err != nil {
		t.Fatalf("the trace does not marshal: %v", err)
	}
}

func TestFiniteOrZero(t *testing.T) {
	for _, v := range []float64{math.NaN(), math.Inf(1), math.Inf(-1)} {
		if got := finiteOrZero(v); got != 0 {
			t.Errorf("finiteOrZero(%v) = %v, want 0", v, got)
		}
	}
	for _, v := range []float64{0, 1.5, -2, 1e300} {
		if got := finiteOrZero(v); got != v {
			t.Errorf("finiteOrZero(%v) = %v, want it unchanged", v, got)
		}
	}
}

// --- IN-B02: a nil lifetime context does not make every pass panic -----------------------

func TestSystem_StartWithNilLifetime(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w := newWorld(t)
		lc := &logCapture{}
		sys := w.system("a1", memCfg(1), newFakeModel(), WithLogger(lc.logger()))
		spy := spyPasses(sys, nil)
		var lifetime context.Context // nil on purpose
		sys.Start(lifetime)

		s1 := w.session()
		w.chat("a1", s1, "hello")
		advance(minute)
		if n := spy.count(); n != 1 {
			t.Fatalf("%d passes, want 1", n)
		}
		if strings.Contains(lc.String(), "panic") {
			t.Fatalf("a pass panicked: %q", lc.String())
		}
		dispose(t, sys)
	})
}
