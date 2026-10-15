package agent

import (
	"context"
	"log/slog"
	"slices"
	"sync"

	"github.com/Phong95/aec-agent-go/components/agent/agentloop"
	"github.com/Phong95/aec-agent-go/components/agent/profile"
	"github.com/Phong95/aec-agent-go/components/skillhub"
	"github.com/Phong95/aec-agent-go/components/skillhub/skill"
	"github.com/Phong95/aec-agent-go/components/toolhub"
	"github.com/Phong95/aec-agent-go/components/toolhub/builtin/skills"
	"github.com/Phong95/aec-agent-go/kernel"
)

// SkillManager manages all the skills an agent has.
//
// It resolves skills lazily from the shared skill hub on every access, so that
//   - a skill registered after the agent is spawned is visible (startup ordering),
//   - an allowlist change through SetAllowedSkills takes effect immediately.
//
// The agent's skills are the ones in its allowlist (profile.Skills) whose required
// tools it has. They reach the model as the <available_skills> index in the system
// message (BuildIndex) plus the skill_view tool, which ToolDefinitions adds whenever
// there is at least one usable skill.
//
// It is the agentloop.Skills port (Tool and ToolDefinitions) and also the agent's
// handle on the library, handed to tools as toolhub.Context.Skills (skill.AgentSkills),
// so a tool only sees List, Get, Create, Update and Delete.
//
// Locking: the allowlist sits behind a read-write mutex, copied on the way in and on
// every read, and no lock of the manager is held across a call into the skill hub.
// The hub's OnDeleted listener may therefore call Agent.UpdateSkills (and so
// SetAllowedSkills) from inside SkillManager.Delete without a deadlock. The one
// other lock, changeMu, serializes the read-modify-write of an allowlist change and
// is held across the onAllowlistChange hook (which saves the profile), never across
// the hub.
//
// It is safe for concurrent use.
type SkillManager struct {
	hub     *skillhub.SkillHub
	agentID string
	logger  *slog.Logger
	view    *skills.ViewTool

	hasTool           func(name string) bool
	onAllowlistChange func(ctx context.Context, names []string) error

	// changeMu serializes the allowlist changes of Create and Delete, so two of them
	// at once cannot lose one. It is taken before mu and never held while the hub is
	// called.
	changeMu sync.Mutex

	mu      sync.RWMutex
	allowed []string
}

var (
	_ agentloop.Skills  = (*SkillManager)(nil)
	_ skill.AgentSkills = (*SkillManager)(nil)
)

// SkillManagerOption configures a SkillManager.
type SkillManagerOption func(*SkillManager)

// WithHasTool sets the check for a tool of the agent: a skill
// whose required tools the agent lacks is hidden from List and the index. The
// default says every tool is available. A nil function is ignored.
func WithHasTool(f func(name string) bool) SkillManagerOption {
	return func(m *SkillManager) {
		if f != nil {
			m.hasTool = f
		}
	}
}

// WithOnAllowlistChange sets the hook called with the new allowlist when a skill the
// agent created or deleted changes it; persist the profile there. The hook is
// responsible for applying the list, normally through Agent.UpdateSkills. Without it
// the manager applies the list itself. An error from the hook is returned by Create
// or Delete. A nil function is ignored.
func WithOnAllowlistChange(f func(ctx context.Context, names []string) error) SkillManagerOption {
	return func(m *SkillManager) {
		if f != nil {
			m.onAllowlistChange = f
		}
	}
}

// WithSkillLogger sets the logger that receives the warning about a failing skill
// store (default slog.Default()). A nil logger is ignored.
func WithSkillLogger(l *slog.Logger) SkillManagerOption {
	return func(m *SkillManager) {
		if l != nil {
			m.logger = l
		}
	}
}

// NewSkillManager returns a SkillManager over hub for the agent of profile p, with
// p.Skills (copied) as the allowlist.
func NewSkillManager(hub *skillhub.SkillHub, p profile.AgentProfile, opts ...SkillManagerOption) *SkillManager {
	m := &SkillManager{
		hub:     hub,
		agentID: p.ID,
		logger:  slog.Default(),
		view:    skills.NewViewTool(),
		hasTool: func(string) bool { return true },
		allowed: slices.Clone(p.Skills),
	}
	for _, opt := range opts {
		if opt != nil {
			opt(m)
		}
	}
	return m
}

// allowlist returns a copy of the current allowlist.
func (m *SkillManager) allowlist() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return slices.Clone(m.allowed)
}

// Get returns a skill enabled for this agent, whether or not its required tools are
// available. The bool is false when the skill is not enabled or the library has none.
func (m *SkillManager) Get(ctx context.Context, name string) (skill.Skill, bool, error) {
	if !slices.Contains(m.allowlist(), name) {
		return skill.Skill{}, false, nil
	}
	return m.hub.Get(ctx, name)
}

// List returns the skills the agent can use now: enabled and every required tool
// available, in the allowlist's order.
func (m *SkillManager) List(ctx context.Context) ([]skill.Skill, error) {
	all, err := m.hub.GetByNames(ctx, m.allowlist())
	if err != nil {
		return nil, err
	}
	usable := make([]skill.Skill, 0, len(all))
	for _, s := range all {
		if slices.ContainsFunc(s.RequiresTools, func(t string) bool { return !m.hasTool(t) }) {
			continue
		}
		usable = append(usable, s)
	}
	return usable, nil
}

