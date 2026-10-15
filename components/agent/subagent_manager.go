package agent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Phong95/aec-agent-go/components/agent/profile"
	"github.com/Phong95/aec-agent-go/components/agent/subagent"
	"github.com/Phong95/aec-agent-go/components/events"
	"github.com/Phong95/aec-agent-go/components/persistence"
	"github.com/Phong95/aec-agent-go/components/sandboxhub"
	"github.com/Phong95/aec-agent-go/components/subagenthub"
	"github.com/Phong95/aec-agent-go/components/subagenthub/subagenttype"
	"github.com/Phong95/aec-agent-go/components/toolhub"
	"github.com/Phong95/aec-agent-go/internal/jsstr"
	"github.com/Phong95/aec-agent-go/internal/nilcheck"
	"github.com/Phong95/aec-agent-go/internal/redact"
	"github.com/Phong95/aec-agent-go/kernel"
	"github.com/Phong95/aec-agent-go/kernel/ordered"
)

// subagentManagerDeps is what a subagentManager needs.
type subagentManagerDeps struct {
	agentID string
	// profile is read on every call, so a profile update (new allowlist, limits) applies
	// to the next spawn.
	profile  func() profile.AgentProfile
	bus      events.Bus
	store    persistence.SubagentRunStore
	sessions persistence.SessionStore
	messages persistence.MessageStore
	// host is the agents a child can be (AgentCore's orchestration). Nil = no subagents.
	host SubagentHost
	// runner is the host's runner (AgentCore.UseSubagentRunner); nil, or a nil result,
	// means the child agent's own Chat.
	// (A-105 revision: nil means the turn itself - TurnContext.Execute: the child agent's
	// Chat, or the type's short-lived loop.)
	runner func() subagent.Runner
	// hasSpawnTool says whether this agent has the spawn_subagent tool (its index is
	// shown only then).
	hasSpawnTool func() bool
	// interaction is this agent's interaction router: the parent session's channel to
	// the user, where a child's approval requests go.
	interaction func(sessionID string) toolhub.InteractionHandler
	// hub is the library of subagent types (AgentCore's SubagentHub, A-105 revision). Nil,
	// or a nil result, means no subagent types.
	hub func() *subagenthub.SubagentHub
	// childLoop is what a subagent type's short-lived loop borrows from this agent
	// (model, tools, sandbox, artifacts, conversations).
	childLoop childLoopDeps
	logger    *slog.Logger
}

// subagentTarget is a resolved target: a subagent type (runs inside this agent) or an
// existing agent.
type subagentTarget struct {
	kind subagent.Kind
	// typ is the subagent type (KindType).
	typ subagenttype.Type
	// agent is the target agent (KindAgent).
	agent  *Agent
	access subagent.Access
	// name is the type's name, or the agent's name.
	name string
}

// subagentStopReason is why a running child is being stopped.
type subagentStopReason int

const (
	stopNone subagentStopReason = iota
	stopCancelled
	stopTimeout
	stopBudget
)

// turnBudget is the shared budget of all children of one parent turn. Guarded by the
// manager's mu.
type turnBudget struct {
	parentSessionID string
	// limits are the parent's limits as of the turn's latest spawn.
	limits subagent.Limits
	sem    *subagentSemaphore
	// launched is the children started or queued (counts against MaxRunsPerTurn).
	launched  int
	tokens    int
	toolCalls int
	// firstStartAt is when the turn's first child started (MaxElapsedMsPerTurn counts
	// from here); zero before.
	firstStartAt time.Time
	// active is the children running now.
	active map[*activeSubagentRun]struct{}
	// touchedAt is the last time a child of this turn started or ended.
	touchedAt time.Time
}

// activeSubagentRun is a child running now. run, reason and maxIterations are guarded
// by the manager's mu.
type activeSubagentRun struct {
	run    subagent.Run
	cancel context.CancelFunc
	reason subagentStopReason
	budget *turnBudget
	// maxIterations: the child's loop ran out of iterations (agent-loop:max-iterations
	// for its session).
	maxIterations bool
}

// subagentBinding is the child-side half of a child run: set up before its turn,
// released after.
type subagentBinding struct {
	parent subagent.ParentRef
	access subagent.Access
	// interaction is where the child's approval requests go (the parent's channel to
	// the user).
	interaction toolhub.InteractionHandler
}

// subagentManager is one agent's subagents (A-105), the SubagentManager of core/ts. It
// owns everything about subagents for one agent:
//
//   - as a PARENT: Spawn validates the brief and target, takes a slot of the turn's
//     semaphore, creates the child's session, runs the child turn through the host's
//     runner (or the child's Chat), enforces the turn's shared budget and the child's
//     deadline, and records the run (PersistenceHub.SubagentRuns) and its lifecycle
//     events;
//     Design revision: the target is a subagent TYPE (a short-lived loop inside this
//     agent, subagent_child_loop.go, in a side-branch session of this agent) or an
//     existing AGENT;
//   - as a CHILD: knows which of its sessions are child runs, routes their approvals to
//     the parent's channel to the user, refuses their questions, scopes their tools
//     (read-only by default) and holds the single writer slot.
//
// It hears the child's model responses, tool calls and max-iterations events on the bus
// (pub-sub, never a call from the loop) to account tokens and tool calls while the child
// runs, and the parent's run completions to drop the turn's budget.
//
// A child turn runs on the goroutine of Spawn; the deadline timer and the parent-stop
// hook only cancel its ctx. It is safe for concurrent use. Lock order: a manager never
// calls another manager (the child's) while it holds its own mu.
type subagentManager struct {
	d subagentManagerDeps

	mu             sync.Mutex
	budgets        map[string]*turnBudget
	active         map[string]*activeSubagentRun
	byChildSession map[string]*activeSubagentRun
	unsubscribe    []events.Unsubscribe
	disposed       bool

	// Child side: this agent's sessions that are someone's child runs, and the run that
	// holds the writer slot ("" = free).
	bindings    map[string]subagentBinding
	writerRunID string
}

