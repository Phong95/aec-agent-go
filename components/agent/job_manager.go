package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"math/rand/v2"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Phong95/aec-agent-go/components/agent/internal/jobkit"
	"github.com/Phong95/aec-agent-go/components/agent/job"
	"github.com/Phong95/aec-agent-go/components/events"
	"github.com/Phong95/aec-agent-go/components/persistence"
	"github.com/Phong95/aec-agent-go/components/toolhub"
	"github.com/Phong95/aec-agent-go/internal/jsstr"
	"github.com/Phong95/aec-agent-go/internal/redact"
	"github.com/Phong95/aec-agent-go/kernel"
	"github.com/Phong95/aec-agent-go/kernel/ordered"
)

// maxJobRunOutputChars is the most of the agent's reply kept on a run record.
const maxJobRunOutputChars = 100_000

// maxToolErrorChars is the most of a failed tool's output put into the run's error
// (A-103; the session keeps all of it).
const maxToolErrorChars = 2_000

// errJobsClosed is returned by Create once the agent was disposed.
var errJobsClosed = job.NewError("The agent is stopped", job.CodeInvalidArgument, "")

// jobRunKey is the context key under which the ctx of a job run carries the manager
// that runs it, so Agent.Dispose called from inside a run does not wait for that run
// (it would wait for itself).
type jobRunKey struct{}

// midDot is U+00B7, built from its code point so this source stays ASCII-only.
var midDot = string(rune(0x00b7))

// jobManagerDeps is what a jobManager needs.
type jobManagerDeps struct {
	agentID  string
	bus      events.Bus
	store    persistence.JobStore
	sessions persistence.SessionStore
	// runner is resolved on every run: the host's runner, or the agent's default one.
	runner func() job.Runner
	// capacity is the agent's running turns and its cap (0 = none): job runs leave room
	// for chats. Nil means no cap.
	capacity func() (running, maxTurns int)
	// loc is the location of a cron schedule that names no timezone.
	loc *time.Location
	// lifetime is the agent's lifetime context; the worker and every run derive from it.
	lifetime context.Context
	logger   *slog.Logger
	// now is the clock and random the jitter source; tests replace them.
	now    func() time.Time
	random func() float64
	// pollInterval, leaseDuration, retryBase and maxParallel default to the job package
	// constants; tests shorten them.
	pollInterval  time.Duration
	leaseDuration time.Duration
	retryBase     time.Duration
	maxParallel   int

	// runTool (A-103) runs one tool call outside a turn, the way a turn's call runs (tool
	// policy, approval gate, retries, tracing) - the agent loop. empty reports that the
	// tool succeeded and printed nothing. Nil means the agent can't run tool jobs.
	runTool func(ctx context.Context, call kernel.ToolCall, sessionID string) (result kernel.ToolResult, empty bool)
	// checkTool (A-103) says why name can't be a tool job's tool for this agent (not one
	// of its tools, not schedulable); "" means it can. Nil means any tool may.
	checkTool func(name string) string
	// appendMessage (A-103) adds a message to a session - a tool job's call and its result.
	appendMessage func(ctx context.Context, sessionID string, m kernel.Message) error
}

// activeJobRun is a run this worker is working on.
type activeJobRun struct {
	// id, jobID and correlationID are fixed at launch (read without a lock).
	id            string
	jobID         string
	correlationID string
	// eventDepth is the depth of an event this run causes (see eventDepthOf); set before
	// the run's session is registered.
	eventDepth atomic.Int64
	// run is the latest stored copy; only the run's own goroutine touches it.
	run    job.Run
	cancel context.CancelFunc
	// stopRequested: a person stopped it (StopRun / Delete): it ends "stopped".
	stopRequested atomic.Bool
	// leaseLost: another worker took the lease, so this one writes nothing more.
	leaseLost atomic.Bool
	// mu guards deniedTools: the tools an approval was asked for - denied, so the run
	// ends "blocked".
	mu          sync.Mutex
	deniedTools []string
	// approvedTool and approvedArgs (A-103, guarded by mu) are, while a tool job's stored
	// call runs, the only call its approval handler approves (the tool and the canonical
	// JSON of its arguments).
	approvedTool string
	approvedArgs string
	// timedOut (A-103): the run went past Limits.MaxRunMs and was cancelled: it ends
	// "failed".
	timedOut atomic.Bool
}

// toolRunOutcome is what a tool job's attempt produced (A-103).
type toolRunOutcome struct {
	job.RunResult
	// toolError is the start of the tool's output when it failed ("" otherwise), and
	// toolFailed says that it failed.
	toolError  string
	toolFailed bool
	// followedUp: a follow-up turn ran; its reply is the output.
	followedUp bool
	// empty: the tool succeeded and printed nothing - a silent run.
	empty bool
}

// jobOrigin is where an event came from, when a job run caused it.
type jobOrigin struct {
	originRunID   string
	correlationID string
	depth         int
}

// jobManager is one agent's event triggers (A-70), the TriggerManager of core/ts: it
// takes events into the durable inbox and runs them as agent turns.
//
//   - Intake: the agent's own SDK events (pub-sub, never a direct call from the scheduler
//     or channels), signed webhooks (HandleWebhook) and test events go through one path:
//     filter -> loop guard -> TriggerStore.Accept (dedupe + coalesce into a run,
//     atomically).
//   - Worker: claims due runs with a lease (TriggerStore.Claim), so a run is worked by one
//     worker at a time and a run whose worker died is taken over when its lease expires.
//     Before a turn it re-checks the trigger (deleted, disabled, changed), stale
//     revisions, a resolved condition and the rate and token limits. The turn goes
//     through the host's Runner in a fresh session; per-session ordering and the agent's
//     turn cap apply as for any turn, and runs leave a turn free for people's chats.
//   - Results: the reply is checkpointed before the run is marked finished, so a crash in
//     between only re-records the result, it does not re-run the turn. Transient
//     failures retry with backoff within MaxAttempts and the run's deadline. Approvals
//     cannot be given in an unattended run: the tool is denied and the run ends
//     "blocked".
//
// Update (A-126): the jobManager (the JobManager of core/ts) - one agent's jobs of every
// kind. Schedule jobs' ticks (the jobClock), run now, bus events, webhooks and test
// events all go through the intake above, and one worker runs them. A run's session
// follows the job's SessionMode (fresh, or one reused session). A run's reply may carry
// a marker: [[SILENT]] (nothing to report: succeeded, never delivered) or [[FAILED]]
// <reason> (failed, not retried).
//
// The bus handlers, the poll loop, the wake timer, the claim loop and the runs are
// counted in wg; close cancels the runs and waits for them, so nothing outlives the
// agent. It is safe for concurrent use.
type jobManager struct {
	d jobManagerDeps
	// owner is this worker's lease identity.
	owner string
	clock *jobClock

	mu sync.Mutex
	// active maps a run id to the run this worker works on, bySession a session id to it.
	active      map[string]*activeJobRun
	bySession   map[string]*activeJobRun
	unsubscribe []events.Unsubscribe
	// queuing holds the schedule jobs whose overlap check and queueing are in progress:
	// a second tick or run now meanwhile sees the job as busy, so two fired at the same
	// moment cannot both pass the check (the slot is taken before any store call).
	queuing map[string]bool
	// wake is the timer of the soonest known due run, due at wakeTime.
	wake      *time.Timer
	wakeTime  time.Time
	stop      chan struct{}
	pumping   bool
	pumpAgain bool
	started   bool
	closed    bool
	wg        sync.WaitGroup
}

var _ job.Jobs = (*jobManager)(nil)

func newJobManager(d jobManagerDeps) *jobManager {
	if d.now == nil {
		d.now = time.Now
	}
	if d.random == nil {
		d.random = randomFloat
	}
	if d.logger == nil {
		d.logger = slog.Default()
	}
	if d.loc == nil {
		d.loc = time.Local
	}
	if d.pollInterval <= 0 {
		d.pollInterval = job.PollInterval
	}
	if d.leaseDuration <= 0 {
		d.leaseDuration = job.LeaseDuration
	}
	if d.retryBase <= 0 {
		d.retryBase = job.RetryBase
	}
	if d.maxParallel <= 0 {
		d.maxParallel = job.MaxParallelRuns
	}
	m := &jobManager{
		d: d, owner: "job-worker:" + kernel.NewID(),
		active: map[string]*activeJobRun{}, bySession: map[string]*activeJobRun{}, queuing: map[string]bool{}, stop: make(chan struct{}),
	}
	m.clock = newJobClock(jobClockDeps{
		agentID: d.agentID, store: d.store, loc: d.loc, lifetime: d.lifetime, logger: d.logger, now: d.now,
		onTick: m.onTick,
	})
	// The "event" source: the agent's own SDK events, by subscription only. A handler runs
	// on the emitter's goroutine, so the work goes to a goroutine of its own.
	if d.bus != nil {
		for _, kind := range jobBusKinds {
			m.unsubscribe = append(m.unsubscribe, d.bus.On(kind, func(e kernel.Event) {
				m.goTracked(func() { m.onBusEvent(e) })
			}))
		}
	}
	return m
}

// interactionFor is the InteractionHandler of a session while a job run works in it:
// nobody is there to answer, so approvals are denied (the run ends "blocked") and
// questions fail. Nil for any other session.
func (m *jobManager) interactionFor(sessionID string) toolhub.InteractionHandler {
	m.mu.Lock()
	entry, ok := m.bySession[sessionID]
	m.mu.Unlock()
	if !ok {
		return nil
	}
	return jobInteraction{entry: entry}
}

// jobInteraction is the InteractionHandler of one job run.
type jobInteraction struct{ entry *activeJobRun }

