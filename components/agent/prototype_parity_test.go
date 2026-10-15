package agent_test

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/Phong95/aec-agent-go/aecmock"
	"github.com/Phong95/aec-agent-go/components/agent"
	"github.com/Phong95/aec-agent-go/components/agent/agentloop"
	"github.com/Phong95/aec-agent-go/components/agent/profile"
	"github.com/Phong95/aec-agent-go/components/persistence"
	"github.com/Phong95/aec-agent-go/components/toolhub"
	"github.com/Phong95/aec-agent-go/kernel"
	"github.com/Phong95/aec-agent-go/kernel/ordered"
)

// The real managers of this package are promoted from the Phase 9 test doubles in
// aecmock (the layer rule keeps agent from importing aecmock, so the code is copied,
// not shared). These differential tests run the same operations on a prototype and on
// its real manager and compare the results byte for byte through kernel.EncodeJSON,
// so the two cannot drift apart unnoticed.

// encode renders v as wire JSON.
func encode(t *testing.T, v any) string {
	t.Helper()
	b, err := kernel.EncodeJSON(v)
	if err != nil {
		t.Fatalf("EncodeJSON: %v", err)
	}
	return string(b)
}

// sameHub returns a persistence hub with a fixed session id and clock, so two hubs
// built by it store byte-identical sessions.
func sameHub() *persistence.PersistenceHub {
	return persistence.NewPersistenceHub(persistence.WithSessions(persistence.NewMemorySessionStore(
		persistence.WithSessionIDs(func() string { return "s-1" }),
		persistence.WithSessionClock(func() int64 { return 1_000 }),
	)))
}

// normalized returns the session with the one field both conversations read from the
// wall clock (UpdatedAt, bumped by every AddMessage through kernel.NowMillis) zeroed.
func normalized(s kernel.Session) kernel.Session {
	s.UpdatedAt = 0
	return s
}

// conv is what the two conversation implementations share for this test.
type conv interface {
	agentloop.Conversation
	Messages() []kernel.Message
}

// snapshotConv renders everything observable about a conversation and the hub behind it.
func snapshotConv(t *testing.T, c conv, hub *persistence.PersistenceHub) []string {
	t.Helper()
	ctx := t.Context()
	var out []string
	if s, ok := c.Session(); ok {
		out = append(out, "session "+encode(t, normalized(s)))
	} else {
		out = append(out, "session none")
	}
	out = append(out, "llm "+encode(t, c.LLMMessages()), "messages "+encode(t, c.Messages()))
	stored, err := hub.Messages.Get(ctx, "s-1")
	if err != nil {
		t.Fatal(err)
	}
	working, err := hub.CompactedMessages.Get(ctx, "s-1")
	if err != nil {
		t.Fatal(err)
	}
	out = append(out, "stored "+encode(t, stored), "stored-working "+encode(t, working))
	if s, err := hub.Sessions.Get(ctx, "s-1"); err != nil {
		t.Fatal(err)
	} else if s != nil {
		out = append(out, "stored-session "+encode(t, normalized(*s)))
	}
	return out
}

