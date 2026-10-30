package agent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/Phong95/aec-agent-go/components/agent/agentloop"
	"github.com/Phong95/aec-agent-go/components/agent/profile"
	"github.com/Phong95/aec-agent-go/components/agent/skilllearning"
	"github.com/Phong95/aec-agent-go/components/agent/skilllearning/learning"
	"github.com/Phong95/aec-agent-go/components/agent/subagent"
	"github.com/Phong95/aec-agent-go/components/events"
	"github.com/Phong95/aec-agent-go/components/llmhub"
	"github.com/Phong95/aec-agent-go/components/persistence"
	"github.com/Phong95/aec-agent-go/components/sandboxhub"
	"github.com/Phong95/aec-agent-go/components/skillhub"
	"github.com/Phong95/aec-agent-go/components/skillhub/skill"
	"github.com/Phong95/aec-agent-go/components/toolhub"
	"github.com/Phong95/aec-agent-go/components/trace"
	"github.com/Phong95/aec-agent-go/kernel"
	"github.com/Phong95/aec-agent-go/kernel/ordered"
)

// ---------------------------------------------------------------------------
// Verification replay (A-174) - the original request runs again in a short-lived loop
// built for this run only (like a subagent type's, see subagent_child_loop.go): the
// agent's model and tools, ONLY the draft skill in its index, a sandbox of its own (a
// fresh, isolated one - never the user's workspace), nobody to approve or answer, in a
// throwaway session deleted afterwards. Three checks decide (Agent Skill Induction):
//  1. solved             - a model judge reads the replay and finds the task done
//  2. skillUsed          - the replay loaded the draft skill (skill_view)
//  3. changedEnvironment - a call of a tool that is not read-only succeeded
// The replay's events carry the agent's id; its session (metadata source
// "skill-verification") is one the skill learner never learns from.
// ---------------------------------------------------------------------------

// replayExcludedTools are the tools a replay never gets beyond the subagent exclusions:
// calls that reach outside systems.
var replayExcludedTools = []string{"http_request", "spawn_subagent"}

const (
	// maxReplayIterations is the most model/tool round trips of one replay.
	maxReplayIterations = 30
	// replayTimeout stops a replay that runs longer (it then fails).
	replayTimeout = 10 * time.Minute
)

// ReplayNote is appended to the replayed request (byte-identical to core/ts REPLAY_NOTE).
const ReplayNote = "(Verification replay: you work in a fresh, empty workspace. If the task needs input files that are not there, " +
	"first create small stand-in inputs that match what the task describes, then do the task. " +
	"Nobody can answer questions or approve actions during this run.)"

// VerifyJudgePrompt is the system prompt of the replay judge (byte-identical to core/ts
// VERIFY_JUDGE_PROMPT).
const VerifyJudgePrompt = `You check whether an AI agent solved a task in a verification replay.

You receive the TASK, the agent's TRAJECTORY (tool calls with short excerpts, failures marked FAILED) and its FINAL REPLY. The replay ran in a fresh workspace where the agent may have created stand-in inputs: judge whether the method worked and produced the requested result, not whether the inputs were real.

Respond with ONLY a JSON object (no markdown fences): {"solved": true or false, "reason": "one sentence"}`

// verificationDeps is what a replay borrows from its agent.
type verificationDeps struct {
	models    agentloop.Models
	chat      skilllearning.Model
	toolHub   *toolhub.ToolHub
	sandboxes *sandboxhub.SandboxHub
	bus       events.Bus
	trace     *trace.Service
	hub       *persistence.PersistenceHub
	open      func(ctx context.Context, sessionID string) (*Conversation, bool, error)
	secrets   *secretManager
	logger    *slog.Logger
}

// nobody answers during a replay: approvals are denied, questions fail.
type nobody struct{}

func (nobody) AskUser(context.Context, toolhub.ClarifyRequest) (toolhub.ClarifyResponse, error) {
	return toolhub.ClarifyResponse{}, errors.New("this is a verification replay and nobody is there to answer")
}

func (nobody) RequestApproval(context.Context, toolhub.ApprovalRequest) (toolhub.ApprovalDecision, error) {
	reason := "This is a verification replay and nobody is there to approve tools, so the tool is denied."
	return toolhub.ApprovalDecision{Approved: false, Reason: &reason}, nil
}

