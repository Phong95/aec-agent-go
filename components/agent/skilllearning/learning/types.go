// Package learning holds the type-only contracts of skill learning (A-166): the config,
// the episode (one finished turn as the learner saw it), the proposal (a draft skill
// waiting for review) and the learned-skill health record (A-172). It is a leaf, so the
// event payloads, the persistence contract and the agent profile can carry these types
// without importing the skill learner itself.
package learning

import (
	"math"

	"github.com/Phong95/aec-agent-go/components/skillhub/skill"
)

// ---------------------------------------------------------------------------
// Config - off by default and set per agent (AgentProfile.SkillLearning), like the
// memory config.
// ---------------------------------------------------------------------------

// Mode is what happens to a draft skill.
type Mode string

// The modes.
const (
	// ModePropose: the draft waits in the review queue for the user to accept, edit or
	// reject it.
	ModePropose Mode = "propose"
	// ModeAuto: the draft is saved without review (experimental). A-174: only behind a
	// verification replay that passed; otherwise it waits for review.
	ModeAuto Mode = "auto"
)

// Config is the skill learning config of an agent. Every number is optional (nil = the
// default) and clamped by ResolveConfig.
type Config struct {
	// Enabled is off by default: nothing subscribes while it is off.
	Enabled bool `json:"enabled"`
	// Mode defaults to "propose".
	Mode Mode `json:"mode,omitempty"`
	// MinToolCalls: a turn needs at least this many tool calls to be a candidate (default 8).
	MinToolCalls *int `json:"minToolCalls,omitempty"`
	// MinFailedCalls: ...and at least this many failed or retried calls (default 2). Denied
	// calls do not count.
	MinFailedCalls *int `json:"minFailedCalls,omitempty"`
	// VerdictWaitMinutes is how long to wait for the user's next message before giving up
	// on a candidate turn (default 30). Silence is not a success: only /learn can still
	// confirm it then.
	VerdictWaitMinutes *int `json:"verdictWaitMinutes,omitempty"`
	// MaxLearnedSkills is the most learned skills this agent keeps, so the skill index
	// stays within its budget (default 20).
	MaxLearnedSkills *int `json:"maxLearnedSkills,omitempty"`
	// RetireAfterDays (A-172): a learned skill not used for this many days is proposed
	// for retirement (default 60; 0 = never by age).
	RetireAfterDays *int `json:"retireAfterDays,omitempty"`
	// VerificationsPerDay (A-174): auto mode saves a draft only after a verification
	// replay passed; at most this many replays a day (default 5; 0 = none, so every draft
	// waits for review).
	VerificationsPerDay *int `json:"verificationsPerDay,omitempty"`
}

// Clone returns a deep copy of c (nil stays nil).
func (c *Config) Clone() *Config {
	if c == nil {
		return nil
	}
	cp := *c
	for _, p := range []**int{&cp.MinToolCalls, &cp.MinFailedCalls, &cp.VerdictWaitMinutes, &cp.MaxLearnedSkills, &cp.RetireAfterDays, &cp.VerificationsPerDay} {
		if *p != nil {
			v := **p
			*p = &v
		}
	}
	return &cp
}

// The defaults.
const (
	DefaultMinToolCalls        = 8
	DefaultMinFailedCalls      = 2
	DefaultVerdictWaitMinutes  = 30
	DefaultMaxLearnedSkills    = 20
	DefaultRetireAfterDays     = 60
	DefaultVerificationsPerDay = 5
)

// ResolvedConfig is the config with every default filled in and every number clamped. It
// is a runtime value, not wire data.
type ResolvedConfig struct {
	Enabled             bool
	Mode                Mode
	MinToolCalls        int
	MinFailedCalls      int
	VerdictWaitMinutes  int
	MaxLearnedSkills    int
	RetireAfterDays     int
	VerificationsPerDay int
}

func clampInt(v *int, fallback, lo, hi int) int {
	if v == nil {
		return fallback
	}
	return int(math.Min(math.Max(float64(*v), float64(lo)), float64(hi)))
}

// ResolveConfig fills in the defaults and clamps the numbers (a profile may hold
// anything a client sent). A nil config is off.
func ResolveConfig(c *Config) ResolvedConfig {
	if c == nil {
		c = &Config{}
	}
	mode := ModePropose
	if c.Mode == ModeAuto {
		mode = ModeAuto
	}
	return ResolvedConfig{
		Enabled:             c.Enabled,
		Mode:                mode,
		MinToolCalls:        clampInt(c.MinToolCalls, DefaultMinToolCalls, 1, 1000),
		MinFailedCalls:      clampInt(c.MinFailedCalls, DefaultMinFailedCalls, 0, 1000),
		VerdictWaitMinutes:  clampInt(c.VerdictWaitMinutes, DefaultVerdictWaitMinutes, 1, 7*24*60),
		MaxLearnedSkills:    clampInt(c.MaxLearnedSkills, DefaultMaxLearnedSkills, 1, 200),
		RetireAfterDays:     clampInt(c.RetireAfterDays, DefaultRetireAfterDays, 0, 3650),
		VerificationsPerDay: clampInt(c.VerificationsPerDay, DefaultVerificationsPerDay, 0, 100),
	}
}

