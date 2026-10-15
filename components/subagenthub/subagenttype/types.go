// Package subagenttype holds the contract of subagent types (A-105, design revision) -
// the Claude Code style of subagent: a definition in a global library
// (subagenthub.SubagentHub), enabled per agent (AgentProfile.Subagents.Types).
// Delegating to one runs a short-lived loop inside the parent agent with the type's
// system prompt, a filtered set of the parent's tools and the parent's sandbox - no
// memory, sessions list, channels or jobs of its own. (Delegating to an existing agent,
// AgentProfile.Subagents.Allow, is the other kind.)
//
// It is the contract the hub shares with its store (persistence.SubagentTypeStore), so
// it lives in its own leaf package, like skillhub/skill. The JSON shapes are the wire
// shapes of core/ts (SubagentTypeDefinition, SubagentType).
package subagenttype

import (
	"slices"

	"github.com/Phong95/aec-agent-go/components/agent/subagent"
)

// Definition is what a type is created from (built-in, an app's registration, or a
// user's definition).
type Definition struct {
	// Name is lowercase letters, digits and dashes, 1-64 characters - what
	// spawn_subagent names.
	Name string `json:"name"`
	// Description is when to use it - shown to the parent's model in
	// <available_subagents>.
	Description string `json:"description"`
	// SystemPrompt is the child's system prompt.
	SystemPrompt string `json:"systemPrompt"`
	// Tools are the parent's tools it may use (intersected with the parent's allowlist,
	// then scoped by Access). Nil = all of the parent's tools; an empty, non-nil list =
	// none.
	Tools []string `json:"tools,omitzero"`
	// Access defaults to read-only: only read-only tools. "write": its writing tools too,
	// one writer per parent sandbox.
	Access subagent.Access `json:"access,omitempty"`
	// Provider is the model provider for its turns; "" = the parent's.
	Provider string `json:"provider,omitempty"`
	// Model is the model id for its turns; "" = the parent's.
	Model string `json:"model,omitempty"`
	// MaxIterations is its turn limit; nil = the parent's maxIterations.
	MaxIterations *int `json:"maxIterations,omitempty"`
}

// Source is where a type comes from. The string values are stable wire values.
type Source string

// The sources.
const (
	// SourceBuiltIn: a type every server has (subagenthub.BuiltInTypes).
	SourceBuiltIn Source = "built-in"
	// SourceApp: a type an app registered (AgentCore.UseSubagentType).
	SourceApp Source = "app"
	// SourceCustom: a type a user created, kept in the persistence.SubagentTypeStore.
	SourceCustom Source = "custom"
)

// Type is a type in the library: a checked Definition (its Access is always set) and
// where it comes from.
type Type struct {
	Definition
	Source Source `json:"source"`
	// CreatedAt and UpdatedAt are set on custom types only.
	CreatedAt string `json:"createdAt,omitempty"`
	UpdatedAt string `json:"updatedAt,omitempty"`
}

// Clone returns a deep copy: Tools and MaxIterations are copied. Add a line here when a
// pointer, slice or map field is added to Type or Definition.
func (t Type) Clone() Type {
	t.Definition = t.Definition.Clone()
	return t
}

// Clone returns a deep copy: Tools and MaxIterations are copied.
func (d Definition) Clone() Definition {
	if d.Tools != nil {
		d.Tools = slices.Clone(d.Tools)
	}
	if d.MaxIterations != nil {
		v := *d.MaxIterations
		d.MaxIterations = &v
	}
	return d
}

// The limits of a definition (SUBAGENT_TYPE_LIMITS of core/ts), in UTF-16 code units
// like a JS string length.
const (
	// MaxNameChars is the most characters of a name.
	MaxNameChars = 64
	// MaxDescriptionChars is the most characters of a description.
	MaxDescriptionChars = 500
	// MaxSystemPromptChars is the most characters of a system prompt.
	MaxSystemPromptChars = 20_000
	// MaxTools is the most tools a type may list.
	MaxTools = 64
	// MaxIterations is the most a type's turn limit may be (a larger one is clamped).
	MaxIterations = 100
)
