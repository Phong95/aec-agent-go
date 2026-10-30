package skilllearning_test

import (
	"context"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/Phong95/aec-agent-go/components/agent"
	"github.com/Phong95/aec-agent-go/components/agent/profile"
	"github.com/Phong95/aec-agent-go/components/agent/skilllearning"
	"github.com/Phong95/aec-agent-go/components/agent/skilllearning/learning"
	"github.com/Phong95/aec-agent-go/components/events"
	"github.com/Phong95/aec-agent-go/components/llmhub"
	"github.com/Phong95/aec-agent-go/components/persistence"
	"github.com/Phong95/aec-agent-go/components/skillhub"
	"github.com/Phong95/aec-agent-go/components/skillhub/skill"
	"github.com/Phong95/aec-agent-go/kernel"
	"github.com/Phong95/aec-agent-go/kernel/ordered"
)

// A-166 skill learning end to end over a real event bus, persistence hub and skill
// manager; turns are played as the agent loop would write and announce them (the
// SkillLearningSystem tests of core/ts tests/unit/skill-learning.test.ts).

func intp(n int) *int { return &n }

func on() *learning.Config {
	return &learning.Config{Enabled: true, MinToolCalls: intp(4), MinFailedCalls: intp(2)}
}

func distilledJSON(overrides string) string {
	base := `"reusable": true, "action": "create", "name": "ifc-door-schedule-export",
		"description": "Export a door schedule from an IFC model to Excel. Use when the user asks for a door or window schedule.",
		"steps": ["Create a Python 3.11 venv", "Install ifcopenshell and openpyxl", "Read type-level property sets", "Write the workbook"],
		"pitfalls": ["pip install fails on Python 3.13: no wheel. Use 3.11."],
		"parameters": ["ifc path", "output path"], "requiresTools": []`
	if overrides != "" {
		return "{" + base + ", " + overrides + "}" // the last duplicate key wins
	}
	return "{" + base + "}"
}

// fakeModel answers distill calls with distill() and verdict calls with verdict.
type fakeModel struct {
	mu       sync.Mutex
	distill  func() string
	verdict  string
	distills int
	verdicts int
}

func (m *fakeModel) Chat(_ context.Context, msgs []kernel.Message, _ llmhub.ModelOptions) (kernel.ModelResponse, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	content := "ok"
	switch system := msgs[0].Content; {
	case strings.Contains(system, "reusable skill"):
		m.distills++
		content = distilledJSON("")
		if m.distill != nil {
			content = m.distill()
		}
	case strings.Contains(system, "accepted the result"):
		m.verdicts++
		content = m.verdict
		if content == "" {
			content = `{"verdict":"accept"}`
		}
	}
	return kernel.ModelResponse{Message: kernel.Message{Role: kernel.RoleAssistant, Content: content}, StopReason: kernel.StopEndTurn}, nil
}

func (m *fakeModel) counts() (distills, verdicts int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.distills, m.verdicts
}

type rig struct {
	t       *testing.T
	agentID string
	bus     *events.Dispatcher
	hub     *persistence.PersistenceHub
	skills  *skillhub.SkillHub
	mgr     *agent.SkillManager
	model   *fakeModel
	sys     *skilllearning.System

	mu       sync.Mutex
	captured []learning.CapturedEpisode
}

type rigOpts struct {
	model    *fakeModel
	verify   skilllearning.Verifier
	toolRisk func(string) string
	now      func() time.Time
}

func newRig(t *testing.T, cfg *learning.Config, o rigOpts) *rig {
	t.Helper()
	r := &rig{t: t, agentID: kernel.NewID(), bus: events.New(), hub: persistence.NewPersistenceHub(), model: o.model}
	if r.model == nil {
		r.model = &fakeModel{}
	}
	r.skills = skillhub.NewSkillHub(func() persistence.SkillStore { return r.hub.Skills })
	r.mgr = agent.NewSkillManager(r.skills, profile.AgentProfile{ID: r.agentID})
	var opts []skilllearning.Option
	if o.now != nil {
		opts = append(opts, skilllearning.WithClock(o.now))
	}
	sys, err := skilllearning.New(skilllearning.Deps{
		AgentID: r.agentID, Events: r.bus, Persistence: func() *persistence.PersistenceHub { return r.hub },
		Model: r.model, Skills: r.mgr, Verify: o.verify, ToolRisk: o.toolRisk,
	}, cfg, opts...)
	if err != nil {
		t.Fatal(err)
	}
	r.sys = sys
	events.Subscribe(r.bus, events.SkillLearningEpisodeCaptured, func(_ kernel.Event, p events.SkillLearningEpisodeCapturedPayload) {
		r.mu.Lock()
		r.captured = append(r.captured, p.Episode)
		r.mu.Unlock()
	})
	sys.Start(context.Background())
	t.Cleanup(func() { _ = sys.Dispose(context.Background()) })
	return r
}