// RequestApproval denies the tool and remembers it, so the run ends "blocked".
// Update (A-103): a tool job's stored call was approved when the job was made - exactly
// that tool with exactly those arguments is approved, nothing else.
func (i jobInteraction) RequestApproval(_ context.Context, req toolhub.ApprovalRequest) (toolhub.ApprovalDecision, error) {
	i.entry.mu.Lock()
	if i.entry.approvedTool != "" && req.ToolName == i.entry.approvedTool && canonicalJSON(json.RawMessage(req.Arguments)) == i.entry.approvedArgs {
		i.entry.mu.Unlock()
		reason := "approved with the job"
		return toolhub.ApprovalDecision{Approved: true, Reason: &reason}, nil
	}
	if !slices.Contains(i.entry.deniedTools, req.ToolName) {
		i.entry.deniedTools = append(i.entry.deniedTools, req.ToolName)
	}
	i.entry.mu.Unlock()
	reason := "This run was started by a job and nobody is there to approve tools, so the tool is denied. Say in your reply which action needs approval."
	return toolhub.ApprovalDecision{Approved: false, Reason: &reason}, nil
}

// AskUser fails: nobody is there to answer.
func (jobInteraction) AskUser(context.Context, toolhub.ClarifyRequest) (toolhub.ClarifyResponse, error) {
	return toolhub.ClarifyResponse{}, errors.New("this run was started by a job and nobody is there to answer")
}

// ---------------------------------------------------------------------------
// Lifecycle
// ---------------------------------------------------------------------------

// start arms the agent's schedule jobs (a time missed while the host was down runs
// once) and begins working the queue: due runs, runs left by a previous process (their
// leases expire) and retries. Idempotent; after close it does nothing.
func (m *jobManager) start(ctx context.Context) error {
	m.mu.Lock()
	if m.started || m.closed {
		m.mu.Unlock()
		return nil
	}
	m.started = true
	m.wg.Go(m.pollLoop)
	m.mu.Unlock()
	if err := m.clock.start(ctx); err != nil {
		return err
	}
	m.pump() //nolint:contextcheck // the worker runs under the agent's lifetime, not the caller's ctx (10-SECURITY UF-3)
	return nil
}

// pollLoop pumps every poll interval until close.
func (m *jobManager) pollLoop() {
	ticker := time.NewTicker(m.d.pollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-m.stop:
			return
		case <-ticker.C:
			m.pump()
		}
	}
}

// close stops the clock, intake and the worker. Runs in progress are cancelled and
// given back to the queue, so they are taken up again (not lost, not counted as an
// attempt) after a restart. It waits for the work to end, or until ctx ends; wait=false
// skips the wait (Dispose called from inside a run of this agent).
func (m *jobManager) close(ctx context.Context, wait bool) error {
	m.clock.close()
	m.mu.Lock()
	if !m.closed {
		m.closed = true
		close(m.stop)
		if m.wake != nil {
			m.wake.Stop()
			m.wake = nil
		}
	}
	unsubscribe := m.unsubscribe
	m.unsubscribe = nil
	for _, entry := range m.active {
		entry.cancel()
	}
	m.mu.Unlock()
	for _, off := range unsubscribe {
		off()
	}
	if !wait {
		return nil
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		m.clock.wait()
		m.wg.Wait()
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (m *jobManager) isClosed() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.closed
}

// goTracked runs f in a goroutine counted in wg, unless the manager is closed.
func (m *jobManager) goTracked(f func()) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return
	}
	m.wg.Go(func() {
		defer func() {
			if r := recover(); r != nil {
				m.d.logger.Error("[jobs] an event handler panicked", "agent", m.d.agentID, "panic", redact.Panic(r))
			}
		}()
		f()
	})
}

// ---------------------------------------------------------------------------
// job.Jobs - configuration
// ---------------------------------------------------------------------------

// List returns the agent's jobs, oldest created first.
func (m *jobManager) List(ctx context.Context) ([]job.Job, error) {
	return m.d.store.ListByAgent(ctx, m.d.agentID)
}

// Get returns the agent's job, or nil and a nil error.
func (m *jobManager) Get(ctx context.Context, id string) (*job.Job, error) {
	j, err := m.d.store.Get(ctx, id)
	if err != nil || j == nil || j.AgentID != m.d.agentID {
		return nil, err
	}
	return j, nil
}

// Create validates the input and stores the job; a webhook job gets a secret, and an
// enabled schedule job is armed at once.
func (m *jobManager) Create(ctx context.Context, in job.CreateInput) (job.Job, error) {
	if m.isClosed() {
		return job.Job{}, errJobsClosed
	}
	now := m.d.now()
	j, err := validateCreateInput(in, now)
	if err != nil {
		return job.Job{}, err
	}
	if err := m.checkToolAction(j.Action, ""); err != nil {
		return job.Job{}, err
	}
	existing, err := m.d.store.ListByAgent(ctx, m.d.agentID)
	if err != nil {
		return job.Job{}, err
	}
	if len(existing) >= job.MaxJobsPerAgent {
		return job.Job{}, job.NewError(fmt.Sprintf("This agent already has %d jobs %s delete one first", job.MaxJobsPerAgent, emDash), job.CodeLimitReached, "")
	}
	for _, e := range existing {
		if e.Name == j.Name {
			return job.Job{}, job.NewError(`A job named "`+j.Name+`" already exists`, job.CodeNameConflict, "")
		}
	}
	nowISO := isoTime(now)
	j.ID, j.AgentID, j.Version, j.CreatedAt, j.UpdatedAt = kernel.NewID(), m.d.agentID, 1, nowISO, nowISO
	if j.Enabled && j.When.IsSchedule() {
		next, err := m.clock.firstRun(j.When)
		if err != nil {
			return job.Job{}, err
		}
		if next != "" {
			j.NextRunAt = &next
		}
	}
	if j.When.Kind == job.KindWebhook {
		j.Secret = job.GenerateWebhookSecret()
	}
	if err := m.d.store.Save(ctx, j); err != nil {
		return job.Job{}, err
	}
	m.clock.sync(j.ID, &j)
	return j, nil
}

// Update applies a partial update; any change bumps the version. A new schedule, or a
// job turned back on, counts from now: there is no catch-up for the time it was off.
//
//nolint:gocyclo // one linear pass over the fields, the same steps as core/ts
func (m *jobManager) Update(ctx context.Context, id string, in job.UpdateInput) (job.Job, error) {
	t, err := m.require(ctx, id)
	if err != nil {
		return job.Job{}, err
	}
	next := t.Clone()
	if in.Name != nil {
		name, err := validateJobName(*in.Name, id)
		if err != nil {
			return job.Job{}, err
		}
		if name != t.Name {
			clash, err := m.d.store.FindByName(ctx, m.d.agentID, name)
			if err != nil {
				return job.Job{}, err
			}
			if clash != nil && clash.ID != id {
				return job.Job{}, job.NewError(`A job named "`+name+`" already exists`, job.CodeNameConflict, id)
			}
		}
		next.Name = name
	}
	if in.Description != nil {
		description, err := validateJobDescription(*in.Description, id)
		if err != nil {
			return job.Job{}, err
		}
		next.Description = description
	}
	if in.Enabled != nil {
		next.Enabled = *in.Enabled
	}
	if in.SessionMode != nil {
		if err := validateSessionMode(*in.SessionMode, id); err != nil {
			return job.Job{}, err
		}
		next.SessionMode = *in.SessionMode
	}
	now := m.d.now()
	whenChanged := in.When != nil
	if whenChanged {
		w, err := validateJobWhen(*in.When, now, id)
		if err != nil {
			return job.Job{}, err
		}
		next.When = w
	}
	// A-103: the action is checked against the (possibly new) when, and the prompt
	// against the action (a tool job needs a prompt only for its follow-up).
	actionChanged := in.Action != nil || in.ClearAction
	if actionChanged || whenChanged {
		a := t.Action
		switch {
		case in.ClearAction:
			a = nil
		case in.Action != nil:
			a = in.Action
		}
		action, err := validateJobAction(a, next.When, id)
		if err != nil {
			return job.Job{}, err
		}
		next.Action = action
		if actionChanged {
			if err := m.checkToolAction(action, id); err != nil {
				return job.Job{}, err
			}
		}
	}
	if in.Prompt != nil || actionChanged {
		p := t.Prompt
		if in.Prompt != nil {
			p = *in.Prompt
		}
		prompt, err := validatePromptFor(next.Action, p, id)
		if err != nil {
			return job.Job{}, err
		}
		next.Prompt = prompt
	}
	kindChanged := next.When.IsSchedule() != t.When.IsSchedule()
	// The filter is checked against the (possibly new) when; a job that moves to a clock
	// drops its filter.
	if in.Filter != nil || whenChanged {
		f := in.Filter
		if f == nil {
			if next.When.IsSchedule() {
				f = &job.Filter{}
			} else {
				f = &t.Filter
			}
		}
		filter, err := validateJobFilter(f, next.When, id)
		if err != nil {
			return job.Job{}, err
		}
		next.Filter = filter
	}
	if in.Coalesce != nil {
		if next.Coalesce, err = resolveCoalesce(in.Coalesce, t.Coalesce, id); err != nil {
			return job.Job{}, err
		}
	}
	// A job that changes between a clock and events takes the other kind's default limits.
	baseLimits := t.Limits
	if kindChanged {
		baseLimits = job.DefaultLimits(next.When)
	}
	if in.Limits != nil || kindChanged {
		if next.Limits, err = resolveLimits(in.Limits, baseLimits, id); err != nil {
			return job.Job{}, err
		}
	}
	// A-70 agent tools: ClearDelivery (the wire's null) removes the delivery.
	// Update (A-127): ClearDeliveries (or an empty list) removes them all.
	switch {
	case in.ClearDeliveries || (in.Deliveries != nil && len(in.Deliveries) == 0):
		next.Deliveries = nil
	case in.Deliveries != nil:
		if next.Deliveries, err = validateJobDeliveries(in.Deliveries, id); err != nil {
			return job.Job{}, err
		}
	}
	if next.When.Kind == job.KindWebhook && next.Secret == "" {
		next.Secret = job.GenerateWebhookSecret()
	}
	if next.When.Kind != job.KindWebhook {
		next.Secret = ""
	}

	switch {
	case !next.Enabled || !next.When.IsSchedule():
		next.NextRunAt = nil
	case whenChanged || !t.Enabled || next.NextRunAt == nil:
		at, err := m.clock.firstRun(next.When)
		if err != nil {
			return job.Job{}, err
		}
		if at == "" {
			return job.Job{}, jobInvalid(job.Describe(next.When)+" has no future run time", id)
		}
		next.NextRunAt = &at
	}
	next.Version = t.Version + 1
	next.UpdatedAt = isoTime(now)
	if err := m.d.store.Save(ctx, next); err != nil {
		return job.Job{}, err
	}
	m.clock.sync(id, &next)
	// Re-enabled: queued runs may be due.
	if next.Enabled && !t.Enabled {
		m.pump() //nolint:contextcheck // the worker runs under the agent's lifetime, not the caller's ctx: a request must not cancel the runs it queued (10-SECURITY UF-3)
	}
	return next, nil
}

