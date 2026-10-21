package agent_test

import (
	"context"
	"errors"
	"iter"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Phong95/aec-agent-go/aecmock"
	"github.com/Phong95/aec-agent-go/components/agent"
	"github.com/Phong95/aec-agent-go/components/agent/profile"
	"github.com/Phong95/aec-agent-go/components/channelplatformhub"
	"github.com/Phong95/aec-agent-go/components/channelplatformhub/channel"
	"github.com/Phong95/aec-agent-go/components/llmhub"
	"github.com/Phong95/aec-agent-go/components/toolhub"
	"github.com/Phong95/aec-agent-go/components/toolhub/builtin/clarify"
	"github.com/Phong95/aec-agent-go/kernel"
)

// A-100 phase 2 - interactive channels: approvals and questions asked in the chat,
// streamed replies and tool progress, Markdown, group chats and /model.

// scriptedProvider streams a scripted turn: when toolName is set and the last message
// is the user's, it calls that tool; otherwise it answers with reply(lastUser,
// lastToolResult), written in pieces over time like a model.
type scriptedProvider struct {
	toolName string
	toolArgs string
	reply    func(lastUser, toolResult string) string

	mu        sync.Mutex
	models    []string
	lastModel string
}

var (
	_ llmhub.Provider    = (*scriptedProvider)(nil)
	_ llmhub.ModelLister = (*scriptedProvider)(nil)
)

func (*scriptedProvider) Name() string                     { return "scripted" }
func (*scriptedProvider) Version() string                  { return "1.0.0" }
func (*scriptedProvider) Initialize(context.Context) error { return nil }
func (*scriptedProvider) Dispose(context.Context) error    { return nil }
func (*scriptedProvider) CountTokens(context.Context, []kernel.Message) (int, error) {
	return 1, nil
}

func (p *scriptedProvider) ListModels(context.Context) ([]llmhub.ModelInfo, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.models == nil {
		return nil, errors.New("no catalog")
	}
	out := make([]llmhub.ModelInfo, 0, len(p.models))
	for _, id := range p.models {
		out = append(out, llmhub.ModelInfo{ModelRecord: llmhub.ModelRecord{ID: id, Provider: "scripted"}})
	}
	return out, nil
}

func (p *scriptedProvider) answer(msgs []kernel.Message) string {
	lastUser, toolResult := "", ""
	for i := len(msgs) - 1; i >= 0; i-- {
		if lastUser == "" && msgs[i].Role == kernel.RoleUser {
			lastUser = msgs[i].Content
		}
		if toolResult == "" && len(msgs[i].ToolResults) > 0 {
			toolResult = msgs[i].ToolResults[0].Content
		}
	}
	return p.reply(lastUser, toolResult)
}

func (p *scriptedProvider) wantsTool(msgs []kernel.Message) bool {
	return p.toolName != "" && len(msgs) > 0 && msgs[len(msgs)-1].Role == kernel.RoleUser
}

func (p *scriptedProvider) Chat(_ context.Context, msgs []kernel.Message, opts llmhub.ModelOptions) (kernel.ModelResponse, error) {
	p.mu.Lock()
	p.lastModel = opts.Model
	p.mu.Unlock()
	if p.wantsTool(msgs) {
		return kernel.ModelResponse{
			Message:    kernel.Message{Role: kernel.RoleAssistant, ToolCalls: []kernel.ToolCall{{ID: "tc-1", Name: p.toolName, Arguments: kernel.Arguments(p.toolArgs)}}},
			StopReason: kernel.StopToolUse,
		}, nil
	}
	return kernel.ModelResponse{Message: kernel.Message{Role: kernel.RoleAssistant, Content: p.answer(msgs)}, StopReason: kernel.StopEndTurn}, nil
}

