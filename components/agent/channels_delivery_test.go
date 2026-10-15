package agent_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/Phong95/aec-agent-go/aecmock"
	"github.com/Phong95/aec-agent-go/components/agent"
	"github.com/Phong95/aec-agent-go/components/agent/job"
	"github.com/Phong95/aec-agent-go/components/channelplatformhub"
	"github.com/Phong95/aec-agent-go/components/channelplatformhub/channel"
	"github.com/Phong95/aec-agent-go/components/events"
	"github.com/Phong95/aec-agent-go/kernel"
	"github.com/Phong95/aec-agent-go/kernel/ordered"
)

// A-100 phase 3: scheduled-job delivery and /sethome. A prompt job's result is sent to a
// chat of one of the agent's channels (its home chat, or a named chat).

// deliveryRig is an agent with the fake channel platform and a swappable job runner.
type deliveryRig struct {
	e        *env
	hub      *channelplatformhub.ChannelPlatformHub
	platform *aecmock.FakeChannelPlatform
	a        *agent.Agent
	runner   atomic.Pointer[job.Runner]
}

func newDeliveryRig(t *testing.T, caps channel.Capabilities) *deliveryRig {
	t.Helper()
	r := &deliveryRig{e: newEnv(t), hub: channelplatformhub.New(), platform: aecmock.NewFakeChannelPlatformWithCapabilities("fake", caps)}
	mustOK(t, r.hub.Register(r.platform))
	r.useRunner(instantRunner)
	r.a = r.spawn(t, "delivery-agent")
	return r
}

func (r *deliveryRig) spawn(t *testing.T, id string) *agent.Agent {
	t.Helper()
	deps := r.e.deps()
	deps.ChannelPlatformHub = r.hub
	deps.JobRunner = func() job.Runner { return *r.runner.Load() }
	a, err := agent.New(t.Context(), deps, testProfile(id))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.Dispose(context.WithoutCancel(t.Context())) })
	// Update (A-126): runs go through the job worker, which StartJobs starts.
	mustOK(t, a.StartJobs(t.Context()))
	return a
}

func (r *deliveryRig) useRunner(run job.Runner) { r.runner.Store(&run) }

// open creates a channel whose owner is "owner", open to any group.
func (r *deliveryRig) open(t *testing.T) (channel.Channel, *aecmock.FakeChannelAdapter) {
	t.Helper()
	allowlist, groups := channel.DmAllowlist, channel.GroupOpen
	ch, err := r.a.Channels().Create(t.Context(), channel.CreateInput{
		Platform: "fake", Credentials: map[string]string{"token": "t"},
		Access: &channel.AccessInput{DmPolicy: &allowlist, AllowFrom: []string{"owner"}, GroupPolicy: &groups},
	})
	mustOK(t, err)
	return ch, r.platform.AdapterFor(ch.ID)
}

// collect records the events of kind k.
func collect[P any](t *testing.T, bus events.Bus, k events.Kind[P]) func() []P {
	t.Helper()
	var mu sync.Mutex
	var got []P
	off := events.Subscribe(bus, k, func(_ kernel.Event, p P) {
		mu.Lock()
		got = append(got, p)
		mu.Unlock()
	})
	t.Cleanup(off)
	return func() []P {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(got)
	}
}

var everyDay = job.When{Kind: job.KindCron, Expr: "0 8 * * *"}

func TestSetHomeByAnOwner(t *testing.T) {
	r := newDeliveryRig(t, channel.Capabilities{})
	ch, adapter := r.open(t)
	mustOK(t, adapter.Receive(aecmock.FakeGroupMessage("g1", "owner", "/sethome")))
	waitFor(t, "/sethome", func() bool { return strings.Contains(lastText(adapter), "is now the home chat") })
	stored, err := r.a.Channels().Get(t.Context(), ch.ID)
	mustOK(t, err)
	if h := stored.Home; h == nil || h.ChatID != "g1" || h.Title != "Group g1" || h.SetBy != "owner" || h.SetAt == "" {
		t.Fatalf("home = %+v", stored.Home)
	}
	mustOK(t, adapter.Receive(aecmock.FakeGroupMessage("g1", "owner", "/status")))
	waitFor(t, "/status", func() bool { return strings.Contains(lastText(adapter), "This is the home chat") })
}