// checkToolAction (A-103): a tool job's tool must be one of the agent's tools and marked
// schedulable.
func (m *jobManager) checkToolAction(a *job.Action, id string) error {
	if a == nil || a.Kind != job.ActionTool {
		return nil
	}
	if m.d.runTool == nil {
		return jobInvalid("This agent can't run tool jobs", id)
	}
	if m.d.checkTool != nil {
		if problem := m.d.checkTool(a.Tool); problem != "" {
			return jobInvalid(problem, id)
		}
	}
	return nil
}

// Delete removes the job with its inbox and run history and stops its runs.
func (m *jobManager) Delete(ctx context.Context, id string) error {
	if _, err := m.require(ctx, id); err != nil {
		return err
	}
	m.clock.sync(id, nil)
	m.mu.Lock()
	for _, entry := range m.active {
		if entry.jobID == id {
			entry.stopRequested.Store(true)
			entry.cancel()
		}
	}
	m.mu.Unlock()
	return m.d.store.Delete(ctx, id)
}

// RotateSecret gives a webhook job a new secret.
func (m *jobManager) RotateSecret(ctx context.Context, id string) (job.Job, error) {
	t, err := m.require(ctx, id)
	if err != nil {
		return job.Job{}, err
	}
	if t.When.Kind != job.KindWebhook {
		return job.Job{}, jobInvalid("Only a webhook job has a secret", id)
	}
	t.Secret = job.GenerateWebhookSecret()
	t.UpdatedAt = isoTime(m.d.now())
	if err := m.d.store.Save(ctx, t); err != nil {
		return job.Job{}, err
	}
	return t, nil
}

// ---------------------------------------------------------------------------
// job.Jobs - intake
// ---------------------------------------------------------------------------

// takeSlot marks a schedule job as being queued; false when it already is.
func (m *jobManager) takeSlot(id string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.queuing[id] {
		return false
	}
	m.queuing[id] = true
	return true
}

func (m *jobManager) releaseSlot(id string) {
	m.mu.Lock()
	delete(m.queuing, id)
	m.mu.Unlock()
}

// RunNow queues a manual run of a schedule job without moving its schedule. It works on
// a disabled job too.
func (m *jobManager) RunNow(ctx context.Context, id string) (job.Run, error) {
	t, err := m.require(ctx, id)
	if err != nil {
		return job.Run{}, err
	}
	if m.isClosed() {
		return job.Run{}, job.NewError("The agent is stopped", job.CodeInvalidArgument, id)
	}
	if !t.When.IsSchedule() {
		return job.Run{}, jobInvalid("Run now is for schedule jobs "+emDash+" send a test event to an event or webhook job", id)
	}
	busy := jobInvalid("A run of this job is already queued or running", id)
	// Never overlap a job with itself.
	if !m.takeSlot(id) {
		return job.Run{}, busy
	}
	defer m.releaseSlot(id)
	active, err := m.d.store.HasActiveRun(ctx, id)
	if err != nil {
		return job.Run{}, err
	}
	if active {
		return job.Run{}, busy
	}
	nowISO := isoTime(m.d.now())
	result, err := m.ingest(ctx, t, jobEnvelope{source: "manual", sourceEventID: kernel.NewID(), typ: "job.run-now", time: nowISO, data: []byte("null")}, job.CauseManual, nil, "", "")
	if err != nil {
		return job.Run{}, err
	}
	if result.Run == nil {
		return job.Run{}, jobInvalid("The run was not queued: "+cmpOr(result.Event.Reason, string(result.Event.Status)), id)
	}
	return *result.Run, nil
}

// SendTestEvent accepts a hand-made event through the same filter and inbox.
func (m *jobManager) SendTestEvent(ctx context.Context, id string, in job.TestEventInput) (job.AcceptResult, error) {
	t, err := m.require(ctx, id)
	if err != nil {
		return job.AcceptResult{}, err
	}
	if t.When.IsSchedule() {
		return job.AcceptResult{}, jobInvalid("A schedule job takes no events "+emDash+" use run now", id)
	}
	typ := jsstr.Trim(in.Type)
	if typ == "" || jobkit.UTF16Len(typ) > 200 {
		return job.AcceptResult{}, jobInvalid(`A test event needs a "type" (at most 200 characters)`, id)
	}
	var subject string
	if in.Subject != "" {
		s, ok := jobkit.ToSubject(in.Subject)
		if !ok {
			return job.AcceptResult{}, jobInvalid(`"subject" must be a non-empty string`, id)
		}
		subject = s
	}
	if in.Revision != nil && (math.IsNaN(*in.Revision) || math.IsInf(*in.Revision, 0)) {
		return job.AcceptResult{}, jobInvalid(`"revision" must be a number`, id)
	}
	raw, size := dataBytes(in.Data)
	if size > job.MaxEventDataBytes {
		return job.AcceptResult{}, jobInvalid(`"data" is larger than `+strconv.Itoa(job.MaxEventDataBytes/1024)+` KB`, id)
	}
	var revision *float64
	if in.Revision != nil {
		r := *in.Revision
		revision = &r
	}
	return m.ingest(ctx, t, jobEnvelope{
		source: "manual", sourceEventID: kernel.NewID(), typ: typ, subject: subject, revision: revision, data: raw,
	}, job.CauseTest, nil, "", "")
}

// HandleWebhook verifies, parses and accepts a webhook delivery.
func (m *jobManager) HandleWebhook(ctx context.Context, id string, req job.WebhookRequest) (job.WebhookResponse, error) {
	if !strings.EqualFold(req.Method, "POST") {
		return webhookError(405, "Use POST"), nil
	}
	t, err := m.Get(ctx, id)
	if err != nil {
		return job.WebhookResponse{}, err
	}
	if t == nil || m.isClosed() {
		return webhookError(404, "Job not found"), nil
	}
	if t.When.Kind != job.KindWebhook {
		return webhookError(404, "This job takes no webhooks"), nil
	}
	if !t.Enabled {
		return webhookError(409, "The job is disabled"), nil
	}
	if t.Secret == "" {
		return webhookError(503, "The job has no secret"), nil
	}
	if len(req.Body) > job.MaxWebhookBodyBytes {
		return webhookError(413, "The body is larger than "+strconv.Itoa(job.MaxWebhookBodyBytes/1024)+" KB"), nil
	}

	// The signature is checked on the raw bytes, before anything is parsed.
	if invalid := job.VerifyWebhookSignature(t.When.Signature, t.Secret, req, m.d.now()); invalid != "" {
		return webhookError(401, "Unauthorized: "+invalid), nil
	}
	env, invalid := parseWebhookEvent(t.When, req)
	if invalid != "" {
		return webhookError(400, invalid), nil
	}
	if len(env.data) > job.MaxEventDataBytes {
		return webhookError(413, "The event data is larger than "+strconv.Itoa(job.MaxEventDataBytes/1024)+" KB"), nil
	}

	// Returns only once the store has the event: a store failure is an error, so the host
	// answers 5xx and the sender keeps the delivery.
	result, err := m.ingest(ctx, *t, env, job.CauseWebhook, nil, "", "")
	if err != nil {
		return job.WebhookResponse{}, err
	}
	body := ordered.NewObject()
	if result.Duplicate {
		body.Set("duplicate", true)
		body.Set("eventId", result.Event.ID)
		return job.WebhookResponse{Status: 200, Body: body}, nil
	}
	body.Set("eventId", result.Event.ID)
	body.Set("status", string(result.Event.Status))
	if result.Run != nil {
		body.Set("runId", result.Run.ID)
	}
	if result.Event.Reason != "" {
		body.Set("reason", result.Event.Reason)
	}
	return job.WebhookResponse{Status: 202, Body: body}, nil
}

func webhookError(status int, message string) job.WebhookResponse {
	body := ordered.NewObject()
	body.Set("error", message)
	return job.WebhookResponse{Status: status, Body: body}
}

// ---------------------------------------------------------------------------
// job.Jobs - history and control
// ---------------------------------------------------------------------------

// ListEvents returns the job's events, newest first.
func (m *jobManager) ListEvents(ctx context.Context, id string, limit int) ([]job.Event, error) {
	if _, err := m.require(ctx, id); err != nil {
		return nil, err
	}
	return m.d.store.ListEvents(ctx, id, clampJobLimit(limit))
}

// ListRuns returns the job's runs, newest first.
func (m *jobManager) ListRuns(ctx context.Context, id string, limit int) ([]job.Run, error) {
	if _, err := m.require(ctx, id); err != nil {
		return nil, err
	}
	return m.d.store.ListRuns(ctx, id, clampJobLimit(limit))
}

// GetRun returns the agent's run, or nil and a nil error.
func (m *jobManager) GetRun(ctx context.Context, runID string) (*job.Run, error) {
	run, err := m.d.store.GetRun(ctx, runID)
	if err != nil || run == nil || run.AgentID != m.d.agentID {
		return nil, err
	}
	return run, nil
}

// GetRunEvents returns the inbox events a run handles.
func (m *jobManager) GetRunEvents(ctx context.Context, runID string) ([]job.Event, error) {
	run, err := m.requireRun(ctx, runID)
	if err != nil {
		return nil, err
	}
	return m.d.store.GetEvents(ctx, run.EventIDs)
}

