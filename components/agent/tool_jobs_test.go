package agent_test

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/Phong95/aec-agent-go/components/agent"
	"github.com/Phong95/aec-agent-go/components/agent/job"
	"github.com/Phong95/aec-agent-go/components/events"
	"github.com/Phong95/aec-agent-go/components/toolhub"
	"github.com/Phong95/aec-agent-go/components/toolhub/builtin/jobs"
	"github.com/Phong95/aec-agent-go/kernel"
)

// A-103 - tool jobs: a schedule job that runs one stored tool call instead of a turn.
// The tests/unit/tool-jobs.test.ts of core/ts (the agent part; the manager part - the
// pre-approval, the time limit, auto-pause and the placeholders - is in
// tool_jobs_internal_test.go).

// scriptTool is a schedulable "script" tool: it prints args.out, fails when args.fail,
// prints nothing (Empty) when args.quiet, and waits for the stop when args.hang.
type scriptTool struct {
	risk toolhub.Risk
	mu   sync.Mutex
	seen []map[string]any
	ctxs []context.Context
}

func (*scriptTool) Name() string    { return "tool:script" }
func (*scriptTool) Version() string { return "1.0.0" }
func (s *scriptTool) Definition() toolhub.Definition {
	risk := s.risk
	if risk == "" {
		risk = toolhub.RiskDraft
	}
	return toolhub.Definition{Name: "script", Description: "run a script", Parameters: toolhub.JSONSchema{Type: "object"}, Risk: risk, Schedulable: true}
}

func (s *scriptTool) Execute(ctx context.Context, raw kernel.Arguments, _ toolhub.Context) (toolhub.Output, error) {
	var args map[string]any
	_ = json.Unmarshal(raw, &args)
	s.mu.Lock()
	s.seen = append(s.seen, args)
	s.ctxs = append(s.ctxs, ctx)
	s.mu.Unlock()
	switch {
	case args["hang"] == true:
		<-ctx.Done()
		return toolhub.Output{Content: "killed", IsError: boolPtr(true)}, nil
	case args["fail"] == true:
		// Like terminal: a failed command is not retried by the harness.
		return toolhub.Output{Content: "exit 1: boom", IsError: boolPtr(true), Retryable: boolPtr(false)}, nil
	case args["quiet"] == true:
		// Like terminal for a command that printed nothing.
		return toolhub.Output{Content: "$ script\nexit code 0\n(no output)", Empty: true}, nil
	}
	out, _ := args["out"].(string)
	return toolhub.Output{Content: out}, nil
}

func (s *scriptTool) calls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.seen)
}

// plainTool is a tool that is not schedulable.
type plainTool struct{}

func (plainTool) Name() string    { return "tool:plain" }
func (plainTool) Version() string { return "1.0.0" }
func (plainTool) Definition() toolhub.Definition {
	return toolhub.Definition{Name: "plain", Description: "plain", Parameters: toolhub.JSONSchema{Type: "object"}, Risk: toolhub.RiskReadOnly}
}

func (plainTool) Execute(context.Context, kernel.Arguments, toolhub.Context) (toolhub.Output, error) {
	return toolhub.Output{Content: "ok"}, nil
}

// toolJobAgent is an agent with the script (and plain) tool whose turns go through a
// runner that answers reply and counts its calls.
func toolJobAgent(t *testing.T, tool *scriptTool, reply string) (*agent.Agent, *env, *atomic.Int32) {
	t.Helper()
	e := newEnv(t)
	if err := e.tools.Register(tool); err != nil {
		t.Fatal(err)
	}
	if err := e.tools.Register(plainTool{}); err != nil {
		t.Fatal(err)
	}
	var turns atomic.Int32
	deps := e.deps()
	deps.JobRunner = func() job.Runner {
		return func(context.Context, job.RunContext) (job.RunResult, error) {
			turns.Add(1)
			return job.RunResult{Output: reply, Usage: &job.Usage{InputTokens: 7, OutputTokens: 3}}, nil
		}
	}
	p := testProfile("tool-job-agent")
	p.Tools = []string{"script", "plain"}
	a, err := agent.New(t.Context(), deps, p)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.Dispose(context.WithoutCancel(t.Context())) })
	if err := a.StartJobs(t.Context()); err != nil {
		t.Fatal(err)
	}
	return a, e, &turns
}

