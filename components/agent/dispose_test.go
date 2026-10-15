package agent_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/Phong95/aec-agent-go/aecmock"
	"github.com/Phong95/aec-agent-go/components/agent"
	"github.com/Phong95/aec-agent-go/components/persistence"
	"github.com/Phong95/aec-agent-go/components/sandboxhub"
)

// heldMessages is a message store whose GetRange, the read of a consolidation pass,
// blocks until the gate is opened or the ctx of the pass ends. It records the ctx of the
// pass, so a test can see whether the lifetime context of the agent was cancelled.
type heldMessages struct {
	persistence.MessageStore
	entered chan struct{}
	gate    chan struct{}

	once sync.Once
	mu   sync.Mutex
	ctx  context.Context
}

func newHeldMessages(inner persistence.MessageStore) *heldMessages {
	return &heldMessages{MessageStore: inner, entered: make(chan struct{}), gate: make(chan struct{})}
}

func (h *heldMessages) GetRange(ctx context.Context, sessionID string, before, limit int) (persistence.MessageSlice, error) {
	h.once.Do(func() {
		h.mu.Lock()
		h.ctx = ctx
		h.mu.Unlock()
		close(h.entered)
	})
	select {
	case <-h.gate:
	case <-ctx.Done():
		return persistence.MessageSlice{}, ctx.Err()
	}
	return h.MessageStore.GetRange(ctx, sessionID, before, limit)
}

// passCtx is the ctx of the held pass.
func (h *heldMessages) passCtx() context.Context {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.ctx
}

func (h *heldMessages) release() { close(h.gate) }

// disposeRig is an agent with an own sandbox that has created it, and a consolidation
// pass that is held open in the message store.
type disposeRig struct {
	*env
	agent    *agent.Agent
	held     *heldMessages
	provider *aecmock.InMemoryProvider
	own      *aecmock.InMemorySandbox
}

// newDisposeRig must run inside a synctest bubble: it advances fake time to start the
// pass.
func newDisposeRig(t *testing.T) *disposeRig {
	t.Helper()
	e := newEnv(t)
	held := newHeldMessages(persistence.NewMemoryMessageStore())
	e.hub = persistence.NewPersistenceHub(persistence.WithMessages(held))
	provider := aecmock.NewInMemoryProvider()
	if err := e.sandboxes.Register(provider); err != nil {
		t.Fatal(err)
	}
	model := extractionModel()
	if err := e.llm.Register(model); err != nil {
		t.Fatal(err)
	}
	p := testProfile("a1")
	p.Provider = "mock"
	p.Memory = everyMinutes(1)
	p.Sandbox = ownSelection(sandboxhub.OwnConfig{})
	a := e.spawn(t, p, pinned())

	if _, err := a.StartSession(t.Context()); err != nil {
		t.Fatal(err)
	}
	chat(t, a, "my answers should be in English")
	if _, err := a.Sandbox(t.Context()); err != nil {
		t.Fatalf("Sandbox: %v", err)
	}
	own, ok := provider.Sandbox("docker:own:a1")
	if !ok {
		t.Fatal("the own sandbox was not created")
	}

	advance(time.Minute) // the tick: the pass starts and blocks in GetRange
	select {
	case <-held.entered:
	default:
		t.Fatal("the consolidation pass did not start at the tick")
	}
	return &disposeRig{env: e, agent: a, held: held, provider: provider, own: own}
}

// TestAgent_DisposeOrder: Dispose waits for the consolidation pass in flight, then
// releases the own sandbox, then cancels the lifetime ctx. While the pass is held the
// sandbox is still running and the pass ctx is still alive.
func TestAgent_DisposeOrder(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		r := newDisposeRig(t)

		done := make(chan error, 1)
		go func() { done <- r.agent.Dispose(t.Context()) }()
		synctest.Wait()

		select {
		case err := <-done:
			t.Fatalf("Dispose returned %v while a pass was running, want it to wait", err)
		default:
		}
		if r.own.Disposed() {
			t.Fatal("the own sandbox was released before the memory system was stopped")
		}
		if err := r.held.passCtx().Err(); err != nil {
			t.Fatalf("the lifetime ctx was cancelled (%v) before Dispose's wait ended", err)
		}

		r.held.release()
		if err := <-done; err != nil {
			t.Fatalf("Dispose = %v, want nil", err)
		}
		if !r.own.Disposed() {
			t.Fatal("Dispose did not release the own sandbox")
		}
		if err := r.held.passCtx().Err(); err == nil {
			t.Fatal("the lifetime ctx is still alive after Dispose")
		}
		// The pass that was waited for completed: the memory was consolidated.
		recs, err := r.hub.Memory.Query(t.Context(), persistence.MemoryFilter{AgentID: "a1"})
		if err != nil || len(recs) != 1 {
			t.Fatalf("records after Dispose = %v, %v, want the waited-for pass to have written one", recs, err)
		}
	})
}

