package aec

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"
)

// runAll runs every step in order, whatever the earlier ones returned, and joins the
// errors (Dispose continues past a failing agent). No real step of Dispose can fail yet (the persistence hub's hook does
// nothing and the sandbox hub swallows the sandbox's own error), so the helper
// Dispose is built on is tested directly.
func TestRunAll_ContinuesPastFailuresAndJoins(t *testing.T) {
	errA, errB := errors.New("agent a failed"), errors.New("sandbox failed")
	var order []string
	step := func(name string, err error) func(context.Context) error {
		return func(context.Context) error {
			order = append(order, name)
			return err
		}
	}

	err := runAll(t.Context(), []func(context.Context) error{
		step("agent-a", errA),
		step("agent-b", nil),
		step("sandbox", errB),
		step("persistence", nil),
	})
	if want := []string{"agent-a", "agent-b", "sandbox", "persistence"}; !slices.Equal(order, want) {
		t.Fatalf("steps run = %v, want all of %v in order", order, want)
	}
	if !errors.Is(err, errA) || !errors.Is(err, errB) {
		t.Fatalf("joined error = %v, want a match on both failures", err)
	}

	if err := runAll(t.Context(), []func(context.Context) error{step("ok", nil)}); err != nil {
		t.Fatalf("runAll with no failure = %v, want nil", err)
	}
}

type ctxKey struct{}

// A cleanup step of Dispose still runs when the Dispose ctx has already ended: it gets
// the values of that ctx but not its cancellation, and a deadline of its own
// (disposeStepTimeout), so an expired ctx cannot make the docker rm of the shared
// sandbox fail at once and leak it.
func TestDetachedStep_SurvivesAnEndedCtx(t *testing.T) {
	ctx, cancel := context.WithCancel(context.WithValue(t.Context(), ctxKey{}, "trace-1"))
	cancel()
	var stepErr error
	var value any
	var deadline time.Time
	var hasDeadline bool
	err := detachedStep(func(ctx context.Context) error {
		stepErr = ctx.Err()
		value = ctx.Value(ctxKey{})
		deadline, hasDeadline = ctx.Deadline()
		return nil
	})(ctx)
	if err != nil || stepErr != nil {
		t.Fatalf("step = %v, ctx.Err in the step = %v, want both nil", err, stepErr)
	}
	if value != "trace-1" {
		t.Errorf("the step lost the ctx values: %v", value)
	}
	if !hasDeadline || time.Until(deadline) > disposeStepTimeout {
		t.Errorf("the step has no bounded deadline: %v %v", deadline, hasDeadline)
	}
}

// forEachBounded runs every item, never more than fanOutParallelism at once, and
// returns only when all of them returned.
func TestForEachBounded(t *testing.T) {
	items := make([]int, 40)
	var mu sync.Mutex
	var live, peak, done int
	forEachBounded(items, func(int) {
		mu.Lock()
		live++
		peak = max(peak, live)
		mu.Unlock()
		time.Sleep(time.Millisecond)
		mu.Lock()
		live--
		done++
		mu.Unlock()
	})
	if done != len(items) {
		t.Fatalf("%d items ran, want %d", done, len(items))
	}
	if peak > fanOutParallelism || peak < 2 {
		t.Fatalf("peak concurrency = %d, want between 2 and %d", peak, fanOutParallelism)
	}
}