// StopRun stops a running or queued run; false when it had already ended.
func (m *jobManager) StopRun(ctx context.Context, runID string) (bool, error) {
	run, err := m.requireRun(ctx, runID)
	if err != nil {
		return false, err
	}
	m.mu.Lock()
	entry, ok := m.active[runID]
	if ok {
		entry.stopRequested.Store(true)
		entry.cancel()
	}
	m.mu.Unlock()
	if ok {
		return true, nil
	}
	if run.Status != job.RunPending && run.Status != job.RunWaiting {
		return false, nil
	}
	nowISO := isoTime(m.d.now())
	const reason = "Stopped before it started"
	stopped, err := m.d.store.UpdateRun(ctx, runID, "", persistence.JobRunPatch{
		Status: ptr(job.RunStopped), FinishedAt: &nowISO, Reason: ptr(reason),
	}, nowISO)
	if err != nil {
		return false, err
	}
	if stopped != nil {
		m.emitFinished(ctx, *stopped, job.RunStopped, "", reason, nil, false)
	}
	return stopped != nil, nil
}

// ReplayRun queues a new run over the same events.
func (m *jobManager) ReplayRun(ctx context.Context, runID string) (job.Run, error) {
	original, err := m.requireRun(ctx, runID)
	if err != nil {
		return job.Run{}, err
	}
	if original.Status == job.RunPending || original.Status == job.RunWaiting || original.Status == job.RunRunning {
		return job.Run{}, jobInvalid("The run has not finished yet", original.JobID)
	}
	t, err := m.require(ctx, original.JobID)
	if err != nil {
		return job.Run{}, err
	}
	now := m.d.now()
	nowISO := isoTime(now)
	replay := job.Run{
		ID: kernel.NewID(), JobID: t.ID, AgentID: m.d.agentID, JobVersion: t.Version, Cause: job.CauseReplay,
		BucketKey: original.BucketKey, Status: job.RunWaiting,
		// The same events: replay keeps their identity.
		EventIDs: slices.Clone(original.EventIDs), Revision: original.Revision,
		CorrelationID: original.CorrelationID, Depth: original.Depth,
		NotBefore: nowISO, Deadline: nowISO, ExpiresAt: isoTime(now.Add(ms(t.Limits.RunDeadlineMs))),
		Attempts: 0, MaxAttempts: t.Limits.MaxAttempts, ReplayOf: original.ID,
		CreatedAt: nowISO, UpdatedAt: nowISO,
	}
	if err := m.d.store.InsertRun(ctx, replay); err != nil {
		return job.Run{}, err
	}
	m.pump() //nolint:contextcheck // the worker runs under the agent's lifetime, not the caller's ctx: a request must not cancel the runs it queued (10-SECURITY UF-3)
	return replay, nil
}

// ---------------------------------------------------------------------------
// Intake
// ---------------------------------------------------------------------------

// onTick is a schedule job's time (the jobClock): one tick into its inbox. Never overlap
// a job with itself: a tick while a run is still queued or running is recorded as
// suppressed (it was "skipped").
func (m *jobManager) onTick(j job.Job, scheduledAt string, cause job.RunCause) {
	if m.isClosed() {
		return
	}
	ctx := m.d.lifetime
	taken := !m.takeSlot(j.ID)
	if !taken {
		defer m.releaseSlot(j.ID)
	}
	busy := taken
	if !busy {
		active, err := m.d.store.HasActiveRun(ctx, j.ID)
		if err != nil {
			m.d.logger.Error(fmt.Sprintf("[jobs] agent=%s job=%s could not check its runs: %v", m.d.agentID, j.ID, err))
			return
		}
		busy = active
	}
	suppress := ""
	if busy {
		suppress = "The previous run was still in progress"
	}
	data := ordered.NewObject()
	data.Set("scheduledAt", scheduledAt)
	raw, _ := jobkit.Marshal(data)
	env := jobEnvelope{source: "schedule", sourceEventID: scheduledAt, typ: "job.tick", time: scheduledAt, data: raw}
	if _, err := m.ingest(ctx, j, env, cause, nil, "", suppress); err != nil {
		m.d.logger.Error(fmt.Sprintf("[jobs] agent=%s job=%s could not take its tick: %v", m.d.agentID, j.ID, err))
	}
}

// onBusEvent offers one of the agent's SDK events to every enabled "event" job.
func (m *jobManager) onBusEvent(e kernel.Event) {
	if m.isClosed() {
		return
	}
	env, ok := internalEnvelope(m.d.agentID, e)
	if !ok {
		return
	}
	ctx := m.d.lifetime
	jobs, err := m.d.store.ListByAgent(ctx, m.d.agentID)
	if err != nil {
		m.d.logger.Error(fmt.Sprintf("[jobs] agent=%s could not take event %s: %v", m.d.agentID, e.Type, err))
		return
	}
	// Only jobs that asked for this type see it - no inbox entry for every job run.
	var interested []job.Job
	for _, t := range jobs {
		if !t.Enabled || t.When.Kind != job.KindEvent {
			continue
		}
		for _, p := range t.Filter.Types {
			if job.TypeMatches(p, env.typ) {
				interested = append(interested, t)
				break
			}
		}
	}
	if len(interested) == 0 {
		return
	}
	origin := m.originOf(ctx, e.SessionID)
	for _, t := range interested {
		if _, err := m.ingest(ctx, t, env, job.CauseEvent, origin, e.ID, ""); err != nil {
			m.d.logger.Error(fmt.Sprintf("[jobs] agent=%s could not take event %s: %v", m.d.agentID, e.Type, err))
			return
		}
	}
}

// ingest is the single intake path: build the inbox entry, filter it, apply the loop
// guard, and accept it into the store (dedupe + coalesce, atomically). suppress records
// the event as suppressed for that reason (a busy schedule job).
//
//nolint:gocyclo // one linear pass, the same steps as core/ts
func (m *jobManager) ingest(ctx context.Context, t job.Job, env jobEnvelope, cause job.RunCause, origin *jobOrigin, causationID, suppress string) (job.AcceptResult, error) {
	now := m.d.now()
	nowISO := isoTime(now)
	event := job.Event{
		ID: kernel.NewID(), JobID: t.ID, AgentID: m.d.agentID, Source: env.source, SourceEventID: env.sourceEventID,
		Type: env.typ, Subject: env.subject, Revision: env.revision, Time: env.time, ReceivedAt: nowISO,
		Data: env.data, CorrelationID: kernel.NewID(), CausationID: causationID, Status: job.EventAccepted,
	}
	if event.Time == "" {
		event.Time = nowISO
	}
	if event.Data == nil {
		event.Data = []byte("null")
	}
	if origin != nil {
		event.CorrelationID, event.OriginRunID, event.Depth = origin.correlationID, origin.originRunID, origin.depth
	}

	schedule := t.When.IsSchedule()
	mismatch := ""
	if !schedule {
		mismatch = job.FilterMismatch(t.Filter, event)
	}
	switch {
	case suppress != "":
		event.Status, event.Reason = job.EventSuppressed, suppress
	case mismatch != "":
		event.Status, event.Reason = job.EventFiltered, mismatch
	case event.Depth > t.Limits.MaxDepth:
		// Loop guard: output of an event job's run does not wake a job unless allowed (MaxDepth).
		event.Status = job.EventSuppressed
		event.Reason = fmt.Sprintf("caused by job run %s (chain depth %d is over the limit of %d)", event.OriginRunID, event.Depth, t.Limits.MaxDepth)
	}

	in := persistence.AcceptJobEventInput{Event: event}
	if event.Status == job.EventAccepted {
		bucketKey := event.Subject
		// A clock tick or run now is due at once and never coalesces with another one.
		coalesce := t.Coalesce
		if schedule {
			coalesce = job.Coalesce{QuietMs: 0, MaxWaitMs: 0, MaxBatch: 1}
		}
		notBefore := now.Add(ms(coalesce.QuietMs))
		if t.Limits.CooldownMs > 0 {
			last, err := m.d.store.LastSucceeded(ctx, t.ID, bucketKey)
			if err != nil {
				return job.AcceptResult{}, err
			}
			if last != nil {
				if finished, ok := parseISO(last.FinishedAt); ok {
					if cool := finished.Add(ms(t.Limits.CooldownMs)); cool.After(notBefore) {
						notBefore = cool
					}
				}
			}
		}
		deadline := now.Add(ms(coalesce.MaxWaitMs))
		if notBefore.After(deadline) {
			deadline = notBefore
		}
		in.Run = &persistence.JobRunPlan{
			NewRun: job.Run{
				ID: kernel.NewID(), JobID: t.ID, AgentID: m.d.agentID, JobVersion: t.Version, Cause: cause,
				BucketKey: bucketKey, Status: job.RunPending, EventIDs: []string{event.ID}, Revision: event.Revision,
				CorrelationID: event.CorrelationID, Depth: event.Depth,
				NotBefore: isoTime(notBefore), Deadline: isoTime(deadline), ExpiresAt: isoTime(notBefore.Add(ms(t.Limits.RunDeadlineMs))),
				Attempts: 0, MaxAttempts: t.Limits.MaxAttempts, CreatedAt: nowISO, UpdatedAt: nowISO,
			},
			JoinNotBefore: isoTime(now.Add(ms(coalesce.QuietMs))),
			MaxBatch:      coalesce.MaxBatch,
		}
	}

	result, err := m.d.store.Accept(ctx, in)
	if err != nil {
		return job.AcceptResult{}, err
	}
	status := string(result.Event.Status)
	if result.Duplicate {
		status = "duplicate"
	}
	payload := events.JobEventReceivedPayload{
		AgentID: m.d.agentID, JobID: t.ID, EventID: result.Event.ID, Type: result.Event.Type,
		Status: status, Reason: result.Event.Reason,
	}
	if result.Run != nil {
		payload.RunID = result.Run.ID
	}
	events.Emit(m.d.bus, events.JobEventReceived, "", payload)
	switch {
	case result.Run != nil:
		if at, ok := parseISO(result.Run.NotBefore); ok {
			m.wakeAt(at) //nolint:contextcheck // the worker runs under the agent's lifetime, not the caller's ctx: a request must not cancel the runs it queued (10-SECURITY UF-3)
		}
	case !result.Duplicate:
		// Filtered and suppressed events start no run (whose end prunes), so they prune here.
		if err := m.d.store.Prune(ctx, t.ID, job.KeepEvents, job.KeepRuns); err != nil {
			m.d.logger.Warn(fmt.Sprintf("[jobs] agent=%s could not prune history: %v", m.d.agentID, err))
		}
	}
	return result, nil
}