func (r *rig) capturedEpisodes() []learning.CapturedEpisode {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.captured)
}

func (r *rig) idle() {
	r.t.Helper()
	if err := r.sys.WaitForIdle(context.Background()); err != nil {
		r.t.Fatal(err)
	}
}

func (r *rig) session(meta map[string]any) string {
	r.t.Helper()
	var m ordered.Object
	for k, v := range meta {
		m.Set(k, v)
	}
	s, err := r.hub.Sessions.Create(context.Background(), m)
	if err != nil {
		r.t.Fatal(err)
	}
	return s.ID
}

func (r *rig) proposals(status learning.ProposalStatus) []learning.Proposal {
	r.t.Helper()
	ps, err := r.sys.ListProposals(context.Background(), learning.ProposalFilter{Status: status})
	if err != nil {
		r.t.Fatal(err)
	}
	return ps
}

type turnSpec struct {
	text    string
	calls   []string // "ok", "fail", "retry", "denied", "error", "cancelled"
	skills  []string
	outcome learning.Outcome
	agentID string
}

var hard = turnSpec{calls: []string{"ok", "fail", "ok", "retry", "ok", "ok"}}

const request = "Export a door schedule from tower-B.ifc to Excel"

// play plays one turn the way the agent loop writes and announces it.
func (r *rig) play(sessionID string, spec turnSpec) {
	r.t.Helper()
	ctx := context.Background()
	msgs := r.hub.Messages
	id := spec.agentID
	if id == "" {
		id = r.agentID
	}
	text := spec.text
	if text == "" {
		text = request
	}
	must := func(err error) {
		if err != nil {
			r.t.Fatal(err)
		}
	}
	must(msgs.Append(ctx, sessionID, kernel.Message{Role: kernel.RoleUser, Content: text}))
	events.Emit(r.bus, events.UserMessage, sessionID, events.UserMessagePayload{AgentID: id, Role: "user", Content: text})
	for _, sk := range spec.skills {
		cid := kernel.NewID()
		args := kernel.Arguments(`{"name":"` + sk + `"}`)
		must(msgs.Append(ctx, sessionID, kernel.Message{Role: kernel.RoleAssistant, ToolCalls: []kernel.ToolCall{{ID: cid, Name: "skill_view", Arguments: args}}}))
		events.Emit(r.bus, events.SkillDispatched, sessionID, events.DispatchStartPayload{AgentID: id, Name: "skill_view", Arguments: args})
		must(msgs.Append(ctx, sessionID, kernel.Message{Role: kernel.RoleTool, ToolResults: []kernel.ToolResult{{ToolCallID: cid, Content: "<skill_content>"}}}))
	}
	for n, kind := range spec.calls {
		cid := fmt.Sprintf("call-%d", n+1)
		args := kernel.Arguments(fmt.Sprintf(`{"command":"step %d"}`, n+1))
		must(msgs.Append(ctx, sessionID, kernel.Message{Role: kernel.RoleAssistant, ToolCalls: []kernel.ToolCall{{ID: cid, Name: "terminal", Arguments: args}}}))
		events.Emit(r.bus, events.ToolDispatched, sessionID, events.DispatchStartPayload{AgentID: id, Name: "terminal", Arguments: args})
		isErr := kind != "ok" && kind != "retry"
		switch kind {
		case "denied":
			events.Emit(r.bus, events.ToolDenied, sessionID, events.ToolDeniedPayload{AgentID: id, Name: "terminal"})
		case "error", "cancelled":
			e := "boom"
			if kind == "cancelled" {
				e = "cancelled"
			}
			events.Emit(r.bus, events.ToolFailed, sessionID, events.DispatchFailedPayload{AgentID: id, Name: "terminal", Error: e})
		default:
			attempt := 0
			if kind == "retry" {
				attempt = 1
			}
			events.Emit(r.bus, events.ToolCompleted, sessionID, events.DispatchEndPayload{AgentID: id, Name: "terminal", IsError: kind == "fail", Attempt: attempt})
		}
		content := "done"
		if isErr {
			content = "pip: no wheel for Python 3.13"
		}
		must(msgs.Append(ctx, sessionID, kernel.Message{Role: kernel.RoleTool, ToolResults: []kernel.ToolResult{{ToolCallID: cid, Content: content, IsError: &isErr}}}))
	}
	must(msgs.Append(ctx, sessionID, kernel.Message{Role: kernel.RoleAssistant, Content: "Here is door-schedule.xlsx."}))
	usage := kernel.TokenUsage{InputTokens: 1, OutputTokens: 1}
	switch spec.outcome {
	case learning.OutcomeMaxIterations:
		events.Emit(r.bus, events.MaxIterationsReached, sessionID, events.MaxIterationsPayload{AgentID: id, Iterations: 10, TotalUsage: usage})
	default:
		events.Emit(r.bus, events.RunCompleted, sessionID, events.RunCompletedPayload{AgentID: id, Iterations: 3, TotalUsage: usage, Aborted: spec.outcome == learning.OutcomeAborted})
	}
}

