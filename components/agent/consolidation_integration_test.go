package agent_test

import (
	"fmt"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/Phong95/aec-agent-go/aecmock"
	"github.com/Phong95/aec-agent-go/components/agent"
	"github.com/Phong95/aec-agent-go/components/agent/memorysystem"
	"github.com/Phong95/aec-agent-go/components/agent/memorysystem/memory"
	"github.com/Phong95/aec-agent-go/components/persistence"
	"github.com/Phong95/aec-agent-go/kernel"
)

// extractionJSON is what the extraction call of a pass gets back: one explicit
// response_language preference.
const extractionJSON = `[{"kind":"user_preference","scope":"user","subject":"user","key":"response_language","value":"English","confidence":"explicit"}]`

// extractionModel is one mock model for both jobs of an agent: a chat turn gets "ok"
// and a consolidation pass (a system message that names the extraction engine) gets
// extractionJSON.
func extractionModel() *aecmock.MockProvider {
	return aecmock.NewMockProvider("mock", func(msgs []kernel.Message) string {
		if len(msgs) > 0 && msgs[0].Role == kernel.RoleSystem && strings.Contains(msgs[0].Content, "memory extraction engine") {
			return extractionJSON
		}
		return "ok"
	})
}

// extractionCalls counts the consolidation passes the model served.
func extractionCalls(m *aecmock.MockProvider) int {
	n := 0
	for _, c := range m.Calls() {
		if len(c.Messages) > 0 && c.Messages[0].Role == kernel.RoleSystem && strings.Contains(c.Messages[0].Content, "memory extraction engine") {
			n++
		}
	}
	return n
}

// everyMinutes is a memory config that consolidates every m minutes (0 disables it).
func everyMinutes(m float64) memory.Config {
	return memory.Config{Enabled: true, UpdateUser: true, Consolidation: &memory.ConsolidationTrigger{IntervalMinutes: &m}}
}

// pinned makes the records of a pass reproducible: a fixed clock and a counting id.
func pinned() agent.Option {
	var n atomic.Int64
	return agent.WithMemoryOptions(
		memorysystem.WithClock(func() time.Time { return time.UnixMilli(1_700_000_000_000) }),
		memorysystem.WithIDs(func() string { return fmt.Sprintf("mem_1700000000000_%d", n.Add(1)) }),
	)
}

// advance sleeps on the bubble's fake clock and lets every goroutine settle.
func advance(d time.Duration) {
	time.Sleep(d)
	synctest.Wait()
}

// schedulerActorFrame is the start of the stack line of the memory scheduler's goroutine.
// It names an unexported method of memorysystem: the entry function of the goroutine that
// rearm starts, so it is always on that goroutine's stack. If a rename makes it stale,
// the "exactly 1 after enabling" assertion of TestAgent_UpdateMemoryConfigRearms fails,
// so the filter cannot go blind silently.
const schedulerActorFrame = "github.com/Phong95/aec-agent-go/components/agent/memorysystem.(*scheduler).actor("

// goroutineCensus is what bubbleGoroutines counts in the caller's synctest bubble.
type goroutineCensus struct {
	all        int    // goroutines in the bubble, the caller included
	schedulers int    // those of them that run the memory scheduler actor
	stacks     string // their stack blocks, joined by blank lines, for failure messages
}

// bubbleGoroutines counts the goroutines of the caller's own synctest bubble, and the
// memory scheduler actors among them. It exists because runtime.NumGoroutine is wrong for
// this job (gap G-12-2): it counts the whole process, so leftovers of earlier tests and
// the finalizer and cleanup goroutines (which count only while they run user code) shift
// it; and it lags synctest.Wait, because a goroutine is marked dead, which wakes Wait,
// before its P frees it. A stop-the-world dump lists only live goroutines, and the header
// of each goroutine of a bubble names that bubble. It must run inside synctest.Test.
func bubbleGoroutines(t *testing.T) goroutineCensus {
	t.Helper()
	buf := make([]byte, 1<<16)
	for {
		n := runtime.Stack(buf, true)
		if n < len(buf) {
			buf = buf[:n]
			break
		}
		buf = make([]byte, 2*len(buf))
	}
	blocks := strings.Split(strings.TrimSpace(string(buf)), "\n\n")

	// runtime.Stack prints the caller first, so the first header names our bubble.
	const marker = ", synctest bubble "
	header, _, _ := strings.Cut(blocks[0], "\n")
	_, rest, ok := strings.Cut(header, marker)
	if !ok {
		t.Fatalf("goroutine header %q has no synctest bubble: bubbleGoroutines must run inside synctest.Test", header)
	}
	id, _, _ := strings.Cut(rest, "]")
	tag := marker + id + "]"

	var c goroutineCensus
	var mine []string
	for _, b := range blocks {
		header, body, _ := strings.Cut(b, "\n")
		if !strings.Contains(header, tag) {
			continue
		}
		c.all++
		mine = append(mine, b)
		for _, line := range strings.Split(body, "\n") {
			if strings.HasPrefix(line, schedulerActorFrame) {
				c.schedulers++
				break
			}
		}
	}
	c.stacks = strings.Join(mine, "\n\n")
	return c
}

