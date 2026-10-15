package aec

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"strconv"
	"strings"

	"github.com/Phong95/aec-agent-go/components/agent"
	"github.com/Phong95/aec-agent-go/components/agent/memorysystem/memory"
	"github.com/Phong95/aec-agent-go/components/agent/profile"
	"github.com/Phong95/aec-agent-go/components/llmhub"
	"github.com/Phong95/aec-agent-go/components/skillhub"
	"github.com/Phong95/aec-agent-go/components/skillhub/skill"
	"github.com/Phong95/aec-agent-go/internal/nilcheck"
	"github.com/Phong95/aec-agent-go/internal/redact"
	"github.com/Phong95/aec-agent-go/kernel"
)

// attrs lists the fields with the secrets replaced and the base URL cleaned.
func (c AgentConfig) attrs() []slog.Attr {
	attrs := make([]slog.Attr, 0, 10)
	plain := func(key, value string) {
		if value != "" {
			attrs = append(attrs, slog.String(key, value))
		}
	}
	secret := func(key, value string) {
		if value != "" {
			attrs = append(attrs, slog.String(key, redact.Marker))
		}
	}
	plain("provider", c.Provider)
	secret("apiKey", c.APIKey)
	plain("model", c.Model)
	if c.BaseURL != "" {
		plain("baseUrl", redact.URL(c.BaseURL))
	}
	secret("secretKey", c.SecretKey)
	plain("region", c.Region)
	if !nilcheck.IsNil(c.LLM) {
		attrs = append(attrs, slog.String("llm", c.LLM.Name()))
	}
	if len(c.Tools) > 0 {
		attrs = append(attrs, slog.Int("tools", len(c.Tools)))
	}
	if len(c.Skills) > 0 {
		attrs = append(attrs, slog.Int("skills", len(c.Skills)))
	}
	if c.Persistence != nil {
		attrs = append(attrs, slog.Bool("persistence", true))
	}
	if c.SystemPrompt != "" {
		attrs = append(attrs, slog.Bool("systemPrompt", true))
	}
	if c.Profile != nil {
		attrs = append(attrs, slog.Bool("profile", true))
	}
	return attrs
}

// LogValue implements slog.LogValuer. It shows the non-secret fields, replaces each
// non-empty secret (APIKey, SecretKey) with [REDACTED] and summarises the rest. It
// protects a configuration logged as an attribute value of its own.
func (c AgentConfig) LogValue() slog.Value {
	return slog.GroupValue(c.attrs()...)
}

// String renders the redacted fields as AgentConfig{key=value ...}.
func (c AgentConfig) String() string {
	var b strings.Builder
	b.WriteString("AgentConfig{")
	for i, a := range c.attrs() {
		if i > 0 {
			b.WriteByte(' ')
		}
		b.WriteString(a.Key)
		b.WriteByte('=')
		b.WriteString(strconv.Quote(a.Value.String()))
	}
	b.WriteByte('}')
	return b.String()
}

// GoString is the %#v rendering; it is the same redacted text as String.
func (c AgentConfig) GoString() string { return c.String() }

// MarshalJSON implements json.Marshaler. The JSON form is the same redacted view as the
// other renderings (the keys of LogValue, in the same order, with [REDACTED] in place of
// APIKey and SecretKey), because an AgentConfig has no reason to be serialised with its
// credentials: a host that returns it from a debug endpoint or logs it through a JSON
// logger gets no secret (review WR-B02). It is one way: an AgentConfig is built in code,
// never decoded from JSON.
func (c AgentConfig) MarshalJSON() ([]byte, error) {
	var b bytes.Buffer
	b.WriteByte('{')
	for i, a := range c.attrs() {
		if i > 0 {
			b.WriteByte(',')
		}
		key, err := kernel.EncodeJSON(a.Key)
		if err != nil {
			return nil, err
		}
		value, err := kernel.EncodeJSON(a.Value.Any())
		if err != nil {
			return nil, err
		}
		b.Write(key)
		b.WriteByte(':')
		b.Write(value)
	}
	b.WriteByte('}')
	return b.Bytes(), nil
}

// Format implements fmt.Formatter. It writes the redacted String for every verb, so no
// fmt verb can print a raw field.
func (c AgentConfig) Format(f fmt.State, _ rune) {
	_, _ = io.WriteString(f, c.String())
}

// createOptions are the settings CreateAgent reads; a nil field keeps the default.
type createOptions struct {
	core     *AgentCore
	registry *llmhub.Registry
}

// CreateAgentOption configures CreateAgent. A nil option, and an option holding a nil or
// typed-nil value, keeps the default.
type CreateAgentOption func(*createOptions)

// WithCore makes CreateAgent register its tools, skills, provider and persistence on the
// given core and spawn the agent there, instead of on a new one. Use it to set an interaction handler, a sandbox provider or any other hub
// before the agent is built.
func WithCore(c *AgentCore) CreateAgentOption {
	return func(o *createOptions) {
		if !nilcheck.IsNil(c) {
			o.core = c
		}
	}
}

// WithProviderRegistry makes CreateAgent resolve the provider from the given registry
// instead of the package-level default registry of llm. It lets a test or a host stay
// clear of the process-wide registry that the provider packages fill from
// their init functions.
func WithProviderRegistry(r *llmhub.Registry) CreateAgentOption {
	return func(o *createOptions) {
		if !nilcheck.IsNil(r) {
			o.registry = r
		}
	}
}

// defaultSystemPrompt is the system prompt of an agent whose config has none.
const defaultSystemPrompt = "You are a helpful assistant."

