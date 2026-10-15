package agent_test

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/Phong95/aec-agent-go/aecmock"
	"github.com/Phong95/aec-agent-go/components/agent"
	"github.com/Phong95/aec-agent-go/components/agent/job"
	"github.com/Phong95/aec-agent-go/components/agent/profile"
	"github.com/Phong95/aec-agent-go/components/channelplatformhub"
	"github.com/Phong95/aec-agent-go/components/channelplatformhub/channel"
	"github.com/Phong95/aec-agent-go/components/events"
	"github.com/Phong95/aec-agent-go/components/llmhub"
	"github.com/Phong95/aec-agent-go/components/toolhub"
	"github.com/Phong95/aec-agent-go/components/toolhub/builtin/jobs"
	"github.com/Phong95/aec-agent-go/kernel"
)

// A-70 follow-up (trigger-tools.test.ts of core/ts) - the agent sets triggers up itself
// (manage_trigger / list_triggers - Update (A-126): manage_job / list_jobs -, approval on every call, capped limits, no secret for
// the model), and a trigger with a delivery posts each run's result to a channel chat
// (the channel manager subscribes to TriggerRunFinished).

// triggerToolsRig is an agent with the trigger tools, the fake channel platform (one
// channel, owner "owner"), a scripted model and a swappable trigger runner (nil = the
// agent's own Chat). Its trigger worker is started.
type triggerToolsRig struct {
	e        *env
	a        *agent.Agent
	provider *scriptedProvider
	ch       channel.Channel
	adapter  *aecmock.FakeChannelAdapter
	runner   atomic.Pointer[job.Runner]
}

// echoReply answers "echo: <first line of the user message>", or "done: <tool result>"
// after a tool call.
func echoReply(lastUser, toolResult string) string {
	if toolResult != "" {
		return "done: " + toolResult
	}
	first, _, _ := strings.Cut(lastUser, "\n")
	return "echo: " + first
}

func newTriggerToolsRig(t *testing.T, provider *scriptedProvider) *triggerToolsRig {
	t.Helper()
	if provider == nil {
		provider = &scriptedProvider{reply: echoReply}
	}
	r := &triggerToolsRig{e: newEnv(t, "unused"), provider: provider}
	r.e.llm = llmhub.NewLLMHub()
	mustOK(t, r.e.llm.Register(provider))
	for _, tool := range jobs.NewTools() {
		mustOK(t, r.e.tools.Register(tool))
	}
	hub := channelplatformhub.New()
	platform := aecmock.NewFakeChannelPlatformWithCapabilities("fake", channel.Capabilities{MaxMessageLength: 4000, Buttons: true})
	mustOK(t, hub.Register(platform))
	deps := r.e.deps()
	deps.ChannelPlatformHub = hub
	deps.JobRunner = func() job.Runner {
		if p := r.runner.Load(); p != nil {
			return *p
		}
		return nil
	}
	p := testProfile("site-assistant")
	p.Name = "Site Assistant"
	p.Tools = jobs.ToolNames()
	retries := 0
	p.LoopConfig = &profile.LoopConfig{MaxToolRetries: &retries}
	a, err := agent.New(t.Context(), deps, p)
	mustOK(t, err)
	t.Cleanup(func() { _ = a.Dispose(context.WithoutCancel(t.Context())) })
	r.a = a
	allowlist := channel.DmAllowlist
	r.ch, err = a.Channels().Create(t.Context(), channel.CreateInput{
		Platform: "fake", Credentials: map[string]string{"token": "t"},
		Access: &channel.AccessInput{DmPolicy: &allowlist, AllowFrom: []string{"owner"}},
	})
	mustOK(t, err)
	r.adapter = platform.AdapterFor(r.ch.ID)
	mustOK(t, a.StartJobs(t.Context()))
	return r
}

func (r *triggerToolsRig) useRunner(run job.Runner) { r.runner.Store(&run) }

