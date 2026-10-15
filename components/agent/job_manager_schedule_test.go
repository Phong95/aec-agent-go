package agent_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Phong95/aec-agent-go/components/agent"
	"github.com/Phong95/aec-agent-go/components/agent/job"
	"github.com/Phong95/aec-agent-go/components/events"
	"github.com/Phong95/aec-agent-go/kernel"
)

// A-69 prompt jobs through Agent.Jobs: the scheduler, the default runner, and the
// lifecycle with Dispose.
// Update (A-126): schedule jobs through the job manager - the clock puts a tick in the
// job's inbox and the worker runs it; run now queues a run; reply markers.

// gateRunner is a Runner whose runs block until released (or until their ctx ends).
type gateRunner struct {
	mu      sync.Mutex
	calls   []job.RunContext
	ctxs    []context.Context
	release chan struct{}
}

func newGateRunner() *gateRunner { return &gateRunner{release: make(chan struct{})} }

func (g *gateRunner) run(ctx context.Context, rc job.RunContext) (job.RunResult, error) {
	g.mu.Lock()
	g.calls = append(g.calls, rc)
	g.ctxs = append(g.ctxs, ctx)
	g.mu.Unlock()
	select {
	case <-g.release:
		return job.RunResult{Output: "did " + rc.JobName, Usage: &job.Usage{InputTokens: 3, OutputTokens: 2}}, nil
	case <-ctx.Done():
		return job.RunResult{Aborted: true}, nil
	}
}

func (g *gateRunner) count() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.calls)
}

// instantRunner finishes every run at once.
func instantRunner(_ context.Context, rc job.RunContext) (job.RunResult, error) {
	return job.RunResult{Output: "did " + rc.JobName}, nil
}

// jobAgent is an agent whose jobs run through runner (nil: the default runner), with its
// jobs started.
func jobAgent(t *testing.T, e *env, id string, runner job.Runner) *agent.Agent {
	t.Helper()
	deps := e.deps()
	if runner != nil {
		deps.JobRunner = func() job.Runner { return runner }
	}
	a, err := agent.New(t.Context(), deps, testProfile(id))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.Dispose(context.WithoutCancel(t.Context())) })
	if err := a.StartJobs(t.Context()); err != nil {
		t.Fatal(err)
	}
	return a
}

