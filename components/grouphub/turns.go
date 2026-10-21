package grouphub

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"math"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Phong95/aec-agent-go/components/agent"
	"github.com/Phong95/aec-agent-go/components/agent/subagent"
	"github.com/Phong95/aec-agent-go/components/events"
	"github.com/Phong95/aec-agent-go/components/grouphub/group"
	"github.com/Phong95/aec-agent-go/components/persistence"
	"github.com/Phong95/aec-agent-go/components/sandboxhub"
	"github.com/Phong95/aec-agent-go/internal/jsonx"
	"github.com/Phong95/aec-agent-go/internal/jsstr"
	"github.com/Phong95/aec-agent-go/internal/redact"
	"github.com/Phong95/aec-agent-go/kernel"
	"github.com/Phong95/aec-agent-go/kernel/ordered"
)

// spawnTool is the tool a lead hands work out with.
const spawnTool = "spawn_subagent"

// turnsDeps is what the group turns need.
type turnsDeps struct {
	bus      events.Bus
	th       *thread
	sessions func() persistence.SessionStore
	agents   func() AgentHost
	// runner is the host's runner (AgentCore.UseGroupRunner); nil, or a nil result, means
	// the member's own ChatStream (MemberTurnContext.Execute).
	runner func() MemberRunner
	logger *slog.Logger
}

// memberStop is why a member's own run was stopped.
type memberStop int

const (
	memberRunning memberStop = iota
	memberCancelled
	memberTimeout
)

// turnStop is why the whole turn was stopped.
type turnStop int

const (
	turnRunning turnStop = iota
	turnStopped
	turnLimited
)

// directRun is a member answering directly: its run's cancel and why it was stopped.
type directRun struct {
	cancel context.CancelFunc
	reason memberStop
}

// spawnedRun is a member the lead handed work to: the child run (stopped through the
// lead's subagents).
type spawnedRun struct {
	runID  string
	leadID string
}

// activeTurn is a group turn running in one group session. The fields below cancel are
// guarded by the turns' mu.
type activeTurn struct {
	id        string
	group     group.Group
	sessionID string
	limits    group.Limits
	// cancel stops every run of the turn (its ctx is handed down the call chain).
	cancel context.CancelFunc
	// cleanup stops the turn's deadline and its link to the caller's ctx.
	cleanup func()
	// done is closed when the turn has ended.
	done chan struct{}
	// settling counts the hand-offs settling after their child run ended (the turn's end
	// waits for them).
	settling sync.WaitGroup

	reason    turnStop
	limitNote string
	direct    map[string]*directRun
	spawned   map[string]spawnedRun
	// memberSessions are the member sessions of the turn (their model responses count
	// against its budget).
	memberSessions map[string]bool
	usage          kernel.TokenUsage
	// added are the message ids this turn added, in order.
	added         []string
	presence      map[string]events.GroupPresencePayload
	presenceOrder []string
}

// memberTurnOptions is why and how a member runs.
type memberTurnOptions struct {
	role    MemberRole
	roundID string
	missing []MissingMember
	retry   *group.Message
}

// preparedMember is a member ready to run: told what, and where its answer goes.
type preparedMember struct {
	agentID       string
	agent         *agent.Agent
	memberSession string
	prompt        string
	writer        *answerWriter
	options       memberTurnOptions
}

// turns is the group turns (A-109, A-112, A-115) - it runs one user message through a
// group:
//   - one member answers alone; or several answer side by side as one round and the lead
//     sums up; or the lead answers and hands parts to members through spawn_subagent
//     (A-105, under a spawn scope: the members see the thread, work in their own member
//     sessions, stream into the thread, and need no extra approval), then sums up;
//   - guard rails: members per round, a token budget and a deadline for the whole turn, a
//     deadline per member; hand-off depth is A-105's (a member cannot delegate further);
//   - stop the whole turn or one member; one member failing does not fail the round, and
//     the summary says what is missing; retry one member's answer.
//
// One group turn runs per group session at a time. It implements subagent.Scopes: a
// lead's session has a spawn scope while it leads a turn. It is safe for concurrent use.
type turns struct {
	d turnsDeps

	mu     sync.Mutex
	active map[string]*activeTurn
	// scopes is lead session -> its spawn scope while it leads a turn.
	scopes map[string]*subagent.SpawnScope
	// handoffWriters are the hand-off answers by child run id, until the run's end
	// settles them.
	handoffWriters map[string]*answerWriter
	unsubscribe    []events.Unsubscribe

	// sessionsMu guards memberSessions and serializes their creation.
	sessionsMu sync.Mutex
	// memberSessions is "<groupSessionID>\x00<agentID>" -> the member's session id.
	memberSessions map[string]string
}

var _ subagent.Scopes = (*turns)(nil)

func newTurns(d turnsDeps) *turns {
	t := &turns{
		d:              d,
		active:         map[string]*activeTurn{},
		scopes:         map[string]*subagent.SpawnScope{},
		handoffWriters: map[string]*answerWriter{},
		memberSessions: map[string]string{},
	}
	// The turn's budget counts every model response in its member sessions.
	t.unsubscribe = []events.Unsubscribe{
		events.Subscribe(d.bus, events.ModelResponded, func(e kernel.Event, p events.ModelRespondedPayload) {
			if p.Usage != nil {
				t.onUsage(e.SessionID, *p.Usage)
			}
		}),
	}
	return t
}

func (t *turns) onUsage(sessionID string, usage kernel.TokenUsage) {
	t.mu.Lock()
	var over []*activeTurn
	for _, turn := range t.active {
		if !turn.memberSessions[sessionID] {
			continue
		}
		turn.usage = addUsage(turn.usage, usage)
		if turn.usage.InputTokens+turn.usage.OutputTokens > turn.limits.MaxTokensPerTurn {
			over = append(over, turn)
		}
	}
	t.mu.Unlock()
	for _, turn := range over {
		t.limit(turn, "Stopped: this turn used its "+thousands(turn.limits.MaxTokensPerTurn)+" tokens (maxTokensPerTurn).")
	}
}

// Get is subagent.Scopes: a lead session's spawn scope while it leads a group turn.
func (t *turns) Get(sessionID string) *subagent.SpawnScope {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.scopes[sessionID]
}

func (t *turns) isBusy(sessionID string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.active[sessionID] != nil
}

