package aec

import (
	"github.com/Phong95/aec-agent-go/components/agent/profile"
	"github.com/Phong95/aec-agent-go/components/llmhub"
	"github.com/Phong95/aec-agent-go/components/persistence"
	"github.com/Phong95/aec-agent-go/components/skillhub/skill"
	"github.com/Phong95/aec-agent-go/components/toolhub"
)

// AgentConfig is the configuration of CreateAgent: one struct from which a working agent
// is built.
//
// Every field is optional. A zero value means "use the default": an empty string, a nil
// slice and a nil pointer are absent.
//
// AgentConfig holds credentials, so every text rendering of it redacts them, like
// llmhub.ProviderConfig does: it implements slog.LogValuer, fmt.Formatter (which serves every
// verb, so %v, %+v, %#v and %s alike), fmt.Stringer, fmt.GoStringer and json.Marshaler
// (the JSON form is the redacted view too, it is never decoded). APIKey and
// SecretKey are replaced with [REDACTED], BaseURL shows only its scheme and host, and the
// tools, skills, persistence hub and system prompt are summarised and never printed. The
// same limits as for ProviderConfig apply: slog resolves a LogValuer only for a value
// passed directly as an attribute, and fmt cannot call Format on an unexported struct
// field, so hold the configuration in an exported field or log it directly.
type AgentConfig struct {
	// Provider is the registered provider name, for example "anthropic". When it is set
	// the provider is always resolved and registered, whatever APIKey and Model hold, and
	// a factory that refuses the config fails CreateAgent. The name also becomes the
	// agent name "<provider>-agent" and the provider of its profile. Empty means no provider is
	// configured and the profile names none (the core's primary provider is used).
	Provider string
	// APIKey is the provider credential.
	APIKey string
	// Model is the model id.
	Model string
	// BaseURL overrides the provider endpoint.
	BaseURL string
	// SecretKey is the AWS secret access key of a Bedrock agent, with APIKey as the access
	// key id, so a Bedrock agent can be created in one call. It is passed to
	// llmhub.ProviderConfig.SecretKey.
	SecretKey string
	// Region is the AWS region of a Bedrock agent. Like SecretKey, it is passed to
	// llmhub.ProviderConfig.Region.
	Region string
	// Tools are the tools to register on the core and to give the agent. A duplicate
	// name is an error. Only these tools are registered: CreateAgent never
	// registers a built-in tool on its own.
	Tools []toolhub.Handler
	// Skills are app skill packages to register (registered with the "app" source). The agent gets the essential skills first, then these, with no duplicate.
	Skills []skill.Package
	// Persistence replaces the persistence hub of the core when set.
	Persistence *persistence.PersistenceHub
	// SystemPrompt is the system prompt of the agent. Empty means the
	// default "You are a helpful assistant.", because a Go empty string is absent.
	SystemPrompt string
	// LLM is a provider built by the caller. When it is set it is registered
	// on the core as it is and nothing is resolved from a registry, so APIKey, BaseURL,
	// SecretKey and Region are not used (the provider already holds its credentials);
	// Model then only names the model of the agent (the profile's LLMModel), and the
	// provider's Name becomes the provider of the profile and the agent name
	// "<name>-agent". A Provider that is set must equal LLM.Name(), or CreateAgent
	// fails. Precedence: LLM, then Provider, then the core's primary provider.
	LLM llmhub.Provider
	// Profile is applied to the agent profile last, after every other field of the
	// config was applied and before the agent is spawned. It lets a caller
	// set what AgentConfig has no field for (the sandbox, the loop or inference config,
	// the memory config) without dropping to AgentCore.SpawnAgent. Whatever it leaves
	// in the profile is what is spawned and saved, so clearing the id or a tool name it
	// does not register makes the spawn fail.
	Profile func(*profile.AgentProfile)
}