func (r *rig) learnOnce(spec turnSpec) learning.Proposal {
	r.t.Helper()
	s := r.session(nil)
	r.play(s, spec)
	r.play(s, turnSpec{text: "thanks"})
	r.idle()
	ps := r.proposals("")
	if len(ps) == 0 {
		r.t.Fatal("no proposal")
	}
	return ps[0]
}

func TestDisabledLearnsNothing(t *testing.T) {
	r := newRig(t, nil, rigOpts{})
	s := r.session(nil)
	r.play(s, hard)
	r.play(s, turnSpec{text: "thanks"})
	r.idle()
	if len(r.capturedEpisodes()) != 0 || len(r.proposals("")) != 0 {
		t.Fatal("a disabled learner must learn nothing")
	}
	res, err := r.sys.ConfirmLastTurn(context.Background(), s)
	if err != nil || res.Status != learning.LearnDisabled {
		t.Fatalf("learn = %+v %v", res, err)
	}
}

func TestAcceptedStruggleBecomesAProposalAndAcceptingCreatesTheSkill(t *testing.T) {
	r := newRig(t, on(), rigOpts{})
	var proposed []events.SkillLearningProposedPayload
	events.Subscribe(r.bus, events.SkillLearningProposed, func(_ kernel.Event, p events.SkillLearningProposedPayload) { proposed = append(proposed, p) })
	s := r.session(nil)
	r.play(s, hard)
	r.idle()
	if len(r.capturedEpisodes()) != 0 {
		t.Fatal("captured before the verdict")
	}
	r.play(s, turnSpec{text: "Perfect, thanks!"})
	r.idle()
	eps := r.capturedEpisodes()
	if len(eps) != 1 {
		t.Fatalf("captured %d", len(eps))
	}
	ep := eps[0]
	if ep.SessionID != s || ep.ToolCalls != 6 || ep.FailedCalls != 2 || ep.Signal != learning.SignalNextMessage || ep.MessageStart != 0 ||
		ep.MessageEnd != 14 || ep.Outcome != learning.OutcomeCompleted {
		t.Fatalf("episode = %+v", ep)
	}
	distills, verdicts := r.model.counts()
	if verdicts != 0 || distills != 1 {
		t.Fatalf("distills %d verdicts %d", distills, verdicts)
	}
	pending := r.proposals(learning.StatusPending)
	if len(pending) != 1 {
		t.Fatalf("pending = %d", len(pending))
	}
	p := pending[0]
	if p.Action != learning.ActionCreate || p.SkillName != "ifc-door-schedule-export" || p.Episode.Request != request ||
		p.Episode.ToolCalls != 6 || p.Draft == nil {
		t.Fatalf("proposal = %+v", p)
	}
	if _, err := skill.ParsePackage(*p.Draft); err != nil {
		t.Fatal(err)
	}
	if len(proposed) != 1 || proposed[0].ProposalID != p.ID {
		t.Fatalf("proposed = %+v", proposed)
	}
	accepted, err := r.sys.Accept(context.Background(), p.ID, nil)
	if err != nil || accepted.Status != learning.StatusAccepted {
		t.Fatalf("accept: %+v %v", accepted, err)
	}
	if _, ok, _ := r.mgr.Get(context.Background(), "ifc-door-schedule-export"); !ok {
		t.Fatal("the skill is not enabled for the agent")
	}
	if idx, ok := r.mgr.BuildIndex(context.Background()); !ok || !strings.Contains(idx, "ifc-door-schedule-export") {
		t.Fatalf("index = %q", idx)
	}
	learned, _ := r.sys.ListLearnedSkills(context.Background())
	if len(learned) != 1 || learned[0].Health != 3 {
		t.Fatalf("learned = %+v", learned)
	}
	if _, err := r.sys.Accept(context.Background(), p.ID, nil); !kernel.HasCode(err, kernel.SkillCodeInvalid) {
		t.Fatalf("second accept: %v", err)
	}
}

