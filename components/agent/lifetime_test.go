package agent_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/Phong95/aec-agent-go/components/agent"
	"github.com/Phong95/aec-agent-go/components/persistence"
)

// requestKey is a context key that stands for a request-scoped value (an auth token
// or a trace id) of the request that spawns an agent.
type requestKey struct{}

// TestAgent_LifetimeDropsRequestValues: the background lifetime of an agent derives
// from context.Background(), so a value carried by the ctx given to agent.New is not
// visible in the ctx of a consolidation pass (10-SECURITY UF-3, Phase 12 12-02). The
// lifetime is still cancelled by Dispose, and New still fails on a ctx that is
// already done.
func TestAgent_LifetimeDropsRequestValues(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newEnv(t)
		held := newHeldMessages(persistence.NewMemoryMessageStore())
		e.hub = persistence.NewPersistenceHub(persistence.WithMessages(held))
		if err := e.llm.Register(extractionModel()); err != nil {
			t.Fatal(err)
		}
		p := testProfile("a1")
		p.Provider = "mock"
		p.Memory = everyMinutes(1)

		request := context.WithValue(t.Context(), requestKey{}, "request-secret")
		if request.Value(requestKey{}) == nil {
			t.Fatal("the request ctx does not carry its value")
		}
		a, err := agent.New(request, e.deps(), p, pinned())
		if err != nil {
			t.Fatalf("agent.New: %v", err)
		}
		t.Cleanup(func() { _ = a.Dispose(context.WithoutCancel(t.Context())) })
		// A failed assertion must not leave the held pass blocking the Dispose above.
		var releaseOnce sync.Once
		release := func() { releaseOnce.Do(held.release) }
		t.Cleanup(release)

		if _, err := a.StartSession(t.Context()); err != nil {
			t.Fatal(err)
		}
		chat(t, a, "my answers should be in English")

		advance(time.Minute) // the tick: the pass starts and blocks in GetRange
		select {
		case <-held.entered:
		default:
			t.Fatal("the consolidation pass did not start at the tick")
		}

		if v := held.passCtx().Value(requestKey{}); v != nil {
			t.Fatalf("the pass ctx carries the request value %v, want the lifetime to drop it", v)
		}
		if err := held.passCtx().Err(); err != nil {
			t.Fatalf("the lifetime ctx is cancelled (%v) before Dispose", err)
		}

		done := make(chan error, 1)
		go func() { done <- a.Dispose(t.Context()) }()
		synctest.Wait()
		release()
		if err := <-done; err != nil {
			t.Fatalf("Dispose = %v, want nil", err)
		}
		if err := held.passCtx().Err(); err == nil {
			t.Fatal("the lifetime ctx is still alive after Dispose")
		}
	})
}

// TestAgent_NewDoneCtx: the ctx.Err() precondition of New is kept when the lifetime
// no longer derives from the ctx.
func TestAgent_NewDoneCtx(t *testing.T) {
	e := newEnv(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := agent.New(ctx, e.deps(), testProfile("a1"), pinned()); !errors.Is(err, context.Canceled) {
		t.Fatalf("agent.New with a cancelled ctx = %v, want %v", err, context.Canceled)
	}
}
