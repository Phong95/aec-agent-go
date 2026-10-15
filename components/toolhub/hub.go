package toolhub

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"sync/atomic"

	"github.com/Phong95/aec-agent-go/kernel"
	"github.com/Phong95/aec-agent-go/kernel/ordered"
)

// The sentinel errors of the ToolHub. The errors the ToolHub returns satisfy
// errors.Is against these, while their Error() text is the fixed message text.
var (
	ErrAlreadyRegistered = errors.New("tool: already registered")
	ErrNotFound          = errors.New("tool: not found")
	ErrNotAuthorized     = errors.New("tool: call was not authorized")
	ErrNilHandler        = errors.New("tool: nil handler")

	// ErrAlreadyExecuted is returned by a second Authorized.Execute: an
	// authorization is single-use. It also matches ErrNotAuthorized.
	ErrAlreadyExecuted = fmt.Errorf("%w: the authorization was already used", ErrNotAuthorized)
	// ErrPolicyChanged is returned by Authorized.Execute when SetPolicy ran after
	// the call was authorized: the decision was made by a policy that no longer
	// applies. It also matches ErrNotAuthorized.
	ErrPolicyChanged = fmt.Errorf("%w: the policy changed after the call was authorized", ErrNotAuthorized)
)

// hubError carries a fixed error text unchanged and unwraps to a sentinel. It is a
// type of its own, not fmt.Errorf or errors.New: the texts are capitalised and
// exact, and the golden fixtures compare them.
type hubError struct {
	msg      string
	sentinel error
}

func (e *hubError) Error() string { return e.msg }

func (e *hubError) Unwrap() error { return e.sentinel }

// fallbackPolicy serves a ToolHub that was not built with NewToolHub, so dispatch never
// meets a nil policy. DefaultPolicy without a hook holds no state, so sharing
// one value is safe.
var fallbackPolicy Policy = &DefaultPolicy{}

// ToolHub is the registry of tools of an application, for example a hub holding 50
// tools of which a DevOps agent enables only the 10 DevOps tools.
//
// The hub is the choke point: Authorize followed by Authorized.Execute is the
// only way to run a registered handler. The handler never leaves the hub
// otherwise: Get, GetByNames and List return Definition values (lookup and
// introspection, for Kernel.Tool and the Phase 10 ToolManager), which carry no
// Execute method, so there is no way to reach a handler around the policy.
//
// A ToolHub is safe for concurrent use.
type ToolHub struct {
	mu     sync.RWMutex
	tools  *ordered.Map[entry]
	policy Policy
	// gen counts SetPolicy calls. An Authorized remembers the value it was minted
	// under and refuses to run once it has moved.
	gen uint64
	// interaction is the host channel to the human user - clarify questions and
	// approvals for consequential tools. Nil means no interactive user.
	interaction InteractionHandler
}

// entry is a registered tool: the handler, which stays private to the hub, and
// the definition read once at registration. The definition is a fixed property
// of the handler, so reading it once is enough and keeps a handler's Definition
// method out of the hub's locks and of the lookup paths.
type entry struct {
	handler Handler
	def     Definition
}

// NewToolHub returns an empty ToolHub whose policy is a DefaultPolicy without a hook.
func NewToolHub() *ToolHub {
	return &ToolHub{
		tools:  ordered.New[entry](),
		policy: NewDefaultPolicy(DefaultPolicyOptions{}),
	}
}