func TestCorrectionIsNeverLearnedAndUnclearAsksTheJudge(t *testing.T) {
	r := newRig(t, on(), rigOpts{model: &fakeModel{verdict: `{"verdict":"correct"}`}})
	s := r.session(nil)
	r.play(s, hard)
	r.play(s, turnSpec{text: "No, the totals are wrong"})
	r.idle()
	if _, v := r.model.counts(); v != 0 || len(r.capturedEpisodes()) != 0 {
		t.Fatal("a correction must be dropped without a judge")
	}
	r.play(s, hard)
	r.play(s, turnSpec{text: "Now do the windows"})
	r.idle()
	if _, v := r.model.counts(); v != 1 || len(r.capturedEpisodes()) != 0 {
		t.Fatalf("verdicts %d captured %d", v, len(r.capturedEpisodes()))
	}
}

func TestUnclearReplyTheJudgeAcceptsIsCaptured(t *testing.T) {
	r := newRig(t, on(), rigOpts{})
	s := r.session(nil)
	r.play(s, hard)
	r.play(s, turnSpec{text: "Now do the windows"})
	r.idle()
	if _, v := r.model.counts(); v != 1 || len(r.capturedEpisodes()) != 1 {
		t.Fatalf("verdicts %d captured %d", v, len(r.capturedEpisodes()))
	}
}

func TestDeniedAreNotFailuresAndAbortedOrMaxIterationsAreDropped(t *testing.T) {
	r := newRig(t, on(), rigOpts{})
	s := r.session(nil)
	r.play(s, turnSpec{calls: []string{"ok", "denied", "denied", "fail", "ok", "cancelled"}})
	r.play(s, turnSpec{text: "thanks"})
	aborted := hard
	aborted.outcome = learning.OutcomeAborted
	r.play(s, aborted)
	r.play(s, turnSpec{text: "thanks"})
	maxed := hard
	maxed.outcome = learning.OutcomeMaxIterations
	r.play(s, maxed)
	r.play(s, turnSpec{text: "thanks"})
	r.idle()
	if len(r.capturedEpisodes()) != 0 {
		t.Fatalf("captured %+v", r.capturedEpisodes())
	}
}

func TestJobSubagentAndVerificationSessionsAreIgnored(t *testing.T) {
	r := newRig(t, on(), rigOpts{})
	ctx := context.Background()
	for _, s := range []string{r.session(map[string]any{"source": "subagent"}), r.session(map[string]any{"source": skilllearning.VerificationSource})} {
		r.play(s, hard)
		r.play(s, turnSpec{text: "thanks"})
	}
	job := r.session(nil)
	sess, _ := r.hub.Sessions.Get(ctx, job)
	sess.JobID = "job-1"
	if err := r.hub.Sessions.Update(ctx, *sess); err != nil {
		t.Fatal(err)
	}
	r.play(job, hard)
	r.play(job, turnSpec{text: "thanks"})
	r.idle()
	if len(r.capturedEpisodes()) != 0 {
		t.Fatal("job, subagent and verification sessions must be ignored")
	}
	if res, _ := r.sys.ConfirmLastTurn(ctx, job); res.Status != learning.LearnNoTurn {
		t.Fatalf("learn = %+v", res)
	}
}

func TestAnotherAgentsEventsAreIgnored(t *testing.T) {
	r := newRig(t, on(), rigOpts{})
	s := r.session(nil)
	stranger := hard
	stranger.agentID = "someone-else"
	r.play(s, stranger)
	r.play(s, turnSpec{text: "thanks", agentID: "someone-else"})
	r.idle()
	if len(r.capturedEpisodes()) != 0 {
		t.Fatal("captured another agent's turn")
	}
}

