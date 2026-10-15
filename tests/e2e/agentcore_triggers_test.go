package e2e_test

import (
	"strconv"
	"strings"
	"testing"
	"time"

	aec "github.com/Phong95/aec-agent-go"
	"github.com/Phong95/aec-agent-go/aecmock"
	"github.com/Phong95/aec-agent-go/components/agent"
	"github.com/Phong95/aec-agent-go/components/agent/job"
	"github.com/Phong95/aec-agent-go/components/events"
	"github.com/Phong95/aec-agent-go/kernel"
)

// A-70 event triggers through an AgentCore: the agent's default runner (its own Chat on
// a scripted model), webhook routing, the worker of an agent spawned after
// StartTriggers, and the cleanup of RemoveAgent.

// newEchoCore builds an AgentCore whose model answers "echo: " + the first line of the
// last user message.
func newEchoCore(t *testing.T) *aec.AgentCore {
	t.Helper()
	core, err := aec.NewAgentCore()
	if err != nil {
		t.Fatalf("NewAgentCore: %v", err)
	}
	mock := aecmock.NewMockProvider("echo", func(msgs []kernel.Message) string {
		for i := len(msgs) - 1; i >= 0; i-- {
			if msgs[i].Role == kernel.RoleUser {
				first, _, _ := strings.Cut(msgs[i].Content, "\n")
				return "echo: " + first
			}
		}
		return "echo:"
	})
	if err := core.UseLLMProvider(mock); err != nil {
		t.Fatalf("UseLLMProvider: %v", err)
	}
	return core
}

// waitRunStatus polls the trigger's newest run until it has status.
func waitRunStatus(t *testing.T, a *agent.Agent, triggerID string, status job.RunStatus) job.Run {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		runs, err := a.Jobs().ListRuns(t.Context(), triggerID, 1)
		if err != nil {
			t.Fatal(err)
		}
		if len(runs) > 0 && runs[0].Status == status {
			return runs[0]
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("trigger %s has no %s run", triggerID, status)
	return job.Run{}
}

func zeroCoalesce() *job.CoalesceInput {
	zero := 0
	return &job.CoalesceInput{QuietMs: &zero, MaxWaitMs: &zero}
}

// A failed prompt job wakes its agent's trigger through the default runner; removing
// the agent deletes its triggers.
func TestAgentCore_TriggerDefaultRunnerAndRemove(t *testing.T) {
	ctx := t.Context()
	core := newEchoCore(t)
	a, err := core.SpawnAgent(ctx, testProfile("a1"))
	if err != nil {
		t.Fatalf("SpawnAgent: %v", err)
	}
	tr, err := a.Jobs().Create(ctx, job.CreateInput{
		Name: "diagnose", When: job.When{Kind: job.KindEvent}, Filter: &job.Filter{Types: []string{"job.failed"}},
		Prompt: "Diagnose {{event.subject}}", Coalesce: zeroCoalesce(),
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := core.StartJobs(ctx); err != nil {
		t.Fatalf("StartTriggers: %v", err)
	}

	events.Emit(core.Events(), events.JobRunFinished, "s", events.JobRunFinishedPayload{AgentID: "a1", JobName: "n", JobID: "job-9", RunID: "r9", Cause: "schedule", Status: "failed", Error: "x"})

	run := waitRunStatus(t, a, tr.ID, job.RunSucceeded)
	if run.Output != "echo: Diagnose job-9" {
		t.Fatalf("output %q", run.Output)
	}

	empty := job.WebhookRequest{Method: "POST", Headers: map[string]string{}, Body: []byte("{}")}
	if res, err := core.HandleJobWebhook(ctx, tr.ID, empty); err != nil || res.Status != 404 {
		t.Fatalf("webhook to an event trigger = %+v, %v", res, err)
	}
	if err := core.RemoveAgent(ctx, "a1"); err != nil {
		t.Fatalf("RemoveAgent: %v", err)
	}
	if got, _ := core.PersistenceHub().Jobs.Get(ctx, tr.ID); got != nil {
		t.Fatal("the trigger survived RemoveAgent")
	}
	if res, err := core.HandleJobWebhook(ctx, tr.ID, empty); err != nil || res.Status != 404 {
		t.Fatalf("webhook after remove = %+v, %v", res, err)
	}
	if err := core.Dispose(ctx); err != nil {
		t.Fatalf("Dispose: %v", err)
	}
}

// An agent spawned after StartTriggers starts its trigger worker too (agent:spawned).
func TestAgentCore_TriggerWorkerOfALaterAgent(t *testing.T) {
	ctx := t.Context()
	core := newEchoCore(t)
	if err := core.StartJobs(ctx); err != nil {
		t.Fatalf("StartTriggers: %v", err)
	}
	a, err := core.SpawnAgent(ctx, testProfile("late"))
	if err != nil {
		t.Fatalf("SpawnAgent: %v", err)
	}
	tr, err := a.Jobs().Create(ctx, job.CreateInput{
		Name: "ci", When: job.When{Kind: job.KindWebhook}, Filter: &job.Filter{Types: []string{"build.failed"}},
		Prompt: "Late run", Coalesce: zeroCoalesce(),
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := a.Jobs().SendTestEvent(ctx, tr.ID, job.TestEventInput{Type: "build.failed"}); err != nil {
		t.Fatalf("SendTestEvent: %v", err)
	}

	run := waitRunStatus(t, a, tr.ID, job.RunSucceeded)
	if run.Output != "echo: Late run" {
		t.Fatalf("output %q", run.Output)
	}

	// A signed delivery is routed to the agent that owns the job.
	body := []byte(`{"type":"build.failed","data":{}}`)
	now := time.Now().Unix()
	res, err := core.HandleJobWebhook(ctx, tr.ID, job.WebhookRequest{Method: "POST", Headers: map[string]string{
		"webhook-id": "m1", "webhook-timestamp": strconv.FormatInt(now, 10), "webhook-signature": job.SignStandardWebhook(tr.Secret, "m1", now, body),
	}, Body: body})
	if err != nil || res.Status != 202 {
		t.Fatalf("routed webhook = %+v, %v", res, err)
	}
	if err := core.Dispose(ctx); err != nil {
		t.Fatalf("Dispose: %v", err)
	}
}