// originOf is, when sessionID is a job run's session, the run and chain depth an event
// from it continues; nil otherwise.
func (m *jobManager) originOf(ctx context.Context, sessionID string) *jobOrigin {
	if sessionID == "" {
		return nil
	}
	m.mu.Lock()
	entry, ok := m.bySession[sessionID]
	m.mu.Unlock()
	if ok {
		return &jobOrigin{originRunID: entry.id, correlationID: entry.correlationID, depth: int(entry.eventDepth.Load())}
	}
	session, err := m.d.sessions.Get(ctx, sessionID)
	if err != nil || session == nil {
		return nil
	}
	meta := session.Metadata
	source, _ := meta.Get("source")
	runID, _ := meta.Get("runId")
	run, isString := runID.(string)
	if source != "job" || !isString {
		return nil
	}
	origin := &jobOrigin{originRunID: run, correlationID: kernel.NewID(), depth: 0}
	if c, ok := meta.Get("correlationId"); ok {
		if s, ok := c.(string); ok {
			origin.correlationID = s
		}
	}
	if d, ok := meta.Get("eventDepth"); ok {
		if n, ok := jobkit.Number(d); ok {
			origin.depth = int(n)
		}
	}
	return origin
}

// ---------------------------------------------------------------------------
// Worker
// ---------------------------------------------------------------------------

// wakeAt arms a wake-up for a run that becomes due at `at` (only the soonest is kept).
func (m *jobManager) wakeAt(at time.Time) {
	m.mu.Lock()
	if !m.started || m.closed {
		m.mu.Unlock()
		return
	}
	now := m.d.now()
	if !at.After(now) {
		m.mu.Unlock()
		m.pump()
		return
	}
	if m.wake != nil && !m.wakeTime.After(at) {
		m.mu.Unlock()
		return
	}
	if m.wake != nil {
		m.wake.Stop()
	}
	var t *time.Timer
	// The callback takes m.mu before it reads t, and wakeAt holds m.mu until t is stored,
	// so the callback always sees its own timer.
	t = time.AfterFunc(at.Sub(now), func() {
		m.mu.Lock()
		if m.closed || m.wake != t {
			m.mu.Unlock()
			return
		}
		m.wake = nil
		m.wg.Add(1)
		m.mu.Unlock()
		defer m.wg.Done()
		m.pump()
	})
	m.wake, m.wakeTime = t, at
	m.mu.Unlock()
}

// pump claims and starts due runs while there is room, in a goroutine of its own.
// Re-entrant calls coalesce: a pump asked for while one runs runs once more after it.
func (m *jobManager) pump() {
	m.mu.Lock()
	if !m.started || m.closed {
		m.mu.Unlock()
		return
	}
	if m.pumping {
		m.pumpAgain = true
		m.mu.Unlock()
		return
	}
	m.pumping = true
	m.wg.Add(1)
	m.mu.Unlock()
	go func() {
		defer m.wg.Done()
		for {
			if err := m.claimLoop(); err != nil {
				m.d.logger.Error(fmt.Sprintf("[jobs] agent=%s could not claim runs: %v", m.d.agentID, err))
			}
			m.mu.Lock()
			if m.pumpAgain && !m.closed {
				m.pumpAgain = false
				m.mu.Unlock()
				continue
			}
			m.pumping, m.pumpAgain = false, false
			m.mu.Unlock()
			return
		}
	}()
}

// claimLoop claims due runs and launches them while there is room.
func (m *jobManager) claimLoop() error {
	ctx := m.d.lifetime
	for m.hasRoom() {
		// Jobs already at their own concurrency limit are skipped this round.
		jobs, err := m.d.store.ListByAgent(ctx, m.d.agentID)
		if err != nil {
			return err
		}
		running := map[string]int{}
		m.mu.Lock()
		for _, entry := range m.active {
			running[entry.jobID]++
		}
		m.mu.Unlock()
		var exclude []string
		for _, t := range jobs {
			if running[t.ID] >= t.Limits.MaxConcurrentRuns {
				exclude = append(exclude, t.ID)
			}
		}
		now := m.d.now()
		run, err := m.d.store.Claim(ctx, persistence.ClaimJobRunInput{
			AgentID: m.d.agentID, Now: isoTime(now), Owner: m.owner,
			LeaseUntil: isoTime(now.Add(m.d.leaseDuration)), ExcludeJobIDs: exclude,
		})
		if err != nil || run == nil {
			return err
		}
		m.launch(*run)
	}
	return nil
}

// hasRoom says whether one more run may start: under this manager's cap, and a turn left
// for people's chats.
func (m *jobManager) hasRoom() bool {
	m.mu.Lock()
	full := m.closed || len(m.active) >= m.d.maxParallel
	m.mu.Unlock()
	if full {
		return false
	}
	if m.d.capacity == nil {
		return true
	}
	running, maxTurns := m.d.capacity()
	switch {
	case maxTurns <= 0:
		return true
	case maxTurns == 1:
		return running == 0
	default:
		return running < maxTurns-1
	}
}

// launch works a claimed run in a goroutine counted in wg (the caller, the claim loop,
// is counted too, so the Add is safe while close waits).
func (m *jobManager) launch(run job.Run) {
	runCtx, cancel := context.WithCancel(context.WithValue(m.d.lifetime, jobRunKey{}, m))
	entry := &activeJobRun{id: run.ID, jobID: run.JobID, correlationID: run.CorrelationID, run: run, cancel: cancel}
	m.mu.Lock()
	m.active[run.ID] = entry
	if m.closed {
		// Closed meanwhile: the run starts cancelled and is given back at once.
		cancel()
	}
	m.wg.Add(1)
	m.mu.Unlock()
	go func() {
		defer m.wg.Done()
		defer func() { //nolint:contextcheck // the worker runs under the agent's lifetime, not the caller's ctx: a request must not cancel the runs it queued (10-SECURITY UF-3)
			if r := recover(); r != nil {
				m.d.logger.Error("[jobs] a run failed unexpectedly", "agent", m.d.agentID, "run", run.ID, "panic", redact.Panic(r))
			}
			m.mu.Lock()
			delete(m.active, run.ID)
			for sid, e := range m.bySession {
				if e == entry {
					delete(m.bySession, sid)
				}
			}
			m.mu.Unlock()
			cancel()
			m.pump()
		}()
		m.process(runCtx, entry)
	}()
}

// eventDepthOf is the depth of an event a run causes. Only event and webhook jobs chain:
// an event from their run is one level deeper than the run, so with MaxDepth 0 it wakes
// no event job. A schedule job's run (and run now) starts no chain: its events have
// depth 0, so "when my scheduled job fails, look into it" works.
func eventDepthOf(t job.Job, run job.Run) int {
	if t.When.IsSchedule() {
		return 0
	}
	return run.Depth + 1
}