func (p *scriptedProvider) Stream(ctx context.Context, msgs []kernel.Message, opts llmhub.ModelOptions) iter.Seq2[kernel.StreamChunk, error] {
	p.mu.Lock()
	p.lastModel = opts.Model
	p.mu.Unlock()
	return func(yield func(kernel.StreamChunk, error) bool) {
		if p.wantsTool(msgs) {
			call := kernel.ToolCall{ID: "tc-1", Name: p.toolName, Arguments: kernel.Arguments(p.toolArgs)}
			if !yield(kernel.StreamChunk{Type: kernel.ChunkToolCall, ToolCall: &call}, nil) {
				return
			}
			yield(kernel.StreamChunk{Type: kernel.ChunkDone, Usage: &kernel.TokenUsage{InputTokens: 1, OutputTokens: 1}}, nil)
			return
		}
		u := []rune(p.answer(msgs))
		for i := 0; i < len(u); i += 20 {
			// Written over time, like a model, so the chat can show a preview.
			time.Sleep(5 * time.Millisecond)
			if err := ctx.Err(); err != nil {
				yield(kernel.StreamChunk{}, err)
				return
			}
			piece := string(u[i:min(i+20, len(u))])
			if !yield(kernel.StreamChunk{Type: kernel.ChunkText, Content: &piece}, nil) {
				return
			}
		}
		yield(kernel.StreamChunk{Type: kernel.ChunkDone, Usage: &kernel.TokenUsage{InputTokens: 1, OutputTokens: 1}}, nil)
	}
}

func (p *scriptedProvider) model() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.lastModel
}

// dangerTool is a consequential tool that counts its runs.
type dangerTool struct{ runs atomic.Int32 }

func (*dangerTool) Name() string                     { return "tool:danger" }
func (*dangerTool) Version() string                  { return "1.0.0" }
func (*dangerTool) Initialize(context.Context) error { return nil }
func (*dangerTool) Dispose(context.Context) error    { return nil }
func (*dangerTool) Definition() toolhub.Definition {
	return toolhub.Definition{
		Name:        "delete_drawings",
		Description: "Delete the project's drawings.\n\nMore details nobody reads.",
		Parameters:  toolhub.JSONSchema{Type: "object"},
		Risk:        toolhub.RiskConsequential,
	}
}

func (d *dangerTool) Execute(context.Context, kernel.Arguments, toolhub.Context) (toolhub.Output, error) {
	d.runs.Add(1)
	return toolhub.Output{Content: "deleted"}, nil
}

type interactiveRig struct {
	a        *agent.Agent
	e        *env
	provider *scriptedProvider
	ch       channel.Channel
	adapter  *aecmock.FakeChannelAdapter
	runner   atomic.Pointer[channel.TurnRunner]
}

type interactiveOptions struct {
	provider *scriptedProvider
	tools    []toolhub.Handler
	caps     channel.Capabilities
	access   *channel.AccessInput
	settings *channel.SettingsInput
	host     toolhub.InteractionHandler
}

func newInteractiveRig(t *testing.T, o interactiveOptions) *interactiveRig {
	t.Helper()
	r := &interactiveRig{e: newEnv(t, "unused"), provider: o.provider}
	r.e.llm = llmhub.NewLLMHub()
	mustOK(t, r.e.llm.Register(o.provider))
	var names []string
	for _, tool := range o.tools {
		mustOK(t, r.e.tools.Register(tool))
		names = append(names, tool.Definition().Name)
	}
	if o.host != nil {
		r.e.tools.SetInteractionHandler(o.host)
	}
	if o.caps.MaxMessageLength == 0 {
		o.caps = channel.Capabilities{MaxMessageLength: 4000, Buttons: true}
	}
	hub := channelplatformhub.New()
	platform := aecmock.NewFakeChannelPlatformWithCapabilities("fake", o.caps)
	mustOK(t, hub.Register(platform))
	deps := r.e.deps()
	deps.ChannelPlatformHub = hub
	deps.ChannelRunner = func() channel.TurnRunner {
		if p := r.runner.Load(); p != nil {
			return *p
		}
		return nil
	}
	p := testProfile("site-assistant")
	p.Name = "Site Assistant"
	p.Tools = names
	retries := 0
	p.LoopConfig = &profile.LoopConfig{MaxToolRetries: &retries}
	a, err := agent.New(t.Context(), deps, p)
	mustOK(t, err)
	t.Cleanup(func() { _ = a.Dispose(context.WithoutCancel(t.Context())) })
	r.a = a

	access := o.access
	if access == nil {
		access = &channel.AccessInput{}
	}
	if access.DmPolicy == nil {
		open := channel.DmOpen
		access.DmPolicy = &open
	}
	r.ch, err = a.Channels().Create(t.Context(), channel.CreateInput{
		Platform: "fake", Credentials: map[string]string{"token": "t"}, Access: access, Settings: o.settings,
	})
	mustOK(t, err)
	r.adapter = platform.AdapterFor(r.ch.ID)
	return r
}