func TestLearnConfirmsTheLastFinishedTurnOnce(t *testing.T) {
	r := newRig(t, on(), rigOpts{})
	ctx := context.Background()
	s := r.session(nil)
	if res, _ := r.sys.ConfirmLastTurn(ctx, s); res.Status != learning.LearnNoTurn {
		t.Fatalf("first = %+v", res)
	}
	r.play(s, turnSpec{calls: []string{"ok"}})
	if res, _ := r.sys.ConfirmLastTurn(ctx, s); res.Status != learning.LearnCaptured || !strings.Contains(res.Text, "draft skill") {
		t.Fatalf("learn = %+v", res)
	}
	if res, _ := r.sys.ConfirmLastTurn(ctx, s); res.Status != learning.LearnAlreadyCaptured {
		t.Fatalf("again = %+v", res)
	}
	r.idle()
	eps := r.capturedEpisodes()
	if len(eps) != 1 || eps[0].Signal != learning.SignalLearnCommand || eps[0].ToolCalls != 1 || len(r.proposals("")) != 1 {
		t.Fatalf("captured = %+v", eps)
	}
	r.play(s, turnSpec{})
	if res, _ := r.sys.ConfirmLastTurn(ctx, s); res.Status != learning.LearnNotEligible {
		t.Fatalf("no tools = %+v", res)
	}
}

func TestSilenceGivesUpButLearnCanStillConfirm(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cfg := on()
		cfg.VerdictWaitMinutes = intp(5)
		r := newRig(t, cfg, rigOpts{})
		s := r.session(nil)
		r.play(s, hard)
		r.idle()
		time.Sleep(5*time.Minute + time.Second)
		synctest.Wait()
		r.play(s, turnSpec{text: "thanks"})
		r.idle()
		if len(r.capturedEpisodes()) != 0 {
			t.Fatal("a reply after the wait must not capture")
		}
	})
	synctest.Test(t, func(t *testing.T) {
		cfg := on()
		cfg.VerdictWaitMinutes = intp(5)
		r := newRig(t, cfg, rigOpts{})
		s := r.session(nil)
		r.play(s, hard)
		r.idle()
		time.Sleep(5*time.Minute + time.Second)
		synctest.Wait()
		if res, _ := r.sys.ConfirmLastTurn(context.Background(), s); res.Status != learning.LearnCaptured {
			t.Fatalf("learn = %+v", res)
		}
		r.idle()
		if len(r.capturedEpisodes()) != 1 {
			t.Fatal("/learn after the wait must capture")
		}
	})
}

func TestDisposeStopsTheVerdictTimerAndRuntimeToggle(t *testing.T) {
	r := newRig(t, on(), rigOpts{})
	s := r.session(nil)
	r.play(s, hard)
	r.idle()
	if err := r.sys.Dispose(context.Background()); err != nil {
		t.Fatal(err)
	}
	r.play(s, turnSpec{text: "thanks"})
	if len(r.capturedEpisodes()) != 0 {
		t.Fatal("a disposed learner must not capture")
	}

	off := newRig(t, nil, rigOpts{})
	off.sys.UpdateConfig(&learning.Config{Enabled: true})
	if !off.sys.Enabled() {
		t.Fatal("not enabled")
	}
	off.sys.UpdateConfig(nil)
	if off.sys.Enabled() {
		t.Fatal("still enabled")
	}
}

func TestInvalidDraftIsTriedOnceMoreThenDropped(t *testing.T) {
	var n atomic.Int32
	r := newRig(t, on(), rigOpts{model: &fakeModel{distill: func() string {
		if n.Add(1) == 1 {
			return "not json"
		}
		return distilledJSON("")
	}}})
	r.learnOnce(hard)
	if d, _ := r.model.counts(); d != 2 {
		t.Fatalf("distills = %d", d)
	}
	bad := newRig(t, on(), rigOpts{model: &fakeModel{distill: func() string { return "garbage" }}})
	s := bad.session(nil)
	bad.play(s, hard)
	bad.play(s, turnSpec{text: "thanks"})
	bad.idle()
	if d, _ := bad.model.counts(); d != 2 || len(bad.proposals("")) != 0 {
		t.Fatalf("distills %d proposals %d", d, len(bad.proposals("")))
	}
}

