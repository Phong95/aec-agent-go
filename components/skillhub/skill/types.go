// Package skill holds the type-only contracts of the skill library.
//
// Skill - learned or predefined procedures the agent can execute.
//
// A skill is a SKILL.md file (YAML frontmatter + Markdown instructions) plus
// optional files - scripts/, references/, assets/ - in the Agent Skills format
// (agentskills.io), the same format Claude, Hermes Agent and OpenClaw use. A
// skill is NOT a tool: it adds no new abilities, it tells the model how to use
// the tools it already has. Only the name + description are in the prompt; the
// body is loaded on demand (skill_view).
//
// Phase 5 delivers the data types and the AgentSkills contract only. The hub,
// the SKILL.md parser and the essential skills arrive in Phase 6.
package skill

import (
	"slices"

	"github.com/Phong95/aec-agent-go/internal/jsonx"
	"github.com/Phong95/aec-agent-go/kernel/ordered"
)

// File is one extra file of a skill, e.g. "scripts/extract.py" or
// "references/format.md". Text only.
type File struct {
	// Path is a relative POSIX path inside the skill folder.
	Path    string `json:"path"`
	Content string `json:"content"`
}

// Package is what you register or create: the SKILL.md text and the skill's
// other files.
type Package struct {
	// Content is the full SKILL.md: "---" frontmatter (name, description, ...)
	// "---" then the instructions.
	Content string `json:"content"`
	Files   []File `json:"files,omitzero"`
}

// Source says where a skill comes from.
type Source string

// The skill sources.
const (
	// SourceBuiltIn is shipped with the SDK (the essential skills) - read-only.
	SourceBuiltIn Source = "built-in"
	// SourceApp is registered by the host app with Kernel.useSkill() - read-only.
	SourceApp Source = "app"
	// SourceCustom is created at runtime (by an agent through skill_manage, or by
	// a user) and kept in the SkillStore - the only kind that can be edited or deleted.
	SourceCustom Source = "custom"
)

// Frontmatter holds the SKILL.md frontmatter fields (Agent Skills specification). The SKILL.md key "allowed-tools" is read into
// AllowedTools by the Phase 6 parser; on the wire the key is "allowedTools".
type Frontmatter struct {
	// Name is 1-64 chars of a-z, 0-9 and single hyphens; unique in the library.
	Name string `json:"name"`
	// Description says what the skill does and when to use it (1-1024 chars) -
	// this is what the model sees.
	Description string `json:"description"`
	License     string `json:"license,omitempty"`
	// Compatibility holds environment requirements, free text (max 500 chars).
	Compatibility string `json:"compatibility,omitempty"`
	// AllowedTools (`allowed-tools`) is a hint, never a permission boundary.
	AllowedTools []string `json:"allowedTools,omitzero"`
	// Metadata holds extra string key/values. Nested maps are flattened to dotted
	// keys ("hermes.tags"). A nil map is written as {}. It is an ordered map so
	// the keys keep the order the SKILL.md parser produced (JS object order:
	// integer-like keys first, then the order they were written).
	Metadata ordered.Map[string] `json:"metadata"`
}

// MarshalJSON writes an empty Metadata as an empty object. The zero ordered.Map
// already encodes as {}, so no nil normalisation is needed here any more.
func (f Frontmatter) MarshalJSON() ([]byte, error) {
	type plain Frontmatter
	return jsonx.Marshal(plain(f))
}

// Skill is a skill in the library - parsed, validated and ready to show to an
// agent. It carries every Frontmatter field.
//
// The frontmatter fields are declared flat, in the runtime key order of the real
// hub, and Frontmatter is not embedded: a MarshalJSON method on an embedded
// struct would be promoted to Skill and replace its encoding. Frontmatter()
// returns the frontmatter part.
type Skill struct {
	Name          string              `json:"name"`
	Description   string              `json:"description"`
	License       string              `json:"license,omitempty"`
	Compatibility string              `json:"compatibility,omitempty"`
	AllowedTools  []string            `json:"allowedTools,omitzero"`
	Metadata      ordered.Map[string] `json:"metadata"`
	// Body is the SKILL.md instructions without the frontmatter.
	Body string `json:"body"`
	// Content is the SKILL.md text as written - the source of truth.
	Content string `json:"content"`
	Files   []File `json:"files"`
	// RequiresTools are the tools the skill needs (metadata `requires-tools`, space
	// or comma separated). An agent that lacks any of them does not see the skill
	// in its index.
	RequiresTools []string `json:"requiresTools"`
	Source        Source   `json:"source"`
	// Version increments on every update of a custom skill; 1 for built-in and app skills.
	Version int `json:"version"`
	// CreatedAt and UpdatedAt are ISO timestamps. The real hub writes createdAt,
	// updatedAt and then createdBy for a custom skill, so they are declared in that order.
	CreatedAt string `json:"createdAt,omitempty"`
	UpdatedAt string `json:"updatedAt,omitempty"`
	// CreatedBy is the agent that created it (custom skills made through skill_manage).
	CreatedBy string `json:"createdBy,omitempty"`
}

// Frontmatter returns the SKILL.md frontmatter fields of the skill. Metadata and
// AllowedTools are copies: the result shares nothing with the skill, so a caller
// may change it without reaching a skill that a hub or a store handed out.
func (s Skill) Frontmatter() Frontmatter {
	return Frontmatter{
		Name:          s.Name,
		Description:   s.Description,
		License:       s.License,
		Compatibility: s.Compatibility,
		AllowedTools:  slices.Clone(s.AllowedTools),
		Metadata:      s.Metadata.Clone(),
	}
}

// MarshalJSON writes an empty Metadata as {} (the zero ordered.Map encodes that
// way by itself, so no nil normalisation is needed) and a nil Files or
// RequiresTools as [], because the fields are required.
func (s Skill) MarshalJSON() ([]byte, error) {
	type plain Skill
	if s.Files == nil {
		s.Files = []File{}
	}
	if s.RequiresTools == nil {
		s.RequiresTools = []string{}
	}
	return jsonx.Marshal(plain(s))
}

// Parsed is a parsed, valid skill package.
type Parsed struct {
	Frontmatter   Frontmatter `json:"frontmatter"`
	Body          string      `json:"body"`
	Files         []File      `json:"files"`
	RequiresTools []string    `json:"requiresTools"`
}
