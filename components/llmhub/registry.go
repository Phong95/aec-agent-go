package llmhub

import (
	"fmt"
	"slices"
	"strings"
	"sync"

	"github.com/Phong95/aec-agent-go/kernel/ordered"
)

// builtinOrder is the canonical order of the built-in providers, and so the
// order ListProviders reports them in.
//
// It is an explicit table because Go initialises packages sorted by import path:
// the Phase 8 provider packages register themselves from init(), and without this
// table they would register (and list) in alphabetical order, not this order.
var builtinOrder = []string{"anthropic", "novita", "openai", "bedrock", "openai-subscription"}

// Registry maps provider names to the factories that build them. A Registry is
// safe for concurrent use; the zero value is ready to use.
type Registry struct {
	mu        sync.RWMutex
	factories ordered.Map[Factory]
}

// NewRegistry returns an empty Registry. Tests use it to stay clear of the
// package-level default registry.
func NewRegistry() *Registry {
	return &Registry{}
}

// Register adds a factory under name. Registering a name again replaces its
// factory and keeps its position; the last registration wins.
// In-process code is trusted, so a package can replace a built-in on purpose.
//
// It has no return value so that a provider package can call it from init().
func (r *Registry) Register(name string, f Factory) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.factories.Set(name, f)
}

// Resolve builds the provider registered under name from cfg and returns what its
// factory returns, unchanged. An unknown name returns the error
//
//	Unknown provider "<name>". Built-in: <List joined by ", ">. Use registerProvider() to add custom providers.
//
// which matches ErrUnknownProvider. A name registered with a nil factory returns
// an error instead of panicking. The factory runs outside the registry lock, so
// it may itself use the registry.
func (r *Registry) Resolve(name string, cfg ProviderConfig) (Provider, error) {
	r.mu.RLock()
	factory, ok := r.factories.Get(name)
	var names []string
	if !ok {
		names = r.listLocked()
	}
	r.mu.RUnlock()
	if !ok {
		return nil, &hubError{
			msg: `Unknown provider "` + name + `". Built-in: ` + strings.Join(names, ", ") +
				`. Use registerProvider() to add custom providers.`,
			sentinel: ErrUnknownProvider,
		}
	}
	if factory == nil {
		return nil, fmt.Errorf("llm: provider %q has a nil factory", name)
	}
	return factory(cfg)
}

// List returns the registered provider names: the built-ins that are registered
// in canonical order (anthropic, novita, openai, bedrock, openai-subscription), then
// every other name in registration order. Only built-ins whose packages are
// imported are registered; llm/all imports all four.
func (r *Registry) List() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.listLocked()
}

// listLocked is List for a caller that holds the lock.
func (r *Registry) listLocked() []string {
	out := make([]string, 0, r.factories.Len())
	for _, name := range builtinOrder {
		if r.factories.Has(name) {
			out = append(out, name)
		}
	}
	for name := range r.factories.All() {
		if !slices.Contains(builtinOrder, name) {
			out = append(out, name)
		}
	}
	return out
}

// defaultRegistry backs the package-level functions. Provider packages register
// into it from init().
var defaultRegistry = NewRegistry()

// RegisterProvider registers a factory under name in the default registry. A
// provider package calls it from init(); an application may call it to add a
// custom provider. Re-registering a name replaces its factory.
func RegisterProvider(name string, f Factory) {
	defaultRegistry.Register(name, f)
}

// ResolveProvider builds the provider registered under name in the default
// registry. See Registry.Resolve.
func ResolveProvider(name string, cfg ProviderConfig) (Provider, error) {
	return defaultRegistry.Resolve(name, cfg)
}

// ListProviders returns the names registered in the default registry, built-ins
// first in canonical order. See Registry.List.
func ListProviders() []string {
	return defaultRegistry.List()
}
