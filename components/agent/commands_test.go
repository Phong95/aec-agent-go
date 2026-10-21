package agent_test

import (
	"context"
	"errors"
	"iter"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Phong95/aec-agent-go/components/agent"
	"github.com/Phong95/aec-agent-go/components/agent/command"
	"github.com/Phong95/aec-agent-go/components/agent/profile"
	"github.com/Phong95/aec-agent-go/components/events"
	"github.com/Phong95/aec-agent-go/components/llmhub"
	"github.com/Phong95/aec-agent-go/components/sandboxhub"
	"github.com/Phong95/aec-agent-go/components/toolhub"
	"github.com/Phong95/aec-agent-go/kernel"
	"github.com/Phong95/aec-agent-go/kernel/ordered"
)

// A-136: slash commands - the command manager's registry, parser and built-ins (the Go
// port of tests/unit/commands.test.ts of core/ts; the /diff, /review and /init tests over
// a stub workspace are in command_builtins_internal_test.go, the host commands of
// AgentCore.UseCommand in the root package).

// commandCall is one tool call the scripted model makes (nil = reply "done" there,
// ending that turn).
type commandCall *kernel.ToolCall

// commandModel makes the scripted tool calls (one per model call), then replies
// "done"; it answers summary requests too.
type commandModel struct {
	calls []commandCall

	mu             sync.Mutex
	step           int
	summaryPrompts []string
	toolNamesSeen  [][]string
}

func (*commandModel) Name() string                     { return "scripted-provider" }
func (*commandModel) Version() string                  { return "1.0.0" }
func (*commandModel) Initialize(context.Context) error { return nil }
func (*commandModel) Dispose(context.Context) error    { return nil }

func (p *commandModel) Chat(_ context.Context, _ []kernel.Message, opts llmhub.ModelOptions) (kernel.ModelResponse, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if strings.HasPrefix(opts.SystemPrompt, "You are a context-compaction worker") {
		p.summaryPrompts = append(p.summaryPrompts, opts.SystemPrompt)
		return kernel.ModelResponse{Message: kernel.Message{Role: kernel.RoleAssistant, Content: "## Goal\nsummary"}, Usage: kernel.TokenUsage{InputTokens: 5, OutputTokens: 5}, StopReason: kernel.StopEndTurn}, nil
	}
	var names []string
	for _, d := range opts.Tools {
		names = append(names, d.Name)
	}
	p.toolNamesSeen = append(p.toolNamesSeen, names)
	var call commandCall
	if p.step < len(p.calls) {
		call = p.calls[p.step]
	}
	p.step++
	if call != nil {
		c := *call
		c.ID = "tc-" + strconv.Itoa(p.step)
		return kernel.ModelResponse{
			Message:    kernel.Message{Role: kernel.RoleAssistant, ToolCalls: []kernel.ToolCall{c}},
			Usage:      kernel.TokenUsage{InputTokens: 10, OutputTokens: 5},
			StopReason: kernel.StopToolUse,
		}, nil
	}
	return kernel.ModelResponse{Message: kernel.Message{Role: kernel.RoleAssistant, Content: "done"}, Usage: kernel.TokenUsage{InputTokens: 1_000, OutputTokens: 50}, StopReason: kernel.StopEndTurn}, nil
}

func (p *commandModel) Stream(ctx context.Context, msgs []kernel.Message, opts llmhub.ModelOptions) iter.Seq2[kernel.StreamChunk, error] {
	return func(yield func(kernel.StreamChunk, error) bool) {
		resp, _ := p.Chat(ctx, msgs, opts)
		content := resp.Message.Content
		if content != "" && !yield(kernel.StreamChunk{Type: kernel.ChunkText, Content: &content}, nil) {
			return
		}
		usage := resp.Usage
		yield(kernel.StreamChunk{Type: kernel.ChunkDone, Usage: &usage}, nil)
	}
}

func (*commandModel) CountTokens(context.Context, []kernel.Message) (int, error) { return 10, nil }

