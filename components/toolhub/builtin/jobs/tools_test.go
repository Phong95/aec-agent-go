package jobs_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/Phong95/aec-agent-go/components/agent/job"
	"github.com/Phong95/aec-agent-go/components/toolhub"
	"github.com/Phong95/aec-agent-go/components/toolhub/builtin/jobs"
	"github.com/Phong95/aec-agent-go/kernel"
)

// A-70 follow-up: the tools on their own, over a fake trigger.Triggers that records what
// the tools ask for. The trigger manager's own rules (validation, secrets, delivery) are
// tested with a real agent in components/agent (trigger_tools_test.go).
// Update (A-126): over a fake job.Jobs; the job manager's own rules are tested with a real
// agent in components/agent (job_tools_test.go).

const secret = "whsec_c2VjcmV0LXRoYXQtbXVzdC1ub3QtbGVhaw=="

// fakeJobs is an in-memory job.Jobs: enough behaviour for the tools. The methods the
// tools never call are left to the embedded nil interface (they panic).
type fakeJobs struct {
	job.Jobs
	jobs    []job.Job
	created []job.CreateInput
	updates []job.UpdateInput
}

func (f *fakeJobs) List(context.Context) ([]job.Job, error) {
	return append([]job.Job(nil), f.jobs...), nil
}

func (f *fakeJobs) Get(_ context.Context, id string) (*job.Job, error) {
	for _, j := range f.jobs {
		if j.ID == id {
			return &j, nil
		}
	}
	return nil, nil //nolint:nilnil // the contract of Get
}

func (f *fakeJobs) Create(_ context.Context, in job.CreateInput) (job.Job, error) {
	f.created = append(f.created, in)
	j := job.Job{
		ID: kernel.NewID(), Name: in.Name, Enabled: true, Version: 1, When: in.When, Prompt: in.Prompt,
		SessionMode: in.SessionMode, CreatedBy: in.CreatedBy, CreatedInSessionID: in.CreatedInSessionID, Deliveries: in.Deliveries,
	}
	if in.Filter != nil {
		j.Filter = *in.Filter
	}
	if in.Limits != nil {
		j.Limits = job.Limits{MaxRunsPerHour: in.Limits.MaxRunsPerHour, MaxTokensPerDay: in.Limits.MaxTokensPerDay}
	}
	if j.When.IsSchedule() {
		next := "2026-10-12T09:15:00.000Z"
		j.NextRunAt = &next
	}
	if j.When.Kind == job.KindWebhook {
		j.Secret = secret
	}
	f.jobs = append(f.jobs, j)
	return j, nil
}

func (f *fakeJobs) Update(_ context.Context, id string, in job.UpdateInput) (job.Job, error) {
	f.updates = append(f.updates, in)
	for i, j := range f.jobs {
		if j.ID != id {
			continue
		}
		if in.When != nil {
			j.When = *in.When
		}
		if in.Filter != nil {
			j.Filter = *in.Filter
		}
		if in.Name != nil {
			j.Name = *in.Name
		}
		j.Version++
		f.jobs[i] = j
		return j, nil
	}
	return job.Job{}, job.NewError("no such job", job.CodeNotFound, id)
}

func (f *fakeJobs) ListRuns(context.Context, string, int) ([]job.Run, error) { return nil, nil }

func isErr(o toolhub.Output) bool { return o.IsError != nil && *o.IsError }