var _ subagent.Subagents = (*subagentManager)(nil)

func newSubagentManager(d subagentManagerDeps) *subagentManager {
	if d.logger == nil {
		d.logger = slog.Default()
	}
	if nilcheck.IsNil(d.store) {
		d.store = persistence.NewMemorySubagentRunStore()
	}
	if nilcheck.IsNil(d.host) {
		d.host = nil
	}
	m := &subagentManager{
		d:              d,
		budgets:        map[string]*turnBudget{},
		active:         map[string]*activeSubagentRun{},
		byChildSession: map[string]*activeSubagentRun{},
		bindings:       map[string]subagentBinding{},
	}
	m.unsubscribe = []events.Unsubscribe{
		events.Subscribe(d.bus, events.ModelResponded, func(e kernel.Event, p events.ModelRespondedPayload) {
			if p.Usage != nil {
				m.onChildUsage(e.SessionID, *p.Usage)
			}
		}),
		events.Subscribe(d.bus, events.ToolDispatched, func(e kernel.Event, _ events.DispatchStartPayload) {
			m.onChildToolCall(e.SessionID)
		}),
		events.Subscribe(d.bus, events.MaxIterationsReached, func(e kernel.Event, _ events.MaxIterationsPayload) {
			m.mu.Lock()
			if run := m.byChildSession[e.SessionID]; run != nil {
				run.maxIterations = true
			}
			m.mu.Unlock()
		}),
		// A parent turn ended: its children are done (Spawn waits for them), so its budget
		// goes.
		events.Subscribe(d.bus, events.RunCompleted, func(e kernel.Event, p events.RunCompletedPayload) {
			if p.AgentID == d.agentID {
				m.dropIdleBudgets(e.SessionID)
			}
		}),
	}
	return m
}

// ---------------------------------------------------------------------------
// Parent side
// ---------------------------------------------------------------------------

// Running returns how many child runs of this agent run now.
func (m *subagentManager) Running() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.active)
}

// Targets returns the agents this one may delegate to (configured and running).
// Update (A-105 revision): what this agent may delegate to - the enabled subagent types,
// then the allowed running agents. A type the library cannot read is left out.
func (m *subagentManager) Targets(ctx context.Context) []subagent.TargetInfo {
	return m.targets(ctx, m.d.profile())
}

func (m *subagentManager) targets(ctx context.Context, self profile.AgentProfile) []subagent.TargetInfo {
	out := []subagent.TargetInfo{}
	cfg := subagent.ResolveConfig(self.Subagents)
	if hub := m.typeHub(); hub != nil {
		for _, name := range cfg.Types {
			t, ok, err := hub.Get(ctx, name)
			if err != nil || !ok {
				continue
			}
			out = append(out, subagent.TargetInfo{Kind: subagent.KindType, AgentID: t.Name, Name: t.Name, Access: t.Access, Description: t.Description})
		}
	}
	if m.d.host == nil {
		return out
	}
	for _, t := range cfg.Allow {
		a, ok := m.d.host.Get(t.AgentID)
		if !ok || a == nil || a.ID() == m.d.agentID {
			continue
		}
		p := a.Profile()
		out = append(out, subagent.TargetInfo{Kind: subagent.KindAgent, AgentID: p.ID, Name: p.Name, Access: t.Access, Description: firstLine(p.SystemPrompt, 160)})
	}
	return out
}

// typeHub is the library of subagent types, or nil when there is none.
func (m *subagentManager) typeHub() *subagenthub.SubagentHub {
	if m.d.hub == nil {
		return nil
	}
	return m.d.hub()
}

// List returns the child runs started from one of this agent's sessions, oldest first,
// with the live counts of the ones running here.
func (m *subagentManager) List(ctx context.Context, parentSessionID string) ([]subagent.Run, error) {
	runs, err := m.d.store.ListByParentSession(ctx, parentSessionID)
	if err != nil {
		return nil, err
	}
	out := make([]subagent.Run, 0, len(runs))
	for _, r := range runs {
		if r.ParentAgentID == m.d.agentID {
			out = append(out, m.live(r))
		}
	}
	return out, nil
}

// Get returns one child run started by this agent, or nil and a nil error.
func (m *subagentManager) Get(ctx context.Context, runID string) (*subagent.Run, error) {
	r, err := m.d.store.Get(ctx, runID)
	if err != nil || r == nil || r.ParentAgentID != m.d.agentID {
		return nil, err
	}
	out := m.live(*r)
	return &out, nil
}

// Stop stops a running child of this agent. False when it is not running here.
func (m *subagentManager) Stop(runID string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	run := m.active[runID]
	if run == nil {
		return false
	}
	m.abortLocked(run, stopCancelled)
	return true
}