// Config returns the resolved config as a Config with every field set (what the servers
// store after clamping).
func (r ResolvedConfig) Config() Config {
	v := func(n int) *int { return &n }
	return Config{
		Enabled: r.Enabled, Mode: r.Mode,
		MinToolCalls: v(r.MinToolCalls), MinFailedCalls: v(r.MinFailedCalls),
		VerdictWaitMinutes: v(r.VerdictWaitMinutes), MaxLearnedSkills: v(r.MaxLearnedSkills),
		RetireAfterDays: v(r.RetireAfterDays), VerificationsPerDay: v(r.VerificationsPerDay),
	}
}

// ---------------------------------------------------------------------------
// Episode - one finished turn as the learner saw it on the event bus (the site diary).
// Only counts and names: the turn's messages are read later from the MessageStore.
// ---------------------------------------------------------------------------

// Outcome is how a turn ended. Only "completed" turns can be learned from.
type Outcome string

// The outcomes.
const (
	OutcomeCompleted     Outcome = "completed"
	OutcomeAborted       Outcome = "aborted"
	OutcomeMaxIterations Outcome = "max-iterations"
)

// Signal is what confirmed a turn was a success.
type Signal string

// The signals.
const (
	// SignalLearnCommand: the user ran /learn after the turn.
	SignalLearnCommand Signal = "learn-command"
	// SignalNextMessage: the user's next message accepted the result.
	SignalNextMessage Signal = "next-message"
)

// Episode is one finished turn.
type Episode struct {
	AgentID   string `json:"agentId"`
	SessionID string `json:"sessionId"`
	// MessageStart is the index (MessageStore order) of the turn's user message; -1 when
	// it is not known.
	MessageStart int `json:"messageStart"`
	// MessageEnd is the index after the turn's last message (exclusive).
	MessageEnd int `json:"messageEnd"`
	// ToolCalls the turn made (skill_view calls excluded).
	ToolCalls int `json:"toolCalls"`
	// FailedCalls failed or were retried at least once.
	FailedCalls int `json:"failedCalls"`
	// DeniedCalls the user (or a policy) refused - a decision, not a failed approach.
	DeniedCalls int `json:"deniedCalls"`
	// SkillsUsed are the skills the turn loaded with skill_view, in order of first use.
	SkillsUsed []string `json:"skillsUsed"`
	Outcome    Outcome  `json:"outcome"`
	// StartedAt and EndedAt are ISO timestamps.
	StartedAt string `json:"startedAt"`
	EndedAt   string `json:"endedAt"`
}

// CapturedEpisode is a confirmed success worth distilling.
type CapturedEpisode struct {
	Episode
	Signal Signal `json:"signal"`
	// PatchTarget is a learned or custom skill the turn used and still struggled with -
	// the distiller is asked to patch it instead of writing a new one.
	PatchTarget string `json:"patchTarget,omitempty"`
}

// ---------------------------------------------------------------------------
// Proposal - a draft the learner wrote from an episode, waiting for the user's review
// (or saved straight away in auto mode). Kept in the SkillProposalStore.
// ---------------------------------------------------------------------------

// ProposalStatus is where a proposal is in the review.
type ProposalStatus string

// The statuses.
const (
	StatusPending  ProposalStatus = "pending"
	StatusAccepted ProposalStatus = "accepted"
	StatusRejected ProposalStatus = "rejected"
)

// ProposalAction is what accepting the proposal does.
type ProposalAction string

// The actions.
const (
	// ActionCreate adds a new custom skill and enables it for the agent.
	ActionCreate ProposalAction = "create"
	// ActionPatch replaces an existing custom skill (the same name).
	ActionPatch ProposalAction = "patch"
	// ActionRetire deletes a learned skill that stopped helping (A-172).
	ActionRetire ProposalAction = "retire"
)

// EpisodeRef is where a proposal came from: one turn of one session.
type EpisodeRef struct {
	SessionID    string   `json:"sessionId"`
	MessageStart int      `json:"messageStart"`
	MessageEnd   int      `json:"messageEnd"`
	ToolCalls    int      `json:"toolCalls"`
	FailedCalls  int      `json:"failedCalls"`
	SkillsUsed   []string `json:"skillsUsed"`
	// Signal is empty for a retire proposal, which no success started.
	Signal Signal `json:"signal,omitempty"`
	// Request is the user's request (first characters), to show in the review queue.
	Request string `json:"request"`
	EndedAt string `json:"endedAt"`
}

