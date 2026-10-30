package skilllearning

import (
	"errors"
	"strings"
	"testing"

	"github.com/Phong95/aec-agent-go/components/agent/skilllearning/learning"
	"github.com/Phong95/aec-agent-go/components/skillhub/skill"
	"github.com/Phong95/aec-agent-go/kernel"
)

// The internals of A-166 skill learning: the ledger, the judge, the verdict rule and the
// distiller (the unit tests of core/ts tests/unit/skill-learning.test.ts).

func intp(n int) *int { return &n }

func TestLedgerCountsFailuresNeverDenialsOrCancels(t *testing.T) {
	l := newEpisodeLedger("a1")
	l.begin("s1", "t0")
	l.setMessageStart("s1", l.tokenOf("s1"), 7)
	for range 6 {
		l.toolStarted("s1")
	}
	l.toolEnded("s1", false, 0)
	l.toolEnded("s1", true, 0)
	l.toolEnded("s1", false, 2)
	l.toolFailed("s1", "boom")
	l.toolFailed("s1", "cancelled")
	l.toolDenied("s1")
	l.skillStarted("s1", "plan")
	l.skillStarted("s1", "plan")
	ep, ok := l.end("s1", learning.OutcomeCompleted, "t1", 20)
	if !ok {
		t.Fatal("no episode")
	}
	if ep.ToolCalls != 6 || ep.FailedCalls != 3 || ep.DeniedCalls != 1 || ep.MessageStart != 7 || ep.MessageEnd != 20 ||
		len(ep.SkillsUsed) != 1 || ep.SkillsUsed[0] != "plan" {
		t.Fatalf("episode = %+v", ep)
	}
	if _, ok := l.end("s1", learning.OutcomeCompleted, "t2", 20); ok {
		t.Fatal("a second end must find no turn")
	}
}

func TestLedgerStaleMessageStartNeverLandsOnANewerTurn(t *testing.T) {
	l := newEpisodeLedger("a1")
	first := l.begin("s1", "t0")
	l.begin("s1", "t1")
	l.setMessageStart("s1", first, 3)
	ep, _ := l.end("s1", learning.OutcomeCompleted, "t2", 9)
	if ep.MessageStart != -1 {
		t.Fatalf("messageStart = %d", ep.MessageStart)
	}
}

func TestHardWonNeedsANormalEndEnoughCallsAndFailures(t *testing.T) {
	c := learning.ResolveConfig(&learning.Config{Enabled: true, MinToolCalls: intp(4), MinFailedCalls: intp(2)})
	ep := func(calls, failed int, o learning.Outcome) learning.Episode {
		return learning.Episode{ToolCalls: calls, FailedCalls: failed, Outcome: o}
	}
	cases := []struct {
		e    learning.Episode
		want bool
	}{
		{ep(4, 2, learning.OutcomeCompleted), true},
		{ep(3, 2, learning.OutcomeCompleted), false},
		{ep(9, 1, learning.OutcomeCompleted), false},
		{ep(9, 5, learning.OutcomeAborted), false},
		{ep(9, 5, learning.OutcomeMaxIterations), false},
	}
	for i, c2 := range cases {
		if got := isHardWon(c2.e, c); got != c2.want {
			t.Errorf("case %d: %v, want %v", i, got, c2.want)
		}
	}
}

func TestConfigDefaultsAndClamps(t *testing.T) {
	r := learning.ResolveConfig(nil)
	if r.Enabled || r.Mode != learning.ModePropose || r.MinToolCalls != 8 || r.MinFailedCalls != 2 || r.VerdictWaitMinutes != 30 ||
		r.MaxLearnedSkills != 20 || r.RetireAfterDays != 60 || r.VerificationsPerDay != 5 {
		t.Fatalf("defaults = %+v", r)
	}
	r = learning.ResolveConfig(&learning.Config{Enabled: true, MinToolCalls: intp(-3), VerdictWaitMinutes: intp(1_000_000_000), Mode: learning.ModeAuto, VerificationsPerDay: intp(500)})
	if r.MinToolCalls != 1 || r.VerdictWaitMinutes != 10080 || r.Mode != learning.ModeAuto || r.VerificationsPerDay != 100 {
		t.Fatalf("clamped = %+v", r)
	}
}

func TestClassifyReply(t *testing.T) {
	for text, want := range map[string]replyVerdict{
		"Perfect, thanks!":         verdictAccept,
		"No, the totals are wrong": verdictCorrect,
		"it still fails":           verdictCorrect,
		"No problem, thanks":       verdictUnclear,
		"Now do the windows":       verdictUnclear,
	} {
		if got := classifyReply(text); got != want {
			t.Errorf("%q: %s, want %s", text, got, want)
		}
	}
}

