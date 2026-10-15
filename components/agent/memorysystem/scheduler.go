package memorysystem

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/Phong95/aec-agent-go/components/agent/memorysystem/memory"
	"github.com/Phong95/aec-agent-go/components/events"
	"github.com/Phong95/aec-agent-go/components/persistence"
	"github.com/Phong95/aec-agent-go/internal/redact"
	"github.com/Phong95/aec-agent-go/kernel"
)

// ---------------------------------------------------------------------------
// scheduler - decides WHEN to run a consolidation pass
//
// The scheduler is shaped as an actor: one goroutine per System owns the interval
// ticker, the FIFO queue and the passes, and selects on the ticker, a rearm signal and
// a stop signal. The event handler that hears about finished turns only inserts the
// session into a mutex-guarded, insertion-ordered set; it never blocks and does no I/O.
// Passes run inline in the actor, so "at most one pass at a time" is structural, not a
// flag.
//
//   - A finished turn (RunCompleted, aborted or not, or MaxIterationsReached) of this
//     agent marks its session dirty. The set keeps a session until a tick finds it
//     fully consolidated, so it holds one id per session for the agent's lifetime.
//   - Every tick consolidates ALL the dirty sessions that still have unprocessed
//     messages, in the order in which they became dirty - not just the active one. A
//     failed (or panicking) pass leaves its session dirty, so the next tick retries.
//   - The goroutine exists only while the interval is above zero. rearm starts it,
//     restarts its period or lets it exit, according to the current config; the
//     event subscription is made in start whatever the interval is.
//   - stop unsubscribes, drops the queue, lets the actor exit and waits for a pass in
//     flight. It never starts a final pass.
//
// The actor collects the whole dirty set before it drains; a tick that arrives while
// a pass runs is dropped, because a time.Ticker keeps at most one pending tick; and a
// panic in a store or model is recovered and logged.
// ---------------------------------------------------------------------------

// schedulerDeps are the scheduler's collaborators, all consumer-side.
type schedulerDeps struct {
	agentID  string
	bus      events.Bus
	messages persistence.MessageStore
	logger   *slog.Logger
	// config is the current memory config, read on every decision so that runtime
	// updates apply immediately.
	config func() memory.Config
	// checkpoint is the number of messages of a session that a pass already
	// processed (the persisted Session checkpoint).
	checkpoint func(ctx context.Context, sessionID string) (int, error)
	// consolidate runs one consolidation pass for a session.
	consolidate func(ctx context.Context, sessionID string) ([]memory.ConsolidationResult, error)
	// passTimeout bounds one pass (WithPassTimeout); <= 0 means no bound.
	passTimeout time.Duration
}

// scheduler is the consolidation scheduler of one System.
type scheduler struct {
	schedulerDeps

	// wake carries the rearm signal to the actor (capacity one: a pending signal
	// already says "restart your period"). quit is closed by stop.
	wake chan struct{}
	quit chan struct{}

	// mu guards everything below. The event handler holds it only for a set insert and
	// the actor only for short reads and writes of this state, never across I/O.
	mu       sync.Mutex
	lifetime context.Context
	started  bool
	stopped  bool
	unsubs   []events.Unsubscribe
	// dirty holds the sessions that received messages and may not be fully
	// consolidated yet, in insertion order like a JS Set; dirtySet is its index.
	dirty    []string
	dirtySet map[string]struct{}
	// gen counts the finished turns of each session that is in the dirty set. A tick
	// reads it before it counts the pending messages and removes the session only
	// when it has not moved, so a turn that ends between the count and the removal
	// is not lost (WR-B01). An entry exists only while the session is dirty.
	gen map[string]uint64
	// queue is the FIFO of sessions that the current tick decided to consolidate.
	queue []string
	// running is true while an actor goroutine is alive; done is closed when it exits.
	running bool
	done    chan struct{}
	// busy is non-nil while the actor evaluates a tick or runs a pass, and is closed
	// when it is idle again (no tick pending and no pass queued or running).
	busy chan struct{}
}

func newScheduler(d schedulerDeps) *scheduler {
	return &scheduler{
		schedulerDeps: d,
		wake:          make(chan struct{}, 1),
		quit:          make(chan struct{}),
		dirtySet:      map[string]struct{}{},
		gen:           map[string]uint64{},
	}
}