// TestBubbleGoroutines tests bubbleGoroutines itself: a goroutine from outside the bubble
// that ends mid-test does not move the count, and goroutines started and released inside
// the bubble move it by exactly their number.
func TestBubbleGoroutines(t *testing.T) {
	// One goroutine outside the bubble, released from inside it.
	hold := make(chan struct{})
	gone := make(chan struct{})
	go func() {
		<-hold
		close(gone)
	}()
	t.Cleanup(func() {
		select {
		case <-hold:
		default:
			close(hold)
		}
	})

	synctest.Test(t, func(t *testing.T) {
		base := bubbleGoroutines(t)
		if base.schedulers != 0 || base.all < 1 {
			t.Fatalf("base = %d goroutines with %d scheduler goroutines, want at least 1 with 0:\n%s", base.all, base.schedulers, base.stacks)
		}

		release := make(chan struct{})
		for range 2 {
			go func() { <-release }()
		}
		synctest.Wait()
		if c := bubbleGoroutines(t); c.all != base.all+2 || c.schedulers != 0 {
			t.Fatalf("after starting two = %d goroutines with %d scheduler goroutines, want %d with 0:\n%s", c.all, c.schedulers, base.all+2, c.stacks)
		}

		// The outside goroutine ends: the bubble's count does not move.
		close(hold)
		<-gone
		if c := bubbleGoroutines(t); c.all != base.all+2 {
			t.Fatalf("after the outside goroutine ended = %d goroutines, want %d:\n%s", c.all, base.all+2, c.stacks)
		}

		close(release)
		synctest.Wait()
		if c := bubbleGoroutines(t); c.all != base.all {
			t.Fatalf("after releasing the two = %d goroutines, want %d:\n%s", c.all, base.all, c.stacks)
		}
	})
}

// consolidatingAgent is an agent whose profile uses the extraction model and the memory
// config cfg, built over env e.
func consolidatingAgent(t *testing.T, e *env, cfg memory.Config, model *aecmock.MockProvider) *agent.Agent {
	t.Helper()
	if err := e.llm.Register(model); err != nil {
		t.Fatal(err)
	}
	p := testProfile("a1")
	p.Provider = "mock"
	p.Memory = cfg
	return e.spawn(t, p, pinned())
}

// TestAgent_ConsolidatesFinishedTurnOnTick is ROADMAP success criterion 4 through a
// real Agent: a turn that finished is consolidated by the memory system the agent
// built, at exactly the interval on virtual time. The record is stamped with the agent
// id, the checkpoint equals the message count, and Dispose leaves no goroutine.
func TestAgent_ConsolidatesFinishedTurnOnTick(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newEnv(t)
		model := extractionModel()
		a := consolidatingAgent(t, e, everyMinutes(1), model)

		session, err := a.StartSession(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		chat(t, a, "my answers should be in English")
		synctest.Wait()

		// One millisecond short of the interval: no pass.
		advance(time.Minute - time.Millisecond)
		if n := extractionCalls(model); n != 0 {
			t.Fatalf("%d consolidation passes before the tick, want 0", n)
		}

		// The tick: one pass.
		advance(time.Millisecond)
		if n := extractionCalls(model); n != 1 {
			t.Fatalf("%d consolidation passes at the tick, want 1", n)
		}
		recs, err := e.hub.Memory.Query(t.Context(), persistence.MemoryFilter{AgentID: "a1"})
		if err != nil {
			t.Fatal(err)
		}
		if len(recs) != 1 || recs[0].AgentID != "a1" || recs[0].Key != "response_language" || recs[0].Value != "English" {
			t.Fatalf("records = %+v, want one response_language record stamped a1", recs)
		}
		stored, err := e.hub.Messages.Get(t.Context(), session.ID)
		if err != nil {
			t.Fatal(err)
		}
		s, err := e.hub.Sessions.Get(t.Context(), session.ID)
		if err != nil || s == nil || s.ConsolidatedMessageCount == nil {
			t.Fatalf("session = %v, %v, want a checkpoint", s, err)
		}
		if got := *s.ConsolidatedMessageCount; got != len(stored) || got != 2 {
			t.Fatalf("checkpoint = %d with %d stored messages, want 2 and 2", got, len(stored))
		}

		// Nothing more to consolidate: the next ticks make no pass.
		advance(3 * time.Minute)
		if n := extractionCalls(model); n != 1 {
			t.Fatalf("%d passes after idle ticks, want still 1", n)
		}

		if err := a.Dispose(t.Context()); err != nil {
			t.Fatalf("Dispose = %v, want nil", err)
		}
	})
}

