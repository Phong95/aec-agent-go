package memorysystem

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/Phong95/aec-agent-go/components/agent/memorysystem/memory"
	"github.com/Phong95/aec-agent-go/components/persistence"
	"github.com/Phong95/aec-agent-go/kernel"
)

// ---------------------------------------------------------------------------
// The consolidation scheduler on virtual time. Every test is one testing/synctest
// bubble: the actor goroutine, the real dispatcher and the in-memory stores run inside
// it, time.Sleep moves the fake clock and synctest.Wait lets every goroutine settle.
// Every test ends with Dispose, because a bubble that returns with the actor still
// alive panics.
//
// These tests drive the System over a real dispatcher and replace the pass through
// spyPasses (see helpers_test.go). The Agent-level end to end run is plan 10-05.
// ---------------------------------------------------------------------------

// --- interval trigger --------------------------------------------------------

// Case "does not consolidate during a turn - only on the interval tick".
func TestScheduler_NoPassDuringTurn_OnlyOnTick(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w := newWorld(t)
		sys := w.system("a1", memCfg(1), newFakeModel())
		spy := spyPasses(sys, nil)
		sys.Start(t.Context())

		s1 := w.session()
		w.chat("a1", s1, "hello")
		synctest.Wait()
		if n := spy.count(); n != 0 {
			t.Fatalf("%d passes during the turn, want 0", n)
		}

		advance(minute)
		if n := spy.count(); n != 1 {
			t.Fatalf("%d passes after the tick, want 1", n)
		}
		dispose(t, sys)
	})
}

// Case "does not re-run on later ticks when there are no new messages".
func TestScheduler_NoRerunWithoutNewMessages(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w := newWorld(t)
		sys := w.system("a1", memCfg(1), newFakeModel())
		spy := spyPasses(sys, nil)
		sys.Start(t.Context())

		s1 := w.session()
		w.chat("a1", s1, "hello")
		advance(minute)
		advance(minute)
		advance(minute)
		if n := spy.count(); n != 1 {
			t.Fatalf("%d passes after three ticks, want 1", n)
		}

		w.chat("a1", s1, "more")
		advance(minute)
		if n := spy.count(); n != 2 {
			t.Fatalf("%d passes after new messages, want 2", n)
		}
		dispose(t, sys)
	})
}

// Case "consolidates every session with new messages, not just the active one". The
// sessions are consolidated in the order in which they became dirty.
func TestScheduler_AllDirtySessions(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w := newWorld(t)
		sys := w.system("a1", memCfg(1), newFakeModel())
		spy := spyPasses(sys, nil)
		sys.Start(t.Context())

		s1 := w.session()
		w.chat("a1", s1, "in session 1")
		s2 := w.session()
		w.chat("a1", s2, "in session 2")
		s3 := w.session() // active, but no messages
		synctest.Wait()

		advance(minute)
		got := spy.calls()
		if len(got) != 2 || got[0] != s1 || got[1] != s2 {
			t.Fatalf("passes for %v, want [%s %s] and not %s", got, s1, s2, s3)
		}
		dispose(t, sys)
	})
}

// Case "does nothing when no turn has run".
func TestScheduler_NothingWithoutTurn(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w := newWorld(t)
		sys := w.system("a1", memCfg(1), newFakeModel())
		spy := spyPasses(sys, nil)
		sys.Start(t.Context())

		w.session()
		advance(5 * minute)
		if n := spy.count(); n != 0 {
			t.Fatalf("%d passes with no turn, want 0", n)
		}
		dispose(t, sys)
	})
}

// Case "ignores turns from other agents sharing the same EventBus".
func TestScheduler_IgnoresOtherAgents(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w := newWorld(t)
		sys := w.system("a1", memCfg(1), newFakeModel())
		spy := spyPasses(sys, nil)
		sys.Start(t.Context())
		other := w.system("other", memCfg(0), newFakeModel())
		other.Start(t.Context())

		so := w.session()
		w.chat("other", so, "not mine")
		advance(minute)
		if n := spy.count(); n != 0 {
			t.Fatalf("%d passes for another agent's turn, want 0", n)
		}
		dispose(t, other)
		dispose(t, sys)
	})
}