func call(t *testing.T, h toolhub.Handler, tc toolhub.Context, args map[string]any) toolhub.Output {
	t.Helper()
	raw, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	out, err := h.Execute(context.Background(), kernel.Arguments(raw), tc)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestRisksAndNames(t *testing.T) {
	tools := jobs.NewTools()
	if tools[0].Definition().Name != "manage_job" || tools[0].Definition().Risk != toolhub.RiskConsequential {
		t.Fatal("manage_job must be consequential")
	}
	if tools[1].Definition().Name != "list_jobs" || tools[1].Definition().Risk != toolhub.RiskReadOnly {
		t.Fatal("list_jobs must be read-only")
	}
	if got := jobs.ToolNames(); len(got) != 2 || got[0] != "manage_job" || got[1] != "list_jobs" {
		t.Fatalf("names = %v", got)
	}
}

func TestCreateEventJobWithCaps(t *testing.T) {
	f := &fakeJobs{}
	tc := toolhub.Context{SessionID: "chat-1", Jobs: f}
	out := call(t, jobs.NewManageTool(), tc, map[string]any{"action": "create", "name": "diagnose", "prompt": "Why?", "when": "event", "event_types": []string{"job.failed"}})
	if isErr(out) {
		t.Fatalf("create failed: %s", out.Content)
	}
	in := f.created[0]
	if in.When.Kind != job.KindEvent || in.CreatedBy != job.CreatedByAgent || in.CreatedInSessionID != "chat-1" ||
		*in.Limits.MaxRunsPerHour != jobs.AgentJobDefaultRunsPerHour || *in.Limits.MaxTokensPerDay != jobs.AgentJobDefaultTokensPerDay || *in.Limits.MaxDepth != 0 {
		t.Fatalf("create input = %+v", in)
	}
	if !strings.Contains(out.Content, "stays in its own conversation") {
		t.Fatalf("content = %s", out.Content)
	}
}

func TestWebhookSecretNeverShown(t *testing.T) {
	f := &fakeJobs{}
	tc := toolhub.Context{Jobs: f}
	out := call(t, jobs.NewManageTool(), tc, map[string]any{"action": "create", "name": "uploads", "prompt": "Report.", "when": "webhook", "event_types": []string{"model.uploaded"}})
	raw, _ := json.Marshal(out)
	if strings.Contains(string(raw), secret) || !strings.Contains(out.Content, "/webhooks/jobs/"+f.jobs[0].ID) || !strings.Contains(out.Content, "Jobs page") {
		t.Fatalf("output = %s", raw)
	}
	listed := call(t, jobs.NewListTool(), tc, map[string]any{})
	raw, _ = json.Marshal(listed)
	if strings.Contains(string(raw), secret) || !strings.Contains(listed.Content, "uploads") {
		t.Fatalf("list = %s", raw)
	}
}

// What gpt models send when every parameter is required: empty strings, 0 and [] for
// "not given"; and "here" in a web chat keeps results in the run instead of failing.
func TestStrictSchemaArguments(t *testing.T) {
	f := &fakeJobs{}
	tc := toolhub.Context{Jobs: f}
	filled := map[string]any{
		"action": "create", "job": "", "name": "CoinDesk Bitcoin watch", "description": "Bitcoin news.",
		"prompt": "Check CoinDesk. Reply with the items, or exactly [[SILENT]].", "when": "schedule",
		"cron": "", "timezone": "", "every_minutes": 15, "at": "", "session_mode": "reuse",
		"event_types": []string{}, "subjects": []string{}, "where": []any{}, "signature": "standard",
		"subject_path": "", "revision_path": "", "max_runs_per_hour": 4, "max_tokens_per_day": 50000,
		"deliver": "here", "deliver_when": "on-output",
	}
	out := call(t, jobs.NewManageTool(), tc, filled)
	if isErr(out) || !strings.Contains(out.Content, "not in a chat app") {
		t.Fatalf("create = %s", out.Content)
	}
	in := f.created[0]
	if in.When != (job.When{Kind: job.KindEvery, Ms: 900_000}) || in.SessionMode != job.SessionReuse || len(in.Deliveries) > 0 || in.Limits != nil || in.Filter != nil {
		t.Fatalf("create input = %+v", in)
	}

	filled["action"], filled["job"], filled["name"], filled["every_minutes"] = "update", "CoinDesk Bitcoin watch", "", 30
	out = call(t, jobs.NewManageTool(), tc, filled)
	if isErr(out) {
		t.Fatalf("update = %s", out.Content)
	}
	if u := f.updates[0]; u.When == nil || u.When.Ms != 1_800_000 || u.Name != nil || u.Filter != nil {
		t.Fatalf("update input = %+v", u)
	}
}

func TestBadCalls(t *testing.T) {
	f := &fakeJobs{}
	tc := toolhub.Context{Jobs: f}
	cases := []map[string]any{
		{"action": "launch"},
		{"action": "create", "name": "x", "prompt": "p"},
		{"action": "create", "name": "x", "prompt": "p", "when": "cron"},
		{"action": "create", "name": "x", "prompt": "p", "when": "schedule"},
		{"action": "create", "name": "x", "prompt": "p", "cron": "0 8 * * *", "every_minutes": 5},
		{"action": "create", "name": "x", "prompt": "p", "when": "event", "every_minutes": 5},
		{"action": "create", "name": "x", "prompt": "p", "when": "event", "event_types": []string{"job.failed"}, "max_runs_per_hour": 50},
		{"action": "create", "name": "x", "prompt": "p", "when": "event", "event_types": []string{"job.failed"}, "max_tokens_per_day": 5_000_000},
		{"action": "delete", "job": "missing"},
	}
	for _, args := range cases {
		out := call(t, jobs.NewManageTool(), tc, args)
		if !isErr(out) || (out.Retryable != nil && *out.Retryable) {
			t.Errorf("%v: %+v", args, out)
		}
	}
	if len(f.created) != 0 {
		t.Fatalf("created %d jobs", len(f.created))
	}
	none := call(t, jobs.NewListTool(), toolhub.Context{}, map[string]any{})
	if !isErr(none) {
		t.Fatal("list without jobs must fail")
	}
}

// A-127: a job made outside a chat app posts to every home chat by default; the user can
// pick some of them, or all of them from inside a chat.
func TestDeliverToHomeChats(t *testing.T) {
	homes := []job.HomeChat{{ChannelID: "tg", Name: "Telegram bot", Platform: "telegram"}, {ChannelID: "sl", Name: "Team", Platform: "slack"}}
	home := func(id string) job.Delivery {
		return job.Delivery{ChannelID: id, Target: job.DeliveryHome, When: job.DeliverAlways}
	}
	create := func(tc toolhub.Context, extra map[string]any) (toolhub.Output, *fakeJobs) {
		f := &fakeJobs{}
		tc.Jobs = f
		args := map[string]any{"action": "create", "name": "digest", "prompt": "p", "every_minutes": 60}
		for k, v := range extra {
			args[k] = v
		}
		return call(t, jobs.NewManageTool(), tc, args), f
	}
	web := toolhub.Context{JobDeliveryOptions: job.DeliveryOptions{Homes: homes}}

	out, f := create(web, nil)
	if isErr(out) || !strings.Contains(out.Content, "home chat of Telegram bot (telegram), the home chat of Team (slack)") {
		t.Fatalf("default = %s", out.Content)
	}
	if got := f.created[0].Deliveries; len(got) != 2 || got[0] != home("tg") || got[1] != home("sl") {
		t.Fatalf("default deliveries = %+v", got)
	}

	_, f = create(web, map[string]any{"deliver_channels": []any{"TELEGRAM"}})
	if got := f.created[0].Deliveries; len(got) != 1 || got[0] != home("tg") {
		t.Fatalf("picked = %+v", got)
	}

	out, _ = create(web, map[string]any{"deliver_channels": "discord"})
	if !isErr(out) || !strings.Contains(out.Content, `"Telegram bot" (telegram), "Team" (slack)`) {
		t.Fatalf("unknown = %s", out.Content)
	}

	here := job.Delivery{ChannelID: "tg", Target: "chat-1", When: job.DeliverAlways}
	chat := toolhub.Context{JobDeliveryOptions: job.DeliveryOptions{Here: &here, Homes: homes}}
	_, f = create(chat, nil)
	if got := f.created[0].Deliveries; len(got) != 1 || got[0] != here {
		t.Fatalf("in a chat = %+v", got)
	}
	_, f = create(chat, map[string]any{"deliver": "home", "deliver_when": "on-failure"})
	if got := f.created[0].Deliveries; len(got) != 2 || got[0].ChannelID != "tg" || got[1].ChannelID != "sl" || got[1].When != job.DeliverOnFailure {
		t.Fatalf("all homes from a chat = %+v", got)
	}
	_, f = create(chat, map[string]any{"deliver": "here", "deliver_channels": []any{"slack"}})
	if got := f.created[0].Deliveries; len(got) != 2 || got[0] != here || got[1] != home("sl") {
		t.Fatalf("here and slack = %+v", got)
	}

	out, f = create(toolhub.Context{}, map[string]any{"deliver": "home"})
	if isErr(out) || len(f.created[0].Deliveries) != 0 || !strings.Contains(out.Content, "no channel has a home chat") {
		t.Fatalf("no homes = %s", out.Content)
	}
}