// TestAgent_UpdateMemoryConfigRearms: UpdateMemoryConfig re-arms the scheduler of the
// agent's memory system. From the interval 0 to 2 minutes it starts the goroutine, and
// the turn that finished while it was off is consolidated; back to 0 it stops the
// goroutine; Profile().Memory reflects each change.
// The scheduler goroutine is identified by its function, and counted only within the
// test's own synctest bubble.
func TestAgent_UpdateMemoryConfigRearms(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newEnv(t)
		model := extractionModel()
		a := consolidatingAgent(t, e, everyMinutes(0), model)

		if _, err := a.StartSession(t.Context()); err != nil {
			t.Fatal(err)
		}
		chat(t, a, "my answers should be in English")
		synctest.Wait()
		advance(10 * time.Minute)
		if n := extractionCalls(model); n != 0 {
			t.Fatalf("%d passes while the interval is 0, want 0", n)
		}

		// The goroutines are counted in this bubble only, by bubbleGoroutines (gap G-12-2).
		base := bubbleGoroutines(t)
		if base.schedulers != 0 {
			t.Fatalf("%d scheduler goroutines while the interval is 0, want 0:\n%s", base.schedulers, base.stacks)
		}
		a.UpdateMemoryConfig(everyMinutes(2))
		synctest.Wait()
		if got := a.Profile().Memory.Consolidation.IntervalMinutes; got == nil || *got != 2 {
			t.Fatalf("Profile().Memory interval = %v, want 2", got)
		}
		if c := bubbleGoroutines(t); c.schedulers != 1 || c.all != base.all+1 {
			t.Fatalf("goroutines in this bubble after enabling = %d with %d scheduler goroutines, want %d with 1: one scheduler goroutine\n%s", c.all, c.schedulers, base.all+1, c.stacks)
		}

		advance(2*time.Minute - time.Millisecond)
		if n := extractionCalls(model); n != 0 {
			t.Fatalf("%d passes before the new interval elapsed, want 0", n)
		}
		advance(time.Millisecond)
		if n := extractionCalls(model); n != 1 {
			t.Fatalf("%d passes at the new interval, want 1: the turn finished while it was off", n)
		}

		// Back to 0: the goroutine ends and a new turn is not consolidated.
		a.UpdateMemoryConfig(everyMinutes(0))
		synctest.Wait()
		if got := a.Profile().Memory.Consolidation.IntervalMinutes; got == nil || *got != 0 {
			t.Fatalf("Profile().Memory interval = %v, want 0", got)
		}
		if c := bubbleGoroutines(t); c.schedulers != 0 || c.all != base.all {
			t.Fatalf("goroutines in this bubble after disabling = %d with %d scheduler goroutines, want %d with 0: the scheduler goroutine must exit\n%s", c.all, c.schedulers, base.all, c.stacks)
		}
		chat(t, a, "and keep it short")
		advance(10 * time.Minute)
		if n := extractionCalls(model); n != 1 {
			t.Fatalf("%d passes after disabling, want still 1", n)
		}

		if err := a.Dispose(t.Context()); err != nil {
			t.Fatalf("Dispose = %v, want nil", err)
		}
	})
}
