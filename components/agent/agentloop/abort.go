package agentloop

import (
	"context"
	"fmt"
	"time"
)

// raceAbort runs work and returns as soon as it finishes or ctx is done, whichever
// comes first. When ctx finishes first, aborted is true and the late
// result is dropped.
//
// work may ignore ctx, as a tool that does not watch it does: it keeps running in
// its own goroutine after the abort, but it can never block the caller. The channel
// holds one value, so the goroutine's send never blocks and the goroutine always
// exits when work returns. A panic in work is recovered and returned as an error
// (a second net: the per-attempt recover of the executor is the first).
//
// The error is the last return value, as the linter's error-return rule wants.
func raceAbort[T any](ctx context.Context, work func() (T, error)) (v T, aborted bool, err error) {
	return race(ctx, work, false)
}

// raceAbortKeepFinished is raceAbort for work whose finished result must not be
// lost: when the stop is seen and the work has already finished, its result wins
// and aborted is false. The tool dispatch uses it, so a tool that ran is never
// reported as cancelled. Work that is still running when
// the stop is seen is abandoned exactly as with raceAbort.
func raceAbortKeepFinished[T any](ctx context.Context, work func() (T, error)) (v T, aborted bool, err error) {
	return race(ctx, work, true)
}

// race is raceAbort and raceAbortKeepFinished; keepFinished says whether a result
// that is ready when ctx is done is returned instead of the abort.
func race[T any](ctx context.Context, work func() (T, error), keepFinished bool) (v T, aborted bool, err error) {
	select {
	case <-ctx.Done():
		return v, true, nil // already stopped: do not even start the work
	default:
	}
	type outcome struct {
		v   T
		err error
	}
	ch := make(chan outcome, 1)
	go func() {
		var o outcome
		defer func() {
			if r := recover(); r != nil {
				o.err = fmt.Errorf("panic: %v", r)
			}
			ch <- o
		}()
		o.v, o.err = work()
	}()
	select {
	case o := <-ch:
		return o.v, false, o.err
	case <-ctx.Done():
		if keepFinished {
			// Both may be ready at once (select picks at random): a finished result wins.
			select {
			case o := <-ch:
				return o.v, false, o.err
			default:
			}
		}
		return v, true, nil
	}
}

// abortableDelay waits d, or less when ctx is done first, so a
// stopped run does not sit through the rest of a retry backoff. The timer is
// stopped on every path.
func abortableDelay(ctx context.Context, d time.Duration) {
	if d <= 0 {
		return
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
	case <-ctx.Done():
	}
}