// start subscribes to the finished turns and arms the interval. The subscription is
// unconditional; only the goroutine depends on the interval.
func (s *scheduler) start(lifetime context.Context) {
	s.mu.Lock()
	if s.started || s.stopped {
		s.mu.Unlock()
		return
	}
	s.started = true
	s.lifetime = lifetime
	s.mu.Unlock()

	onRunCompleted := events.Subscribe(s.bus, events.RunCompleted, func(e kernel.Event, p events.RunCompletedPayload) {
		s.onTurnEnd(e.SessionID, p.AgentID)
	})
	onMaxIterations := events.Subscribe(s.bus, events.MaxIterationsReached, func(e kernel.Event, p events.MaxIterationsPayload) {
		s.onTurnEnd(e.SessionID, p.AgentID)
	})

	s.mu.Lock()
	if s.stopped {
		// Dispose won the race: do not leave the subscriptions behind.
		s.mu.Unlock()
		onRunCompleted()
		onMaxIterations()
		return
	}
	s.unsubs = append(s.unsubs, onRunCompleted, onMaxIterations)
	s.mu.Unlock()

	s.rearm()
}

// onTurnEnd is the whole event handler: a finished turn of this agent makes its
// session dirty. It never blocks and does no I/O.
func (s *scheduler) onTurnEnd(sessionID, agentID string) {
	if agentID != s.agentID {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopped {
		return
	}
	// Always, also for a session that is already dirty: a tick that is counting the
	// session's messages right now must see that a turn ended after it read the
	// generation, so it does not remove the session.
	s.gen[sessionID]++
	if _, ok := s.dirtySet[sessionID]; !ok {
		s.dirtySet[sessionID] = struct{}{}
		s.dirty = append(s.dirty, sessionID)
	}
}

// rearm re-reads the config and starts, restarts or stops the actor: an actor that runs
// restarts its period, one that does not run is started when the interval is above
// zero, and an actor whose interval became zero exits by itself. It does nothing before
// start or after stop.
func (s *scheduler) rearm() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.started || s.stopped {
		return
	}
	if s.running {
		select {
		case s.wake <- struct{}{}:
		default:
		}
		return
	}
	if IntervalOf(s.config()) == 0 {
		return
	}
	// A signal left over from an actor that has exited must not restart the period of
	// the new one.
	select {
	case <-s.wake:
	default:
	}
	s.running = true
	s.done = make(chan struct{})
	go s.actor(s.done)
}

// actor is the goroutine that owns the ticker, the queue and the passes. It re-reads
// the interval at the top of every loop and exits when it is zero or after stop; the
// decision is made under the lock rearm uses, so a rearm either reaches a running
// actor or starts a new one, never neither.
func (s *scheduler) actor(done chan struct{}) {
	defer close(done)
	var (
		ticker *time.Ticker
		period time.Duration
	)
	defer func() {
		if ticker != nil {
			ticker.Stop()
		}
	}()
	reset := true
	for {
		s.mu.Lock()
		d := IntervalOf(s.config())
		if s.stopped || d == 0 {
			s.running = false
			s.mu.Unlock()
			return
		}
		s.mu.Unlock()

		if reset || d != period {
			if ticker == nil {
				ticker = time.NewTicker(d)
			} else {
				ticker.Reset(d)
			}
			period, reset = d, false
		}
		select {
		case <-ticker.C:
			s.onTick()
		case <-s.wake:
			reset = true // clearInterval + setInterval: the period starts again
		case <-s.quit:
			s.mu.Lock()
			s.running = false
			s.mu.Unlock()
			return
		}
	}
}

// onTick is one interval tick: skip when stopped, disabled or when nothing is dirty,
// else collect the sessions that still have unprocessed messages and drain them.
func (s *scheduler) onTick() {
	s.mu.Lock()
	if s.stopped || len(s.dirty) == 0 || IntervalOf(s.config()) == 0 {
		s.mu.Unlock()
		return
	}
	snapshot := append([]string(nil), s.dirty...)
	lifetime := s.lifetime
	s.busy = make(chan struct{})
	busy := s.busy
	s.mu.Unlock()

	defer func() {
		s.mu.Lock()
		s.busy = nil
		s.mu.Unlock()
		close(busy)
	}()

	s.collect(lifetime, snapshot)
	s.drain(lifetime)
}

// collect walks the dirty sessions in insertion order: a session that is already
// queued is skipped, one with unprocessed messages is queued, and one without leaves
// the dirty set (it comes back with its next finished turn), unless a turn of that
// session finished since the tick started to look at it. A failure ends the walk after
// it was logged; the sessions queued so far still run.
func (s *scheduler) collect(ctx context.Context, snapshot []string) {
	defer func() {
		if r := recover(); r != nil {
			s.logger.Error("memory: consolidation tick panicked", "agent", s.agentID, "panic", redact.Panic(r))
		}
	}()
	for _, sessionID := range snapshot {
		if s.isQueued(sessionID) {
			continue
		}
		// Read before the counts: a turn that ends after this point bumps it.
		before := s.generation(sessionID)
		pending, err := s.pendingCount(ctx, sessionID)
		if err != nil {
			s.logger.Error("memory: consolidation tick failed", "agent", s.agentID, "error", redactForLog(err))
			return
		}
		if pending > 0 {
			s.enqueue(sessionID)
		} else {
			s.removeDirtyIf(sessionID, before)
		}
	}
}