// Both turn-end events count, the aborted run and the max-iterations one included:
// RunCompleted carries Aborted, MaxIterationsReached has its own payload.
func TestScheduler_BothTurnEndEventsMarkDirty(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w := newWorld(t)
		sys := w.system("a1", memCfg(1), newFakeModel())
		spy := spyPasses(sys, nil)
		sys.Start(t.Context())

		s1, s2 := w.session(), w.session()
		w.say(s1, "run")
		w.say(s2, "run")
		emitMaxIterations(w, "a1", s1)
		emitAborted(w, "a1", s2)
		advance(minute)
		if got := spy.calls(); len(got) != 2 || got[0] != s1 || got[1] != s2 {
			t.Fatalf("passes for %v, want [%s %s]", got, s1, s2)
		}
		dispose(t, sys)
	})
}

// --- config ------------------------------------------------------------------

// Cases "never consolidates when %s" (enabled=false, intervalMinutes=0,
// intervalMinutes undefined).
func TestScheduler_Disabled(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  memory.Config
	}{
		{"enabled=false", memCfgOff(1)},
		{"intervalMinutes=0", memCfg(0)},
		{"intervalMinutes undefined", memCfgNoInterval()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				w := newWorld(t)
				sys := w.system("a1", tc.cfg, newFakeModel())
				spy := spyPasses(sys, nil)
				sys.Start(t.Context())

				s1 := w.session()
				w.chat("a1", s1, "hello")
				advance(10 * minute)
				if n := spy.count(); n != 0 {
					t.Fatalf("%d passes while disabled, want 0", n)
				}
				dispose(t, sys)
			})
		})
	}
}

// Case "re-arms on updateMemoryConfig and disarms on 0". The turn finished while the
// interval was 0 and is still consolidated after the re-arm: dirty tracking keeps
// running while the scheduler is disarmed.
func TestScheduler_RearmAndDisarm(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w := newWorld(t)
		sys := w.system("a1", memCfg(0), newFakeModel())
		spy := spyPasses(sys, nil)
		sys.Start(t.Context())

		s1 := w.session()
		w.chat("a1", s1, "hello")
		advance(5 * minute)
		if n := spy.count(); n != 0 {
			t.Fatalf("%d passes while the interval is 0, want 0", n)
		}

		sys.UpdateConfig(memCfg(2))
		synctest.Wait()
		advance(minute)
		if n := spy.count(); n != 0 {
			t.Fatalf("%d passes after one of two minutes, want 0", n)
		}
		advance(minute)
		if n := spy.count(); n != 1 {
			t.Fatalf("%d passes after the re-arm interval, want 1", n)
		}

		sys.UpdateConfig(memCfg(0))
		synctest.Wait()
		w.chat("a1", s1, "more")
		advance(10 * minute)
		if n := spy.count(); n != 1 {
			t.Fatalf("%d passes after disarming, want 1", n)
		}
		if sys.sched.isRunning() {
			t.Fatal("the actor goroutine is still alive after the interval became 0")
		}
		dispose(t, sys)
	})
}