// toolContext is the context of a tool call in session "chat-1" of the rig's agent.
func (r *triggerToolsRig) toolContext(delivery *job.Delivery) toolhub.Context {
	return toolhub.Context{SessionID: "chat-1", AgentID: r.a.Profile().ID, Jobs: r.a.Jobs(), JobDeliveryOptions: job.DeliveryOptions{Here: delivery}}
}

func (r *triggerToolsRig) list(t *testing.T) []job.Job {
	t.Helper()
	all, err := r.a.Jobs().List(t.Context())
	mustOK(t, err)
	sort.Slice(all, func(i, j int) bool { return all[i].Name < all[j].Name })
	return all
}

func execTool(t *testing.T, h toolhub.Handler, tc toolhub.Context, args string) toolhub.Output {
	t.Helper()
	out, err := h.Execute(t.Context(), kernel.Arguments(args), tc)
	mustOK(t, err)
	return out
}

func outputIsError(o toolhub.Output) bool { return o.IsError != nil && *o.IsError }

func outputJSON(t *testing.T, o toolhub.Output) string {
	t.Helper()
	raw, err := json.Marshal(o)
	mustOK(t, err)
	return string(raw)
}

func zeroTriggerCoalesce() *job.CoalesceInput {
	zero := 0
	return &job.CoalesceInput{QuietMs: &zero, MaxWaitMs: &zero}
}

// newestTriggerRun is the trigger's newest run (zero when it has none).
func newestTriggerRun(t *testing.T, a *agent.Agent, id string) job.Run {
	t.Helper()
	runs, err := a.Jobs().ListRuns(t.Context(), id, 1)
	mustOK(t, err)
	if len(runs) == 0 {
		return job.Run{}
	}
	return runs[0]
}

// ---------------------------------------------------------------------------
// The tools on their own
// ---------------------------------------------------------------------------

func TestTriggerToolCreatesAnAgentMadeTriggerWithCappedLimits(t *testing.T) {
	r := newTriggerToolsRig(t, nil)
	out := execTool(t, jobs.NewManageTool(), r.toolContext(nil),
		`{"action":"create","name":"diagnose","prompt":"Find out why {{event.subject}} failed.","when":"event","event_types":["job.failed"]}`)
	if outputIsError(out) {
		t.Fatalf("create = %+v", out)
	}
	all := r.list(t)
	if len(all) != 1 {
		t.Fatalf("triggers = %+v", all)
	}
	tr := all[0]
	l := tr.Limits
	if tr.Name != "diagnose" || tr.CreatedBy != job.CreatedByAgent || tr.CreatedInSessionID != "chat-1" || strings.Join(tr.Filter.Types, ",") != "job.failed" ||
		l.MaxRunsPerHour == nil || *l.MaxRunsPerHour != jobs.AgentJobDefaultRunsPerHour || l.MaxTokensPerDay == nil || *l.MaxTokensPerDay != jobs.AgentJobDefaultTokensPerDay ||
		l.MaxConcurrentRuns != 1 || l.MaxDepth != 0 || len(tr.Deliveries) > 0 {
		t.Fatalf("trigger = %+v", tr)
	}
	if !strings.Contains(out.Content, "stays in its own conversation") {
		t.Fatalf("content = %s", out.Content)
	}
}

func TestTriggerToolsNeverShowTheWebhookSecret(t *testing.T) {
	r := newTriggerToolsRig(t, nil)
	out := execTool(t, jobs.NewManageTool(), r.toolContext(nil),
		`{"action":"create","name":"uploads","prompt":"Report the new model.","when":"webhook","event_types":["model.uploaded"],"subject_path":"data.modelId"}`)
	tr := r.list(t)[0]
	if !strings.HasPrefix(tr.Secret, "whsec_") {
		t.Fatalf("secret = %q", tr.Secret)
	}
	if !strings.Contains(out.Content, "/webhooks/jobs/"+tr.ID) || !strings.Contains(out.Content, "Jobs page") {
		t.Fatalf("content = %s", out.Content)
	}
	if strings.Contains(outputJSON(t, out), tr.Secret) {
		t.Fatal("the create output carries the secret")
	}
	listed := execTool(t, jobs.NewListTool(), r.toolContext(nil), `{}`)
	if !strings.Contains(listed.Content, "uploads") || strings.Contains(outputJSON(t, listed), tr.Secret) {
		t.Fatalf("list = %s", outputJSON(t, listed))
	}
}