// process works one claimed run to its next state.
//
//nolint:gocyclo // one linear pass over the run's checks, the same steps as core/ts
func (m *jobManager) process(runCtx context.Context, entry *activeJobRun) {
	// The store calls outlive a stop of the run: a stopped or given-back run is still
	// recorded.
	ctx := context.WithoutCancel(runCtx)
	run := entry.run
	t, err := m.d.store.Get(ctx, run.JobID)
	if err != nil {
		m.d.logger.Error(fmt.Sprintf("[jobs] agent=%s run=%s could not read its job: %v", m.d.agentID, run.ID, err))
		return
	}
	if t == nil || t.AgentID != m.d.agentID {
		m.finish(entry, job.RunSuppressed, finishFields{reason: "The job was deleted"}, nil)
		return
	}
	// Run now works on a disabled job too (as before A-126), and a one-off ("at") job turns
	// itself off as its time fires; anything else waits for the job to be enabled.
	oneOffTick := t.When.Kind == job.KindAt && (run.Cause == job.CauseSchedule || run.Cause == job.CauseCatchUp)
	if !t.Enabled && run.Cause != job.CauseManual && !oneOffTick {
		m.finish(entry, job.RunSuppressed, finishFields{reason: "The job is disabled"}, t)
		return
	}
	entry.eventDepth.Store(int64(eventDepthOf(*t, run)))

	// The turn already finished before a crash: only record the result.
	if run.ResultAt != "" {
		m.finalize(entry, t)
		return
	}

	if run.Attempts > run.MaxAttempts {
		m.finish(entry, job.RunFailed, finishFields{errText: fmt.Sprintf("Gave up after %d attempts%s", run.MaxAttempts, suffix(run.Error))}, t)
		return
	}
	now := m.d.now()
	if expires, ok := parseISO(run.ExpiresAt); ok && now.After(expires) {
		m.finish(entry, job.RunFailed, finishFields{errText: "The run's deadline passed" + suffix(run.Error)}, t)
		return
	}

	// -- Is the work still wanted? --
	evs, err := m.d.store.GetEvents(ctx, run.EventIDs)
	if err != nil {
		m.d.logger.Error(fmt.Sprintf("[jobs] agent=%s run=%s could not read its events: %v", m.d.agentID, run.ID, err))
		return
	}
	if run.JobVersion != t.Version && !t.When.IsSchedule() {
		// The job was edited after these events were queued: keep the ones it still accepts.
		evs = slices.DeleteFunc(evs, func(e job.Event) bool { return job.FilterMismatch(t.Filter, e) != "" })
	}
	if len(evs) == 0 {
		m.finish(entry, job.RunSuppressed, finishFields{reason: "The job no longer accepts these events"}, t)
		return
	}
	if run.Revision != nil {
		last, err := m.d.store.LastSucceeded(ctx, t.ID, run.BucketKey)
		if err != nil {
			m.d.logger.Error(fmt.Sprintf("[jobs] agent=%s run=%s could not read the last run: %v", m.d.agentID, run.ID, err))
			return
		}
		if last != nil && last.ID != run.ID && last.Revision != nil && *last.Revision >= *run.Revision {
			m.finish(entry, job.RunSuppressed, finishFields{reason: "Revision " + jobkit.FormatNumber(*run.Revision) +
				" is not newer than revision " + jobkit.FormatNumber(*last.Revision) + ", already handled"}, t)
			return
		}
	}
	resolved, err := resolvedSince(ctx, evs, m.d.store)
	if err != nil {
		m.d.logger.Error(fmt.Sprintf("[jobs] agent=%s run=%s could not check the job: %v", m.d.agentID, run.ID, err))
		return
	}
	if resolved != "" {
		m.finish(entry, job.RunSuppressed, finishFields{reason: "Resolved: " + resolved}, t)
		return
	}

	// -- Limits --
	if hourly := t.Limits.MaxRunsPerHour; hourly != nil {
		hour, err := m.d.store.UsageSince(ctx, t.ID, isoTime(now.Add(-time.Hour)))
		if err != nil {
			m.d.logger.Error(fmt.Sprintf("[jobs] agent=%s run=%s could not read its usage: %v", m.d.agentID, run.ID, err))
			return
		}
		if hour.Runs >= *hourly {
			window := max(60_000, (hourMs+*hourly-1) / *hourly)
			retryAt := now.Add(ms(window))
			// Waiting for the window is not an attempt.
			m.write(entry, persistence.JobRunPatch{
				Status: ptr(job.RunWaiting), NotBefore: ptr(isoTime(retryAt)), Attempts: ptr(max(0, run.Attempts-1)),
				Reason:       ptr(fmt.Sprintf("Rate limit: %d runs in the last hour (limit %d)", hour.Runs, *hourly)),
				ReleaseLease: true,
			})
			m.wakeAt(retryAt) //nolint:contextcheck // the worker runs under the agent's lifetime, not the caller's ctx: a request must not cancel the runs it queued (10-SECURITY UF-3)
			return
		}
	}
	if t.Limits.MaxTokensPerDay != nil {
		day, err := m.d.store.UsageSince(ctx, t.ID, isoTime(now.Add(-24*time.Hour)))
		if err != nil {
			m.d.logger.Error(fmt.Sprintf("[jobs] agent=%s run=%s could not read its usage: %v", m.d.agentID, run.ID, err))
			return
		}
		if day.Tokens >= *t.Limits.MaxTokensPerDay {
			m.finish(entry, job.RunBlocked, finishFields{reason: fmt.Sprintf("Token budget used up: %d of %d tokens in the last 24 hours", day.Tokens, *t.Limits.MaxTokensPerDay)}, t)
			return
		}
	}

	// -- Run the turn --
	// A-103: a tool job runs its stored call (and maybe a follow-up turn) instead of a turn.
	var toolAction *job.Action
	if t.Action != nil && t.Action.Kind == job.ActionTool {
		toolAction = t.Action
	}
	// A-103: "since last run" placeholders read the history before this run joins it.
	var history *job.RunHistory
	if job.UsesRunHistory(t.Prompt) {
		history = m.runHistory(ctx, t.ID, run.ID)
	}
	sessionID := run.SessionID
	if sessionID == "" {
		if sessionID, err = m.resolveSession(ctx, *t, run, int(entry.eventDepth.Load())); err != nil {
			m.d.logger.Error(fmt.Sprintf("[jobs] agent=%s run=%s could not create its session: %v", m.d.agentID, run.ID, err))
			return
		}
	}
	patch := persistence.JobRunPatch{SessionID: &sessionID, Clear: []persistence.JobRunField{persistence.JobRunReason}}
	if run.StartedAt == "" {
		patch.StartedAt = ptr(isoTime(now))
	}
	if toolAction != nil {
		patch.Action = ptr(job.ActionTool)
	}
	started := m.write(entry, patch)
	if started == nil {
		return
	}
	run = *started
	m.mu.Lock()
	m.bySession[sessionID] = entry
	m.mu.Unlock()
	m.setJobState(t.ID, persistence.JobStatePatch{LastRunAt: ptr(run.StartedAt), LastStatus: ptr(job.RunRunning)})
	events.Emit(m.d.bus, events.JobRunStarted, sessionID, events.JobRunStartedPayload{
		AgentID: m.d.agentID, JobID: t.ID, JobName: t.Name, RunID: run.ID, Cause: string(run.Cause), Attempt: run.Attempts,
	})
	m.d.logger.Info(fmt.Sprintf("[jobs] agent=%s job=%q run=%s cause=%s attempt=%d events=%d session=%s", m.d.agentID, t.Name, run.ID, run.Cause, run.Attempts, len(evs), sessionID))

	stopRenew := m.renewWhileRunning(entry)
	// A-103: no attempt runs longer than the job's time limit.
	maxRunMs := job.DefaultMaxRunMs
	if t.Limits.MaxRunMs != nil {
		maxRunMs = *t.Limits.MaxRunMs
	}
	timeLimit := time.AfterFunc(ms(maxRunMs), func() {
		entry.timedOut.Store(true)
		entry.cancel()
	})
	rc := job.RunContext{
		AgentID: m.d.agentID, JobID: t.ID, JobName: t.Name, RunID: run.ID, Cause: run.Cause, Attempt: run.Attempts,
		SessionID: sessionID, Prompt: job.RenderPromptWithHistory(*t, evs, history),
	}
	var outcome toolRunOutcome
	var failure error
	if toolAction != nil {
		outcome, failure = m.runToolAction(runCtx, entry, *toolAction, rc)
	} else {
		outcome.RunResult, failure = m.callRunner(runCtx, rc)
	}
	timeLimit.Stop()
	entry.mu.Lock()
	entry.approvedTool, entry.approvedArgs = "", ""
	entry.mu.Unlock()
	stopRenew()
	result := outcome.RunResult

	if entry.leaseLost.Load() {
		m.d.logger.Warn(fmt.Sprintf("[jobs] agent=%s run=%s lost its lease; another worker has it", m.d.agentID, run.ID))
		return
	}
	closed := m.isClosed()
	// A-103: past its time limit - a failure, not a person's stop, and not retried.
	if entry.timedOut.Load() && !entry.stopRequested.Load() && !closed {
		m.finish(entry, job.RunFailed, finishFields{
			errText: "Timed out after " + describeDuration(maxRunMs) + " (the job's time limit)",
			output:  jobkit.SliceUTF16(result.Output, maxJobRunOutputChars),
		}, t)
		return
	}
	// A-103: the tool failed and no follow-up turn ran - the run failed with the start of
	// the tool's output (the session keeps all of it). Retried like any failed attempt.
	toolErrText := ""
	if toolAction != nil && outcome.toolFailed {
		toolErrText = toolAction.Tool + " failed: " + jobkit.SliceUTF16(outcome.toolError, maxToolErrorChars)
	}
	if failure == nil && outcome.toolFailed && !outcome.followedUp {
		failure = errors.New(toolErrText)
	}
	// Stopped here (StopRun / Delete) or through the host (its stop button for the session).
	if entry.stopRequested.Load() || (failure == nil && result.Aborted && !closed) {
		m.finish(entry, job.RunStopped, finishFields{reason: "Stopped by a person", output: jobkit.SliceUTF16(result.Output, maxJobRunOutputChars)}, t)
		return
	}
	// A turn that finished before the shutdown keeps its result (recorded below), so it is not run again.
	finishedTurn := failure == nil && !result.Aborted
	if closed && runCtx.Err() != nil && !finishedTurn {
		// Shut down mid-run: give the run back so it is taken up after the restart.
		m.write(entry, persistence.JobRunPatch{
			Status: ptr(job.RunWaiting), NotBefore: ptr(isoTime(m.d.now())), Attempts: ptr(max(0, run.Attempts-1)),
			Reason: ptr("Interrupted: the agent stopped during the run"), ReleaseLease: true,
		})
		return
	}

	if failure == nil && !result.Aborted {
		entry.mu.Lock()
		denied := slices.Clone(entry.deniedTools)
		entry.mu.Unlock()
		// Checkpoint the result first: if the process dies now, a reclaim only records it.
		checkpoint := persistence.JobRunPatch{ResultAt: ptr(isoTime(m.d.now())), Clear: []persistence.JobRunField{persistence.JobRunError}}
		if result.Output != "" {
			checkpoint.Output = ptr(jobkit.SliceUTF16(result.Output, maxJobRunOutputChars))
		}
		if result.Usage != nil {
			checkpoint.Usage = &job.Usage{InputTokens: result.Usage.InputTokens, OutputTokens: result.Usage.OutputTokens}
		}
		if result.StopReason != "" {
			checkpoint.StopReason = &result.StopReason
		}
		if len(denied) > 0 {
			checkpoint.Reason = ptr("Needs approval: " + strings.Join(denied, ", ") + " (approvals cannot be given in a run started by a job)")
		}
		// A-103: a tool that printed nothing - a silent run (finalize).
		if outcome.empty {
			checkpoint.Silent = ptr(true)
		}
		// A-103: a follow-up turn after a failed tool - the run stays failed (finalize).
		if outcome.toolFailed {
			checkpoint.Error = &toolErrText
			checkpoint.Clear = nil
		}
		if m.write(entry, checkpoint) == nil {
			return
		}
		m.finalize(entry, t)
		return
	}

	errText := "The turn ended without a result"
	if failure != nil {
		errText = failure.Error()
	}
	// An SDK error says whether retrying can help; anything else (a network error...) is
	// treated as transient.
	var sdkErr *kernel.SDKError
	retryable := failure == nil || !errors.As(failure, &sdkErr) || sdkErr.Retryable
	retryAt := m.d.now().Add(m.retryDelay(run.Attempts))
	expires, _ := parseISO(run.ExpiresAt)
	if retryable && run.Attempts < run.MaxAttempts && retryAt.Before(expires) {
		m.write(entry, persistence.JobRunPatch{Status: ptr(job.RunWaiting), NotBefore: ptr(isoTime(retryAt)), Error: &errText, ReleaseLease: true})
		events.Emit(m.d.bus, events.JobRunRetrying, sessionID, events.JobRunRetryingPayload{
			AgentID: m.d.agentID, JobID: t.ID, JobName: t.Name, RunID: run.ID, Attempt: run.Attempts, Error: errText, RetryAt: isoTime(retryAt),
		})
		m.d.logger.Warn(fmt.Sprintf("[jobs] agent=%s run=%s attempt %d failed, retrying at %s: %s", m.d.agentID, run.ID, run.Attempts, isoTime(retryAt), errText))
		m.wakeAt(retryAt) //nolint:contextcheck // the worker runs under the agent's lifetime, not the caller's ctx: a request must not cancel the runs it queued (10-SECURITY UF-3)
		return
	}
	m.finish(entry, job.RunFailed, finishFields{errText: errText}, t)
}