func lastText(a *aecmock.FakeChannelAdapter) string {
	texts := a.SentTexts()
	if len(texts) == 0 {
		return ""
	}
	return texts[len(texts)-1]
}

func findSent(a *aecmock.FakeChannelAdapter, match func(aecmock.FakeSentMessage) bool) (aecmock.FakeSentMessage, bool) {
	for _, m := range a.Sent() {
		if match(m) {
			return m, true
		}
	}
	return aecmock.FakeSentMessage{}, false
}

func hasButtons(m aecmock.FakeSentMessage) bool { return len(m.Buttons) > 0 }

const longReply = "This is a long reply that is written in several pieces, so the chat shows it while it is written."

func TestChannelStreamsAReplyByEditingOneMessage(t *testing.T) {
	r := newInteractiveRig(t, interactiveOptions{provider: &scriptedProvider{reply: func(string, string) string { return longReply }}})
	mustOK(t, r.adapter.Receive(aecmock.FakeDirectMessage("u1", "hello")))
	waitTexts(t, r.adapter, longReply)
	if sent := r.adapter.Sent(); sent[0].Edits < 1 {
		t.Fatalf("edits = %d", sent[0].Edits)
	}
}

func TestChannelSendsMarkdownForAnAdapterThatRendersIt(t *testing.T) {
	r := newInteractiveRig(t, interactiveOptions{
		provider: &scriptedProvider{reply: func(string, string) string { return "**done**" }},
		caps:     channel.Capabilities{MaxMessageLength: 4000, Markdown: true},
	})
	mustOK(t, r.adapter.Receive(aecmock.FakeDirectMessage("u1", "hello")))
	waitTexts(t, r.adapter, "**done**")
	if f := r.adapter.Sent()[0].Format; f != channel.FormatMarkdown {
		t.Fatalf("format = %q", f)
	}
}

func TestChannelWithStreamingOffSendsTheReplyWhenDone(t *testing.T) {
	off := false
	r := newInteractiveRig(t, interactiveOptions{
		provider: &scriptedProvider{reply: func(string, string) string { return longReply }},
		settings: &channel.SettingsInput{Streaming: &off},
	})
	mustOK(t, r.adapter.Receive(aecmock.FakeDirectMessage("u1", "hello")))
	waitTexts(t, r.adapter, longReply)
	if e := r.adapter.Sent()[0].Edits; e != 0 {
		t.Fatalf("edits = %d", e)
	}
}

func TestChannelKeepsOneToolProgressMessage(t *testing.T) {
	r := newInteractiveRig(t, interactiveOptions{provider: &scriptedProvider{reply: func(string, string) string { return "ok" }}})
	var run channel.TurnRunner = func(_ context.Context, tc channel.TurnContext) (channel.TurnResult, error) {
		yes := true
		tc.OnChunk(kernel.StreamChunk{Type: kernel.ChunkToolCall, ToolCall: &kernel.ToolCall{ID: "a", Name: "web_search"}})
		tc.OnChunk(kernel.StreamChunk{Type: kernel.ChunkToolCall, ToolCall: &kernel.ToolCall{ID: "b", Name: "read_file"}})
		tc.OnChunk(kernel.StreamChunk{Type: kernel.ChunkToolResult, ToolResult: &kernel.ToolResult{ToolCallID: "a", Content: "x"}})
		tc.OnChunk(kernel.StreamChunk{Type: kernel.ChunkToolResult, ToolResult: &kernel.ToolResult{ToolCallID: "b", Content: "x", IsError: &yes}})
		return channel.TurnResult{Output: "Found it."}, nil
	}
	r.runner.Store(&run)
	mustOK(t, r.adapter.Receive(aecmock.FakeDirectMessage("u1", "search")))
	check, warn := string(rune(0x2705)), string([]rune{0x26a0, 0xfe0f})
	waitTexts(t, r.adapter, check+" web_search\n"+warn+" read_file", "Found it.")
}

