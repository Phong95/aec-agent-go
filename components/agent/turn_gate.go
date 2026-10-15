package agent

import (
	"context"
	"slices"
	"sync"
)

// turnGate decides when an agent's turns may run (A-97). It replaces the per-agent run
// lock (one turn at a time per agent):
//
//   - Turns in different sessions run in parallel.
//   - Turns in the same session run one at a time, in arrival order: a session's history
//     is appended in order, so two turns at once would interleave.
//   - An optional per-agent cap (maxConcurrent, 0 = no cap) bounds how many turns run at
//     once across all sessions, for cost and rate-limit control. It is read on every
//     acquire and release, so a runtime change of the loop config applies to the next
//     turn.
//   - A nested turn (one started from inside a running turn of the same agent, on
//     another session) never waits for a slot under the cap: the turn that started it
//     holds a slot and waits for it, so waiting would deadlock once the cap is reached.
//     It still counts as running.
//   - A turn waiting for its slot gives up when its ctx ends (the ctx error) or when the
//     agent's closing channel is closed (ErrAgentDisposed). A turn that does not have to
//     wait starts at once, even with a ctx that is already done: the loop then ends it at
//     its first safe point (the A-65 contract of an idle agent).
//
// The gate only orders the turns; the ConversationManager keeps the dispose bookkeeping (runs,
// closing, runsCtx) around it. It is safe for concurrent use.
type turnGate struct {
	// maxConcurrent returns the agent cap (loopConfig.maxConcurrentTurns); 0 or less
	// means no cap. It is never called with mu held.
	maxConcurrent func() int

	mu sync.Mutex
	// sessions holds one queue per session that has a turn running or waiting; an idle
	// session has no entry, so idle sessions cost nothing.
	sessions map[string]*sessionQueue
	// running counts the turns that hold a slot under the cap (every running turn).
	running int
	// capWaiters are the turns that hold their session and wait for a slot under the
	// cap, oldest first.
	capWaiters []chan struct{}
}

// sessionQueue is the FIFO of one session: the turn that runs (busy) and the turns that
// wait behind it, oldest first. A release hands the session to the first waiter by
// closing its channel, so busy stays true across the hand-off.
type sessionQueue struct {
	busy    bool
	waiters []chan struct{}
}

func newTurnGate(maxConcurrent func() int) *turnGate {
	return &turnGate{maxConcurrent: maxConcurrent, sessions: map[string]*sessionQueue{}}
}

// acquire waits for sessionID's turn and then for a slot under the agent cap (not for a
// nested turn, see the type comment). On success the caller must call
// release(sessionID) once the turn ends. It returns the ctx error when ctx ended while
// waiting, and ErrAgentDisposed when closing was closed while waiting; nothing is held
// then.
func (g *turnGate) acquire(ctx context.Context, sessionID string, nested bool, closing <-chan struct{}) error {
	if err := g.takeSession(ctx, sessionID, closing); err != nil {
		return err
	}
	if err := g.takeSlot(ctx, nested, closing); err != nil {
		g.releaseSession(sessionID)
		return err
	}
	return nil
}

// release ends a turn: it frees the turn's slot under the cap and lets the session's
// next turn start.
func (g *turnGate) release(sessionID string) {
	g.freeSlot()
	g.releaseSession(sessionID)
}

// runningTurns is how many turns run now, across all sessions.
func (g *turnGate) runningTurns() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.running
}

// takeSession joins the session's queue and returns once every earlier turn of the
// session has ended.
func (g *turnGate) takeSession(ctx context.Context, sessionID string, closing <-chan struct{}) error {
	g.mu.Lock()
	q := g.sessions[sessionID]
	if q == nil {
		q = &sessionQueue{}
		g.sessions[sessionID] = q
	}
	if !q.busy {
		// A free session starts at once, whatever ctx says (see the type comment).
		q.busy = true
		g.mu.Unlock()
		return nil
	}
	ch := make(chan struct{})
	q.waiters = append(q.waiters, ch)
	g.mu.Unlock()

	var err error
	select {
	case <-ch:
		return nil
	case <-ctx.Done():
		err = ctx.Err()
	case <-closing:
		err = ErrAgentDisposed
	}
	g.mu.Lock()
	if i := slices.Index(q.waiters, ch); i >= 0 {
		q.waiters = slices.Delete(q.waiters, i, i+1)
		g.mu.Unlock()
		return err
	}
	g.mu.Unlock()
	// The session was handed to us right as we gave up: pass it on.
	g.releaseSession(sessionID)
	return err
}

// releaseSession hands the session to its oldest waiter, or drops its queue when nobody
// waits.
func (g *turnGate) releaseSession(sessionID string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	q := g.sessions[sessionID]
	if q == nil {
		return
	}
	if len(q.waiters) > 0 {
		next := q.waiters[0]
		q.waiters = q.waiters[1:]
		close(next)
		return
	}
	delete(g.sessions, sessionID)
}

// takeSlot takes a slot under the agent cap, waiting for one when the cap is reached
// (a nested turn takes one at once, even over the cap).
func (g *turnGate) takeSlot(ctx context.Context, nested bool, closing <-chan struct{}) error {
	limit := g.maxConcurrent()
	g.mu.Lock()
	if nested || limit <= 0 || g.running < limit {
		g.running++
		g.mu.Unlock()
		return nil
	}
	ch := make(chan struct{})
	g.capWaiters = append(g.capWaiters, ch)
	g.mu.Unlock()

	var err error
	select {
	case <-ch:
		// The releasing turn counted the slot for us (running was incremented).
		return nil
	case <-ctx.Done():
		err = ctx.Err()
	case <-closing:
		err = ErrAgentDisposed
	}
	g.mu.Lock()
	if i := slices.Index(g.capWaiters, ch); i >= 0 {
		g.capWaiters = slices.Delete(g.capWaiters, i, i+1)
		g.mu.Unlock()
		return err
	}
	g.mu.Unlock()
	// We were handed a slot right as we gave up: pass it on.
	g.freeSlot()
	return err
}

// freeSlot frees a slot and wakes the oldest waiters while there is room under the cap
// (more than one when the cap was raised or removed meanwhile).
func (g *turnGate) freeSlot() {
	limit := g.maxConcurrent()
	g.mu.Lock()
	defer g.mu.Unlock()
	g.running--
	for len(g.capWaiters) > 0 && (limit <= 0 || g.running < limit) {
		next := g.capWaiters[0]
		g.capWaiters = g.capWaiters[1:]
		g.running++
		close(next)
	}
}
