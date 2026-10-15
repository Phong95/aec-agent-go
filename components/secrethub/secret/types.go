package secret

import (
	"context"

	"github.com/Phong95/aec-agent-go/internal/jsonx"
)

// ---------------------------------------------------------------------------
// Secrets (A-101) - sensitive keys (API keys, tokens) an agent can use but never see.
// The model works with secret NAMES; a tool that supports secrets puts the value in at
// the moment it runs (http_request headers/body, terminal/run_code environment). Values
// are sealed with envelope encryption before they reach a store: each value gets its own
// data key, and the data key is wrapped by a master key the host's SecretHubProvider manages
// (a local key, or an external KMS).
// ---------------------------------------------------------------------------

// Creator is who created the secret: a person (through the host's UI/API) or an agent
// (manage_secret).
type Creator string

// The creators of a secret.
const (
	CreatorUser  Creator = "user"
	CreatorAgent Creator = "agent"
)

// Definition is what a secret is created or updated from - never the value itself.
//
// For an update, a nil field keeps what the secret has: a nil Description keeps its
// description (a pointer to "" clears it), nil AllowedHosts / AllowedTools keep its lists
// (an empty, non-nil slice clears them). For a new secret a nil list means the default.
type Definition struct {
	// Name is upper-case letters, digits and underscores, starting with a letter (max 64)
	// - the name the model writes in {{secret:NAME}} and the environment variable a
	// sandbox command sees. Ignored by an update.
	Name string `json:"name"`
	// Description is what the key is for - shown to the model in <available_secrets>.
	Description *string `json:"description,omitempty"`
	// AllowedHosts are the hosts http_request may send it to: "api.example.com", or
	// "*.example.com" for its subdomains. Empty = no host (the secret can't be sent
	// anywhere until hosts are set).
	AllowedHosts []string `json:"allowedHosts,omitempty"`
	// AllowedTools are the tools that may use it ("http_request", "terminal", "run_code").
	// Default: ["http_request"]. A sandbox tool gets the value as an environment variable,
	// so a host can't be enforced there.
	AllowedTools []string `json:"allowedTools,omitempty"`
}

// Meta is the part of a secret's metadata a Definition sets, checked and normalised
// (NormalizeDefinition).
type Meta struct {
	Description  string   `json:"description,omitempty"`
	AllowedHosts []string `json:"allowedHosts"`
	AllowedTools []string `json:"allowedTools"`
}

// Info is a secret's metadata - what lists, tools and APIs show. It never carries the
// value.
type Info struct {
	Name string `json:"name"`
	// OwnerAgentID is the agent that owns it; "" = a global secret, usable by the agents
	// that enable it.
	OwnerAgentID string   `json:"ownerAgentId,omitempty"`
	Description  string   `json:"description,omitempty"`
	AllowedHosts []string `json:"allowedHosts"`
	AllowedTools []string `json:"allowedTools"`
	// KeyID is the master key that wraps its data key ("" = stored without encryption: no
	// SecretHub provider).
	KeyID      string  `json:"keyId"`
	CreatedBy  Creator `json:"createdBy"`
	CreatedAt  string  `json:"createdAt"`
	UpdatedAt  string  `json:"updatedAt"`
	LastUsedAt string  `json:"lastUsedAt,omitempty"`
}

// Meta returns the definition-set part of the metadata (lists copied).
func (i Info) Meta() Meta {
	c := i.Clone()
	return Meta{Description: c.Description, AllowedHosts: c.AllowedHosts, AllowedTools: c.AllowedTools}
}

// Stored is a secret as a store keeps it: the metadata plus the sealed value.
type Stored struct {
	Info
	// Value is the value as the sealer wrote it ("enc:v2:..."; plain text only when no
	// SecretHub provider is set).
	Value string `json:"value"`
}

// Use is how a tool wants to use a secret - checked against AllowedTools / AllowedHosts.
type Use struct {
	Tool string `json:"tool"`
	// Host is the host the value is sent to (http_request); "" for sandbox tools, whose
	// host can't be enforced.
	Host string `json:"host,omitempty"`
}

