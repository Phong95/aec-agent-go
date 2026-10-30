package agent_test

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Phong95/aec-agent-go/aecmock"
	"github.com/Phong95/aec-agent-go/components/agent"
	"github.com/Phong95/aec-agent-go/components/channelplatformhub"
	"github.com/Phong95/aec-agent-go/components/channelplatformhub/channel"
	"github.com/Phong95/aec-agent-go/components/events"
	"github.com/Phong95/aec-agent-go/kernel"
	"github.com/Phong95/aec-agent-go/kernel/ordered"
)

// A-100 channels through Agent.Channels: access rules, sessions, commands and lifecycle,
// over the in-memory fake platform of aecmock.

// channelRig is an env, a channel platform hub with the fake platform, and an agent.
type channelRig struct {
	e        *env
	hub      *channelplatformhub.ChannelPlatformHub
	platform *aecmock.FakeChannelPlatform
	a        *agent.Agent
	runner   atomic.Pointer[channel.TurnRunner]
}

func newChannelRig(t *testing.T, maxLen int) *channelRig {
	t.Helper()
	r := &channelRig{e: newEnv(t), hub: channelplatformhub.New(), platform: aecmock.NewFakeChannelPlatform("fake", maxLen)}
	if err := r.hub.Register(r.platform); err != nil {
		t.Fatal(err)
	}
	r.a = r.spawn(t, "channel-agent")
	return r
}

// spawn builds another agent over the rig's hubs.
func (r *channelRig) spawn(t *testing.T, id string) *agent.Agent {
	t.Helper()
	deps := r.e.deps()
	deps.ChannelPlatformHub = r.hub
	deps.ChannelRunner = func() channel.TurnRunner {
		if p := r.runner.Load(); p != nil {
			return *p
		}
		return nil
	}
	a, err := agent.New(t.Context(), deps, testProfile(id))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.Dispose(context.WithoutCancel(t.Context())) })
	return a
}

func (r *channelRig) useRunner(run channel.TurnRunner) { r.runner.Store(&run) }

// open creates an enabled channel with dmPolicy open and returns it with its adapter.
func (r *channelRig) open(t *testing.T) (channel.Channel, *aecmock.FakeChannelAdapter) {
	t.Helper()
	open := channel.DmOpen
	ch, err := r.a.Channels().Create(t.Context(), channel.CreateInput{
		Platform: "fake", Credentials: map[string]string{"token": "t"}, Access: &channel.AccessInput{DmPolicy: &open},
	})
	if err != nil {
		t.Fatal(err)
	}
	return ch, r.platform.AdapterFor(ch.ID)
}

