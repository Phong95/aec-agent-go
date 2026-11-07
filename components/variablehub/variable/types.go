package variable

import (
	"context"
	"slices"

	"github.com/Phong95/aec-agent-go/internal/jsonx"
)

// ---------------------------------------------------------------------------
// Variables (A-215) - settings that are not secret. Scopes like secrets: global (owner "")
// and an agent's own; an agent uses the global ones it enables in AgentProfile.Variables,
// and its own wins over a global one of the same name.
// ---------------------------------------------------------------------------

// Creator is who created the variable: a person (the host's UI / API) or an agent (the
// variable tool).
type Creator string

// The creators of a variable.
const (
	CreatorUser  Creator = "user"
	CreatorAgent Creator = "agent"
)

// Definition is what a variable is created from.
type Definition struct {
	// Name is upper-case letters, digits and underscores, starting with a letter (max 64)
	// - the name the model writes in {{var:NAME}} and the environment variable a sandbox
	// command sees.
	Name  string `json:"name"`
	Value string `json:"value"`
	// Description is what it is for - shown to the model in <available_variables>.
	Description string `json:"description,omitempty"`
}

// Info is a variable as stores, tools and APIs show it (value included: it is not
// secret).
type Info struct {
	Name  string `json:"name"`
	Value string `json:"value"`
	// OwnerAgentID is the agent that owns it; "" = a global variable, usable by the agents
	// that enable it.
	OwnerAgentID string  `json:"ownerAgentId,omitempty"`
	Description  string  `json:"description,omitempty"`
	CreatedBy    Creator `json:"createdBy"`
	CreatedAt    string  `json:"createdAt"`
	UpdatedAt    string  `json:"updatedAt"`
}

// The limits of a variable (VARIABLE_LIMITS of core/ts).
const (
	// MaxNameChars is the longest name.
	MaxNameChars = 64
	// MaxDescriptionChars is the longest description, in UTF-16 code units.
	MaxDescriptionChars = 500
	// MaxValueBytes is the largest value, in UTF-8 bytes.
	MaxValueBytes = 4_096
	// MaxPerOwner is the most variables per owner (an agent, or the global library).
	MaxPerOwner = 200
)

// Config is AgentProfile.Variables - the global variables this agent may use (it can
// always use its own).
type Config struct {
	// Use names the global variables (the VariableHub's library) this agent may use.
	Use []string `json:"use"`
}

// MarshalJSON encodes the config with Use as an array (never null), the shape core/ts
// writes.
func (c Config) MarshalJSON() ([]byte, error) {
	type alias Config // drops the methods, so this does not recurse
	a := alias(c)
	if a.Use == nil {
		a.Use = []string{}
	}
	return jsonx.Marshal(a)
}

// Clone returns a deep copy (nil stays nil).
func (c *Config) Clone() *Config {
	if c == nil {
		return nil
	}
	out := *c
	out.Use = slices.Clone(c.Use)
	return &out
}

// Scope says whose a variable an agent sees is: its own, or a global one it enabled.
type Scope string

// The scopes of an AgentInfo.
const (
	ScopeAgent  Scope = "agent"
	ScopeGlobal Scope = "global"
)

// AgentInfo is a variable as the agent sees it: its own, or a global one it enabled.
type AgentInfo struct {
	Info
	Scope Scope `json:"scope"`
}

// Patch changes a variable: a nil field keeps what it has. A Description pointing to ""
// removes the description.
type Patch struct {
	Value       *string `json:"value,omitempty"`
	Description *string `json:"description,omitempty"`
}

// Variables is the agent's variables as its tools reach them (toolhub.Context.Variables).
// It is safe for concurrent use.
type Variables interface {
	// List returns the variables this agent can use: its own, then the global ones it
	// enabled (an own one hides a global of the same name).
	List(ctx context.Context) ([]AgentInfo, error)
	// Resolve returns the values of names; the agent's own wins over a global one. It
	// returns an *Error ("not_found").
	Resolve(ctx context.Context, names []string) (map[string]string, error)
	// Create creates one of the agent's own variables (the variable tool).
	Create(ctx context.Context, def Definition) (AgentInfo, error)
	// Update changes one of the agent's own variables.
	Update(ctx context.Context, name string, patch Patch) (AgentInfo, error)
	// Delete deletes one of the agent's own variables.
	Delete(ctx context.Context, name string) error
}