// Verification (A-174) is how the verification replay of a draft went: it passes only
// when all three checks hold.
type Verification struct {
	Passed bool `json:"passed"`
	// Solved: a model judge read the replay and found the task solved.
	Solved bool `json:"solved"`
	// SkillUsed: the replay loaded the draft skill (skill_view).
	SkillUsed bool `json:"skillUsed"`
	// ChangedEnvironment: the replay changed its workspace.
	ChangedEnvironment bool `json:"changedEnvironment"`
	// Reason is why it did not pass, or why no replay ran (the draft then waits for review).
	Reason      string `json:"reason,omitempty"`
	ToolCalls   int    `json:"toolCalls"`
	FailedCalls int    `json:"failedCalls"`
	// At is an ISO timestamp.
	At string `json:"at"`
}

// VerificationRequest (A-174) is what a verification replay runs.
type VerificationRequest struct {
	// Request is the user's original request.
	Request   string
	SkillName string
	Draft     skill.Package
}

// Proposal is a draft skill and its review state.
type Proposal struct {
	ID      string         `json:"id"`
	AgentID string         `json:"agentId"`
	Action  ProposalAction `json:"action"`
	// SkillName is the skill to create, patch or retire.
	SkillName string `json:"skillName"`
	// Summary is one line for the queue: the draft's description, or why it should be retired.
	Summary string `json:"summary"`
	// Draft is the draft SKILL.md (and files); nil for "retire".
	Draft *skill.Package `json:"draft,omitempty"`
	// PreviousContent is, for "patch" / "retire", the skill's SKILL.md when the proposal
	// was made, for the diff. Nil = absent.
	PreviousContent *string        `json:"previousContent,omitempty"`
	Episode         EpisodeRef     `json:"episode"`
	Status          ProposalStatus `json:"status"`
	// Auto: saved without review (mode "auto"); the status is then "accepted".
	Auto bool `json:"auto,omitempty"`
	// Verification (A-174) is the verification replay of the draft in auto mode.
	Verification *Verification `json:"verification,omitempty"`
	// Reason is why it was rejected, when the reviewer said.
	Reason string `json:"reason,omitempty"`
	// CreatedAt, UpdatedAt and DecidedAt are ISO timestamps.
	CreatedAt string `json:"createdAt"`
	UpdatedAt string `json:"updatedAt"`
	DecidedAt string `json:"decidedAt,omitempty"`
}

// Clone returns a deep copy of p.
func (p Proposal) Clone() Proposal {
	cp := p
	if p.Draft != nil {
		d := *p.Draft
		d.Files = append([]skill.File(nil), p.Draft.Files...)
		cp.Draft = &d
	}
	if p.PreviousContent != nil {
		s := *p.PreviousContent
		cp.PreviousContent = &s
	}
	if p.Verification != nil {
		v := *p.Verification
		cp.Verification = &v
	}
	cp.Episode.SkillsUsed = append([]string{}, p.Episode.SkillsUsed...)
	return cp
}

// ProposalFilter narrows a listing.
type ProposalFilter struct {
	// Status, when set, keeps only the proposals with this status.
	Status ProposalStatus
}

// LearnedSkill (A-172) is a skill an agent learned, and how it has done since - the
// health score goes up with every smooth use and down with every use that still
// struggled.
type LearnedSkill struct {
	AgentID   string `json:"agentId"`
	SkillName string `json:"skillName"`
	// ProposalID is the proposal that created it.
	ProposalID string `json:"proposalId"`
	// Health is 0..MaxHealth; it starts at StartHealth. At 0 the learner proposes
	// retiring it.
	Health        int `json:"health"`
	Uses          int `json:"uses"`
	SmoothUses    int `json:"smoothUses"`
	StruggledUses int `json:"struggledUses"`
	// CreatedAt, UpdatedAt and LastUsedAt are ISO timestamps.
	CreatedAt  string `json:"createdAt"`
	UpdatedAt  string `json:"updatedAt"`
	LastUsedAt string `json:"lastUsedAt,omitempty"`
}

// A learned skill's health starts here, moves by 1 per use, and stays within [0, MaxHealth].
const (
	StartHealth = 3
	MaxHealth   = 10
)

// LearnStatus is what /learn did.
type LearnStatus string

// The statuses of /learn.
const (
	// LearnCaptured: a draft skill will be proposed (or saved, in auto mode).
	LearnCaptured LearnStatus = "captured"
	// LearnDisabled: skill learning is off for this agent.
	LearnDisabled LearnStatus = "disabled"
	// LearnNoTurn: no finished turn in this conversation (since skill learning was on).
	LearnNoTurn LearnStatus = "no-turn"
	// LearnNotEligible: the last turn was stopped, ran out of steps, used no tools, or is
	// a job's or subagent's.
	LearnNotEligible LearnStatus = "not-eligible"
	// LearnAlreadyCaptured: the last turn was already captured.
	LearnAlreadyCaptured LearnStatus = "already-captured"
)

// LearnResult is what /learn did, with one line for the user.
type LearnResult struct {
	Status LearnStatus `json:"status"`
	Text   string      `json:"text"`
}