func TestPrototypeParity_Conversation(t *testing.T) {
	const stamp = "2026-01-01T00:00:00.000Z"
	clock := func() string { return stamp }
	protoHub, mgrHub := sameHub(), sameHub()
	proto := aecmock.NewConversation(protoHub, aecmock.WithConversationClock(clock))
	mgr := agent.NewConversationManager(mgrHub, agent.WithConversationClock(clock))
	// A-97: the real side of the conversation is the per-turn Conversation the manager
	// opens once the session exists; before that there is none.
	var realConv *agent.Conversation

	call := kernel.ToolCall{ID: "tc-1", Name: "schedule_lookup", Arguments: kernel.Arguments(`{"projectId":"p1"}`)}
	isErr := true
	usage := &kernel.TokenUsage{InputTokens: 7, OutputTokens: 3}
	calibration := &kernel.CounterCalibration{CharsPerToken: 3.5, Samples: 2}

	// Each step runs on both implementations; the helper compares the snapshots after it.
	type step struct {
		name string
		run  func(ctx context.Context, c conv) error
	}
	steps := []step{
		{"add a message with no session", func(ctx context.Context, c conv) error {
			return c.AddMessage(ctx, kernel.Message{Role: kernel.RoleUser, Content: "early"})
		}},
		{"add user message", func(ctx context.Context, c conv) error {
			return c.AddMessage(ctx, kernel.Message{Role: kernel.RoleUser, Content: "Hello"})
		}},
		{"add assistant message with a tool call", func(ctx context.Context, c conv) error {
			return c.AddMessage(ctx, kernel.Message{Role: kernel.RoleAssistant, ToolCalls: []kernel.ToolCall{call}})
		}},
		{"add tool results", func(ctx context.Context, c conv) error {
			return c.AddMessage(ctx, kernel.Message{Role: kernel.RoleAssistant, ToolResults: []kernel.ToolResult{{ToolCallID: "tc-1", Content: "boom", IsError: &isErr}}})
		}},
		{"set the compacted working set", func(ctx context.Context, c conv) error {
			return c.SetCompactedMessages(ctx, []kernel.Message{{Role: kernel.RoleUser, Content: "summary", Timestamp: stamp}})
		}},
		{"add a message with a working set", func(ctx context.Context, c conv) error {
			return c.AddMessage(ctx, kernel.Message{Role: kernel.RoleAssistant, Content: "after compaction"})
		}},
		{"update the session usage", func(ctx context.Context, c conv) error {
			return c.UpdateSession(ctx, kernel.Session{CurrentTokenUsage: usage, CounterCalibration: calibration})
		}},
		{"update the session with nothing", func(ctx context.Context, c conv) error {
			return c.UpdateSession(ctx, kernel.Session{})
		}},
		{"replace the working set with an empty one", func(ctx context.Context, c conv) error {
			return c.SetCompactedMessages(ctx, []kernel.Message{})
		}},
		{"add a message to the empty working set", func(ctx context.Context, c conv) error {
			return c.AddMessage(ctx, kernel.Message{Role: kernel.RoleUser, Content: "again"})
		}},
	}

	check := func(label string) {
		t.Helper()
		want, got := snapshotConv(t, proto, protoHub), snapshotConv(t, realConv, mgrHub)
		if !slices.Equal(want, got) {
			t.Fatalf("%s: the real manager drifted from its prototype\nprototype %q\nreal      %q", label, want, got)
		}
	}

	for i, s := range steps {
		if i == 1 { // the session starts before the second step, on both
			ps, err := proto.StartSession(t.Context(), ordered.Object{})
			if err != nil {
				t.Fatal(err)
			}
			rs, err := mgr.StartSession(t.Context(), ordered.Object{}, "")
			if err != nil {
				t.Fatal(err)
			}
			if encode(t, ps) != encode(t, rs) {
				t.Fatalf("StartSession: prototype %s, real %s", encode(t, ps), encode(t, rs))
			}
			opened, ok, err := mgr.Open(t.Context(), rs.ID)
			if err != nil || !ok {
				t.Fatalf("Open(%s) = %v, %v", rs.ID, ok, err)
			}
			realConv = opened
			check("after StartSession")
		}
		if realConv == nil {
			// A-97: no session, no Conversation on the real side; the prototype refuses
			// the write.
			if err := s.run(t.Context(), proto); !errors.Is(err, agentloop.ErrNoActiveSession) {
				t.Fatalf("%s: prototype error %v, want ErrNoActiveSession", s.name, err)
			}
			continue
		}
		perr, rerr := s.run(t.Context(), proto), s.run(t.Context(), realConv)
		if (perr == nil) != (rerr == nil) || (perr != nil && perr.Error() != rerr.Error()) {
			t.Fatalf("%s: prototype error %v, real error %v", s.name, perr, rerr)
		}
		check(s.name)
	}
}

func TestPrototypeParity_Tools(t *testing.T) {
	hub := toolhub.NewToolHub()
	mustRegister(t, hub, &aecmock.ScheduleLookupTool{}, &aecmock.BudgetQueryTool{}, &aecmock.DocumentSearchTool{})
	allowed := []string{"schedule_lookup", "budget_query", "ghost_tool"}
	proto := aecmock.NewHubTools(hub, allowed)
	mgr := agent.NewToolManager(hub, profile.AgentProfile{Tools: allowed})

	if want, got := encode(t, proto.List()), encode(t, mgr.List()); want != got {
		t.Fatalf("List: prototype %s, real %s", want, got)
	}

	args := kernel.Arguments(`{"projectId":"p1"}`)
	for _, name := range []string{"schedule_lookup", "budget_query", "document_search", "ghost_tool", "nobody"} {
		pAuth, pDecision, pErr := proto.Authorize(t.Context(), name, args, toolhub.Context{})
		rAuth, rDecision, rErr := mgr.Authorize(t.Context(), name, args, toolhub.Context{})
		if (pErr == nil) != (rErr == nil) || (pErr != nil && pErr.Error() != rErr.Error()) {
			t.Fatalf("Authorize(%s) errors: prototype %v, mgr %v", name, pErr, rErr)
		}
		if pErr != nil && errors.Is(pErr, toolhub.ErrNotFound) != errors.Is(rErr, toolhub.ErrNotFound) {
			t.Fatalf("Authorize(%s): the errors disagree on toolhub.ErrNotFound", name)
		}
		if encode(t, pDecision) != encode(t, rDecision) {
			t.Fatalf("Authorize(%s) decisions: prototype %s, real %s", name, encode(t, pDecision), encode(t, rDecision))
		}
		if pErr != nil || !pDecision.Allowed {
			continue
		}
		pOut, pRunErr := pAuth.Execute(t.Context())
		rOut, rRunErr := rAuth.Execute(t.Context())
		if pRunErr != nil || rRunErr != nil || encode(t, pOut) != encode(t, rOut) {
			t.Fatalf("Execute(%s): prototype %s %v, real %s %v", name, encode(t, pOut), pRunErr, encode(t, rOut), rRunErr)
		}
	}

	// An allowlist change is the same on both.
	proto2 := aecmock.NewHubTools(hub, nil)
	mgr2 := agent.NewToolManager(hub, profile.AgentProfile{})
	if want, got := encode(t, proto2.List()), encode(t, mgr2.List()); want != got || want != "[]" {
		t.Fatalf("empty allowlist List: prototype %s, real %s, want []", want, got)
	}
}