// running returns the running turns (by session id), with who is doing what.
func (t *turns) running() []RunningTurn {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := []RunningTurn{}
	for _, id := range slices.Sorted(maps.Keys(t.active)) {
		turn := t.active[id]
		out = append(out, RunningTurn{GroupID: turn.group.ID, SessionID: turn.sessionID, TurnID: turn.id, Presence: presenceListLocked(turn)})
	}
	return out
}

// presenceOf is what each member of the session's running turn is doing (empty when
// idle).
func (t *turns) presenceOf(sessionID string) []events.GroupPresencePayload {
	t.mu.Lock()
	defer t.mu.Unlock()
	turn := t.active[sessionID]
	if turn == nil {
		return []events.GroupPresencePayload{}
	}
	return presenceListLocked(turn)
}

func presenceListLocked(turn *activeTurn) []events.GroupPresencePayload {
	out := make([]events.GroupPresencePayload, 0, len(turn.presenceOrder))
	for _, id := range turn.presenceOrder {
		out = append(out, turn.presence[id])
	}
	return out
}

// send runs one user message through the group. Refusals decided before anything runs
// are *group.Error.
func (t *turns) send(ctx context.Context, g group.Group, session kernel.Session, req SendRequest) (TurnResult, error) {
	text := jsstr.Trim(req.Text)
	if text == "" {
		return TurnResult{}, group.NewError("the message is empty", group.CodeInvalidArgument)
	}
	if n := kernel.UTF16Len(text); n > group.MaxMessageChars {
		return TurnResult{}, group.NewError("the message is "+strconv.Itoa(n)+" characters; keep it under "+strconv.Itoa(group.MaxMessageChars), group.CodeInvalidArgument)
	}
	host, err := t.requireReady(g)
	if err != nil {
		return TurnResult{}, err
	}
	turn, turnCtx, err := t.begin(ctx, g, session.ID)
	if err != nil {
		return TurnResult{}, err
	}
	persist := context.WithoutCancel(ctx)
	var members []MemberRef
	for _, id := range g.Members {
		if a, ok := host.Get(id); ok {
			members = append(members, MemberRef{ID: id, Name: a.Profile().Name})
		}
	}
	rec := ResolveRecipients(RecipientInput{Text: text, To: req.To, Members: members, LeadID: g.LeadID, ReplyMode: g.ReplyMode, Muted: g.Muted})
	run := func() error {
		user, err := t.d.th.append(persist, group.Message{SessionID: session.ID, GroupID: g.ID, Kind: group.KindUser, Text: text, To: rec.Addressed})
		if err != nil {
			return err
		}
		t.addMessage(turn, user.ID)
		if err := t.titleFrom(persist, session, text); err != nil {
			return err
		}
		events.Emit(t.d.bus, events.GroupTurnStarted, session.ID, events.GroupTurnStartedPayload{GroupID: g.ID, TurnID: turn.id, Plan: rec.Plan.Kind})
		return t.runPlan(turnCtx, turn, rec.Plan)
	}
	if err := run(); err != nil {
		_, _ = t.end(persist, turn, group.Plan{Kind: group.PlanNone, Reason: "failed"})
		return TurnResult{}, err
	}
	return t.end(persist, turn, rec.Plan)
}

// retry runs a member's answer again, in place. Refusals decided before anything runs
// are *group.Error.
func (t *turns) retry(ctx context.Context, g group.Group, session kernel.Session, m group.Message) (TurnResult, error) {
	if m.Kind != group.KindAgent || m.AgentID == "" {
		return TurnResult{}, group.NewError("only a member's answer can be retried", group.CodeInvalidArgument)
	}
	if m.Status == group.StatusRunning {
		return TurnResult{}, group.NewError("that answer is still being written", group.CodeBusy)
	}
	host, err := t.requireReady(g)
	if err != nil {
		return TurnResult{}, err
	}
	if _, ok := host.Get(m.AgentID); !ok || !slices.Contains(g.Members, m.AgentID) {
		return TurnResult{}, group.NewError(`"`+m.AgentID+`" is not a running member of this group`, group.CodeUnknownAgent)
	}
	turn, turnCtx, err := t.begin(ctx, g, session.ID)
	if err != nil {
		return TurnResult{}, err
	}
	persist := context.WithoutCancel(ctx)
	plan := group.Plan{Kind: group.PlanSingle, AgentID: m.AgentID}
	events.Emit(t.d.bus, events.GroupTurnStarted, session.ID, events.GroupTurnStartedPayload{GroupID: g.ID, TurnID: turn.id, Plan: plan.Kind})
	role := RoleRetry
	if m.Summary {
		role = RoleSummary
	}
	if err := t.memberTurn(turnCtx, turn, m.AgentID, memberTurnOptions{role: role, retry: &m, roundID: m.RoundID}); err != nil {
		_, _ = t.end(persist, turn, plan)
		return TurnResult{}, err
	}
	return t.end(persist, turn, plan)
}

// stop stops the session's running turn, or (with agentID) one member of it. False when
// nothing ran.
func (t *turns) stop(sessionID, agentID string) bool {
	t.mu.Lock()
	turn := t.active[sessionID]
	if turn == nil {
		t.mu.Unlock()
		return false
	}
	if agentID == "" {
		if turn.reason == turnRunning {
			turn.reason = turnStopped
		}
		t.mu.Unlock()
		turn.cancel()
		return true
	}
	if direct := turn.direct[agentID]; direct != nil {
		if direct.reason == memberRunning {
			direct.reason = memberCancelled
		}
		t.mu.Unlock()
		direct.cancel()
		return true
	}
	child, ok := turn.spawned[agentID]
	t.mu.Unlock()
	if !ok {
		return false
	}
	lead, ok := t.agent(child.leadID)
	return ok && lead.Subagents().Stop(child.runID)
}