func TestTriggerToolUpdatesByNameOrIDAndKeepsTheRest(t *testing.T) {
	r := newTriggerToolsRig(t, nil)
	manage, tc := jobs.NewManageTool(), r.toolContext(nil)
	execTool(t, manage, tc, `{"action":"create","name":"uploads","prompt":"p","when":"webhook","signature":"github","event_types":["model.uploaded"],"subject_path":"data.modelId"}`)
	tr := r.list(t)[0]

	out := execTool(t, manage, tc, `{"action":"update","job":"uploads","where":[{"path":"data.project","op":"in","values":["a","b"]}],"revision_path":"data.rev","max_runs_per_hour":2}`)
	if outputIsError(out) {
		t.Fatalf("update = %+v", out)
	}
	after, err := r.a.Jobs().Get(t.Context(), tr.ID)
	mustOK(t, err)
	if after.When != (job.When{Kind: job.KindWebhook, Signature: job.SignatureGithub, SubjectPath: "data.modelId", RevisionPath: "data.rev"}) {
		t.Fatalf("source = %+v", after.When)
	}
	if strings.Join(after.Filter.Types, ",") != "model.uploaded" || len(after.Filter.Where) != 1 {
		t.Fatalf("filter = %+v", after.Filter)
	}
	if c := after.Filter.Where[0]; c.Path != "data.project" || c.Op != job.OpIn {
		t.Fatalf("condition = %+v", c)
	}
	if after.Limits.MaxRunsPerHour == nil || *after.Limits.MaxRunsPerHour != 2 || after.Limits.MaxDepth != 0 {
		t.Fatalf("limits = %+v", after.Limits)
	}

	execTool(t, manage, tc, `{"action":"disable","job":"`+tr.ID+`"}`)
	if got, _ := r.a.Jobs().Get(t.Context(), tr.ID); got.Enabled {
		t.Fatal("still enabled")
	}
	execTool(t, manage, tc, `{"action":"enable","job":"uploads"}`)
	if got, _ := r.a.Jobs().Get(t.Context(), tr.ID); !got.Enabled {
		t.Fatal("still disabled")
	}
	execTool(t, manage, tc, `{"action":"delete","job":"uploads"}`)
	if all := r.list(t); len(all) != 0 {
		t.Fatalf("triggers = %+v", all)
	}
}

func TestTriggerToolReportsBadCallsAsNonRetryable(t *testing.T) {
	r := newTriggerToolsRig(t, nil)
	manage, tc := jobs.NewManageTool(), r.toolContext(nil)
	cases := []string{
		`{"action":"launch"}`,
		`{"action":"create","name":"x","prompt":"p"}`,
		`{"action":"create","name":"x","prompt":"p","when":"cron"}`,
		`{"action":"create","name":"x","prompt":"p","when":"event"}`,
		`{"action":"create","name":"x","prompt":"p","when":"event","event_types":["job.failed"],"max_runs_per_hour":50}`,
		`{"action":"create","name":"x","prompt":"p","when":"event","event_types":["job.failed"],"max_tokens_per_day":5000000}`,
		`{"action":"delete","job":"missing"}`,
	}
	for _, args := range cases {
		out := execTool(t, manage, tc, args)
		if !outputIsError(out) || out.Retryable == nil || *out.Retryable {
			t.Errorf("%s: %+v, want a non-retryable error", args, out)
		}
	}
	if all := r.list(t); len(all) != 0 {
		t.Fatalf("triggers = %+v", all)
	}
	if out := execTool(t, jobs.NewListTool(), toolhub.Context{SessionID: "s"}, `{}`); !outputIsError(out) {
		t.Fatalf("no triggers on the context = %+v", out)
	}
}

