package agent_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/Phong95/aec-agent-go/aecmock"
	"github.com/Phong95/aec-agent-go/components/agent"
	"github.com/Phong95/aec-agent-go/components/agent/conversation"
	"github.com/Phong95/aec-agent-go/components/agent/profile"
	"github.com/Phong95/aec-agent-go/components/agent/subagent"
	"github.com/Phong95/aec-agent-go/components/channelplatformhub"
	"github.com/Phong95/aec-agent-go/components/channelplatformhub/channel"
	"github.com/Phong95/aec-agent-go/components/events"
	"github.com/Phong95/aec-agent-go/components/llmhub"
	"github.com/Phong95/aec-agent-go/components/toolhub"
	"github.com/Phong95/aec-agent-go/components/toolhub/builtin/channels"
	"github.com/Phong95/aec-agent-go/components/toolhub/builtin/conversations"
	"github.com/Phong95/aec-agent-go/kernel"
	"github.com/Phong95/aec-agent-go/kernel/ordered"
)

// A-218 (agent-reach.test.ts of core/ts): the agent's reach. The channel tool (A-219) posts
// to a channel's home chat; the conversations tool (A-220) finds and reads the agent's
// other conversations, inside the access scope worked out for the calling session.

// recordingInteraction approves or denies every call and records what it was asked.
type recordingInteraction struct {
	deny bool
	mu   sync.Mutex
	asks []toolhub.ApprovalRequest
}

func (*recordingInteraction) AskUser(context.Context, toolhub.ClarifyRequest) (toolhub.ClarifyResponse, error) {
	return toolhub.ClarifyResponse{}, nil
}

func (r *recordingInteraction) RequestApproval(_ context.Context, req toolhub.ApprovalRequest) (toolhub.ApprovalDecision, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.asks = append(r.asks, req)
	return toolhub.ApprovalDecision{Approved: !r.deny}, nil
}

func (r *recordingInteraction) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.asks)
}

type reachOptions struct {
	access      channel.ConversationAccess
	maxLength   int
	interaction *recordingInteraction
	scope       conversation.Scope
}

type reachRig struct {
	e        *env
	a        *agent.Agent
	provider *scriptedProvider
	platform *aecmock.FakeChannelPlatform
	ch       channel.Channel
	adapter  *aecmock.FakeChannelAdapter
	// release ends the turn of a message saying "hold"; entered is closed once that turn
	// has made its model call, so the test may change the script.
	release chan struct{}
	entered chan struct{}
}

func newReachRig(t *testing.T, o reachOptions) *reachRig {
	t.Helper()
	if o.access == "" {
		o.access = channel.ConversationAccessChat
	}
	if o.maxLength == 0 {
		o.maxLength = 4000
	}
	if o.interaction == nil {
		o.interaction = &recordingInteraction{}
	}
	r := &reachRig{e: newEnv(t, "unused"), release: make(chan struct{}), entered: make(chan struct{})}
	enter := sync.OnceFunc(func() { close(r.entered) })
	r.provider = &scriptedProvider{reply: func(lastUser, result string) string {
		if lastUser == "hold" {
			enter()
			<-r.release
		}
		if result != "" {
			return "RESULT " + result
		}
		return "ok"
	}}
	r.e.llm = llmhub.NewLLMHub()
	mustOK(t, r.e.llm.Register(r.provider))
	for _, tool := range append(channels.NewTools(), conversations.NewTools()...) {
		mustOK(t, r.e.tools.Register(tool))
	}
	r.e.tools.SetInteractionHandler(o.interaction)
	hub := channelplatformhub.New()
	r.platform = aecmock.NewFakeChannelPlatformWithCapabilities("fake", channel.Capabilities{MaxMessageLength: o.maxLength, Buttons: true})
	mustOK(t, hub.Register(r.platform))
	deps := r.e.deps()
	deps.ChannelPlatformHub = hub
	if o.scope != nil {
		deps.ConversationScope = func() conversation.Scope { return o.scope }
	}
	p := testProfile("site-assistant")
	p.Name = "Site Assistant"
	p.Tools = append(channels.ToolNames(), conversations.ToolNames()...)
	retries := 0
	p.LoopConfig = &profile.LoopConfig{MaxToolRetries: &retries}
	a, err := agent.New(t.Context(), deps, p)
	mustOK(t, err)
	t.Cleanup(func() { _ = a.Dispose(context.WithoutCancel(t.Context())) })
	r.a = a
	open, no := channel.DmOpen, false
	groups := channel.GroupOpen
	r.ch, err = a.Channels().Create(t.Context(), channel.CreateInput{
		Name: "Team bot", Platform: "fake", Credentials: map[string]string{"token": "t"},
		Access: &channel.AccessInput{DmPolicy: &open, AllowFrom: []string{"owner"}, GroupPolicy: &groups, RequireMention: &no, ConversationAccess: &o.access},
	})
	mustOK(t, err)
	r.adapter = r.platform.AdapterFor(r.ch.ID)
	return r
}

