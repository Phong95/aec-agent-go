package agent_test

import (
	"strings"
	"testing"

	"github.com/Phong95/aec-agent-go/aecmock"
	"github.com/Phong95/aec-agent-go/components/agent/job"
	"github.com/Phong95/aec-agent-go/components/channelplatformhub/channel"
	"github.com/Phong95/aec-agent-go/components/toolhub"
	"github.com/Phong95/aec-agent-go/components/toolhub/builtin/jobs"
)

// A-100 phase 3: a job the agent schedules from a chat (schedule_job; Update (A-126): manage_job) delivers back to
// that chat.

const standUpArgs = `{"action":"create","name":"Stand up","prompt":"Remind the user to stand up.","every_minutes":1}`

func newSchedulingRig(t *testing.T, host toolhub.InteractionHandler) *interactiveRig {
	t.Helper()
	return newInteractiveRig(t, interactiveOptions{
		provider: &scriptedProvider{toolName: "manage_job", toolArgs: standUpArgs, reply: func(_, res string) string { return "done: " + res }},
		tools:    jobs.NewTools(),
		access:   &channel.AccessInput{AllowFrom: []string{"owner"}},
		host:     host,
	})
}

// scheduleFromChat asks for the job in msg's chat and approves it there.
func scheduleFromChat(t *testing.T, r *interactiveRig, msg channel.InboundMessage) job.Job {
	t.Helper()
	mustOK(t, r.adapter.Receive(msg))
	waitFor(t, "the approval prompt", func() bool { _, ok := findSent(r.adapter, hasButtons); return ok })
	mustOK(t, r.adapter.Press(string(rune(0x2705))+" Approve", channel.Sender{ID: "owner", Name: "Owner"}, channel.ChatDM))
	waitFor(t, "the reply", func() bool { return strings.Contains(lastText(r.adapter), "will be posted in ") })
	jobs, err := r.a.Jobs().List(t.Context())
	mustOK(t, err)
	if len(jobs) != 1 {
		t.Fatalf("jobs = %d", len(jobs))
	}
	return jobs[0]
}

func TestScheduleJobInTheHomeChatDeliversHome(t *testing.T) {
	r := newSchedulingRig(t, nil)
	mustOK(t, r.adapter.Receive(aecmock.FakeDirectMessage("owner", "/sethome")))
	waitFor(t, "the /sethome reply", func() bool { return strings.Contains(lastText(r.adapter), "is now the home chat") })

	j := scheduleFromChat(t, r, aecmock.FakeDirectMessage("owner", "remind me to stand up every minute"))
	want := job.Delivery{ChannelID: r.ch.ID, Target: job.DeliveryHome, When: job.DeliverAlways}
	if len(j.Deliveries) == 0 || j.Deliveries[0] != want {
		t.Fatalf("delivery = %+v, want %+v", j.Deliveries, want)
	}
}

func TestScheduleJobInAnotherChatDeliversThere(t *testing.T) {
	r := newSchedulingRig(t, nil)
	home := aecmock.FakeDirectMessage("owner", "/sethome")
	home.Chat.ID = "chat-home"
	mustOK(t, r.adapter.Receive(home))
	waitFor(t, "the /sethome reply", func() bool { return strings.Contains(lastText(r.adapter), "is now the home chat") })

	j := scheduleFromChat(t, r, aecmock.FakeDirectMessage("owner", "remind me to stand up every minute"))
	want := job.Delivery{ChannelID: r.ch.ID, Target: "chat-owner", When: job.DeliverAlways}
	if len(j.Deliveries) == 0 || j.Deliveries[0] != want {
		t.Fatalf("delivery = %+v, want %+v", j.Deliveries, want)
	}
}

func TestScheduleJobInAWebChatKeepsResultsInTheConversation(t *testing.T) {
	r := newSchedulingRig(t, &countingInteraction{})
	_, err := r.a.StartSession(t.Context())
	mustOK(t, err)
	res, err := r.a.Chat(t.Context(), "remind me to stand up every minute")
	mustOK(t, err)
	if strings.Contains(res.Message.Content, "will be posted in this chat") {
		t.Fatalf("reply = %q", res.Message.Content)
	}
	jobs, err := r.a.Jobs().List(t.Context())
	mustOK(t, err)
	if len(jobs) != 1 || len(jobs[0].Deliveries) > 0 {
		t.Fatalf("jobs = %+v", jobs)
	}
}

func TestScheduleJobInAWebChatDeliversToTheHomeChat(t *testing.T) {
	r := newSchedulingRig(t, &countingInteraction{})
	mustOK(t, r.adapter.Receive(aecmock.FakeDirectMessage("owner", "/sethome")))
	waitFor(t, "the /sethome reply", func() bool { return strings.Contains(lastText(r.adapter), "is now the home chat") })
	_, err := r.a.StartSession(t.Context())
	mustOK(t, err)
	res, err := r.a.Chat(t.Context(), "remind me to stand up every minute")
	mustOK(t, err)
	if !strings.Contains(res.Message.Content, "home chat") {
		t.Fatalf("reply = %q", res.Message.Content)
	}
	jobs, err := r.a.Jobs().List(t.Context())
	mustOK(t, err)
	want := job.Delivery{ChannelID: r.ch.ID, Target: job.DeliveryHome, When: job.DeliverAlways}
	if len(jobs) != 1 || len(jobs[0].Deliveries) == 0 || jobs[0].Deliveries[0] != want {
		t.Fatalf("jobs = %+v", jobs)
	}
}
