package agent

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/Phong95/aec-agent-go/components/agent/job"
	"github.com/Phong95/aec-agent-go/components/persistence"
)

// ---------------------------------------------------------------------------
// The clock of schedule jobs (A-126) - what was the promptJobs scheduler (A-69), the
// JobClock of core/ts.
//
// It schedules one agent's prompt jobs (A-69): it decides WHEN each job runs - the
// runner from the host decides HOW.
//
//   - One wall-clock timer (time.AfterFunc) per enabled job; Go timers have no
//     setTimeout overflow, so a far run time needs no stepping.
//   - start resumes stored jobs after a restart. A job whose time passed while the host
//     was down runs once ("catch-up") - never one run per missed slot.
//   - The next run time is saved before a run starts, so a crash mid-run does not
//     repeat it on the next start.
//
// Update (A-126): the clock only decides WHEN. At a job's time it saves the next run
// time, then hands the tick to the job manager (onTick), which puts it in the job's
// inbox like any other event - deduped on the scheduled time, so a time runs at most
// once even when two processes (or a restart) see it. Overlap is checked there: a tick
// while a run is still queued or running is recorded as a suppressed event (it was
// "skipped"). Runs are recorded by the job manager's worker.
// ---------------------------------------------------------------------------

// jobClockDeps is what a jobClock needs.
type jobClockDeps struct {
	agentID string
	store   persistence.JobStore
	// onTick is called when a schedule job is due: scheduledAt is the slot (ISO) it is for.
	onTick func(j job.Job, scheduledAt string, cause job.RunCause)
	// loc is the location of a cron schedule that names no timezone.
	loc *time.Location
	// lifetime is the agent's lifetime context; the ticks run under it.
	lifetime context.Context
	logger   *slog.Logger
	now      func() time.Time
}

// jobClock arms one timer per enabled schedule job. The timer callbacks are counted in
// wg; close stops the timers and waits for the callbacks. It is safe for concurrent use.
type jobClock struct {
	d jobClockDeps

	mu      sync.Mutex
	timers  map[string]*time.Timer
	started bool
	closed  bool
	wg      sync.WaitGroup
}

func newJobClock(d jobClockDeps) *jobClock {
	return &jobClock{d: d, timers: map[string]*time.Timer{}}
}

// start loads the agent's schedule jobs and arms their timers. Jobs missed while down
// run once. Idempotent.
func (c *jobClock) start(ctx context.Context) error {
	c.mu.Lock()
	if c.started || c.closed {
		c.mu.Unlock()
		return nil
	}
	c.started = true
	c.mu.Unlock()

	now := c.d.now()
	jobs, err := c.d.store.ListByAgent(ctx, c.d.agentID)
	if err != nil {
		return err
	}
	for _, j := range jobs {
		if !j.Enabled || !j.When.IsSchedule() {
			continue
		}
		if j.NextRunAt == nil {
			// Enabled but never computed (or an at job that had already run): recompute
			// from now.
			next, ok, err := NextRun(j.When, now, c.d.loc)
			if err != nil {
				c.d.logger.Error(fmt.Sprintf("[jobs] agent=%s job=%q has an unreadable schedule: %v", c.d.agentID, j.Name, err))
				continue
			}
			patch := persistence.JobStatePatch{}
			if ok {
				patch.NextRunAt = ptr(isoTime(next))
			} else {
				patch.ClearNextRunAt, patch.Enabled = true, ptr(false)
			}
			if _, err := c.d.store.UpdateJobState(ctx, j.ID, patch); err != nil {
				return err
			}
			if ok {
				c.arm(j.ID, next, job.CauseSchedule)
			}
			continue
		}
		due, ok := parseISO(*j.NextRunAt)
		if !ok {
			c.d.logger.Error(fmt.Sprintf("[jobs] agent=%s job=%q has an unreadable next run time %q", c.d.agentID, j.Name, *j.NextRunAt))
			continue
		}
		cause := job.CauseSchedule
		if !due.After(now) {
			// Missed while the host was down: one catch-up run now.
			cause = job.CauseCatchUp
		}
		c.arm(j.ID, due, cause)
	}
	return nil
}

// close stops every timer; no tick fires afterwards (wait waits for the callbacks in
// progress).
func (c *jobClock) close() {
	c.mu.Lock()
	c.closed = true
	for id, t := range c.timers {
		t.Stop()
		delete(c.timers, id)
	}
	c.mu.Unlock()
}

// wait waits for the timer callbacks in progress.
func (c *jobClock) wait() { c.wg.Wait() }

// sync re-arms (or disarms) a job after it was created, edited or deleted (nil).
func (c *jobClock) sync(id string, j *job.Job) {
	c.disarm(id)
	c.mu.Lock()
	started := c.started && !c.closed
	c.mu.Unlock()
	if !started || j == nil || !j.Enabled || !j.When.IsSchedule() || j.NextRunAt == nil {
		return
	}
	if at, ok := parseISO(*j.NextRunAt); ok {
		c.arm(id, at, job.CauseSchedule)
	}
}