func TestSetHomeRefusedForOthers(t *testing.T) {
	r := newDeliveryRig(t, channel.Capabilities{})
	ch, adapter := r.open(t)
	mustOK(t, adapter.Receive(aecmock.FakeGroupMessage("g1", "member", "/sethome")))
	waitFor(t, "refusal", func() bool { return strings.Contains(lastText(adapter), "Only the bot's owners") })
	stored, err := r.a.Channels().Get(t.Context(), ch.ID)
	mustOK(t, err)
	if stored.Home != nil {
		t.Fatalf("home = %+v, want none", stored.Home)
	}
}

func TestHomeSetAndClearedThroughUpdate(t *testing.T) {
	r := newDeliveryRig(t, channel.Capabilities{})
	ch, _ := r.open(t)
	set, err := r.a.Channels().Update(t.Context(), ch.ID, channel.UpdateInput{Home: &channel.HomeInput{ChatID: "c9", ThreadID: "t1"}})
	mustOK(t, err)
	if set.Home == nil || set.Home.ChatID != "c9" || set.Home.ThreadID != "t1" || set.Home.SetAt == "" {
		t.Fatalf("home = %+v", set.Home)
	}
	cleared, err := r.a.Channels().Update(t.Context(), ch.ID, channel.UpdateInput{ClearHome: true})
	mustOK(t, err)
	if cleared.Home != nil {
		t.Fatalf("home = %+v, want none", cleared.Home)
	}
	if _, err := r.a.Channels().Update(t.Context(), ch.ID, channel.UpdateInput{Home: &channel.HomeInput{ChatID: " "}}); codeOf(err) != channel.CodeInvalidArgument {
		t.Fatalf("err = %v, want invalid_argument", err)
	}
}

func TestDeliveryToTheHomeChat(t *testing.T) {
	r := newDeliveryRig(t, channel.Capabilities{})
	ch, adapter := r.open(t)
	sent := collect(t, r.e.bus, events.ChannelMessageSent)
	mustOK(t, adapter.Receive(aecmock.FakeDirectMessage("owner", "/sethome")))
	waitFor(t, "/sethome", func() bool { return len(adapter.SentTexts()) == 1 })

	jobs := r.a.Jobs()
	j, err := jobs.Create(t.Context(), job.CreateInput{
		Name: "digest", Prompt: "summarise", When: everyDay,
		Deliveries: job.Deliveries{{ChannelID: ch.ID, Target: job.DeliveryHome}},
	})
	mustOK(t, err)
	if len(j.Deliveries) == 0 || j.Deliveries[0].When != job.DeliverAlways {
		t.Fatalf("delivery = %+v, want when=always by default", j.Deliveries)
	}
	_, err = jobs.RunNow(t.Context(), j.ID)
	mustOK(t, err)

	waitFor(t, "the delivery", func() bool { return len(adapter.Sent()) == 2 })
	if got := adapter.Sent()[1]; got.ChatID != "chat-owner" || got.Text != "digest\n\ndid digest" {
		t.Fatalf("sent %+v", got)
	}
	waitFor(t, "channel:message-sent", func() bool {
		for _, p := range sent() {
			if p.JobID == j.ID && p.ChatID == "chat-owner" && p.Parts == 1 {
				return true
			}
		}
		return false
	})
}

func TestDeliveryAsMarkdownToANamedChat(t *testing.T) {
	r := newDeliveryRig(t, channel.Capabilities{Markdown: true})
	ch, adapter := r.open(t)
	j, err := r.a.Jobs().Create(t.Context(), job.CreateInput{
		Name: "digest", Prompt: "go", When: everyDay,
		Deliveries: job.Deliveries{{ChannelID: ch.ID, Target: "c5", ThreadID: "t2", When: job.DeliverOnOutput}},
	})
	mustOK(t, err)
	_, err = r.a.Jobs().RunNow(t.Context(), j.ID)
	mustOK(t, err)
	waitFor(t, "the delivery", func() bool { return len(adapter.Sent()) == 1 })
	got := adapter.Sent()[0]
	if got.ChatID != "c5" || got.Target.ThreadID != "t2" || got.Text != "**digest**\n\ndid digest" || got.Format != string(channel.FormatMarkdown) {
		t.Fatalf("sent %+v", got)
	}
}