func (r *reachRig) setHome(t *testing.T, id, chatID, title string) {
	t.Helper()
	_, err := r.a.Channels().Update(t.Context(), id, channel.UpdateInput{Home: &channel.HomeInput{ChatID: chatID, Title: title}})
	mustOK(t, err)
}

// session makes a conversation of the agent with these extra metadata pairs.
func (r *reachRig) session(t *testing.T, title string, pairs ...string) string {
	t.Helper()
	md := ordered.NewObject()
	md.Set("agentId", r.a.Profile().ID)
	md.Set("title", title)
	for i := 0; i+1 < len(pairs); i += 2 {
		md.Set(pairs[i], pairs[i+1])
	}
	s, err := r.e.hub.Sessions.Create(t.Context(), md)
	mustOK(t, err)
	return s.ID
}

func (r *reachRig) say(t *testing.T, id string, role kernel.Role, content string) {
	t.Helper()
	mustOK(t, r.e.hub.Messages.Append(t.Context(), id, kernel.Message{Role: role, Content: content}))
}

func (r *reachRig) markJob(t *testing.T, id string) {
	t.Helper()
	s, err := r.e.hub.Sessions.Get(t.Context(), id)
	mustOK(t, err)
	s.JobID = "job-1"
	mustOK(t, r.e.hub.Sessions.Update(t.Context(), *s))
}

// turn runs one turn calling the tool, returning what the tool answered.
func (r *reachRig) turn(t *testing.T, session, tool, args string) string {
	t.Helper()
	r.provider.toolName, r.provider.toolArgs = tool, args
	res, err := r.a.Chat(t.Context(), "go", agent.WithChatSession(session))
	mustOK(t, err)
	return res.Message.Content
}

// inChat sends a chat message whose turn calls the tool, returning the bot's reply.
func (r *reachRig) inChat(t *testing.T, msg channel.InboundMessage, tool, args string) string {
	t.Helper()
	before := len(r.adapter.Sent())
	r.provider.toolName, r.provider.toolArgs = tool, args
	mustOK(t, r.adapter.Receive(msg))
	reply := ""
	waitFor(t, "the tool's result in the chat", func() bool {
		for _, m := range r.adapter.Sent()[before:] {
			// Not while it is still being streamed in (a cursor at its end).
			if strings.Contains(m.Text, "RESULT") && !strings.HasSuffix(m.Text, "\u258d") {
				reply = m.Text
				return true
			}
		}
		return false
	})
	return reply
}

func (r *reachRig) channelSessions(t *testing.T) []kernel.Session {
	t.Helper()
	filter := ordered.NewObject()
	filter.Set("agentId", r.a.Profile().ID)
	filter.Set("source", "channel")
	all, err := r.e.hub.Sessions.List(t.Context(), kernel.SessionFilter{Metadata: filter})
	mustOK(t, err)
	return all
}

func (r *reachRig) lastMessage(t *testing.T, id string) string {
	t.Helper()
	all, err := r.e.hub.Messages.Get(t.Context(), id)
	mustOK(t, err)
	if len(all) == 0 {
		return ""
	}
	return all[len(all)-1].Content
}