// idle waits until the session's running turn (if any) has ended, or ctx ends.
func (t *turns) idle(ctx context.Context, sessionID string) error {
	t.mu.Lock()
	turn := t.active[sessionID]
	t.mu.Unlock()
	if turn == nil {
		return nil
	}
	select {
	case <-turn.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// dispose stops every running turn and stops listening.
func (t *turns) dispose() {
	t.mu.Lock()
	running := slices.Collect(maps.Values(t.active))
	for _, turn := range running {
		if turn.reason == turnRunning {
			turn.reason = turnStopped
		}
	}
	unsubscribe := t.unsubscribe
	t.unsubscribe = nil
	t.mu.Unlock()
	for _, turn := range running {
		turn.cancel()
	}
	for _, off := range unsubscribe {
		off()
	}
}

// forgetSession forgets a deleted group session (its member sessions are deleted by the
// hub).
func (t *turns) forgetSession(sessionID string) {
	t.sessionsMu.Lock()
	defer t.sessionsMu.Unlock()
	for key := range t.memberSessions {
		if strings.HasPrefix(key, sessionID+"\x00") {
			delete(t.memberSessions, key)
		}
	}
}

// memberSession is the member's private session in a group session, created on first
// use.
func (t *turns) memberSession(ctx context.Context, g group.Group, groupSessionID, agentID string) (string, error) {
	key := groupSessionID + "\x00" + agentID
	t.sessionsMu.Lock()
	defer t.sessionsMu.Unlock()
	if id, ok := t.memberSessions[key]; ok {
		return id, nil
	}
	sessions := t.d.sessions()
	filter := ordered.NewObject()
	filter.Set("source", MemberSessionSource)
	filter.Set("groupSessionId", groupSessionID)
	filter.Set("agentId", agentID)
	existing, err := sessions.List(ctx, kernel.SessionFilter{Metadata: filter})
	if err != nil {
		return "", err
	}
	if len(existing) > 0 {
		t.memberSessions[key] = existing[0].ID
		return existing[0].ID, nil
	}
	meta := ordered.NewObject()
	meta.Set("agentId", agentID)
	meta.Set("source", MemberSessionSource)
	meta.Set("groupId", g.ID)
	meta.Set("groupSessionId", groupSessionID)
	meta.Set("title", "Group: "+g.Name)
	s, err := sessions.Create(ctx, meta)
	if err != nil {
		return "", err
	}
	t.memberSessions[key] = s.ID
	return s.ID, nil
}

// ---------------------------------------------------------------------------
// A turn's life
// ---------------------------------------------------------------------------

func (t *turns) requireReady(g group.Group) (AgentHost, error) {
	host := t.host()
	if host == nil {
		return nil, group.NewError("no agents are running in this host", group.CodeUnknownAgent)
	}
	running := 0
	for _, id := range g.Members {
		if _, ok := host.Get(id); ok {
			running++
		}
	}
	if running < 2 {
		return nil, group.NewError("the group needs at least 2 running members", group.CodeInvalidArgument)
	}
	if _, ok := host.Get(g.LeadID); !ok {
		return nil, group.NewError(`the lead "`+g.LeadID+`" is not running`, group.CodeUnknownAgent)
	}
	return host, nil
}

// begin registers a turn in the session and returns it with its ctx: cancelled by stop,
// a guard rail, or the caller's ctx ending.
func (t *turns) begin(ctx context.Context, g group.Group, sessionID string) (*activeTurn, context.Context, error) {
	limits := group.ResolveLimits(g.Limits)
	turnCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	turn := &activeTurn{
		id:             kernel.NewID(),
		group:          g.Clone(),
		sessionID:      sessionID,
		limits:         limits,
		cancel:         cancel,
		done:           make(chan struct{}),
		direct:         map[string]*directRun{},
		spawned:        map[string]spawnedRun{},
		memberSessions: map[string]bool{},
		presence:       map[string]events.GroupPresencePayload{},
	}
	t.mu.Lock()
	if t.active[sessionID] != nil {
		t.mu.Unlock()
		cancel()
		return nil, nil, group.NewError("a group turn is already running in this session; wait for it or stop it", group.CodeBusy)
	}
	t.active[sessionID] = turn
	t.mu.Unlock()
	stopCaller := context.AfterFunc(ctx, func() {
		t.mu.Lock()
		if turn.reason == turnRunning {
			turn.reason = turnStopped
		}
		t.mu.Unlock()
		turn.cancel()
	})
	minutes := strconv.Itoa(int(math.Round(float64(limits.MaxElapsedMsPerTurn) / 60_000)))
	timer := time.AfterFunc(time.Duration(limits.MaxElapsedMsPerTurn)*time.Millisecond, func() {
		t.limit(turn, "Stopped: this turn ran for "+minutes+" minutes (maxElapsedMsPerTurn).")
	})
	turn.cleanup = func() {
		timer.Stop()
		stopCaller()
	}
	return turn, turnCtx, nil
}

// limit is a guard rail ending the turn: everyone is stopped, and why is noted once.
func (t *turns) limit(turn *activeTurn, note string) {
	t.mu.Lock()
	if turn.reason != turnRunning {
		t.mu.Unlock()
		return
	}
	turn.reason = turnLimited
	turn.limitNote = note
	t.mu.Unlock()
	turn.cancel()
}

func (t *turns) end(ctx context.Context, turn *activeTurn, plan group.Plan) (TurnResult, error) {
	turn.cleanup()
	defer turn.cancel()
	t.mu.Lock()
	present := slices.Clone(turn.presenceOrder)
	t.mu.Unlock()
	for _, agentID := range present {
		t.setPresence(turn, agentID, group.PresenceIdle, "", "")
	}
	turn.settling.Wait()
	t.mu.Lock()
	note := turn.limitNote
	t.mu.Unlock()
	var noteErr error
	if note != "" {
		var m group.Message
		if m, noteErr = t.d.th.note(ctx, turn.group.ID, turn.sessionID, note); noteErr == nil {
			t.addMessage(turn, m.ID)
		}
	}
	t.d.th.settle(turn.sessionID)

	t.mu.Lock()
	delete(t.active, turn.sessionID)
	status := group.TurnCompleted
	switch {
	case turn.reason == turnStopped:
		status = group.TurnStopped
	case turn.reason == turnLimited:
		status = group.TurnLimited
	case plan.Kind == group.PlanNone:
		status = group.TurnNoReply
	}
	usage := turn.usage.Clone()
	added := slices.Clone(turn.added)
	t.mu.Unlock()
	close(turn.done)

	events.Emit(t.d.bus, events.GroupTurnFinished, turn.sessionID, events.GroupTurnFinishedPayload{
		GroupID: turn.group.ID, TurnID: turn.id, Status: status, Usage: usage.Clone(),
	})
	all, err := t.d.th.list(ctx, turn.sessionID, 0)
	if err != nil {
		return TurnResult{}, err
	}
	byID := map[string]group.Message{}
	for _, m := range all {
		byID[m.ID] = m
	}
	messages := []group.Message{}
	for _, id := range added {
		if m, ok := byID[id]; ok {
			messages = append(messages, m)
		}
	}
	return TurnResult{TurnID: turn.id, Status: status, Plan: plan, Messages: messages, Usage: usage}, noteErr
}

func (t *turns) runPlan(ctx context.Context, turn *activeTurn, plan group.Plan) error {
	switch plan.Kind {
	case group.PlanNone:
		m, err := t.d.th.note(context.WithoutCancel(ctx), turn.group.ID, turn.sessionID, plan.Reason)
		if err != nil {
			return err
		}
		t.addMessage(turn, m.ID)
		return nil
	case group.PlanSingle:
		return t.memberTurn(ctx, turn, plan.AgentID, memberTurnOptions{role: RoleMember})
	case group.PlanRound:
		return t.round(ctx, turn, plan.AgentIDs, plan.Summarize)
	case group.PlanLead:
		return t.leadTurn(ctx, turn, plan.Helpers)
	default:
		return nil
	}
}

// round runs several members side by side; then the lead sums up.
func (t *turns) round(ctx context.Context, turn *activeTurn, agentIDs []string, summarize bool) error {
	persist := context.WithoutCancel(ctx)
	maxMembers := turn.limits.MaxMembersPerRound
	ids := agentIDs
	if len(agentIDs) > maxMembers {
		ids = agentIDs[:maxMembers]
		var left []string
		for _, id := range agentIDs[maxMembers:] {
			left = append(left, t.nameOf(id))
		}
		m, err := t.d.th.note(persist, turn.group.ID, turn.sessionID, strings.Join(left, ", ")+" left out: at most "+strconv.Itoa(maxMembers)+" members answer in one round.")
		if err != nil {
			return err
		}
		t.addMessage(turn, m.ID)
	}
	roundID := kernel.NewID()
	// Every answer is opened first, in plan order, so the thread's order does not depend
	// on who starts first.
	prepared := make([]*preparedMember, len(ids))
	for i, id := range ids {
		p, err := t.prepareMember(ctx, turn, id, memberTurnOptions{role: RoleMember, roundID: roundID})
		if err != nil {
			return err
		}
		prepared[i] = p
	}
	answers := make([]*group.Message, len(ids))
	var wg sync.WaitGroup
	for i, p := range prepared {
		if p == nil {
			continue
		}
		wg.Go(func() { answers[i] = t.runMember(ctx, turn, p) })
	}
	wg.Wait()
	if !summarize || len(ids) < 2 || isDone(ctx) {
		return nil
	}
	var missing []MissingMember
	for i, m := range answers {
		if m != nil && (m.Status == group.StatusCompleted || m.Status == group.StatusPartial) {
			continue
		}
		reason := "no answer"
		switch {
		case m != nil && m.Error != "":
			reason = m.Error
		case m != nil && m.Status != "":
			reason = statusWords(m.Status)
		}
		missing = append(missing, MissingMember{AgentID: ids[i], Reason: reason})
	}
	return t.memberTurn(ctx, turn, turn.group.LeadID, memberTurnOptions{role: RoleSummary, missing: missing})
}

// ---------------------------------------------------------------------------
// One member's turn
// ---------------------------------------------------------------------------

// memberTurn has one member answer in its own session (nothing runs when it is not
// running or the turn was stopped).
func (t *turns) memberTurn(ctx context.Context, turn *activeTurn, agentID string, opts memberTurnOptions) error {
	p, err := t.prepareMember(ctx, turn, agentID, opts)
	if err != nil || p == nil {
		return err
	}
	t.runMember(ctx, turn, p)
	return nil
}

// isDone reports whether ctx was cancelled (the turn was stopped or limited).
func isDone(ctx context.Context) bool {
	select {
	case <-ctx.Done():
		return true
	default:
		return false
	}
}

// prepareMember writes what a member is told and opens its answer in the thread
// ("running"). Nil when the member is not running or the turn was stopped.
func (t *turns) prepareMember(ctx context.Context, turn *activeTurn, agentID string, opts memberTurnOptions) (*preparedMember, error) {
	a, ok := t.agent(agentID)
	if !ok || isDone(ctx) {
		return nil, nil //nolint:nilnil // nil, nil means the member does not run
	}
	persist := context.WithoutCancel(ctx)
	g, groupSessionID := turn.group, turn.sessionID
	memberSession, err := t.memberSession(persist, g, groupSessionID, agentID)
	if err != nil {
		return nil, err
	}
	t.addMemberSession(turn, memberSession)

	messages, cursor, err := t.d.th.unseen(persist, groupSessionID, agentID)
	if err != nil {
		return nil, err
	}
	in := MemberPromptInput{Group: g, Names: t.names(g), Self: agentID, Role: opts.role, Messages: messages, View: g.MemberView, Missing: opts.missing}
	if opts.retry != nil {
		in.RetryStatus = opts.retry.Status
		if in.RetryStatus == "" {
			in.RetryStatus = group.StatusFailed
		}
	}
	prompt := BuildMemberPrompt(in)
	if err := t.d.th.markSeen(persist, groupSessionID, map[string]int{agentID: cursor}); err != nil {
		return nil, err
	}
	w := newAnswerWriter(t.d.th, group.Message{SessionID: groupSessionID, GroupID: g.ID, AgentID: agentID, RoundID: opts.roundID, Summary: opts.role == RoleSummary})
	m, err := w.open(persist, opts.retry)
	if err != nil {
		return nil, err
	}
	t.addMessage(turn, m.ID)
	return &preparedMember{agentID: agentID, agent: a, memberSession: memberSession, prompt: prompt, writer: w, options: opts}, nil
}

// runMember runs a prepared member and stores how its answer ended.
func (t *turns) runMember(ctx context.Context, turn *activeTurn, p *preparedMember) *group.Message {
	persist := context.WithoutCancel(ctx)
	memberCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	own := &directRun{cancel: cancel}
	t.mu.Lock()
	turn.direct[p.agentID] = own
	t.mu.Unlock()
	timer := time.AfterFunc(time.Duration(turn.limits.MemberTimeoutMs)*time.Millisecond, func() {
		t.mu.Lock()
		if own.reason == memberRunning {
			own.reason = memberTimeout
		}
		t.mu.Unlock()
		cancel()
	})

	roundID := p.options.roundID
	presence := func(state group.PresenceState, tool string) { t.setPresence(turn, p.agentID, state, tool, roundID) }
	presence(group.PresenceThinking, "")
	traceAttributes := t.traceAttributes(turn, p.agentID, p.options.role, roundID)
	mc := MemberTurnContext{
		GroupID:         turn.group.ID,
		GroupSessionID:  turn.sessionID,
		AgentID:         p.agentID,
		SessionID:       p.memberSession,
		Role:            p.options.role,
		RoundID:         roundID,
		Prompt:          p.prompt,
		TraceAttributes: traceAttributes,
		Execute: func(ctx context.Context) (MemberTurnResult, error) {
			return t.stream(ctx, turn, p.agent, p.memberSession, p.prompt, traceAttributes, func(c kernel.StreamChunk) error {
				return p.writer.chunk(persist, c, presence)
			})
		},
	}
	result, failure := t.runRunner(memberCtx, mc)

	timer.Stop()
	t.mu.Lock()
	if turn.direct[p.agentID] == own {
		delete(turn.direct, p.agentID)
	}
	mStop, tStop := own.reason, turn.reason
	t.mu.Unlock()
	presence(group.PresenceIdle, "")

	streamErr := p.writer.streamErr()
	status := statusOf(result, failure, mStop, tStop, streamErr, p.writer.hasText())
	var usage *kernel.TokenUsage
	if result != nil {
		usage = &result.Usage
	}
	m, err := p.writer.finish(persist, status, usage, errorFor(status, failure, streamErr, turn.limits.MemberTimeoutMs))
	if err != nil {
		t.d.logger.Warn("group: could not store a member's answer", "group", turn.group.ID, "agent", p.agentID, "error", err)
	}
	return m
}

// leadState is the lead's text in segments: what it says before handing off, then
// (after the hand-offs) its summary. A segment is opened by its first text or tool call.
// It is guarded by mu: hand-offs (Brief) run on the spawn calls' goroutines.
type leadState struct {
	mu        sync.Mutex
	segment   *answerWriter
	handedOff bool
	segments  []*answerWriter
	// streamError is the lead's stream error when no segment was open.
	streamError string
}

func (t *turns) openSegment(ctx context.Context, turn *activeTurn, ls *leadState) (*answerWriter, error) {
	ls.mu.Lock()
	defer ls.mu.Unlock()
	if ls.segment != nil {
		return ls.segment, nil
	}
	w := newAnswerWriter(t.d.th, group.Message{SessionID: turn.sessionID, GroupID: turn.group.ID, AgentID: turn.group.LeadID, Summary: ls.handedOff})
	m, err := w.open(ctx, nil)
	if err != nil {
		return nil, err
	}
	ls.segment = w
	ls.segments = append(ls.segments, w)
	t.addMessage(turn, m.ID)
	return w, nil
}

func (t *turns) closeSegment(ctx context.Context, ls *leadState, handOff bool) error {
	ls.mu.Lock()
	defer ls.mu.Unlock()
	if handOff {
		ls.handedOff = true
	}
	open := ls.segment
	ls.segment = nil
	if open != nil && !open.isFinished() {
		_, err := open.finish(ctx, group.StatusCompleted, nil, "")
		return err
	}
	return nil
}

// leadTurn has the lead answer; it may hand parts to helpers, whose answers stream in as
// one round.
func (t *turns) leadTurn(ctx context.Context, turn *activeTurn, helpers []string) error {
	persist := context.WithoutCancel(ctx)
	g, groupSessionID := turn.group, turn.sessionID
	leadID := g.LeadID
	lead, ok := t.agent(leadID)
	if !ok {
		return nil
	}
	leadSession, err := t.memberSession(persist, g, groupSessionID, leadID)
	if err != nil {
		return err
	}
	t.addMemberSession(turn, leadSession)
	roundID := kernel.NewID()
	messages, cursor, err := t.d.th.unseen(persist, groupSessionID, leadID)
	if err != nil {
		return err
	}
	prompt := BuildMemberPrompt(MemberPromptInput{Group: g, Names: t.names(g), Self: leadID, Role: RoleLead, Messages: messages, View: g.MemberView, Helpers: helpers})
	if err := t.d.th.markSeen(persist, groupSessionID, map[string]int{leadID: cursor}); err != nil {
		return err
	}

	ls := &leadState{}
	presence := func(state group.PresenceState, tool string) { t.setPresence(turn, leadID, state, tool, "") }
	scope := t.leadScope(turn, ls, helpers, roundID)

	// Hand-off rows and answers take the child run's final status (A-105 decides it).
	onFinished := func(e kernel.Event, p events.SubagentFinishedPayload) {
		if e.SessionID != leadSession {
			return
		}
		turn.settling.Go(func() {
			if err := t.settleHandoff(persist, turn, p.RunID, group.MessageStatus(p.Status), p.Error); err != nil {
				t.d.logger.Warn("group: could not record the end of a hand-off", "run", p.RunID, "error", err)
			}
		})
	}
	offs := []events.Unsubscribe{
		events.Subscribe(t.d.bus, events.SubagentCompleted, onFinished),
		events.Subscribe(t.d.bus, events.SubagentFailed, onFinished),
		events.Subscribe(t.d.bus, events.SubagentCancelled, onFinished),
	}
	leadCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	own := &directRun{cancel: cancel}
	t.mu.Lock()
	t.scopes[leadSession] = scope
	turn.direct[leadID] = own
	t.mu.Unlock()

	presence(group.PresenceThinking, "")
	traceAttributes := t.traceAttributes(turn, leadID, RoleLead, "")
	onChunk := func(c kernel.StreamChunk) error {
		switch {
		case c.Type == kernel.ChunkToolCall && c.ToolCall != nil && c.ToolCall.Name == spawnTool:
			if err := t.closeSegment(persist, ls, false); err != nil {
				return err
			}
			presence(group.PresenceTool, spawnTool)
			return nil
		case c.Type == kernel.ChunkToolResult:
			ls.mu.Lock()
			known := c.ToolResult != nil && slices.ContainsFunc(ls.segments, func(s *answerWriter) bool { return s.hasTool(c.ToolResult.ToolCallID) })
			ls.mu.Unlock()
			if !known {
				return nil
			}
		case c.Type == kernel.ChunkError:
			ls.mu.Lock()
			seg := ls.segment
			if seg == nil {
				ls.streamError = c.Error
			}
			ls.mu.Unlock()
			if seg != nil {
				return seg.chunk(persist, c, presence)
			}
			return nil
		case c.Type == kernel.ChunkText && (c.Content == nil || *c.Content == ""):
			return nil
		case c.Type != kernel.ChunkText && c.Type != kernel.ChunkToolCall:
			return nil
		}
		seg, err := t.openSegment(persist, turn, ls)
		if err != nil {
			return err
		}
		return seg.chunk(persist, c, presence)
	}
	result, failure := t.runRunner(leadCtx, MemberTurnContext{
		GroupID:         g.ID,
		GroupSessionID:  groupSessionID,
		AgentID:         leadID,
		SessionID:       leadSession,
		Role:            RoleLead,
		Prompt:          prompt,
		TraceAttributes: traceAttributes,
		Execute: func(ctx context.Context) (MemberTurnResult, error) {
			return t.stream(ctx, turn, lead, leadSession, prompt, traceAttributes, onChunk)
		},
	})

	t.mu.Lock()
	delete(t.scopes, leadSession)
	if turn.direct[leadID] == own {
		delete(turn.direct, leadID)
	}
	mStop, tStop := own.reason, turn.reason
	t.mu.Unlock()
	for _, off := range offs {
		off()
	}
	presence(group.PresenceIdle, "")

	ls.mu.Lock()
	last, segments, leadStreamErr := ls.segment, len(ls.segments), ls.streamError
	ls.mu.Unlock()
	var usage *kernel.TokenUsage
	if result != nil {
		usage = &result.Usage
	}
	streamErr, hasText := leadStreamErr, false
	if last != nil {
		streamErr, hasText = last.streamErr(), last.hasText()
	}
	status := statusOf(result, failure, mStop, tStop, streamErr, hasText)
	switch {
	case last != nil && !last.isFinished():
		if _, err := last.finish(persist, status, usage, errorFor(status, failure, streamErr, turn.limits.MemberTimeoutMs)); err != nil {
			return err
		}
	case segments == 0 && status != group.StatusCompleted:
		// The lead never wrote anything: still show that it did not answer.
		empty := newAnswerWriter(t.d.th, group.Message{SessionID: groupSessionID, GroupID: g.ID, AgentID: leadID})
		m, err := empty.open(persist, nil)
		if err != nil {
			return err
		}
		t.addMessage(turn, m.ID)
		if _, err := empty.finish(persist, status, usage, errorFor(status, failure, streamErr, turn.limits.MemberTimeoutMs)); err != nil {
			return err
		}
	}
	// The lead learned every hand-off result in its own turn.
	seq, err := t.d.th.lastSeq(persist, groupSessionID)
	if err != nil {
		return err
	}
	return t.d.th.markSeen(persist, groupSessionID, map[string]int{leadID: seq})
}

// leadScope is the spawn scope of the lead's session while it leads the turn: its
// targets are the helpers, its hand-offs need no approval, and each member works in its
// member session, told the thread and the lead's ask, its answer streaming into the
// thread as part of the round.
func (t *turns) leadScope(turn *activeTurn, ls *leadState, helpers []string, roundID string) *subagent.SpawnScope {
	g, groupSessionID, leadID := turn.group, turn.sessionID, turn.group.LeadID
	allow := make([]subagent.TargetConfig, 0, len(helpers))
	for _, id := range helpers {
		access := subagent.AccessReadOnly
		if a, ok := t.agent(id); ok && a.Profile().Sandbox.Mode == sandboxhub.SelectionOwn {
			access = subagent.AccessWrite
		}
		allow = append(allow, subagent.TargetConfig{AgentID: id, Access: access})
	}
	return &subagent.SpawnScope{
		Allow: allow,
		Limits: subagent.Limits{
			MaxConcurrent:       max(1, min(len(helpers), turn.limits.MaxMembersPerRound)),
			MaxRunsPerTurn:      turn.limits.MaxMembersPerRound,
			MaxTokensPerTurn:    turn.limits.MaxTokensPerTurn,
			MaxElapsedMsPerTurn: turn.limits.MaxElapsedMsPerTurn,
			ChildTimeoutMs:      turn.limits.MemberTimeoutMs,
		},
		PreApproved:    true,
		GrantSpawnTool: true,
		IndexIntro: "These are the other members of your group chat. They see the group thread, so a short, specific task is enough; " +
			"each answer is shown in the thread and comes back to you.",
		ChildSession: func(ctx context.Context, agentID string) (string, error) {
			id, err := t.memberSession(ctx, g, groupSessionID, agentID)
			if err == nil {
				t.addMemberSession(turn, id)
			}
			return id, err
		},
		Brief: func(ctx context.Context, req subagent.Request, target subagent.ScopeTarget) (string, error) {
			persist := context.WithoutCancel(ctx)
			if err := t.closeSegment(persist, ls, true); err != nil {
				return "", err
			}
			row, err := t.d.th.append(persist, group.Message{
				SessionID: groupSessionID, GroupID: g.ID, Kind: group.KindHandoff, AgentID: leadID, ToAgentID: target.AgentID,
				Text: req.Task, Status: group.StatusRunning, SubagentRunID: target.RunID,
			})
			if err != nil {
				return "", err
			}
			t.addMessage(turn, row.ID)
			t.mu.Lock()
			turn.spawned[target.AgentID] = spawnedRun{runID: target.RunID, leadID: leadID}
			t.mu.Unlock()
			messages, cursor, err := t.d.th.unseen(persist, groupSessionID, target.AgentID)
			if err != nil {
				return "", err
			}
			if err := t.d.th.markSeen(persist, groupSessionID, map[string]int{target.AgentID: cursor}); err != nil {
				return "", err
			}
			ask := req.Task
			if !req.Context.IsZero() {
				ask += "\n\n" + prettyJSON(req.Context)
			}
			return BuildMemberPrompt(MemberPromptInput{Group: g, Names: t.names(g), Self: target.AgentID, Role: RoleHandoff, Messages: messages, View: g.MemberView, Ask: ask}), nil
		},
		Runner: func(ctx context.Context, child subagent.TurnContext) (subagent.TurnResult, error) {
			return t.handoffTurn(ctx, turn, child, roundID)
		},
	}
}

// handoffTurn is a member's turn for a hand-off: its answer streams into the thread as
// part of the round.
func (t *turns) handoffTurn(ctx context.Context, turn *activeTurn, child subagent.TurnContext, roundID string) (subagent.TurnResult, error) {
	a, ok := t.agent(child.AgentID)
	if !ok {
		return subagent.TurnResult{}, fmt.Errorf("%q is not running", child.AgentID)
	}
	persist := context.WithoutCancel(ctx)
	w := newAnswerWriter(t.d.th, group.Message{
		SessionID: turn.sessionID, GroupID: turn.group.ID, AgentID: child.AgentID, RoundID: roundID, SubagentRunID: child.Parent.SubagentRunID,
	})
	m, err := w.open(persist, nil)
	if err != nil {
		return subagent.TurnResult{}, err
	}
	t.addMessage(turn, m.ID)
	t.mu.Lock()
	t.handoffWriters[child.Parent.SubagentRunID] = w
	t.mu.Unlock()
	presence := func(state group.PresenceState, tool string) { t.setPresence(turn, child.AgentID, state, tool, roundID) }
	presence(group.PresenceThinking, "")
	defer presence(group.PresenceIdle, "")

	traceAttributes := maps.Clone(child.TraceAttributes)
	if traceAttributes == nil {
		traceAttributes = map[string]any{}
	}
	maps.Copy(traceAttributes, t.traceAttributes(turn, child.AgentID, RoleHandoff, roundID))
	result, failure := t.runRunner(ctx, MemberTurnContext{
		GroupID:         turn.group.ID,
		GroupSessionID:  turn.sessionID,
		AgentID:         child.AgentID,
		SessionID:       child.SessionID,
		Role:            RoleHandoff,
		RoundID:         roundID,
		Prompt:          child.Prompt,
		TraceAttributes: traceAttributes,
		Execute: func(ctx context.Context) (MemberTurnResult, error) {
			return t.stream(ctx, turn, a, child.SessionID, child.Prompt, child.TraceAttributes, func(c kernel.StreamChunk) error {
				return w.chunk(persist, c, presence)
			})
		},
	})
	if failure != nil {
		return subagent.TurnResult{}, failure
	}
	// The answer's text is the report; its final status comes with the run's end
	// (settleHandoff).
	out := subagent.TurnResult{Output: w.text(), Usage: result.Usage, Aborted: result.Aborted, StopReason: result.StopReason}
	if out.Output == "" {
		out.Output = result.Output
	}
	if out.StopReason == "" && w.streamErr() != "" {
		out.StopReason = kernel.StopError
		if w.hasText() {
			out.StopReason = kernel.StopMaxTokens
		}
	}
	return out, nil
}

// settleHandoff is a hand-off's child run ending: its row and its answer take the run's
// status.
func (t *turns) settleHandoff(ctx context.Context, turn *activeTurn, runID string, status group.MessageStatus, errMsg string) error {
	t.mu.Lock()
	for agentID, child := range turn.spawned {
		if child.runID == runID {
			delete(turn.spawned, agentID)
		}
	}
	w := t.handoffWriters[runID]
	delete(t.handoffWriters, runID)
	t.mu.Unlock()
	all, err := t.d.th.list(ctx, turn.sessionID, 0)
	if err != nil {
		return err
	}
	for _, m := range all {
		if m.Kind == group.KindHandoff && m.SubagentRunID == runID {
			row := m.Clone()
			row.Status = status
			if errMsg != "" {
				row.Error = errMsg
			}
			if _, err := t.d.th.update(ctx, row); err != nil {
				return err
			}
			break
		}
	}
	if w != nil {
		if !w.isFinished() {
			_, err := w.finish(ctx, status, nil, errMsg)
			return err
		}
		return nil
	}
	for _, m := range all {
		if m.Kind == group.KindAgent && m.SubagentRunID == runID {
			if m.Status != status {
				answer := m.Clone()
				answer.Status = status
				if errMsg != "" {
					answer.Error = errMsg
				}
				_, err := t.d.th.update(ctx, answer)
				return err
			}
			break
		}
	}
	return nil
}

// stream runs a member's streamed turn, handing each chunk to onChunk. An error chunk
// (the model's failure, the loop's turn limit) is handed on too; the sequence ends after
// it.
func (t *turns) stream(
	ctx context.Context,
	turn *activeTurn,
	a *agent.Agent,
	sessionID, prompt string,
	traceAttributes map[string]any,
	onChunk func(kernel.StreamChunk) error,
) (MemberTurnResult, error) {
	t.addMemberSession(turn, sessionID)
	var res MemberTurnResult
	for c, err := range a.ChatStream(ctx, prompt, agent.WithChatSession(sessionID), agent.WithChatTraceAttributes(traceAttributes)) {
		if err != nil {
			if c.Type == kernel.ChunkError {
				if cerr := onChunk(c); cerr != nil {
					return res, cerr
				}
				continue
			}
			if ctx.Err() != nil {
				res.Aborted = true
				return res, nil
			}
			return res, err
		}
		if c.Type == kernel.ChunkDone {
			if c.Usage != nil {
				res.Usage = c.Usage.Clone()
			}
			res.Aborted = c.Aborted
			if c.StopReason == kernel.StopMaxTokens || c.StopReason == kernel.StopError {
				res.StopReason = c.StopReason
			}
		}
		if cerr := onChunk(c); cerr != nil {
			return res, cerr
		}
	}
	return res, nil
}

// runRunner runs a member turn through the host's runner, or its own Execute. A panic of
// the runner is a failure of the turn (logged redacted), never a crash.
func (t *turns) runRunner(ctx context.Context, mc MemberTurnContext) (result *MemberTurnResult, failure error) {
	defer func() {
		if rec := recover(); rec != nil {
			t.d.logger.Error("group: the member runner panicked", "group", mc.GroupID, "agent", mc.AgentID, "panic", redact.Panic(rec))
			result, failure = nil, errors.New("the runner panicked")
		}
	}()
	var runner MemberRunner
	if t.d.runner != nil {
		runner = t.d.runner()
	}
	if runner == nil {
		runner = func(ctx context.Context, mc MemberTurnContext) (MemberTurnResult, error) { return mc.Execute(ctx) }
	}
	res, err := runner(ctx, mc)
	if err != nil {
		return nil, err
	}
	return &res, nil
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func (t *turns) host() AgentHost {
	if t.d.agents == nil {
		return nil
	}
	return t.d.agents()
}

func (t *turns) agent(id string) (*agent.Agent, bool) {
	host := t.host()
	if host == nil {
		return nil, false
	}
	a, ok := host.Get(id)
	return a, ok && a != nil
}

func (t *turns) addMessage(turn *activeTurn, id string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	turn.added = append(turn.added, id)
}

func (t *turns) addMemberSession(turn *activeTurn, sessionID string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	turn.memberSessions[sessionID] = true
}

func (t *turns) setPresence(turn *activeTurn, agentID string, state group.PresenceState, tool, roundID string) {
	t.mu.Lock()
	prev, ok := turn.presence[agentID]
	if (ok && prev.State == state && prev.Tool == tool) || (!ok && state == group.PresenceIdle) {
		t.mu.Unlock()
		return
	}
	p := events.GroupPresencePayload{GroupID: turn.group.ID, AgentID: agentID, State: state, Tool: tool, RoundID: roundID}
	if state == group.PresenceIdle {
		delete(turn.presence, agentID)
		turn.presenceOrder = slices.DeleteFunc(turn.presenceOrder, func(id string) bool { return id == agentID })
	} else {
		if !ok {
			turn.presenceOrder = append(turn.presenceOrder, agentID)
		}
		turn.presence[agentID] = p
	}
	t.mu.Unlock()
	t.d.th.presence(turn.sessionID, p)
}

func (t *turns) traceAttributes(turn *activeTurn, agentID string, role MemberRole, roundID string) map[string]any {
	attrs := map[string]any{
		"source":         SessionSource,
		"groupId":        turn.group.ID,
		"groupSessionId": turn.sessionID,
		"groupTurnId":    turn.id,
		"groupRole":      string(role),
		"memberId":       agentID,
	}
	if roundID != "" {
		attrs["groupRoundId"] = roundID
	}
	return attrs
}

// names maps every member to its display name (its id when it is not running).
func (t *turns) names(g group.Group) map[string]string {
	out := make(map[string]string, len(g.Members))
	for _, id := range g.Members {
		out[id] = t.nameOf(id)
	}
	return out
}

func (t *turns) nameOf(agentID string) string {
	if a, ok := t.agent(agentID); ok {
		return a.Profile().Name
	}
	return agentID
}

// titleFrom gives a new session its first message as its title.
func (t *turns) titleFrom(ctx context.Context, session kernel.Session, text string) error {
	if title, _ := session.Metadata.Get("title"); title != nil && title != "" {
		return nil
	}
	line := ""
	for _, l := range strings.Split(text, "\n") {
		if l = jsstr.Trim(l); l != "" {
			line = l
			break
		}
	}
	if kernel.UTF16Len(line) > 80 {
		line = jsstr.TruncateUTF16(line, 80) + ellipsis
	}
	_, err := t.d.th.updateMetadata(ctx, session.ID, func(meta ordered.Object) ordered.Object {
		if title, _ := meta.Get("title"); title != nil && title != "" {
			return meta
		}
		meta.Set("title", line)
		return meta
	})
	return err
}

// statusOf decides how a member's own turn ended. A turn that ended on its own keeps its
// outcome even when a stop came in as it finished. The stream's error chunk (a model
// failure, the turn limit) is a partial answer when some text was written, a failure
// otherwise.
func statusOf(result *MemberTurnResult, failure error, mStop memberStop, tStop turnStop, streamError string, hasText bool) group.MessageStatus {
	endedOnItsOwn := failure == nil && result != nil && !result.Aborted
	if !endedOnItsOwn {
		switch {
		case mStop == memberTimeout:
			return group.StatusTimedOut
		case tStop == turnLimited:
			return group.StatusBudgetExhausted
		case mStop == memberCancelled || tStop == turnStopped || (result != nil && result.Aborted):
			return group.StatusCancelled
		}
		return group.StatusFailed
	}
	switch {
	case result.StopReason == kernel.StopError:
		return group.StatusFailed
	case result.StopReason == kernel.StopMaxTokens:
		return group.StatusPartial
	case streamError != "":
		if hasText {
			return group.StatusPartial
		}
		return group.StatusFailed
	}
	return group.StatusCompleted
}

func errorFor(status group.MessageStatus, failure error, streamError string, memberTimeoutMs int) string {
	switch status {
	case group.StatusFailed:
		switch {
		case failure != nil:
			return subagent.SafeError(failure.Error())
		case streamError != "":
			return subagent.SafeError(streamError)
		}
		return subagent.SafeError("the model stopped the reply (refusal or content filter)")
	case group.StatusPartial:
		return "the reply hit the output token limit"
	case group.StatusTimedOut:
		return "did not finish within " + strconv.Itoa(int(math.Round(float64(memberTimeoutMs)/1000))) + "s"
	case group.StatusBudgetExhausted:
		return "the turn's budget ran out"
	case group.StatusCancelled:
		return "stopped"
	default:
		return ""
	}
}

func addUsage(a, b kernel.TokenUsage) kernel.TokenUsage {
	out := kernel.TokenUsage{InputTokens: a.InputTokens + b.InputTokens, OutputTokens: a.OutputTokens + b.OutputTokens}
	sum := func(x, y *int) *int {
		xv, yv := 0, 0
		if x != nil {
			xv = *x
		}
		if y != nil {
			yv = *y
		}
		if xv == 0 && yv == 0 {
			return nil
		}
		s := xv + yv
		return &s
	}
	out.CacheReadTokens = sum(a.CacheReadTokens, b.CacheReadTokens)
	out.CacheWriteTokens = sum(a.CacheWriteTokens, b.CacheWriteTokens)
	return out
}

// thousands writes n with "," every three digits (toLocaleString("en-US")).
func thousands(n int) string {
	s := strconv.Itoa(n)
	if n < 0 {
		return "-" + thousands(-n)
	}
	var b strings.Builder
	for i, c := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(c)
	}
	return b.String()
}

// prettyJSON is JSON.stringify(v, null, 2).
func prettyJSON(v ordered.Object) string {
	raw, err := jsonx.Marshal(v)
	if err != nil {
		return "{}"
	}
	var buf bytes.Buffer
	if err := json.Indent(&buf, raw, "", "  "); err != nil {
		return string(raw)
	}
	return buf.String()
}