func TestChannelAsksForApprovalWithButtonsAndApproveRunsTheTool(t *testing.T) {
	tool := &dangerTool{}
	r := newInteractiveRig(t, interactiveOptions{
		provider: &scriptedProvider{toolName: "delete_drawings", toolArgs: `{"folder":"old"}`, reply: func(_, res string) string { return "result: " + res }},
		tools:    []toolhub.Handler{tool},
	})
	mustOK(t, r.adapter.Receive(aecmock.FakeDirectMessage("u1", "clean up")))
	waitFor(t, "the approval prompt", func() bool { _, ok := findSent(r.adapter, hasButtons); return ok })
	prompt, _ := findSent(r.adapter, hasButtons)
	for _, want := range []string{"Site Assistant wants to run delete_drawings.", "Delete the project's drawings.", `"folder": "old"`} {
		if !strings.Contains(prompt.Text, want) {
			t.Fatalf("prompt %q lacks %q", prompt.Text, want)
		}
	}
	if strings.Contains(prompt.Text, "More details") {
		t.Fatalf("prompt has the whole description: %q", prompt.Text)
	}

	mustOK(t, r.adapter.Press(string(rune(0x2705))+" Approve", channel.Sender{ID: "u1", Name: "Ann"}, channel.ChatDM))
	waitFor(t, "the reply", func() bool { return lastText(r.adapter) == "result: deleted" })
	if tool.runs.Load() != 1 {
		t.Fatalf("runs = %d", tool.runs.Load())
	}
	waitFor(t, "the settled prompt", func() bool {
		m, _ := findSent(r.adapter, func(m aecmock.FakeSentMessage) bool { return m.MessageID == prompt.MessageID })
		return len(m.Buttons) == 0 && strings.Contains(m.Text, string(rune(0x2705))+" Approved by Ann")
	})
}

func TestChannelDenyWithAReasonDeniesTheTool(t *testing.T) {
	tool := &dangerTool{}
	r := newInteractiveRig(t, interactiveOptions{
		provider: &scriptedProvider{toolName: "delete_drawings", toolArgs: `{}`, reply: func(_, res string) string { return "result: " + res }},
		tools:    []toolhub.Handler{tool},
	})
	mustOK(t, r.adapter.Receive(aecmock.FakeDirectMessage("u1", "clean up")))
	waitFor(t, "the approval prompt", func() bool {
		_, ok := findSent(r.adapter, func(m aecmock.FakeSentMessage) bool { return strings.HasPrefix(m.Text, "Approval needed (#") })
		return ok
	})
	mustOK(t, r.adapter.Receive(aecmock.FakeDirectMessage("u1", "/deny not today")))
	waitFor(t, "the reply", func() bool {
		return strings.HasPrefix(lastText(r.adapter), "result: Approval denied: user declined (not today)")
	})
	if tool.runs.Load() != 0 {
		t.Fatal("the tool ran")
	}
	waitFor(t, "the settled prompt", func() bool {
		m, _ := findSent(r.adapter, func(m aecmock.FakeSentMessage) bool { return strings.HasPrefix(m.Text, "Approval needed") })
		return strings.Contains(m.Text, string(rune(0x274c))+" Denied by User u1: not today")
	})
}

func TestChannelGroupApprovalsOnlyBySendersInAllowFrom(t *testing.T) {
	tool := &dangerTool{}
	open := channel.GroupOpen
	r := newInteractiveRig(t, interactiveOptions{
		provider: &scriptedProvider{toolName: "delete_drawings", toolArgs: `{}`, reply: func(_, res string) string { return "result: " + res }},
		tools:    []toolhub.Handler{tool},
		access:   &channel.AccessInput{GroupPolicy: &open, AllowFrom: []string{"boss"}},
	})
	msg := aecmock.FakeGroupMessage("g1", "worker", "clean up")
	msg.MentionsBot = true
	mustOK(t, r.adapter.Receive(msg))
	waitFor(t, "the approval prompt", func() bool { _, ok := findSent(r.adapter, hasButtons); return ok })
	approve := string(rune(0x2705)) + " Approve"
	mustOK(t, r.adapter.Press(approve, channel.Sender{ID: "worker"}, channel.ChatGroup))
	waitFor(t, "the refusal", func() bool { return lastText(r.adapter) == "You are not allowed to approve tools here." })
	mustOK(t, r.adapter.Press(approve, channel.Sender{ID: "boss", Name: "Boss"}, channel.ChatGroup))
	waitFor(t, "the reply", func() bool { return lastText(r.adapter) == "result: deleted" })
}

