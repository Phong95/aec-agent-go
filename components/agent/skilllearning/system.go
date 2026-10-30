package skilllearning

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/Phong95/aec-agent-go/components/agent/skilllearning/learning"
	"github.com/Phong95/aec-agent-go/components/events"
	"github.com/Phong95/aec-agent-go/components/persistence"
	"github.com/Phong95/aec-agent-go/components/skillhub/skill"
	"github.com/Phong95/aec-agent-go/kernel"
)

// The limits of the learner.
const (
	// maxTurnMessages is the most messages of one turn read back for distilling.
	maxTurnMessages = 1000
	// maxDistillAttempts: an invalid draft is tried once more, then dropped.
	maxDistillAttempts = 2
	// retireCheckInterval is how often (at most) the learner looks for skills to retire.
	retireCheckInterval = time.Hour
	day                 = 24 * time.Hour
	maxEpisodeRequest   = 300
	// countTimeout bounds the message count read while an event is delivered.
	countTimeout = 5 * time.Second
	// jobTimeout bounds one background job (a verdict, a distill pass, a retire check).
	jobTimeout = 10 * time.Minute
	// verifyTimeout bounds one verification replay (A-174).
	verifyTimeout = 15 * time.Minute
)

// VerificationSource is the session metadata "source" of a verification replay's
// throwaway session (A-174); the learner never learns from it.
const VerificationSource = "skill-verification"

// Verifier runs a verification replay (A-174). It never fails: a replay that could not
// run is a failed Verification with its reason.
type Verifier func(ctx context.Context, req learning.VerificationRequest) learning.Verification

// Deps is what a System needs from the agent that owns it.
type Deps struct {
	AgentID string
	Events  events.Bus
	// Persistence returns the persistence hub, read on every use: the Messages,
	// Sessions and SkillProposals stores are used.
	Persistence func() *persistence.PersistenceHub
	// Model is the agent's model access (its ModelManager).
	Model Model
	// Skills is the agent's skills - Create enables the new skill for the agent and saves
	// its profile. Nil (an agent without a skill hub): the learner never subscribes.
	Skills skill.AgentSkills
	// Verify (A-174) runs a verification replay in auto mode. Nil = auto mode proposes only.
	Verify Verifier
	// ToolRisk (A-174) is a tool's risk ("read-only", "draft", "consequential"), to keep
	// tasks that reach outside systems from replays. Nil = unknown.
	ToolRisk func(toolName string) string
}

// Option configures a System.
type Option func(*System)

// WithClock sets the clock of every timestamp and of the retire check (default time.Now).
func WithClock(now func() time.Time) Option {
	return func(s *System) {
		if now != nil {
			s.now = now
		}
	}
}

// WithLogger sets the logger (default slog.Default()). Log lines carry ids only, never
// message text.
func WithLogger(l *slog.Logger) Option {
	return func(s *System) {
		if l != nil {
			s.logger = l
		}
	}
}

// AgentSkillLearning is the agent's skill learning as hosts and commands see it
// (Agent.SkillLearning).
type AgentSkillLearning interface {
	// Enabled reports whether skill learning is on for this agent.
	Enabled() bool
	// ConfirmLastTurn is /learn: the user confirms the session's last finished turn was a success.
	ConfirmLastTurn(ctx context.Context, sessionID string) (learning.LearnResult, error)
	// ListProposals returns this agent's proposals, newest first.
	ListProposals(ctx context.Context, filter learning.ProposalFilter) ([]learning.Proposal, error)
	// GetProposal returns one of this agent's proposals, or nil.
	GetProposal(ctx context.Context, id string) (*learning.Proposal, error)
	// Accept accepts a pending proposal: create, patch or retire the skill (create enables
	// it for this agent). edited replaces the draft (create / patch). Errors are
	// *kernel.SkillError (the skill call's codes, "not_found" for an unknown proposal,
	// "invalid" when it is not pending).
	Accept(ctx context.Context, id string, edited *skill.Package) (learning.Proposal, error)
	// Reject rejects a pending proposal. Errors: *kernel.SkillError "not_found", "invalid".
	Reject(ctx context.Context, id, reason string) (learning.Proposal, error)
	// ListLearnedSkills returns the skills this agent learned, with their health (A-172).
	ListLearnedSkills(ctx context.Context) ([]learning.LearnedSkill, error)
}

// finishedTurn is the last finished turn of a session, kept until its next turn starts.
// Its fields are guarded by System.mu.
type finishedTurn struct {
	episode         learning.Episode
	candidate       bool
	awaitingVerdict bool
	captured        bool
	timer           *time.Timer
}

// chain is the serialized work of one key: a session's (message lookups, verdicts, in
// event order) or the background worker's ("" : distill passes and retire checks).
type chain struct {
	jobs []func(ctx context.Context)
	done chan struct{}
}