func (*commandModel) ListModels(context.Context) ([]llmhub.ModelInfo, error) {
	return []llmhub.ModelInfo{{ModelRecord: llmhub.ModelRecord{ID: "model-a"}}, {ModelRecord: llmhub.ModelRecord{ID: "model-b"}}}, nil
}

func (p *commandModel) seen() ([]string, [][]string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.summaryPrompts), slices.Clone(p.toolNamesSeen)
}

// commandTool is a tool whose Execute is run.
type commandTool struct {
	name string
	risk toolhub.Risk
	run  func(ctx context.Context) (toolhub.Output, error)
	runs atomic.Int32
}

func (t *commandTool) Name() string  { return "tool:" + t.name }
func (*commandTool) Version() string { return "1.0.0" }
func (t *commandTool) Definition() toolhub.Definition {
	return toolhub.Definition{Name: t.name, Description: t.name, Parameters: toolhub.JSONSchema{Type: "object"}, Risk: t.risk}
}

func (t *commandTool) Execute(ctx context.Context, _ kernel.Arguments, _ toolhub.Context) (toolhub.Output, error) {
	t.runs.Add(1)
	return t.run(ctx)
}

type commandRig struct {
	e        *env
	a        *agent.Agent
	session  kernel.Session
	provider *commandModel

	mu     sync.Mutex
	events []kernel.Event
}

// newCommandRig builds an agent named "Site Assistant" over a scripted model, with the
// tools, no sandbox, and a session titled "First chat". hostCommands are its host's.
func newCommandRig(t *testing.T, calls []commandCall, tools ...*commandTool) *commandRig {
	t.Helper()
	r := &commandRig{e: newEnv(t, "unused"), provider: &commandModel{calls: calls}}
	r.e.llm = llmhub.NewLLMHub()
	mustOK(t, r.e.llm.Register(r.provider))
	p := testProfile("site-assistant")
	p.Name = "Site Assistant"
	p.Sandbox = sandboxhub.Selection{Mode: sandboxhub.SelectionNone}
	ten := 10
	p.Compactor = profile.CompactorConfig{
		ContextWindowTokens: 200_000, Threshold: 0.8, MinTailUserMessages: 4, ProtectFirstN: 1,
		ProactivePruneTokens: 50_000, OldToolResultMinTokens: 2_000, TailTargetRatio: 0.2,
		ProtectLastN: 2, LeanTailTokenBudget: &ten, SummarizeMaxOutputTokens: 1_000,
	}
	for _, tool := range tools {
		mustOK(t, r.e.tools.Register(tool))
		p.Tools = append(p.Tools, tool.name)
	}
	zero := 0
	p.LoopConfig = &profile.LoopConfig{MaxToolRetries: &zero, RetryBackoffMs: &zero}
	r.e.bus.OnAny(func(ev kernel.Event) {
		r.mu.Lock()
		r.events = append(r.events, ev)
		r.mu.Unlock()
	})
	r.a = r.e.spawn(t, p)
	s, err := r.a.StartSession(t.Context(), agent.WithMetadata(scope("agentId", "site-assistant", "title", "First chat")))
	mustOK(t, err)
	r.session = s
	return r
}

func scope(pairs ...string) ordered.Object {
	o := ordered.NewObject()
	for i := 0; i+1 < len(pairs); i += 2 {
		o.Set(pairs[i], pairs[i+1])
	}
	return o
}

// run runs a command in the rig's session from the web app.
func (r *commandRig) run(t *testing.T, text string, edit ...func(*command.Invocation)) command.Result {
	t.Helper()
	inv := command.Invocation{SessionID: r.session.ID, Client: "web", SessionScope: scope("agentId", "site-assistant")}
	for _, e := range edit {
		e(&inv)
	}
	return r.a.Commands().Run(t.Context(), text, inv)
}

func (r *commandRig) payloads(kind string) []any {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []any
	for _, ev := range r.events {
		if ev.Type == kind {
			out = append(out, ev.Payload)
		}
	}
	return out
}