func reachContains(t *testing.T, got string, wants ...string) {
	t.Helper()
	for _, want := range wants {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
}

func reachMissing(t *testing.T, got string, hidden ...string) {
	t.Helper()
	for _, h := range hidden {
		if strings.Contains(got, h) {
			t.Errorf("should not contain %q:\n%s", h, got)
		}
	}
}

const channelList = `{"action":"list"}`

func channelSend(text, name string) string {
	return fmt.Sprintf(`{"action":"send","text":%q,"channel":%q}`, text, name)
}

func TestChannelToolPostsTheExactTextAndRecordsIt(t *testing.T) {
	r := newReachRig(t, reachOptions{})
	r.setHome(t, r.ch.ID, "chat-owner", "Owner")
	var mu sync.Mutex
	var sessions []string
	off := events.Subscribe(r.e.bus, events.ChannelMessageSent, func(e kernel.Event, _ events.ChannelMessageSentPayload) {
		mu.Lock()
		defer mu.Unlock()
		sessions = append(sessions, e.SessionID)
	})
	defer off()
	web := r.session(t, "web")

	out := r.turn(t, web, "channel", channelSend("Pour *C30* at 7am", ""))

	reachContains(t, out, `Sent to "Owner" on Team bot (fake), in 1 message.`)
	if got := r.adapter.SentTexts(); len(got) != 1 || got[0] != "Pour *C30* at 7am" {
		t.Fatalf("sent = %q", got)
	}
	waitFor(t, "the sent text in the chat's conversation", func() bool {
		all := r.channelSessions(t)
		return len(all) == 1 && r.lastMessage(t, all[0].ID) == "Pour *C30* at 7am"
	})
	mu.Lock()
	defer mu.Unlock()
	if len(sessions) != 1 || sessions[0] != web {
		t.Fatalf("message-sent sessions = %v, want the sender's", sessions)
	}
}

func TestChannelToolListNeverShowsAChatID(t *testing.T) {
	r := newReachRig(t, reachOptions{})
	web := r.session(t, "web")
	reachContains(t, r.turn(t, web, "channel", channelList), "no home chat")
	r.setHome(t, r.ch.ID, "chat-owner", "Owner")
	out := r.turn(t, web, "channel", channelList)
	reachContains(t, out, `"Team bot" (fake): home chat "Owner"`)
	reachMissing(t, out, "chat-owner")
}

func TestChannelToolListNeverAsksAndSendAsks(t *testing.T) {
	asked := &recordingInteraction{deny: true}
	r := newReachRig(t, reachOptions{interaction: asked})
	r.setHome(t, r.ch.ID, "chat-owner", "Owner")
	web := r.session(t, "web")
	r.turn(t, web, "channel", channelList)
	if asked.count() != 0 {
		t.Fatalf("list asked %d times", asked.count())
	}
	r.turn(t, web, "channel", channelSend("secret plan", "fake"))
	if asked.count() != 1 {
		t.Fatalf("send asked %d times", asked.count())
	}
	reachContains(t, string(asked.asks[0].Arguments), "secret plan", "fake")
	if got := r.adapter.SentTexts(); len(got) != 0 {
		t.Fatalf("a denied send posted %q", got)
	}
}

func TestChannelToolChoosingTheChannel(t *testing.T) {
	r := newReachRig(t, reachOptions{})
	web := r.session(t, "web")
	reachContains(t, r.turn(t, web, "channel", channelSend("x", "")), "/sethome")

	r.setHome(t, r.ch.ID, "chat-owner", "Owner")
	second, err := r.a.Channels().Create(t.Context(), channel.CreateInput{Name: "Ops bot", Platform: "fake", Credentials: map[string]string{"token": "u"}})
	mustOK(t, err)
	r.setHome(t, second.ID, "chat-ops", "Ops")

	reachContains(t, r.turn(t, web, "channel", channelSend("x", "")), "several channels have a home chat", `"Team bot"`, `"Ops bot"`)
	if got := r.adapter.SentTexts(); len(got) != 0 {
		t.Fatalf("an ambiguous send posted %q", got)
	}
	reachContains(t, r.turn(t, web, "channel", channelSend("x", "fake")), "matches several channels")
	r.turn(t, web, "channel", channelSend("by name", "OPS BOT"))
	if got := r.platform.AdapterFor(second.ID).SentTexts(); len(got) != 1 || got[0] != "by name" {
		t.Fatalf("second channel sent = %q", got)
	}
	r.turn(t, web, "channel", channelSend("by id", r.ch.ID))
	if got := r.adapter.SentTexts(); len(got) != 1 || got[0] != "by id" {
		t.Fatalf("first channel sent = %q", got)
	}
}

func TestChannelToolUnknownOrHomelessChannelFails(t *testing.T) {
	r := newReachRig(t, reachOptions{})
	r.setHome(t, r.ch.ID, "chat-owner", "Owner")
	_, err := r.a.Channels().Create(t.Context(), channel.CreateInput{Name: "Ops bot", Platform: "fake", Credentials: map[string]string{"token": "u"}})
	mustOK(t, err)
	web := r.session(t, "web")
	reachContains(t, r.turn(t, web, "channel", channelSend("x", "slack")), `no channel "slack"`, `"Team bot"`)
	reachContains(t, r.turn(t, web, "channel", channelSend("x", "Ops bot")), `"Ops bot" has no home chat yet`)
}

func TestChannelToolStoppedChannelReadsAsNotConnected(t *testing.T) {
	r := newReachRig(t, reachOptions{})
	r.setHome(t, r.ch.ID, "chat-owner", "Owner")
	disabled := false
	_, err := r.a.Channels().Update(t.Context(), r.ch.ID, channel.UpdateInput{Enabled: &disabled})
	mustOK(t, err)
	reachContains(t, r.turn(t, r.session(t, "web"), "channel", channelSend("x", "")), "is not connected")
}

func TestChannelToolPlatformFailureComesBackAndIsNotRecorded(t *testing.T) {
	r := newReachRig(t, reachOptions{})
	r.setHome(t, r.ch.ID, "chat-owner", "Owner")
	r.adapter.FailNextSend(errors.New("429 slow down"))
	reachContains(t, r.turn(t, r.session(t, "web"), "channel", channelSend("x", "")), "429 slow down")
	if all := r.channelSessions(t); len(all) != 0 {
		t.Fatalf("a failed send made %d chat sessions", len(all))
	}
}

func TestChannelToolSplitsLongText(t *testing.T) {
	r := newReachRig(t, reachOptions{maxLength: 50})
	r.setHome(t, r.ch.ID, "chat-owner", "Owner")
	words := make([]string, 30)
	for i := range words {
		words[i] = fmt.Sprintf("word%d", i)
	}
	out := r.turn(t, r.session(t, "web"), "channel", channelSend(strings.Join(words, " "), ""))
	sent := r.adapter.SentTexts()
	if len(sent) < 2 {
		t.Fatalf("sent = %q", sent)
	}
	reachContains(t, out, fmt.Sprintf("in %d messages", len(sent)))
	for _, m := range sent {
		if len(m) > 50 {
			t.Fatalf("part of %d chars", len(m))
		}
	}
}

func TestChannelToolFromTheHomeChatSaysJustReply(t *testing.T) {
	r := newReachRig(t, reachOptions{})
	r.setHome(t, r.ch.ID, "chat-owner", "Owner")
	mustOK(t, r.adapter.Receive(aecmock.FakeDirectMessage("owner", "/yolo on")))
	waitFor(t, "the /yolo reply", func() bool { return len(r.adapter.Sent()) == 1 })
	out := r.inChat(t, aecmock.FakeDirectMessage("owner", "send this home"), "channel", channelSend("x", ""))
	reachContains(t, out, "just reply here")
	for _, m := range r.adapter.SentTexts() {
		if m == "x" {
			t.Fatal("posted to the chat the turn came from")
		}
	}
}

func TestChannelToolRefusesJobRunsAndSubagents(t *testing.T) {
	r := newReachRig(t, reachOptions{})
	r.setHome(t, r.ch.ID, "chat-owner", "Owner")
	job := r.session(t, "run")
	r.markJob(t, job)
	child := r.session(t, "child", "source", "subagent")
	for _, id := range []string{job, child} {
		reachContains(t, r.turn(t, id, "channel", channelSend("x", "")), "job runs and subagents cannot post")
	}
	if got := r.adapter.SentTexts(); len(got) != 0 {
		t.Fatalf("sent = %q", got)
	}
	if !subagent.IsChildExcludedTool("channel") || !subagent.IsChildExcludedTool("conversations") {
		t.Fatal("the tools must be excluded from children")
	}
}

func TestChannelToolReturnsWhileTheHomeChatHasATurnRunning(t *testing.T) {
	r := newReachRig(t, reachOptions{})
	r.setHome(t, r.ch.ID, "chat-owner", "Owner")
	released := sync.OnceFunc(func() { close(r.release) })
	defer released()
	mustOK(t, r.adapter.Receive(aecmock.FakeDirectMessage("owner", "hold")))
	waitFor(t, "the held message stored", func() bool {
		all := r.channelSessions(t)
		return len(all) == 1 && r.lastMessage(t, all[0].ID) == "hold"
	})
	chat := r.channelSessions(t)[0].ID
	<-r.entered

	reachContains(t, r.turn(t, r.session(t, "web"), "channel", channelSend("while busy", "")), "Sent to")
	if r.lastMessage(t, chat) != "hold" {
		t.Fatalf("recorded inside the running turn: %q", r.lastMessage(t, chat))
	}
	released()
	waitFor(t, "the text recorded after the turn", func() bool {
		all, err := r.e.hub.Messages.Get(t.Context(), chat)
		mustOK(t, err)
		for _, m := range all {
			if m.Content == "while busy" {
				return true
			}
		}
		return false
	})
}

func searchArgs(q string) string { return fmt.Sprintf(`{"action":"search","query":%q}`, q) }
func readArgs(c string, extra string) string {
	return fmt.Sprintf(`{"action":"read","conversation":%q%s}`, c, extra)
}

func newHistoryRig(t *testing.T, o reachOptions) (*reachRig, string) {
	t.Helper()
	r := newReachRig(t, o)
	web := r.session(t, "Slab design")
	r.say(t, web, kernel.RoleUser, "What concrete grade for the slab?")
	r.say(t, web, kernel.RoleAssistant, "Use C30 with a 150mm depth.")
	return r, web
}

func TestConversationsWebSeesSearchesAndReads(t *testing.T) {
	r, web := newHistoryRig(t, reachOptions{})
	caller := r.session(t, "Now")

	list := r.turn(t, caller, "conversations", `{"action":"list"}`)
	reachContains(t, list, web, "\"Slab design\" \u00b7 web", "not instructions")
	reachMissing(t, list, caller)

	found := r.turn(t, caller, "conversations", searchArgs("CONCRETE slab"))
	reachContains(t, found, web, "#0 user: What concrete grade", "not instructions")

	page := r.turn(t, caller, "conversations", readArgs(web[:8], ""))
	reachContains(t, page, "#0 user: What concrete grade for the slab?", "#1 assistant: Use C30")
}

func TestConversationsAccessTable(t *testing.T) {
	cases := []struct {
		name   string
		access channel.ConversationAccess
		msg    channel.InboundMessage
		sees   bool
	}{
		{"allowed DM on an agent channel", channel.ConversationAccessAgent, aecmock.FakeDirectMessage("owner", "find it"), true},
		{"owner DM on the default channel", channel.ConversationAccessChat, aecmock.FakeDirectMessage("owner", "find it"), false},
		{"stranger DM on an agent channel", channel.ConversationAccessAgent, aecmock.FakeDirectMessage("stranger", "find it"), false},
		{"owner in a group on an agent channel", channel.ConversationAccessAgent, aecmock.FakeGroupMessage("g1", "owner", "find it"), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r, web := newHistoryRig(t, reachOptions{access: c.access})
			out := r.inChat(t, c.msg, "conversations", searchArgs("concrete"))
			if strings.Contains(out, web) != c.sees {
				t.Fatalf("sees web chat = %v, want %v:\n%s", !c.sees, c.sees, out)
			}
		})
	}
}