// System is the skill learner of one agent (A-166) - it turns a hard-won success into a
// reusable skill. Standalone and opt-in, like the memory system; the Agent only
// constructs, starts, disposes it and passes its config. It never touches the turn:
//
//	In:   the agent's event bus (turn, tool and skill events) - stage 1, the ledger -
//	      and MessageStore.GetRange for one turn's messages (the evidence)
//	Work: the agent's ModelManager - the verdict and distill calls
//	Out:  skill.AgentSkills Create / Update / Delete (the same port tools get, so the
//	      enable-for-this-agent and profile-save path runs unchanged)
//	      and skill-learning:* events (announcements)
//	Own:  the SkillProposalStore - proposals and learned-skill health records
//
//	1 Observe - the ledger counts each turn's tool calls, failures, denials and skills.
//	2 Judge   - a turn that ended normally after >= MinToolCalls calls with >= MinFailedCalls
//	            failures waits for the user's verdict: /learn, or a next message that
//	            accepts it (a correction is never learned). Silence for VerdictWaitMinutes
//	            gives up (only /learn can still confirm it). Job, subagent, group-member and
//	            verification sessions have nobody to confirm, so they are never learned from.
//	3 Distill - on skill-learning:episode-captured (its own event), one serialized worker
//	            per agent reads the turn and writes a create / patch draft SKILL.md.
//	4 Gate    - "propose": the draft waits in the review queue; "auto" (A-174): saved only
//	            when a verification replay passed, otherwise it waits for review.
//	5 Track   - A-172: each later use of a learned skill moves its health up (smooth) or
//	            down (struggled); a struggled use becomes a patch candidate, health 0 or
//	            long disuse a retire proposal (never deleted without approval).
//
// Event handlers run on the goroutine of the Emit, so they only update the ledger and
// queue the rest: per-session goroutines run the lookups and verdicts in event order, one
// background goroutine runs the distill passes. A started System must be disposed.
type System struct {
	d      Deps
	ledger *episodeLedger
	now    func() time.Time
	logger *slog.Logger

	mu       sync.Mutex
	cfg      learning.ResolvedConfig
	lifetime context.Context
	started  bool
	stopped  bool
	unsubs   []events.Unsubscribe
	finished map[string]*finishedTurn
	chains   map[string]*chain
	// lastRetireCheck is when the learner last looked for skills to retire.
	lastRetireCheck time.Time
	// verifyDay and verifyUsed count the verification replays of the current UTC day (A-174).
	verifyDay  string
	verifyUsed int
}

var _ AgentSkillLearning = (*System)(nil)

// New returns the skill learner of one agent. cfg is the profile's config (nil = off).
func New(d Deps, cfg *learning.Config, opts ...Option) (*System, error) {
	switch {
	case d.AgentID == "":
		return nil, errors.New("skilllearning: Deps.AgentID is required")
	case d.Events == nil:
		return nil, errors.New("skilllearning: Deps.Events is required")
	case d.Persistence == nil:
		return nil, errors.New("skilllearning: Deps.Persistence is required")
	case d.Model == nil:
		return nil, errors.New("skilllearning: Deps.Model is required")
	}
	s := &System{
		d: d, ledger: newEpisodeLedger(d.AgentID), now: time.Now, logger: slog.Default(),
		cfg: learning.ResolveConfig(cfg), finished: map[string]*finishedTurn{}, chains: map[string]*chain{},
	}
	for _, o := range opts {
		if o != nil {
			o(s)
		}
	}
	return s, nil
}

// Enabled reports whether skill learning is on for this agent.
func (s *System) Enabled() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cfg.Enabled
}

func (s *System) config() learning.ResolvedConfig {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cfg
}

func (s *System) iso() string { return kernel.MillisToISO(s.now().UnixMilli()) }

func (s *System) store() persistence.SkillProposalStore { return s.d.Persistence().SkillProposals }

// ---------------------------------------------------------------------------
// Lifecycle
// ---------------------------------------------------------------------------

// Start subscribes to the agent's events - only while skill learning is enabled. lifetime
// is the context of the background work; it should outlive Dispose. It has an effect only
// once, and none after Dispose.
func (s *System) Start(lifetime context.Context) { //nolint:contextcheck // a nil lifetime is replaced by Background
	if lifetime == nil {
		lifetime = context.Background()
	}
	s.mu.Lock()
	if s.started || s.stopped {
		s.mu.Unlock()
		return
	}
	s.started, s.lifetime = true, lifetime
	s.mu.Unlock()
	s.resubscribe()
}

// UpdateConfig replaces the config at runtime: it subscribes or unsubscribes when
// Enabled changed.
func (s *System) UpdateConfig(cfg *learning.Config) {
	s.mu.Lock()
	s.cfg = learning.ResolveConfig(cfg)
	live := s.started && !s.stopped
	s.mu.Unlock()
	if live {
		s.resubscribe()
	}
}

// Dispose stops listening, drops queued work, stops the verdict timers and waits for the
// work in flight (bounded by ctx). It is safe to call more than once.
func (s *System) Dispose(ctx context.Context) error {
	s.mu.Lock()
	s.stopped = true
	for _, c := range s.chains {
		c.jobs = nil
	}
	s.mu.Unlock()
	s.unsubscribe()
	return s.WaitForIdle(ctx)
}