// Case "clamps sub-minute intervals up to 60s and caps overflow-sized intervals". A
// 1e9 minute interval does not storm, and the cap is exact: the first pass is due at
// 2147483647 ms after the re-arm, to the millisecond.
func TestScheduler_ClampAndCap(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w := newWorld(t)
		sys := w.system("a1", memCfg(0.01), newFakeModel())
		spy := spyPasses(sys, nil)
		sys.Start(t.Context())

		s1 := w.session()
		w.chat("a1", s1, "hello")
		advance(30 * time.Second)
		if n := spy.count(); n != 0 {
			t.Fatalf("%d passes after 30 s, want 0 (clamped to 60 s)", n)
		}
		advance(30 * time.Second)
		if n := spy.count(); n != 1 {
			t.Fatalf("%d passes after 60 s, want 1", n)
		}

		// 1e9 minutes would overflow setInterval (it would fire every millisecond): it
		// is capped, not a storm.
		sys.UpdateConfig(memCfg(1e9))
		synctest.Wait()
		w.chat("a1", s1, "more")
		advance(10 * minute)
		if n := spy.count(); n != 1 {
			t.Fatalf("%d passes after 10 minutes of a capped interval, want 1", n)
		}
		const capInterval = 2147483647 * time.Millisecond
		advance(capInterval - 10*minute - time.Millisecond)
		if n := spy.count(); n != 1 {
			t.Fatalf("%d passes one millisecond before the cap, want 1", n)
		}
		advance(time.Millisecond)
		if n := spy.count(); n != 2 {
			t.Fatalf("%d passes at the cap, want 2", n)
		}
		dispose(t, sys)
	})
}

// --- queue -------------------------------------------------------------------

// Case "runs at most one pass at a time across sessions". The first pass is held open
// across a second tick, which must not start a parallel pass; once released both
// sessions are consolidated, one after the other.
func TestScheduler_OnePassAtATime(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w := newWorld(t)
		sys := w.system("a1", memCfg(1), newFakeModel())
		open := make(chan struct{})
		spy := spyPasses(sys, func(ctx context.Context, sessionID string, inner passFn) ([]memory.ConsolidationResult, error) {
			select {
			case <-open:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
			return inner(ctx, sessionID)
		})
		sys.Start(t.Context())

		s1 := w.session()
		w.chat("a1", s1, "session 1")
		s2 := w.session()
		w.chat("a1", s2, "session 2")

		advance(minute)
		// A second tick while the first pass is blocked must not start a parallel pass.
		advance(minute)
		if p := spy.peakActive(); p != 1 {
			t.Fatalf("peak = %d while blocked, want 1", p)
		}

		close(open)
		synctest.Wait()
		if p := spy.peakActive(); p != 1 {
			t.Fatalf("peak = %d after release, want 1", p)
		}
		if got := spy.calls(); len(got) != 2 || got[0] != s1 || got[1] != s2 {
			t.Fatalf("passes for %v, want [%s %s]", got, s1, s2)
		}
		dispose(t, sys)
	})
}

// Case "logs a failed pass without an unhandled rejection and retries it on the next
// tick". The first pass fails and, like any failed pass, does not advance the
// checkpoint, so the session is still pending on the next tick.
func TestScheduler_FailedPassRetried(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w := newWorld(t)
		logs := &logCapture{}
		sys := w.system("a1", memCfg(1), newFakeModel(), WithLogger(logs.logger()))
		var spy *passSpy
		spy = spyPasses(sys, func(_ context.Context, _ string, _ passFn) ([]memory.ConsolidationResult, error) {
			if spy.count() == 1 {
				return nil, errors.New("boom")
			}
			return []memory.ConsolidationResult{}, nil
		})
		sys.Start(t.Context())

		s1 := w.session()
		w.chat("a1", s1, "hello")
		advance(minute)
		out := logs.String()
		if !strings.Contains(out, "consolidation failed") || !strings.Contains(out, "boom") || !strings.Contains(out, "agent=a1") || !strings.Contains(out, "session="+s1) {
			t.Fatalf("the failure is not logged with the agent and session: %s", out)
		}

		advance(minute)
		if n := spy.count(); n != 2 {
			t.Fatalf("%d passes, want 2: the failed session is retried on the next tick", n)
		}
		dispose(t, sys)
	})
}

// --- dispose -----------------------------------------------------------------

