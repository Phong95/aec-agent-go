package artifacthub

import (
	"context"
	"sync"
)

// The hub serialises the operations that conflict with each other, not all of
// them. Two locks are involved, always taken in this order:
//
//  1. the agent gate (shared by Save, Update and Delete, exclusive for
//     ReleaseAgentData), so removing an agent's data waits for the writes in
//     flight and a write cannot add a version to an artifact that was just
//     removed;
//  2. the artifact lock, keyed by agent id and artifact name, so a check-then-write
//     on one name never interleaves with another one on the same name.
//
// Operations on other agents, or on other names of the same agent, run in
// parallel while no ReleaseAgentData is pending for that agent: a slow sandbox
// write holds up only its own name. A pending ReleaseAgentData does hold the whole
// agent back: it waits for the slowest write in flight, and the Save, Update and
// Delete that arrive meanwhile queue behind it (a writer that waits must not be
// starved by a stream of new readers). Every one of those waits - the agent gate
// for the shared and for the exclusive side, and the artifact lock - ends with the
// context error when the context ends, so a stuck sandbox write can delay an
// operation but never park it beyond its own deadline.

// lockRef is one entry of a refTable with the number of holders and waiters.
type lockRef[L any] struct {
	refs int
	lock L
}

// refTable is a map of locks that drops an entry when nobody holds or waits for
// it, so the table does not grow with every name ever used.
type refTable[L any] struct {
	mu      sync.Mutex
	entries map[string]*lockRef[L]
	init    func(*L)
}

// acquire returns the entry for key (creating it) and counts the caller as a
// user of it; the caller must release it.
func (t *refTable[L]) acquire(key string) *lockRef[L] {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.entries == nil {
		t.entries = make(map[string]*lockRef[L])
	}
	e := t.entries[key]
	if e == nil {
		e = &lockRef[L]{}
		if t.init != nil {
			t.init(&e.lock)
		}
		t.entries[key] = e
	}
	e.refs++
	return e
}

// release ends one use of the entry and drops it when it was the last.
func (t *refTable[L]) release(key string, e *lockRef[L]) {
	t.mu.Lock()
	defer t.mu.Unlock()
	e.refs--
	if e.refs == 0 {
		delete(t.entries, key)
	}
}

// nameSem is a one-slot semaphore: a channel, so that a wait can be cancelled.
type nameSem struct {
	slot chan struct{}
}

// agentGate is a reader-writer lock whose waits honour a context. It has the
// fairness of a sync.RWMutex (a waiting writer holds new readers back) but is built
// from a mutex and a broadcast channel, because sync.RWMutex.RLock and Lock cannot
// be cancelled. changed is closed and replaced on every change that may let a
// waiter in; a waiter takes the channel under mu, releases mu and selects on it and
// on ctx.Done().
type agentGate struct {
	mu             sync.Mutex
	readers        int
	writing        bool
	writersWaiting int
	changed        chan struct{}
}

// wake lets every waiter re-check. The caller holds mu.
func (g *agentGate) wake() {
	close(g.changed)
	g.changed = make(chan struct{})
}

// rlock takes the shared side. A free gate is taken whatever the context says;
// when it has to wait, the wait ends with the context error.
func (g *agentGate) rlock(ctx context.Context) error {
	g.mu.Lock()
	for {
		if !g.writing && g.writersWaiting == 0 {
			g.readers++
			g.mu.Unlock()
			return nil
		}
		changed := g.changed
		g.mu.Unlock()
		select {
		case <-changed:
		case <-ctx.Done():
			return ctx.Err()
		}
		g.mu.Lock()
	}
}

func (g *agentGate) runlock() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.readers--
	if g.readers == 0 {
		g.wake()
	}
}

// lock takes the exclusive side: it waits for the shared holders in flight and,
// while it waits, holds new shared takers back. When the wait is cancelled the
// takers it held back are let in again.
func (g *agentGate) lock(ctx context.Context) error {
	g.mu.Lock()
	g.writersWaiting++
	for {
		if !g.writing && g.readers == 0 {
			g.writersWaiting--
			g.writing = true
			g.mu.Unlock()
			return nil
		}
		changed := g.changed
		g.mu.Unlock()
		select {
		case <-changed:
		case <-ctx.Done():
			g.mu.Lock()
			g.writersWaiting--
			g.wake()
			g.mu.Unlock()
			return ctx.Err()
		}
		g.mu.Lock()
	}
}

func (g *agentGate) unlock() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.writing = false
	g.wake()
}

// locks are the hub's keyed locks.
type locks struct {
	agents refTable[agentGate]
	names  refTable[nameSem]
}

func newLocks() *locks {
	l := &locks{}
	l.agents.init = func(g *agentGate) { g.changed = make(chan struct{}) }
	l.names.init = func(s *nameSem) { s.slot = make(chan struct{}, 1) }
	return l
}

// lockArtifact takes the agent gate (shared) and the artifact lock of (agentID,
// name) and returns the function that releases both. It fails with the context
// error when the context ends while either of them is busy.
func (l *locks) lockArtifact(ctx context.Context, agentID, name string) (unlock func(), err error) {
	agent := l.agents.acquire(agentID)
	if err := agent.lock.rlock(ctx); err != nil {
		l.agents.release(agentID, agent)
		return nil, err
	}

	key := agentID + "\x00" + name
	n := l.names.acquire(key)
	select {
	case n.lock.slot <- struct{}{}: // free: take it whatever the context says
	default:
		select {
		case n.lock.slot <- struct{}{}:
		case <-ctx.Done():
			l.names.release(key, n)
			agent.lock.runlock()
			l.agents.release(agentID, agent)
			return nil, ctx.Err()
		}
	}
	return func() {
		<-n.lock.slot
		l.names.release(key, n)
		agent.lock.runlock()
		l.agents.release(agentID, agent)
	}, nil
}

// lockAgent takes the agent gate exclusively: it waits for every Save, Update and
// Delete of the agent in flight and holds new ones back until it is released. The
// wait ends with the context error when the context ends.
func (l *locks) lockAgent(ctx context.Context, agentID string) (unlock func(), err error) {
	agent := l.agents.acquire(agentID)
	if err := agent.lock.lock(ctx); err != nil {
		l.agents.release(agentID, agent)
		return nil, err
	}
	return func() {
		agent.lock.unlock()
		l.agents.release(agentID, agent)
	}, nil
}