// Register adds a tool under its definition name. A nil handler (a nil
// interface or a typed nil pointer) returns an error matching ErrNilHandler; a
// handler whose Definition method panics returns an error; a name that is
// already registered returns an error whose text is `Tool "<name>" already
// registered` and that matches ErrAlreadyRegistered.
//
// The definition is read once, here, and stored as a deep copy: changing the
// handler's schema afterwards does not change what the hub checks or reports.
func (h *ToolHub) Register(t Handler) error {
	if isNilHandler(t) {
		return ErrNilHandler
	}
	def, err := readDefinition(t)
	if err != nil {
		return err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.tools == nil {
		h.tools = ordered.New[entry]()
	}
	if h.tools.Has(def.Name) {
		return &hubError{msg: `Tool "` + def.Name + `" already registered`, sentinel: ErrAlreadyRegistered}
	}
	h.tools.Set(def.Name, entry{handler: t, def: def})
	return nil
}

// isNilHandler reports a nil interface and also a typed nil pointer (and the
// other nil-able kinds) wrapped in a non-nil interface.
func isNilHandler(t Handler) bool {
	if t == nil {
		return true
	}
	switch v := reflect.ValueOf(t); v.Kind() {
	case reflect.Pointer, reflect.Map, reflect.Slice, reflect.Func, reflect.Chan, reflect.Interface:
		return v.IsNil()
	default:
		return false
	}
}

// readDefinition calls the handler's Definition method once, turns a panic into
// an error and returns a deep copy.
func readDefinition(t Handler) (def Definition, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("tool: reading the definition of a handler failed: %v", r)
		}
	}()
	return t.Definition().Clone(), nil
}

// Get returns the definition of the tool registered under name, as a deep copy.
// It is lookup, not an execution path: the handler stays inside the hub and a
// tool runs only through Authorize.
func (h *ToolHub) Get(name string) (Definition, bool) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	e, ok := h.tools.Get(name)
	if !ok {
		return Definition{}, false
	}
	return e.def.Clone(), true
}

// GetByNames returns the definitions of the registered tools among names, in the
// given order. Unknown names are skipped.
func (h *ToolHub) GetByNames(names []string) []Definition {
	h.mu.RLock()
	defer h.mu.RUnlock()
	out := make([]Definition, 0, len(names))
	for _, n := range names {
		if e, ok := h.tools.Get(n); ok {
			out = append(out, e.def.Clone())
		}
	}
	return out
}

// List returns the definition of every registered tool in registration order,
// as a fresh slice of deep copies.
func (h *ToolHub) List() []Definition {
	h.mu.RLock()
	defer h.mu.RUnlock()
	out := make([]Definition, 0, h.tools.Len())
	for _, e := range h.tools.Values() {
		out = append(out, e.def.Clone())
	}
	return out
}

// SetInteractionHandler sets (or clears with nil) the host's interaction channel.
// Unset means there is no interactive user (clarify errors, consequential tools
// are denied). It applies on the next dispatch, to running agents as well.
func (h *ToolHub) SetInteractionHandler(ih InteractionHandler) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.interaction = ih
}

// InteractionHandler returns the host's interaction channel, or nil when none is
// set.
func (h *ToolHub) InteractionHandler() InteractionHandler {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.interaction
}