func TestTriggerToolDeliversHereByDefaultOrNone(t *testing.T) {
	r := newTriggerToolsRig(t, nil)
	manage := jobs.NewManageTool()
	tc := r.toolContext(&job.Delivery{ChannelID: r.ch.ID, Target: "c1", When: job.DeliverAlways})
	execTool(t, manage, tc, `{"action":"create","name":"a","prompt":"p","when":"event","event_types":["job.failed"],"deliver_when":"on-output"}`)
	execTool(t, manage, tc, `{"action":"create","name":"b","prompt":"p","when":"event","event_types":["job.failed"],"deliver":"none"}`)
	all := r.list(t)
	want := job.Delivery{ChannelID: r.ch.ID, Target: "c1", When: job.DeliverOnOutput}
	if len(all) != 2 || len(all[0].Deliveries) == 0 || all[0].Deliveries[0] != want || len(all[1].Deliveries) > 0 {
		t.Fatalf("triggers = %+v", all)
	}
	execTool(t, manage, tc, `{"action":"update","job":"a","deliver":"none"}`)
	if got := r.list(t)[0]; len(got.Deliveries) > 0 {
		t.Fatalf("delivery = %+v", got.Deliveries)
	}
}

// ---------------------------------------------------------------------------
// Delivery of trigger results to a channel chat
// ---------------------------------------------------------------------------

func webhookTrigger(name string, mut func(*job.CreateInput)) job.CreateInput {
	in := job.CreateInput{Name: name, When: job.When{Kind: job.KindWebhook, Signature: job.SignatureStandard}, Prompt: "p"}
	if mut != nil {
		mut(&in)
	}
	return in
}

func TestTriggerDeliveryIsValidatedAndUpdated(t *testing.T) {
	r := newTriggerToolsRig(t, nil)
	tg := r.a.Jobs()
	invalid := func(err error) bool {
		var te *job.Error
		return errors.As(err, &te) && te.Code == job.CodeInvalidArgument
	}
	for _, in := range []job.CreateInput{
		webhookTrigger("a", func(in *job.CreateInput) {
			in.Deliveries = job.Deliveries{{ChannelID: "", Target: "home", When: job.DeliverAlways}}
		}),
		webhookTrigger("a", func(in *job.CreateInput) {
			in.Deliveries = job.Deliveries{{ChannelID: r.ch.ID, Target: "home", When: "sometimes"}}
		}),
		webhookTrigger("a", func(in *job.CreateInput) { in.CreatedBy = "robot" }),
	} {
		if _, err := tg.Create(t.Context(), in); !invalid(err) {
			t.Errorf("create %+v: %v, want invalid_argument", in, err)
		}
	}

	tr, err := tg.Create(t.Context(), webhookTrigger("a", func(in *job.CreateInput) {
		in.Deliveries = job.Deliveries{{ChannelID: r.ch.ID, Target: "home", ThreadID: "ignored", When: job.DeliverAlways}}
	}))
	mustOK(t, err)
	if tr.CreatedBy != job.CreatedByUser || len(tr.Deliveries) == 0 || tr.Deliveries[0] != (job.Delivery{ChannelID: r.ch.ID, Target: "home", When: job.DeliverAlways}) {
		t.Fatalf("trigger = %+v (delivery %+v)", tr, tr.Deliveries)
	}
	changed, err := tg.Update(t.Context(), tr.ID, job.UpdateInput{Deliveries: job.Deliveries{{ChannelID: r.ch.ID, Target: "c2", ThreadID: "t1", When: job.DeliverOnOutput}}})
	mustOK(t, err)
	if changed.Deliveries[0] != (job.Delivery{ChannelID: r.ch.ID, Target: "c2", ThreadID: "t1", When: job.DeliverOnOutput}) {
		t.Fatalf("delivery = %+v", changed.Deliveries)
	}
	cleared, err := tg.Update(t.Context(), tr.ID, job.UpdateInput{ClearDeliveries: true})
	mustOK(t, err)
	if len(cleared.Deliveries) > 0 {
		t.Fatalf("delivery = %+v", cleared.Deliveries)
	}
}