// TestAgent_DisposeTwice: a second Dispose with the same options returns nil and does
// nothing more. One with RemoveData is not a no-op (WR-A01): the first Dispose kept the
// data, so the second one removes it, once.
func TestAgent_DisposeTwice(t *testing.T) {
	r := newSandboxRig(t, ownSelection(sandboxhub.OwnConfig{}), true)
	if _, err := r.agent.Sandbox(t.Context()); err != nil {
		t.Fatal(err)
	}
	ctx := context.WithoutCancel(t.Context())
	if err := r.agent.Dispose(ctx); err != nil {
		t.Fatalf("first Dispose = %v, want nil", err)
	}
	if err := r.agent.Dispose(ctx); err != nil {
		t.Fatalf("second Dispose = %v, want nil", err)
	}
	if got := r.provider.RemovedData(); len(got) != 0 {
		t.Fatalf("RemovedData after a second Dispose without RemoveData = %v, want none", got)
	}
	if err := r.agent.Delete(ctx); err != nil {
		t.Fatalf("Delete after Dispose = %v, want nil", err)
	}
	if got := r.provider.RemovedData(); len(got) != 1 {
		t.Fatalf("RemovedData after Delete = %v, want the own data removed once", got)
	}
	if err := r.agent.Delete(ctx); err != nil {
		t.Fatalf("repeated Delete = %v, want nil", err)
	}
	if got := r.provider.RemovedData(); len(got) != 1 {
		t.Fatalf("RemovedData after a repeated Delete = %v, want it removed only once", got)
	}
}

// TestAgent_DisposeContextExpiry: when the ctx of Dispose ends before the held pass
// does, Dispose returns the ctx error after exactly that much fake time, still
// releases the own sandbox and still cancels the lifetime ctx, so the pass sees the
// cancellation and ends: nothing outlives Dispose.
func TestAgent_DisposeContextExpiry(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		r := newDisposeRig(t)

		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		defer cancel()
		start := time.Now()
		done := make(chan error, 1)
		go func() { done <- r.agent.Dispose(ctx) }()
		synctest.Wait()
		select {
		case err := <-done:
			t.Fatalf("Dispose returned %v before the ctx expired", err)
		default:
		}

		time.Sleep(5 * time.Second)
		err := <-done
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("Dispose = %v, want the ctx error", err)
		}
		if got := time.Since(start); got != 5*time.Second {
			t.Fatalf("Dispose returned after %v of fake time, want 5s", got)
		}
		if !r.own.Disposed() {
			t.Fatal("Dispose did not release the own sandbox after the memory step failed")
		}
		if err := r.held.passCtx().Err(); err == nil {
			t.Fatal("the lifetime ctx was not cancelled: the held pass would run for ever")
		}
		synctest.Wait() // the pass ends on its own; the bubble has no goroutine left
		if err := r.agent.Dispose(t.Context()); err != nil {
			t.Fatalf("second Dispose = %v, want nil", err)
		}
	})
}

// Update (A-126): the scheduled-task Dispose tests (a running task is waited for, the
// ctx of Dispose expires) were removed with scheduled tasks; the job manager's close is
// covered by its own tests.

// Regression tests of the phase 10 code review warning WR-A07 (the WR-A01 cases are
// above and in the root package tests).

// --- WR-A07: Dispose from a job, and a second Dispose that honours its ctx ----------------

// Update (A-126): the case of a scheduled task that disposes its own agent was removed
// with scheduled tasks.

// A second Dispose that finds the first one still waiting returns when its own ctx
// ends, not when the first one is done.
func TestAgent_SecondDisposeHonoursItsContext(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		r := newDisposeRig(t)

		first := make(chan error, 1)
		go func() { first <- r.agent.Dispose(t.Context()) }()
		synctest.Wait() // the first Dispose waits for the held pass

		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		defer cancel()
		start := time.Now()
		err := r.agent.Dispose(ctx)
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("second Dispose = %v, want the ctx error", err)
		}
		if got := time.Since(start); got != 5*time.Second {
			t.Fatalf("second Dispose returned after %v of fake time, want 5s", got)
		}
		select {
		case err := <-first:
			t.Fatalf("the first Dispose returned %v early", err)
		default:
		}

		r.held.release()
		if err := <-first; err != nil {
			t.Fatalf("first Dispose = %v, want nil", err)
		}
	})
}
