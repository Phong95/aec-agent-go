package aecmock_test

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/Phong95/aec-agent-go/aecmock"
	"github.com/Phong95/aec-agent-go/components/agent/agentloop"
	"github.com/Phong95/aec-agent-go/components/agent/memorysystem/memory"
	"github.com/Phong95/aec-agent-go/components/agent/profile"
	"github.com/Phong95/aec-agent-go/components/llmhub"
	"github.com/Phong95/aec-agent-go/components/persistence"
	"github.com/Phong95/aec-agent-go/components/toolhub"
	"github.com/Phong95/aec-agent-go/kernel"
	"github.com/Phong95/aec-agent-go/kernel/ordered"
)

// Each fake satisfies the agentloop port it stands in for.
var (
	_ agentloop.Models         = (*aecmock.ScriptedModels)(nil)
	_ agentloop.OptionResolver = (*aecmock.ScriptedModels)(nil)
	_ agentloop.ProviderNamer  = (*aecmock.ScriptedModels)(nil)
	_ agentloop.Conversation   = (*aecmock.Conversation)(nil)
	_ agentloop.Tools          = (*aecmock.HubTools)(nil)
	_ agentloop.Skills         = (*aecmock.SkillTools)(nil)
	_ agentloop.Context        = (*aecmock.PromptContext)(nil)
	_ agentloop.Context        = aecmock.PassthroughContext{}
	_ agentloop.Memory         = (*aecmock.ScriptedMemory)(nil)
)

func call(id, name, args string) kernel.ToolCall {
	return kernel.ToolCall{ID: id, Name: name, Arguments: kernel.Arguments(args)}
}

func TestScriptedModels_ChatFollowsTheScript(t *testing.T) {
	custom := &kernel.TokenUsage{InputTokens: 7, OutputTokens: 3}
	m := aecmock.NewScriptedModels(
		aecmock.ScriptedTurn{Content: "one"},
		aecmock.ScriptedTurn{Content: "two", ToolCalls: []kernel.ToolCall{call("c1", "t", `{"a":1}`)}},
		aecmock.ScriptedTurn{Content: "three", StopReason: kernel.StopMaxTokens, Usage: custom},
	)
	ctx := t.Context()

	r1, err := m.Chat(ctx, []kernel.Message{{Role: kernel.RoleUser, Content: "hi"}}, llmhub.ModelOptions{Model: "x"})
	if err != nil || r1.Message.Content != "one" || r1.Message.Role != kernel.RoleAssistant || r1.StopReason != kernel.StopEndTurn {
		t.Fatalf("turn 1 = %+v, %v", r1, err)
	}
	if r1.Usage.InputTokens != 10 || r1.Usage.OutputTokens != 5 || r1.Usage.CacheReadTokens != nil {
		t.Fatalf("default usage = %+v, want 10 in and 5 out, no cache fields", r1.Usage)
	}

	r2, err := m.Chat(ctx, nil, llmhub.ModelOptions{})
	if err != nil || r2.StopReason != kernel.StopToolUse || len(r2.Message.ToolCalls) != 1 || string(r2.Message.ToolCalls[0].Arguments) != `{"a":1}` {
		t.Fatalf("turn 2 = %+v, %v (tool calls imply tool_use)", r2, err)
	}

	r3, err := m.Chat(ctx, nil, llmhub.ModelOptions{})
	if err != nil || r3.StopReason != kernel.StopMaxTokens || r3.Usage.InputTokens != 7 {
		t.Fatalf("turn 3 = %+v, %v", r3, err)
	}
	custom.InputTokens = 99 // the scripted usage is copied, not shared
	if r3.Usage.InputTokens != 7 {
		t.Fatal("the response shares the scripted usage")
	}

	if _, err := m.Chat(ctx, nil, llmhub.ModelOptions{}); err == nil {
		t.Fatal("a call past the last turn must fail")
	}
}