// renewWhileRunning renews the run's lease every third of the lease (at least every
// second) until the returned stop is called. A renewal that finds the lease taken marks
// the run as lost and cancels it.
func (m *jobManager) renewWhileRunning(entry *activeJobRun) (stop func()) {
	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Go(func() {
		ticker := time.NewTicker(max(time.Second, m.d.leaseDuration/3))
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				held, err := m.d.store.RenewLease(m.d.lifetime, entry.id, m.owner, isoTime(m.d.now().Add(m.d.leaseDuration)))
				if err == nil && !held && entry.leaseLost.CompareAndSwap(false, true) {
					entry.cancel()
				}
			}
		}
	})
	return func() {
		close(done)
		wg.Wait()
	}
}

// callRunner calls the runner under recover.
func (m *jobManager) callRunner(ctx context.Context, rc job.RunContext) (result job.RunResult, err error) {
	defer func() {
		if r := recover(); r != nil {
			result, err = job.RunResult{}, errors.New("panic: "+redact.Panic(r))
		}
	}()
	runner := m.d.runner()
	if runner == nil {
		return job.RunResult{}, errors.New("no job runner is configured")
	}
	return runner(ctx, rc)
}

// finishFields are the fields finish records with the status.
type finishFields struct {
	errText     string
	reason      string
	output      string
	deliveredAt string
	silent      bool
}

// finalize records the checkpointed result as the run's outcome; the in-app delivery is
// this same write.
// Update (A-126): the reply's marker decides it - [[SILENT]] succeeds without anything
// to deliver, [[FAILED]] <reason> fails (a deliberate answer, so it is not retried).
func (m *jobManager) finalize(entry *activeJobRun, t *job.Job) {
	deliveredAt := isoTime(m.d.now())
	if entry.run.Reason != "" {
		m.finish(entry, job.RunBlocked, finishFields{deliveredAt: deliveredAt}, t)
		return
	}
	tool := entry.run.Action == job.ActionTool
	switch outcome := job.ReadReply(entry.run.Output); {
	case outcome.Kind == job.OutcomeSilent:
		m.finish(entry, job.RunSucceeded, finishFields{deliveredAt: deliveredAt, silent: true}, t)
	case outcome.Kind == job.OutcomeFailed:
		m.finish(entry, job.RunFailed, finishFields{deliveredAt: deliveredAt, errText: outcome.Reason}, t)
	// A-103: a tool job whose tool failed stays failed after its follow-up turn; the reply
	// (e.g. "what went wrong and how to fix it") is the error people are told.
	case tool && entry.run.Error != "":
		errText := entry.run.Error
		if reply := jsstr.Trim(entry.run.Output); reply != "" {
			errText = jobkit.SliceUTF16(reply, maxJobRunOutputChars)
		}
		m.finish(entry, job.RunFailed, finishFields{deliveredAt: deliveredAt, errText: errText}, t)
	// A-103: a tool job that printed nothing has nothing to report, like [[SILENT]].
	case tool && (entry.run.Silent || jsstr.Trim(entry.run.Output) == ""):
		m.finish(entry, job.RunSucceeded, finishFields{deliveredAt: deliveredAt, silent: true}, t)
	default:
		m.finish(entry, job.RunSucceeded, finishFields{deliveredAt: deliveredAt}, t)
	}
}

// finish ends the run with status, records the job's last status, publishes it, and
// prunes the job's history. known is the job as read by the run (nil when unknown).
func (m *jobManager) finish(entry *activeJobRun, status job.RunStatus, f finishFields, known *job.Job) {
	patch := persistence.JobRunPatch{Status: &status, FinishedAt: ptr(isoTime(m.d.now())), ReleaseLease: true}
	if f.errText != "" {
		patch.Error = &f.errText
	}
	if f.reason != "" {
		patch.Reason = &f.reason
	}
	if f.output != "" {
		patch.Output = &f.output
	}
	if f.deliveredAt != "" {
		patch.DeliveredAt = &f.deliveredAt
	}
	if f.silent {
		patch.Silent = ptr(true)
	}
	done := m.write(entry, patch)
	if done == nil {
		return
	}
	// A suppressed run did no work: the job's last status stays what it was.
	paused := false
	if status != job.RunSuppressed {
		lastErr := f.errText
		if lastErr == "" && status == job.RunBlocked {
			lastErr = done.Reason
		}
		// A-103: too many failed runs in a row turn the job off.
		streak := 0
		if status == job.RunFailed || status == job.RunBlocked {
			streak = m.failureStreak(done.JobID)
		}
		paused = streak > 0
		if paused {
			lastErr = fmt.Sprintf("Paused after %d failed runs in a row%s", streak, suffix(lastErr))
		}
		state := persistence.JobStatePatch{LastStatus: ptr(status)}
		if lastErr != "" {
			state.LastError = &lastErr
		} else {
			state.ClearLastError = true
		}
		if paused {
			state.Enabled = ptr(false)
			state.ClearNextRunAt = true
		}
		m.setJobState(done.JobID, state)
		if paused {
			if off, err := m.d.store.Get(m.d.lifetime, done.JobID); err == nil && off != nil {
				m.clock.sync(off.ID, off)
			}
			m.d.logger.Warn(fmt.Sprintf("[jobs] agent=%s job=%s paused after %d failed runs in a row", m.d.agentID, done.JobID, streak))
		}
	}
	m.emitFinished(m.d.lifetime, *done, status, f.errText, f.reason, known, paused)
	silent := ""
	if done.Silent {
		silent = " (silent)"
	}
	log := fmt.Sprintf("[jobs] agent=%s run=%s %s%s%s", m.d.agentID, done.ID, status, silent, suffix(cmpOr(f.errText, f.reason)))
	if status == job.RunFailed {
		m.d.logger.Error(log)
	} else {
		m.d.logger.Info(log)
	}
	if err := m.d.store.Prune(m.d.lifetime, done.JobID, job.KeepEvents, job.KeepRuns); err != nil {
		m.d.logger.Warn(fmt.Sprintf("[jobs] agent=%s could not prune history: %v", m.d.agentID, err))
	}
}

func (m *jobManager) emitFinished(ctx context.Context, run job.Run, status job.RunStatus, errText, reason string, known *job.Job, paused bool) {
	// Update (A-70 agent tools): the payload also carries the trigger's name, the run's
	// output and the trigger's delivery, which the channel manager sends. The trigger as
	// it is now: the user may have changed its delivery while the run went on. A trigger
	// deleted meanwhile (or a store that fails) has nothing to deliver to.
	t, err := m.d.store.Get(ctx, run.JobID)
	if err != nil {
		t = nil
	}
	p := events.JobRunFinishedPayload{
		AgentID: m.d.agentID, JobID: run.JobID, RunID: run.ID, Cause: string(run.Cause), Status: string(status),
		// A blocked turn's reason was checkpointed on the run, not passed to finish().
		Error: cmpOr(errText, run.Error), Reason: cmpOr(reason, run.Reason),
		Output: run.Output, Silent: run.Silent,
		// A-103: what the run did, and whether it paused its job.
		Action: string(run.Action), Paused: paused,
	}
	switch {
	case t != nil:
		p.JobName = t.Name
		p.Deliveries = toEventDeliveries(t.Deliveries)
	case known != nil && known.ID == run.JobID:
		p.JobName = known.Name
	}
	events.Emit(m.d.bus, events.JobRunFinished, run.SessionID, p)
}

