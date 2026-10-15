package skill

import "context"

// ---------------------------------------------------------------------------
// AgentSkills - one agent's handle on the skill library, given to tools as
// toolhub.Context.Skills (skill_view reads through it, skill_manage writes).
//
// Its methods return an error that is a *kernel.SkillError with a skill error
// code.
// ---------------------------------------------------------------------------

// AgentSkills is one agent's handle on the skill library.
type AgentSkills interface {
	// List returns the skills the agent can use now: enabled for it and every
	// required tool available.
	List(ctx context.Context) ([]Skill, error)
	// Get returns a skill enabled for this agent (even if a required tool is
	// missing); the bool is false when there is none.
	Get(ctx context.Context, name string) (Skill, bool, error)
	// Create adds a custom skill to the library and enables it for this agent.
	// Other agents can enable it later. Errors: SkillError "invalid",
	// "already_exists".
	Create(ctx context.Context, pkg Package) (Skill, error)
	// Update replaces a custom skill enabled for this agent (SKILL.md and all its
	// files). The name cannot change. Errors: SkillError "not_found",
	// "not_allowed", "read_only", "invalid".
	Update(ctx context.Context, name string, pkg Package) (Skill, error)
	// Delete deletes a custom skill enabled for this agent, and disables it.
	// Errors are SkillError.
	Delete(ctx context.Context, name string) error
}