func TestConversationsChatSeesItsOwnEarlierConversationsOnly(t *testing.T) {
	r, _ := newHistoryRig(t, reachOptions{})
	earlier := r.session(t, "earlier", "source", "channel", "channelId", r.ch.ID, "chatKey", "chat-owner")
	other := r.session(t, "other", "source", "channel", "channelId", r.ch.ID, "chatKey", "chat-someone")
	r.say(t, earlier, kernel.RoleUser, "the rebar spacing question")
	r.say(t, other, kernel.RoleUser, "the rebar secret")
	mustOK(t, r.adapter.Receive(aecmock.FakeDirectMessage("owner", "/new")))
	waitFor(t, "the /new reply", func() bool { return len(r.adapter.Sent()) == 1 })

	out := r.inChat(t, aecmock.FakeDirectMessage("owner", "find it"), "conversations", searchArgs("rebar"))
	reachContains(t, out, earlier)
	reachMissing(t, out, other)
}

func TestConversationsGuessedIDOutsideTheScopeReadsAsNotFound(t *testing.T) {
	r, web := newHistoryRig(t, reachOptions{})
	out := r.inChat(t, aecmock.FakeDirectMessage("owner", "read it"), "conversations", readArgs(web, ""))
	reachContains(t, out, `no conversation "`+web+`" found`)
}

