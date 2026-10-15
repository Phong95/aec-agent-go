package agent_test

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/Phong95/aec-agent-go/components/agent/job"
	"github.com/Phong95/aec-agent-go/components/skillhub"
	"github.com/Phong95/aec-agent-go/components/skillhub/skill"
	"github.com/Phong95/aec-agent-go/components/toolhub/builtin/jobs"
)

// A-70 - the watch-and-react essential skill: a watcher scheduled job marks a match with
// [[FOUND]] in its reply, and a reaction trigger on that job's completed runs fires only
// when the mark is there.
// Update (A-126): the watch-and-notify skill - one schedule job that replies with what it
// found, [[SILENT]] when there is nothing new, or [[FAILED]] when it could not check.
// These tests run the exact setup the skill tells the agent to make (the
// watch-and-notify.test.ts of core/ts).

func TestWatchAndNotifyIsAnEssentialSkillThatNeedsManageJob(t *testing.T) {
	var pkg *skill.Package
	for _, p := range skillhub.EssentialSkills() {
		parsed, err := skill.ParsePackage(p)
		mustOK(t, err)
		if parsed.Frontmatter.Name == "watch-and-notify" {
			pkg = &p
			if !slices.Equal(parsed.RequiresTools, []string{"manage_job"}) {
				t.Fatalf("requires-tools = %v", parsed.RequiresTools)
			}
			if !strings.Contains(parsed.Body, "[[SILENT]]") || !strings.Contains(parsed.Body, "[[FAILED]]") || strings.Contains(parsed.Body, "[[FOUND]]") {
				t.Fatal("the skill body does not teach the markers")
			}
		}
	}
	if pkg == nil || slices.Contains(skillhub.EssentialSkillNames(), "watch-and-react") {
		t.Fatalf("essential skills = %v", skillhub.EssentialSkillNames())
	}
}

// watchJob is the one job the skill sets up, delivering to the rig's chat.
func watchJob(t *testing.T, r *triggerToolsRig) job.Job {
	t.Helper()
	j, err := r.a.Jobs().Create(t.Context(), job.CreateInput{
		Name:        "AI news watch",
		Prompt:      "Check the news for items that mention AI. Reply with the items, exactly [[SILENT]] when there is nothing new, or [[FAILED]] and the reason.",
		When:        job.When{Kind: job.KindEvery, Ms: 300_000},
		SessionMode: job.SessionReuse,
		Deliveries:  job.Deliveries{{ChannelID: r.ch.ID, Target: "chat-owner", When: job.DeliverAlways}},
	})
	mustOK(t, err)
	return j
}

func replyWith(text string) job.Runner {
	return func(context.Context, job.RunContext) (job.RunResult, error) { return job.RunResult{Output: text}, nil }
}

func TestWatchNothingNewIsSilent(t *testing.T) {
	r := newTriggerToolsRig(t, nil)
	r.useRunner(replyWith("[[SILENT]]"))
	j := watchJob(t, r)
	_, err := r.a.Jobs().RunNow(t.Context(), j.ID)
	mustOK(t, err)
	run := waitRun(t, r.a.Jobs(), j.ID, done)
	if run.Status != job.RunSucceeded || !run.Silent {
		t.Fatalf("run = %+v", run)
	}
	if n := len(r.adapter.Sent()); n != 0 {
		t.Fatalf("sent %d messages", n)
	}
}

func TestWatchMatchIsPostedInOneTurn(t *testing.T) {
	r := newTriggerToolsRig(t, nil)
	r.useRunner(replyWith("- AI model released (example.com)"))
	j := watchJob(t, r)
	_, err := r.a.Jobs().RunNow(t.Context(), j.ID)
	mustOK(t, err)
	waitFor(t, "the delivery", func() bool { return len(r.adapter.Sent()) == 1 })
	if got := r.adapter.Sent()[0]; got.ChatID != "chat-owner" || got.Text != "AI news watch\n\n- AI model released (example.com)" {
		t.Fatalf("sent %+v", got)
	}
	runs, err := r.a.Jobs().ListRuns(t.Context(), j.ID, 0)
	mustOK(t, err)
	if len(runs) != 1 {
		t.Fatalf("runs = %d", len(runs))
	}
}

func TestWatchBrokenCheckFailsAndTheChatHearsWhy(t *testing.T) {
	r := newTriggerToolsRig(t, nil)
	r.useRunner(replyWith("[[FAILED]] the news search was rate-limited"))
	j := watchJob(t, r)
	_, err := r.a.Jobs().RunNow(t.Context(), j.ID)
	mustOK(t, err)
	waitFor(t, "the delivery", func() bool { return len(r.adapter.Sent()) == 1 })
	if got := r.adapter.Sent()[0].Text; got != `Job "AI news watch" failed: the news search was rate-limited` {
		t.Fatalf("sent %q", got)
	}
	run := waitRun(t, r.a.Jobs(), j.ID, done)
	if run.Status != job.RunFailed || run.Error != "the news search was rate-limited" {
		t.Fatalf("run = %+v", run)
	}
}

// "here" without a chat app: the job is created, its results stay in the run, and the
// model is told so.
func TestWatchToolDeliverHereOutsideAChatApp(t *testing.T) {
	r := newTriggerToolsRig(t, nil)
	out := execTool(t, jobs.NewManageTool(), r.toolContext(nil), `{"action":"create","name":"z","prompt":"p","every_minutes":5,"deliver":"here"}`)
	if outputIsError(out) || !strings.Contains(out.Content, "not in a chat app") {
		t.Fatalf("out = %+v", out)
	}
	list := r.list(t)
	if len(list) != 1 || len(list[0].Deliveries) > 0 {
		t.Fatalf("jobs = %+v", list)
	}
}