func (r *commandRig) stored(t *testing.T, id string) kernel.Session {
	t.Helper()
	s, err := r.e.hub.Sessions.Get(t.Context(), id)
	mustOK(t, err)
	if s == nil {
		t.Fatalf("session %s is gone", id)
	}
	return *s
}

func (r *commandRig) chatIn(t *testing.T, text string, opts ...agent.ChatOption) {
	t.Helper()
	_, err := r.a.Chat(t.Context(), text, append([]agent.ChatOption{agent.WithChatSession(r.session.ID)}, opts...)...)
	mustOK(t, err)
}

func client(name string, caps ...command.ClientCapability) func(*command.Invocation) {
	return func(inv *command.Invocation) { inv.Client, inv.Capabilities = name, caps }
}

func confirmed(inv *command.Invocation) { inv.Confirmed = true }

func wantText(t *testing.T, got command.Result, want string) {
	t.Helper()
	if got.Text != want {
		t.Fatalf("text = %q, want %q (error %q)", got.Text, want, got.Error)
	}
}

func wantContains(t *testing.T, got command.Result, want string) {
	t.Helper()
	if !strings.Contains(got.Text, want) {
		t.Fatalf("text %q does not contain %q", got.Text, want)
	}
}

func wantError(t *testing.T, got command.Result, code command.ResultCode) {
	t.Helper()
	if got.Error != code {
		t.Fatalf("error = %q, want %q (text %q)", got.Error, code, got.Text)
	}
}

// blocker is a read-only tool that waits until released.
func blocker() (*commandTool, func(), <-chan struct{}) {
	release := make(chan struct{})
	started := make(chan struct{}, 1)
	return &commandTool{name: "wait", risk: toolhub.RiskReadOnly, run: func(context.Context) (toolhub.Output, error) {
		started <- struct{}{}
		<-release
		return toolhub.Output{Content: "ok"}, nil
	}}, func() { close(release) }, started
}

func toolCall(name string) commandCall {
	return &kernel.ToolCall{Name: name, Arguments: kernel.Arguments(`{}`)}
}

// --- The registry ----------------------------------------------------------------------

func TestCommandsListTheBuiltInsWithUsageAliasesAndAvailability(t *testing.T) {
	r := newCommandRig(t, nil)
	cmds := r.a.Commands()
	web := cmds.List(command.AvailabilityContext{Client: "web", Capabilities: []command.ClientCapability{command.CapabilityClipboard}})
	var names []string
	byName := map[string]command.Info{}
	for _, c := range web {
		names = append(names, c.Name)
		byName[c.Name] = c
	}
	want := []string{"help", "clear", "compact", "resume", "rename", "status", "context", "usage", "model", "permissions", "yolo", "plan", "init", "diff", "review", "copy", "exit"}
	if !slices.Equal(names, want) {
		t.Fatalf("names = %v", names)
	}
	if !slices.Equal(byName["clear"].Aliases, []string{"new", "reset"}) {
		t.Fatalf("clear aliases = %v", byName["clear"].Aliases)
	}
	if byName["rename"].Usage != "/rename <name>" || byName["permissions"].Usage != "/permissions [ask|yolo|plan]" {
		t.Fatalf("usage = %q, %q", byName["rename"].Usage, byName["permissions"].Usage)
	}
	if !byName["copy"].Available || byName["exit"].Available {
		t.Fatalf("copy %v, exit %v", byName["copy"].Available, byName["exit"].Available)
	}
	if info, _ := cmds.Get("copy", command.AvailabilityContext{Client: "channel"}); !strings.Contains(info.UnavailableReason, "clipboard") {
		t.Fatalf("copy in a channel = %+v", info)
	}
	if info, ok := cmds.Get("/new", command.AvailabilityContext{}); !ok || info.Name != "clear" {
		t.Fatalf("/new = %+v", info)
	}
	if !cmds.IsCommand("/reset") || cmds.IsCommand("/nope") {
		t.Fatal("IsCommand")
	}
}