func TestConversationsRefusedForJobsSubagentsAndMembers(t *testing.T) {
	r, _ := newHistoryRig(t, reachOptions{})
	job := r.session(t, "run")
	r.markJob(t, job)
	for _, id := range []string{job, r.session(t, "child", "source", "subagent"), r.session(t, "member", "source", "group-member")} {
		reachContains(t, r.turn(t, id, "conversations", `{"action":"list"}`), "cannot read other conversations")
	}
}

func TestConversationsNeverShowsOtherAgentsOrHiddenSessions(t *testing.T) {
	r, _ := newHistoryRig(t, reachOptions{})
	md := ordered.NewObject()
	md.Set("agentId", "another-agent")
	otherAgent, err := r.e.hub.Sessions.Create(t.Context(), md)
	mustOK(t, err)
	job := r.session(t, "job run")
	r.markJob(t, job)
	hidden := []string{otherAgent.ID, job, r.session(t, "child run", "source", "subagent"), r.session(t, "member run", "source", "group-member")}
	for _, id := range hidden {
		r.say(t, id, kernel.RoleUser, "concrete")
	}
	reachMissing(t, r.turn(t, r.session(t, "Now"), "conversations", searchArgs("concrete")), hidden...)
}

func TestConversationsHostScopeSeparatesUsers(t *testing.T) {
	scope := func(s kernel.Session) ordered.Object {
		out := ordered.NewObject()
		if id, ok := s.Metadata.Get("userId"); ok {
			out.Set("userId", id)
		}
		return out
	}
	r, _ := newHistoryRig(t, reachOptions{scope: scope})
	mine := r.session(t, "mine", "userId", "u1")
	theirs := r.session(t, "theirs", "userId", "u2")
	r.say(t, mine, kernel.RoleUser, "concrete mine")
	r.say(t, theirs, kernel.RoleUser, "concrete theirs")

	out := r.turn(t, r.session(t, "Now", "userId", "u1"), "conversations", searchArgs("concrete"))
	reachContains(t, out, mine)
	reachMissing(t, out, theirs)
}