// Case "clears the timer and never starts a new pass".
func TestScheduler_StopNoNewPass(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w := newWorld(t)
		sys := w.system("a1", memCfg(1), newFakeModel())
		spy := spyPasses(sys, nil)
		sys.Start(t.Context())

		s1 := w.session()
		w.chat("a1", s1, "hello")
		dispose(t, sys)

		advance(5 * minute)
		if n := spy.count(); n != 0 {
			t.Fatalf("%d passes after Dispose, want 0", n)
		}
		// A turn after Dispose is not even remembered.
		w.chat("a1", s1, "late")
		advance(5 * minute)
		if n := spy.count(); n != 0 {
			t.Fatalf("%d passes for a turn after Dispose, want 0", n)
		}
	})
}

// Case "waits for an in-flight pass to finish before resolving". The queued second
// session never starts: no final pass.
func TestScheduler_StopWaitsForPass(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w := newWorld(t)
		sys := w.system("a1", memCfg(1), newFakeModel())
		open := make(chan struct{})
		spy := spyPasses(sys, func(ctx context.Context, _ string, _ passFn) ([]memory.ConsolidationResult, error) {
			select {
			case <-open:
			case <-ctx.Done():
			}
			return []memory.ConsolidationResult{}, nil
		})
		sys.Start(t.Context())

		s1 := w.session()
		w.chat("a1", s1, "hello")
		s2 := w.session()
		w.chat("a1", s2, "hello again")
		advance(minute) // the first pass starts and blocks; the second session is queued

		disposed := make(chan error, 1)
		go func() { disposed <- sys.Dispose(context.Background()) }()
		synctest.Wait()
		select {
		case err := <-disposed:
			t.Fatalf("Dispose returned (%v) while a pass was in flight", err)
		default:
		}

		close(open)
		synctest.Wait()
		select {
		case err := <-disposed:
			if err != nil {
				t.Fatalf("Dispose = %v, want nil", err)
			}
		default:
			t.Fatal("Dispose did not return after the pass finished")
		}
		if n := spy.count(); n != 1 {
			t.Fatalf("%d passes, want 1: Dispose must not start the queued session", n)
		}
	})
}

// --- proofs of the actor -----------------------------------------------------

// Stop(ctx) with an expiring ctx returns the ctx error after exactly 5 s of fake time
// while a pass is held open, and the pass is not abandoned: a second Dispose waits for
// it and returns nil.
func TestScheduler_StopContextExpiry(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w := newWorld(t)
		sys := w.system("a1", memCfg(1), newFakeModel())
		open := make(chan struct{})
		spyPasses(sys, func(ctx context.Context, _ string, _ passFn) ([]memory.ConsolidationResult, error) {
			select {
			case <-open:
			case <-ctx.Done():
			}
			return []memory.ConsolidationResult{}, nil
		})
		sys.Start(t.Context())

		s1 := w.session()
		w.chat("a1", s1, "hello")
		advance(minute) // the pass is held

		start := time.Now()
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		defer cancel()
		err := sys.Dispose(ctx)
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("Dispose = %v, want context.DeadlineExceeded", err)
		}
		if got := time.Since(start); got != 5*time.Second {
			t.Fatalf("Dispose returned after %v of fake time, want exactly 5s", got)
		}

		close(open)
		synctest.Wait()
		if err := sys.Dispose(t.Context()); err != nil {
			t.Fatalf("second Dispose = %v, want nil once the pass finished", err)
		}
	})
}

// An actor whose interval became 0 really exits (the bubble would panic with a deadlock
// report if a goroutine stayed blocked), and a later re-arm starts a new one that fires
// at the new interval.
func TestScheduler_DisarmExitsGoroutine(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w := newWorld(t)
		sys := w.system("a1", memCfg(1), newFakeModel())
		spy := spyPasses(sys, nil)
		sys.Start(t.Context())
		synctest.Wait()
		if !sys.sched.isRunning() {
			t.Fatal("no actor goroutine while the interval is above 0")
		}

		sys.UpdateConfig(memCfg(0))
		synctest.Wait()
		if sys.sched.isRunning() {
			t.Fatal("the actor goroutine survived the interval becoming 0")
		}
		if sys.sched.doneClosed() != true {
			t.Fatal("the actor's done channel is not closed")
		}

		s1 := w.session()
		w.chat("a1", s1, "hello")
		sys.UpdateConfig(memCfg(3))
		synctest.Wait()
		if !sys.sched.isRunning() {
			t.Fatal("re-arming did not start a new actor")
		}
		advance(3*minute - time.Millisecond)
		if n := spy.count(); n != 0 {
			t.Fatalf("%d passes before the new interval, want 0", n)
		}
		advance(time.Millisecond)
		if n := spy.count(); n != 1 {
			t.Fatalf("%d passes at the new interval, want 1", n)
		}
		dispose(t, sys)
		if sys.sched.isRunning() {
			t.Fatal("the actor goroutine survived Dispose")
		}
	})
}