func TestTriggerRunReplyGoesToTheDeliveryChat(t *testing.T) {
	r := newTriggerToolsRig(t, nil)
	sent := collect(t, r.e.bus, events.ChannelMessageSent)
	tr, err := r.a.Jobs().Create(t.Context(), webhookTrigger("uploads", func(in *job.CreateInput) {
		in.Prompt = "Report the new model"
		in.Deliveries = job.Deliveries{{ChannelID: r.ch.ID, Target: "c7", When: job.DeliverAlways}}
		in.Coalesce = zeroTriggerCoalesce()
	}))
	mustOK(t, err)
	_, err = r.a.Jobs().SendTestEvent(t.Context(), tr.ID, job.TestEventInput{Type: "model.uploaded"})
	mustOK(t, err)

	waitFor(t, "the delivery", func() bool { return len(r.adapter.Sent()) == 1 })
	if got := r.adapter.Sent()[0]; got.ChatID != "c7" || got.Text != "uploads\n\necho: Report the new model" {
		t.Fatalf("sent %+v", got)
	}
	waitFor(t, "channel:message-sent", func() bool {
		for _, p := range sent() {
			if p.JobID == tr.ID && p.ChannelID == r.ch.ID && p.ChatID == "c7" {
				return true
			}
		}
		return false
	})
	// It also joins the chat's conversation, like a job's result.
	waitFor(t, "the chat's conversation", func() bool {
		for _, s := range chatSessions(t, r.a, r.ch.ID, "c7") {
			msgs, err := r.e.hub.Messages.Get(t.Context(), s.ID)
			mustOK(t, err)
			for _, m := range msgs {
				if m.Role == kernel.RoleAssistant && strings.Contains(m.Content, "echo: Report the new model") {
					return true
				}
			}
		}
		return false
	})
}

func TestTriggerDeliveryFollowsWhenAndSendsFailures(t *testing.T) {
	r := newTriggerToolsRig(t, nil)
	finished := collect(t, r.e.bus, events.JobRunFinished)
	tg := r.a.Jobs()
	one := 1
	tr, err := tg.Create(t.Context(), webhookTrigger("watch", func(in *job.CreateInput) {
		in.Prompt = "Look"
		in.Deliveries = job.Deliveries{{ChannelID: r.ch.ID, Target: "c1", When: job.DeliverOnFailure}}
		in.Limits = &job.LimitsInput{MaxAttempts: &one}
		in.Coalesce = zeroTriggerCoalesce()
	}))
	mustOK(t, err)

	// A success with on-failure: nothing is sent.
	_, err = tg.SendTestEvent(t.Context(), tr.ID, job.TestEventInput{Type: "x", Subject: "s1"})
	mustOK(t, err)
	waitFor(t, "the first run", func() bool { return len(finished()) == 1 })
	if p := finished()[0]; p.Status != "succeeded" || p.JobName != "watch" || p.Output != "echo: Look" || len(p.Deliveries) == 0 || p.Deliveries[0].ChannelID != r.ch.ID || p.Deliveries[0].Target != "c1" {
		t.Fatalf("finished %+v", p)
	}

	// A failure is sent.
	r.useRunner(func(context.Context, job.RunContext) (job.RunResult, error) {
		return job.RunResult{}, job.NewError("the model is gone", job.CodeInvalidArgument, "")
	})
	_, err = tg.SendTestEvent(t.Context(), tr.ID, job.TestEventInput{Type: "x", Subject: "s2"})
	mustOK(t, err)
	waitFor(t, "the failure", func() bool { return len(r.adapter.Sent()) == 1 })
	if got := r.adapter.Sent()[0].Text; got != `Job "watch" failed: the model is gone` {
		t.Fatalf("sent %q", got)
	}

	// A stopped run did nothing worth reporting, whatever When says.
	minute := 60_000
	_, err = tg.Update(t.Context(), tr.ID, job.UpdateInput{
		Deliveries: job.Deliveries{{ChannelID: r.ch.ID, Target: "c1", When: job.DeliverAlways}},
		Coalesce:   &job.CoalesceInput{QuietMs: &minute, MaxWaitMs: &minute},
	})
	mustOK(t, err)
	accepted, err := tg.SendTestEvent(t.Context(), tr.ID, job.TestEventInput{Type: "x", Subject: "s3"})
	mustOK(t, err)
	stopped, err := tg.StopRun(t.Context(), accepted.Run.ID)
	mustOK(t, err)
	if !stopped {
		t.Fatal("the run was not stopped")
	}
	waitFor(t, "the stopped run", func() bool {
		all := finished()
		return len(all) > 0 && all[len(all)-1].Status == "stopped"
	})
	if n := len(r.adapter.Sent()); n != 1 {
		t.Fatalf("sent %d messages", n)
	}
}