func TestCommandsAnswerUserErrorsAndEmitCommandExecuted(t *testing.T) {
	r := newCommandRig(t, nil)
	got := r.run(t, "/stauts")
	wantText(t, got, "There is no command /stauts. Did you mean /status? Send /help to see the commands.")
	wantError(t, got, command.ResultUnknownCommand)
	got = r.run(t, "/rename")
	wantText(t, got, "/rename needs a name. Usage: /rename <name>")
	wantError(t, got, command.ResultInvalidArguments)
	wantError(t, r.run(t, "/permissions maybe"), command.ResultInvalidArguments)
	got = r.run(t, "/status now")
	wantText(t, got, "/status takes no arguments. Usage: /status")
	wantError(t, r.run(t, "hello"), command.ResultNotACommand)
	wantError(t, r.run(t, "/copy", client("channel")), command.ResultUnavailable)

	executed := r.payloads(events.CommandExecuted.Name)
	has := func(want events.CommandExecutedPayload) bool {
		return slices.ContainsFunc(executed, func(p any) bool {
			got, _ := p.(events.CommandExecutedPayload)
			return got.Name == want.Name && got.OK == want.OK && got.Error == want.Error && got.Client == want.Client && got.AgentID == "site-assistant"
		})
	}
	if !has(events.CommandExecutedPayload{Name: "stauts", Error: "unknown_command", Client: "web"}) ||
		!has(events.CommandExecutedPayload{Name: "copy", Error: "unavailable", Client: "channel"}) {
		t.Fatalf("CommandExecuted = %+v", executed)
	}
}

func TestCommandsRegisterAgentAndHostCommandsWithCollisionChecks(t *testing.T) {
	var host []command.HostCommand
	var hostMu sync.Mutex
	r := &commandRig{e: newEnv(t)}
	deps := r.e.deps()
	deps.HostCommands = func() []command.HostCommand {
		hostMu.Lock()
		defer hostMu.Unlock()
		return slices.Clone(host)
	}
	a, err := agent.New(t.Context(), deps, testProfile("cmd-agent"))
	mustOK(t, err)
	t.Cleanup(func() { _ = a.Dispose(context.WithoutCancel(t.Context())) })
	s, err := a.StartSession(t.Context())
	mustOK(t, err)
	run := func(text string) command.Result {
		return a.Commands().Run(t.Context(), text, command.Invocation{SessionID: s.ID, Client: "web"})
	}
	reply := func(text string) func(context.Context, command.RunContext) (command.Result, error) {
		return func(_ context.Context, rc command.RunContext) (command.Result, error) {
			return command.Result{Text: text + rc.Args}, nil
		}
	}
	cmds := a.Commands()
	mustOK(t, cmds.Register(command.Spec{Name: "standup", Description: "Daily summary", Safety: command.SafetyReadOnly, Prompt: "Summarize today's work: $ARGUMENTS"}, command.RegisterOptions{}))
	mustOK(t, cmds.Register(command.Spec{Name: "ping", Aliases: []string{"p"}, Description: "Pong", Safety: command.SafetyReadOnly, Run: reply("pong ")}, command.RegisterOptions{}))
	got := run("/standup site B")
	if got.Text != "Running /standup\u2026" || got.Action == nil || *got.Action != (command.Action{Type: command.ActionSendPrompt, Prompt: "Summarize today's work: site B"}) {
		t.Fatalf("/standup = %+v %+v", got, got.Action)
	}
	wantText(t, run("/p 1"), "pong 1")

	isCode := func(err error, code command.ErrorCode) bool {
		var ce *command.Error
		return errors.As(err, &ce) && ce.Code == code
	}
	if err := cmds.Register(command.Spec{Name: "status", Description: "x", Safety: command.SafetyReadOnly, Run: reply("")}, command.RegisterOptions{}); !isCode(err, command.CodeConflict) {
		t.Fatalf("/status: %v", err)
	}
	if err := cmds.Register(command.Spec{Name: "x", Aliases: []string{"new"}, Description: "x", Safety: command.SafetyReadOnly, Run: reply("")}, command.RegisterOptions{}); err == nil || !strings.Contains(err.Error(), "alias of /clear") {
		t.Fatalf("alias: %v", err)
	}
	if err := cmds.Register(command.Spec{Name: "Bad Name", Description: "x", Safety: command.SafetyReadOnly, Run: reply("")}, command.RegisterOptions{}); err == nil || !strings.Contains(err.Error(), "Invalid command name") {
		t.Fatalf("bad name: %v", err)
	}
	if err := cmds.Register(command.Spec{Name: "both", Description: "x", Safety: command.SafetyReadOnly, Prompt: "x", Run: reply("")}, command.RegisterOptions{}); err == nil || !strings.Contains(err.Error(), "exactly one") {
		t.Fatalf("both: %v", err)
	}

	// A host replaces the replaceable /usage (it has token accounting); every agent offers it.
	hostMu.Lock()
	host = append(host, command.HostCommand{Spec: command.Spec{Name: "usage", Description: "Tokens and cost", Safety: command.SafetyReadOnly, Run: reply("host usage")}, Options: command.RegisterOptions{Replace: true}})
	hostMu.Unlock()
	wantText(t, run("/usage"), "host usage")
	if info, _ := cmds.Get("usage", command.AvailabilityContext{}); info.Source != command.SourceHost {
		t.Fatalf("usage source = %q", info.Source)
	}
	if err := cmds.Register(command.Spec{Name: "usage", Description: "x", Safety: command.SafetyReadOnly, Run: reply("")}, command.RegisterOptions{}); !isCode(err, command.CodeConflict) {
		t.Fatalf("agent /usage: %v", err)
	}

	if !cmds.Unregister("ping") {
		t.Fatal("Unregister(ping) = false")
	}
	wantError(t, run("/ping"), command.ResultUnknownCommand)
}