// Eight emitters, a rearm storm and Stop together are race-clean and never run two
// passes at once; once the emitters are done every session is consolidated.
func TestScheduler_ConcurrentEmitRearmStop(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w := newWorld(t)
		sys := w.system("a1", memCfg(1), newFakeModel())
		spy := spyPasses(sys, nil)
		sys.Start(t.Context())

		sessions := make([]string, 4)
		for i := range sessions {
			sessions[i] = w.session()
		}
		var wg sync.WaitGroup
		for i := range 8 {
			wg.Go(func() {
				for j := range 20 {
					sid := sessions[(i+j)%len(sessions)]
					for _, m := range []kernel.Message{{Role: kernel.RoleUser, Content: "x"}, {Role: kernel.RoleAssistant, Content: "ok"}} {
						if err := w.hub.Messages.Append(t.Context(), sid, m); err != nil {
							t.Errorf("append: %v", err)
							return
						}
					}
					w.finish("a1", sid)
					time.Sleep(50 * time.Second)
				}
			})
		}
		wg.Go(func() {
			for j := range 20 {
				if j%2 == 0 {
					sys.UpdateConfig(memCfg(1.5))
				} else {
					sys.UpdateConfig(memCfg(1))
				}
				time.Sleep(70 * time.Second)
			}
		})
		wg.Wait()

		advance(5 * minute)
		if p := spy.peakActive(); p > 1 {
			t.Fatalf("peak = %d, want at most 1 pass at a time", p)
		}
		if spy.count() == 0 {
			t.Fatal("no pass ran")
		}
		for _, sid := range sessions {
			total, err := w.hub.Messages.Count(t.Context(), sid)
			if err != nil {
				t.Fatal(err)
			}
			if got := w.checkpoint(sid); got != total {
				t.Fatalf("session %s: checkpoint %d of %d messages", sid, got, total)
			}
		}
		dispose(t, sys)
	})
}

// A panicking model (or store) is recovered, logged with the agent and session, and the
// session stays dirty: the next tick retries it and succeeds.
func TestScheduler_PanicInPassRecovered(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w := newWorld(t)
		logs := &logCapture{}
		model := newFakeModel()
		var panicked atomic.Bool
		model.hold = func(_ context.Context, msgs []kernel.Message) error {
			if isExtractionCall(msgs) && panicked.CompareAndSwap(false, true) {
				panic("kaboom")
			}
			return nil
		}
		sys := w.system("a1", memCfg(1), model, WithLogger(logs.logger()))
		sys.Start(t.Context())

		s1 := w.session()
		w.chat("a1", s1, "hello")
		advance(minute)
		out := logs.String()
		if !strings.Contains(out, "consolidation panicked") || !strings.Contains(out, "panic=string") || !strings.Contains(out, "agent=a1") {
			t.Fatalf("the panic is not logged: %s", out)
		}
		// The value of a recovered panic can carry text, so only its type is logged
		// (10-SECURITY UF-1, Phase 12 12-02).
		if strings.Contains(out, "kaboom") {
			t.Fatalf("the log shows the panic value: %s", out)
		}
		if w.checkpoint(s1) != 0 {
			t.Fatal("a panicking pass moved the checkpoint")
		}
		if !sys.sched.isDirty(s1) {
			t.Fatal("the session left the dirty set after a panicking pass")
		}

		advance(minute)
		if got := w.checkpoint(s1); got != 2 {
			t.Fatalf("checkpoint = %d after the retry, want 2", got)
		}
		dispose(t, sys)
	})
}