// CreateAgent builds a working agent from one config: an AgentCore with the provider,
// tools and skills of the config, and one spawned agent over it. It runs these steps:
//
//  1. a named provider is always resolved and registered, through llmhub.ResolveProvider or
//     the registry given with WithProviderRegistry, with the APIKey, Model, BaseURL,
//     SecretKey and Region of the config as they are (an empty APIKey or Model is passed
//     on: the factory decides, and some providers need no key). The factory error is
//     returned unchanged, for example Unknown provider "<name>" for an unknown name.
//     The profile never names a provider that was not configured. A provider given in
//     AgentConfig.LLM is registered as it is instead, and nothing is resolved;
//  2. every config tool is registered with UseTool, and a duplicate is an error;
//  3. every app skill is registered with the app source;
//  4. the agent is spawned with a new random id, the name "<provider>-agent" or "agent",
//     the system prompt of the config or "You are a helpful assistant.", the names of the
//     config tools only, the essential skills followed by the app skills, and the default
//     compactor and memory settings (consolidation every 30 minutes). The
//     AgentConfig.Profile hook, when set, is applied to that profile last.
//
// No built-in tool is registered automatically: the agent has exactly the tools of the
// config, so a developer passes the tools it wants. An agent without tools has an empty
// tool hub and an empty profile tool list.
//
// The save is synchronous, and it is the contract of AgentCore.SpawnAgent: when the agent was spawned but its profile could
// not be saved, CreateAgent returns the agent AND the error.
//
// The caller must dispose the agent it gets (Agent.Dispose): the consolidation interval
// of the memory defaults runs a scheduler goroutine for the agent. When CreateAgent built
// the core itself, Agent.Dispose is enough, because that core owns no goroutines and
// nothing else needs closing. Use WithCore to set an interaction handler, a sandbox
// provider or other hubs on the core, and then dispose the core as well.
//
// The credentials of cfg reach only the provider factory. They are never copied into the
// profile or into an error text.
func CreateAgent(ctx context.Context, cfg AgentConfig, opts ...CreateAgentOption) (*agent.Agent, error) {
	var o createOptions
	for _, opt := range opts {
		if opt != nil {
			opt(&o)
		}
	}
	core := o.core
	if core == nil {
		var err error
		if core, err = NewAgentCore(); err != nil {
			return nil, err
		}
	}

	if !nilcheck.IsNil(cfg.Persistence) {
		if err := core.UsePersistence(cfg.Persistence); err != nil {
			return nil, err
		}
	}

	// A named provider is resolved whenever one is named, so the profile never points at
	// a provider that was never registered; the factory decides whether the config is enough (an empty APIKey is
	// passed on, some providers need none) and its error is returned.
	providerName := cfg.Provider
	llmModel := ""
	if !nilcheck.IsNil(cfg.LLM) {
		// A provider built by the caller is used as it is: nothing is resolved,
		// and the credentials of the config are not used.
		name := cfg.LLM.Name()
		if cfg.Provider != "" && cfg.Provider != name {
			return nil, fmt.Errorf("aec: AgentConfig.Provider %q does not name AgentConfig.LLM %q", cfg.Provider, name)
		}
		if err := core.UseLLMProvider(cfg.LLM); err != nil {
			return nil, err
		}
		providerName = name
		llmModel = cfg.Model
	} else if cfg.Provider != "" {
		pc := llmhub.ProviderConfig{
			APIKey:    cfg.APIKey,
			Model:     cfg.Model,
			BaseURL:   cfg.BaseURL,
			SecretKey: cfg.SecretKey,
			Region:    cfg.Region,
		}
		var (
			p   llmhub.Provider
			err error
		)
		if o.registry != nil {
			p, err = o.registry.Resolve(cfg.Provider, pc)
		} else {
			p, err = llmhub.ResolveProvider(cfg.Provider, pc)
		}
		if err != nil {
			return nil, err
		}
		if err := core.UseLLMProvider(p); err != nil {
			return nil, err
		}
	}

	toolNames := make([]string, 0, len(cfg.Tools))
	for _, t := range cfg.Tools {
		if err := core.UseTool(t); err != nil {
			return nil, err
		}
		toolNames = append(toolNames, t.Definition().Name)
	}

	// The essential skills plus the app's; each is used only when the agent has the tools it
	// requires. A set removes the duplicates and keeps the first occurrence.
	skillNames := skillhub.EssentialSkillNames()
	for _, pkg := range cfg.Skills {
		sk, err := core.SkillHub().Register(pkg, skill.SourceApp)
		if err != nil {
			return nil, err
		}
		if !slices.Contains(skillNames, sk.Name) {
			skillNames = append(skillNames, sk.Name)
		}
	}

	name := "agent"
	if providerName != "" {
		name = providerName + "-agent"
	}
	systemPrompt := cfg.SystemPrompt
	if systemPrompt == "" {
		systemPrompt = defaultSystemPrompt
	}
	consolidationMinutes := 30.0
	p := profile.AgentProfile{
		ID:           kernel.NewID(),
		Name:         name,
		SystemPrompt: systemPrompt,
		Provider:     providerName,
		LLMModel:     llmModel,
		Tools:        toolNames,
		Skills:       skillNames,
		Compactor:    profile.DefaultCompactorConfig(),
		Memory: memory.Config{
			Enabled:       true,
			UpdateSoul:    false,
			UpdateUser:    true,
			Consolidation: &memory.ConsolidationTrigger{IntervalMinutes: &consolidationMinutes},
		},
	}
	// The caller's hook goes last, so it can change anything the steps above set.
	if cfg.Profile != nil {
		cfg.Profile(&p)
	}
	return core.SpawnAgent(ctx, p)
}