func TestCommandsHelpListsEveryCommandAndExplainsOne(t *testing.T) {
	r := newCommandRig(t, nil)
	all := r.run(t, "/help").Text
	for _, want := range []string{"/compact [focus] \u2014 Summarize the conversation so far to free context", "/clear (/new, /reset) \u2014 "} {
		if !strings.Contains(all, want) {
			t.Fatalf("/help does not contain %q:\n%s", want, all)
		}
	}
	if !regexp.MustCompile("Not available here:\n/copy \u2014 this app can't copy to your clipboard").MatchString(all) {
		t.Fatalf("/help unavailable part:\n%s", all)
	}
	one := r.run(t, "/help permissions").Text
	for _, want := range []string{"/permissions [ask|yolo|plan]", "one of ask, yolo, plan", "asks you to confirm first"} {
		if !strings.Contains(one, want) {
			t.Fatalf("/help permissions does not contain %q:\n%s", want, one)
		}
	}
}

// --- Session and context ---------------------------------------------------------------

func TestCommandsClearStartsAFreshConversationAndResumeGoesBack(t *testing.T) {
	r := newCommandRig(t, nil)
	r.chatIn(t, "hello")

	cleared := r.run(t, "/new")
	if cleared.Action == nil || cleared.Action.Type != command.ActionOpenSession || cleared.Action.SessionID == r.session.ID {
		t.Fatalf("/new = %+v", cleared)
	}
	fresh := r.stored(t, cleared.Action.SessionID)
	if keys := fresh.Metadata.Keys(); !slices.Equal(keys, []string{"agentId"}) {
		t.Fatalf("fresh metadata keys = %v", keys)
	}
	if _, ok, err := r.a.LoadSession(t.Context(), r.session.ID); err != nil || !ok {
		t.Fatalf("the old session is gone: %v %v", ok, err)
	}

	inFresh := func(text string, withScope bool) command.Result {
		inv := command.Invocation{SessionID: fresh.ID, Client: "web"}
		if withScope {
			inv.SessionScope = scope("agentId", "site-assistant")
		}
		return r.a.Commands().Run(t.Context(), text, inv)
	}
	wantContains(t, inFresh("/resume", true), "1. First chat")
	back := inFresh("/resume 1", true)
	wantText(t, back, "Resumed \u201cFirst chat\u201d.")
	if back.Action == nil || *back.Action != (command.Action{Type: command.ActionOpenSession, SessionID: r.session.ID}) {
		t.Fatalf("/resume 1 action = %+v", back.Action)
	}
	if byTitle := inFresh("/resume first", false); byTitle.Action == nil || byTitle.Action.SessionID != r.session.ID {
		t.Fatalf("/resume first = %+v", byTitle)
	}
	wantError(t, inFresh("/resume nothing-like-it", false), command.ResultNotFound)
}