// firstRun is the next run time of a schedule from now (ISO), or "" when it never fires
// again.
func (c *jobClock) firstRun(w job.When) (string, error) {
	next, ok, err := NextRun(w, c.d.now(), c.d.loc)
	if err != nil || !ok {
		return "", err
	}
	return isoTime(next), nil
}

// arm (re)arms the job's timer to fire at `at` (at once when it has passed).
func (c *jobClock) arm(id string, at time.Time, cause job.RunCause) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return
	}
	if old, ok := c.timers[id]; ok {
		old.Stop()
	}
	var t *time.Timer
	// The callback takes c.mu before it reads t, and arm holds c.mu until t is stored,
	// so the callback always sees its own timer.
	t = time.AfterFunc(max(at.Sub(c.d.now()), 0), func() {
		c.mu.Lock()
		if c.closed || c.timers[id] != t {
			c.mu.Unlock()
			return
		}
		delete(c.timers, id)
		c.wg.Add(1)
		c.mu.Unlock()
		defer c.wg.Done()
		c.onTimer(id, cause)
	})
	c.timers[id] = t
}

// disarm stops the job's timer.
func (c *jobClock) disarm(id string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if t, ok := c.timers[id]; ok {
		t.Stop()
		delete(c.timers, id)
	}
}

func (c *jobClock) isClosed() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closed
}

// onTimer runs a timer-driven tick. A failure is logged, and the job is re-armed a
// minute later from its stored record, so one store hiccup does not stop it for good.
func (c *jobClock) onTimer(id string, cause job.RunCause) {
	ctx := c.d.lifetime
	if c.isClosed() {
		return
	}
	err := c.tick(ctx, id, cause)
	if err == nil || ctx.Err() != nil {
		return
	}
	// A failed tick must never stop the job silently. Re-arm from the stored record if we
	// can, so one store hiccup doesn't stop the job for good.
	c.d.logger.Error(fmt.Sprintf("[jobs] agent=%s job=%s tick failed: %v", c.d.agentID, id, err))
	latest, gerr := c.d.store.Get(ctx, id)
	if gerr != nil || latest == nil || !latest.Enabled || !latest.When.IsSchedule() {
		return
	}
	c.mu.Lock()
	_, armed := c.timers[id]
	c.mu.Unlock()
	if armed {
		return
	}
	retry := c.d.now().Add(time.Minute)
	if latest.NextRunAt != nil {
		if t, ok := parseISO(*latest.NextRunAt); ok && t.After(retry) {
			retry = t
		}
	}
	c.arm(id, retry, job.CauseSchedule)
}

// tick saves the next run time (armed first, so a crash mid-run does not repeat this
// one), then hands the tick to the job manager.
func (c *jobClock) tick(ctx context.Context, id string, cause job.RunCause) error {
	j, err := c.d.store.Get(ctx, id)
	if err != nil {
		return err
	}
	if j == nil || j.AgentID != c.d.agentID || !j.Enabled || !j.When.IsSchedule() || j.NextRunAt == nil {
		return nil
	}
	scheduledAt := *j.NextRunAt
	now := c.d.now()
	next, hasNext, err := c.nextAfterFire(j.When, scheduledAt, now)
	if err != nil {
		return err
	}
	// A one-off job is done once it has fired.
	oneOff := j.When.Kind == job.KindAt
	patch := persistence.JobStatePatch{ClearNextRunAt: !hasNext}
	if hasNext {
		patch.NextRunAt = ptr(isoTime(next))
	}
	if oneOff {
		patch.Enabled = ptr(false)
	}
	updated, err := c.d.store.UpdateJobState(ctx, j.ID, patch)
	if err != nil || updated == nil {
		return err
	}
	if hasNext && !oneOff {
		c.arm(j.ID, next, job.CauseSchedule)
	}
	c.d.onTick(*updated, scheduledAt, cause)
	return nil
}

// nextAfterFire is the next run time after a timer-driven fire at now. An every job
// keeps its cadence from the scheduled time, but never schedules into the past (no
// burst of runs after a pause).
func (c *jobClock) nextAfterFire(w job.When, scheduledAt string, now time.Time) (time.Time, bool, error) {
	if w.Kind == job.KindEvery {
		interval := time.Duration(w.Ms) * time.Millisecond
		scheduled := now
		if t, ok := parseISO(scheduledAt); ok {
			scheduled = t
		}
		next := scheduled.Add(interval)
		if !next.After(now) {
			next = now.Add(interval)
		}
		return next, true, nil
	}
	return NextRun(w, now, c.d.loc)
}