// WaitForIdle returns once no session work, verdict or distill pass is queued or running,
// or with the ctx error.
func (s *System) WaitForIdle(ctx context.Context) error {
	for {
		s.mu.Lock()
		dones := make([]chan struct{}, 0, len(s.chains))
		for _, c := range s.chains {
			dones = append(dones, c.done)
		}
		s.mu.Unlock()
		if len(dones) == 0 {
			return nil
		}
		for _, d := range dones {
			select {
			case <-d:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
	}
}

func (s *System) resubscribe() {
	s.mu.Lock()
	enabled, subscribed := s.cfg.Enabled && s.d.Skills != nil, len(s.unsubs) > 0
	s.mu.Unlock()
	switch {
	case enabled && !subscribed:
		s.subscribe()
	case !enabled && subscribed:
		s.unsubscribe()
	}
}

func (s *System) subscribe() {
	bus, id := s.d.Events, s.d.AgentID
	unsubs := []events.Unsubscribe{
		events.Subscribe(bus, events.UserMessage, func(e kernel.Event, p events.UserMessagePayload) {
			if p.AgentID == id {
				s.onUserMessage(e.SessionID, p.Content)
			}
		}),
		events.Subscribe(bus, events.ToolDispatched, func(e kernel.Event, p events.DispatchStartPayload) {
			if p.AgentID == id {
				s.ledger.toolStarted(e.SessionID)
			}
		}),
		events.Subscribe(bus, events.ToolCompleted, func(e kernel.Event, p events.DispatchEndPayload) {
			if p.AgentID == id {
				s.ledger.toolEnded(e.SessionID, p.IsError, p.Attempt)
			}
		}),
		events.Subscribe(bus, events.ToolFailed, func(e kernel.Event, p events.DispatchFailedPayload) {
			if p.AgentID == id {
				s.ledger.toolFailed(e.SessionID, p.Error)
			}
		}),
		events.Subscribe(bus, events.ToolDenied, func(e kernel.Event, p events.ToolDeniedPayload) {
			if p.AgentID == id {
				s.ledger.toolDenied(e.SessionID)
			}
		}),
		events.Subscribe(bus, events.SkillDispatched, func(e kernel.Event, p events.DispatchStartPayload) {
			if p.AgentID == id {
				s.ledger.skillStarted(e.SessionID, skillNameOf(p.Arguments))
			}
		}),
		events.Subscribe(bus, events.RunCompleted, func(e kernel.Event, p events.RunCompletedPayload) {
			if p.AgentID != id {
				return
			}
			outcome := learning.OutcomeCompleted
			if p.Aborted {
				outcome = learning.OutcomeAborted
			}
			s.onTurnEnd(e.SessionID, outcome)
		}),
		events.Subscribe(bus, events.MaxIterationsReached, func(e kernel.Event, p events.MaxIterationsPayload) {
			if p.AgentID == id {
				s.onTurnEnd(e.SessionID, learning.OutcomeMaxIterations)
			}
		}),
		// Stage 3 listens to stage 2's announcement, like any other subscriber would.
		events.Subscribe(bus, events.SkillLearningEpisodeCaptured, func(_ kernel.Event, p events.SkillLearningEpisodeCapturedPayload) {
			if p.AgentID == id {
				ep := p.Episode
				s.enqueue("", func(ctx context.Context) { s.distillEpisode(ctx, ep) })
			}
		}),
	}
	s.mu.Lock()
	if s.stopped || len(s.unsubs) > 0 {
		s.mu.Unlock()
		for _, u := range unsubs {
			u()
		}
		return
	}
	s.unsubs = unsubs
	s.mu.Unlock()
}

func (s *System) unsubscribe() {
	s.mu.Lock()
	unsubs := s.unsubs
	s.unsubs = nil
	for _, t := range s.finished {
		stopTimer(t)
	}
	clear(s.finished)
	s.mu.Unlock()
	for _, u := range unsubs {
		u()
	}
	s.ledger.clear()
}

func skillNameOf(args kernel.Arguments) string {
	var v struct {
		Name string `json:"name"`
	}
	if len(args) == 0 {
		return ""
	}
	if err := unmarshal(args, &v); err != nil {
		return ""
	}
	return v.Name
}

// countNow reads the session's message count while the event is delivered, so it is
// taken while the event's message is still the last one saved.
func (s *System) countNow(sessionID string) int {
	ctx, cancel := context.WithTimeout(s.lifetimeCtx(), countTimeout)
	defer cancel()
	n, err := s.d.Persistence().Messages.Count(ctx, sessionID)
	if err != nil {
		s.logger.Warn("skill-learning: message count failed", "agent", s.d.AgentID, "session", sessionID, "error", err)
		return -1
	}
	return n
}

func (s *System) lifetimeCtx() context.Context {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.lifetime == nil {
		return context.Background()
	}
	return s.lifetime
}

// ---------------------------------------------------------------------------
// Stages 1-2: observe the turn, judge it, wait for the user's verdict
// ---------------------------------------------------------------------------

func (s *System) onUserMessage(sessionID, content string) {
	// Counted from now on, so no tool event of the turn is missed.
	token := s.ledger.begin(sessionID, s.iso())
	// Read while the user message is still the last one saved, and set at once (a short
	// turn may end before a queued job would run).
	if count := s.countNow(sessionID); count > 0 {
		s.ledger.setMessageStart(sessionID, token, count-1)
	}
	s.enqueue(sessionID, func(ctx context.Context) {
		// This message is the verdict on the session's previous turn.
		s.mu.Lock()
		previous := s.finished[sessionID]
		delete(s.finished, sessionID)
		judge := false
		if previous != nil {
			stopTimer(previous)
			judge = previous.awaitingVerdict && !previous.captured
		}
		s.mu.Unlock()
		if judge {
			s.judgeNextMessage(ctx, previous, content)
		}
	})
}

func (s *System) onTurnEnd(sessionID string, outcome learning.Outcome) {
	ep, ok := s.ledger.end(sessionID, outcome, s.iso(), -1)
	if !ok {
		return
	}
	ep.MessageEnd = s.countNow(sessionID)
	s.enqueue(sessionID, func(ctx context.Context) {
		session, err := s.d.Persistence().Sessions.Get(ctx, sessionID)
		if err != nil || !learnableSession(session) {
			return
		}
		cfg := s.config()
		usedLearnedAndStruggled := s.trackUsage(ctx, ep, cfg)
		candidate := isHardWon(ep, cfg) || (ep.Outcome == learning.OutcomeCompleted && usedLearnedAndStruggled)
		turn := &finishedTurn{episode: ep, candidate: candidate, awaitingVerdict: candidate}
		s.mu.Lock()
		if s.stopped {
			s.mu.Unlock()
			return
		}
		if candidate {
			turn.timer = time.AfterFunc(time.Duration(cfg.VerdictWaitMinutes)*time.Minute, func() {
				// Silence is not a success; /learn can still confirm the turn.
				s.mu.Lock()
				turn.awaitingVerdict = false
				turn.timer = nil
				s.mu.Unlock()
			})
		}
		s.finished[sessionID] = turn
		s.mu.Unlock()
		s.maybeCheckRetirements() //nolint:contextcheck // background work runs on the lifetime context on purpose
	})
}

func (s *System) judgeNextMessage(ctx context.Context, turn *finishedTurn, content string) {
	verdict := classifyReply(content)
	if verdict == verdictUnclear {
		msgs, err := s.loadTurn(ctx, turn.episode)
		reply := ""
		for i := len(msgs) - 1; i >= 0 && err == nil; i-- {
			if msgs[i].Role == kernel.RoleAssistant && strings.TrimSpace(msgs[i].Content) != "" {
				reply = msgs[i].Content
				break
			}
		}
		if err == nil {
			verdict, err = judgeReply(ctx, s.d.Model, reply, content)
		}
		if err != nil {
			s.logger.Error("skill-learning: verdict failed", "agent", s.d.AgentID, "session", turn.episode.SessionID, "error", err)
			verdict = verdictUnclear
		}
	}
	if verdict == verdictAccept {
		s.capture(ctx, turn, learning.SignalNextMessage)
	}
}

// ConfirmLastTurn is /learn: the user confirms the session's last finished turn was a
// success.
func (s *System) ConfirmLastTurn(ctx context.Context, sessionID string) (learning.LearnResult, error) {
	cfg := s.config()
	if !cfg.Enabled {
		return learning.LearnResult{Status: learning.LearnDisabled, Text: "Skill learning is off for this agent. Turn it on in the agent's settings first."}, nil
	}
	// Wait for the session's queued work (a turn end still being recorded).
	barrier := make(chan struct{})
	if s.enqueue(sessionID, func(context.Context) { close(barrier) }) { //nolint:contextcheck // the barrier runs in the session's queue; ctx bounds the wait below
		select {
		case <-barrier:
		case <-ctx.Done():
			return learning.LearnResult{}, ctx.Err()
		}
	}
	s.mu.Lock()
	turn := s.finished[sessionID]
	var res *learning.LearnResult
	switch {
	case turn == nil:
		res = &learning.LearnResult{Status: learning.LearnNoTurn, Text: "There is no finished task in this conversation to learn from yet."}
	case turn.captured:
		res = &learning.LearnResult{Status: learning.LearnAlreadyCaptured, Text: "Already noted: the last task is being turned into a skill."}
	case turn.episode.Outcome != learning.OutcomeCompleted || turn.episode.ToolCalls < 1:
		res = &learning.LearnResult{Status: learning.LearnNotEligible, Text: "The last task can't be learned from: it was stopped, ran out of steps, or used no tools."}
	}
	s.mu.Unlock()
	if res != nil {
		return *res, nil
	}
	s.capture(ctx, turn, learning.SignalLearnCommand)
	text := "Noted. I'll turn the last task into a draft skill for you to review (Capabilities → Skills → Learned)."
	if cfg.Mode == learning.ModeAuto {
		text = "Noted. I'll turn the last task into a skill and save it."
	}
	return learning.LearnResult{Status: learning.LearnCaptured, Text: text}, nil
}

// capture announces a confirmed turn (the distiller picks it up).
func (s *System) capture(ctx context.Context, turn *finishedTurn, signal learning.Signal) {
	s.mu.Lock()
	turn.captured = true
	turn.awaitingVerdict = false
	stopTimer(turn)
	ep := turn.episode
	s.mu.Unlock()
	captured := learning.CapturedEpisode{Episode: ep, Signal: signal, PatchTarget: s.patchTargetFor(ctx, ep)}
	events.Emit(s.d.Events, events.SkillLearningEpisodeCaptured, ep.SessionID, events.SkillLearningEpisodeCapturedPayload{
		AgentID: s.d.AgentID, Episode: captured,
	})
}

// patchTargetFor returns a custom skill the turn used and still struggled with - the
// latest one used.
func (s *System) patchTargetFor(ctx context.Context, ep learning.Episode) string {
	if !struggled(ep, s.config()) {
		return ""
	}
	for _, name := range slices.Backward(ep.SkillsUsed) {
		if sk, ok := s.editableSkill(ctx, name); ok {
			return sk.Name
		}
	}
	return ""
}

// ---------------------------------------------------------------------------
// Stages 3-4: distill a captured episode into a proposal (or save it, in auto mode)
// ---------------------------------------------------------------------------

type draft struct {
	action          learning.ProposalAction
	skillName       string
	pkg             skill.Package
	previousContent *string
}

func (s *System) distillEpisode(ctx context.Context, ep learning.CapturedEpisode) {
	cfg := s.config()
	if !cfg.Enabled {
		return
	}
	msgs, err := s.loadTurn(ctx, ep.Episode)
	if err != nil || len(msgs) == 0 {
		s.logger.Warn("skill-learning: turn messages not found", "agent", s.d.AgentID, "session", ep.SessionID, "error", err)
		return
	}
	skills, err := s.d.Skills.List(ctx)
	if err != nil {
		s.logger.Error("skill-learning: skills not listed", "agent", s.d.AgentID, "error", err)
		return
	}
	in := distillInput{messages: msgs, skills: skills}
	if ep.PatchTarget != "" {
		if sk, ok := s.editableSkill(ctx, ep.PatchTarget); ok {
			in.target = &sk
		}
	}
	var dr *draft
	for attempt := 1; attempt <= maxDistillAttempts; attempt++ {
		d, err := distill(ctx, s.d.Model, in)
		if err == nil && d == nil {
			s.logger.Info("skill-learning: nothing reusable to learn", "agent", s.d.AgentID, "session", ep.SessionID)
			return
		}
		if err == nil {
			dr, err = s.toDraft(ctx, *d)
		}
		if err == nil {
			break
		}
		s.logger.Error("skill-learning: distill attempt failed", "agent", s.d.AgentID, "session", ep.SessionID, "attempt", attempt, "error", err)
		if attempt == maxDistillAttempts || ctx.Err() != nil {
			return
		}
	}
	if dr == nil || s.isStopped() {
		return
	}
	if dr.action == learning.ActionCreate {
		if slots, err := s.learnedSkillSlots(ctx, cfg); err != nil || slots <= 0 {
			s.logger.Warn("skill-learning: learned-skill cap reached; the draft was not proposed (retire one or raise maxLearnedSkills)",
				"agent", s.d.AgentID, "max", cfg.MaxLearnedSkills, "skill", dr.skillName, "error", err)
			return
		}
	}
	now := s.iso()
	pkg := dr.pkg
	p := learning.Proposal{
		ID: kernel.NewID(), AgentID: s.d.AgentID, Action: dr.action, SkillName: dr.skillName,
		Summary: descriptionOf(pkg), Draft: &pkg, PreviousContent: dr.previousContent,
		Episode: episodeRef(ep, msgs), Status: learning.StatusPending, CreatedAt: now, UpdatedAt: now,
	}
	if err := s.supersedePending(ctx, p.SkillName, now); err != nil {
		s.logger.Error("skill-learning: older drafts not replaced", "agent", s.d.AgentID, "error", err)
	}
	// A-174: auto mode saves only behind a passing verification replay.
	if cfg.Mode == learning.ModeAuto {
		v := s.verifyDraft(ctx, p, msgs, cfg)
		p.Verification = &v
		if s.isStopped() {
			return
		}
	}
	if err := s.store().Save(ctx, p); err != nil {
		s.logger.Error("skill-learning: proposal not saved", "agent", s.d.AgentID, "error", err)
		return
	}
	auto := p.Verification != nil && p.Verification.Passed
	events.Emit(s.d.Events, events.SkillLearningProposed, ep.SessionID, events.SkillLearningProposedPayload{
		AgentID: s.d.AgentID, ProposalID: p.ID, Action: p.Action, SkillName: p.SkillName, Auto: auto,
	})
	if auto {
		if _, err := s.apply(ctx, p, nil, true); err != nil {
			// Left pending: the reviewer sees why in the queue (and may fix the draft).
			s.logger.Error("skill-learning: auto-save failed", "agent", s.d.AgentID, "skill", p.SkillName, "error", err)
		}
	}
}

// verifyDraft replays the draft before auto mode saves it (A-174). It never fails; a
// replay that does not run (outside systems, no verifier, budget used up) fails with its
// reason.
func (s *System) verifyDraft(ctx context.Context, p learning.Proposal, msgs []kernel.Message, cfg learning.ResolvedConfig) learning.Verification {
	skipped := func(reason string) learning.Verification {
		return learning.Verification{Reason: reason, At: s.iso()}
	}
	var outside []string
	if s.d.ToolRisk != nil {
		for _, m := range msgs {
			for _, c := range m.ToolCalls {
				if s.d.ToolRisk(c.Name) == "consequential" && !slices.Contains(outside, c.Name) {
					outside = append(outside, c.Name)
				}
			}
		}
	}
	if len(outside) > 0 {
		return skipped("the task used " + strings.Join(outside, ", ") + ", which reach outside systems, so it is never replayed")
	}
	if s.d.Verify == nil {
		return skipped("no verification replay is available here")
	}
	today := s.iso()[:10]
	s.mu.Lock()
	if s.verifyDay != today {
		s.verifyDay, s.verifyUsed = today, 0
	}
	if s.verifyUsed >= cfg.VerificationsPerDay {
		s.mu.Unlock()
		return skipped(fmt.Sprintf("today's %d verification replay(s) are used up", cfg.VerificationsPerDay))
	}
	s.verifyUsed++
	s.mu.Unlock()
	request := p.Episode.Request
	if len(msgs) > 0 && msgs[0].Role == kernel.RoleUser {
		request = msgs[0].Content
	}
	vctx, cancel := context.WithTimeout(ctx, verifyTimeout)
	defer cancel()
	return s.d.Verify(vctx, learning.VerificationRequest{Request: request, SkillName: p.SkillName, Draft: *p.Draft})
}

// toDraft decides create or patch and renders the SKILL.md (validated).
func (s *System) toDraft(ctx context.Context, d distilled) (*draft, error) {
	// "patch" only reaches a custom skill this agent has; anything else becomes a new skill.
	var existing *skill.Skill
	if d.action == "patch" && d.target != "" {
		if sk, ok := s.editableSkill(ctx, d.target); ok {
			existing = &sk
		}
	}
	// A new skill whose name is one of the agent's custom skills patches it instead.
	if existing == nil {
		if sk, ok := s.editableSkill(ctx, d.name); ok {
			existing = &sk
		}
	}
	if existing != nil {
		d.name = existing.Name
	}
	pkg, err := renderSkill(d)
	if err != nil {
		return nil, err
	}
	if existing != nil {
		prev := existing.Content
		return &draft{action: learning.ActionPatch, skillName: d.name, pkg: pkg, previousContent: &prev}, nil
	}
	return &draft{action: learning.ActionCreate, skillName: d.name, pkg: pkg}, nil
}

// editableSkill returns a custom skill enabled for this agent (one the learner may patch).
func (s *System) editableSkill(ctx context.Context, name string) (skill.Skill, bool) {
	sk, ok, err := s.d.Skills.Get(ctx, name)
	if err != nil || !ok || sk.Source != skill.SourceCustom {
		return skill.Skill{}, false
	}
	return sk, true
}

// learnedSkillSlots is how many more skills the agent may learn: the cap minus learned
// skills and pending new ones.
func (s *System) learnedSkillSlots(ctx context.Context, cfg learning.ResolvedConfig) (int, error) {
	learned, err := s.store().ListLearnedSkills(ctx, s.d.AgentID)
	if err != nil {
		return 0, err
	}
	pending, err := s.store().List(ctx, s.d.AgentID, learning.ProposalFilter{Status: learning.StatusPending})
	if err != nil {
		return 0, err
	}
	n := cfg.MaxLearnedSkills - len(learned)
	for _, p := range pending {
		if p.Action == learning.ActionCreate {
			n--
		}
	}
	return n, nil
}

// supersedePending rejects an older pending draft of the same skill.
func (s *System) supersedePending(ctx context.Context, skillName, now string) error {
	pending, err := s.store().List(ctx, s.d.AgentID, learning.ProposalFilter{Status: learning.StatusPending})
	if err != nil {
		return err
	}
	for _, old := range pending {
		if old.SkillName != skillName || old.Action == learning.ActionRetire {
			continue
		}
		old.Status, old.Reason, old.DecidedAt, old.UpdatedAt = learning.StatusRejected, "Replaced by a newer draft.", now, now
		if err := s.store().Save(ctx, old); err != nil {
			return err
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// Review - accept / reject (the queue's buttons; auto mode accepts itself)
// ---------------------------------------------------------------------------

// ListProposals returns this agent's proposals, newest first.
func (s *System) ListProposals(ctx context.Context, filter learning.ProposalFilter) ([]learning.Proposal, error) {
	return s.store().List(ctx, s.d.AgentID, filter)
}

// GetProposal returns one of this agent's proposals, or nil.
func (s *System) GetProposal(ctx context.Context, id string) (*learning.Proposal, error) {
	p, err := s.store().Get(ctx, id)
	if err != nil || p == nil || p.AgentID != s.d.AgentID {
		return nil, err
	}
	return p, nil
}

// Accept accepts a pending proposal (see AgentSkillLearning).
func (s *System) Accept(ctx context.Context, id string, edited *skill.Package) (learning.Proposal, error) {
	p, err := s.pendingProposal(ctx, id)
	if err != nil {
		return learning.Proposal{}, err
	}
	return s.apply(ctx, p, edited, false)
}

// Reject rejects a pending proposal (see AgentSkillLearning).
func (s *System) Reject(ctx context.Context, id, reason string) (learning.Proposal, error) {
	p, err := s.pendingProposal(ctx, id)
	if err != nil {
		return learning.Proposal{}, err
	}
	now := s.iso()
	p.Status, p.DecidedAt, p.UpdatedAt = learning.StatusRejected, now, now
	if r := strings.TrimSpace(reason); r != "" {
		p.Reason = head(r, 1000)
	}
	if err := s.store().Save(ctx, p); err != nil {
		return learning.Proposal{}, err
	}
	events.Emit(s.d.Events, events.SkillLearningRejected, p.Episode.SessionID, events.SkillLearningReviewedPayload{
		AgentID: s.d.AgentID, ProposalID: p.ID, Action: p.Action, SkillName: p.SkillName, Reason: p.Reason,
	})
	return p, nil
}

// ListLearnedSkills returns the skills this agent learned, with their health.
func (s *System) ListLearnedSkills(ctx context.Context) ([]learning.LearnedSkill, error) {
	return s.store().ListLearnedSkills(ctx, s.d.AgentID)
}

func (s *System) pendingProposal(ctx context.Context, id string) (learning.Proposal, error) {
	p, err := s.GetProposal(ctx, id)
	if err != nil {
		return learning.Proposal{}, err
	}
	if p == nil {
		return learning.Proposal{}, kernel.NewSkillError(`Skill proposal "`+id+`" not found`, kernel.SkillCodeNotFound, "", false)
	}
	if p.Status != learning.StatusPending {
		return learning.Proposal{}, kernel.NewSkillError(`Skill proposal "`+id+`" was already `+string(p.Status), kernel.SkillCodeInvalid, p.SkillName, false)
	}
	return *p, nil
}

// apply makes the skill change a proposal stands for, then records it as accepted.
func (s *System) apply(ctx context.Context, p learning.Proposal, edited *skill.Package, auto bool) (learning.Proposal, error) {
	if s.d.Skills == nil {
		return learning.Proposal{}, kernel.NewSkillError("this agent has no skill library", kernel.SkillCodeNotAllowed, p.SkillName, false)
	}
	store := s.store()
	now := s.iso()
	pkg := p.Draft
	if edited != nil {
		pkg = edited
	}
	name := p.SkillName
	switch p.Action {
	case learning.ActionRetire:
		if err := s.d.Skills.Delete(ctx, name); err != nil {
			// Already gone (deleted by hand): retiring it is done.
			if !kernel.HasCode(err, kernel.SkillCodeNotFound) {
				return learning.Proposal{}, err
			}
		}
		if err := store.DeleteLearnedSkill(ctx, s.d.AgentID, name); err != nil {
			return learning.Proposal{}, err
		}
	case learning.ActionCreate:
		if pkg == nil {
			return learning.Proposal{}, kernel.NewSkillError(`Skill proposal "`+p.ID+`" has no draft`, kernel.SkillCodeInvalid, name, false)
		}
		sk, err := s.d.Skills.Create(ctx, *pkg)
		if err != nil {
			return learning.Proposal{}, err
		}
		name = sk.Name
		if err := store.SaveLearnedSkill(ctx, learning.LearnedSkill{
			AgentID: s.d.AgentID, SkillName: name, ProposalID: p.ID, Health: learning.StartHealth, CreatedAt: now, UpdatedAt: now,
		}); err != nil {
			return learning.Proposal{}, err
		}
	default: // patch
		if pkg == nil {
			return learning.Proposal{}, kernel.NewSkillError(`Skill proposal "`+p.ID+`" has no draft`, kernel.SkillCodeInvalid, name, false)
		}
		if _, err := s.d.Skills.Update(ctx, name, *pkg); err != nil {
			return learning.Proposal{}, err
		}
		// A patched learned skill gets a fresh chance.
		rec, err := store.GetLearnedSkill(ctx, s.d.AgentID, name)
		if err != nil {
			return learning.Proposal{}, err
		}
		if rec != nil && rec.Health < learning.StartHealth {
			rec.Health, rec.UpdatedAt = learning.StartHealth, now
			if err := store.SaveLearnedSkill(ctx, *rec); err != nil {
				return learning.Proposal{}, err
			}
		}
	}
	accepted := p.Clone()
	accepted.SkillName = name
	if pkg != nil && p.Action != learning.ActionRetire {
		cp := *pkg
		accepted.Draft = &cp
		accepted.Summary = descriptionOf(cp)
	}
	accepted.Status, accepted.DecidedAt, accepted.UpdatedAt = learning.StatusAccepted, now, now
	if auto {
		accepted.Auto = true
	}
	if err := store.Save(ctx, accepted); err != nil {
		return learning.Proposal{}, err
	}
	if !auto {
		events.Emit(s.d.Events, events.SkillLearningAccepted, p.Episode.SessionID, events.SkillLearningReviewedPayload{
			AgentID: s.d.AgentID, ProposalID: p.ID, Action: p.Action, SkillName: name, Edited: edited != nil,
		})
	}
	events.Emit(s.d.Events, events.SkillLearningSkillUpdated, p.Episode.SessionID, events.SkillLearningSkillUpdatedPayload{
		AgentID: s.d.AgentID, ProposalID: p.ID, Action: p.Action, SkillName: name, Auto: auto,
	})
	return accepted, nil
}

// ---------------------------------------------------------------------------
// Stage 5 (A-172): learned-skill health, retire proposals
// ---------------------------------------------------------------------------

// trackUsage records each learned skill the turn used: smooth -> health up, struggled ->
// down (at 0 a retire proposal). It reports whether a learned skill was used and the
// turn struggled.
func (s *System) trackUsage(ctx context.Context, ep learning.Episode, cfg learning.ResolvedConfig) bool {
	if len(ep.SkillsUsed) == 0 {
		return false
	}
	store := s.store()
	hard := struggled(ep, cfg)
	usedLearnedAndStruggled := false
	for _, name := range ep.SkillsUsed {
		rec, err := store.GetLearnedSkill(ctx, s.d.AgentID, name)
		if err != nil || rec == nil {
			continue
		}
		delta := 1
		if hard {
			delta = -1
		}
		u := *rec
		u.Health = min(learning.MaxHealth, max(0, rec.Health+delta))
		u.Uses++
		if hard {
			u.StruggledUses++
		} else {
			u.SmoothUses++
		}
		u.LastUsedAt, u.UpdatedAt = ep.EndedAt, ep.EndedAt
		if err := store.SaveLearnedSkill(ctx, u); err != nil {
			s.logger.Error("skill-learning: health not saved", "agent", s.d.AgentID, "skill", name, "error", err)
			continue
		}
		payload := events.SkillLearningSkillUsedPayload{
			AgentID: s.d.AgentID, SkillName: name, Smooth: !hard, Health: u.Health,
			ToolCalls: ep.ToolCalls, FailedCalls: ep.FailedCalls,
		}
		if origin, err := store.Get(ctx, rec.ProposalID); err == nil && origin != nil {
			payload.LearnedFrom = &events.SkillLearningCallCounts{ToolCalls: origin.Episode.ToolCalls, FailedCalls: origin.Episode.FailedCalls}
		}
		events.Emit(s.d.Events, events.SkillLearningSkillUsed, ep.SessionID, payload)
		if hard {
			usedLearnedAndStruggled = true
		}
		if u.Health == 0 {
			if sk, ok, err := s.d.Skills.Get(ctx, name); err == nil && ok {
				why := fmt.Sprintf("Still struggled in %d of %d uses; its health reached 0.", u.StruggledUses, u.Uses)
				s.enqueue("", func(ctx context.Context) { s.proposeRetire(ctx, u, sk, why) }) //nolint:contextcheck // background work runs on the lifetime context on purpose
			}
		}
	}
	return usedLearnedAndStruggled
}

// maybeCheckRetirements looks, at most once an hour, for learned skills unused for
// RetireAfterDays.
func (s *System) maybeCheckRetirements() {
	now := s.now()
	s.mu.Lock()
	if !s.lastRetireCheck.IsZero() && now.Sub(s.lastRetireCheck) < retireCheckInterval {
		s.mu.Unlock()
		return
	}
	s.lastRetireCheck = now
	s.mu.Unlock()
	s.enqueue("", s.checkRetirements)
}

func (s *System) checkRetirements(ctx context.Context) {
	store := s.store()
	recs, err := store.ListLearnedSkills(ctx, s.d.AgentID)
	if err != nil {
		s.logger.Error("skill-learning: learned skills not listed", "agent", s.d.AgentID, "error", err)
		return
	}
	cfg := s.config()
	now := s.now()
	for _, rec := range recs {
		sk, ok, err := s.d.Skills.Get(ctx, rec.SkillName)
		if err != nil {
			continue
		}
		if !ok {
			// Deleted or disabled by hand: nothing left to track.
			_ = store.DeleteLearnedSkill(ctx, s.d.AgentID, rec.SkillName)
			continue
		}
		if rec.Health <= 0 {
			s.proposeRetire(ctx, rec, sk, fmt.Sprintf("Its health reached 0 after %d struggling uses.", rec.StruggledUses))
			continue
		}
		if cfg.RetireAfterDays <= 0 {
			continue
		}
		last := rec.LastUsedAt
		if last == "" {
			last = rec.CreatedAt
		}
		t, err := time.Parse(time.RFC3339Nano, last)
		if err != nil {
			continue
		}
		if idle := int(now.Sub(t) / day); idle >= cfg.RetireAfterDays {
			s.proposeRetire(ctx, rec, sk, fmt.Sprintf("Not used for %d days.", idle))
		}
	}
}

// proposeRetire proposes deleting a learned skill - always for review, even in auto mode.
func (s *System) proposeRetire(ctx context.Context, rec learning.LearnedSkill, sk skill.Skill, why string) {
	store := s.store()
	pending, err := store.List(ctx, s.d.AgentID, learning.ProposalFilter{Status: learning.StatusPending})
	if err != nil {
		return
	}
	for _, p := range pending {
		if p.SkillName == rec.SkillName {
			return
		}
	}
	now := s.iso()
	prev := sk.Content
	p := learning.Proposal{
		ID: kernel.NewID(), AgentID: s.d.AgentID, Action: learning.ActionRetire, SkillName: rec.SkillName, Summary: why,
		PreviousContent: &prev, Episode: learning.EpisodeRef{SkillsUsed: []string{}, EndedAt: now},
		Status: learning.StatusPending, CreatedAt: now, UpdatedAt: now,
	}
	if err := store.Save(ctx, p); err != nil {
		s.logger.Error("skill-learning: retire proposal not saved", "agent", s.d.AgentID, "error", err)
		return
	}
	events.Emit(s.d.Events, events.SkillLearningProposed, "", events.SkillLearningProposedPayload{
		AgentID: s.d.AgentID, ProposalID: p.ID, Action: learning.ActionRetire, SkillName: rec.SkillName,
	})
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// loadTurn returns the turn's messages, its user message first; later turns cut off.
func (s *System) loadTurn(ctx context.Context, ep learning.Episode) ([]kernel.Message, error) {
	msgs := s.d.Persistence().Messages
	wanted := ep.MessageEnd
	if wanted < 0 {
		n, err := msgs.Count(ctx, ep.SessionID)
		if err != nil {
			return nil, err
		}
		wanted = n
	}
	slice, err := msgs.GetRange(ctx, ep.SessionID, wanted, maxTurnMessages)
	if err != nil {
		return nil, err
	}
	list := slice.Messages
	// Index of list[0] in the session.
	offset := min(wanted, slice.Total) - len(list)
	// The turn starts at its user message: at MessageStart, or the nearest user message
	// before it (or before the end, when the start is not known).
	from := len(list) - 1
	if ep.MessageStart >= 0 {
		from = min(ep.MessageStart-offset, len(list)-1)
	}
	for i := from; i >= 0; i-- {
		if list[i].Role == kernel.RoleUser {
			return turnMessages(list[i:]), nil
		}
	}
	return []kernel.Message{}, nil
}

// enqueue runs job after the earlier jobs of key, on that key's goroutine. It reports
// false (and drops the job) once the System is stopped.
func (s *System) enqueue(key string, job func(ctx context.Context)) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopped {
		return false
	}
	c := s.chains[key]
	if c != nil {
		c.jobs = append(c.jobs, job)
		return true
	}
	c = &chain{jobs: []func(context.Context){job}, done: make(chan struct{})}
	s.chains[key] = c
	lifetime := s.lifetime
	if lifetime == nil {
		lifetime = context.Background()
	}
	go s.drain(lifetime, key, c)
	return true
}

func (s *System) drain(lifetime context.Context, key string, c *chain) {
	defer close(c.done)
	for {
		s.mu.Lock()
		if len(c.jobs) == 0 {
			delete(s.chains, key)
			s.mu.Unlock()
			return
		}
		job := c.jobs[0]
		c.jobs = c.jobs[1:]
		s.mu.Unlock()
		s.run(lifetime, key, job)
	}
}

func (s *System) run(lifetime context.Context, key string, job func(context.Context)) {
	defer func() {
		if r := recover(); r != nil {
			s.logger.Error("skill-learning: job panicked", "agent", s.d.AgentID, "session", key, "panic", fmt.Sprint(r))
		}
	}()
	ctx, cancel := context.WithTimeout(lifetime, jobTimeout+verifyTimeout)
	defer cancel()
	job(ctx)
}

func (s *System) isStopped() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stopped
}

func stopTimer(t *finishedTurn) {
	if t.timer != nil {
		t.timer.Stop()
		t.timer = nil
	}
}

// learnableSession: job, subagent, group-member and verification sessions have nobody to
// confirm a success.
func learnableSession(session *kernel.Session) bool {
	if session == nil || session.JobID != "" {
		return false
	}
	v, _ := session.Metadata.Get("source")
	switch v {
	case "subagent", "group-member", VerificationSource:
		return false
	}
	return true
}

func episodeRef(ep learning.CapturedEpisode, msgs []kernel.Message) learning.EpisodeRef {
	request := ""
	if len(msgs) > 0 {
		request = head(strings.TrimSpace(spaces.ReplaceAllString(msgs[0].Content, " ")), maxEpisodeRequest)
	}
	return learning.EpisodeRef{
		SessionID: ep.SessionID, MessageStart: ep.MessageStart, MessageEnd: ep.MessageEnd,
		ToolCalls: ep.ToolCalls, FailedCalls: ep.FailedCalls, SkillsUsed: slices.Clone(ep.SkillsUsed),
		Signal: ep.Signal, Request: request, EndedAt: ep.EndedAt,
	}
}

// descriptionOf is the package's frontmatter description, for the queue's one line.
func descriptionOf(pkg skill.Package) string {
	parsed, err := skill.ParsePackage(pkg)
	if err != nil {
		return ""
	}
	return head(parsed.Frontmatter.Description, 300)
}