// waitRun polls the job's runs until one matches.
func waitRun(t *testing.T, jobs job.Jobs, jobID string, match func(job.Run) bool) job.Run {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		runs, err := jobs.ListRuns(t.Context(), jobID, 50)
		if err != nil {
			t.Fatal(err)
		}
		if i := slices.IndexFunc(runs, match); i >= 0 {
			return runs[i]
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("no matching run of job %s", jobID)
	return job.Run{}
}

func done(r job.Run) bool {
	return r.Status != job.RunRunning && r.Status != job.RunPending && r.Status != job.RunWaiting
}

func everyHour() job.When {
	return job.When{Kind: job.KindEvery, Ms: 3_600_000}
}

func TestScheduleJobCreateValidatesAndRejects(t *testing.T) {
	e := newEnv(t)
	jobs := jobAgent(t, e, "a1", instantRunner).Jobs()
	ctx := t.Context()
	j, err := jobs.Create(ctx, job.CreateInput{Name: " digest ", Prompt: "Summarise open RFIs", When: job.When{Kind: job.KindCron, Expr: "0 8 * * *", TZ: "UTC"}})
	if err != nil {
		t.Fatal(err)
	}
	if j.Name != "digest" || !j.Enabled || j.SessionMode != job.SessionNew || j.CreatedBy != job.CreatedByUser || j.NextRunAt == nil ||
		j.Limits.MaxRunsPerHour != nil || j.Limits.MaxAttempts != 1 {
		t.Fatalf("job = %+v", j)
	}
	bad := []struct {
		in   job.CreateInput
		code job.ErrorCode
	}{
		{job.CreateInput{Name: "digest", Prompt: "p", When: everyHour()}, job.CodeNameConflict},
		{job.CreateInput{Name: "x", Prompt: "  ", When: everyHour()}, job.CodeInvalidArgument},
		{job.CreateInput{Name: "x", Prompt: "p", When: job.When{Kind: job.KindEvery, Ms: 10}}, job.CodeInvalidArgument},
		{job.CreateInput{Name: "x", Prompt: "p", When: everyHour(), SessionMode: "sometimes"}, job.CodeInvalidArgument},
		{job.CreateInput{Name: "x", Prompt: "p", When: everyHour(), Filter: &job.Filter{Types: []string{"job.failed"}}}, job.CodeInvalidArgument},
	}
	for _, c := range bad {
		if _, err := jobs.Create(ctx, c.in); !errors.Is(err, &job.Error{Code: c.code}) {
			t.Errorf("%+v: err = %v, want %s", c.in, err, c.code)
		}
	}
	if _, err := jobs.SendTestEvent(ctx, j.ID, job.TestEventInput{Type: "x"}); !errors.Is(err, &job.Error{Code: job.CodeInvalidArgument}) {
		t.Fatalf("a schedule job takes no test event: %v", err)
	}
	for range job.MaxJobsPerAgent - 1 {
		if _, err := jobs.Create(ctx, job.CreateInput{Name: "job-" + kernel.NewID(), Prompt: "p", When: everyHour(), Enabled: boolPtr(false)}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := jobs.Create(ctx, job.CreateInput{Name: "one-too-many", Prompt: "p", When: everyHour()}); !errors.Is(err, &job.Error{Code: job.CodeLimitReached}) {
		t.Fatalf("limit: %v", err)
	}
}

// A one-off job fires at its time through the clock, runs once, and turns itself off.
func TestScheduleJobAtRunsOnceThroughTheClock(t *testing.T) {
	e := newEnv(t)
	finished := collect(t, e.bus, events.JobRunFinished)
	a := jobAgent(t, e, "a1", instantRunner)
	at := time.Now().Add(300 * time.Millisecond).UTC().Format("2006-01-02T15:04:05.000Z")
	j, err := a.Jobs().Create(t.Context(), job.CreateInput{Name: "once", Prompt: "p", When: job.When{Kind: job.KindAt, At: at}})
	if err != nil {
		t.Fatal(err)
	}
	r := waitRun(t, a.Jobs(), j.ID, done)
	if r.Status != job.RunSucceeded || r.Cause != job.CauseSchedule || r.Output != "did once" {
		t.Fatalf("run = %+v", r)
	}
	evs, err := a.Jobs().ListEvents(t.Context(), j.ID, 0)
	if err != nil || len(evs) != 1 || evs[0].Source != "schedule" || evs[0].SourceEventID != at || evs[0].Type != "job.tick" {
		t.Fatalf("tick = %+v %v", evs, err)
	}
	waitFor(t, "the job state", func() bool {
		got, _ := a.Jobs().Get(t.Context(), j.ID)
		return got != nil && !got.Enabled && got.NextRunAt == nil && got.LastStatus == job.RunSucceeded
	})
	waitFor(t, "job:run-finished", func() bool { return len(finished()) == 1 })
	if p := finished()[0]; p.JobID != j.ID || p.Cause != "schedule" || p.Status != "succeeded" || p.JobName != "once" {
		t.Fatalf("payload = %+v", p)
	}
}

// Run now never overlaps a job with itself, and is for schedule jobs only.
func TestScheduleJobRunNowNeverOverlaps(t *testing.T) {
	e := newEnv(t)
	g := newGateRunner()
	a := jobAgent(t, e, "a1", g.run)
	j, _ := a.Jobs().Create(t.Context(), job.CreateInput{Name: "slow", Prompt: "p", When: everyHour()})
	first, err := a.Jobs().RunNow(t.Context(), j.ID)
	if err != nil || first.Cause != job.CauseManual || first.Status != job.RunWaiting {
		t.Fatalf("run now = %+v %v", first, err)
	}
	waitFor(t, "the run to start", func() bool { return g.count() == 1 })
	if _, err := a.Jobs().RunNow(t.Context(), j.ID); !errors.Is(err, &job.Error{Code: job.CodeInvalidArgument}) {
		t.Fatalf("a second run now must be refused: %v", err)
	}
	close(g.release)
	r := waitRun(t, a.Jobs(), j.ID, done)
	if r.Status != job.RunSucceeded || r.Output != "did slow" {
		t.Fatalf("run = %+v", r)
	}
	got, _ := a.Jobs().Get(t.Context(), j.ID)
	if got.NextRunAt == nil {
		t.Fatal("run now moved the schedule")
	}
}

// [[SILENT]] succeeds without anything to report; [[FAILED]] fails with its reason and is
// not retried.
func TestScheduleJobReplyMarkers(t *testing.T) {
	e := newEnv(t)
	var mu sync.Mutex
	replies := []string{"[[SILENT]]", "[[FAILED]] the search was rate-limited", "Found: a new article"}
	calls := 0
	a := jobAgent(t, e, "a1", func(context.Context, job.RunContext) (job.RunResult, error) {
		mu.Lock()
		defer mu.Unlock()
		out := replies[calls]
		calls++
		return job.RunResult{Output: out}, nil
	})
	j, _ := a.Jobs().Create(t.Context(), job.CreateInput{Name: "watch", Prompt: "p", When: everyHour()})
	want := []struct {
		status job.RunStatus
		silent bool
		err    string
	}{{job.RunSucceeded, true, ""}, {job.RunFailed, false, "the search was rate-limited"}, {job.RunSucceeded, false, ""}}
	for i, w := range want {
		r, err := a.Jobs().RunNow(t.Context(), j.ID)
		if err != nil {
			t.Fatal(err)
		}
		got := waitRun(t, a.Jobs(), j.ID, func(x job.Run) bool { return x.ID == r.ID && done(x) })
		if got.Status != w.status || got.Silent != w.silent || got.Error != w.err || got.Attempts != 1 {
			t.Fatalf("run %d = %+v", i, got)
		}
	}
	got, _ := a.Jobs().Get(t.Context(), j.ID)
	if got.LastStatus != job.RunSucceeded || got.LastError != "" {
		t.Fatalf("job = %+v", got)
	}
}

// "reuse" mode appends every run to one session, tagged with the job (Session.JobID).
func TestScheduleJobReuseSession(t *testing.T) {
	e := newEnv(t)
	a := jobAgent(t, e, "a1", instantRunner)
	j, _ := a.Jobs().Create(t.Context(), job.CreateInput{Name: "log", Prompt: "p", When: everyHour(), SessionMode: job.SessionReuse})
	var sessions []string
	for range 2 {
		r, err := a.Jobs().RunNow(t.Context(), j.ID)
		if err != nil {
			t.Fatal(err)
		}
		got := waitRun(t, a.Jobs(), j.ID, func(x job.Run) bool { return x.ID == r.ID && done(x) })
		sessions = append(sessions, got.SessionID)
	}
	if sessions[0] == "" || sessions[0] != sessions[1] {
		t.Fatalf("sessions = %v", sessions)
	}
	got, _ := a.Jobs().Get(t.Context(), j.ID)
	if got.SessionID != sessions[0] {
		t.Fatalf("job session = %q", got.SessionID)
	}
	list, err := e.hub.Sessions.List(t.Context(), kernel.SessionFilter{JobID: j.ID})
	if err != nil || len(list) != 1 {
		t.Fatalf("sessions of the job = %d %v", len(list), err)
	}
	if src, _ := list[0].Metadata.Get("source"); src != "job" {
		t.Fatalf("metadata source = %v", src)
	}
	if d, _ := list[0].Metadata.Get("eventDepth"); d != 0 {
		t.Fatalf("eventDepth = %v (%T)", d, d)
	}
}

// Delete stops a run in progress and removes the job and its history.
func TestScheduleJobDeleteStopsItsRun(t *testing.T) {
	e := newEnv(t)
	g := newGateRunner()
	a := jobAgent(t, e, "a1", g.run)
	j, _ := a.Jobs().Create(t.Context(), job.CreateInput{Name: "j", Prompt: "p", When: everyHour()})
	if _, err := a.Jobs().RunNow(t.Context(), j.ID); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the run to start", func() bool { return g.count() == 1 })
	if err := a.Jobs().Delete(t.Context(), j.ID); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the run ctx to end", func() bool {
		g.mu.Lock()
		defer g.mu.Unlock()
		return g.ctxs[0].Err() != nil
	})
	if got, _ := a.Jobs().Get(t.Context(), j.ID); got != nil {
		t.Fatal("job still there")
	}
}

// Dispose cancels a run in progress and gives it back to the queue (no attempt spent).
func TestScheduleJobDisposeGivesTheRunBack(t *testing.T) {
	e := newEnv(t)
	g := newGateRunner()
	a := jobAgent(t, e, "a1", g.run)
	j, _ := a.Jobs().Create(t.Context(), job.CreateInput{Name: "j", Prompt: "p", When: everyHour()})
	r, err := a.Jobs().RunNow(t.Context(), j.ID)
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the run to start", func() bool { return g.count() == 1 })
	if err := a.Dispose(t.Context()); err != nil {
		t.Fatal(err)
	}
	got, err := e.hub.Jobs.GetRun(t.Context(), r.ID)
	if err != nil || got == nil || got.Status != job.RunWaiting || got.Attempts != 0 || !strings.Contains(got.Reason, "Interrupted") {
		t.Fatalf("run = %+v %v", got, err)
	}
}

// The default runner runs a real turn in the run's session.
func TestScheduleJobDefaultRunner(t *testing.T) {
	e := newEnv(t)
	a := jobAgent(t, e, "a1", nil)
	j, _ := a.Jobs().Create(t.Context(), job.CreateInput{Name: "ping", Prompt: "status report please", When: everyHour()})
	if _, err := a.Jobs().RunNow(t.Context(), j.ID); err != nil {
		t.Fatal(err)
	}
	r := waitRun(t, a.Jobs(), j.ID, done)
	if r.Status != job.RunSucceeded || r.SessionID == "" {
		t.Fatalf("run = %+v", r)
	}
	msgs, err := e.hub.Messages.Get(t.Context(), r.SessionID)
	if err != nil || len(msgs) < 2 {
		t.Fatalf("messages = %d %v", len(msgs), err)
	}
}