// runToolAction is one attempt of a tool job (A-103). The stored call runs through the
// agent loop like a turn's call (policy, approval gate, retries, traces); its approval
// handler approves exactly this call. The call and its result go into the run's
// session, so the session shows what ran and a follow-up turn sees the output. A
// follow-up turn (always, or on-failure when the tool failed) then runs through the
// host's runner as usual.
func (m *jobManager) runToolAction(ctx context.Context, entry *activeJobRun, a job.Action, rc job.RunContext) (out toolRunOutcome, err error) {
	defer func() {
		if r := recover(); r != nil {
			out, err = toolRunOutcome{}, errors.New("panic: "+redact.Panic(r))
		}
	}()
	if m.d.runTool == nil {
		return toolRunOutcome{}, job.NewError("This agent can't run tool jobs", job.CodeInvalidArgument, rc.JobID)
	}
	raw, err := jobkit.Marshal(a.Args)
	if err != nil || string(raw) == "null" {
		raw = []byte("{}")
	}
	call := kernel.ToolCall{ID: fmt.Sprintf("job_%s_%d", rc.RunID, rc.Attempt), Name: a.Tool, Arguments: kernel.Arguments(raw)}
	entry.mu.Lock()
	entry.approvedTool, entry.approvedArgs = a.Tool, canonicalJSON(a.Args)
	entry.mu.Unlock()
	// The writes outlive a stop of the run: a stopped call is still recorded.
	store := context.WithoutCancel(ctx)
	if m.d.appendMessage != nil {
		if err := m.d.appendMessage(store, rc.SessionID, kernel.Message{Role: kernel.RoleAssistant, Content: "", ToolCalls: []kernel.ToolCall{call}}); err != nil {
			return toolRunOutcome{}, err
		}
	}
	result, empty := m.d.runTool(ctx, call, rc.SessionID)
	entry.mu.Lock()
	entry.approvedTool, entry.approvedArgs = "", ""
	entry.mu.Unlock()
	// Every call gets its result in the session, a stopped one too (never the empty flag).
	if m.d.appendMessage != nil {
		stored := kernel.ToolResult{ToolCallID: result.ToolCallID, Content: result.Content}
		if result.IsError != nil && *result.IsError {
			stored.IsError = ptr(true)
		}
		// A-132: what the call produced for the user to open, so the chat can link to it
		stored.Link = result.Link
		if err := m.d.appendMessage(store, rc.SessionID, kernel.Message{Role: kernel.RoleAssistant, Content: "", ToolResults: []kernel.ToolResult{stored}}); err != nil {
			return toolRunOutcome{}, err
		}
	}
	if ctx.Err() != nil {
		return toolRunOutcome{RunResult: job.RunResult{Aborted: true, Output: result.Content}}, nil
	}
	failed := result.IsError != nil && *result.IsError
	toolErr := ""
	if failed {
		toolErr = cmpOr(result.Content, "the tool reported an error")
	}
	followUp := a.FollowUp
	if followUp == job.FollowUpAlways || (followUp == job.FollowUpOnFailure && failed) {
		reply, err := m.callRunner(ctx, rc)
		return toolRunOutcome{RunResult: reply, followedUp: true, toolFailed: failed, toolError: toolErr}, err
	}
	return toolRunOutcome{RunResult: job.RunResult{Output: result.Content}, toolFailed: failed, toolError: toolErr, empty: empty && !failed}, nil
}

// failureStreak is the number of finished runs in a row that failed or were blocked,
// when it has reached the job's PauseAfterFailures (0 otherwise, or when the job is
// already off). Succeeded runs end the streak; suppressed and stopped runs are skipped.
// Runs from before the job's last edit don't count, so turning it back on starts a fresh
// streak (A-103).
func (m *jobManager) failureStreak(jobID string) int {
	t, err := m.d.store.Get(m.d.lifetime, jobID)
	if err != nil || t == nil || !t.Enabled {
		return 0
	}
	limit := job.DefaultPauseAfterFailures
	if t.Limits.PauseAfterFailures != nil {
		limit = *t.Limits.PauseAfterFailures
	}
	if limit <= 0 {
		return 0
	}
	runs, err := m.d.store.ListRuns(m.d.lifetime, jobID, min(200, limit*4+10))
	if err != nil {
		return 0
	}
	streak := 0
	for _, r := range runs {
		if r.CreatedAt < t.UpdatedAt || r.Status == job.RunSucceeded {
			break
		}
		if r.Status == job.RunFailed || r.Status == job.RunBlocked {
			streak++
			if streak >= limit {
				return streak
			}
		}
	}
	return 0
}

// runHistory is the job's earlier runs, for the "since last run" placeholders (A-103).
func (m *jobManager) runHistory(ctx context.Context, jobID, currentRunID string) *job.RunHistory {
	h := &job.RunHistory{Now: isoTime(m.d.now())}
	runs, err := m.d.store.ListRuns(ctx, jobID, 50)
	if err != nil {
		return h
	}
	for _, r := range runs {
		if r.ID == currentRunID || r.StartedAt == "" {
			continue
		}
		if h.LastRunAt == "" && (r.Status == job.RunSucceeded || r.Status == job.RunFailed || r.Status == job.RunBlocked) {
			h.LastRunAt = r.StartedAt
		}
		if r.Status == job.RunSucceeded && !r.Silent && jsstr.Trim(r.Output) != "" {
			h.LastSuccessAt, h.LastResult, h.HasLastResult = r.StartedAt, r.Output, true
			break
		}
	}
	return h
}

// describeDuration is "45 seconds", "30 minutes", "2 hours" - for the time-limit error.
func describeDuration(n int) string {
	switch {
	case n < 60_000:
		return strconv.Itoa(int(math.Round(float64(n)/1000))) + " seconds"
	case n < hourMs || n%hourMs != 0:
		return strconv.Itoa(int(math.Round(float64(n)/60_000))) + " minutes"
	case n == hourMs:
		return "1 hour"
	default:
		return strconv.Itoa(n/hourMs) + " hours"
	}
}

// write is a lease-guarded write. Nil (and the entry marked) when this worker no longer
// holds the run; nil (logged) when the store failed.
func (m *jobManager) write(entry *activeJobRun, patch persistence.JobRunPatch) *job.Run {
	updated, err := m.d.store.UpdateRun(m.d.lifetime, entry.id, m.owner, patch, isoTime(m.d.now()))
	if err != nil {
		m.d.logger.Error(fmt.Sprintf("[jobs] agent=%s run=%s could not be written: %v", m.d.agentID, entry.id, err))
		return nil
	}
	if updated == nil {
		entry.leaseLost.Store(true)
		return nil
	}
	entry.run = *updated
	return updated
}

// setJobState records a job's run state; a failure is logged - the run's own record is
// what counts.
func (m *jobManager) setJobState(jobID string, patch persistence.JobStatePatch) {
	if _, err := m.d.store.UpdateJobState(m.d.lifetime, jobID, patch); err != nil {
		m.d.logger.Warn(fmt.Sprintf("[jobs] agent=%s job=%s could not record its state: %v", m.d.agentID, jobID, err))
	}
}

// resolveSession returns the session a run writes into - created on the first attempt
// and reused by retries. A "reuse" job appends every run to one session, created on its
// first run.
func (m *jobManager) resolveSession(ctx context.Context, t job.Job, run job.Run, eventDepth int) (string, error) {
	if t.SessionMode == job.SessionReuse && t.SessionID != "" {
		s, err := m.d.sessions.Get(ctx, t.SessionID)
		if err != nil {
			return "", err
		}
		if s != nil {
			if owner, ok := s.Metadata.Get("agentId"); ok && owner == m.d.agentID {
				// A session from before Session.JobID existed: tag it so it groups with the
				// job's runs.
				if s.JobID == "" {
					s.JobID = t.ID
					if err := m.d.sessions.Update(ctx, *s); err != nil {
						return "", err
					}
				}
				return s.ID, nil
			}
		}
	}
	title := t.Name
	if t.SessionMode != job.SessionReuse {
		title = t.Name + " " + midDot + " " + m.d.now().UTC().Format("2006-01-02 15:04") + " UTC"
	}
	meta := ordered.NewObject()
	// The same ownership key the host routes use, so the session lists and opens like any
	// other.
	meta.Set("agentId", m.d.agentID)
	meta.Set("source", "job")
	meta.Set("jobId", t.ID)
	meta.Set("jobName", t.Name)
	meta.Set("runId", run.ID)
	meta.Set("cause", string(run.Cause))
	meta.Set("correlationId", run.CorrelationID)
	// The depth of events this session's runs cause (the loop guard reads it after a restart).
	meta.Set("eventDepth", eventDepth)
	meta.Set("title", title)
	s, err := m.d.sessions.Create(ctx, meta)
	if err != nil {
		return "", err
	}
	// Tag the session with its job (Session.JobID) so clients can group a job's runs.
	s.JobID = t.ID
	if err := m.d.sessions.Update(ctx, s); err != nil {
		return "", err
	}
	if t.SessionMode == job.SessionReuse {
		m.setJobState(t.ID, persistence.JobStatePatch{SessionID: &s.ID})
	}
	return s.ID, nil
}

// retryDelay is the delay before the next attempt: RetryBase x 2^(attempt-1), +-25%
// jitter, at most MaxRetryDelay.
func (m *jobManager) retryDelay(attempt int) time.Duration {
	base := float64(m.d.retryBase.Milliseconds()) * math.Pow(2, float64(max(0, attempt-1)))
	delay := math.Round(base * (0.75 + m.d.random()*0.5))
	return min(job.MaxRetryDelay, time.Duration(delay)*time.Millisecond)
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func (m *jobManager) require(ctx context.Context, id string) (job.Job, error) {
	t, err := m.Get(ctx, id)
	if err != nil {
		return job.Job{}, err
	}
	if t == nil {
		return job.Job{}, job.NewError(`No job "`+id+`" for this agent`, job.CodeNotFound, id)
	}
	return *t, nil
}

func (m *jobManager) requireRun(ctx context.Context, runID string) (job.Run, error) {
	run, err := m.GetRun(ctx, runID)
	if err != nil {
		return job.Run{}, err
	}
	if run == nil {
		return job.Run{}, job.NewError(`No job run "`+runID+`" for this agent`, job.CodeNotFound, "")
	}
	return *run, nil
}

// toEventDelivery is the job's delivery as the job events carry it.
// Update (A-127): toEventDeliveries - all of them.
func toEventDeliveries(list job.Deliveries) []events.JobDelivery {
	var out []events.JobDelivery
	for _, d := range list {
		out = append(out, events.JobDelivery{ChannelID: d.ChannelID, Target: d.Target, ThreadID: d.ThreadID, When: string(d.When)})
	}
	return out
}

// clampJobLimit is a history page size: 0 (or less) means 50, at most 200.
func clampJobLimit(limit int) int {
	if limit <= 0 {
		return 50
	}
	return min(limit, 200)
}

// ms is n milliseconds.
func ms(n int) time.Duration { return time.Duration(n) * time.Millisecond }

// suffix is ": " + s, or "" when s is empty.
func suffix(s string) string {
	if s == "" {
		return ""
	}
	return ": " + s
}

// cmpOr is the first non-empty string.
func cmpOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func ptr[T any](v T) *T { return &v }

// randomFloat is the default jitter source.
func randomFloat() float64 {
	return rand.Float64() //nolint:gosec // retry jitter, not a secret
}