// Status counts the stored values by how they are sealed (A-101 status / rotation).
type Status struct {
	// Provider is the SecretHub provider's id ("none" when values are stored without
	// encryption).
	Provider string `json:"provider"`
	// CurrentKeyID is the master key new values are sealed with ("" without a provider).
	CurrentKeyID string `json:"currentKeyId"`
	// ByKeyID counts the secrets per master key id (envelope-encrypted, enc:v2).
	ByKeyID map[string]int `json:"byKeyId"`
	// Legacy counts the secrets still in the A-100 single-key format (enc:v1).
	Legacy int `json:"legacy"`
	// Plain counts the secrets stored as plain text.
	Plain int `json:"plain"`
}

// The limits of a secret (SECRET_LIMITS of core/ts).
const (
	// MaxNameChars is the longest name.
	MaxNameChars = 64
	// MaxDescriptionChars is the longest description, in UTF-16 code units.
	MaxDescriptionChars = 500
	// MaxValueBytes is the largest value, in UTF-8 bytes.
	MaxValueBytes = 16_384
	// MaxHosts is the most allowed hosts.
	MaxHosts = 32
	// MaxTools is the most allowed tools.
	MaxTools = 16
	// MaxPerOwner is the most secrets per owner (an agent, or the global library).
	MaxPerOwner = 200
)

// Tools returns the tools that use secrets in the SDK (SECRET_TOOLS of core/ts), as a
// fresh slice; AllowedTools may name others (an app's own tools).
func Tools() []string { return []string{"http_request", "terminal", "run_code"} }

// DefaultTools returns the default AllowedTools of a new secret, as a fresh slice.
func DefaultTools() []string { return []string{"http_request"} }

// ---------------------------------------------------------------------------
// Secrets of one agent (A-101) - what its tools and the host use. Owned by the agent's
// secret manager; the values themselves live sealed in the SecretHub's store.
// ---------------------------------------------------------------------------

// Config is AgentProfile.Secrets - the global secrets this agent may use (it can always
// use its own).
type Config struct {
	// Use names the global secrets (the SecretHub's library) this agent may use.
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

// Scope says whose a secret an agent sees is: its own, or a global one it enabled.
type Scope string

// The scopes of an AgentInfo.
const (
	ScopeAgent  Scope = "agent"
	ScopeGlobal Scope = "global"
)

// AgentInfo is a secret as the agent sees it: its own, or a global one it enabled.
type AgentInfo struct {
	Info
	Scope Scope `json:"scope"`
}

// ResolveContext is where a Resolve happens: the turn's session, for the secret:used
// event.
type ResolveContext struct {
	SessionID string
}

// Secrets is the agent's secrets as its tools reach them (toolhub.Context.Secrets). Values
// only ever go from Resolve into a tool's request or command - never into a tool result.
// It is safe for concurrent use.
type Secrets interface {
	// List returns the secrets this agent can use: its own, then the global ones it
	// enabled. Metadata only.
	List(ctx context.Context) ([]AgentInfo, error)
	// Resolve returns the values of names for one use (a tool, and the host it sends them
	// to). The agent's own secret wins over a global one of the same name. It returns an
	// *Error ("not_found", "not_allowed", "decrypt_failed", "unavailable"). Each value is
	// remembered for redaction.
	Resolve(ctx context.Context, names []string, use Use, rc ResolveContext) (map[string]string, error)
	// Redact returns text with every secret value used in this process replaced by
	// [secret:NAME].
	Redact(text string) string
	// Create creates one of the agent's own secrets (manage_secret, after the user entered
	// the value).
	Create(ctx context.Context, def Definition, value string) (AgentInfo, error)
	// Update changes one of the agent's own secrets' description, hosts or tools (the
	// Name of patch is ignored).
	Update(ctx context.Context, name string, patch Definition) (AgentInfo, error)
	// SetValue gives one of the agent's own secrets a new value.
	SetValue(ctx context.Context, name string, value string) (AgentInfo, error)
	// Delete deletes one of the agent's own secrets.
	Delete(ctx context.Context, name string) error
}