func TestDeliveryFollowsWhen(t *testing.T) {
	r := newDeliveryRig(t, channel.Capabilities{})
	ch, adapter := r.open(t)
	completed := collect(t, r.e.bus, events.JobRunFinished)
	jobs := r.a.Jobs()
	j, err := jobs.Create(t.Context(), job.CreateInput{
		Name: "check", Prompt: "check", When: everyDay,
		Deliveries: job.Deliveries{{ChannelID: ch.ID, Target: "c1", When: job.DeliverOnFailure}},
	})
	mustOK(t, err)
	_, err = jobs.RunNow(t.Context(), j.ID)
	mustOK(t, err)
	waitFor(t, "the run", func() bool { return len(completed()) == 1 })
	waitRun(t, jobs, j.ID, func(run job.Run) bool { return run.Status == job.RunSucceeded })

	r.useRunner(func(context.Context, job.RunContext) (job.RunResult, error) {
		return job.RunResult{}, errors.New("model unavailable")
	})
	_, err = jobs.RunNow(t.Context(), j.ID)
	mustOK(t, err)
	waitFor(t, "the failure", func() bool { return len(adapter.Sent()) == 1 })
	if got := adapter.Sent()[0]; got.ChatID != "c1" || got.Text != `Job "check" failed: model unavailable` {
		t.Fatalf("sent %+v", got)
	}
}

func TestDeliveryOfAnEmptyReplyOnlyWhenAlways(t *testing.T) {
	r := newDeliveryRig(t, channel.Capabilities{})
	ch, adapter := r.open(t)
	r.useRunner(func(context.Context, job.RunContext) (job.RunResult, error) {
		return job.RunResult{}, nil
	})
	jobs := r.a.Jobs()
	always, err := jobs.Create(t.Context(), job.CreateInput{
		Name: "a", Prompt: "p", When: everyDay,
		Deliveries: job.Deliveries{{ChannelID: ch.ID, Target: "c1", When: job.DeliverAlways}},
	})
	mustOK(t, err)
	onOutput, err := jobs.Create(t.Context(), job.CreateInput{
		Name: "b", Prompt: "p", When: everyDay,
		Deliveries: job.Deliveries{{ChannelID: ch.ID, Target: "c2", When: job.DeliverOnOutput}},
	})
	mustOK(t, err)
	_, err = jobs.RunNow(t.Context(), onOutput.ID)
	mustOK(t, err)
	waitRun(t, jobs, onOutput.ID, func(run job.Run) bool { return run.Status == job.RunSucceeded })
	_, err = jobs.RunNow(t.Context(), always.ID)
	mustOK(t, err)
	waitFor(t, "the delivery", func() bool { return len(adapter.Sent()) == 1 })
	if got := adapter.Sent()[0]; got.ChatID != "c1" || got.Text != `Job "a" finished without a reply.` {
		t.Fatalf("sent %+v", got)
	}
}

func TestDeliveryWithoutAHomeChatFails(t *testing.T) {
	r := newDeliveryRig(t, channel.Capabilities{})
	ch, adapter := r.open(t)
	failed := collect(t, r.e.bus, events.ChannelFailed)
	j, err := r.a.Jobs().Create(t.Context(), job.CreateInput{
		Name: "digest", Prompt: "p", When: everyDay,
		Deliveries: job.Deliveries{{ChannelID: ch.ID, Target: job.DeliveryHome}},
	})
	mustOK(t, err)
	_, err = r.a.Jobs().RunNow(t.Context(), j.ID)
	mustOK(t, err)
	waitFor(t, "channel:error", func() bool { return len(failed()) == 1 })
	if p := failed()[0]; p.ChannelID != ch.ID || p.JobID != j.ID || !strings.Contains(p.Error, "/sethome") {
		t.Fatalf("channel:error %+v", p)
	}
	if n := len(adapter.Sent()); n != 0 {
		t.Fatalf("sent %d messages, want 0", n)
	}
}