// Spawn runs a child for the parent turn in sc and waits for its report. See
// subagent.Subagents.
func (m *subagentManager) Spawn(ctx context.Context, req subagent.Request, sc subagent.SpawnContext) (subagent.Run, error) {
	m.mu.Lock()
	disposed := m.disposed
	m.mu.Unlock()
	if disposed {
		return subagent.Run{}, subagent.NewError("this agent is shutting down", subagent.CodeNotFound)
	}
	self := m.d.profile()
	cfg := subagent.ResolveConfig(self.Subagents)
	if err := m.refuseNested(ctx, sc.SessionID); err != nil {
		return subagent.Run{}, err
	}
	brief, err := subagent.ValidateRequest(req)
	if err != nil {
		return subagent.Run{}, err
	}
	target, err := m.resolveTarget(ctx, brief.AgentID, cfg, self)
	if err != nil {
		return subagent.Run{}, err
	}
	access := target.access
	// A type works in this agent's sandbox; an agent in its own.
	writerSide := m
	childAgentID := m.d.agentID
	if target.kind == subagent.KindAgent {
		writerSide = target.agent.subagents
		childAgentID = target.agent.ID()
	}

	parentRunID := sc.RunID
	if parentRunID == "" {
		parentRunID = "session:" + sc.SessionID
	}
	started := time.Now()
	run := subagent.Run{
		ID:               kernel.NewID(),
		Kind:             target.kind,
		ParentAgentID:    m.d.agentID,
		ParentSessionID:  sc.SessionID,
		ParentRunID:      parentRunID,
		ParentToolCallID: sc.ToolCallID,
		ChildAgentID:     childAgentID,
		ChildAgentName:   target.name,
		Access:           access,
		Task:             brief.Task,
		Status:           subagent.StatusRunning,
		StartedAt:        kernel.MillisToISO(started.UnixMilli()),
	}
	// Records and events must be written even when the parent turn is being stopped.
	persistCtx := context.WithoutCancel(ctx)

	// One writer per child sandbox: refused now, before anything is queued or recorded.
	// (A-105 revision: a type's writer slot is this agent's - it works in its sandbox.)
	if access == subagent.AccessWrite {
		if !writerSide.tryAcquireWriter(run.ID) {
			msg := `"` + target.name + `" is already running a write-capable task; wait for it, or delegate read-only work`
			if target.kind == subagent.KindType {
				msg = "a write-capable subagent is already working in your sandbox; wait for it, or delegate read-only work"
			}
			return subagent.Run{}, subagent.NewError(msg, subagent.CodeWriteUnavailable)
		}
		defer writerSide.releaseWriter(run.ID)
	}

	m.mu.Lock()
	budget := m.budgetForLocked(parentRunID, sc.SessionID, cfg)
	refused := budgetRefusal(budget, false)
	if refused == "" {
		budget.launched++
		budget.touchedAt = time.Now()
	}
	m.mu.Unlock()
	if refused != "" {
		return m.finishUnstarted(persistCtx, run, started, subagent.StatusBudgetExhausted, refused), nil
	}

	if !budget.sem.acquire(ctx) {
		return m.finishUnstarted(persistCtx, run, started, subagent.StatusCancelled, "the parent turn was stopped before the subagent started"), nil
	}
	defer budget.sem.release()
	m.mu.Lock()
	late := budgetRefusal(budget, true)
	m.mu.Unlock()
	if late != "" {
		return m.finishUnstarted(persistCtx, run, started, subagent.StatusBudgetExhausted, late), nil
	}
	return m.runChild(ctx, run, started, target, brief, sc, budget, cfg), nil
}

// dispose stops every running child and stops listening. Runs in progress record
// themselves as cancelled.
func (m *subagentManager) dispose() {
	m.mu.Lock()
	m.disposed = true
	for _, run := range m.active {
		m.abortLocked(run, stopCancelled)
	}
	unsubscribe := m.unsubscribe
	m.unsubscribe = nil
	m.mu.Unlock()
	for _, off := range unsubscribe {
		off()
	}
}