// runVerification runs one replay. It never fails: a replay that could not run fails with
// its reason.
func runVerification(ctx context.Context, d verificationDeps, parent profile.AgentProfile, req learning.VerificationRequest) learning.Verification {
	at := func() string { return kernel.NowISO() }
	failed := func(reason string, checks learning.Verification) learning.Verification {
		checks.Passed, checks.Solved, checks.Reason, checks.At = false, false, reason, at()
		return checks
	}
	if d.sandboxes == nil {
		return failed("no isolated sandbox for a replay: the agent has no sandbox hub", learning.Verification{})
	}
	// A sandbox of its own, under an id of its own, so nothing touches the agent's files.
	own := sandboxhub.Selection{Mode: sandboxhub.SelectionOwn, Own: &sandboxhub.OwnConfig{}}
	sandboxes := NewSandboxManager(d.sandboxes, parent.ID+"-verify-"+kernel.NewID()[:8], func() sandboxhub.Selection { return own.Clone() })
	if err := sandboxes.Validate(own); err != nil {
		return failed("no isolated sandbox for a replay: "+err.Error(), learning.Verification{})
	}

	// Only the draft is in the replay's skill index.
	skills := skillhub.NewSkillHub(func() persistence.SkillStore { return persistence.NewMemorySkillStore() })
	if _, err := skills.Register(req.Draft, skill.SourceApp); err != nil {
		return failed("the draft is not a valid skill: "+err.Error(), learning.Verification{})
	}

	p := parent.Clone()
	p.Tools = slices.DeleteFunc(p.Tools, func(name string) bool {
		return subagent.IsChildExcludedTool(name) || slices.Contains(replayExcludedTools, name)
	})
	p.Skills = []string{req.SkillName}
	lc := profile.LoopConfig{}
	if p.LoopConfig != nil {
		lc = *p.LoopConfig
	}
	maxIter, zero := maxReplayIterations, 0
	if lc.MaxIterations != nil && *lc.MaxIterations < maxIter {
		maxIter = *lc.MaxIterations
	}
	lc.MaxIterations, lc.MaxConcurrentTurns = &maxIter, &zero
	p.LoopConfig = &lc
	p.Memory.Enabled = false
	p.SkillLearning = &learning.Config{Enabled: false}
	p.Subagents = nil
	tools := NewToolManager(d.toolHub, p)
	skillMgr := NewSkillManager(skills, p, WithHasTool(tools.Has))

	var meta ordered.Object
	meta.Set("source", skilllearning.VerificationSource)
	meta.Set("parentAgentId", parent.ID)
	session, err := d.hub.Sessions.Create(ctx, meta)
	if err != nil {
		return failed("the replay session could not be created: "+err.Error(), learning.Verification{})
	}
	rctx, cancel := context.WithTimeout(ctx, replayTimeout)
	defer cancel()
	defer func() {
		cleanup, done := cleanupContext(ctx)
		defer done()
		if err := sandboxes.Release(cleanup, true); err != nil && d.logger != nil {
			d.logger.Warn("skill-learning: replay sandbox not removed", "agent", parent.ID, "error", err)
		}
		_ = d.hub.Messages.Clear(cleanup, session.ID)
		_ = d.hub.CompactedMessages.Clear(cleanup, session.ID)
		_ = d.hub.Sessions.Delete(cleanup, session.ID)
	}()

	ctxOpts := []ContextOption{WithSkillIndex(skillMgr.BuildIndex)}
	var secrets agentloop.Deps
	if d.secrets != nil {
		ctxOpts = append(ctxOpts, WithSecretIndex(func(ctx context.Context) (string, bool) { return d.secrets.buildIndex(ctx, false) }))
		secrets.Secrets = d.secrets
	}
	var loopOpts []agentloop.Option
	if d.logger != nil {
		loopOpts = append(loopOpts, agentloop.WithLogger(d.logger))
	}
	loop, err := agentloop.New(agentloop.Deps{
		Models:      d.models,
		Tools:       tools,
		Skills:      skillMgr,
		Context:     NewContextManager(p, ctxOpts...),
		Events:      d.bus,
		Profile:     p.Clone,
		Trace:       d.trace,
		Sandbox:     sandboxes,
		Interaction: func(string) toolhub.InteractionHandler { return nobody{} },
		Secrets:     secrets.Secrets,
	}, loopOpts...)
	if err != nil {
		return failed("the replay failed: "+err.Error(), learning.Verification{})
	}
	conv, ok, err := d.open(rctx, session.ID)
	if err != nil || !ok {
		return failed("the replay session could not be opened", learning.Verification{})
	}
	res, runErr := loop.Run(rctx, req.Request+"\n\n"+ReplayNote, session.ID, llmhub.ModelOptions{},
		agentloop.RunOptions{Conversation: conv, TraceAttributes: map[string]any{"skillVerification": req.SkillName}})
	msgs, err := d.hub.Messages.Get(ctx, session.ID)
	if err != nil {
		return failed("the replay messages could not be read: "+err.Error(), learning.Verification{})
	}
	checks := inspectReplay(msgs, req.SkillName, func(name string) string { return toolRisk(tools, name) })
	if runErr != nil {
		if rctx.Err() != nil {
			return failed("the replay was stopped or ran out of time", checks)
		}
		return failed("the replay failed: "+runErr.Error(), checks)
	}
	if res.Aborted {
		return failed("the replay was stopped or ran out of time", checks)
	}

	solved, judged, err := judgeReplay(ctx, d.chat, req.Request, msgs)
	if err != nil {
		return failed("the replay could not be judged: "+err.Error(), checks)
	}
	checks.Solved = solved
	checks.Passed = solved && checks.SkillUsed && checks.ChangedEnvironment
	if !checks.Passed {
		var reasons []string
		if !solved {
			r := "not solved"
			if judged != "" {
				r += " (" + judged + ")"
			}
			reasons = append(reasons, r)
		}
		if !checks.SkillUsed {
			reasons = append(reasons, "the draft skill was not used")
		}
		if !checks.ChangedEnvironment {
			reasons = append(reasons, "nothing in the workspace changed")
		}
		checks.Reason = strings.Join(reasons, "; ")
	}
	checks.At = at()
	return checks
}

