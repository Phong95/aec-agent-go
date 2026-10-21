package aec_test

import (
	"context"
	"encoding/json"
	"errors"
	"iter"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	aec "github.com/Phong95/aec-agent-go"
	"github.com/Phong95/aec-agent-go/components/agent/profile"
	"github.com/Phong95/aec-agent-go/components/events"
	"github.com/Phong95/aec-agent-go/components/grouphub"
	"github.com/Phong95/aec-agent-go/components/grouphub/group"
	"github.com/Phong95/aec-agent-go/components/llmhub"
	"github.com/Phong95/aec-agent-go/components/toolhub"
	"github.com/Phong95/aec-agent-go/components/toolhub/builtin/subagents"
	"github.com/Phong95/aec-agent-go/kernel"
	"github.com/Phong95/aec-agent-go/kernel/ordered"
)

// A-106 - group chats, end to end through the AgentCore: every case of
// tests/unit/group-chat.test.ts of core/ts.

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// groupScript answers one model call of a member (call counts from 1).
type groupScript func(ctx context.Context, msgs []kernel.Message, opts llmhub.ModelOptions, call int) (kernel.ModelResponse, error)

type groupCall struct {
	msgs []kernel.Message
	opts llmhub.ModelOptions
}

// groupProvider is a provider whose replies come from a script, streamed as text and
// tool-call chunks; it records every call.
type groupProvider struct {
	name   string
	script groupScript

	mu    sync.Mutex
	calls []groupCall
}

func (p *groupProvider) Name() string    { return p.name }
func (p *groupProvider) Version() string { return "1.0.0" }

func (p *groupProvider) Chat(ctx context.Context, msgs []kernel.Message, opts llmhub.ModelOptions) (kernel.ModelResponse, error) {
	p.mu.Lock()
	p.calls = append(p.calls, groupCall{msgs: slices.Clone(msgs), opts: opts})
	n := len(p.calls)
	p.mu.Unlock()
	return p.script(ctx, msgs, opts, n)
}

func (p *groupProvider) Stream(ctx context.Context, msgs []kernel.Message, opts llmhub.ModelOptions) iter.Seq2[kernel.StreamChunk, error] {
	return func(yield func(kernel.StreamChunk, error) bool) {
		resp, err := p.Chat(ctx, msgs, opts)
		if err != nil {
			if ctx.Err() != nil {
				yield(kernel.StreamChunk{}, ctx.Err())
				return
			}
			yield(kernel.StreamChunk{Type: kernel.ChunkError, Error: err.Error()}, nil)
			return
		}
		text := resp.Message.Content
		if text != "" {
			half := (len(text) + 1) / 2
			for half < len(text) && !isRuneStart(text[half]) {
				half++
			}
			first, second := text[:half], text[half:]
			if !yield(kernel.StreamChunk{Type: kernel.ChunkText, Content: &first}, nil) {
				return
			}
			if second != "" && !yield(kernel.StreamChunk{Type: kernel.ChunkText, Content: &second}, nil) {
				return
			}
		}
		for i := range resp.Message.ToolCalls {
			if !yield(kernel.StreamChunk{Type: kernel.ChunkToolCall, ToolCall: &resp.Message.ToolCalls[i]}, nil) {
				return
			}
		}
		usage := resp.Usage
		yield(kernel.StreamChunk{Type: kernel.ChunkDone, Usage: &usage, StopReason: resp.StopReason}, nil)
	}
}

func isRuneStart(b byte) bool { return b&0xC0 != 0x80 }

func (p *groupProvider) CountTokens(context.Context, []kernel.Message) (int, error) { return 10, nil }

func (p *groupProvider) callCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.calls)
}

func (p *groupProvider) call(i int) groupCall {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls[i]
}

// prompts is the member prompt (last user message) of every call.
func (p *groupProvider) prompts() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]string, len(p.calls))
	for i, c := range p.calls {
		out[i] = lastPrompt(c.msgs)
	}
	return out
}

func lastPrompt(msgs []kernel.Message) string {
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == kernel.RoleUser {
			return msgs[i].Content
		}
	}
	return ""
}

func lastToolResults(msgs []kernel.Message) []kernel.ToolResult {
	for i := len(msgs) - 1; i >= 0; i-- {
		if len(msgs[i].ToolResults) > 0 {
			return msgs[i].ToolResults
		}
	}
	return nil
}

func groupReply(content string) (kernel.ModelResponse, error) {
	return kernel.ModelResponse{
		Message:    kernel.Message{Role: kernel.RoleAssistant, Content: content},
		Usage:      kernel.TokenUsage{InputTokens: 10, OutputTokens: 5},
		StopReason: kernel.StopEndTurn,
	}, nil
}