func TestScriptedModels_ChatErrAndCancel(t *testing.T) {
	boom := kernel.NewModelError("boom", kernel.ModelCodeProvider, false)
	m := aecmock.NewScriptedModels(aecmock.ScriptedTurn{Err: boom}, aecmock.ScriptedTurn{Content: "after"})
	if _, err := m.Chat(t.Context(), nil, llmhub.ModelOptions{}); !errors.Is(err, boom) {
		t.Fatalf("Chat error = %v, want the scripted one", err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := m.Chat(ctx, nil, llmhub.ModelOptions{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("Chat on a cancelled ctx = %v, want the bare ctx error", err)
	}
	// The cancelled call did not use the next turn.
	if r, err := m.Chat(t.Context(), nil, llmhub.ModelOptions{}); err != nil || r.Message.Content != "after" {
		t.Fatalf("next turn = %+v, %v", r, err)
	}
}

func TestScriptedModels_RecordsCopies(t *testing.T) {
	m := aecmock.NewScriptedModels(aecmock.ScriptedTurn{Content: "a"})
	msgs := []kernel.Message{{Role: kernel.RoleAssistant, ToolCalls: []kernel.ToolCall{call("c", "t", `{}`)}}}
	if _, err := m.Chat(t.Context(), msgs, llmhub.ModelOptions{Model: "m"}); err != nil {
		t.Fatal(err)
	}
	msgs[0].ToolCalls[0].Name = "changed"
	calls := m.Calls()
	if len(calls) != 1 || calls[0].Messages[0].ToolCalls[0].Name != "t" || calls[0].Options.Model != "m" {
		t.Fatalf("recorded calls = %+v", calls)
	}
	calls[0].Messages[0].ToolCalls[0].Name = "changed too"
	if m.Calls()[0].Messages[0].ToolCalls[0].Name != "t" {
		t.Fatal("Calls() exposed its internal state")
	}
}

func drain(ctx context.Context, t *testing.T, m *aecmock.ScriptedModels) ([]kernel.StreamChunk, error) {
	t.Helper()
	var out []kernel.StreamChunk
	for c, err := range m.Stream(ctx, nil, llmhub.ModelOptions{}) {
		if err != nil {
			return out, err
		}
		out = append(out, c)
	}
	return out, nil
}

func TestScriptedModels_StreamDefaultShape(t *testing.T) {
	m := aecmock.NewScriptedModels(
		aecmock.ScriptedTurn{Content: "Hello back", ToolCalls: []kernel.ToolCall{call("c1", "t", `{}`)}},
		aecmock.ScriptedTurn{Content: "x", Usage: &kernel.TokenUsage{InputTokens: 4, OutputTokens: 2}},
	)
	chunks, err := drain(t.Context(), t, m)
	if err != nil {
		t.Fatal(err)
	}
	var types []kernel.ChunkType
	var text string
	for _, c := range chunks {
		types = append(types, c.Type)
		if c.Type == kernel.ChunkText {
			text += *c.Content
		}
	}
	wantTypes := []kernel.ChunkType{kernel.ChunkText, kernel.ChunkText, kernel.ChunkToolCall, kernel.ChunkDone}
	if !slices.Equal(types, wantTypes) {
		t.Fatalf("chunk types = %v, want %v", types, wantTypes)
	}
	if text != "Hello back " { // each word is followed by a space: "Hello " + "back "
		t.Fatalf("text = %q, want each word followed by a space", text)
	}
	if last := chunks[len(chunks)-1]; last.Usage != nil {
		t.Fatalf("done chunk carries usage %+v when the turn set none", last.Usage)
	}

	chunks, err = drain(t.Context(), t, m)
	if err != nil {
		t.Fatal(err)
	}
	if last := chunks[len(chunks)-1]; last.Type != kernel.ChunkDone || last.Usage == nil || last.Usage.InputTokens != 4 {
		t.Fatalf("done chunk = %+v, want the scripted usage", last)
	}
}

func TestScriptedModels_StreamVerbatimChunksAndFailures(t *testing.T) {
	text := "partial"
	scripted := []kernel.StreamChunk{{Type: kernel.ChunkText, Content: &text}, {Type: kernel.ChunkError, Error: "provider down"}}
	m := aecmock.NewScriptedModels(
		aecmock.ScriptedTurn{Chunks: scripted},
		aecmock.ScriptedTurn{Err: errors.New("scripted failure")},
	)
	chunks, err := drain(t.Context(), t, m)
	if err != nil || len(chunks) != 2 || chunks[1].Error != "provider down" {
		t.Fatalf("verbatim chunks = %+v, %v", chunks, err)
	}
	chunks, err = drain(t.Context(), t, m)
	if err != nil || len(chunks) != 1 || chunks[0].Type != kernel.ChunkError || chunks[0].Error != "scripted failure" {
		t.Fatalf("failed turn = %+v, %v, want one terminal error chunk with a nil error", chunks, err)
	}
	// The script is exhausted: in-band failure again, not a panic.
	chunks, err = drain(t.Context(), t, m)
	if err != nil || len(chunks) != 1 || chunks[0].Type != kernel.ChunkError {
		t.Fatalf("exhausted = %+v, %v", chunks, err)
	}
}

func TestScriptedModels_StreamStopsOnBreakAndCtx(t *testing.T) {
	m := aecmock.NewScriptedModels(aecmock.ScriptedTurn{Content: "a b c d"})
	n := 0
	for range m.Stream(t.Context(), nil, llmhub.ModelOptions{}) {
		n++
		if n == 2 {
			break // yield returns false: the iterator must return and never yield again
		}
	}
	if n != 2 {
		t.Fatalf("saw %d chunks, want 2", n)
	}

	m = aecmock.NewScriptedModels(aecmock.ScriptedTurn{Content: "a b c d"})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var errs []error
	seen := 0
	for _, err := range m.Stream(ctx, nil, llmhub.ModelOptions{}) {
		if err != nil {
			errs = append(errs, err)
			continue
		}
		seen++
		cancel()
	}
	if seen != 1 || len(errs) != 1 || !errors.Is(errs[0], context.Canceled) {
		t.Fatalf("after cancel: %d chunks, errors %v, want 1 chunk then the ctx error once", seen, errs)
	}
}

func TestScriptedModels_TracingHooks(t *testing.T) {
	m := aecmock.NewScriptedModels()
	opts := llmhub.ModelOptions{Model: "x"}
	if got := m.ResolveOptions(opts); got.Model != "x" {
		t.Fatalf("ResolveOptions changed the options: %+v", got)
	}
	if _, err := m.ResolveProviderName("anything"); err == nil {
		t.Fatal("ResolveProviderName must fail (the TS harness stub throws)")
	}
}

func TestScriptedModels_SummaryReplyDoesNotConsumeATurn(t *testing.T) {
	m := aecmock.NewScriptedModels(aecmock.ScriptedTurn{Content: "main"}).With(aecmock.WithSummaryReply("SUMMARY"))
	ctx := t.Context()

	// A call with a system prompt is the summary call: answered from the option.
	sum, err := m.Chat(ctx, []kernel.Message{{Role: kernel.RoleUser, Content: "x"}}, llmhub.ModelOptions{SystemPrompt: "summarise"})
	if err != nil || sum.Message.Content != "SUMMARY" || sum.StopReason != kernel.StopEndTurn || sum.Usage.InputTokens != 10 {
		t.Fatalf("summary call = %+v, %v", sum, err)
	}
	// The scripted turn is still there for the main call.
	main, err := m.Chat(ctx, nil, llmhub.ModelOptions{})
	if err != nil || main.Message.Content != "main" {
		t.Fatalf("main call = %+v, %v", main, err)
	}
	if calls := m.Calls(); len(calls) != 2 || calls[0].Options.SystemPrompt != "summarise" {
		t.Fatalf("recorded calls = %+v, want both, the summary call first", calls)
	}

	// An empty reply is a valid reply, and an error option fails the call.
	empty := aecmock.NewScriptedModels().With(aecmock.WithSummaryReply(""))
	if r, err := empty.Chat(ctx, nil, llmhub.ModelOptions{SystemPrompt: "s"}); err != nil || r.Message.Content != "" {
		t.Fatalf("empty summary = %+v, %v", r, err)
	}
	boom := errors.New("boom")
	failing := aecmock.NewScriptedModels().With(aecmock.WithSummaryError(boom))
	if _, err := failing.Chat(ctx, nil, llmhub.ModelOptions{SystemPrompt: "s"}); !errors.Is(err, boom) {
		t.Fatalf("summary error = %v, want the scripted one", err)
	}
}

func TestScriptedModels_ResolverAndProviderNameOptions(t *testing.T) {
	m := aecmock.NewScriptedModels().With(
		aecmock.WithOptionResolver(func(o llmhub.ModelOptions) llmhub.ModelOptions {
			o.Model = "resolved-model"
			return o
		}),
		aecmock.WithProviderName("scripted"),
	)
	if got := m.ResolveOptions(llmhub.ModelOptions{Model: "x"}); got.Model != "resolved-model" {
		t.Fatalf("ResolveOptions = %+v, want the resolver applied", got)
	}
	if name, err := m.ResolveProviderName("anything"); err != nil || name != "scripted" {
		t.Fatalf("ResolveProviderName = %q, %v", name, err)
	}
}

func TestScriptedMemory_AnswersInOrderAndRepeatsTheLast(t *testing.T) {
	one, two := "first block", "second block"
	m := aecmock.NewScriptedMemory(
		memory.RecalledMemories{Content: &one, RecordIDs: []string{"a"}, Memories: []memory.RecalledMemoryTrace{}, MaxResults: 8},
		memory.RecalledMemories{Content: &two, RecordIDs: []string{"b"}, Memories: []memory.RecalledMemoryTrace{}, MaxResults: 8},
	)
	ctx := t.Context()
	for i, want := range []string{"first block", "second block", "second block"} {
		got, err := m.RecallForTurn(ctx, "msg", "s-1")
		if err != nil || got.Content == nil || *got.Content != want {
			t.Fatalf("call %d = %+v, %v, want %q", i+1, got, err, want)
		}
	}
	if calls := m.Calls(); len(calls) != 3 || calls[0].UserMessage != "msg" || calls[0].SessionID != "s-1" {
		t.Fatalf("recorded calls = %+v", calls)
	}

	// A returned result is a copy: changing it does not change the script.
	got, _ := m.RecallForTurn(ctx, "msg", "s-1")
	got.RecordIDs[0] = "changed"
	*got.Content = "changed"
	again, _ := m.RecallForTurn(ctx, "msg", "s-1")
	if again.RecordIDs[0] != "b" || *again.Content != "second block" {
		t.Fatalf("the script was changed through a result: %+v", again)
	}

	// With no results it recalls nothing, with empty lists (arrays on the wire).
	none, err := aecmock.NewScriptedMemory().RecallForTurn(ctx, "x", "s")
	if err != nil || none.Content != nil || none.RecordIDs == nil || none.Memories == nil {
		t.Fatalf("an empty script recalled %+v, %v", none, err)
	}
}

func TestScriptedMemory_WithRecallError(t *testing.T) {
	boom := errors.New("boom")
	m := aecmock.NewScriptedMemory(memory.RecalledMemories{}).With(aecmock.WithRecallError(boom))
	if _, err := m.RecallForTurn(t.Context(), "x", "s"); !errors.Is(err, boom) {
		t.Fatalf("RecallForTurn error = %v, want the scripted one", err)
	}
	if len(m.Calls()) != 1 {
		t.Fatal("the failed call was not recorded")
	}
}

const stamp = "2026-01-01T00:00:00.000Z"

func newConv(t *testing.T) (*aecmock.Conversation, *persistence.PersistenceHub) {
	t.Helper()
	hub := persistence.NewPersistenceHub()
	c := aecmock.NewConversation(hub, aecmock.WithConversationClock(func() string { return stamp }))
	if _, err := c.StartSession(t.Context(), ordered.Object{}); err != nil {
		t.Fatal(err)
	}
	return c, hub
}

func TestConversation_AddMessageStampsAndStores(t *testing.T) {
	c, hub := newConv(t)
	ctx := t.Context()
	s, _ := c.Session()
	before := s.UpdatedAt

	if err := c.AddMessage(ctx, kernel.Message{Role: kernel.RoleUser, Content: "hi"}); err != nil {
		t.Fatal(err)
	}
	if err := c.AddMessage(ctx, kernel.Message{Role: kernel.RoleAssistant, Content: "yo", Timestamp: "kept"}); err != nil {
		t.Fatal(err)
	}
	got := c.Messages()
	if len(got) != 2 || got[0].Timestamp != stamp || got[1].Timestamp != "kept" {
		t.Fatalf("messages = %+v, want the clock stamp only on the message without one", got)
	}
	stored, err := hub.Messages.Get(ctx, s.ID)
	if err != nil || len(stored) != 2 || stored[0].Timestamp != stamp {
		t.Fatalf("stored history = %+v, %v", stored, err)
	}
	if after, _ := c.Session(); after.UpdatedAt < before {
		t.Fatalf("UpdatedAt went back: %d < %d", after.UpdatedAt, before)
	}
	if !slices.Equal(roles(c.LLMMessages()), []kernel.Role{kernel.RoleUser, kernel.RoleAssistant}) {
		t.Fatalf("LLMMessages without a working set = %+v", c.LLMMessages())
	}
}

func roles(msgs []kernel.Message) []kernel.Role {
	out := make([]kernel.Role, len(msgs))
	for i, m := range msgs {
		out[i] = m.Role
	}
	return out
}

func TestConversation_CompactedWorkingSet(t *testing.T) {
	c, hub := newConv(t)
	ctx := t.Context()
	for _, content := range []string{"a", "b", "c"} {
		if err := c.AddMessage(ctx, kernel.Message{Role: kernel.RoleUser, Content: content}); err != nil {
			t.Fatal(err)
		}
	}
	if err := c.SetCompactedMessages(ctx, []kernel.Message{{Role: kernel.RoleAssistant, Content: "summary"}}); err != nil {
		t.Fatal(err)
	}
	if got := c.LLMMessages(); len(got) != 1 || got[0].Content != "summary" {
		t.Fatalf("LLMMessages after compaction = %+v", got)
	}
	// A message after the compaction goes to both the history and the working set.
	if err := c.AddMessage(ctx, kernel.Message{Role: kernel.RoleUser, Content: "d"}); err != nil {
		t.Fatal(err)
	}
	if len(c.Messages()) != 4 {
		t.Fatalf("history = %d messages, want 4", len(c.Messages()))
	}
	if got := c.LLMMessages(); len(got) != 2 || got[1].Content != "d" {
		t.Fatalf("working set = %+v, want summary then d", got)
	}
	s, _ := c.Session()
	compacted, err := hub.CompactedMessages.Get(ctx, s.ID)
	if err != nil || len(compacted) != 2 {
		t.Fatalf("stored working set = %+v, %v", compacted, err)
	}

	// An empty working set still exists: later messages are appended to it.
	c2, _ := newConv(t)
	if err := c2.SetCompactedMessages(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if err := c2.AddMessage(ctx, kernel.Message{Role: kernel.RoleUser, Content: "x"}); err != nil {
		t.Fatal(err)
	}
	if len(c2.LLMMessages()) != 1 {
		t.Fatalf("an empty working set must collect later messages: %+v", c2.LLMMessages())
	}
}

func TestConversation_UpdateSessionMergesUsage(t *testing.T) {
	c, hub := newConv(t)
	ctx := t.Context()
	s, _ := c.Session()
	s.CurrentTokenUsage = &kernel.TokenUsage{InputTokens: 10, OutputTokens: 5}
	s.CounterCalibration = &kernel.CounterCalibration{CharsPerToken: 3.5, Samples: 2}
	s.LLMModel = "ignored"
	if err := c.UpdateSession(ctx, s); err != nil {
		t.Fatal(err)
	}
	got, _ := c.Session()
	if got.CurrentTokenUsage == nil || got.CurrentTokenUsage.InputTokens != 10 || got.CounterCalibration == nil || got.CounterCalibration.Samples != 2 {
		t.Fatalf("session after UpdateSession = %+v", got)
	}
	if got.LLMModel != "" {
		t.Fatalf("UpdateSession copied %q: only the usage fields are merged", got.LLMModel)
	}
	stored, err := hub.Sessions.Get(ctx, s.ID)
	if err != nil || stored == nil || stored.CurrentTokenUsage == nil || stored.CurrentTokenUsage.OutputTokens != 5 {
		t.Fatalf("stored session = %+v, %v", stored, err)
	}

	// A snapshot without usage leaves the stored usage alone.
	empty, _ := c.Session()
	empty.CurrentTokenUsage, empty.CounterCalibration = nil, nil
	if err := c.UpdateSession(ctx, empty); err != nil {
		t.Fatal(err)
	}
	if again, _ := c.Session(); again.CurrentTokenUsage == nil {
		t.Fatal("UpdateSession cleared the usage")
	}
}

func TestConversation_ReturnsCopiesAndRequiresASession(t *testing.T) {
	c, _ := newConv(t)
	ctx := t.Context()
	if err := c.AddMessage(ctx, kernel.Message{Role: kernel.RoleAssistant, ToolCalls: []kernel.ToolCall{call("c", "t", `{}`)}}); err != nil {
		t.Fatal(err)
	}
	c.Messages()[0].ToolCalls[0].Name = "changed"
	c.LLMMessages()[0].ToolCalls[0].Name = "changed"
	if c.Messages()[0].ToolCalls[0].Name != "t" {
		t.Fatal("Messages() exposed its internal state")
	}
	s, _ := c.Session()
	s.Metadata.Set("k", "v")
	if again, _ := c.Session(); again.Metadata.Has("k") {
		t.Fatal("Session() exposed its internal state")
	}

	bare := aecmock.NewConversation(persistence.NewPersistenceHub())
	if _, ok := bare.Session(); ok {
		t.Fatal("a conversation without StartSession has a session")
	}
	for name, err := range map[string]error{
		"AddMessage":           bare.AddMessage(ctx, kernel.Message{}),
		"SetCompactedMessages": bare.SetCompactedMessages(ctx, nil),
		"UpdateSession":        bare.UpdateSession(ctx, kernel.Session{}),
	} {
		if !errors.Is(err, agentloop.ErrNoActiveSession) || err.Error() != "No active session" {
			t.Fatalf("%s without a session = %v, want ErrNoActiveSession", name, err)
		}
	}
}

// echoTool is a minimal registered tool.
type echoTool struct{ name string }

func (e echoTool) Name() string                   { return "tool:" + e.name }
func (echoTool) Version() string                  { return "1.0.0" }
func (echoTool) Initialize(context.Context) error { return nil }
func (echoTool) Dispose(context.Context) error    { return nil }
func (e echoTool) Definition() toolhub.Definition {
	return toolhub.Definition{Name: e.name, Description: "echo", Parameters: toolhub.JSONSchema{Type: "object"}}
}

func (echoTool) Execute(context.Context, kernel.Arguments, toolhub.Context) (toolhub.Output, error) {
	return toolhub.Output{Content: "ok"}, nil
}

func TestHubTools_ListsAndAuthorizesOnlyTheAgentsTools(t *testing.T) {
	hub := toolhub.NewToolHub()
	for _, n := range []string{"a", "b", "c"} {
		if err := hub.Register(echoTool{name: n}); err != nil {
			t.Fatal(err)
		}
	}
	tools := aecmock.NewHubTools(hub, []string{"c", "a", "missing"})
	var names []string
	for _, d := range tools.List() {
		names = append(names, d.Name)
	}
	if !slices.Equal(names, []string{"c", "a"}) {
		t.Fatalf("List = %v, want the agent's tools in the list's order, unknown names skipped", names)
	}

	ctx := t.Context()
	auth, decision, err := tools.Authorize(ctx, "a", kernel.Arguments(`{}`), toolhub.Context{})
	if err != nil || !decision.Allowed {
		t.Fatalf("Authorize a = %v, %+v", err, decision)
	}
	if out, err := auth.Execute(ctx); err != nil || out.Content != "ok" {
		t.Fatalf("Execute = %+v, %v", out, err)
	}

	// b is registered but not in the agent's list, so it is not available.
	_, _, err = tools.Authorize(ctx, "b", kernel.Arguments(`{}`), toolhub.Context{})
	if !errors.Is(err, toolhub.ErrNotFound) || err.Error() != `Tool "b" not available for this agent` {
		t.Fatalf("Authorize b = %v, want ErrNotFound with the TS text", err)
	}
	// A name that is not registered at all: the hub's own error, same text.
	_, _, err = tools.Authorize(ctx, "missing", kernel.Arguments(`{}`), toolhub.Context{})
	if !errors.Is(err, toolhub.ErrNotFound) || err.Error() != `Tool "missing" not available for this agent` {
		t.Fatalf("Authorize missing = %v", err)
	}

	// An empty list means no tools at all (not allow-all).
	none := aecmock.NewHubTools(hub, nil)
	if len(none.List()) != 0 {
		t.Fatalf("List with no allowed names = %v", none.List())
	}
	if _, _, err := none.Authorize(ctx, "a", kernel.Arguments(`{}`), toolhub.Context{}); !errors.Is(err, toolhub.ErrNotFound) {
		t.Fatalf("Authorize with no allowed names = %v, want ErrNotFound", err)
	}
}

// markMiddleware appends its tag to the content of the last message and to the
// response content, so the order of the hooks shows in the result.
type markMiddleware struct {
	tag      string
	priority int
}

func (m markMiddleware) Name() string  { return "mw-" + m.tag }
func (markMiddleware) Version() string { return "1.0.0" }
func (m markMiddleware) Priority() int { return m.priority }
func (m markMiddleware) BeforeModel(_ context.Context, msgs []kernel.Message, _ agentloop.MiddlewareContext) ([]kernel.Message, error) {
	out := slices.Clone(msgs)
	out[len(out)-1].Content += m.tag
	return out, nil
}

func (m markMiddleware) AfterModel(_ context.Context, resp kernel.ModelResponse, _ agentloop.MiddlewareContext) (kernel.ModelResponse, error) {
	resp.Message.Content += m.tag
	return resp, nil
}

func TestPromptContext_BuildsTheSystemMessageAndRunsMiddlewares(t *testing.T) {
	p := profile.AgentProfile{
		SystemPrompt:   "SYS",
		InitialContext: &profile.InitialContext{UserMd: "USER", SoulMd: "SOUL"},
	}
	c := aecmock.NewPromptContext(p, markMiddleware{"B", 20}, markMiddleware{"A", 10}, markMiddleware{"C", 20})
	ctx := t.Context()

	got, err := c.PrepareMessages(ctx, []kernel.Message{{Role: kernel.RoleUser, Content: "q"}}, agentloop.MiddlewareContext{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Role != kernel.RoleSystem || got[0].Content != "SYS\n\nUSER\n\nSOUL" {
		t.Fatalf("prepared = %+v, want one system message of the profile parts joined by a blank line", got)
	}
	if got[1].Content != "qABC" {
		t.Fatalf("last message = %q, want the hooks applied by priority (A) then insertion order (B, C)", got[1].Content)
	}
	resp, err := c.ProcessResponse(ctx, kernel.ModelResponse{Message: kernel.Message{Content: "r"}}, agentloop.MiddlewareContext{})
	if err != nil || resp.Message.Content != "rABC" {
		t.Fatalf("response = %+v, %v", resp, err)
	}

	// Each call builds a fresh system message (a behaviour the loop depends on).
	again, _ := c.PrepareMessages(ctx, nil, agentloop.MiddlewareContext{})
	again[0].Content = "changed"
	if third, _ := c.PrepareMessages(ctx, nil, agentloop.MiddlewareContext{}); third[0].Content == "changed" {
		t.Fatal("the system message is shared between calls")
	}
}

func TestPassthroughContextChangesNothing(t *testing.T) {
	var c aecmock.PassthroughContext
	msgs := []kernel.Message{{Role: kernel.RoleUser, Content: "q"}}
	got, err := c.PrepareMessages(t.Context(), msgs, agentloop.MiddlewareContext{})
	if err != nil || len(got) != 1 || got[0].Content != "q" {
		t.Fatalf("PrepareMessages = %+v, %v (no system message is added)", got, err)
	}
	resp := kernel.ModelResponse{Message: kernel.Message{Content: "r"}, StopReason: kernel.StopEndTurn}
	if out, err := c.ProcessResponse(t.Context(), resp, agentloop.MiddlewareContext{}); err != nil || out.Message.Content != "r" {
		t.Fatalf("ProcessResponse = %+v, %v", out, err)
	}
}

func TestSkillTools_FindsByDefinitionNameAndListsInOrder(t *testing.T) {
	skills := aecmock.NewSkillTools(echoTool{name: "skill_view"}, echoTool{name: "skill_list"})

	h, ok := skills.Tool("skill_list")
	if !ok || h.Definition().Name != "skill_list" {
		t.Fatalf("Tool(skill_list) = %v, %v", h, ok)
	}
	if h, ok := skills.Tool("skill:view"); ok || h != nil {
		t.Fatalf("Tool matched the plugin name instead of the definition name: %v, %v", h, ok)
	}
	if _, ok := skills.Tool("missing"); ok {
		t.Fatal("Tool found a name nobody registered")
	}

	defs, err := skills.ToolDefinitions(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(defs) != 2 || defs[0].Name != "skill_view" || defs[1].Name != "skill_list" {
		t.Fatalf("ToolDefinitions = %+v, want registration order", defs)
	}
	defs[0].Name = "changed"
	if again, _ := skills.ToolDefinitions(t.Context()); again[0].Name != "skill_view" {
		t.Fatal("ToolDefinitions returned shared state")
	}

	none := aecmock.NewSkillTools()
	if defs, _ := none.ToolDefinitions(t.Context()); len(defs) != 0 {
		t.Fatalf("an empty SkillTools lists %v", defs)
	}
}