func TestChannelClarifyQuestionIsAnsweredByTheNextMessage(t *testing.T) {
	r := newInteractiveRig(t, interactiveOptions{
		provider: &scriptedProvider{
			toolName: "clarify", toolArgs: `{"question":"Which tower?","choices":["Tower A","Tower B"]}`,
			reply: func(_, res string) string { return "answer: " + res },
		},
		tools: []toolhub.Handler{clarify.NewTool()},
	})
	mustOK(t, r.adapter.Receive(aecmock.FakeDirectMessage("u1", "check the tower")))
	isQuestion := func(m aecmock.FakeSentMessage) bool { return strings.HasPrefix(m.Text, "Question (#") }
	waitFor(t, "the question", func() bool { _, ok := findSent(r.adapter, isQuestion); return ok })
	q, _ := findSent(r.adapter, isQuestion)
	if !strings.Contains(q.Text, "Which tower?\n\n1. Tower A\n2. Tower B") || len(q.Buttons) != 2 || q.Buttons[1][0].Label != "Tower B" {
		t.Fatalf("question = %+v", q)
	}
	mustOK(t, r.adapter.Receive(aecmock.FakeDirectMessage("u1", "2")))
	waitFor(t, "the reply", func() bool {
		last := lastText(r.adapter)
		return strings.HasPrefix(last, "answer: ") && strings.Contains(last, "Tower B")
	})
	q, _ = findSent(r.adapter, isQuestion)
	if !strings.Contains(q.Text, string(rune(0x1f4ac))+" User u1: Tower B") {
		t.Fatalf("settled question = %q", q.Text)
	}
}

type countingInteraction struct{ approvals atomic.Int32 }

func (*countingInteraction) AskUser(context.Context, toolhub.ClarifyRequest) (toolhub.ClarifyResponse, error) {
	return toolhub.ClarifyResponse{}, nil
}

func (c *countingInteraction) RequestApproval(context.Context, toolhub.ApprovalRequest) (toolhub.ApprovalDecision, error) {
	c.approvals.Add(1)
	return toolhub.ApprovalDecision{Approved: true}, nil
}

func TestASessionNotInAChannelTurnKeepsTheHostHandler(t *testing.T) {
	host := &countingInteraction{}
	r := newInteractiveRig(t, interactiveOptions{
		provider: &scriptedProvider{toolName: "delete_drawings", toolArgs: `{}`, reply: func(_, res string) string { return "result: " + res }},
		tools:    []toolhub.Handler{&dangerTool{}},
		host:     host,
	})
	s, err := r.a.StartSession(t.Context())
	mustOK(t, err)
	res, err := r.a.Chat(t.Context(), "clean up", agent.WithChatSession(s.ID))
	mustOK(t, err)
	if res.Message.Content != "result: deleted" || host.approvals.Load() != 1 {
		t.Fatalf("reply = %q, host approvals = %d", res.Message.Content, host.approvals.Load())
	}
}

func TestChannelGroupsAreIgnoredByDefaultButWhoamiShowsTheGroupID(t *testing.T) {
	r := newInteractiveRig(t, interactiveOptions{provider: &scriptedProvider{reply: func(u, _ string) string { return "echo " + u }}})
	hello := aecmock.FakeGroupMessage("g1", "u1", "hello")
	hello.MentionsBot = true
	mustOK(t, r.adapter.Receive(hello))
	mustOK(t, r.adapter.Receive(aecmock.FakeGroupMessage("g1", "u1", "/whoami")))
	waitTexts(t, r.adapter, "Your Fake id is u1.\nThis group's id is g1.")
}