func groupSpawn(id, agentID, task string) kernel.ToolCall {
	args, _ := kernel.EncodeJSON(map[string]string{"subagent": agentID, "task": task})
	return kernel.ToolCall{ID: id, Name: "spawn_subagent", Arguments: kernel.Arguments(args)}
}

func groupWithCalls(content string, calls ...kernel.ToolCall) (kernel.ModelResponse, error) {
	return kernel.ModelResponse{
		Message:    kernel.Message{Role: kernel.RoleAssistant, Content: content, ToolCalls: calls},
		Usage:      kernel.TokenUsage{InputTokens: 10, OutputTokens: 5},
		StopReason: kernel.StopToolUse,
	}, nil
}

func untilAborted(ctx context.Context) (kernel.ModelResponse, error) {
	<-ctx.Done()
	return kernel.ModelResponse{}, ctx.Err()
}

func always(text string) groupScript {
	return func(context.Context, []kernel.Message, llmhub.ModelOptions, int) (kernel.ModelResponse, error) {
		return groupReply(text)
	}
}

var summaryAsk = regexp.MustCompile(`write a short summary`)

var groupNames = map[string]string{"rfi": "RFI Desk", "spec": "Spec Checker", "sched": "Site Scheduler"}

// approvalRecorder approves every tool and records the requests.
type approvalRecorder struct {
	mu       sync.Mutex
	requests []toolhub.ApprovalRequest
}

func (*approvalRecorder) AskUser(context.Context, toolhub.ClarifyRequest) (toolhub.ClarifyResponse, error) {
	return toolhub.ClarifyResponse{Answer: "?"}, nil
}

func (r *approvalRecorder) RequestApproval(_ context.Context, req toolhub.ApprovalRequest) (toolhub.ApprovalDecision, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.requests = append(r.requests, req)
	return toolhub.ApprovalDecision{Approved: true}, nil
}

func (r *approvalRecorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.requests)
}

type member struct {
	id     string
	script groupScript
}

type groupSetup struct {
	core      *aec.AgentCore
	providers map[string]*groupProvider
	approvals *approvalRecorder
	groupID   string
	sessionID string

	mu     sync.Mutex
	events []kernel.Event
}

func (s *groupSetup) eventsOf(kind string) []kernel.Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []kernel.Event
	for _, e := range s.events {
		if e.Type == kind {
			out = append(out, e)
		}
	}
	return out
}

func (s *groupSetup) send(t *testing.T, text string, to ...string) grouphub.TurnResult {
	t.Helper()
	res, err := s.core.Groups().Send(t.Context(), s.groupID, s.sessionID, grouphub.SendRequest{Text: text, To: to})
	if err != nil {
		t.Fatalf("send %q: %v", text, err)
	}
	return res
}

func (s *groupSetup) thread(t *testing.T) []group.Message {
	t.Helper()
	msgs, err := s.core.Groups().Messages(t.Context(), s.groupID, s.sessionID, 0)
	if err != nil {
		t.Fatal(err)
	}
	return msgs
}

func setupGroup(t *testing.T, members []member, mut func(*group.Definition)) *groupSetup {
	t.Helper()
	core, err := aec.NewAgentCore()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = core.Dispose(context.WithoutCancel(t.Context())) })
	if err := core.UseTool(subagents.NewSpawnSubagentTool()); err != nil {
		t.Fatal(err)
	}
	s := &groupSetup{core: core, providers: map[string]*groupProvider{}, approvals: &approvalRecorder{}}
	core.ToolHub().SetInteractionHandler(s.approvals)
	off := core.Events().OnAny(func(e kernel.Event) {
		s.mu.Lock()
		s.events = append(s.events, e)
		s.mu.Unlock()
	})
	t.Cleanup(off)
	ctx := t.Context()
	var ids []string
	for _, m := range members {
		p := &groupProvider{name: m.id + "-llm", script: m.script}
		s.providers[m.id] = p
		if err := core.UseLLMProvider(p); err != nil {
			t.Fatal(err)
		}
		name := groupNames[m.id]
		if name == "" {
			name = m.id
		}
		zero := 0
		if _, err := core.SpawnAgent(ctx, profile.AgentProfile{
			ID: m.id, Name: name, SystemPrompt: "You are " + name + ".", Provider: p.name,
			LoopConfig: &profile.LoopConfig{MaxToolRetries: &zero, RetryBackoffMs: &zero},
		}); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, m.id)
	}
	def := group.Definition{Name: "Façade war room", Topic: "East elevation start", Members: ids, LeadID: "rfi"}
	if mut != nil {
		mut(&def)
	}
	g, err := core.Groups().Create(ctx, def)
	if err != nil {
		t.Fatal(err)
	}
	session, err := core.Groups().StartSession(ctx, g.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	s.groupID, s.sessionID = g.ID, session.ID
	return s
}