// toolRisk is a tool's risk ("" when the agent lacks it or it declares none).
func toolRisk(tools *ToolManager, name string) string {
	for _, def := range tools.List() {
		if def.Name == name {
			return string(def.Risk)
		}
	}
	return ""
}

// inspectReplay computes checks 2 and 3 from the replay's messages, with its call counts.
func inspectReplay(msgs []kernel.Message, skillName string, riskOf func(toolName string) string) learning.Verification {
	calls := map[string]string{}
	var v learning.Verification
	for _, m := range msgs {
		for _, c := range m.ToolCalls {
			calls[c.ID] = c.Name
			if c.Name == "skill_view" {
				if skillNameArg(c.Arguments) == skillName {
					v.SkillUsed = true
				}
			} else {
				v.ToolCalls++
			}
		}
		for _, r := range m.ToolResults {
			name, ok := calls[r.ToolCallID]
			if !ok || name == "skill_view" {
				continue
			}
			if r.IsError != nil && *r.IsError {
				v.FailedCalls++
				continue
			}
			if risk := riskOf(name); risk != "" && risk != string(toolhub.RiskReadOnly) {
				v.ChangedEnvironment = true
			}
		}
	}
	return v
}

func skillNameArg(args kernel.Arguments) string {
	var v struct {
		Name string `json:"name"`
	}
	if len(args) == 0 || args.Decode(&v) != nil {
		return ""
	}
	return v.Name
}

// judgeReplay is check 1: the model judge. It fails on a malformed answer.
func judgeReplay(ctx context.Context, chat skilllearning.Model, task string, msgs []kernel.Message) (solved bool, reason string, err error) {
	from := slices.IndexFunc(msgs, func(m kernel.Message) bool { return m.Role == kernel.RoleUser })
	if from < 0 {
		from = 0
	}
	_, trajectory, finalReply := skilllearning.FormatTrajectory(msgs[from:])
	if trajectory == "" {
		trajectory = "(no tool calls)"
	}
	maxTokens := 4096
	res, err := chat.Chat(ctx, []kernel.Message{
		{Role: kernel.RoleSystem, Content: VerifyJudgePrompt},
		{Role: kernel.RoleUser, Content: "TASK:\n" + task + "\n\nTRAJECTORY:\n" + trajectory + "\n\nFINAL REPLY:\n" + finalReply},
	}, llmhub.ModelOptions{MaxTokens: &maxTokens})
	if err != nil {
		return false, "", err
	}
	if res.StopReason == kernel.StopMaxTokens {
		return false, "", errors.New("replay judge truncated")
	}
	parsed, err := skilllearning.ParseJSONObject(res.Message.Content, "replay judge")
	if err != nil {
		return false, "", fmt.Errorf("%w", err)
	}
	reason, _ = parsed["reason"].(string)
	if r := []rune(reason); len(r) > 300 {
		reason = string(r[:300])
	}
	return parsed["solved"] == true, reason, nil
}