func TestChannelAllowedGroupAnswersMentionsAndReplies(t *testing.T) {
	allow := channel.GroupAllowlist
	r := newInteractiveRig(t, interactiveOptions{
		provider: &scriptedProvider{reply: func(u, _ string) string { return "echo " + u }},
		access:   &channel.AccessInput{GroupPolicy: &allow, Groups: []string{"g1"}},
	})
	other := aecmock.FakeGroupMessage("g2", "u1", "@fake_bot other group")
	other.MentionsBot = true
	mustOK(t, r.adapter.Receive(other))
	mustOK(t, r.adapter.Receive(aecmock.FakeGroupMessage("g1", "u1", "not for the bot")))
	hello := aecmock.FakeGroupMessage("g1", "u1", "@fake_bot hello there")
	hello.MentionsBot = true
	mustOK(t, r.adapter.Receive(hello))
	waitTexts(t, r.adapter, "echo [User u1]: hello there")
	reply := aecmock.FakeGroupMessage("g1", "u2", "and me")
	reply.ReplyToBot = true
	mustOK(t, r.adapter.Receive(reply))
	waitTexts(t, r.adapter, "echo [User u1]: hello there", "echo [User u2]: and me")
}

func TestChannelGroupWithoutRequireMentionSharesOneSession(t *testing.T) {
	open, no := channel.GroupOpen, false
	r := newInteractiveRig(t, interactiveOptions{
		provider: &scriptedProvider{reply: func(u, _ string) string { return "echo " + u }},
		access:   &channel.AccessInput{GroupPolicy: &open, RequireMention: &no},
	})
	mustOK(t, r.adapter.Receive(aecmock.FakeGroupMessage("g1", "u1", "one")))
	mustOK(t, r.adapter.Receive(aecmock.FakeGroupMessage("g1", "u2", "two")))
	waitFor(t, "two replies", func() bool { return len(r.adapter.SentTexts()) == 2 })
	sessions := channelSessions(t, r.a)
	if len(sessions) != 1 {
		t.Fatalf("sessions = %d", len(sessions))
	}
	meta := sessions[0].Metadata
	if v, _ := meta.Get("chatType"); v != "group" {
		t.Fatalf("chatType = %v", v)
	}
	if v, _ := meta.Get("title"); v != "Group g1 "+string(rune(0x00b7))+" Fake" {
		t.Fatalf("title = %v", v)
	}
}

func TestChannelValidatesGroupAccess(t *testing.T) {
	r := newInteractiveRig(t, interactiveOptions{provider: &scriptedProvider{reply: func(string, string) string { return "" }}})
	bad := channel.GroupPolicy("everyone")
	if _, err := r.a.Channels().Update(t.Context(), r.ch.ID, channel.UpdateInput{Access: &channel.AccessInput{GroupPolicy: &bad}}); err == nil || !strings.Contains(err.Error(), "groupPolicy") {
		t.Fatalf("err = %v", err)
	}
	off := false
	updated, err := r.a.Channels().Update(t.Context(), r.ch.ID, channel.UpdateInput{
		Access: &channel.AccessInput{Groups: []string{" g1 ", "g1"}}, Settings: &channel.SettingsInput{ToolProgress: &off},
	})
	mustOK(t, err)
	if !slices.Equal(updated.Access.Groups, []string{"g1"}) || updated.Settings != (channel.Settings{Streaming: true, ToolProgress: false}) {
		t.Fatalf("updated = %+v", updated)
	}
	if !r.ch.Access.RequireMention || r.ch.Access.GroupPolicy != channel.GroupDisabled {
		t.Fatalf("defaults = %+v", r.ch.Access)
	}
}

func TestChannelModelCommandShowsAndSwitchesTheModel(t *testing.T) {
	p := &scriptedProvider{reply: func(string, string) string { return "hi" }, models: []string{"model-a", "model-b"}}
	r := newInteractiveRig(t, interactiveOptions{provider: p})
	mustOK(t, r.adapter.Receive(aecmock.FakeDirectMessage("u1", "/model")))
	waitFor(t, "the list", func() bool { return len(r.adapter.SentTexts()) == 1 })
	bulletSign := string(rune(0x2022))
	if !strings.Contains(r.adapter.SentTexts()[0], bulletSign+" model-a\n"+bulletSign+" model-b") {
		t.Fatalf("list = %q", r.adapter.SentTexts()[0])
	}
	mustOK(t, r.adapter.Receive(aecmock.FakeDirectMessage("u1", "/model nope")))
	mustOK(t, r.adapter.Receive(aecmock.FakeDirectMessage("u1", "/model MODEL-B")))
	waitFor(t, "two answers", func() bool { return len(r.adapter.SentTexts()) == 3 })
	if got := r.adapter.SentTexts()[1:]; !slices.Equal(got, []string{`There is no model "nope". Send /model to see the list.`, "This conversation now uses model-b."}) {
		t.Fatalf("answers = %q", got)
	}
	if s := channelSessions(t, r.a); len(s) != 1 || s[0].LLMModel != "model-b" {
		t.Fatalf("sessions = %+v", s)
	}
	mustOK(t, r.adapter.Receive(aecmock.FakeDirectMessage("u1", "hello")))
	waitFor(t, "the reply", func() bool { return lastText(r.adapter) == "hi" })
	if p.model() != "model-b" {
		t.Fatalf("model = %q", p.model())
	}
	mustOK(t, r.adapter.Receive(aecmock.FakeDirectMessage("u1", "/model default")))
	waitFor(t, "the reset", func() bool { return lastText(r.adapter) == "This conversation now uses the agent's model." })
}