// A tick that arrives while a pass runs is dropped, because a time.Ticker keeps at most
// one pending tick. After a pass held for five intervals exactly one more tick is
// evaluated, not five.
func TestScheduler_TicksDuringPassAreDropped(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w := newWorld(t)
		sys := w.system("a1", memCfg(1), newFakeModel())
		var ticks atomic.Int32
		realCheckpoint := sys.sched.checkpoint
		sys.sched.checkpoint = func(ctx context.Context, sessionID string) (int, error) {
			ticks.Add(1) // the scheduler asks once per dirty session of every tick it evaluates
			return realCheckpoint(ctx, sessionID)
		}
		open := make(chan struct{})
		spyPasses(sys, func(ctx context.Context, sessionID string, inner passFn) ([]memory.ConsolidationResult, error) {
			select {
			case <-open:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
			return inner(ctx, sessionID)
		})
		sys.Start(t.Context())

		s1 := w.session()
		w.chat("a1", s1, "hello")
		advance(minute) // tick 1: the pass starts and is held
		advance(5 * minute)
		close(open)
		synctest.Wait()
		if n := ticks.Load(); n != 2 {
			t.Fatalf("%d ticks evaluated, want 2: the first one and the single pending one", n)
		}
		dispose(t, sys)
	})
}

// A turn whose events carry a payload the dispatcher cannot decode is ignored, never
// a crash: the handler only inserts.
func TestScheduler_UndecodablePayloadIgnored(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w := newWorld(t)
		sys := w.system("a1", memCfg(1), newFakeModel())
		spy := spyPasses(sys, nil)
		sys.Start(t.Context())

		w.bus.Emit(kernel.Event{Type: "agent-loop:complete", SessionID: "s1", Payload: "not a payload"})
		advance(minute)
		if n := spy.count(); n != 0 {
			t.Fatalf("%d passes for an undecodable event, want 0", n)
		}
		dispose(t, sys)
	})
}

// Start is idempotent and Dispose is safe to repeat; a System that was never started
// has nothing to dispose.
func TestScheduler_StartOnceDisposeTwice(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w := newWorld(t)
		sys := w.system("a1", memCfg(1), newFakeModel())
		spy := spyPasses(sys, nil)
		sys.Start(t.Context())
		sys.Start(t.Context()) // a second start must not subscribe twice

		s1 := w.session()
		w.chat("a1", s1, "hello")
		advance(minute)
		if n := spy.count(); n != 1 {
			t.Fatalf("%d passes, want 1", n)
		}
		dispose(t, sys)
		dispose(t, sys)

		idle := w.system("a1", memCfg(1), newFakeModel())
		dispose(t, idle)
	})
}

// TestScheduler_PassTimeout: a background pass that hangs gets a context that ends
// after WithPassTimeout, fails like any failed pass (logged, session stays dirty), and
// the next tick retries the session.
func TestScheduler_PassTimeout(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w := newWorld(t)
		logs := &logCapture{}
		sys := w.system("a1", memCfg(1), newFakeModel(), WithLogger(logs.logger()), WithPassTimeout(30*time.Second))
		var spy *passSpy
		var hung time.Duration
		spy = spyPasses(sys, func(ctx context.Context, _ string, _ passFn) ([]memory.ConsolidationResult, error) {
			if spy.count() == 1 {
				start := time.Now()
				<-ctx.Done() // a model that never answers
				hung = time.Since(start)
				return nil, ctx.Err()
			}
			return []memory.ConsolidationResult{}, nil
		})
		sys.Start(t.Context())

		s1 := w.session()
		w.chat("a1", s1, "hello")
		advance(minute)
		advance(30 * time.Second)
		if hung != 30*time.Second {
			t.Fatalf("the hung pass ran %v, want the 30s pass timeout", hung)
		}
		if out := logs.String(); !strings.Contains(out, "consolidation failed") || !strings.Contains(out, "deadline exceeded") {
			t.Fatalf("the timed-out pass is not logged as failed: %s", out)
		}
		advance(minute)
		if n := spy.count(); n != 2 {
			t.Fatalf("%d passes, want 2: the timed-out session is retried", n)
		}
		dispose(t, sys)
	})
}