func TestSecretsAreNeverCopiedIntoAProposal(t *testing.T) {
	r := newRig(t, on(), rigOpts{model: &fakeModel{distill: func() string {
		return distilledJSON(`"steps": ["curl -H 'Authorization: sk-ant-abcdefghijklmnopqrstuvwxyz0123' ..."]`)
	}}})
	s := r.session(nil)
	r.play(s, hard)
	r.play(s, turnSpec{text: "thanks"})
	r.idle()
	if len(r.proposals("")) != 0 {
		t.Fatal("a draft with a secret was proposed")
	}
}

func TestRejectAndEditedAccept(t *testing.T) {
	r := newRig(t, on(), rigOpts{})
	ctx := context.Background()
	p1 := r.learnOnce(hard)
	rejected, err := r.sys.Reject(ctx, p1.ID, "too specific")
	if err != nil || rejected.Status != learning.StatusRejected || rejected.Reason != "too specific" {
		t.Fatalf("reject: %+v %v", rejected, err)
	}
	if _, err := r.sys.Reject(ctx, "nope", ""); !kernel.HasCode(err, kernel.SkillCodeNotFound) {
		t.Fatalf("unknown: %v", err)
	}
	r.learnOnce(hard)
	p2 := r.proposals(learning.StatusPending)[0]
	edited := skill.Package{Content: "---\nname: door-export\ndescription: Edited by hand.\n---\n\nDo it my way.\n"}
	accepted, err := r.sys.Accept(ctx, p2.ID, &edited)
	if err != nil || accepted.SkillName != "door-export" || accepted.Summary != "Edited by hand." {
		t.Fatalf("accept: %+v %v", accepted, err)
	}
	if _, ok, _ := r.mgr.Get(ctx, "door-export"); !ok {
		t.Fatal("edited skill not enabled")
	}
}

func TestStruggleWithACustomSkillBecomesAPatch(t *testing.T) {
	r := newRig(t, on(), rigOpts{model: &fakeModel{distill: func() string { return distilledJSON(`"action": "patch", "target": "door-export"`) }}})
	ctx := context.Background()
	if _, err := r.mgr.Create(ctx, skill.Package{Content: "---\nname: door-export\ndescription: Old way.\n---\n\nOld steps.\n"}); err != nil {
		t.Fatal(err)
	}
	spec := hard
	spec.skills = []string{"door-export"}
	p := r.learnOnce(spec)
	if eps := r.capturedEpisodes(); eps[0].PatchTarget != "door-export" {
		t.Fatalf("patch target = %q", eps[0].PatchTarget)
	}
	if p.Action != learning.ActionPatch || p.SkillName != "door-export" || p.PreviousContent == nil || !strings.Contains(*p.PreviousContent, "Old steps.") {
		t.Fatalf("proposal = %+v", p)
	}
	if _, err := r.sys.Accept(ctx, p.ID, nil); err != nil {
		t.Fatal(err)
	}
	sk, _, _ := r.skills.Get(ctx, "door-export")
	if !strings.Contains(sk.Body, "## Pitfalls") {
		t.Fatalf("body = %q", sk.Body)
	}
}

var pass = learning.Verification{Passed: true, Solved: true, SkillUsed: true, ChangedEnvironment: true, ToolCalls: 3, At: "t"}

func TestAutoModePassingReplaySavesAtOnce(t *testing.T) {
	var calls []learning.VerificationRequest
	var mu sync.Mutex
	cfg := on()
	cfg.Mode = learning.ModeAuto
	r := newRig(t, cfg, rigOpts{verify: func(_ context.Context, req learning.VerificationRequest) learning.Verification {
		mu.Lock()
		calls = append(calls, req)
		mu.Unlock()
		return pass
	}})
	var updated []events.SkillLearningSkillUpdatedPayload
	events.Subscribe(r.bus, events.SkillLearningSkillUpdated, func(_ kernel.Event, p events.SkillLearningSkillUpdatedPayload) { updated = append(updated, p) })
	p := r.learnOnce(hard)
	if len(calls) != 1 || calls[0].Request != request || calls[0].SkillName != "ifc-door-schedule-export" {
		t.Fatalf("calls = %+v", calls)
	}
	if p.Status != learning.StatusAccepted || !p.Auto || p.Verification == nil || !p.Verification.Passed {
		t.Fatalf("proposal = %+v", p)
	}
	if len(updated) != 1 || updated[0].Action != learning.ActionCreate || !updated[0].Auto {
		t.Fatalf("updated = %+v", updated)
	}
}