func TestCommandsRenameWhileATurnRunsAndEmitsSessionUpdated(t *testing.T) {
	wait, release, started := blocker()
	r := newCommandRig(t, []commandCall{toolCall("wait")}, wait)
	done := make(chan error, 1)
	go func() {
		_, err := r.a.Chat(context.WithoutCancel(t.Context()), "go", agent.WithChatSession(r.session.ID))
		done <- err
	}()
	<-started

	wantText(t, r.run(t, "/rename  Level 3   slab "), "Renamed this conversation to \u201cLevel 3 slab\u201d.")
	release()
	mustOK(t, <-done)
	// The turn's own session writes kept the new title.
	if title, _ := r.stored(t, r.session.ID).Metadata.Get("title"); title != "Level 3 slab" {
		t.Fatalf("title = %v", title)
	}
	updated := r.payloads(events.SessionUpdated.Name)
	if len(updated) == 0 {
		t.Fatal("no SessionUpdated")
	}
	if got, _ := updated[0].(events.SessionUpdatedPayload); got.AgentID != "site-assistant" || got.SessionID != r.session.ID || !slices.Equal(got.Fields, []string{"title"}) {
		t.Fatalf("SessionUpdated = %+v", updated[0])
	}
}

func TestCommandsCompactRefusesWhileATurnRunsThenSummarizesWithTheFocus(t *testing.T) {
	wait, release, started := blocker()
	r := newCommandRig(t, []commandCall{toolCall("wait")}, wait)
	done := make(chan error, 1)
	go func() {
		_, err := r.a.Chat(context.WithoutCancel(t.Context()), "first", agent.WithChatSession(r.session.ID))
		done <- err
	}()
	<-started
	wantError(t, r.run(t, "/compact"), command.ResultBusy)
	release()
	mustOK(t, <-done)
	for _, text := range []string{"second", "third", "fourth"} {
		r.chatIn(t, text)
	}
	usage, _, err := r.a.ContextUsage(t.Context(), r.session.ID)
	mustOK(t, err)
	if usage.UsedSource != "reported" {
		t.Fatalf("used source = %q", usage.UsedSource)
	}

	result := r.run(t, "/compact keep the RFI numbers")
	if result.Error != "" || !regexp.MustCompile(`^Compacted the conversation: \d+ messages \x{2192} \d+`).MatchString(result.Text) {
		t.Fatalf("/compact = %+v", result)
	}
	prompts, _ := r.provider.seen()
	if len(prompts) == 0 || !strings.Contains(prompts[len(prompts)-1], "The user asked this summary to focus on: keep the RFI numbers") {
		t.Fatalf("summary prompts = %q", prompts)
	}
	usage, _, err = r.a.ContextUsage(t.Context(), r.session.ID)
	mustOK(t, err)
	if usage.UsedSource != "estimated" {
		t.Fatalf("used source after = %q", usage.UsedSource)
	}
	// The full history is untouched.
	msgs, err := r.e.hub.Messages.Get(t.Context(), r.session.ID)
	mustOK(t, err)
	if len(msgs) != 10 {
		t.Fatalf("history = %d messages", len(msgs))
	}
	if updated := r.payloads(events.SessionUpdated.Name); !slices.ContainsFunc(updated, func(p any) bool {
		got, _ := p.(events.SessionUpdatedPayload)
		return slices.Equal(got.Fields, []string{"compactedMessages"})
	}) {
		t.Fatalf("SessionUpdated = %+v", updated)
	}
}

func TestCommandsCompactOnAShortConversationCompactsNothing(t *testing.T) {
	r := newCommandRig(t, nil)
	wantContains(t, r.run(t, "/compact"), "Nothing to compact yet")
}