// runChild runs the child turn of a run that got its slot, and records how it ended.
func (m *subagentManager) runChild(
	ctx context.Context,
	run subagent.Run,
	started time.Time,
	target subagentTarget,
	brief subagent.Request,
	sc subagent.SpawnContext,
	budget *turnBudget,
	cfg subagent.ResolvedConfig,
) subagent.Run {
	persistCtx := context.WithoutCancel(ctx)
	parent := subagent.ParentRef{RunID: run.ParentRunID, AgentID: m.d.agentID, SessionID: sc.SessionID, SubagentRunID: run.ID}
	meta := ordered.NewObject()
	meta.Set("agentId", run.ChildAgentID)
	meta.Set("source", subagent.SessionSource)
	meta.Set("kind", string(target.kind))
	meta.Set("parentRunId", parent.RunID)
	meta.Set("parentSessionId", parent.SessionID)
	meta.Set("parentAgentId", parent.AgentID)
	meta.Set("subagentRunId", run.ID)
	meta.Set("title", target.name+": "+firstLine(brief.Task, 60))
	session, err := m.d.sessions.Create(persistCtx, meta)
	if err != nil {
		return m.finishUnstarted(persistCtx, run, started, subagent.StatusFailed, "could not create the subagent's session: "+err.Error())
	}
	run.ChildSessionID = session.ID

	childCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	active := &activeSubagentRun{run: run.Clone(), cancel: cancel, budget: budget}
	m.mu.Lock()
	m.active[run.ID] = active
	m.byChildSession[session.ID] = active
	budget.active[active] = struct{}{}
	now := time.Now()
	if budget.firstStartAt.IsZero() {
		budget.firstStartAt = now
	}
	budget.touchedAt = now
	remaining := budget.firstStartAt.Add(time.Duration(cfg.MaxElapsedMsPerTurn) * time.Millisecond).Sub(now)
	m.mu.Unlock()

	// The child's channel to the user: approvals go to the parent's session.
	interaction := m.childInteraction(m.parentInteraction(sc.SessionID), target.name, run.ChildAgentID, parent)
	// An agent child learns which of its sessions is a child run (approvals, tool scope); a
	// type's loop is built with both, so nothing needs binding.
	if target.kind == subagent.KindAgent {
		target.agent.subagents.bind(session.ID, subagentBinding{parent: parent, access: run.Access, interaction: interaction})
	}
	m.save(persistCtx, run)
	events.Emit(m.d.bus, events.SubagentSpawned, sc.SessionID, events.SubagentSpawnedPayload{
		AgentID:          m.d.agentID,
		RunID:            run.ID,
		ParentRunID:      run.ParentRunID,
		ParentToolCallID: run.ParentToolCallID,
		ChildAgentID:     run.ChildAgentID,
		ChildSessionID:   session.ID,
		Access:           string(run.Access),
	})

	// The parent turn's stop, the child's deadline (never past the turn's elapsed budget)
	// and Stop(runID) all cancel the child's ctx.
	stopParent := context.AfterFunc(ctx, func() { m.abort(active, stopCancelled) })
	childTimeout := time.Duration(cfg.ChildTimeoutMs) * time.Millisecond
	reasonAtDeadline := stopTimeout
	if childTimeout > remaining {
		reasonAtDeadline = stopBudget
	}
	timer := time.AfterFunc(max(0, min(childTimeout, remaining)), func() { m.abort(active, reasonAtDeadline) })

	prompt := subagent.BuildBrief(brief, m.d.profile().Name, run.Access)
	traceAttributes := map[string]any{
		"source":          subagent.SessionSource,
		"subagentKind":    string(target.kind),
		"parentAgentId":   parent.AgentID,
		"parentSessionId": parent.SessionID,
		"parentRunId":     parent.RunID,
		"subagentRunId":   run.ID,
	}
	if target.kind == subagent.KindType {
		traceAttributes["subagentType"] = target.name
	}
	if run.ParentToolCallID != "" {
		traceAttributes["parentToolCallId"] = run.ParentToolCallID
	}
	sessionID := session.ID
	turn := subagent.TurnContext{
		Kind:            target.kind,
		AgentID:         run.ChildAgentID,
		SessionID:       sessionID,
		Prompt:          prompt,
		Parent:          parent,
		TraceAttributes: traceAttributes,
		Execute: func(ctx context.Context) (subagent.TurnResult, error) {
			if target.kind == subagent.KindType {
				return runTypeTurn(ctx, m.d.childLoop, m.d.profile(), target.typ, typeTurn{
					sessionID: sessionID, prompt: prompt, traceAttributes: traceAttributes, interaction: interaction,
				})
			}
			return chatTurn(ctx, target.agent, sessionID, prompt, traceAttributes)
		},
	}
	result, failure := m.runTurn(childCtx, turn)

	timer.Stop()
	stopParent()
	if target.kind == subagent.KindAgent {
		target.agent.subagents.unbind(session.ID)
	}
	m.mu.Lock()
	delete(m.active, run.ID)
	delete(m.byChildSession, session.ID)
	delete(budget.active, active)
	budget.touchedAt = time.Now()
	// Tokens: the result's total wins when the events missed some (a streamed turn
	// reports none).
	if result != nil {
		reported := result.Usage.InputTokens + result.Usage.OutputTokens
		counted := active.run.Usage.InputTokens + active.run.Usage.OutputTokens
		if reported > counted {
			budget.tokens += reported - counted
			active.run.Usage = cloneTokenUsage(result.Usage)
		}
	}
	run = active.run.Clone()
	reason, maxIterations := active.reason, active.maxIterations
	m.mu.Unlock()

	status := subagentStatusOf(reason, maxIterations, ctx.Err() != nil, result, failure)
	output := ""
	if status == subagent.StatusCompleted && result != nil {
		output = result.Output
	}
	if status != subagent.StatusCompleted && status != subagent.StatusCancelled {
		output = m.lastReport(persistCtx, session.ID)
	}
	var errMsg string
	switch status {
	case subagent.StatusCompleted:
	case subagent.StatusFailed:
		if failure != nil {
			errMsg = subagent.SafeError("the subagent's run failed: " + failure.Error())
		} else {
			errMsg = subagent.SafeError("the model stopped the subagent's reply (refusal or content filter)")
		}
	case subagent.StatusPartial:
		if maxIterations {
			errMsg = "the subagent reached its turn limit (maxIterations)"
		} else {
			errMsg = "the subagent's reply hit the output token limit"
		}
	case subagent.StatusTimedOut:
		errMsg = "the subagent did not finish within " + strconv.Itoa(int(math.Round(float64(cfg.ChildTimeoutMs)/1000))) + "s"
	case subagent.StatusBudgetExhausted:
		errMsg = "the subagent budget of this turn ran out"
	default:
		errMsg = "the subagent was stopped"
	}
	return m.finish(persistCtx, run, started, status, output, errMsg)
}

// runTurn runs the child turn through the host's runner, or the child's own Chat. A
// panic of the runner is a failure of the run (logged redacted), never a crash.
// (Update (A-105 revision): without a host runner, the turn itself -
// TurnContext.Execute.)
func (m *subagentManager) runTurn(ctx context.Context, turn subagent.TurnContext) (result *subagent.TurnResult, failure error) {
	defer func() {
		if rec := recover(); rec != nil {
			m.d.logger.Error("subagent: the runner panicked", "agent", m.d.agentID, "child", turn.AgentID, "panic", redact.Panic(rec))
			result, failure = nil, errors.New("the runner panicked")
		}
	}()
	runner := m.defaultRunner
	if m.d.runner != nil {
		if r := m.d.runner(); r != nil {
			runner = r
		}
	}
	res, err := runner(ctx, turn)
	if err != nil {
		return nil, err
	}
	return &res, nil
}