func TestDeliveryNeverReachesAnotherAgentsChannel(t *testing.T) {
	r := newDeliveryRig(t, channel.Capabilities{})
	ch, adapter := r.open(t)
	other := r.spawn(t, "other-agent")
	failed := collect(t, r.e.bus, events.ChannelFailed)
	j, err := other.Jobs().Create(t.Context(), job.CreateInput{
		Name: "x", Prompt: "p", When: everyDay,
		Deliveries: job.Deliveries{{ChannelID: ch.ID, Target: "c1"}},
	})
	mustOK(t, err)
	_, err = other.Jobs().RunNow(t.Context(), j.ID)
	mustOK(t, err)
	waitFor(t, "channel:error", func() bool { return len(failed()) == 1 })
	if p := failed()[0]; !strings.Contains(p.Error, "no such channel") {
		t.Fatalf("channel:error %+v", p)
	}
	if n := len(adapter.Sent()); n != 0 {
		t.Fatalf("sent %d messages, want 0", n)
	}
}

func TestDeliveryValidationAndUpdate(t *testing.T) {
	r := newDeliveryRig(t, channel.Capabilities{})
	ch, _ := r.open(t)
	jobs := r.a.Jobs()
	bad := []job.Delivery{
		{ChannelID: ch.ID, Target: "home", When: "sometimes"},
		{ChannelID: "", Target: "home"},
		{ChannelID: ch.ID, Target: " "},
	}
	for _, d := range bad {
		_, err := jobs.Create(t.Context(), job.CreateInput{Name: "x", Prompt: "p", When: everyDay, Deliveries: job.Deliveries{d}})
		var pe *job.Error
		if !errors.As(err, &pe) || pe.Code != job.CodeInvalidArgument {
			t.Fatalf("delivery %+v: err = %v, want invalid_argument", d, err)
		}
	}
	j, err := jobs.Create(t.Context(), job.CreateInput{Name: "x", Prompt: "p", When: everyDay})
	mustOK(t, err)
	changed, err := jobs.Update(t.Context(), j.ID, job.UpdateInput{Deliveries: job.Deliveries{{ChannelID: ch.ID, Target: "c1", When: job.DeliverOnOutput}}})
	mustOK(t, err)
	if d := changed.Deliveries; len(d) != 1 || d[0] != (job.Delivery{ChannelID: ch.ID, Target: "c1", When: job.DeliverOnOutput}) {
		t.Fatalf("delivery = %+v", changed.Deliveries)
	}
	removed, err := jobs.Update(t.Context(), j.ID, job.UpdateInput{ClearDeliveries: true})
	mustOK(t, err)
	if len(removed.Deliveries) > 0 {
		t.Fatalf("delivery = %+v, want none", removed.Deliveries)
	}
}

// chatSessions lists the agent's sessions of one chat of a channel.
func chatSessions(t *testing.T, a *agent.Agent, channelID, chatKey string) []kernel.Session {
	t.Helper()
	f := ordered.NewObject()
	f.Set("source", "channel")
	f.Set("channelId", channelID)
	f.Set("chatKey", chatKey)
	s, err := a.ListSessions(t.Context(), kernel.SessionFilter{Metadata: f})
	mustOK(t, err)
	return s
}