// Create adds a custom skill to the library and enables it for this agent: the hub
// records the agent as its creator, and the allowlist change runs when the name is
// not allowed yet. When the hook fails, the skill stays in the library and Create
// returns it together with the error, as SpawnAgent does for a failed profile save.
func (m *SkillManager) Create(ctx context.Context, pkg skill.Package) (skill.Skill, error) {
	created, err := m.hub.Create(ctx, pkg, skillhub.CreateOptions{CreatedBy: m.agentID})
	if err != nil {
		return skill.Skill{}, err
	}
	err = m.changeAllowlist(ctx, func(cur []string) ([]string, bool) {
		if slices.Contains(cur, created.Name) {
			return nil, false
		}
		return append(cur, created.Name), true
	})
	return created, err
}

// Update replaces a custom skill enabled for this agent. A skill that does not exist
// is a not_found SkillError and one that is not enabled for the agent a not_allowed
// SkillError.
func (m *SkillManager) Update(ctx context.Context, name string, pkg skill.Package) (skill.Skill, error) {
	if err := m.requireEnabled(ctx, name); err != nil {
		return skill.Skill{}, err
	}
	return m.hub.Update(ctx, name, pkg)
}

// Delete deletes a custom skill enabled for this agent and disables it. The errors
// are those of Update.
func (m *SkillManager) Delete(ctx context.Context, name string) error {
	if err := m.requireEnabled(ctx, name); err != nil {
		return err
	}
	if err := m.hub.Delete(ctx, name); err != nil {
		return err
	}
	// Inside an AgentCore the hub's onDeleted listener has already disabled it for every agent.
	return m.changeAllowlist(ctx, func(cur []string) ([]string, bool) {
		if !slices.Contains(cur, name) {
			return nil, false
		}
		return slices.DeleteFunc(cur, func(n string) bool { return n == name }), true
	})
}

// BuildIndex returns the <available_skills> block for the system message. The bool
// is false when the agent has no usable skills. A failing skill store never blocks a
// turn: the failure is logged and the turn runs without skills.
func (m *SkillManager) BuildIndex(ctx context.Context) (string, bool) {
	return FormatSkillIndex(m.listForTurn(ctx), SkillIndexMaxChars)
}

// ToolDefinitions returns the tools the skill system adds to the model's tools this
// turn: skill_view when there is at least one usable skill, none otherwise. It never
// fails; a failing skill store is logged and counts as no skills.
func (m *SkillManager) ToolDefinitions(ctx context.Context) ([]toolhub.Definition, error) {
	if len(m.listForTurn(ctx)) == 0 {
		return []toolhub.Definition{}, nil
	}
	return []toolhub.Definition{m.view.Definition()}, nil
}

// listForTurn is List for a model call: a failing skill store is logged with the
// agent id and gives no skills.
func (m *SkillManager) listForTurn(ctx context.Context) []skill.Skill {
	list, err := m.List(ctx)
	if err != nil {
		m.logger.Warn("skills: could not load skills for agent "+m.agentID, "error", err)
		return nil
	}
	return list
}

// Tool returns the skill tool registered under name: skill_view, for the
// loop to dispatch. Any other name has none.
func (m *SkillManager) Tool(name string) (toolhub.Handler, bool) {
	if name == m.view.Definition().Name {
		return m.view, true
	}
	return nil, false
}

// SetAllowedSkills replaces the skill allowlist (for example after a profile
// update); names is copied.
func (m *SkillManager) SetAllowedSkills(names []string) {
	names = slices.Clone(names)
	m.mu.Lock()
	defer m.mu.Unlock()
	m.allowed = names
}

// changeAllowlist applies one allowlist change. mutate gets a
// copy of the current list and returns the new one and whether to apply it; the
// read-modify-write is serialized by changeMu. With the hook set the hook applies
// the list, without it the manager does.
func (m *SkillManager) changeAllowlist(ctx context.Context, mutate func(cur []string) ([]string, bool)) error {
	m.changeMu.Lock()
	defer m.changeMu.Unlock()
	names, apply := mutate(m.allowlist())
	if !apply {
		return nil
	}
	if m.onAllowlistChange != nil {
		return m.onAllowlistChange(ctx, names)
	}
	m.SetAllowedSkills(names)
	return nil
}

// disable takes name out of the allowlist as one allowlist change (see
// Agent.DisableSkill). The bool says whether the name was enabled.
func (m *SkillManager) disable(ctx context.Context, name string) (bool, error) {
	changed := false
	err := m.changeAllowlist(ctx, func(cur []string) ([]string, bool) {
		if !slices.Contains(cur, name) {
			return nil, false
		}
		changed = true
		return slices.DeleteFunc(cur, func(n string) bool { return n == name }), true
	})
	return changed, err
}

// requireEnabled checks that the skill exists and is enabled for this agent.
// The existence check goes first.
func (m *SkillManager) requireEnabled(ctx context.Context, name string) error {
	_, found, err := m.hub.Get(ctx, name)
	if err != nil {
		return err
	}
	if !found {
		return kernel.NewSkillError(`Skill "`+name+`" not found`, kernel.SkillCodeNotFound, name, false)
	}
	if !slices.Contains(m.allowlist(), name) {
		return kernel.NewSkillError(`Skill "`+name+`" is not enabled for this agent, so it cannot change it`, kernel.SkillCodeNotAllowed, name, false)
	}
	return nil
}