// defaultRunner is the child agent's own Chat in the child session.
// Update (A-105 revision): the runner without a host runner is the turn itself - the
// child agent's own Chat (chatTurn below), or the type's short-lived loop.
func (m *subagentManager) defaultRunner(ctx context.Context, turn subagent.TurnContext) (subagent.TurnResult, error) {
	if turn.Execute == nil {
		return subagent.TurnResult{}, fmt.Errorf("agent %s: the child turn has nothing to run", turn.AgentID)
	}
	return turn.Execute(ctx)
}

// chatTurn is one turn of an existing agent in its child session.
func chatTurn(ctx context.Context, a *Agent, sessionID, prompt string, traceAttributes map[string]any) (subagent.TurnResult, error) {
	res, err := a.Chat(ctx, prompt, WithChatSession(sessionID), WithChatTraceAttributes(traceAttributes))
	if err != nil {
		// A turn stopped while it waited for its session is a stopped turn, not a failure.
		if ctx.Err() != nil && errors.Is(err, ctx.Err()) {
			return subagent.TurnResult{Aborted: true}, nil
		}
		return subagent.TurnResult{}, err
	}
	return subagent.TurnResult{Output: res.Message.Content, Usage: res.Usage, Aborted: res.Aborted, StopReason: res.StopReason}, nil
}

// subagentStatusOf decides how a child run ended.
func subagentStatusOf(reason subagentStopReason, maxIterations, parentStopped bool, result *subagent.TurnResult, failure error) subagent.Status {
	// A turn that ended on its own keeps its outcome, even when a stop came in as it
	// finished (its last response crossed the budget, the deadline fired during the final
	// save).
	endedOnItsOwn := failure == nil && result != nil && !result.Aborted
	if !endedOnItsOwn {
		switch {
		case reason == stopBudget:
			return subagent.StatusBudgetExhausted
		case reason == stopTimeout:
			return subagent.StatusTimedOut
		case reason == stopCancelled || parentStopped:
			return subagent.StatusCancelled
		}
	}
	switch {
	case failure != nil || result == nil:
		return subagent.StatusFailed
	case result.Aborted:
		return subagent.StatusCancelled
	case result.StopReason == kernel.StopError:
		return subagent.StatusFailed
	case result.StopReason == kernel.StopMaxTokens || maxIterations:
		return subagent.StatusPartial
	}
	return subagent.StatusCompleted
}

// lastReport is the child's last non-empty reply in its session - what a partial run
// hands back.
func (m *subagentManager) lastReport(ctx context.Context, sessionID string) string {
	msgs, err := m.d.messages.Get(ctx, sessionID)
	if err != nil {
		return ""
	}
	for i := len(msgs) - 1; i >= 0; i-- {
		msg := msgs[i]
		if msg.Role == kernel.RoleAssistant && len(msg.ToolResults) == 0 && jsstr.Trim(msg.Content) != "" {
			return msg.Content
		}
	}
	return ""
}

// finish records how a run ended and emits its lifecycle event.
func (m *subagentManager) finish(ctx context.Context, run subagent.Run, started time.Time, status subagent.Status, output, errMsg string) subagent.Run {
	finished := time.Now()
	result := run.Clone()
	result.Status = status
	result.Output = ""
	if jsstr.Trim(output) != "" {
		result.Output = subagent.SanitizeChildOutput(output)
	}
	result.Error = errMsg
	result.FinishedAt = kernel.MillisToISO(finished.UnixMilli())
	duration := max(0, finished.UnixMilli()-started.UnixMilli())
	result.DurationMs = &duration
	m.save(ctx, result)
	var kind events.Kind[events.SubagentFinishedPayload]
	switch status {
	case subagent.StatusCompleted, subagent.StatusPartial:
		kind = events.SubagentCompleted
	case subagent.StatusCancelled:
		kind = events.SubagentCancelled
	case subagent.StatusFailed, subagent.StatusTimedOut, subagent.StatusBudgetExhausted, subagent.StatusRunning:
		kind = events.SubagentFailed
	}
	events.Emit(m.d.bus, kind, result.ParentSessionID, events.SubagentFinishedPayload{
		AgentID:          m.d.agentID,
		RunID:            result.ID,
		ParentRunID:      result.ParentRunID,
		ParentToolCallID: result.ParentToolCallID,
		ChildAgentID:     result.ChildAgentID,
		ChildSessionID:   result.ChildSessionID,
		Status:           string(status),
		Usage:            cloneTokenUsage(result.Usage),
		ToolCalls:        result.ToolCalls,
		DurationMs:       duration,
		Error:            result.Error,
	})
	return result
}

// finishUnstarted ends a run before its child turn started.
func (m *subagentManager) finishUnstarted(ctx context.Context, run subagent.Run, started time.Time, status subagent.Status, reason string) subagent.Run {
	return m.finish(ctx, run, started, status, "", subagent.SafeError(reason))
}

func (m *subagentManager) save(ctx context.Context, run subagent.Run) {
	if err := m.d.store.Save(ctx, run); err != nil {
		m.d.logger.Warn("subagent: could not save run", "agent", m.d.agentID, "run", run.ID, "error", err)
	}
}

// live returns a stored record with the live token and tool-call counts while it runs
// here.
func (m *subagentManager) live(run subagent.Run) subagent.Run {
	m.mu.Lock()
	defer m.mu.Unlock()
	if a := m.active[run.ID]; a != nil {
		run.Usage = cloneTokenUsage(a.run.Usage)
		run.ToolCalls = a.run.ToolCalls
	}
	return run
}