func TestCommandsStatusContextAndUsageAreReadOnly(t *testing.T) {
	r := newCommandRig(t, nil)
	r.chatIn(t, "hello")
	before := r.stored(t, r.session.ID).UpdatedAt
	time.Sleep(2 * time.Millisecond)
	status := r.run(t, "/status")
	for _, want := range []string{"Agent: Site Assistant", "Conversation: First chat", "Permissions: ask \u2014 tools that need approval ask first", "Workspace: none"} {
		wantContains(t, status, want)
	}
	if !regexp.MustCompile(`Context: [\d.k]+ of 200k tokens`).MatchString(status.Text) {
		t.Fatalf("/status = %q", status.Text)
	}
	wantContains(t, r.run(t, "/context"), "\u2022 Messages:")
	wantContains(t, r.run(t, "/usage"), "Last response: 1k input, 50 output tokens.")
	if after := r.stored(t, r.session.ID).UpdatedAt; after != before {
		t.Fatalf("updatedAt changed: %d -> %d", before, after)
	}
}

func TestCommandsModelListsAndSwitchesTheConversationsModel(t *testing.T) {
	r := newCommandRig(t, nil)
	wantContains(t, r.run(t, "/model"), "\u2022 model-b")
	wantError(t, r.run(t, "/model nope"), command.ResultInvalidArguments)
	wantText(t, r.run(t, "/model MODEL-B"), "This conversation now uses model-b.")
	if m := r.stored(t, r.session.ID).LLMModel; m != "model-b" {
		t.Fatalf("model = %q", m)
	}
	wantContains(t, r.run(t, "/model default"), "the agent's model")
	wantContains(t, r.run(t, "/status"), "(the agent's default)")
}

func TestCommandsCopyReturnsTheLastReplyWhereTheClientHasAClipboard(t *testing.T) {
	r := newCommandRig(t, nil)
	clip := client("web", command.CapabilityClipboard)
	wantError(t, r.run(t, "/copy", clip), command.ResultNotFound)
	r.chatIn(t, "hello")
	got := r.run(t, "/copy", clip)
	wantText(t, got, "Copied the last reply.")
	if got.Action == nil || *got.Action != (command.Action{Type: command.ActionCopy, Text: "done"}) {
		t.Fatalf("/copy action = %+v", got.Action)
	}
}

// --- Permissions and plan mode ---------------------------------------------------------

func TestCommandsPermissionsYoloAsksToConfirmAndAJobSessionCantUseIt(t *testing.T) {
	r := newCommandRig(t, nil)
	ask := r.run(t, "/permissions yolo")
	if ask.Action == nil || *ask.Action != (command.Action{Type: command.ActionConfirm, Command: "/permissions yolo"}) {
		t.Fatalf("/permissions yolo = %+v", ask)
	}
	if m, _ := r.a.ApprovalMode(t.Context(), r.session.ID); m != kernel.ApprovalModeAsk {
		t.Fatalf("mode = %q", m)
	}
	wantText(t, r.run(t, "/permissions yolo", confirmed), "Permissions: YOLO \u2014 tools run without asking.")
	if m, _ := r.a.ApprovalMode(t.Context(), r.session.ID); m != kernel.ApprovalModeYolo {
		t.Fatalf("mode = %q", m)
	}
	wantContains(t, r.run(t, "/status"), "Permissions: YOLO")
	if changed := r.payloads(events.SessionApprovalModeChanged.Name); !slices.ContainsFunc(changed, func(p any) bool {
		got, _ := p.(events.SessionApprovalModeChangedPayload)
		return got.Mode == kernel.ApprovalModeYolo
	}) {
		t.Fatalf("SessionApprovalModeChanged = %+v", changed)
	}

	job, err := r.a.StartSession(t.Context(), agent.WithMetadata(scope("agentId", "site-assistant")))
	mustOK(t, err)
	job.JobID = "job-1"
	mustOK(t, r.e.hub.Sessions.Update(t.Context(), job))
	inJob := func(text string, edit ...func(*command.Invocation)) command.Result {
		inv := command.Invocation{SessionID: job.ID, Client: "web"}
		for _, e := range edit {
			e(&inv)
		}
		return r.a.Commands().Run(t.Context(), text, inv)
	}
	wantError(t, inJob("/permissions yolo", confirmed), command.ResultNotAllowed)
	// Plan mode only takes permissions away, so a job's session may use it.
	if got := inJob("/permissions plan"); got.Error != "" {
		t.Fatalf("/permissions plan in a job's session = %+v", got)
	}
}

