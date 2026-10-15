package e2e_test

import (
	"context"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/Phong95/aec-agent-go/components/agent/job"
	"github.com/Phong95/aec-agent-go/components/events"
	"github.com/Phong95/aec-agent-go/kernel"
)

// A-69 prompt jobs through an AgentCore: the host runner, the every cadence on
// virtual time, the auto-start of agents spawned after StartScheduledJobs, and the
// cleanup of RemoveAgent.

// recordingRunner counts the runs it gets and answers at once.
type recordingRunner struct {
	mu   sync.Mutex
	runs []job.RunContext
}

func (r *recordingRunner) run(_ context.Context, rc job.RunContext) (job.RunResult, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.runs = append(r.runs, rc)
	return job.RunResult{Output: "host ran " + rc.JobName}, nil
}

func (r *recordingRunner) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.runs)
}

// An every job fires on its cadence through the core's runner, each run in its own
// session, and RemoveAgent deletes the agent's jobs.
func TestAgentCore_PromptJobEveryCadenceAndRemove(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx := t.Context()
		core, _ := newCore(t, "ok")
		runner := &recordingRunner{}
		core.UseJobRunner(runner.run)
		// Update (A-126): the clock arms once the agent's jobs are started.
		if err := core.StartJobs(ctx); err != nil {
			t.Fatalf("StartJobs: %v", err)
		}
		a, err := core.SpawnAgent(ctx, testProfile("a1"))
		if err != nil {
			t.Fatalf("SpawnAgent: %v", err)
		}
		var mu sync.Mutex
		var sessions []string
		events.Subscribe(core.Events(), events.JobRunFinished, func(e kernel.Event, p events.JobRunFinishedPayload) {
			mu.Lock()
			defer mu.Unlock()
			if p.JobID != "" {
				sessions = append(sessions, e.SessionID)
			}
		})

		j, err := a.Jobs().Create(ctx, job.CreateInput{Name: "digest", Prompt: "p", When: job.When{Kind: job.KindEvery, Ms: 600_000}})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		time.Sleep(30*time.Minute + time.Second)
		synctest.Wait()

		if runner.count() != 3 {
			t.Fatalf("runs after 30 min of a 10 min job = %d, want 3", runner.count())
		}
		mu.Lock()
		if len(sessions) != 3 || sessions[0] == sessions[1] || sessions[1] == sessions[2] {
			t.Fatalf("each run must get its own session: %v", sessions)
		}
		mu.Unlock()

		if err := core.RemoveAgent(ctx, "a1"); err != nil {
			t.Fatalf("RemoveAgent: %v", err)
		}
		left, err := core.PersistenceHub().Jobs.ListByAgent(ctx, "a1")
		if err != nil || len(left) != 0 {
			t.Fatalf("jobs left after RemoveAgent: %v %v", left, err)
		}
		if got, _ := core.PersistenceHub().Jobs.Get(ctx, j.ID); got != nil {
			t.Fatal("the job survived RemoveAgent")
		}
		if err := core.Dispose(ctx); err != nil {
			t.Fatalf("Dispose: %v", err)
		}
	})
}

// After StartScheduledJobs, an agent spawned later (or rebuilt: DisposeAgent then
// SpawnAgent) starts its stored jobs at once: a job missed meanwhile catches up.
func TestAgentCore_StartScheduledJobsCoversLaterSpawns(t *testing.T) {
	ctx := t.Context()
	core, _ := newCore(t, "ok")
	runner := &recordingRunner{}
	core.UseJobRunner(runner.run)
	if err := core.StartJobs(ctx); err != nil {
		t.Fatalf("StartScheduledJobs: %v", err)
	}
	past := time.Now().Add(-2 * time.Hour).UTC().Format("2006-01-02T15:04:05.000Z")
	stored := job.Job{
		ID: kernel.NewID(), AgentID: "later", Name: "hourly", Prompt: "p", Enabled: true,
		When:        job.When{Kind: job.KindEvery, Ms: 3_600_000},
		SessionMode: job.SessionNew, NextRunAt: &past, CreatedBy: job.CreatedByUser, CreatedAt: past, UpdatedAt: past,
		Version: 1, Coalesce: job.DefaultCoalesce(), Limits: job.DefaultScheduleLimits(),
	}
	if err := core.PersistenceHub().Jobs.Save(ctx, stored); err != nil {
		t.Fatal(err)
	}
	if _, err := core.SpawnAgent(ctx, testProfile("later")); err != nil {
		t.Fatalf("SpawnAgent: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for runner.count() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if runner.count() != 1 || runner.runs[0].Cause != job.CauseCatchUp {
		t.Fatalf("runs = %+v, want one catch-up run", runner.runs)
	}
	if err := core.Dispose(context.WithoutCancel(ctx)); err != nil {
		t.Fatalf("Dispose: %v", err)
	}
}