// refuseNested refuses a spawn from a session that is itself a child run (V1 nesting
// depth is 1).
func (m *subagentManager) refuseNested(ctx context.Context, sessionID string) error {
	m.mu.Lock()
	_, nested := m.bindings[sessionID]
	m.mu.Unlock()
	if !nested {
		if s, err := m.d.sessions.Get(ctx, sessionID); err == nil {
			nested = subagent.IsSession(s)
		}
	}
	if nested {
		return subagent.NewError("you are running as a subagent and cannot delegate further "+emDash+" do the work yourself", subagent.CodeDepthExceeded)
	}
	return nil
}

// resolveTarget finds the allowed, running agent ref names (its id or name) and the
// access it has.
// Update (A-105 revision): the target ref names - an enabled subagent type (by name)
// first, then an allowed running agent (by id or name). Anything that can't run is an
// *Error.
func (m *subagentManager) resolveTarget(ctx context.Context, ref string, cfg subagent.ResolvedConfig, self profile.AgentProfile) (subagentTarget, error) {
	lower := strings.ToLower(ref)
	if ref == self.ID || lower == strings.ToLower(self.Name) {
		return subagentTarget{}, subagent.NewError("an agent cannot delegate to itself", subagent.CodeDepthExceeded)
	}
	hub := m.typeHub()
	if (len(cfg.Types) == 0 || hub == nil) && (len(cfg.Allow) == 0 || m.d.host == nil) {
		return subagentTarget{}, subagent.NewError("you have no subagents configured "+emDash+" do the work yourself", subagent.CodeNotAllowed)
	}

	if hub != nil && (slices.Contains(cfg.Types, ref) || slices.Contains(cfg.Types, lower)) {
		name := lower
		if slices.Contains(cfg.Types, ref) {
			name = ref
		}
		t, ok, err := hub.Get(ctx, name)
		if err != nil {
			return subagentTarget{}, err
		}
		if !ok {
			return subagentTarget{}, subagent.NewError(`the subagent type "`+ref+`" no longer exists`, subagent.CodeNotFound)
		}
		return subagentTarget{kind: subagent.KindType, typ: t, access: t.Access, name: t.Name}, nil
	}

	var (
		match                *Agent
		access               subagent.Access
		configuredButStopped bool
	)
	if m.d.host != nil {
		for _, t := range cfg.Allow {
			a, ok := m.d.host.Get(t.AgentID)
			running := ok && a != nil
			named := t.AgentID == ref || (running && strings.ToLower(a.Profile().Name) == lower)
			if !named {
				continue
			}
			if !running {
				configuredButStopped = true
				continue
			}
			match, access = a, t.Access
			break
		}
	}
	if match == nil {
		if configuredButStopped {
			return subagentTarget{}, subagent.NewError(`the agent "`+ref+`" is not running`, subagent.CodeNotFound)
		}
		var allowed []string
		for _, t := range m.targets(ctx, self) {
			if t.Kind == subagent.KindType {
				allowed = append(allowed, `"`+t.Name+`"`)
			} else {
				allowed = append(allowed, `"`+t.Name+`" (`+t.AgentID+`)`)
			}
		}
		msg := `"` + ref + `" is not one of your subagents`
		if len(allowed) > 0 {
			msg += " " + emDash + " you may delegate to: " + strings.Join(allowed, ", ")
		}
		return subagentTarget{}, subagent.NewError(msg, subagent.CodeNotAllowed)
	}
	p := match.Profile()
	if access == subagent.AccessWrite && p.Sandbox.Mode != sandboxhub.SelectionOwn {
		return subagentTarget{}, subagent.NewError(
			`"`+p.Name+`" is set up for write access but has no sandbox of its own `+emDash+` a writing subagent must not share a workspace`,
			subagent.CodeWriteUnavailable,
		)
	}
	return subagentTarget{kind: subagent.KindAgent, agent: match, access: access, name: p.Name}, nil
}

// parentInteraction is the parent session's channel to the user (the agent's
// interaction router), or nil when there is none.
func (m *subagentManager) parentInteraction(sessionID string) toolhub.InteractionHandler {
	if m.d.interaction == nil {
		return nil
	}
	h := m.d.interaction(sessionID)
	if nilcheck.IsNil(h) {
		return nil
	}
	return h
}

// childInteraction is the child's channel to the user: approvals go to the parent's
// (shown in the parent's chat), questions are refused.
func (m *subagentManager) childInteraction(parentHandler toolhub.InteractionHandler, childName, childAgentID string, parent subagent.ParentRef) toolhub.InteractionHandler {
	return subagentInteraction{parentHandler: parentHandler, childID: childAgentID, childName: childName, parent: parent}
}

// subagentInteraction is the InteractionHandler of a child run's session.
type subagentInteraction struct {
	parentHandler toolhub.InteractionHandler
	childID       string
	childName     string
	parent        subagent.ParentRef
}

// errSubagentAsk is what a child gets when it asks the user something.
var errSubagentAsk = errors.New("you are running as a subagent and cannot ask the user " + emDash + " make a reasonable assumption and state it in your report")

// AskUser refuses: a child never talks to the user.
func (subagentInteraction) AskUser(context.Context, toolhub.ClarifyRequest) (toolhub.ClarifyResponse, error) {
	return toolhub.ClarifyResponse{}, errSubagentAsk
}