func TestAutoModeFailingReplayLeavesTheDraftForReview(t *testing.T) {
	cfg := on()
	cfg.Mode = learning.ModeAuto
	r := newRig(t, cfg, rigOpts{verify: func(context.Context, learning.VerificationRequest) learning.Verification {
		v := pass
		v.Passed, v.SkillUsed, v.Reason = false, false, "the draft skill was not used"
		return v
	}})
	p := r.learnOnce(hard)
	if p.Status != learning.StatusPending || p.Auto || p.Verification == nil || p.Verification.Reason != "the draft skill was not used" {
		t.Fatalf("proposal = %+v", p)
	}
	if _, ok, _ := r.mgr.Get(context.Background(), "ifc-door-schedule-export"); ok {
		t.Fatal("saved without a passing replay")
	}
}

func TestAutoModeNeverReplaysAConsequentialTask(t *testing.T) {
	var calls atomic.Int32
	cfg := on()
	cfg.Mode = learning.ModeAuto
	r := newRig(t, cfg, rigOpts{
		verify: func(context.Context, learning.VerificationRequest) learning.Verification { calls.Add(1); return pass },
		toolRisk: func(n string) string {
			return map[bool]string{true: "consequential", false: "read-only"}[n == "terminal"]
		},
	})
	p := r.learnOnce(hard)
	if calls.Load() != 0 || p.Status != learning.StatusPending || p.Verification == nil || p.Verification.Passed ||
		!regexp.MustCompile(`terminal.*outside systems`).MatchString(p.Verification.Reason) {
		t.Fatalf("proposal = %+v", p)
	}
}

func TestAutoModeRespectsTheDailyReplayBudget(t *testing.T) {
	var calls, i atomic.Int32
	cfg := on()
	cfg.Mode, cfg.VerificationsPerDay = learning.ModeAuto, intp(1)
	r := newRig(t, cfg, rigOpts{
		model: &fakeModel{distill: func() string { return distilledJSON(fmt.Sprintf(`"name": "skill-%d"`, i.Add(1))) }},
		verify: func(context.Context, learning.VerificationRequest) learning.Verification {
			calls.Add(1)
			v := pass
			v.Passed, v.Reason = false, "not solved"
			return v
		},
	})
	r.learnOnce(hard)
	r.learnOnce(hard)
	if calls.Load() != 1 {
		t.Fatalf("replays = %d", calls.Load())
	}
	for _, p := range r.proposals("") {
		if p.SkillName == "skill-2" && (p.Verification == nil || !strings.Contains(p.Verification.Reason, "used up")) {
			t.Fatalf("second = %+v", p)
		}
	}
}

func TestProposeModeNeverReplays(t *testing.T) {
	var calls atomic.Int32
	r := newRig(t, on(), rigOpts{verify: func(context.Context, learning.VerificationRequest) learning.Verification { calls.Add(1); return pass }})
	p := r.learnOnce(hard)
	if calls.Load() != 0 || p.Verification != nil {
		t.Fatalf("proposal = %+v", p)
	}
}

func TestRespectsMaxLearnedSkills(t *testing.T) {
	var i atomic.Int32
	cfg := on()
	cfg.MaxLearnedSkills = intp(1)
	r := newRig(t, cfg, rigOpts{model: &fakeModel{distill: func() string { return distilledJSON(fmt.Sprintf(`"name": "skill-%d"`, i.Add(1))) }}})
	s := r.session(nil)
	for range 2 {
		r.play(s, hard)
		r.play(s, turnSpec{text: "thanks"})
	}
	r.idle()
	ps := r.proposals("")
	if len(ps) != 1 || ps[0].SkillName != "skill-1" {
		t.Fatalf("proposals = %+v", ps)
	}
}

// --- learned-skill health (A-172) ---------------------------------------------------

func learnOne(t *testing.T, o rigOpts) (*rig, string) {
	t.Helper()
	if o.model == nil {
		o.model = &fakeModel{distill: func() string { return distilledJSON(`"action": "patch", "target": "ifc-door-schedule-export"`) }}
	}
	r := newRig(t, on(), o)
	s := r.session(nil)
	r.play(s, hard)
	r.play(s, turnSpec{text: "thanks"})
	r.idle()
	if _, err := r.sys.Accept(context.Background(), r.proposals("")[0].ID, nil); err != nil {
		t.Fatal(err)
	}
	return r, s
}