// markMiddleware is a context middleware that appends its name to the messages it
// sees and to the reply it processes, so a priority order shows in the output.
type markMiddleware struct {
	name     string
	priority int
}

func (m markMiddleware) Name() string  { return m.name }
func (markMiddleware) Version() string { return "1.0.0" }
func (m markMiddleware) Priority() int { return m.priority }

func (m markMiddleware) BeforeModel(_ context.Context, msgs []kernel.Message, _ agentloop.MiddlewareContext) ([]kernel.Message, error) {
	return append(slices.Clone(msgs), kernel.Message{Role: kernel.RoleUser, Content: "before " + m.name}), nil
}

func (m markMiddleware) AfterModel(_ context.Context, resp kernel.ModelResponse, _ agentloop.MiddlewareContext) (kernel.ModelResponse, error) {
	resp.Message.Content += " after " + m.name
	return resp, nil
}

func TestPrototypeParity_Context(t *testing.T) {
	p := testProfile("a")
	p.InitialContext = &profile.InitialContext{UserMd: "user notes", SoulMd: "soul text"}
	// Two middlewares share a priority: their order must follow the order they were given.
	mws := []agentloop.Middleware{
		markMiddleware{name: "late", priority: 20},
		markMiddleware{name: "first", priority: 5},
		markMiddleware{name: "tie-a", priority: 10},
		markMiddleware{name: "tie-b", priority: 10},
	}
	proto := aecmock.NewPromptContext(p, mws...)
	mgr := agent.NewContextManager(p)
	for _, mw := range mws {
		mgr.AddMiddleware(mw)
	}

	msgs := []kernel.Message{{Role: kernel.RoleUser, Content: "Hello", Timestamp: "2026-01-01T00:00:00.000Z"}}
	mc := agentloop.MiddlewareContext{TokenBudget: agentloop.TokenBudget}
	pPrepared, pErr := proto.PrepareMessages(t.Context(), msgs, mc)
	rPrepared, rErr := mgr.PrepareMessages(t.Context(), msgs, mc)
	if pErr != nil || rErr != nil || encode(t, pPrepared) != encode(t, rPrepared) {
		t.Fatalf("PrepareMessages: prototype %s %v, real %s %v", encode(t, pPrepared), pErr, encode(t, rPrepared), rErr)
	}

	resp := kernel.ModelResponse{Message: kernel.Message{Role: kernel.RoleAssistant, Content: "reply"}, StopReason: kernel.StopEndTurn}
	pResp, pErr := proto.ProcessResponse(t.Context(), resp, mc)
	rResp, rErr := mgr.ProcessResponse(t.Context(), resp, mc)
	if pErr != nil || rErr != nil || encode(t, pResp) != encode(t, rResp) {
		t.Fatalf("ProcessResponse: prototype %s %v, real %s %v", encode(t, pResp), pErr, encode(t, rResp), rErr)
	}

	// The input slice is never changed by either.
	if len(msgs) != 1 || msgs[0].Content != "Hello" {
		t.Fatalf("PrepareMessages changed its input: %+v", msgs)
	}
}

// The skill index is the one thing the real manager has that the prototype lacks: it
// goes at the end of the system message after a blank line, or is the whole message
// when the prompt is empty, and nothing is added when the index is empty or absent.
func TestContextManager_SkillIndex(t *testing.T) {
	index := "<available_skills>\n</available_skills>"
	withIndex := func(text string, ok bool) *agent.ContextManager {
		return agent.NewContextManager(testProfile("a"), agent.WithSkillIndex(func(context.Context) (string, bool) { return text, ok }))
	}
	system := func(c *agent.ContextManager) string {
		out, err := c.PrepareMessages(t.Context(), nil, agentloop.MiddlewareContext{})
		if err != nil || len(out) != 1 || out[0].Role != kernel.RoleSystem {
			t.Fatalf("PrepareMessages = %+v, %v, want one system message", out, err)
		}
		return out[0].Content
	}

	if got, want := system(withIndex(index, true)), "You are a helpful assistant.\n\n"+index; got != want {
		t.Fatalf("system with an index = %q, want %q", got, want)
	}
	for _, c := range []*agent.ContextManager{withIndex("", true), withIndex(index, false), agent.NewContextManager(testProfile("a"))} {
		if got := system(c); got != "You are a helpful assistant." {
			t.Fatalf("system without an index = %q", got)
		}
	}
	empty := testProfile("a")
	empty.SystemPrompt = ""
	c := agent.NewContextManager(empty, agent.WithSkillIndex(func(context.Context) (string, bool) { return index, true }))
	if got := system(c); got != index {
		t.Fatalf("system with an empty prompt = %q, want the index alone", got)
	}
}