// RequestApproval asks the parent's channel to the user, in the parent's session, with
// the child named in the request; without one the tool is denied.
func (i subagentInteraction) RequestApproval(ctx context.Context, req toolhub.ApprovalRequest) (toolhub.ApprovalDecision, error) {
	if i.parentHandler == nil {
		reason := "no interactive user is available to approve a subagent's action"
		return toolhub.ApprovalDecision{Approved: false, Reason: &reason}, nil
	}
	req.Subagent = &toolhub.ApprovalSubagent{AgentID: i.childID, AgentName: i.childName, SessionID: req.SessionID, RunID: i.parent.SubagentRunID}
	req.SessionID = i.parent.SessionID
	req.AgentID = i.parent.AgentID
	return i.parentHandler.RequestApproval(ctx, req)
}

// ---------------------------------------------------------------------------
// Budgets
// ---------------------------------------------------------------------------

// budgetForLocked returns the budget of the parent turn key, made on first use. The
// caller holds mu.
func (m *subagentManager) budgetForLocked(key, parentSessionID string, cfg subagent.ResolvedConfig) *turnBudget {
	m.pruneBudgetsLocked(cfg)
	b := m.budgets[key]
	if b == nil {
		b = &turnBudget{
			parentSessionID: parentSessionID,
			sem:             newSubagentSemaphore(cfg.MaxConcurrent),
			active:          map[*activeSubagentRun]struct{}{},
			touchedAt:       time.Now(),
		}
		m.budgets[key] = b
	}
	b.limits = cfg.Limits
	return b
}

// budgetRefusal says why no further child may start in this turn, or "". started: the
// run already counts in launched. The caller holds mu.
func budgetRefusal(b *turnBudget, started bool) string {
	l := b.limits
	switch {
	case !started && b.launched >= l.MaxRunsPerTurn:
		return "this turn already started " + strconv.Itoa(l.MaxRunsPerTurn) + " subagents (maxRunsPerTurn)"
	case b.tokens >= l.MaxTokensPerTurn:
		return "the subagents of this turn used their " + strconv.Itoa(l.MaxTokensPerTurn) + " tokens (maxTokensPerTurn)"
	case b.toolCalls >= l.MaxToolCallsPerTurn:
		return "the subagents of this turn made their " + strconv.Itoa(l.MaxToolCallsPerTurn) + " tool calls (maxToolCallsPerTurn)"
	case !b.firstStartAt.IsZero() && time.Since(b.firstStartAt) >= time.Duration(l.MaxElapsedMsPerTurn)*time.Millisecond:
		return "the subagents of this turn used their " + strconv.Itoa(int(math.Round(float64(l.MaxElapsedMsPerTurn)/1000))) + "s (maxElapsedMsPerTurn)"
	}
	return ""
}

// onChildUsage accounts a child's model response.
func (m *subagentManager) onChildUsage(sessionID string, usage kernel.TokenUsage) {
	m.mu.Lock()
	defer m.mu.Unlock()
	run := m.byChildSession[sessionID]
	if run == nil {
		return
	}
	u := &run.run.Usage
	u.InputTokens += usage.InputTokens
	u.OutputTokens += usage.OutputTokens
	u.CacheReadTokens = addOptional(u.CacheReadTokens, usage.CacheReadTokens)
	u.CacheWriteTokens = addOptional(u.CacheWriteTokens, usage.CacheWriteTokens)
	run.budget.tokens += usage.InputTokens + usage.OutputTokens
	m.enforceBudgetLocked(run.budget)
}

// onChildToolCall accounts a child's tool call.
func (m *subagentManager) onChildToolCall(sessionID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	run := m.byChildSession[sessionID]
	if run == nil {
		return
	}
	run.run.ToolCalls++
	run.budget.toolCalls++
	m.enforceBudgetLocked(run.budget)
}

// addOptional adds two optional counters: nil when both are nil or zero, like the
// cache counters of core/ts.
func addOptional(a, b *int) *int {
	av, bv := 0, 0
	if a != nil {
		av = *a
	}
	if b != nil {
		bv = *b
	}
	if av == 0 && bv == 0 {
		return a
	}
	sum := av + bv
	return &sum
}

// enforceBudgetLocked stops every running child of the turn once it is over its token
// or tool-call budget. The caller holds mu.
func (m *subagentManager) enforceBudgetLocked(b *turnBudget) {
	if b.tokens <= b.limits.MaxTokensPerTurn && b.toolCalls <= b.limits.MaxToolCallsPerTurn {
		return
	}
	for run := range b.active {
		m.abortLocked(run, stopBudget)
	}
}

func (m *subagentManager) abort(run *activeSubagentRun, reason subagentStopReason) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.abortLocked(run, reason)
}

// abortLocked records the first stop reason and cancels the child's ctx. The caller
// holds mu; a cancel runs no callback inline (context.AfterFunc runs its own goroutine).
func (m *subagentManager) abortLocked(run *activeSubagentRun, reason subagentStopReason) {
	if run.reason == stopNone {
		run.reason = reason
	}
	run.cancel()
}

// dropIdleBudgets drops the idle budgets of a parent session's turns.
func (m *subagentManager) dropIdleBudgets(sessionID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for key, b := range m.budgets {
		if b.parentSessionID == sessionID && len(b.active) == 0 && b.sem.busy() == 0 {
			delete(m.budgets, key)
		}
	}
}

// pruneBudgetsLocked drops the budgets of turns that never reported their end (a tool
// job's call) once idle long enough. The caller holds mu.
func (m *subagentManager) pruneBudgetsLocked(cfg subagent.ResolvedConfig) {
	cutoff := time.Now().Add(-2 * time.Duration(cfg.MaxElapsedMsPerTurn) * time.Millisecond)
	for key, b := range m.budgets {
		if len(b.active) == 0 && b.sem.busy() == 0 && b.touchedAt.Before(cutoff) {
			delete(m.budgets, key)
		}
	}
}