func TestHealthUpOnSmoothUseDownOnStruggleWhichBecomesAPatch(t *testing.T) {
	r, s := learnOne(t, rigOpts{})
	var used []events.SkillLearningSkillUsedPayload
	events.Subscribe(r.bus, events.SkillLearningSkillUsed, func(_ kernel.Event, p events.SkillLearningSkillUsedPayload) { used = append(used, p) })
	ctx := context.Background()
	r.play(s, turnSpec{calls: []string{"ok", "ok"}, skills: []string{"ifc-door-schedule-export"}})
	r.idle()
	if l, _ := r.sys.ListLearnedSkills(ctx); l[0].Health != 4 || l[0].Uses != 1 || l[0].SmoothUses != 1 {
		t.Fatalf("after smooth = %+v", l[0])
	}
	// Struggled (2 failures, only 3 calls - below MinToolCalls): still a patch candidate.
	r.play(s, turnSpec{calls: []string{"fail", "fail", "ok"}, skills: []string{"ifc-door-schedule-export"}})
	r.play(s, turnSpec{text: "thanks"})
	r.idle()
	if len(used) != 2 || !used[0].Smooth || used[1].Smooth {
		t.Fatalf("used = %+v", used)
	}
	// A-171: before/after for the metrics - the learned-from episode had 6 calls, 2 failed.
	if used[0].ToolCalls != 2 || used[0].FailedCalls != 0 || used[0].LearnedFrom == nil || *used[0].LearnedFrom != (events.SkillLearningCallCounts{ToolCalls: 6, FailedCalls: 2}) {
		t.Fatalf("used[0] = %+v", used[0])
	}
	if l, _ := r.sys.ListLearnedSkills(ctx); l[0].Health != 3 || l[0].Uses != 2 || l[0].StruggledUses != 1 {
		t.Fatalf("after struggle = %+v", l[0])
	}
	pending := r.proposals(learning.StatusPending)
	if len(pending) != 1 || pending[0].Action != learning.ActionPatch || pending[0].SkillName != "ifc-door-schedule-export" {
		t.Fatalf("pending = %+v", pending)
	}
}

func TestHealthZeroProposesRetiringAndAcceptingDeletes(t *testing.T) {
	r, s := learnOne(t, rigOpts{})
	ctx := context.Background()
	for range 3 {
		r.play(s, turnSpec{calls: []string{"fail", "fail"}, skills: []string{"ifc-door-schedule-export"}, outcome: learning.OutcomeAborted})
	}
	r.idle()
	var retire *learning.Proposal
	for _, p := range r.proposals(learning.StatusPending) {
		if p.Action == learning.ActionRetire {
			retire = &p
		}
	}
	if retire == nil || retire.SkillName != "ifc-door-schedule-export" || retire.PreviousContent == nil || !strings.Contains(*retire.PreviousContent, "ifc-door-schedule-export") {
		t.Fatalf("retire = %+v", retire)
	}
	if _, err := r.sys.Accept(ctx, retire.ID, nil); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := r.skills.Get(ctx, "ifc-door-schedule-export"); ok {
		t.Fatal("the skill must be deleted")
	}
	if _, ok, _ := r.mgr.Get(ctx, "ifc-door-schedule-export"); ok {
		t.Fatal("the skill must be disabled")
	}
	if l, _ := r.sys.ListLearnedSkills(ctx); len(l) != 0 {
		t.Fatalf("learned = %+v", l)
	}
}

func TestUnusedLearnedSkillIsProposedForRetirement(t *testing.T) {
	var now atomic.Int64
	now.Store(time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC).UnixMilli())
	cfg := on()
	cfg.RetireAfterDays = intp(30)
	r := newRig(t, cfg, rigOpts{now: func() time.Time { return time.UnixMilli(now.Load()) }})
	p := r.learnOnce(hard)
	if _, err := r.sys.Accept(context.Background(), p.ID, nil); err != nil {
		t.Fatal(err)
	}
	now.Store(time.Date(2026, 12, 15, 0, 0, 0, 0, time.UTC).UnixMilli())
	s := r.session(nil)
	r.play(s, turnSpec{})
	r.idle()
	for _, x := range r.proposals(learning.StatusPending) {
		if x.Action == learning.ActionRetire && strings.Contains(x.Summary, "Not used for 44 days") {
			return
		}
	}
	t.Fatalf("no retire proposal: %+v", r.proposals(""))
}
