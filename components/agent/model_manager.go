package agent

import (
	"context"
	"iter"
	"slices"
	"sync"

	"github.com/Phong95/aec-agent-go/components/agent/agentloop"
	"github.com/Phong95/aec-agent-go/components/agent/profile"
	"github.com/Phong95/aec-agent-go/components/llmhub"
	"github.com/Phong95/aec-agent-go/components/toolhub"
	"github.com/Phong95/aec-agent-go/kernel"
)

// ModelManager is the agent's model access over the LLM provider hub.
// It resolves the provider on every call, so a provider registered
// after the agent was spawned is found, and it layers the agent's defaults under
// the caller's options (provider, model and inference config).
//
// It satisfies agentloop.Models, agentloop.OptionResolver and
// agentloop.ProviderNamer, and it is the model port of the memory system. The
// defaults sit behind a mutex, so the Update* setters of the Agent may run while a
// chat is in flight; a call reads them once.
type ModelManager struct {
	hub *llmhub.LLMHub

	mu        sync.RWMutex
	provider  string
	model     string
	inference *profile.InferenceConfig // never mutated after it is stored; replaced as a whole
}

var (
	_ agentloop.Models         = (*ModelManager)(nil)
	_ agentloop.OptionResolver = (*ModelManager)(nil)
	_ agentloop.ProviderNamer  = (*ModelManager)(nil)
)

// NewModelManager returns a ModelManager over hub whose defaults are the provider,
// the model and the inference config of p (the config is copied).
func NewModelManager(hub *llmhub.LLMHub, p profile.AgentProfile) *ModelManager {
	return &ModelManager{
		hub:       hub,
		provider:  p.Provider,
		model:     p.LLMModel,
		inference: cloneInferenceConfig(p.InferenceConfig),
	}
}

// Chat sends one request to the resolved provider with the defaults merged under
// opts. A missing provider is the hub's error.
func (m *ModelManager) Chat(ctx context.Context, msgs []kernel.Message, opts llmhub.ModelOptions) (kernel.ModelResponse, error) {
	p, err := m.resolve(opts.Provider)
	if err != nil {
		return kernel.ModelResponse{}, err
	}
	return p.Chat(ctx, msgs, m.withDefaults(opts))
}

// Stream streams one response from the resolved provider with the defaults merged
// under opts. It keeps the llmhub.Provider contract: when no provider can be resolved
// the sequence is one terminal chunk of type error carrying the hub's message,
// yielded with a nil error.
func (m *ModelManager) Stream(ctx context.Context, msgs []kernel.Message, opts llmhub.ModelOptions) iter.Seq2[kernel.StreamChunk, error] {
	p, err := m.resolve(opts.Provider)
	if err != nil {
		return func(yield func(kernel.StreamChunk, error) bool) {
			yield(kernel.StreamChunk{Type: kernel.ChunkError, Error: err.Error()}, nil)
		}
	}
	return p.Stream(ctx, msgs, m.withDefaults(opts))
}

// CountTokens counts the tokens of msgs with the agent's default provider.
func (m *ModelManager) CountTokens(ctx context.Context, msgs []kernel.Message) (int, error) {
	p, err := m.resolve("")
	if err != nil {
		return 0, err
	}
	return p.CountTokens(ctx, msgs)
}

// ListProviders returns the names of the registered providers in registration
// order.
func (m *ModelManager) ListProviders() []string {
	providers := m.hub.List()
	names := make([]string, 0, len(providers))
	for _, p := range providers {
		names = append(names, p.Name())
	}
	return names
}

// ListModels returns the model ids the agent's provider offers (A-100: the chat's /model
// command). ok is false when the provider has no catalog, none is registered, or the
// catalog could not be read.
func (m *ModelManager) ListModels(ctx context.Context) (models []string, ok bool) {
	p, err := m.resolve("")
	if err != nil {
		return nil, false
	}
	lister, isLister := p.(llmhub.ModelLister)
	if !isLister {
		return nil, false
	}
	infos, err := lister.ListModels(ctx)
	if err != nil {
		return nil, false
	}
	ids := make([]string, 0, len(infos))
	for _, info := range infos {
		ids = append(ids, info.ID)
	}
	return ids, true
}

// SetDefaultProvider changes the provider used when a call names none ("" means
// the hub's primary).
func (m *ModelManager) SetDefaultProvider(provider string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.provider = provider
}

