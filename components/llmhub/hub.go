package llmhub

import (
	"errors"
	"fmt"
	"reflect"
	"sync"

	"github.com/Phong95/aec-agent-go/kernel/ordered"
)

// The sentinel errors of the LLMHub and the Registry. The errors they return
// satisfy errors.Is against these, while their Error() text is the exact text
// the golden fixtures record.
var (
	ErrNoProvider            = errors.New("llm: no provider registered")
	ErrPrimaryNotFound       = errors.New("llm: primary provider not found")
	ErrProviderNotRegistered = errors.New("llm: provider not registered")
	ErrNilProvider           = errors.New("llm: nil provider")
	ErrUnknownProvider       = errors.New("llm: unknown provider")
)

// hubError carries an error text unchanged and unwraps to a sentinel. It is a
// type of its own, not fmt.Errorf or errors.New: the texts are capitalised
// and exact, and the golden fixtures compare them.
type hubError struct {
	msg      string
	sentinel error
}

func (e *hubError) Error() string { return e.msg }

func (e *hubError) Unwrap() error { return e.sentinel }

// LLMHub is the hub of LLM providers of one agent core. During a session a user can
// switch between models, for example when a session's limit is hit.
//
// A LLMHub is an instance: one per AgentCore, never a package-level singleton. The
// package-level provider registry (RegisterProvider, ResolveProvider) is separate.
//
// A LLMHub is safe for concurrent use. The zero value is ready to use.
type LLMHub struct {
	mu        sync.RWMutex
	providers ordered.Map[Provider]
	primary   string
}

// NewLLMHub returns an empty LLMHub.
func NewLLMHub() *LLMHub {
	return &LLMHub{}
}

// Register adds a provider under its Name. Registering a name again replaces the
// provider and keeps the name's position in List and its primary role. The first
// provider registered becomes the primary, and stays so until SetPrimary. A nil
// provider (a nil interface or a typed nil pointer) returns an error matching
// ErrNilProvider; a provider whose Name method
// panics returns an error instead of panicking.
func (h *LLMHub) Register(p Provider) error {
	if isNilProvider(p) {
		return ErrNilProvider
	}
	name, err := providerName(p)
	if err != nil {
		return err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.providers.Set(name, p)
	// An empty primary name is "no primary yet".
	if h.primary == "" {
		h.primary = name
	}
	return nil
}

// isNilProvider reports a nil interface and also a typed nil pointer (and the
// other nil-able kinds) wrapped in a non-nil interface.
func isNilProvider(p Provider) bool {
	if p == nil {
		return true
	}
	switch v := reflect.ValueOf(p); v.Kind() {
	case reflect.Pointer, reflect.Map, reflect.Slice, reflect.Func, reflect.Chan, reflect.Interface:
		return v.IsNil()
	default:
		return false
	}
}

// providerName calls p.Name and turns a panic into an error.
func providerName(p Provider) (name string, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("llm: reading the name of a provider failed: %v", r)
		}
	}()
	return p.Name(), nil
}

// Get returns the provider registered under name.
func (h *LLMHub) Get(name string) (Provider, bool) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.providers.Get(name)
}

// Primary returns the primary provider. With no provider registered the error
// text is `No LLMModelProvider registered` and matches ErrNoProvider. A primary
// name that is no longer in the map gives `Primary LLMModelProvider not found`
// and matches ErrPrimaryNotFound; that is defensive, because nothing
// is ever removed.
func (h *LLMHub) Primary() (Provider, error) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	if h.primary == "" {
		return nil, &hubError{msg: "No LLMModelProvider registered", sentinel: ErrNoProvider}
	}
	p, ok := h.providers.Get(h.primary)
	if !ok {
		return nil, &hubError{msg: "Primary LLMModelProvider not found", sentinel: ErrPrimaryNotFound}
	}
	return p, nil
}

// SetPrimary makes the provider registered under name the primary. An unknown
// name returns the error `LLMModelProvider "<name>" not registered`, which
// matches ErrProviderNotRegistered.
func (h *LLMHub) SetPrimary(name string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.providers.Has(name) {
		return &hubError{msg: `LLMModelProvider "` + name + `" not registered`, sentinel: ErrProviderNotRegistered}
	}
	h.primary = name
	return nil
}

// List returns every registered provider in registration order, as a fresh
// slice.
func (h *LLMHub) List() []Provider {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.providers.Values()
}