func TestConversationsReturnsOnlyUserAndAssistantText(t *testing.T) {
	r, _ := newHistoryRig(t, reachOptions{})
	id := r.session(t, "tools")
	mustOK(t, r.e.hub.Messages.Set(t.Context(), id, []kernel.Message{
		{Role: kernel.RoleSystem, Content: "SYSTEM PROMPT"},
		{Role: kernel.RoleUser, Content: "start"},
		{Role: kernel.RoleAssistant, ToolCalls: []kernel.ToolCall{{ID: "1", Name: "terminal", Arguments: kernel.Arguments(`{"command":"TOOLCALL"}`)}}},
		{Role: kernel.RoleTool, Content: "TOOLRESULT"},
		{Role: kernel.RoleAssistant, Content: "finished"},
	}))
	out := r.turn(t, r.session(t, "Now"), "conversations", readArgs(id, ""))
	reachContains(t, out, "#1 user: start", "#4 assistant: finished")
	reachMissing(t, out, "SYSTEM PROMPT", "TOOLCALL", "TOOLRESULT")
}

func TestConversationsPagesBackwards(t *testing.T) {
	r, _ := newHistoryRig(t, reachOptions{})
	id := r.session(t, "long")
	for i := range 45 {
		role := kernel.RoleUser
		if i%2 == 1 {
			role = kernel.RoleAssistant
		}
		r.say(t, id, role, fmt.Sprintf("message %d", i))
	}
	newest := r.turn(t, r.session(t, "a"), "conversations", readArgs(id, ""))
	reachContains(t, newest, "#44 ", "#15 ", "pass before 15")
	reachMissing(t, newest, "#14 ")

	older := r.turn(t, r.session(t, "b"), "conversations", readArgs(id, `,"before":15,"limit":5`))
	reachContains(t, older, "#14 ", "#10 ", "pass before 10")
	reachMissing(t, older, "#15 ")

	first := r.turn(t, r.session(t, "c"), "conversations", readArgs(id, `,"before":3`))
	reachContains(t, first, "#0 ")
	reachMissing(t, first, "pass before")
}