// SetPolicy replaces the policy checked before every dispatch; it applies to
// the next Authorize call. A nil policy restores the default, so dispatch never
// meets a nil policy.
//
// An Authorized minted before the call refuses to execute afterwards (see
// ErrPolicyChanged): a decision made by the old policy is not valid under the
// new one.
func (h *ToolHub) SetPolicy(p Policy) {
	if p == nil {
		p = NewDefaultPolicy(DefaultPolicyOptions{})
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.policy = p
	h.gen++
}

// policyGeneration returns the number of SetPolicy calls so far.
func (h *ToolHub) policyGeneration() uint64 {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.gen
}

// Policy returns the current policy.
func (h *ToolHub) Policy() Policy {
	h.mu.RLock()
	defer h.mu.RUnlock()
	if h.policy == nil {
		return fallbackPolicy
	}
	return h.policy
}

// Authorize looks up the tool, runs the current policy on the exact arguments
// and, only when the policy allows the call, returns an Authorized holding the
// handler. The handler and the policy are read once per call, so SetPolicy
// takes effect on the next call. The registered definition, not a fresh call of
// the handler's Definition method, is what the policy sees.
//
// An unknown name returns an error whose text is `Tool "<name>" not available
// for this agent` and that matches ErrNotFound; the policy is not
// consulted. A denial returns the zero Authorized and the policy's Decision. A
// policy that panics is a denial with the reason `Policy check failed: <value>`.
// A nil tc.Emit is replaced with a no-op.
//
// args is copied exactly once, at the start of the call. The policy gets a copy
// of that snapshot and the Authorized keeps the snapshot itself, so the bytes the
// policy checked are the bytes that run, even if the caller changes its slice
// while the policy is running; neither a policy nor a later caller can change
// them.
//
// A nil or empty allowedTools means no allowlist: every registered tool passes
// that check. A caller that builds the list from an unset
// agent configuration must therefore not pass a nil list by accident.
func (h *ToolHub) Authorize(ctx context.Context, name string, args kernel.Arguments, tc Context, allowedTools []string) (Authorized, Decision, error) {
	h.mu.RLock()
	e, ok := h.tools.Get(name)
	policy := h.policy
	gen := h.gen
	h.mu.RUnlock()
	if !ok {
		return Authorized{}, Decision{}, &hubError{msg: `Tool "` + name + `" not available for this agent`, sentinel: ErrNotFound}
	}
	if policy == nil {
		policy = fallbackPolicy
	}
	if tc.Emit == nil {
		tc.Emit = func(string, any) {}
	}

	snapshot := bytes.Clone(args)
	decision := checkPolicy(ctx, policy, name, bytes.Clone(snapshot), tc, e.def.Clone(), allowedTools)
	if !decision.Allowed {
		return Authorized{}, decision, nil
	}
	return Authorized{hub: h, gen: gen, handler: e.handler, def: e.def.Clone(), args: snapshot, tc: tc, used: new(atomic.Bool)}, decision, nil
}

// checkPolicy calls the policy and turns a panic into a denial (fail closed).
func checkPolicy(ctx context.Context, policy Policy, name string, args kernel.Arguments, tc Context, def Definition, allowedTools []string) (d Decision) {
	defer func() {
		if r := recover(); r != nil {
			d = Deny(fmt.Sprintf("Policy check failed: %v", r))
		}
	}()
	return policy.Check(ctx, name, args, tc, def, allowedTools)
}

// Name returns the tool name.
func (a Authorized) Name() string { return a.def.Name }

// Definition returns a copy of the definition of the authorized tool.
func (a Authorized) Definition() Definition { return a.def.Clone() }

// Risk returns the risk of this call (A-101): the handler's RiskFor of the authorized
// arguments when it implements CallRisker, else the definition's Risk.
func (a Authorized) Risk() Risk {
	if r, ok := a.handler.(CallRisker); ok {
		return r.RiskFor(bytes.Clone(a.args))
	}
	return a.def.Risk
}

// Arguments returns a fresh copy of the authorized argument bytes.
func (a Authorized) Arguments() kernel.Arguments { return bytes.Clone(a.args) }

// Execute runs the handler with the authorized arguments and passes its
// (Output, error) through unchanged. On the zero value it returns an error
// matching ErrNotAuthorized without running anything. It runs at most once: a
// second call returns ErrAlreadyExecuted, and a call after SetPolicy returns
// ErrPolicyChanged; both match ErrNotAuthorized. A SetPolicy that lands between
// that check and the handler starting is not prevented.
//
// Converting an error into an isError output, approval, retry and recover all
// belong to the Phase 9 executor; Execute does none of them.
func (a Authorized) Execute(ctx context.Context) (Output, error) {
	if a.handler == nil || a.hub == nil || a.used == nil {
		return Output{}, ErrNotAuthorized
	}
	if a.hub.policyGeneration() != a.gen {
		return Output{}, ErrPolicyChanged
	}
	if !a.used.CompareAndSwap(false, true) {
		return Output{}, ErrAlreadyExecuted
	}
	return a.handler.Execute(ctx, bytes.Clone(a.args), a.tc)
}