func distilledJSON(overrides string) string {
	base := `"reusable": true, "action": "create", "name": "ifc-door-schedule-export",
		"description": "Export a door schedule from an IFC model to Excel. Use when the user asks for a door or window schedule.",
		"steps": ["Create a Python 3.11 venv", "Install ifcopenshell and openpyxl", "Read type-level property sets", "Write the workbook"],
		"pitfalls": ["pip install fails on Python 3.13: no wheel. Use 3.11."],
		"parameters": ["ifc path", "output path"], "requiresTools": ["terminal"]`
	if overrides != "" {
		return "{" + base + ", " + overrides + "}" // the last duplicate key wins
	}
	return "{" + base + "}"
}

func isErr() *bool { b := true; return &b }

func TestDistillerKeepsOneTurnAndMarksFailures(t *testing.T) {
	turn := []kernel.Message{
		{Role: kernel.RoleUser, Content: "Export doors"},
		{Role: kernel.RoleAssistant, Content: "Trying pip", ToolCalls: []kernel.ToolCall{{ID: "c1", Name: "terminal", Arguments: kernel.Arguments(`{"command":"pip install ifcopenshell"}`)}}},
		{Role: kernel.RoleTool, ToolResults: []kernel.ToolResult{{ToolCallID: "c1", Content: "ERROR: no matching distribution", IsError: isErr()}}},
		{Role: kernel.RoleAssistant, ToolCalls: []kernel.ToolCall{{ID: "c2", Name: "terminal", Arguments: kernel.Arguments(`{"command":"python3.11 -m venv v"}`)}}},
		{Role: kernel.RoleTool, ToolResults: []kernel.ToolResult{{ToolCallID: "c2", Content: "ok"}}},
		{Role: kernel.RoleAssistant, Content: "Done: doors.xlsx"},
		{Role: kernel.RoleUser, Content: "thanks"},
	}
	msgs := turnMessages(turn)
	if len(msgs) != 6 {
		t.Fatalf("turn has %d messages", len(msgs))
	}
	request, trajectory, finalReply := formatTrajectory(msgs)
	if request != "Export doors" || finalReply != "Done: doors.xlsx" {
		t.Fatalf("request %q reply %q", request, finalReply)
	}
	for _, want := range []string{"1. call terminal", "FAILED terminal: ERROR: no matching distribution", "ok terminal: ok"} {
		if !strings.Contains(trajectory, want) {
			t.Errorf("trajectory lacks %q:\n%s", want, trajectory)
		}
	}
	prompt := buildDistillPrompt(distillInput{messages: msgs, skills: []skill.Skill{{Name: "plan", Description: "Plan work"}}, target: &skill.Skill{Name: "x", Content: "OLD"}})
	if !strings.Contains(prompt, "EXISTING SKILLS:\n- plan: Plan work") || !strings.Contains(prompt, "TARGET SKILL (x):\nOLD") {
		t.Fatalf("prompt:\n%s", prompt)
	}
}

func TestDistillerRendersAValidSkill(t *testing.T) {
	d, err := parseDistilled("```json\n" + distilledJSON(`"description": "Say \"hi\": then go"`) + "\n```")
	if err != nil || d == nil {
		t.Fatalf("parse: %v", err)
	}
	pkg, err := renderSkill(*d)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := skill.ParsePackage(pkg)
	if err != nil {
		t.Fatal(err)
	}
	learnedBy, _ := parsed.Frontmatter.Metadata.Get("learned-by")
	if parsed.Frontmatter.Name != "ifc-door-schedule-export" || parsed.Frontmatter.Description != `Say "hi": then go` ||
		len(parsed.RequiresTools) != 1 || parsed.RequiresTools[0] != "terminal" || learnedBy != "skill-learning" ||
		!strings.Contains(parsed.Body, "## Pitfalls") || !strings.Contains(parsed.Body, "1. Create a Python 3.11 venv") {
		t.Fatalf("parsed = %+v", parsed)
	}
}

func TestDistillerNotReusableAndMalformed(t *testing.T) {
	if d, err := parseDistilled(`{"reusable": false, "reason": "one-off"}`); d != nil || err != nil {
		t.Fatalf("not reusable: %v %v", d, err)
	}
	for _, bad := range []string{"", "[1,2]", distilledJSON(`"name": "Bad Name"`), distilledJSON(`"steps": []`)} {
		if _, err := parseDistilled(bad); err == nil {
			t.Errorf("%q must fail", bad)
		}
	}
}

func TestDistillerDraftWithASecretFails(t *testing.T) {
	d, err := parseDistilled(distilledJSON(`"steps": ["Use key sk-ant-abcdefghijklmnopqrstuvwxyz0123"]`))
	if err != nil {
		t.Fatal(err)
	}
	_, err = renderSkill(*d)
	var se *kernel.SkillError
	if !errors.As(err, &se) {
		t.Fatalf("err = %v", err)
	}
}