// waitFor polls cond until it holds, or fails the test after 5 s.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// waitTexts waits until the adapter has sent exactly want.
func waitTexts(t *testing.T, a *aecmock.FakeChannelAdapter, want ...string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if slices.Equal(a.SentTexts(), want) {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("sent %q, want %q", a.SentTexts(), want)
}

func channelSessions(t *testing.T, a *agent.Agent) []kernel.Session {
	t.Helper()
	filter := ordered.NewObject()
	filter.Set("source", "channel")
	sessions, err := a.ListSessions(t.Context(), kernel.SessionFilter{Metadata: filter})
	if err != nil {
		t.Fatal(err)
	}
	return sessions
}

func mustOK(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func codeOf(err error) channel.ErrorCode {
	var ce *channel.Error
	if errors.As(err, &ce) {
		return ce.Code
	}
	return ""
}

func TestChannelCreateConnectsAndRecordsTheAccount(t *testing.T) {
	r := newChannelRig(t, 0)
	ch, err := r.a.Channels().Create(t.Context(), channel.CreateInput{Platform: "fake", Credentials: map[string]string{"token": "t1"}})
	if err != nil {
		t.Fatal(err)
	}
	if ch.Name != "Fake Bot" || !ch.Enabled || ch.Access.DmPolicy != channel.DmPairing || len(ch.Access.AllowFrom) != 0 {
		t.Fatalf("channel = %+v", ch)
	}
	if ch.Account == nil || *ch.Account != (channel.AccountInfo{ID: "bot-t1", Username: "fake_bot", DisplayName: "Fake Bot"}) {
		t.Fatalf("account = %+v", ch.Account)
	}
	if s := r.a.Channels().State(ch.ID); s.Status != channel.StatusRunning {
		t.Fatalf("state = %+v", s)
	}
	stored, err := r.a.Channels().Get(t.Context(), ch.ID)
	if err != nil || stored == nil || stored.Account == nil || stored.Name != "Fake Bot" {
		t.Fatalf("stored = %+v %v", stored, err)
	}
}

func TestChannelCreateSavesNothingWhenTheCredentialsAreRejected(t *testing.T) {
	r := newChannelRig(t, 0)
	_, err := r.a.Channels().Create(t.Context(), channel.CreateInput{Platform: "fake", Credentials: map[string]string{"token": "bad"}})
	if codeOf(err) != channel.CodeConnectFailed || !strings.Contains(err.Error(), "invalid token") {
		t.Fatalf("err = %v", err)
	}
	if all, _ := r.a.Channels().List(t.Context()); len(all) != 0 {
		t.Fatalf("saved %d channels", len(all))
	}
}

func TestChannelCreateValidates(t *testing.T) {
	r := newChannelRig(t, 0)
	everyone := channel.DmPolicy("everyone")
	blank := " "
	cases := map[channel.ErrorCode]channel.CreateInput{
		channel.CodeUnknownPlatform: {Platform: "slack"},
		channel.CodeInvalidArgument: {Platform: "fake", Credentials: map[string]string{}},
	}
	for want, in := range cases {
		if _, err := r.a.Channels().Create(t.Context(), in); codeOf(err) != want {
			t.Errorf("%+v: err %v, want %s", in, err, want)
		}
	}
	if _, err := r.a.Channels().Create(t.Context(), channel.CreateInput{Platform: "fake", Credentials: map[string]string{"token": "t"}, Access: &channel.AccessInput{DmPolicy: &everyone}}); codeOf(err) != channel.CodeInvalidArgument {
		t.Errorf("bad dmPolicy: %v", err)
	}
	if _, err := r.a.Channels().Create(t.Context(), channel.CreateInput{Platform: "fake", Credentials: map[string]string{"token": "t"}, Name: blank}); codeOf(err) != channel.CodeInvalidArgument {
		t.Errorf("blank name: %v", err)
	}
}

func TestChannelSameBotCannotConnectTwice(t *testing.T) {
	r := newChannelRig(t, 0)
	if _, err := r.a.Channels().Create(t.Context(), channel.CreateInput{Platform: "fake", Credentials: map[string]string{"token": "same"}}); err != nil {
		t.Fatal(err)
	}
	other := r.spawn(t, "other-agent")
	_, err := other.Channels().Create(t.Context(), channel.CreateInput{Platform: "fake", Credentials: map[string]string{"token": "same"}})
	if codeOf(err) != channel.CodeConnectFailed || !strings.Contains(err.Error(), "already connected") {
		t.Fatalf("err = %v", err)
	}
	if got, _ := other.Channels().Get(t.Context(), "x"); got != nil {
		t.Fatal("unknown id must be nil")
	}
}

func TestChannelUpdateReconnectsStopsAndRestarts(t *testing.T) {
	r := newChannelRig(t, 0)
	chs := r.a.Channels()
	ch, err := chs.Create(t.Context(), channel.CreateInput{Platform: "fake", Credentials: map[string]string{"token": "one"}})
	if err != nil {
		t.Fatal(err)
	}
	updated, err := chs.Update(t.Context(), ch.ID, channel.UpdateInput{Credentials: map[string]string{"token": "two"}})
	if err != nil || updated.Account == nil || updated.Account.ID != "bot-two" {
		t.Fatalf("updated = %+v %v", updated, err)
	}
	if n := len(r.platform.Adapters()); n != 2 || r.platform.Adapters()[0].Started() {
		t.Fatalf("adapters = %d, first started = %v", n, r.platform.Adapters()[0].Started())
	}
	off := false
	if _, err := chs.Update(t.Context(), ch.ID, channel.UpdateInput{Enabled: &off}); err != nil {
		t.Fatal(err)
	}
	if chs.State(ch.ID).Status != channel.StatusStopped || r.platform.AdapterFor(ch.ID) != nil {
		t.Fatalf("disabled channel still running: %+v", chs.State(ch.ID))
	}
	on := true
	if _, err := chs.Update(t.Context(), ch.ID, channel.UpdateInput{Enabled: &on}); err != nil {
		t.Fatal(err)
	}
	if chs.State(ch.ID).Status != channel.StatusRunning {
		t.Fatalf("re-enabled state = %+v", chs.State(ch.ID))
	}
	// A failed reconnect is saved and shows in State.
	saved, err := chs.Update(t.Context(), ch.ID, channel.UpdateInput{Credentials: map[string]string{"token": "bad"}})
	if err != nil || saved.Credentials["token"] != "bad" {
		t.Fatalf("saved = %+v %v", saved, err)
	}
	if s := chs.State(ch.ID); s.Status != channel.StatusFailed || s.Error != "invalid token" {
		t.Fatalf("state = %+v", s)
	}
}

func TestChannelReportsAdapterStatus(t *testing.T) {
	r := newChannelRig(t, 0)
	var mu sync.Mutex
	var statuses []string
	events.Subscribe(r.e.bus, events.ChannelStatusChanged, func(_ kernel.Event, p events.ChannelStatusChangedPayload) {
		mu.Lock()
		statuses = append(statuses, p.Status)
		mu.Unlock()
	})
	ch, adapter := r.open(t)
	mustOK(t, adapter.Report(channel.AdapterRetrying, "network down"))
	if s := r.a.Channels().State(ch.ID); s.Status != channel.StatusRetrying || s.Error != "network down" {
		t.Fatalf("state = %+v", s)
	}
	mustOK(t, adapter.Report(channel.AdapterFailed, "token revoked"))
	if s := r.a.Channels().State(ch.ID); s.Status != channel.StatusFailed {
		t.Fatalf("state = %+v", s)
	}
	mu.Lock()
	defer mu.Unlock()
	if !slices.Equal(statuses, []string{"connecting", "running", "retrying", "failed"}) {
		t.Fatalf("statuses = %v", statuses)
	}
	if _, err := r.a.Channels().Send(t.Context(), ch.ID, channel.Target{ChatID: "c"}, "x"); codeOf(err) != channel.CodeNotRunning {
		t.Fatalf("send on a failed channel: %v", err)
	}
}

func TestChannelMessageRunsATurnInTheChatSession(t *testing.T) {
	r := newChannelRig(t, 0)
	ch, adapter := r.open(t)
	mustOK(t, adapter.Receive(aecmock.FakeDirectMessage("u1", "hello")))

	waitTexts(t, adapter, "reply from primary")
	if len(adapter.Typing()) == 0 {
		t.Error("no typing action was sent")
	}
	sessions := channelSessions(t, r.a)
	if len(sessions) != 1 {
		t.Fatalf("%d sessions", len(sessions))
	}
	meta := sessions[0].Metadata
	for k, want := range map[string]any{"source": "channel", "channelId": ch.ID, "platform": "fake", "chatId": "chat-u1", "chatKey": "chat-u1", "senderId": "u1", "title": "User u1 " + "·" + " Fake"} {
		if got, _ := meta.Get(k); got != want {
			t.Errorf("metadata %s = %v, want %v", k, got, want)
		}
	}
	msgs, err := r.e.hub.Messages.Get(t.Context(), sessions[0].ID)
	if err != nil || len(msgs) != 2 || msgs[0].Content != "hello" {
		t.Fatalf("messages = %+v %v", msgs, err)
	}
}

func TestChannelChatKeepsItsSessionAndNewStartsAFreshOne(t *testing.T) {
	r := newChannelRig(t, 0)
	_, adapter := r.open(t)
	r.useRunner(func(_ context.Context, tc channel.TurnContext) (channel.TurnResult, error) {
		return channel.TurnResult{Output: tc.Text}, nil
	})
	mustOK(t, adapter.Receive(aecmock.FakeDirectMessage("u1", "one")))
	waitTexts(t, adapter, "one")
	mustOK(t, adapter.Receive(aecmock.FakeDirectMessage("u1", "two")))
	waitTexts(t, adapter, "one", "two")
	if n := len(channelSessions(t, r.a)); n != 1 {
		t.Fatalf("%d sessions after two messages of one chat", n)
	}
	mustOK(t, adapter.Receive(aecmock.FakeDirectMessage("u2", "hi")))
	waitTexts(t, adapter, "one", "two", "hi")
	if n := len(channelSessions(t, r.a)); n != 2 {
		t.Fatalf("%d sessions after a second chat", n)
	}
	mustOK(t, adapter.Receive(aecmock.FakeDirectMessage("u1", "/new")))
	waitTexts(t, adapter, "one", "two", "hi", "Started a new conversation.")
	if n := len(channelSessions(t, r.a)); n != 3 {
		t.Fatalf("%d sessions after /new", n)
	}
}

func TestChannelSplitsALongReply(t *testing.T) {
	r := newChannelRig(t, 20)
	_, adapter := r.open(t)
	long := strings.TrimSpace(strings.Repeat("word ", 10))
	r.useRunner(func(context.Context, channel.TurnContext) (channel.TurnResult, error) {
		return channel.TurnResult{Output: "echo: " + long}, nil
	})
	mustOK(t, adapter.Receive(aecmock.FakeDirectMessage("u1", "go")))
	waitFor(t, "a split reply", func() bool { return strings.Join(adapter.SentTexts(), " ") == "echo: "+long })
	for _, text := range adapter.SentTexts() {
		if len(text) > 20 {
			t.Errorf("part %q is longer than 20", text)
		}
	}
}

func TestChannelDropsDuplicatesBotsGroupsAndOtherBotsCommands(t *testing.T) {
	r := newChannelRig(t, 0)
	_, adapter := r.open(t)
	r.useRunner(func(_ context.Context, tc channel.TurnContext) (channel.TurnResult, error) {
		return channel.TurnResult{Output: "echo: " + tc.Text}, nil
	})
	dup := aecmock.FakeDirectMessage("u1", "once")
	mustOK(t, adapter.Receive(dup))
	mustOK(t, adapter.Receive(dup))
	bot := aecmock.FakeDirectMessage("bot", "beep")
	bot.Sender.IsBot = true
	mustOK(t, adapter.Receive(bot))
	group := aecmock.FakeDirectMessage("u1", "group hello")
	group.Chat = channel.Chat{ID: "g1", Type: channel.ChatGroup}
	mustOK(t, adapter.Receive(group))
	mustOK(t, adapter.Receive(aecmock.FakeDirectMessage("u1", "/status@other_bot")))
	mustOK(t, adapter.Receive(aecmock.FakeDirectMessage("u1", "last")))
	waitTexts(t, adapter, "echo: once", "echo: last")
}

func TestChannelCommands(t *testing.T) {
	r := newChannelRig(t, 0)
	_, adapter := r.open(t)
	mustOK(t, adapter.Receive(aecmock.FakeDirectMessage("u1", "")))
	waitTexts(t, adapter, "I can only read text messages for now.")
	mustOK(t, adapter.Receive(aecmock.FakeDirectMessage("u1", "/status")))
	waitTexts(t, adapter, "I can only read text messages for now.", "Agent: channel-agent\nNo conversation yet.")
	mustOK(t, adapter.Receive(aecmock.FakeDirectMessage("u1", "/whoami")))
	waitFor(t, "/whoami", func() bool { return slices.Contains(adapter.SentTexts(), "Your Fake id is u1.") })
	mustOK(t, adapter.Receive(aecmock.FakeDirectMessage("u1", "/help")))
	waitFor(t, "/help", func() bool {
		texts := adapter.SentTexts()
		return strings.HasPrefix(texts[len(texts)-1], "Hi! Send a message to talk to channel-agent.")
	})
}

// A-136: the agent's commands run in the chat - /resume goes back to an older
// conversation, privileged changes are for owners.
func TestChannelRunsTheAgentsCommands(t *testing.T) {
	r := newChannelRig(t, 0)
	_, adapter := r.open(t)
	say := func(text, want string) {
		t.Helper()
		mustOK(t, adapter.Receive(aecmock.FakeDirectMessage("u1", text)))
		waitFor(t, text, func() bool { return lastText(adapter) == want })
	}
	say("/context", "No conversation yet.")
	if n := len(channelSessions(t, r.a)); n != 0 {
		t.Fatalf("a read-only command made %d sessions", n)
	}

	say("first", "reply from primary")
	say("/rename Roof RFIs", "Renamed this conversation to \u201cRoof RFIs\u201d.")
	say("/new", "Started a new conversation.")
	mustOK(t, adapter.Receive(aecmock.FakeDirectMessage("u1", "/resume")))
	waitFor(t, "/resume", func() bool { return strings.Contains(lastText(adapter), "1. Roof RFIs") })
	say("/resume 1", "Resumed \u201cRoof RFIs\u201d.")
	mustOK(t, adapter.Receive(aecmock.FakeDirectMessage("u1", "second")))
	waitFor(t, "the second reply", func() bool { return len(adapter.SentTexts()) == 7 && lastText(adapter) == "reply from primary" })
	var roof kernel.Session
	for _, s := range channelSessions(t, r.a) {
		if title, _ := s.Metadata.Get("title"); title == "Roof RFIs" {
			roof = s
		}
	}
	msgs, err := r.e.hub.Messages.Get(t.Context(), roof.ID)
	mustOK(t, err)
	var contents []string
	for _, m := range msgs {
		contents = append(contents, strings.TrimSpace(m.Content))
	}
	if want := []string{"first", "reply from primary", "second", "reply from primary"}; !slices.Equal(contents, want) {
		t.Fatalf("Roof RFIs messages = %q, want %q", contents, want)
	}

	// An open DM: u1 is not on the allow list, so it can look but not widen permissions.
	say("/permissions yolo", "Only the bot's owners (the senders on its allow list) can use /permissions to change things.")
	mustOK(t, adapter.Receive(aecmock.FakeDirectMessage("u1", "/permissions")))
	waitFor(t, "/permissions", func() bool { return strings.Contains(lastText(adapter), "Permissions: ask") })
	// Unknown names still go to the agent as text (a skill, say).
	mustOK(t, adapter.Receive(aecmock.FakeDirectMessage("u1", "/web-research roofs")))
	waitFor(t, "/web-research", func() bool {
		msgs, err := r.e.hub.Messages.Get(t.Context(), roof.ID)
		return err == nil && len(msgs) == 6 && msgs[4].Content == "/web-research roofs"
	})
}

func TestChannelPublishesItsCommandsAndListsThemInHelp(t *testing.T) {
	r := newChannelRig(t, 0)
	_, adapter := r.open(t)
	var names []string
	for _, c := range adapter.Commands() {
		names = append(names, c.Name)
	}
	// A-136: then the agent's own commands (its command manager) that work in a chat.
	want := []string{
		"new", "stop", "status", "model", "approve", "deny", "yolo", "whoami", "sethome", "help",
		"compact", "resume", "rename", "context", "usage", "permissions", "plan", "init", "diff", "review", "learn",
	}
	if !slices.Equal(names, want) {
		t.Fatalf("commands = %v, want %v", names, want)
	}
	mustOK(t, adapter.Receive(aecmock.FakeDirectMessage("u1", "/help")))
	waitFor(t, "/help", func() bool { return len(adapter.SentTexts()) == 1 })
	help := adapter.SentTexts()[0]
	for _, name := range names {
		if name == "help" {
			continue
		}
		// A-136: the agent's commands are listed with their usage ("/compact [focus] - ...").
		if !regexp.MustCompile(`/` + name + `( \S+)* \x{2014} `).MatchString(help) {
			t.Fatalf("/help does not list /%s:\n%s", name, help)
		}
	}
}

func TestChannelUsesTheHostRunnerAndReportsFailures(t *testing.T) {
	r := newChannelRig(t, 0)
	_, adapter := r.open(t)
	var mu sync.Mutex
	var contexts []channel.TurnContext
	var failures []events.ChannelFailedPayload
	events.Subscribe(r.e.bus, events.ChannelFailed, func(_ kernel.Event, p events.ChannelFailedPayload) {
		mu.Lock()
		failures = append(failures, p)
		mu.Unlock()
	})
	r.useRunner(func(_ context.Context, tc channel.TurnContext) (channel.TurnResult, error) {
		mu.Lock()
		contexts = append(contexts, tc)
		mu.Unlock()
		if tc.Text == "boom" {
			return channel.TurnResult{}, errors.New("model unavailable")
		}
		return channel.TurnResult{Output: "host: " + tc.Text}, nil
	})
	mustOK(t, adapter.Receive(aecmock.FakeDirectMessage("u1", "hi")))
	waitTexts(t, adapter, "host: hi")
	mustOK(t, adapter.Receive(aecmock.FakeDirectMessage("u1", "boom")))
	waitTexts(t, adapter, "host: hi", "Sorry, something went wrong: model unavailable")

	mu.Lock()
	defer mu.Unlock()
	if len(contexts) != 2 || contexts[0].SessionID != contexts[1].SessionID || contexts[0].Message.Sender.ID != "u1" {
		t.Fatalf("contexts = %+v", contexts)
	}
	if len(failures) != 1 || failures[0].ChatID != "chat-u1" || failures[0].Error != "model unavailable" {
		t.Fatalf("failures = %+v", failures)
	}
}

func TestChannelStopAbortsTheTurnInProgress(t *testing.T) {
	r := newChannelRig(t, 0)
	_, adapter := r.open(t)
	started := make(chan struct{})
	var once sync.Once
	r.useRunner(func(ctx context.Context, _ channel.TurnContext) (channel.TurnResult, error) {
		once.Do(func() { close(started) })
		<-ctx.Done()
		return channel.TurnResult{Output: "too late", Aborted: true}, nil
	})
	mustOK(t, adapter.Receive(aecmock.FakeDirectMessage("u1", "long task")))
	<-started
	mustOK(t, adapter.Receive(aecmock.FakeDirectMessage("u1", "/stop")))
	waitTexts(t, adapter, "Stopped.")
	mustOK(t, adapter.Receive(aecmock.FakeDirectMessage("u1", "/stop")))
	waitTexts(t, adapter, "Stopped.", "Nothing is running.")
}

func TestChannelTurnsOfOneChatRunInOrder(t *testing.T) {
	r := newChannelRig(t, 0)
	_, adapter := r.open(t)
	var active, maxActive atomic.Int32
	r.useRunner(func(_ context.Context, tc channel.TurnContext) (channel.TurnResult, error) {
		n := active.Add(1)
		for {
			m := maxActive.Load()
			if n <= m || maxActive.CompareAndSwap(m, n) {
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
		active.Add(-1)
		return channel.TurnResult{Output: tc.Text}, nil
	})
	for _, text := range []string{"a", "b", "c"} {
		mustOK(t, adapter.Receive(aecmock.FakeDirectMessage("u1", text)))
	}
	waitTexts(t, adapter, "a", "b", "c")
	if maxActive.Load() != 1 {
		t.Fatalf("%d turns of one chat ran at once", maxActive.Load())
	}
}

func TestChannelPairingFlow(t *testing.T) {
	r := newChannelRig(t, 0)
	var pairingEvents atomic.Int32
	events.Subscribe(r.e.bus, events.ChannelPairingRequested, func(kernel.Event, events.ChannelPairingRequestedPayload) { pairingEvents.Add(1) })
	chs := r.a.Channels()
	ch, err := chs.Create(t.Context(), channel.CreateInput{Platform: "fake", Credentials: map[string]string{"token": "t"}})
	if err != nil {
		t.Fatal(err)
	}
	adapter := r.platform.AdapterFor(ch.ID)
	mustOK(t, adapter.Receive(aecmock.FakeDirectMessage("u1", "hello?")))
	waitFor(t, "a pairing reply", func() bool { return len(adapter.SentTexts()) == 1 })
	mustOK(t, adapter.Receive(aecmock.FakeDirectMessage("u1", "still there?")))
	waitFor(t, "a second pairing reply", func() bool { return len(adapter.SentTexts()) == 2 })

	pairings, err := chs.ListPairings(t.Context(), ch.ID)
	if err != nil || len(pairings) != 1 {
		t.Fatalf("pairings = %+v %v", pairings, err)
	}
	p := pairings[0]
	if p.SenderID != "u1" || p.SenderName != "User u1" || p.ChatID != "chat-u1" || !regexp.MustCompile(`^[A-HJ-NP-Z2-9]{8}$`).MatchString(p.Code) {
		t.Fatalf("pairing = %+v", p)
	}
	for _, text := range adapter.SentTexts() {
		if !strings.Contains(text, p.Code) {
			t.Errorf("reply %q does not repeat the code", text)
		}
	}
	if pairingEvents.Load() != 1 || len(channelSessions(t, r.a)) != 0 {
		t.Fatalf("events = %d, sessions = %d", pairingEvents.Load(), len(channelSessions(t, r.a)))
	}

	approved, err := chs.ApprovePairing(t.Context(), ch.ID, strings.ToLower(p.Code))
	if err != nil || !slices.Equal(approved.Access.AllowFrom, []string{"u1"}) {
		t.Fatalf("approved = %+v %v", approved, err)
	}
	texts := adapter.SentTexts()
	if !strings.Contains(texts[len(texts)-1], "You're approved") {
		t.Fatalf("no approval notice: %q", texts)
	}
	if left, _ := chs.ListPairings(t.Context(), ch.ID); len(left) != 0 {
		t.Fatalf("pairing still pending: %+v", left)
	}
	mustOK(t, adapter.Receive(aecmock.FakeDirectMessage("u1", "now?")))
	waitFor(t, "a reply after approval", func() bool {
		texts := adapter.SentTexts()
		return texts[len(texts)-1] == "reply from primary"
	})
}

func TestChannelPairingLimitAndUnknownCodes(t *testing.T) {
	r := newChannelRig(t, 0)
	chs := r.a.Channels()
	ch, err := chs.Create(t.Context(), channel.CreateInput{Platform: "fake", Credentials: map[string]string{"token": "t"}})
	if err != nil {
		t.Fatal(err)
	}
	adapter := r.platform.AdapterFor(ch.ID)
	for i, id := range []string{"a", "b", "c", "d"} {
		mustOK(t, adapter.Receive(aecmock.FakeDirectMessage(id, "hi")))
		waitFor(t, fmt.Sprintf("reply %d", i+1), func() bool { return len(adapter.SentTexts()) == i+1 })
	}
	if texts := adapter.SentTexts(); !strings.Contains(texts[3], "too many people are waiting") {
		t.Fatalf("fourth reply = %q", texts[3])
	}
	pairings, _ := chs.ListPairings(t.Context(), ch.ID)
	var senders []string
	for _, p := range pairings {
		senders = append(senders, p.SenderID)
	}
	if !slices.Equal(senders, []string{"a", "b", "c"}) {
		t.Fatalf("senders = %v", senders)
	}
	if err := chs.RejectPairing(t.Context(), ch.ID, pairings[0].Code); err != nil {
		t.Fatal(err)
	}
	if err := chs.RejectPairing(t.Context(), ch.ID, pairings[0].Code); codeOf(err) != channel.CodeNotFound {
		t.Fatalf("second reject: %v", err)
	}
	if _, err := chs.ApprovePairing(t.Context(), ch.ID, "NOPE1234"); codeOf(err) != channel.CodeNotFound {
		t.Fatalf("unknown code: %v", err)
	}
}

func TestChannelAllowlistAndLiveAccessChanges(t *testing.T) {
	r := newChannelRig(t, 0)
	chs := r.a.Channels()
	allowlist := channel.DmAllowlist
	ch, err := chs.Create(t.Context(), channel.CreateInput{
		Platform: "fake", Credentials: map[string]string{"token": "t"},
		Access: &channel.AccessInput{DmPolicy: &allowlist, AllowFrom: []string{"boss"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	adapter := r.platform.AdapterFor(ch.ID)
	mustOK(t, adapter.Receive(aecmock.FakeDirectMessage("guest", "hi")))
	waitTexts(t, adapter, "This bot is private. Ask its owner to add your Fake id: guest")
	if _, err := chs.Update(t.Context(), ch.ID, channel.UpdateInput{Access: &channel.AccessInput{AllowFrom: []string{"boss", "guest"}}}); err != nil {
		t.Fatal(err)
	}
	mustOK(t, adapter.Receive(aecmock.FakeDirectMessage("guest", "and now")))
	waitTexts(t, adapter, "This bot is private. Ask its owner to add your Fake id: guest", "reply from primary")
	if n := len(r.platform.Adapters()); n != 1 {
		t.Fatalf("an access change reconnected: %d adapters", n)
	}
}

func TestChannelDeleteAndDispose(t *testing.T) {
	r := newChannelRig(t, 0)
	chs := r.a.Channels()
	a, err := chs.Create(t.Context(), channel.CreateInput{Platform: "fake", Credentials: map[string]string{"token": "a"}})
	if err != nil {
		t.Fatal(err)
	}
	b, adapterB := r.open(t)
	if err := chs.Delete(t.Context(), a.ID); err != nil {
		t.Fatal(err)
	}
	if r.platform.AdapterFor(a.ID) != nil {
		t.Fatal("deleted channel is still connected")
	}
	if err := chs.Delete(t.Context(), a.ID); codeOf(err) != channel.CodeNotFound {
		t.Fatalf("second delete: %v", err)
	}

	// Dispose cancels a running turn and disconnects.
	started := make(chan struct{})
	var once sync.Once
	r.useRunner(func(ctx context.Context, _ channel.TurnContext) (channel.TurnResult, error) {
		once.Do(func() { close(started) })
		<-ctx.Done()
		return channel.TurnResult{Aborted: true}, nil
	})
	mustOK(t, adapterB.Receive(aecmock.FakeDirectMessage("u1", "work")))
	<-started
	if err := r.a.Dispose(t.Context()); err != nil {
		t.Fatal(err)
	}
	if adapterB.Started() || chs.State(b.ID).Status != channel.StatusStopped {
		t.Fatalf("after Dispose: started %v, state %+v", adapterB.Started(), chs.State(b.ID))
	}
	if err := r.a.StartChannels(t.Context()); err != nil {
		t.Fatal(err)
	}
	if r.platform.AdapterFor(b.ID) != nil {
		t.Fatal("StartChannels after Dispose connected a channel")
	}
}

func TestChannelStartReconnectsStoredChannels(t *testing.T) {
	r := newChannelRig(t, 0)
	on, err := r.a.Channels().Create(t.Context(), channel.CreateInput{Platform: "fake", Credentials: map[string]string{"token": "on"}})
	if err != nil {
		t.Fatal(err)
	}
	disabled := false
	off, err := r.a.Channels().Create(t.Context(), channel.CreateInput{Platform: "fake", Name: "Later", Credentials: map[string]string{"token": "off"}, Enabled: &disabled})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.a.Dispose(t.Context()); err != nil {
		t.Fatal(err)
	}

	again := r.spawn(t, "channel-agent")
	if err := again.StartChannels(t.Context()); err != nil {
		t.Fatal(err)
	}
	if again.Channels().State(on.ID).Status != channel.StatusRunning || again.Channels().State(off.ID).Status != channel.StatusStopped {
		t.Fatalf("states: on %+v, off %+v", again.Channels().State(on.ID), again.Channels().State(off.ID))
	}
	// The chat continues in its stored session after the restart.
	adapter := r.platform.AdapterFor(on.ID)
	mustOK(t, adapter.Receive(aecmock.FakeDirectMessage("u9", "/status")))
	waitFor(t, "a status reply", func() bool { return len(adapter.SentTexts()) == 1 })
}