// TestPassTimeoutOption: the default bound and the disabled bound.
func TestPassTimeoutOption(t *testing.T) {
	if got := newOptions().passTimeout; got != DefaultPassTimeout {
		t.Errorf("default pass timeout = %v, want %v", got, DefaultPassTimeout)
	}
	if got := newOptions(WithPassTimeout(0)).passTimeout; got != 0 {
		t.Errorf("WithPassTimeout(0) = %v, want no bound", got)
	}
}

// Regression tests of the phase 10 code review warning WR-B01.

// --- WR-B01: a turn that ends while the tick counts is not lost -------------------------

// racingMessages runs hook once, right after the first Count that follows arm: the
// window between the count of a tick and the removal of the session from the dirty set.
type racingMessages struct {
	persistence.MessageStore
	mu   sync.Mutex
	hook func()
}

func (r *racingMessages) arm(hook func()) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.hook = hook
}

func (r *racingMessages) Count(ctx context.Context, sessionID string) (int, error) {
	n, err := r.MessageStore.Count(ctx, sessionID)
	r.mu.Lock()
	hook := r.hook
	r.hook = nil
	r.mu.Unlock()
	if hook != nil {
		hook()
	}
	return n, err
}

// The tick counts the session (all consolidated), a turn of the session ends and appends
// messages before the tick removes it from the dirty set: the session must stay dirty,
// so the next tick consolidates the new messages. Before the fix the removal dropped it
// and the messages were never consolidated.
func TestScheduler_TurnEndingDuringTickKeepsSessionDirty(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w := newWorld(t)
		racing := &racingMessages{MessageStore: w.hub.Messages}
		w.hub.Messages = racing
		sys := w.system("a1", memCfg(1), newFakeModel())
		spy := spyPasses(sys, nil)
		sys.Start(t.Context())

		s1 := w.session()
		w.chat("a1", s1, "first")
		advance(minute)
		if n := spy.count(); n != 1 {
			t.Fatalf("%d passes after the first tick, want 1", n)
		}

		// The next tick counts 2 messages against a checkpoint of 2 (nothing pending),
		// and a turn ends in that window.
		racing.arm(func() { w.chat("a1", s1, "second") })
		advance(minute)
		if n := spy.count(); n != 1 {
			t.Fatalf("%d passes after the racing tick, want still 1 (it saw nothing pending)", n)
		}
		if !sys.sched.isDirty(s1) {
			t.Fatal("the session left the dirty set although a turn ended after the tick read its count")
		}

		advance(minute)
		if n := spy.count(); n != 2 {
			t.Fatalf("%d passes after the next tick, want 2: the new messages must be consolidated", n)
		}
		dispose(t, sys)
	})
}

// Without a racing turn the session still leaves the dirty set once it is consolidated.
func TestScheduler_FullyConsolidatedSessionLeavesTheDirtySet(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w := newWorld(t)
		sys := w.system("a1", memCfg(1), newFakeModel())
		spyPasses(sys, nil)
		sys.Start(t.Context())

		s1 := w.session()
		w.chat("a1", s1, "first")
		advance(minute) // consolidated
		advance(minute) // sees nothing pending
		if sys.sched.isDirty(s1) {
			t.Fatal("a fully consolidated session is still dirty")
		}
		sys.sched.mu.Lock()
		gens := len(sys.sched.gen)
		sys.sched.mu.Unlock()
		if gens != 0 {
			t.Fatalf("%d generation entries left for sessions that are not dirty", gens)
		}
		dispose(t, sys)
	})
}