func TestConversationsCapsAReadAndDropsTheOldest(t *testing.T) {
	r, _ := newHistoryRig(t, reachOptions{})
	id := r.session(t, "big")
	for i := range 6 {
		r.say(t, id, kernel.RoleUser, fmt.Sprintf("%d:%s", i, strings.Repeat("x", 5000)))
	}
	out := r.turn(t, r.session(t, "a"), "conversations", readArgs(id, ""))
	reachContains(t, out, "#5 ", "#2 ", "[cut]", "pass before 2")
	reachMissing(t, out, "#1 ")
}

func TestConversationsListCapsAndSkipsEmptyChats(t *testing.T) {
	r, _ := newHistoryRig(t, reachOptions{})
	r.session(t, "never used")
	for i := range 30 {
		r.say(t, r.session(t, fmt.Sprintf("s%d", i)), kernel.RoleUser, "x")
	}
	caller := r.session(t, "Now")
	count := func(s string) int { return strings.Count(s, "\n- [") }
	if got := count(r.turn(t, caller, "conversations", `{"action":"list","limit":0}`)); got != 10 {
		t.Fatalf("default list = %d", got)
	}
	big := r.turn(t, caller, "conversations", `{"action":"list","limit":99}`)
	if got := count(big); got != 25 {
		t.Fatalf("capped list = %d", got)
	}
	reachMissing(t, big, "never used")
}

func TestConversationsRanksByWordsHit(t *testing.T) {
	r, _ := newHistoryRig(t, reachOptions{})
	one := r.session(t, "one word")
	r.say(t, one, kernel.RoleUser, "alpha only")
	both := r.session(t, "both words")
	r.say(t, both, kernel.RoleUser, "alpha then")
	r.say(t, both, kernel.RoleAssistant, "beta too")
	out := r.turn(t, r.session(t, "Now"), "conversations", searchArgs("alpha beta"))
	if a, b := strings.Index(out, both), strings.Index(out, one); a < 0 || b < 0 || a > b {
		t.Fatalf("order wrong (both at %d, one at %d):\n%s", a, b, out)
	}
}

func TestConversationsAmbiguousPrefixListsTheMatches(t *testing.T) {
	r, _ := newHistoryRig(t, reachOptions{})
	ids := []string{"abcd1111-0000", "abcd2222-0000"}
	for _, id := range ids {
		md := ordered.NewObject()
		md.Set("agentId", r.a.Profile().ID)
		s, err := r.e.hub.Sessions.Create(t.Context(), md)
		mustOK(t, err)
		mustOK(t, r.e.hub.Sessions.Delete(t.Context(), s.ID))
		s.ID = id
		mustOK(t, r.e.hub.Sessions.Update(t.Context(), s))
		r.say(t, id, kernel.RoleUser, "x")
	}
	reachContains(t, r.turn(t, r.session(t, "Now"), "conversations", readArgs("abcd", "")), "matches 2 conversations", ids[0], ids[1])
	reachContains(t, r.turn(t, r.session(t, "Now2"), "conversations", readArgs("abc", "")), `no conversation "abc" found`)
}

func TestConversationsRunsInPlanModeWithoutAsking(t *testing.T) {
	asked := &recordingInteraction{deny: true}
	r := newReachRig(t, reachOptions{interaction: asked})
	web := r.session(t, "old")
	r.say(t, web, kernel.RoleUser, "concrete")
	caller := r.session(t, "plan")
	_, _, err := r.a.SetApprovalMode(t.Context(), caller, kernel.ApprovalModePlan)
	mustOK(t, err)
	reachContains(t, r.turn(t, caller, "conversations", searchArgs("concrete")), web)
	if asked.count() != 0 {
		t.Fatalf("asked %d times", asked.count())
	}
}