func TestDeliveredResultJoinsTheChatsConversation(t *testing.T) {
	r := newDeliveryRig(t, channel.Capabilities{})
	ch, adapter := r.open(t)
	mustOK(t, adapter.Receive(aecmock.FakeDirectMessage("owner", "hello")))
	waitFor(t, "the reply", func() bool { return len(adapter.Sent()) == 1 })
	sessions := chatSessions(t, r.a, ch.ID, "chat-owner")
	if len(sessions) != 1 {
		t.Fatalf("chat sessions = %d", len(sessions))
	}
	chatSession := sessions[0].ID

	j, err := r.a.Jobs().Create(t.Context(), job.CreateInput{
		Name: "Stand up", Prompt: "remind", When: everyDay,
		Deliveries: job.Deliveries{{ChannelID: ch.ID, Target: "chat-owner"}},
	})
	mustOK(t, err)
	run, err := r.a.Jobs().RunNow(t.Context(), j.ID)
	mustOK(t, err)
	if run.SessionID == chatSession {
		t.Fatal("the job ran in the chat's session")
	}
	const posted = "**Stand up**\n\ndid Stand up"
	waitFor(t, "the result in the chat's conversation", func() bool {
		msgs, err := r.e.hub.Messages.Get(t.Context(), chatSession)
		mustOK(t, err)
		return len(msgs) > 0 && msgs[len(msgs)-1].Role == kernel.RoleAssistant && msgs[len(msgs)-1].Content == posted
	})

	// The chat's next message continues that session, after the posted result.
	mustOK(t, adapter.Receive(aecmock.FakeDirectMessage("owner", "tell me more")))
	waitFor(t, "the second reply", func() bool { return len(adapter.Sent()) == 3 })
	msgs, err := r.e.hub.Messages.Get(t.Context(), chatSession)
	mustOK(t, err)
	var order []string
	for _, m := range msgs {
		if m.Content == posted || m.Content == "tell me more" {
			order = append(order, m.Content)
		}
	}
	if !slices.Equal(order, []string{posted, "tell me more"}) {
		t.Fatalf("conversation order = %q", order)
	}
}

func TestDeliveredResultStartsTheChatsSession(t *testing.T) {
	r := newDeliveryRig(t, channel.Capabilities{})
	ch, adapter := r.open(t)
	_, err := r.a.Channels().Update(t.Context(), ch.ID, channel.UpdateInput{Home: &channel.HomeInput{ChatID: "g7", Title: "Site team"}})
	mustOK(t, err)
	j, err := r.a.Jobs().Create(t.Context(), job.CreateInput{
		Name: "digest", Prompt: "summarise", When: everyDay,
		Deliveries: job.Deliveries{{ChannelID: ch.ID, Target: job.DeliveryHome}},
	})
	mustOK(t, err)
	_, err = r.a.Jobs().RunNow(t.Context(), j.ID)
	mustOK(t, err)
	waitFor(t, "the delivery", func() bool { return len(adapter.Sent()) == 1 })

	var s []kernel.Session
	waitFor(t, "the chat's session", func() bool { s = chatSessions(t, r.a, ch.ID, "g7"); return len(s) == 1 })
	if title, _ := s[0].Metadata.Get("title"); title != "Site team "+string(rune(0xb7))+" Fake" {
		t.Fatalf("title = %v", title)
	}
	waitFor(t, "the result in it", func() bool {
		msgs, err := r.e.hub.Messages.Get(t.Context(), s[0].ID)
		mustOK(t, err)
		return len(msgs) == 1 && msgs[0].Content == "digest\n\ndid digest" == false && msgs[0].Content == "**digest**\n\ndid digest"
	})
}

// A-127: a job with several deliveries posts its result to each chat.
func TestDeliveryToSeveralChats(t *testing.T) {
	r := newDeliveryRig(t, channel.Capabilities{})
	ch, adapter := r.open(t)
	r.useRunner(func(context.Context, job.RunContext) (job.RunResult, error) {
		return job.RunResult{}, nil
	})
	jobs := r.a.Jobs()
	j, err := jobs.Create(t.Context(), job.CreateInput{
		Name: "a", Prompt: "p", When: everyDay,
		Deliveries: job.Deliveries{
			{ChannelID: ch.ID, Target: "c1", When: job.DeliverAlways},
			{ChannelID: ch.ID, Target: "c2", When: job.DeliverAlways},
			{ChannelID: ch.ID, Target: "c3", When: job.DeliverOnOutput},
			{ChannelID: ch.ID, Target: "c1"},
		},
	})
	mustOK(t, err)
	if len(j.Deliveries) != 3 {
		t.Fatalf("deliveries = %+v, want the repeated chat once", j.Deliveries)
	}
	_, err = jobs.RunNow(t.Context(), j.ID)
	mustOK(t, err)
	waitFor(t, "the deliveries", func() bool { return len(adapter.Sent()) == 2 })
	chats := []string{adapter.Sent()[0].ChatID, adapter.Sent()[1].ChatID}
	slices.Sort(chats)
	if !slices.Equal(chats, []string{"c1", "c2"}) {
		t.Fatalf("sent to %v", chats)
	}
}