// kinds is the thread as short labels: user, event, an answer's agent id (* for a
// summary), lead>member for a hand-off.
func kinds(msgs []group.Message) []string {
	out := make([]string, len(msgs))
	for i, m := range msgs {
		switch m.Kind {
		case group.KindAgent:
			out[i] = m.AgentID
			if m.Summary {
				out[i] += "*"
			}
		case group.KindHandoff:
			out[i] = m.AgentID + ">" + m.ToAgentID
		default:
			out[i] = string(m.Kind)
		}
	}
	return out
}

func equalStrings(t *testing.T, got, want []string, what string) {
	t.Helper()
	if !slices.Equal(got, want) {
		t.Fatalf("%s = %q, want %q", what, got, want)
	}
}

func contains(t *testing.T, text, part, what string) {
	t.Helper()
	if !strings.Contains(text, part) {
		t.Fatalf("%s does not contain %q:\n%s", what, part, text)
	}
}

func notContains(t *testing.T, text, part, what string) {
	t.Helper()
	if strings.Contains(text, part) {
		t.Fatalf("%s contains %q:\n%s", what, part, text)
	}
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func findMessage(msgs []group.Message, ok func(group.Message) bool) *group.Message {
	for i := range msgs {
		if ok(msgs[i]) {
			return &msgs[i]
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

func TestGroups_LeadRoutesHandsPartsToMembersThenSumsUp(t *testing.T) {
	s := setupGroup(t, []member{
		{"rfi", func(_ context.Context, msgs []kernel.Message, _ llmhub.ModelOptions, call int) (kernel.ModelResponse, error) {
			if call == 1 {
				return groupWithCalls("Two things gate the start.",
					groupSpawn("tc-1", "spec", "Check the glazing submittal against 08 44 13"),
					groupSpawn("tc-2", "sched", "What slips if the embed RFI takes 5 days?"))
			}
			var found []string
			for _, r := range lastToolResults(msgs) {
				switch {
				case strings.Contains(r.Content, "U-value"):
					found = append(found, "spec ok")
				case strings.Contains(r.Content, "float"):
					found = append(found, "sched ok")
				default:
					found = append(found, "?")
				}
			}
			return groupReply("Summary: " + strings.Join(found, ", "))
		}},
		{"spec", always("U-value deviates: 1.8 vs 1.6")},
		{"sched", always("The start holds with 1 day of float")},
	}, nil)

	result := s.send(t, "Can the east façade still start in week 33?")
	if result.Status != group.TurnCompleted {
		t.Fatalf("status = %s", result.Status)
	}
	if result.Plan.Kind != group.PlanLead || result.Plan.LeadID != "rfi" || !slices.Equal(result.Plan.Helpers, []string{"spec", "sched"}) {
		t.Fatalf("plan = %+v", result.Plan)
	}

	thread := s.thread(t)
	order := kinds(thread)
	equalStrings(t, order[:2], []string{"user", "rfi"}, "thread start")
	if order[len(order)-1] != "rfi*" {
		t.Fatalf("thread = %q, want the lead's summary last", order)
	}
	middle := slices.Sorted(slices.Values(order[2 : len(order)-1]))
	equalStrings(t, middle, []string{"rfi>sched", "rfi>spec", "sched", "spec"}, "hand-offs and answers")
	for i, m := range thread {
		if m.Seq != i+1 {
			t.Fatalf("seq of message %d = %d", i, m.Seq)
		}
	}

	if lead := thread[1]; lead.Kind != group.KindAgent || lead.AgentID != "rfi" || lead.Text != "Two things gate the start." || lead.Status != group.StatusCompleted {
		t.Fatalf("lead's first segment = %+v", lead)
	}
	var answers []group.Message
	for _, m := range thread {
		if m.Kind == group.KindAgent && m.RoundID != "" {
			answers = append(answers, m)
		}
	}
	var answerIDs []string
	rounds := map[string]bool{}
	for _, m := range answers {
		answerIDs = append(answerIDs, m.AgentID)
		rounds[m.RoundID] = true
		if m.Status != group.StatusCompleted || m.SubagentRunID == "" {
			t.Fatalf("answer = %+v", m)
		}
	}
	slices.Sort(answerIDs)
	equalStrings(t, answerIDs, []string{"sched", "spec"}, "answers")
	if len(rounds) != 1 {
		t.Fatalf("answers in %d rounds, want 1", len(rounds))
	}
	var rowStatuses []string
	for _, m := range thread {
		if m.Kind == group.KindHandoff {
			rowStatuses = append(rowStatuses, string(m.Status))
			if m.ToAgentID == "spec" && m.Text != "Check the glazing submittal against 08 44 13" {
				t.Fatalf("hand-off text = %q", m.Text)
			}
		}
	}
	equalStrings(t, rowStatuses, []string{"completed", "completed"}, "hand-off statuses")
	if last := thread[len(thread)-1]; last.AgentID != "rfi" || !last.Summary || last.Text != "Summary: spec ok, sched ok" || last.Status != group.StatusCompleted {
		t.Fatalf("summary = %+v", last)
	}

	// The lead was granted spawn_subagent for the turn and its hand-offs asked nobody.
	if n := s.approvals.count(); n != 0 {
		t.Fatalf("%d approvals were asked, want none", n)
	}
	first := s.providers["rfi"].call(0)
	contains(t, first.msgs[0].Content, "<available_subagents>", "the lead's system prompt")
	contains(t, first.msgs[0].Content, "other members of your group chat", "the lead's system prompt")
	if !slices.ContainsFunc(first.opts.Tools, func(d toolhub.Definition) bool { return d.Name == "spawn_subagent" }) {
		t.Fatalf("the lead was not offered spawn_subagent: %v", first.opts.Tools)
	}

	// A member is told the thread (attributed) and the lead's ask - not a fresh brief.
	specPrompt := s.providers["spec"].prompts()[0]
	contains(t, specPrompt, "User: Can the east façade still start in week 33?", "spec's prompt")
	contains(t, specPrompt, "RFI Desk: Two things gate the start.", "spec's prompt")
	contains(t, specPrompt, "<ask>\nCheck the glazing submittal against 08 44 13\n</ask>", "spec's prompt")
	contains(t, specPrompt, `<group_chat name="Façade war room">`, "spec's prompt")

	// Members work in their own member sessions; nothing is offered outside the turn.
	filter := ordered.NewObject()
	filter.Set("source", "group-member")
	filter.Set("groupSessionId", s.sessionID)
	memberSessions, err := s.core.PersistenceHub().Sessions.List(t.Context(), kernel.SessionFilter{Metadata: filter})
	if err != nil {
		t.Fatal(err)
	}
	var owners []string
	leadSession := ""
	for _, ms := range memberSessions {
		id, _ := ms.Metadata.Get("agentId")
		owners = append(owners, id.(string))
		if id == "rfi" {
			leadSession = ms.ID
		}
	}
	slices.Sort(owners)
	equalStrings(t, owners, []string{"rfi", "sched", "spec"}, "member sessions")
	if s.core.Groups().Scopes().Get(leadSession) != nil {
		t.Fatal("the lead's spawn scope outlived the turn")
	}

	// Streaming events carry the group session.
	deltas := s.eventsOf(events.GroupMessageDelta.Name)
	if len(deltas) == 0 {
		t.Fatal("no group:message-delta events")
	}
	for _, e := range deltas {
		if e.SessionID != s.sessionID {
			t.Fatalf("delta on session %q", e.SessionID)
		}
	}
	var finished []string
	for _, e := range s.eventsOf(events.GroupTurnFinished.Name) {
		p, err := events.PayloadAs[events.GroupTurnFinishedPayload](e)
		if err != nil {
			t.Fatal(err)
		}
		finished = append(finished, string(p.Status))
	}
	equalStrings(t, finished, []string{"completed"}, "group:turn-finished statuses")
	if s.core.Groups().IsBusy(s.sessionID) {
		t.Fatal("the session is still busy")
	}

	// The session took the first message as its title.
	session, err := s.core.Groups().GetSession(t.Context(), s.groupID, s.sessionID)
	if err != nil || session == nil {
		t.Fatalf("session = %v, %v", session, err)
	}
	if title, _ := session.Metadata.Get("title"); title != "Can the east façade still start in week 33?" {
		t.Fatalf("title = %v", title)
	}
}

func TestGroups_EveryoneAnswersWithAMutedMemberQuietAndTheLeadSumsUp(t *testing.T) {
	s := setupGroup(t, []member{
		{"rfi", func(_ context.Context, msgs []kernel.Message, _ llmhub.ModelOptions, _ int) (kernel.ModelResponse, error) {
			if summaryAsk.MatchString(lastPrompt(msgs)) {
				return groupReply("All clear from both.")
			}
			return groupReply("RFI view")
		}},
		{"spec", always("Spec view")},
		{"sched", always("never asked")},
	}, func(d *group.Definition) {
		d.ReplyMode = group.ReplyAll
		d.Muted = []string{"sched"}
	})

	result := s.send(t, "Status from everyone?")
	if result.Plan.Kind != group.PlanRound || !slices.Equal(result.Plan.AgentIDs, []string{"rfi", "spec"}) || !result.Plan.Summarize {
		t.Fatalf("plan = %+v", result.Plan)
	}
	equalStrings(t, kinds(result.Messages), []string{"user", "rfi", "spec", "rfi*"}, "messages")
	a, b, summary := result.Messages[1], result.Messages[2], result.Messages[3]
	if a.RoundID == "" || a.RoundID != b.RoundID || summary.RoundID != "" {
		t.Fatalf("rounds = %q %q %q", a.RoundID, b.RoundID, summary.RoundID)
	}
	if n := s.providers["sched"].callCount(); n != 0 {
		t.Fatalf("the muted member was called %d times", n)
	}

	// Members of a round answer without seeing each other; the summary sees both.
	notContains(t, s.providers["spec"].prompts()[0], "RFI view", "spec's prompt")
	var summaryPrompt string
	for _, p := range s.providers["rfi"].prompts() {
		if summaryAsk.MatchString(p) {
			summaryPrompt = p
		}
	}
	contains(t, summaryPrompt, "Spec Checker: Spec view", "the summary prompt")
	// Its own answer is in its own session already.
	notContains(t, summaryPrompt, "RFI Desk: RFI view", "the summary prompt")
}

func TestGroups_OnlyWhenMentionedNobodyAddressedMeansNobodyAnswers(t *testing.T) {
	s := setupGroup(t, []member{{"rfi", always("x")}, {"spec", always("y")}}, func(d *group.Definition) { d.ReplyMode = group.ReplyMention })
	result := s.send(t, "hello?")
	if result.Status != group.TurnNoReply {
		t.Fatalf("status = %s", result.Status)
	}
	equalStrings(t, kinds(result.Messages), []string{"user", "event"}, "messages")
	if !strings.Contains(result.Messages[1].Text, "Nobody was mentioned") {
		t.Fatalf("note = %q", result.Messages[1].Text)
	}
	if s.providers["rfi"].callCount()+s.providers["spec"].callCount() != 0 {
		t.Fatal("a member was called")
	}
}

func TestGroups_ToListAndMentionsOverrideTheReplyModeAndAMutedMemberAddressedAnswersAlone(t *testing.T) {
	s := setupGroup(t, []member{{"rfi", always("lead")}, {"spec", always("spec")}, {"sched", always("sched here")}}, func(d *group.Definition) {
		d.Muted = []string{"sched"}
	})
	one := s.send(t, "only you", "sched")
	if one.Plan.Kind != group.PlanSingle || one.Plan.AgentID != "sched" {
		t.Fatalf("plan = %+v", one.Plan)
	}
	equalStrings(t, kinds(one.Messages), []string{"user", "sched"}, "messages")
	equalStrings(t, one.Messages[0].To, []string{"sched"}, "to")
	contains(t, s.providers["sched"].prompts()[0], "The user addressed you.", "sched's prompt")

	two := s.send(t, "@Spec Checker and @Site Scheduler, quick check")
	if two.Plan.Kind != group.PlanRound || !slices.Equal(two.Plan.AgentIDs, []string{"spec", "sched"}) || !two.Plan.Summarize {
		t.Fatalf("plan = %+v", two.Plan)
	}
	equalStrings(t, kinds(two.Messages), []string{"user", "spec", "sched", "rfi*"}, "messages")
}

func TestGroups_OneMemberFailingDoesNotFailTheRoundAndTheSummarySaysWhatIsMissing(t *testing.T) {
	s := setupGroup(t, []member{
		{"rfi", func(_ context.Context, msgs []kernel.Message, _ llmhub.ModelOptions, _ int) (kernel.ModelResponse, error) {
			if summaryAsk.MatchString(lastPrompt(msgs)) {
				return groupReply("Summary without spec")
			}
			return groupReply("RFI view")
		}},
		{"spec", func(context.Context, []kernel.Message, llmhub.ModelOptions, int) (kernel.ModelResponse, error) {
			return kernel.ModelResponse{}, errors.New("provider exploded")
		}},
	}, func(d *group.Definition) { d.ReplyMode = group.ReplyAll })
	result := s.send(t, "go")
	if result.Status != group.TurnCompleted {
		t.Fatalf("status = %s", result.Status)
	}
	spec := findMessage(result.Messages, func(m group.Message) bool { return m.AgentID == "spec" })
	if spec == nil || spec.Status != group.StatusFailed || !strings.Contains(spec.Error, "provider exploded") {
		t.Fatalf("spec's answer = %+v", spec)
	}
	prompts := s.providers["rfi"].prompts()
	if !regexp.MustCompile(`did not answer: Spec Checker`).MatchString(prompts[len(prompts)-1]) {
		t.Fatalf("summary prompt = %s", prompts[len(prompts)-1])
	}
	if last := result.Messages[len(result.Messages)-1]; !last.Summary || last.Status != group.StatusCompleted {
		t.Fatalf("last = %+v", last)
	}
}

func TestGroups_RetriesAMembersAnswerInPlace(t *testing.T) {
	var mu sync.Mutex
	specCalls := 0
	s := setupGroup(t, []member{
		{"rfi", always("RFI view")},
		{"spec", func(context.Context, []kernel.Message, llmhub.ModelOptions, int) (kernel.ModelResponse, error) {
			mu.Lock()
			specCalls++
			n := specCalls
			mu.Unlock()
			if n == 1 {
				return kernel.ModelResponse{}, errors.New("flaky")
			}
			return groupReply("Spec view, second try")
		}},
	}, func(d *group.Definition) { d.ReplyMode = group.ReplyMention })
	first := s.send(t, "@Spec Checker check it")
	failed := findMessage(first.Messages, func(m group.Message) bool { return m.AgentID == "spec" })
	if failed == nil || failed.Status != group.StatusFailed {
		t.Fatalf("first answer = %+v", failed)
	}

	again, err := s.core.Groups().Retry(t.Context(), s.groupID, s.sessionID, failed.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(again.Messages) != 1 {
		t.Fatalf("retry messages = %d", len(again.Messages))
	}
	m := again.Messages[0]
	if m.ID != failed.ID || m.Seq != failed.Seq || m.Status != group.StatusCompleted || m.Text != "Spec view, second try" || m.Error != "" {
		t.Fatalf("retried answer = %+v", m)
	}
	if p := s.providers["spec"].prompts()[1]; !strings.Contains(p, "did not finish (failed)") {
		t.Fatalf("retry prompt = %s", p)
	}
}

func TestGroups_StopAllCancelsEveryRunningMember(t *testing.T) {
	blocked := func(ctx context.Context, _ []kernel.Message, _ llmhub.ModelOptions, _ int) (kernel.ModelResponse, error) {
		return untilAborted(ctx)
	}
	s := setupGroup(t, []member{{"rfi", blocked}, {"spec", blocked}}, func(d *group.Definition) { d.ReplyMode = group.ReplyAll })
	type outcome struct {
		res grouphub.TurnResult
		err error
	}
	done := make(chan outcome, 1)
	go func() {
		res, err := s.core.Groups().Send(context.WithoutCancel(t.Context()), s.groupID, s.sessionID, grouphub.SendRequest{Text: "take your time"})
		done <- outcome{res, err}
	}()
	waitFor(t, "both members to think", func() bool { return len(s.core.Groups().Presence(s.sessionID)) == 2 })
	var present []string
	for _, p := range s.core.Groups().Presence(s.sessionID) {
		present = append(present, p.AgentID)
	}
	slices.Sort(present)
	equalStrings(t, present, []string{"rfi", "spec"}, "presence")
	if !s.core.Groups().IsBusy(s.sessionID) {
		t.Fatal("the session is not busy")
	}
	_, err := s.core.Groups().Send(t.Context(), s.groupID, s.sessionID, grouphub.SendRequest{Text: "again"})
	if !errors.Is(err, &group.Error{Code: group.CodeBusy}) {
		t.Fatalf("second send: %v, want busy", err)
	}

	if !s.core.Groups().Stop(s.sessionID, "") {
		t.Fatal("stop returned false")
	}
	out := <-done
	if out.err != nil {
		t.Fatal(out.err)
	}
	if out.res.Status != group.TurnStopped {
		t.Fatalf("status = %s", out.res.Status)
	}
	var statuses []string
	for _, m := range out.res.Messages {
		if m.Kind == group.KindAgent {
			statuses = append(statuses, string(m.Status))
		}
		if m.Summary {
			t.Fatal("a stopped round was summed up")
		}
	}
	equalStrings(t, statuses, []string{"cancelled", "cancelled"}, "answer statuses")
	if p := s.core.Groups().Presence(s.sessionID); len(p) != 0 {
		t.Fatalf("presence after the turn = %+v", p)
	}
}

func TestGroups_StopsOneMemberAndTheOthersGoOn(t *testing.T) {
	s := setupGroup(t, []member{
		{"rfi", func(ctx context.Context, msgs []kernel.Message, _ llmhub.ModelOptions, _ int) (kernel.ModelResponse, error) {
			if summaryAsk.MatchString(lastPrompt(msgs)) {
				return groupReply("Summary")
			}
			select {
			case <-time.After(40 * time.Millisecond):
			case <-ctx.Done():
				return kernel.ModelResponse{}, ctx.Err()
			}
			return groupReply("RFI view")
		}},
		{"spec", func(ctx context.Context, _ []kernel.Message, _ llmhub.ModelOptions, _ int) (kernel.ModelResponse, error) {
			return untilAborted(ctx)
		}},
	}, func(d *group.Definition) { d.ReplyMode = group.ReplyAll })
	type outcome struct {
		res grouphub.TurnResult
		err error
	}
	done := make(chan outcome, 1)
	go func() {
		res, err := s.core.Groups().Send(context.WithoutCancel(t.Context()), s.groupID, s.sessionID, grouphub.SendRequest{Text: "go"})
		done <- outcome{res, err}
	}()
	waitFor(t, "spec to think", func() bool {
		return slices.ContainsFunc(s.core.Groups().Presence(s.sessionID), func(p events.GroupPresencePayload) bool { return p.AgentID == "spec" })
	})
	if !s.core.Groups().Stop(s.sessionID, "spec") {
		t.Fatal("stop returned false")
	}
	out := <-done
	if out.err != nil {
		t.Fatal(out.err)
	}
	if out.res.Status != group.TurnCompleted {
		t.Fatalf("status = %s", out.res.Status)
	}
	if m := findMessage(out.res.Messages, func(m group.Message) bool { return m.AgentID == "spec" && !m.Summary }); m == nil || m.Status != group.StatusCancelled {
		t.Fatalf("spec's answer = %+v", m)
	}
	if m := findMessage(out.res.Messages, func(m group.Message) bool { return m.AgentID == "rfi" && !m.Summary }); m == nil || m.Status != group.StatusCompleted {
		t.Fatalf("rfi's answer = %+v", m)
	}
	if last := out.res.Messages[len(out.res.Messages)-1]; !last.Summary {
		t.Fatalf("last = %+v, want the summary", last)
	}
}

func TestGroups_TheTokenBudgetEndsTheTurnAndSaysSo(t *testing.T) {
	s := setupGroup(t, []member{{"rfi", always("RFI view")}, {"spec", always("Spec view")}}, func(d *group.Definition) {
		d.ReplyMode = group.ReplyAll
		d.Limits = group.Limits{MaxTokensPerTurn: 20}
	})
	result := s.send(t, "go")
	if result.Status != group.TurnLimited {
		t.Fatalf("status = %s", result.Status)
	}
	if slices.ContainsFunc(result.Messages, func(m group.Message) bool { return m.Summary }) {
		t.Fatal("a limited turn was summed up")
	}
	last := result.Messages[len(result.Messages)-1]
	if last.Kind != group.KindEvent || !strings.Contains(last.Text, "used its 20 tokens") {
		t.Fatalf("last = %+v", last)
	}
}

func TestGroups_CapsTheMembersOfOneRound(t *testing.T) {
	s := setupGroup(t, []member{{"rfi", always("a")}, {"spec", always("b")}, {"sched", always("c")}}, func(d *group.Definition) {
		d.ReplyMode = group.ReplyAll
		d.Limits = group.Limits{MaxMembersPerRound: 2}
	})
	result := s.send(t, "everyone")
	equalStrings(t, kinds(result.Messages), []string{"user", "event", "rfi", "spec", "rfi*"}, "messages")
	if !strings.Contains(result.Messages[1].Text, "Site Scheduler left out: at most 2 members") {
		t.Fatalf("note = %q", result.Messages[1].Text)
	}
	if n := s.providers["sched"].callCount(); n != 0 {
		t.Fatalf("the member left out was called %d times", n)
	}
}

func TestGroups_AMemberIsToldWhatWasSaidSinceItsLastTurn(t *testing.T) {
	members := func() []member {
		return []member{
			{"rfi", func(_ context.Context, msgs []kernel.Message, _ llmhub.ModelOptions, _ int) (kernel.ModelResponse, error) {
				if summaryAsk.MatchString(lastPrompt(msgs)) {
					return groupReply("Round summary")
				}
				return groupReply("RFI answer")
			}},
			{"spec", always("Spec answer")},
		}
	}
	full := setupGroup(t, members(), func(d *group.Definition) { d.ReplyMode = group.ReplyAll })
	full.send(t, "first")
	full.send(t, "second")
	second := full.providers["spec"].prompts()[1]
	contains(t, second, "RFI Desk: RFI answer", "spec's second prompt")
	contains(t, second, "RFI Desk (summary): Round summary", "spec's second prompt")
	contains(t, second, "User: second", "spec's second prompt")
	notContains(t, second, "User: first", "spec's second prompt")               // told last time
	notContains(t, second, "Spec Checker: Spec answer", "spec's second prompt") // its own answer

	addressed := setupGroup(t, members(), func(d *group.Definition) {
		d.ReplyMode = group.ReplyAll
		d.MemberView = group.ViewAddressed
	})
	addressed.send(t, "first")
	addressed.send(t, "second")
	told := addressed.providers["spec"].prompts()[1]
	notContains(t, told, "RFI Desk: RFI answer", "spec's second prompt (addressed)")
	contains(t, told, "RFI Desk (summary): Round summary", "spec's second prompt (addressed)")
}

func TestGroups_ValidatesGroups(t *testing.T) {
	s := setupGroup(t, []member{{"rfi", always("a")}, {"spec", always("b")}}, nil)
	cases := []struct {
		mut  func(*group.Definition)
		code group.ErrorCode
	}{
		{func(d *group.Definition) { d.Members = []string{"rfi"} }, group.CodeInvalidArgument},
		{func(d *group.Definition) { d.LeadID = "nobody" }, group.CodeInvalidArgument},
		{func(d *group.Definition) { d.Muted = []string{"rfi"} }, group.CodeInvalidArgument},
		{func(d *group.Definition) { d.Members = []string{"rfi", "ghost"} }, group.CodeUnknownAgent},
		{func(d *group.Definition) { d.Name = "  " }, group.CodeInvalidArgument},
		{func(d *group.Definition) { d.ReplyMode = "loud" }, group.CodeInvalidArgument},
	}
	for i, c := range cases {
		def := group.Definition{Name: "g", Members: []string{"rfi", "spec"}, LeadID: "rfi"}
		c.mut(&def)
		_, err := s.core.Groups().Create(t.Context(), def)
		var gerr *group.Error
		if !errors.As(err, &gerr) || gerr.Code != c.code || gerr.Name() != "GroupError" {
			raw, _ := json.Marshal(def)
			t.Fatalf("case %d (%s): err = %v, want code %s", i, raw, err, c.code)
		}
	}
}

func TestGroups_RecordsChangesAsNotesAndDropsADeletedAgentFromItsGroups(t *testing.T) {
	s := setupGroup(t, []member{{"rfi", always("a")}, {"spec", always("b")}, {"sched", always("c")}}, nil)
	lead, mode := "spec", group.ReplyAll
	if _, err := s.core.Groups().Update(t.Context(), s.groupID, group.Patch{LeadID: &lead, ReplyMode: &mode, Muted: []string{"sched"}}, grouphub.UpdateOptions{NoteIn: s.sessionID}); err != nil {
		t.Fatal(err)
	}
	var notes []string
	for _, m := range s.thread(t) {
		notes = append(notes, m.Text)
	}
	equalStrings(t, notes, []string{"Spec Checker now leads.", "Who answers is now \u201cEveryone answers\u201d.", "Site Scheduler is muted."}, "notes")

	if err := s.core.RemoveAgent(t.Context(), "spec"); err != nil {
		t.Fatal(err)
	}
	g, err := s.core.Groups().Get(t.Context(), s.groupID)
	if err != nil || g == nil {
		t.Fatalf("group = %v, %v", g, err)
	}
	if !slices.Equal(g.Members, []string{"rfi", "sched"}) || g.LeadID != "rfi" {
		t.Fatalf("group after removal = %+v", g)
	}
}

func TestGroups_DeletesASessionWithItsThreadAndMemberSessions(t *testing.T) {
	s := setupGroup(t, []member{{"rfi", always("a")}, {"spec", always("b")}}, func(d *group.Definition) { d.ReplyMode = group.ReplyAll })
	s.send(t, "hi")
	if err := s.core.Groups().DeleteSession(t.Context(), s.groupID, s.sessionID); err != nil {
		t.Fatal(err)
	}
	if session, err := s.core.Groups().GetSession(t.Context(), s.groupID, s.sessionID); err != nil || session != nil {
		t.Fatalf("session after delete = %v, %v", session, err)
	}
	if msgs, err := s.core.PersistenceHub().GroupMessages.List(t.Context(), s.sessionID, 0); err != nil || len(msgs) != 0 {
		t.Fatalf("thread after delete = %v, %v", msgs, err)
	}
	filter := ordered.NewObject()
	filter.Set("source", "group-member")
	filter.Set("groupSessionId", s.sessionID)
	if left, err := s.core.PersistenceHub().Sessions.List(t.Context(), kernel.SessionFilter{Metadata: filter}); err != nil || len(left) != 0 {
		t.Fatalf("member sessions after delete = %v, %v", left, err)
	}
}