// A-135: /yolo.
func TestChannelYoloCommandLetsOwnersRunToolsWithoutAPrompt(t *testing.T) {
	tool := &dangerTool{}
	r := newInteractiveRig(t, interactiveOptions{
		provider: &scriptedProvider{toolName: "delete_drawings", toolArgs: `{"folder":"old"}`, reply: func(_, res string) string { return "result: " + res }},
		tools:    []toolhub.Handler{tool},
		access:   &channel.AccessInput{AllowFrom: []string{"owner"}},
	})

	mustOK(t, r.adapter.Receive(aecmock.FakeDirectMessage("owner", "/yolo")))
	waitFor(t, "the mode", func() bool { return len(r.adapter.SentTexts()) == 1 })
	if got := r.adapter.SentTexts()[0]; !strings.Contains(got, "YOLO mode is off") {
		t.Fatalf("/yolo = %q", got)
	}

	mustOK(t, r.adapter.Receive(aecmock.FakeDirectMessage("guest", "/yolo on")))
	waitFor(t, "the refusal", func() bool { return len(r.adapter.SentTexts()) == 2 })
	if got := r.adapter.SentTexts()[1]; got != "Only the bot's owners (the senders on its allow list) can switch YOLO mode." {
		t.Fatalf("guest /yolo on = %q", got)
	}

	mustOK(t, r.adapter.Receive(aecmock.FakeDirectMessage("owner", "/yolo on")))
	waitFor(t, "the switch", func() bool { return len(r.adapter.SentTexts()) == 3 })
	if got := r.adapter.SentTexts()[2]; !strings.Contains(got, "YOLO mode is on") {
		t.Fatalf("/yolo on = %q", got)
	}
	// The owner's chat is in YOLO mode, the guest's is not.
	var yolo []kernel.Session
	for _, s := range channelSessions(t, r.a) {
		if mode, err := r.a.ApprovalMode(t.Context(), s.ID); err == nil && mode == kernel.ApprovalModeYolo {
			yolo = append(yolo, s)
		}
	}
	if len(yolo) != 1 {
		t.Fatalf("sessions in YOLO mode = %d", len(yolo))
	}

	mustOK(t, r.adapter.Receive(aecmock.FakeDirectMessage("owner", "clean up")))
	waitFor(t, "the reply", func() bool { return lastText(r.adapter) == "result: deleted" })
	if n := tool.runs.Load(); n != 1 {
		t.Fatalf("runs = %d", n)
	}
	if _, ok := findSent(r.adapter, hasButtons); ok {
		t.Fatal("an approval prompt was sent")
	}

	mustOK(t, r.adapter.Receive(aecmock.FakeDirectMessage("owner", "/status")))
	// A-136: /status is the command manager's now - the approval mode is its "Permissions" line.
	waitFor(t, "the status", func() bool {
		return strings.Contains(lastText(r.adapter), "Permissions: YOLO \u2014 tools run without asking")
	})

	mustOK(t, r.adapter.Receive(aecmock.FakeDirectMessage("owner", "/yolo off")))
	waitFor(t, "the switch back", func() bool {
		return lastText(r.adapter) == "YOLO mode is off: tools that need approval ask here first."
	})
	if mode, err := r.a.ApprovalMode(t.Context(), yolo[0].ID); err != nil || mode != kernel.ApprovalModeAsk {
		t.Fatalf("mode = %q, %v", mode, err)
	}
}
