package persistence

import (
	"context"

	"github.com/Phong95/aec-agent-go/components/agent/skilllearning/learning"
)

// ---------------------------------------------------------------------------
// Skill proposal persistence (A-166) - the skill learner's own state: the drafts it
// wrote from hard-won turns (pending, accepted or rejected) and, per learned skill, its
// health record (A-172). Written only by each agent's skill learner.
// ---------------------------------------------------------------------------

// SkillProposalStore persists skill proposals and learned-skill records.
type SkillProposalStore interface {
	// Save inserts or replaces the proposal by id.
	Save(ctx context.Context, p learning.Proposal) error
	// Get returns the proposal, or a nil pointer and a nil error when the id is unknown.
	Get(ctx context.Context, id string) (*learning.Proposal, error)
	// List returns one agent's proposals, newest first (CreatedAt, then id).
	List(ctx context.Context, agentID string, filter learning.ProposalFilter) ([]learning.Proposal, error)
	// Delete removes the proposal; an unknown id is not an error.
	Delete(ctx context.Context, id string) error

	// SaveLearnedSkill inserts or replaces the record by (AgentID, SkillName).
	SaveLearnedSkill(ctx context.Context, r learning.LearnedSkill) error
	// GetLearnedSkill returns the record, or a nil pointer and a nil error when there is none.
	GetLearnedSkill(ctx context.Context, agentID, skillName string) (*learning.LearnedSkill, error)
	// ListLearnedSkills returns one agent's learned skills, by skill name.
	ListLearnedSkills(ctx context.Context, agentID string) ([]learning.LearnedSkill, error)
	// DeleteLearnedSkill removes the record; an unknown name is not an error.
	DeleteLearnedSkill(ctx context.Context, agentID, skillName string) error

	// DeleteByAgent deletes every proposal and learned-skill record of an agent (the agent
	// was deleted).
	DeleteByAgent(ctx context.Context, agentID string) error
}