// pendingCount is the number of messages of the session that no pass processed yet.
func (s *scheduler) pendingCount(ctx context.Context, sessionID string) (int, error) {
	total, err := s.messages.Count(ctx, sessionID)
	if err != nil {
		return 0, err
	}
	done, err := s.checkpoint(ctx, sessionID)
	if err != nil {
		return 0, err
	}
	return max(0, total-done), nil
}

func (s *scheduler) isQueued(sessionID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, q := range s.queue {
		if q == sessionID {
			return true
		}
	}
	return false
}

func (s *scheduler) enqueue(sessionID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopped {
		return
	}
	s.queue = append(s.queue, sessionID)
}

// generation is the number of finished turns of the session seen so far.
func (s *scheduler) generation(sessionID string) uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.gen[sessionID]
}

// removeDirtyIf takes the session out of the dirty set unless a turn of it finished
// after the generation was read (the session then has messages the tick did not
// count and stays dirty for the next tick). Only the actor removes sessions, so a
// generation cannot be removed and re-created between the read and this call.
func (s *scheduler) removeDirtyIf(sessionID string, generation uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.gen[sessionID] != generation {
		return
	}
	delete(s.gen, sessionID)
	if _, ok := s.dirtySet[sessionID]; !ok {
		return
	}
	delete(s.dirtySet, sessionID)
	for i, id := range s.dirty {
		if id == sessionID {
			s.dirty = append(s.dirty[:i:i], s.dirty[i+1:]...)
			break
		}
	}
}

// next pops the head of the queue. It returns false when the queue is empty or the
// scheduler was stopped: a stop never lets a queued session start.
func (s *scheduler) next() (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopped || len(s.queue) == 0 {
		return "", false
	}
	id := s.queue[0]
	s.queue = s.queue[1:]
	return id, true
}

// drain runs the queued sessions one after the other.
func (s *scheduler) drain(ctx context.Context) {
	for {
		sessionID, ok := s.next()
		if !ok {
			return
		}
		s.runPass(ctx, sessionID)
	}
}

// runPass runs one pass under recover. A failed or panicking pass is logged, with the
// agent and session ids and the error (or, for a panic, the type of the panic value
// through internal/redact, never the value itself), never a memory value, and its
// session stays dirty so the next tick retries it.
//
// The pass runs under the lifetime context bounded by passTimeout, so a model or a
// store that hangs fails the pass after that time instead of blocking every later tick
// and the wait of Dispose.
func (s *scheduler) runPass(ctx context.Context, sessionID string) {
	if s.passTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, s.passTimeout)
		defer cancel()
	}
	defer func() {
		if r := recover(); r != nil {
			s.logger.Error("memory: consolidation panicked", "agent", s.agentID, "session", sessionID, "panic", redact.Panic(r))
		}
	}()
	results, err := s.consolidate(ctx, sessionID)
	if err != nil {
		s.logger.Error("memory: consolidation failed", "agent", s.agentID, "session", sessionID, "error", redactForLog(err))
		return
	}
	if len(results) > 0 {
		s.logger.Info("memory: consolidated", "agent", s.agentID, "session", sessionID, "consolidated", len(results))
	}
}

// waitIdle waits until no tick is being evaluated and no pass is running. It returns
// the ctx error when ctx ends first.
func (s *scheduler) waitIdle(ctx context.Context) error {
	for {
		s.mu.Lock()
		busy := s.busy
		s.mu.Unlock()
		if busy == nil {
			return nil
		}
		select {
		case <-busy:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// stop unsubscribes, drops the queue, lets the actor exit and waits for a pass that is
// running, then for the goroutine itself. It never starts a final pass. When ctx ends
// first it returns the ctx error and the pass keeps running; calling stop again waits
// again.
func (s *scheduler) stop(ctx context.Context) error {
	s.mu.Lock()
	var unsubs []events.Unsubscribe
	if !s.stopped {
		s.stopped = true
		unsubs, s.unsubs = s.unsubs, nil
		s.queue = nil
		close(s.quit)
	}
	done := s.done
	s.mu.Unlock()
	for _, unsubscribe := range unsubs {
		unsubscribe()
	}

	if err := s.waitIdle(ctx); err != nil {
		return err
	}
	if done == nil {
		return nil
	}
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