// SetDefaultModel changes the model used when a call names none.
func (m *ModelManager) SetDefaultModel(model string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.model = model
}

// SetDefaultInferenceConfig changes the inference defaults; c is copied and nil
// clears them.
func (m *ModelManager) SetDefaultInferenceConfig(c *profile.InferenceConfig) {
	c = cloneInferenceConfig(c)
	m.mu.Lock()
	defer m.mu.Unlock()
	m.inference = c
}

// ResolveOptions returns the effective options a Chat or Stream call would send:
// the same merge as the private withDefaults, exposed so tracing can describe the
// exact options the provider receives.
func (m *ModelManager) ResolveOptions(opts llmhub.ModelOptions) llmhub.ModelOptions {
	return m.withDefaults(opts)
}

// ResolveProviderName returns the name of the provider a call naming provider would
// use. An error (no provider registered) means there is no provider name.
func (m *ModelManager) ResolveProviderName(provider string) (string, error) {
	p, err := m.resolve(provider)
	if err != nil {
		return "", err
	}
	return p.Name(), nil
}

// resolve returns the named provider, else the agent's default provider, when it is
// registered, else the hub's primary. An empty provider name means "use the default".
func (m *ModelManager) resolve(provider string) (llmhub.Provider, error) {
	name := provider
	if name == "" {
		m.mu.RLock()
		name = m.provider
		m.mu.RUnlock()
	}
	if name != "" {
		if p, ok := m.hub.Get(name); ok {
			return p, nil
		}
	}
	return m.hub.Primary()
}

// withDefaults merges the defaults into opts: the inference config is the base layer, the
// caller's options go on top (a field the caller sets wins), then the default model
// when the merged options carry none.
func (m *ModelManager) withDefaults(opts llmhub.ModelOptions) llmhub.ModelOptions {
	m.mu.RLock()
	inference, model := m.inference, m.model
	m.mu.RUnlock()
	merged := mergeInference(inference, opts)
	if model != "" && merged.Model == "" {
		merged.Model = model
	}
	return merged
}

// mergeInference returns a copy of opts in which every inference field opts leaves
// absent is filled from base. A pointer is absent when nil and a slice when empty,
// a string when empty, so an explicit zero in those forms also means "use the
// default". The result shares no pointer or slice with opts or base.
func mergeInference(base *profile.InferenceConfig, opts llmhub.ModelOptions) llmhub.ModelOptions {
	out := cloneModelOptions(opts)
	if base == nil {
		return out
	}
	if out.MaxTokens == nil && base.MaxTokens != nil {
		v := *base.MaxTokens
		out.MaxTokens = &v
	}
	if out.Temperature == nil && base.Temperature != nil {
		v := *base.Temperature
		out.Temperature = &v
	}
	if out.TopP == nil && base.TopP != nil {
		v := *base.TopP
		out.TopP = &v
	}
	if len(out.StopSequences) == 0 && len(base.StopSequences) > 0 {
		out.StopSequences = slices.Clone(base.StopSequences)
	}
	return out
}

// cloneModelOptions deep-copies the pointers and slices of o.
func cloneModelOptions(o llmhub.ModelOptions) llmhub.ModelOptions {
	if o.MaxTokens != nil {
		v := *o.MaxTokens
		o.MaxTokens = &v
	}
	if o.Temperature != nil {
		v := *o.Temperature
		o.Temperature = &v
	}
	if o.TopP != nil {
		v := *o.TopP
		o.TopP = &v
	}
	o.StopSequences = slices.Clone(o.StopSequences)
	if o.Tools != nil {
		tools := make([]toolhub.Definition, len(o.Tools))
		for i, d := range o.Tools {
			tools[i] = d.Clone()
		}
		o.Tools = tools
	}
	return o
}

// cloneInferenceConfig deep-copies an inference config through the profile clone,
// which the clonecheck test keeps complete (nil stays nil).
func cloneInferenceConfig(c *profile.InferenceConfig) *profile.InferenceConfig {
	return profile.AgentProfile{InferenceConfig: c}.Clone().InferenceConfig
}

// cloneLoopConfig deep-copies a loop config the same way.
func cloneLoopConfig(c *profile.LoopConfig) *profile.LoopConfig {
	return profile.AgentProfile{LoopConfig: c}.Clone().LoopConfig
}