func TestTriggerMadeInAChatPostsBackAndItsRunsCannotMakeTriggers(t *testing.T) {
	r := newTriggerToolsRig(t, &scriptedProvider{
		toolName: "manage_job",
		toolArgs: `{"action":"create","name":"uploads","prompt":"Write a report on the new model.","when":"webhook","event_types":["model.uploaded"]}`,
		reply:    echoReply,
	})
	mustOK(t, r.adapter.Receive(aecmock.FakeDirectMessage("owner", "make me a report when a new model is uploaded")))
	waitFor(t, "the approval prompt", func() bool { _, ok := findSent(r.adapter, hasButtons); return ok })
	mustOK(t, r.adapter.Press(string(rune(0x2705))+" Approve", channel.Sender{ID: "owner", Name: "Owner"}, channel.ChatDM))
	waitFor(t, "the reply", func() bool { return strings.Contains(lastText(r.adapter), "will be posted in this chat") })
	if strings.Contains(lastText(r.adapter), "whsec_") {
		t.Fatalf("the chat saw the secret: %s", lastText(r.adapter))
	}
	all := r.list(t)
	if len(all) != 1 {
		t.Fatalf("triggers = %+v", all)
	}
	tr := all[0]
	if tr.CreatedBy != job.CreatedByAgent || len(tr.Deliveries) == 0 || tr.Deliveries[0] != (job.Delivery{ChannelID: r.ch.ID, Target: "chat-owner", When: job.DeliverAlways}) {
		t.Fatalf("trigger = %+v (delivery %+v)", tr, tr.Deliveries)
	}

	// The run's turn tries manage_trigger again: nobody can approve, so the run is
	// blocked, no trigger is made, and the chat hears why.
	before := len(r.adapter.Sent())
	_, err := r.a.Jobs().Update(t.Context(), tr.ID, job.UpdateInput{Coalesce: zeroTriggerCoalesce()})
	mustOK(t, err)
	_, err = r.a.Jobs().SendTestEvent(t.Context(), tr.ID, job.TestEventInput{Type: "model.uploaded"})
	mustOK(t, err)
	waitFor(t, "the blocked run", func() bool { return newestTriggerRun(t, r.a, tr.ID).Status == job.RunBlocked })
	if all := r.list(t); len(all) != 1 {
		t.Fatalf("triggers = %+v", all)
	}
	waitFor(t, "the delivery", func() bool { return len(r.adapter.Sent()) == before+1 })
	if got := lastText(r.adapter); !strings.Contains(got, `Job "uploads" stopped without finishing: Needs approval: manage_job`) {
		t.Fatalf("sent %q", got)
	}
}