// ---------------------------------------------------------------------------
// Child side - hooks the agent loop and context manager call for this agent's sessions,
// and the bookkeeping a parent's manager does on this agent
// ---------------------------------------------------------------------------

// bind marks sessionID as a child session for one run.
func (m *subagentManager) bind(sessionID string, b subagentBinding) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.bindings[sessionID] = b
}

// unbind is called when the run ended.
func (m *subagentManager) unbind(sessionID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.bindings, sessionID)
}

// tryAcquireWriter takes this agent's single writer slot; false when a write-capable
// child already runs.
func (m *subagentManager) tryAcquireWriter(runID string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.writerRunID != "" && m.writerRunID != runID {
		return false
	}
	m.writerRunID = runID
	return true
}

func (m *subagentManager) releaseWriter(runID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.writerRunID == runID {
		m.writerRunID = ""
	}
}

// interactionFor is a child session's channel to the user (the parent's, for approvals
// only); nil for other sessions.
func (m *subagentManager) interactionFor(sessionID string) toolhub.InteractionHandler {
	m.mu.Lock()
	defer m.mu.Unlock()
	b, ok := m.bindings[sessionID]
	if !ok {
		return nil
	}
	return b.interaction
}

// toolScope is a child session's tool scope (read-only by default); nil for other
// sessions.
func (m *subagentManager) toolScope(sessionID string) func(toolhub.Definition) bool {
	m.mu.Lock()
	b, ok := m.bindings[sessionID]
	m.mu.Unlock()
	if !ok {
		return nil
	}
	return childToolScope(b.access)
}

// childToolScope is the tools a child with access may use.
func childToolScope(access subagent.Access) func(toolhub.Definition) bool {
	return func(d toolhub.Definition) bool { return subagent.ChildMayUse(access, d.Name, string(d.Risk)) }
}

// buildIndex is <available_subagents> for the system prompt: the agents this one may
// delegate to. None in a child run's session, or when the agent lacks spawn_subagent.
// (Update (A-105 revision): the subagent types too.)
func (m *subagentManager) buildIndex(ctx context.Context, session kernel.Session) (string, bool) {
	m.mu.Lock()
	_, bound := m.bindings[session.ID]
	m.mu.Unlock()
	if bound || subagent.IsSession(&session) || m.d.hasSpawnTool == nil || !m.d.hasSpawnTool() {
		return "", false
	}
	targets := m.Targets(ctx)
	if len(targets) == 0 {
		return "", false
	}
	lines := make([]string, 0, len(targets))
	for _, t := range targets {
		var line string
		if t.Kind == subagent.KindType {
			line = "- " + t.Name + " (subagent type, " + string(t.Access) + "): " + t.Description
		} else {
			line = "- " + t.Name + " (agent, id: " + t.AgentID + ", " + string(t.Access) + ")"
			if t.Description != "" {
				line += ": " + t.Description
			}
		}
		lines = append(lines, line)
	}
	return "<available_subagents>\n" +
		"Subagents you can delegate a bounded task to with spawn_subagent (name the subagent type, or the agent's id). " +
		"Each starts fresh: give it everything it needs in the task. A subagent type is a specialist that runs with your " +
		"tools and workspace; an agent is a separate agent with its own tools, memory and workspace. " +
		"Delegate only independent work that benefits from a separate specialist or context " + emDash + " call spawn_subagent several times " +
		"in one response for independent tasks (they run in parallel), and one after another when a task needs an earlier result.\n" +
		strings.Join(lines, "\n") + "\n</available_subagents>", true
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// subagentSemaphore is at most size holders at once. A waiter whose ctx ends leaves
// (acquire returns false), so a stopped parent turn never starts a queued child. Unlike
// the core/ts semaphore it does not promise arrival order.
type subagentSemaphore struct {
	slots   chan struct{}
	waiting atomic.Int32
}

func newSubagentSemaphore(size int) *subagentSemaphore {
	return &subagentSemaphore{slots: make(chan struct{}, max(1, size))}
}

// acquire takes a slot; false when ctx is already done or ends while it waits.
func (s *subagentSemaphore) acquire(ctx context.Context) bool {
	if ctx.Err() != nil {
		return false
	}
	select {
	case s.slots <- struct{}{}:
		return true
	default:
	}
	s.waiting.Add(1)
	defer s.waiting.Add(-1)
	select {
	case s.slots <- struct{}{}:
		return true
	case <-ctx.Done():
		return false
	}
}

func (s *subagentSemaphore) release() { <-s.slots }

// busy is the holders plus the waiters.
func (s *subagentSemaphore) busy() int { return len(s.slots) + int(s.waiting.Load()) }

// firstLine is the first non-empty line of text, trimmed and cut to max characters.
func firstLine(text string, maxChars int) string {
	line := ""
	for _, l := range strings.Split(text, "\n") {
		if t := jsstr.Trim(l); t != "" {
			line = t
			break
		}
	}
	if kernel.UTF16Len(line) > maxChars {
		return jsstr.TruncateUTF16(line, maxChars) + "…"
	}
	return line
}

// cloneTokenUsage is a deep copy of u (its cache counters are pointers).
func cloneTokenUsage(u kernel.TokenUsage) kernel.TokenUsage {
	if u.CacheReadTokens != nil {
		v := *u.CacheReadTokens
		u.CacheReadTokens = &v
	}
	if u.CacheWriteTokens != nil {
		v := *u.CacheWriteTokens
		u.CacheWriteTokens = &v
	}
	return u
}