func TestCommandsPlanModeOffersAndRunsOnlyReadOnlyTools(t *testing.T) {
	write := &commandTool{name: "write_drawing", risk: toolhub.RiskDraft, run: func(context.Context) (toolhub.Output, error) { return toolhub.Output{Content: "written"}, nil }}
	read := &commandTool{name: "read_drawing", risk: toolhub.RiskReadOnly, run: func(context.Context) (toolhub.Output, error) { return toolhub.Output{Content: "read"}, nil }}
	r := newCommandRig(t, []commandCall{toolCall("write_drawing"), toolCall("read_drawing")}, write, read)
	planned := r.run(t, "/plan list what to change")
	if planned.Action == nil || *planned.Action != (command.Action{Type: command.ActionSendPrompt, Prompt: "list what to change"}) {
		t.Fatalf("/plan = %+v", planned)
	}
	if m, _ := r.a.ApprovalMode(t.Context(), r.session.ID); m != kernel.ApprovalModePlan {
		t.Fatalf("mode = %q", m)
	}

	result, err := r.a.Chat(t.Context(), "list what to change", agent.WithChatSession(r.session.ID))
	mustOK(t, err)
	if result.Message.Content != "done" {
		t.Fatalf("reply = %q", result.Message.Content)
	}
	if write.runs.Load() != 0 || read.runs.Load() != 1 {
		t.Fatalf("write runs %d, read runs %d", write.runs.Load(), read.runs.Load())
	}
	if _, seen := r.provider.seen(); len(seen) == 0 || !slices.Equal(seen[0], []string{"read_drawing"}) {
		t.Fatalf("tools offered = %v", seen)
	}
	denied := r.payloads(events.ToolDenied.Name)
	if len(denied) == 0 {
		t.Fatal("no ToolDenied")
	}
	if got, _ := denied[0].(events.ToolDeniedPayload); got.Name != "write_drawing" || got.Reason == nil || !strings.Contains(*got.Reason, "plan mode") {
		t.Fatalf("ToolDenied = %+v", denied[0])
	}

	wantContains(t, r.run(t, "/plan off"), "Plan mode is off")
	if m, _ := r.a.ApprovalMode(t.Context(), r.session.ID); m != kernel.ApprovalModeAsk {
		t.Fatalf("mode = %q", m)
	}
}

func TestCommandsAReadOnlyTurnPlansEvenInAYoloSessionAndOnlyThatTurn(t *testing.T) {
	write := &commandTool{name: "write_drawing", risk: toolhub.RiskDraft, run: func(context.Context) (toolhub.Output, error) { return toolhub.Output{Content: "written"}, nil }}
	r := newCommandRig(t, []commandCall{toolCall("write_drawing"), nil, toolCall("write_drawing")}, write)
	if _, ok, err := r.a.SetApprovalMode(t.Context(), r.session.ID, kernel.ApprovalModeYolo); err != nil || !ok {
		t.Fatalf("SetApprovalMode: %v %v", ok, err)
	}
	r.chatIn(t, "review", agent.WithChatReadOnly())
	if n := write.runs.Load(); n != 0 {
		t.Fatalf("write runs = %d", n)
	}
	if m, _ := r.a.ApprovalMode(t.Context(), r.session.ID); m != kernel.ApprovalModeYolo {
		t.Fatalf("mode = %q", m)
	}
	r.chatIn(t, "now do it")
	if n := write.runs.Load(); n != 1 {
		t.Fatalf("write runs = %d", n)
	}
}