func toolJob(name string, args map[string]any, followUp job.FollowUp, prompt string) job.CreateInput {
	return job.CreateInput{Name: name, Prompt: prompt, When: everyHour(), Action: &job.Action{Kind: job.ActionTool, Tool: "script", Args: args, FollowUp: followUp}}
}

func runOnce(t *testing.T, jobs job.Jobs, id string) job.Run {
	t.Helper()
	queued, err := jobs.RunNow(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	return waitRun(t, jobs, id, func(r job.Run) bool { return r.ID == queued.ID && done(r) })
}

func TestToolJobRunsTheStoredCallWithoutTheModel(t *testing.T) {
	tool := &scriptTool{}
	a, e, turns := toolJobAgent(t, tool, "unused")
	var finished []events.JobRunFinishedPayload
	var mu sync.Mutex
	events.Subscribe(e.bus, events.JobRunFinished, func(_ kernel.Event, p events.JobRunFinishedPayload) {
		mu.Lock()
		finished = append(finished, p)
		mu.Unlock()
	})
	j, err := a.Jobs().Create(t.Context(), toolJob("report", map[string]any{"out": "42 rows"}, "", ""))
	if err != nil {
		t.Fatal(err)
	}
	if j.Action == nil || j.Action.Tool != "script" || j.Prompt != "" || j.Action.FollowUp != "" {
		t.Fatalf("job = %+v", j)
	}
	run := runOnce(t, a.Jobs(), j.ID)
	if run.Status != job.RunSucceeded || run.Output != "42 rows" || run.Action != job.ActionTool || run.Usage != nil {
		t.Fatalf("run = %+v", run)
	}
	if turns.Load() != 0 {
		t.Fatalf("the runner ran %d turns", turns.Load())
	}
	// The session shows what ran: the call, then its result.
	msgs, err := e.hub.Messages.Get(t.Context(), run.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 2 || len(msgs[0].ToolCalls) != 1 || msgs[0].ToolCalls[0].Name != "script" ||
		len(msgs[1].ToolResults) != 1 || msgs[1].ToolResults[0].Content != "42 rows" {
		t.Fatalf("messages = %+v", msgs)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(finished) != 1 || finished[0].Action != "tool" || finished[0].Output != "42 rows" || finished[0].Status != "succeeded" {
		t.Fatalf("finished = %+v", finished)
	}
}

func TestToolJobErrorFailsAndEmptyIsSilent(t *testing.T) {
	a, e, _ := toolJobAgent(t, &scriptTool{}, "unused")
	failing, _ := a.Jobs().Create(t.Context(), toolJob("failing", map[string]any{"fail": true}, "", ""))
	quiet, _ := a.Jobs().Create(t.Context(), toolJob("quiet", map[string]any{"out": ""}, "", ""))
	nothing, _ := a.Jobs().Create(t.Context(), toolJob("nothing", map[string]any{"quiet": true}, "", ""))

	if r := runOnce(t, a.Jobs(), failing.ID); r.Status != job.RunFailed || r.Error != "script failed: exit 1: boom" {
		t.Fatalf("failing = %+v", r)
	}
	if r := runOnce(t, a.Jobs(), quiet.ID); r.Status != job.RunSucceeded || !r.Silent {
		t.Fatalf("quiet = %+v", r)
	}
	r := runOnce(t, a.Jobs(), nothing.ID)
	if r.Status != job.RunSucceeded || !r.Silent || r.Output != "$ script\nexit code 0\n(no output)" {
		t.Fatalf("printed nothing = %+v", r)
	}
	msgs, _ := e.hub.Messages.Get(t.Context(), r.SessionID)
	if len(msgs) != 2 || msgs[1].ToolResults[0].IsError != nil {
		t.Fatalf("stored result = %+v", msgs)
	}
}

func TestToolJobFollowUps(t *testing.T) {
	a, _, turns := toolJobAgent(t, &scriptTool{}, "The script crashed on line 3.")
	ok, _ := a.Jobs().Create(t.Context(), toolJob("ok", map[string]any{"out": "fine"}, job.FollowUpOnFailure, "Explain the error"))
	bad, _ := a.Jobs().Create(t.Context(), toolJob("bad", map[string]any{"fail": true}, job.FollowUpOnFailure, "Explain the error"))

	if r := runOnce(t, a.Jobs(), ok.ID); r.Status != job.RunSucceeded || r.Output != "fine" || turns.Load() != 0 {
		t.Fatalf("ok = %+v, turns %d", r, turns.Load())
	}
	r := runOnce(t, a.Jobs(), bad.ID)
	if turns.Load() != 1 || r.Status != job.RunFailed || r.Error != "The script crashed on line 3." {
		t.Fatalf("bad = %+v, turns %d", r, turns.Load())
	}

	silentAgent, _, _ := toolJobAgent(t, &scriptTool{}, "[[SILENT]]")
	always, _ := silentAgent.Jobs().Create(t.Context(), toolJob("always", map[string]any{"out": "same"}, job.FollowUpAlways, "Report only changes"))
	if r := runOnce(t, silentAgent.Jobs(), always.ID); r.Status != job.RunSucceeded || !r.Silent {
		t.Fatalf("always = %+v", r)
	}
}

func TestToolJobConsequentialToolIsPreApproved(t *testing.T) {
	tool := &scriptTool{risk: toolhub.RiskConsequential}
	a, _, _ := toolJobAgent(t, tool, "unused")
	j, _ := a.Jobs().Create(t.Context(), toolJob("deploy", map[string]any{"out": "deployed"}, "", ""))
	if r := runOnce(t, a.Jobs(), j.ID); r.Status != job.RunSucceeded || r.Output != "deployed" || tool.calls() != 1 {
		t.Fatalf("run = %+v, calls %d", r, tool.calls())
	}
}

func TestToolJobRefusals(t *testing.T) {
	a, _, _ := toolJobAgent(t, &scriptTool{}, "unused")
	ctx := t.Context()
	cases := []struct {
		in   job.CreateInput
		want string
	}{
		{job.CreateInput{Name: "p", When: everyHour(), Action: &job.Action{Kind: job.ActionTool, Tool: "plain"}}, "can't run in a tool job " + "—" + " schedulable tools of this agent: script"},
		{job.CreateInput{Name: "m", When: everyHour(), Action: &job.Action{Kind: job.ActionTool, Tool: "missing"}}, "not one of this agent's tools"},
		{job.CreateInput{Name: "e", When: job.When{Kind: job.KindEvent}, Filter: &job.Filter{Types: []string{"job.failed"}}, Action: &job.Action{Kind: job.ActionTool, Tool: "script"}}, "Only a schedule job can run a tool"},
		{toolJob("f", nil, job.FollowUpAlways, ""), `needs a "prompt"`},
	}
	for _, c := range cases {
		if _, err := a.Jobs().Create(ctx, c.in); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("create %s: err = %v, want %q", c.in.Name, err, c.want)
		}
	}
	j, err := a.Jobs().Create(ctx, toolJob("report", map[string]any{"out": "x"}, "", ""))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Jobs().Update(ctx, j.ID, job.UpdateInput{ClearAction: true}); err == nil || !strings.Contains(err.Error(), `needs a "prompt"`) {
		t.Fatalf("to a prompt job without a prompt: %v", err)
	}
	prompt := "Summarise the report"
	asPrompt, err := a.Jobs().Update(ctx, j.ID, job.UpdateInput{ClearAction: true, Prompt: &prompt})
	if err != nil || asPrompt.Action != nil {
		t.Fatalf("to a prompt job: %+v %v", asPrompt, err)
	}
	back, err := a.Jobs().Update(ctx, j.ID, job.UpdateInput{Action: &job.Action{Kind: job.ActionTool, Tool: "script", Args: map[string]any{"out": "x"}}})
	if err != nil || back.Action == nil || back.Action.Tool != "script" {
		t.Fatalf("back to a tool job: %+v %v", back, err)
	}
	if _, err := a.Jobs().Update(ctx, j.ID, job.UpdateInput{When: &job.When{Kind: job.KindEvent}, Filter: &job.Filter{Types: []string{"job.failed"}}}); err == nil || !strings.Contains(err.Error(), "Only a schedule job") {
		t.Fatalf("to an event job: %v", err)
	}
}

func TestToolJobStopKillsTheTool(t *testing.T) {
	tool := &scriptTool{}
	a, _, _ := toolJobAgent(t, tool, "unused")
	j, _ := a.Jobs().Create(t.Context(), toolJob("hang", map[string]any{"hang": true}, "", ""))
	queued, err := a.Jobs().RunNow(t.Context(), j.ID)
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the tool to start", func() bool { return tool.calls() == 1 })
	if ok, err := a.Jobs().StopRun(t.Context(), queued.ID); err != nil || !ok {
		t.Fatalf("stop = %v %v", ok, err)
	}
	r := waitRun(t, a.Jobs(), j.ID, func(r job.Run) bool { return r.ID == queued.ID && done(r) })
	tool.mu.Lock()
	stopped := tool.ctxs[0].Err() != nil
	tool.mu.Unlock()
	if r.Status != job.RunStopped || !stopped {
		t.Fatalf("run = %+v, tool ctx stopped %v", r, stopped)
	}
}

// manage_job: what a strict-schema model sends for every field it doesn't mean.
func manageArgs(extra map[string]any) kernel.Arguments {
	args := map[string]any{
		"job": "", "name": "", "description": "", "prompt": "", "when": "schedule", "cron": "", "timezone": "", "every_minutes": 0,
		"at": "", "tool": "", "tool_args": "", "follow_up": "never", "session_mode": "new", "deliver": "none", "deliver_when": "always",
	}
	for k, v := range extra {
		args[k] = v
	}
	raw, _ := json.Marshal(args)
	return kernel.Arguments(raw)
}

func manageCall(t *testing.T, a *agent.Agent, args kernel.Arguments) toolhub.Output {
	t.Helper()
	out, err := jobs.NewManageTool().Execute(t.Context(), args, toolhub.Context{SessionID: "s1", AgentID: a.ID(), Jobs: a.Jobs()})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func findByName(t *testing.T, a *agent.Agent, name string) job.Job {
	t.Helper()
	list, err := a.Jobs().List(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for _, j := range list {
		if j.Name == name {
			return j
		}
	}
	t.Fatalf("no job %q", name)
	return job.Job{}
}

func TestManageJobToolJobs(t *testing.T) {
	a, _, _ := toolJobAgent(t, &scriptTool{}, "unused")
	out := manageCall(t, a, manageArgs(map[string]any{"action": "create", "name": "report", "every_minutes": 180, "tool": "script", "tool_args": `{"out": "python report.py"}`}))
	if out.IsError != nil && *out.IsError {
		t.Fatalf("create: %s", out.Content)
	}
	j := findByName(t, a, "report")
	if j.Prompt != "" || j.Action == nil || j.Action.Tool != "script" || j.Action.Args["out"] != "python report.py" {
		t.Fatalf("tool job = %+v", j)
	}
	// Strict-schema filler still makes a prompt job.
	out = manageCall(t, a, manageArgs(map[string]any{"action": "create", "name": "digest", "every_minutes": 60, "prompt": "Summarise"}))
	if out.IsError != nil && *out.IsError {
		t.Fatalf("create prompt job: %s", out.Content)
	}
	if d := findByName(t, a, "digest"); d.Action != nil {
		t.Fatalf("prompt job has an action: %+v", d.Action)
	}
	// Update only the parts given.
	manageCall(t, a, manageArgs(map[string]any{"action": "update", "job": "report", "tool": "script", "follow_up": "on-failure", "prompt": "Explain"}))
	if j := findByName(t, a, "report"); j.Action.FollowUp != job.FollowUpOnFailure || j.Action.Args["out"] != "python report.py" {
		t.Fatalf("after follow-up = %+v", j.Action)
	}
	manageCall(t, a, manageArgs(map[string]any{"action": "update", "job": "report", "tool_args": `{"out": "b"}`}))
	if j := findByName(t, a, "report"); j.Action.FollowUp != job.FollowUpOnFailure || j.Action.Args["out"] != "b" {
		t.Fatalf("after args = %+v", j.Action)
	}
	manageCall(t, a, manageArgs(map[string]any{"action": "update", "job": "report", "tool": "none"}))
	if j := findByName(t, a, "report"); j.Action != nil {
		t.Fatalf("after none = %+v", j.Action)
	}
	// Bad tool arguments are explained.
	for _, c := range []struct {
		args map[string]any
		want string
	}{
		{map[string]any{"action": "create", "name": "r", "every_minutes": 60, "tool": "script", "tool_args": "not json"}, "must be a JSON object"},
		{map[string]any{"action": "create", "name": "r", "every_minutes": 60, "tool": "script", "follow_up": "always"}, `follow-up needs a "prompt"`},
		{map[string]any{"action": "create", "name": "r", "every_minutes": 60, "tool_args": `{"a": 1}`, "prompt": "p"}, `go with "tool"`},
	} {
		out := manageCall(t, a, manageArgs(c.args))
		if !strings.Contains(out.Content, c.want) {
			t.Errorf("%v: %s", c.args, out.Content)
		}
	}
}
