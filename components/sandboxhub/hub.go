package sandboxhub

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/Phong95/aec-agent-go/components/events"
	"github.com/Phong95/aec-agent-go/internal/nilcheck"
	"github.com/Phong95/aec-agent-go/kernel"
)

// sharedFuture is the in-flight (or finished) creation of the shared sandbox.
// sb and err are written once, under the hub lock, before done is closed, so a
// reader that has received from done may read them without the lock. orphaned is
// guarded by the hub lock.
type sharedFuture struct {
	done   chan struct{}
	sb     Sandbox
	err    error
	cancel context.CancelFunc
	// orphaned is set when Dispose stopped waiting for this creation, so the
	// goroutine that finishes it must dispose the sandbox itself.
	orphaned bool
}

// result returns the outcome of a finished future.
func (f *sharedFuture) result() (Sandbox, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.sb, nil
}

// wait returns the outcome of the future once it is done, or ctx.Err() when ctx
// ends first. A finished future wins over a cancelled ctx.
func (f *sharedFuture) wait(ctx context.Context) (Sandbox, error) {
	select {
	case <-f.done:
		return f.result()
	default:
	}
	select {
	case <-f.done:
		return f.result()
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// SandboxHub is the global hub for sandboxes - like the LLM provider hub and the tool
// hub, one per AgentCore:
//
//	SandboxHub (shared / general concerns)
//	  |- providers       one factory per kind (docker now; host, remote later)
//	  `- shared sandbox  the one sandbox every agent uses by default
//
// Own sandboxes (one per agent) are managed by each agent's SandboxManager.
// Sandboxes that several agents share but that are NOT the shared one (a host
// folder, a user's own machine over a remote runner) are deferred: they need an
// instance registry keyed by target - see the note on Selection in types.go. The
// hub provides BuildOwnSpec to validate own configs against policy, and
// ReleaseAgentData to clean up an agent's data in the shared sandbox when it is
// deleted (its own sandbox's data is deleted by SandboxManager's release with
// removeData).
//
// The shared sandbox is created lazily on the first Resolve and cached; a failed
// creation is not cached, so the next call retries. A SandboxHub is safe for concurrent
// use: one mutex guards its state, and the creation is a future that every
// concurrent Resolve waits on, so a provider's Create runs once.
//
// The creation runs on a context detached from the first caller (a caller that
// gives up does not fail the others) and is cancelled by Dispose, and a creation
// that finishes after Dispose began is disposed instead of leaked.
//
// Create a SandboxHub with NewSandboxHub; the zero value is not usable.
type SandboxHub struct {
	mu         sync.Mutex
	providers  map[Kind]Provider
	sharedSpec *Spec
	policy     HubPolicy
	// shared is the running shared sandbox, created on first use. A failed
	// creation is cleared, so the next call retries.
	shared *sharedFuture
	// approval is the policy for actions that need approval (like a write on the
	// host sandbox).
	approval ApprovalPolicy
	// logger reports the errors the hub cannot return (a failed dispose of the
	// shared sandbox); nil means slog.Default() (SetLogger).
	logger *slog.Logger
	// unsubscribe ends the agent:removed subscription of WithEvents; nil without one.
	unsubscribe events.Unsubscribe
}

// Option configures a SandboxHub built by NewSandboxHub.
type Option func(*SandboxHub)

// WithEvents makes the hub release a removed agent's data itself: it subscribes to
// events.AgentRemoved on b and calls ReleaseAgentData with the publisher's ctx. The
// release is best effort: a failure is logged (agent id and error) through the
// hub's logger, never returned. Dispose ends the subscription. A nil bus is ignored.
func WithEvents(b events.Bus) Option {
	return func(h *SandboxHub) {
		if b == nil {
			return
		}
		h.unsubscribe = events.SubscribeCtx(b, events.AgentRemoved, func(ctx context.Context, _ kernel.Event, p events.AgentRemovedPayload) {
			// ...and its folder in the shared sandbox by the hub.
			if err := h.ReleaseAgentData(ctx, p.AgentID); err != nil {
				h.log().Warn("sandbox: could not delete the agent's sandbox files", "agent", p.AgentID, "error", err)
			}
		})
	}
}

// orphanDisposeTimeout bounds the dispose of a shared sandbox whose creation finished
// after Dispose stopped waiting: nobody waits for it, so it must not run forever.
const orphanDisposeTimeout = 30 * time.Second

// SetLogger sets the logger the hub reports a failed sandbox dispose to, at Warn
// with the sandbox id and the error. A nil logger restores slog.Default().
func (h *SandboxHub) SetLogger(l *slog.Logger) {
	h.mu.Lock()
	h.logger = l
	h.mu.Unlock()
}

// log is the hub's logger, slog.Default() when none is set.
func (h *SandboxHub) log() *slog.Logger {
	h.mu.Lock()
	l := h.logger
	h.mu.Unlock()
	if l == nil {
		return slog.Default()
	}
	return l
}

// disposeLogged disposes sb (keeping its data) and logs a failure at Warn: the error
// has no caller to go to.
func (h *SandboxHub) disposeLogged(ctx context.Context, sb Sandbox, reason string) {
	if err := sb.Dispose(ctx, false); err != nil {
		h.log().LogAttrs(ctx, slog.LevelWarn, "sandbox hub: dispose of the shared sandbox failed",
			slog.String("sandboxId", sb.ID()), slog.String("reason", reason), slog.Any("error", err))
	}
}

// NewSandboxHub returns a SandboxHub with the default policy (DefaultHubPolicy), the default
// approval policy (DefaultApprovalPolicy) and no providers.
func NewSandboxHub(opts ...Option) *SandboxHub {
	h := &SandboxHub{
		providers: map[Kind]Provider{},
		policy:    DefaultHubPolicy(),
		approval:  DefaultApprovalPolicy,
	}
	for _, o := range opts {
		if o != nil {
			o(h)
		}
	}
	return h
}

// --- Registration (host code only) ---------------------------------------------

// Register adds a provider under its Kind. A nil provider (a nil interface or a
// typed nil pointer) returns ErrNilProvider; a second provider of a kind returns
// an error matching ErrProviderAlreadyRegistered.
func (h *SandboxHub) Register(p Provider) error {
	if nilcheck.IsNil(p) {
		return ErrNilProvider
	}
	kind := p.Kind()
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, taken := h.providers[kind]; taken {
		return &hubError{
			msg:      `A sandbox provider for "` + string(kind) + `" is already registered`,
			sentinel: ErrProviderAlreadyRegistered,
		}
	}
	h.providers[kind] = p
	return nil
}

// Provider returns the provider registered for kind .
func (h *SandboxHub) Provider(kind Kind) (Provider, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	p, ok := h.providers[kind]
	return p, ok
}

// ConfigureShared sets the sandbox every agent uses by default. It must be set
// before it is first used: once the shared sandbox exists or is being created it
// returns an error matching ErrSharedRunning. The hub keeps a copy of spec.
func (h *SandboxHub) ConfigureShared(spec Spec) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.shared != nil {
		return &hubError{
			msg:      "The shared sandbox is already running \U00002014 configure it before the first agent uses it",
			sentinel: ErrSharedRunning,
		}
	}
	c := spec.Clone()
	h.sharedSpec = &c
	return nil
}

// SharedConfigured reports whether ConfigureShared has been called.
func (h *SandboxHub) SharedConfigured() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.sharedSpec != nil
}

// --- Selection -------------------------------------------------------------------

// ValidateSelection checks a profile's selection without creating anything - it
// lets the server reject a bad agent config at save time. A zero Selection means
// shared. It returns a *kernel.SandboxError
// (policy_denied, not_configured, disabled, provider_unavailable).
func (h *SandboxHub) ValidateSelection(agentID string, sel Selection) error {
	// "none" is always a valid choice - it only fails when a tool asks for a sandbox.
	if err := AssertSafeAgentID(agentID); err != nil {
		return err
	}
	if sel.Mode == SelectionNone {
		return nil
	}
	spec, err := h.specFor(sel)
	if err != nil {
		return err
	}
	if _, ok := h.Provider(spec.Kind); !ok {
		return kernel.NewSandboxError(
			`No sandbox provider is registered for "`+string(spec.Kind)+`"`,
			kernel.SandboxCodeProviderUnavailable, "")
	}
	return nil
}

// Resolve returns the view of the shared sandbox an agent works in, creating the
// sandbox on first use. Own sandboxes go through SandboxManager: an own selection
// returns an error matching ErrOwnSelection. A zero Selection means shared.
//
// Concurrent callers share one creation. ctx bounds only this caller's wait: when
// it ends first Resolve returns ctx.Err() and the creation carries on for the
// others. Errors from the provider's Create or ForAgent are wrapped as
// environment_unavailable unless they already are *kernel.SandboxError.
func (h *SandboxHub) Resolve(ctx context.Context, agentID string, sel Selection) (View, error) {
	if sel.Mode == SelectionOwn {
		return nil, &hubError{
			msg:      "Own sandboxes are managed by the agent's SandboxManager, not SandboxHub.resolve()",
			sentinel: ErrOwnSelection,
		}
	}
	if err := AssertSafeAgentID(agentID); err != nil {
		return nil, err
	}
	spec, err := h.specFor(sel)
	if err != nil {
		return nil, err
	}
	sb, err := h.getOrCreateShared(ctx, spec)
	if err != nil {
		return nil, err
	}
	view, err := sb.ForAgent(ctx, agentID)
	if err != nil {
		return nil, kernel.AsSandboxError(err, sharedSandboxID(spec))
	}
	return view, nil
}

// --- Lifecycle -------------------------------------------------------------------

// ReleaseAgentData cleans up an agent's data when it is deleted: the agent's
// folder inside the shared sandbox. If the shared sandbox is running (a pending
// creation is awaited, and its failure is ignored) it uses the sandbox's optional
// AgentDataRemover; otherwise the provider's optional ProviderDataRemover.
// Own-sandbox data is removed by SandboxManager.
func (h *SandboxHub) ReleaseAgentData(ctx context.Context, agentID string) error {
	if err := AssertSafeAgentID(agentID); err != nil {
		return err
	}
	h.mu.Lock()
	var spec Spec
	configured := h.sharedSpec != nil
	if configured {
		spec = h.sharedSpec.Clone()
	}
	fut := h.shared
	provider := h.providers[spec.Kind]
	h.mu.Unlock()
	if !configured {
		return nil
	}

	var running Sandbox
	if fut != nil {
		sb, err := fut.wait(ctx)
		switch {
		case err == nil:
			running = sb
		case ctx.Err() != nil:
			return ctx.Err()
		}
	}
	if running != nil {
		if r, ok := running.(AgentDataRemover); ok {
			return r.RemoveAgentData(ctx, agentID)
		}
		return nil
	}
	if r, ok := provider.(ProviderDataRemover); ok && !nilcheck.IsNil(provider) {
		return r.RemoveData(ctx, spec, CreateContext{ID: sharedSandboxID(spec), Tenancy: TenancyShared}, agentID)
	}
	return nil
}

// Dispose stops the shared sandbox. Files are kept (volumes / folders) for the
// next start (removeData is false, and the sandbox's own error is logged, not
// returned). It takes the shared future and clears the field first, so a later
// ConfigureShared and Resolve start afresh, then cancels a creation in flight and
// waits for it. If ctx ends while waiting, Dispose returns ctx.Err() and the
// goroutine that finishes the creation disposes the sandbox, so none is leaked.
func (h *SandboxHub) Dispose(ctx context.Context) error {
	h.mu.Lock()
	fut := h.shared
	h.shared = nil
	unsubscribe := h.unsubscribe
	h.unsubscribe = nil
	h.mu.Unlock()
	if unsubscribe != nil {
		unsubscribe()
	}
	if fut == nil {
		return nil
	}
	fut.cancel()

	select {
	case <-fut.done:
	case <-ctx.Done():
		h.mu.Lock()
		select {
		case <-fut.done:
			// Finished meanwhile: dispose it here after all.
		default:
			fut.orphaned = true
			h.mu.Unlock()
			return ctx.Err()
		}
		h.mu.Unlock()
	}
	// A failed creation has nothing to stop. The sandbox's own error is not
	// returned, but logged at Warn.
	if fut.err == nil && fut.sb != nil {
		h.disposeLogged(ctx, fut.sb, "hub dispose")
	}
	return nil
}

// --- Internals -------------------------------------------------------------------

func sharedSandboxID(spec Spec) string {
	return string(spec.Kind) + ":shared"
}

// specFor is the spec a selection asks for. Own specs are checked against the
// policy; "none" returns the disabled error. The zero Selection is shared.
func (h *SandboxHub) specFor(sel Selection) (Spec, error) {
	mode := sel.Mode
	if mode == "" {
		mode = SelectionShared
	}
	switch mode {
	case SelectionNone:
		return Spec{}, kernel.NewSandboxError(`This agent has no sandbox (sandbox: "none")`, kernel.SandboxCodeDisabled, "")
	case SelectionShared:
		h.mu.Lock()
		defer h.mu.Unlock()
		if h.sharedSpec == nil {
			return Spec{}, kernel.NewSandboxError("No shared sandbox is configured on this server", kernel.SandboxCodeNotConfigured, "")
		}
		return h.sharedSpec.Clone(), nil
	case SelectionOwn:
		var own OwnConfig // zero when the selection has no own config
		if sel.Own != nil {
			own = *sel.Own
		}
		return h.BuildOwnSpec(own)
	default:
		return Spec{}, kernel.NewSandboxError("Unrecognised sandbox selection", kernel.SandboxCodePolicyDenied, "")
	}
}

// getOrCreateShared returns the shared sandbox, starting its creation if no one
// has. The creation runs in one goroutine on a context detached from ctx; every
// caller, the first included, waits on the future with its own ctx.
func (h *SandboxHub) getOrCreateShared(ctx context.Context, spec Spec) (Sandbox, error) {
	id := sharedSandboxID(spec)

	h.mu.Lock()
	fut := h.shared
	if fut == nil {
		provider, ok := h.providers[spec.Kind]
		if !ok {
			h.mu.Unlock()
			return nil, kernel.NewSandboxError(
				`No sandbox provider is registered for "`+string(spec.Kind)+`"`,
				kernel.SandboxCodeProviderUnavailable, id)
		}
		if err := ctx.Err(); err != nil {
			h.mu.Unlock()
			return nil, err
		}
		createCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
		fut = &sharedFuture{done: make(chan struct{}), cancel: cancel}
		h.shared = fut
		go h.create(createCtx, fut, provider, spec, id)
	}
	h.mu.Unlock()

	return fut.wait(ctx)
}

// create runs provider.Create and publishes the outcome on fut. A failed
// creation is not cached - the next call retries (e.g. Docker Desktop was still
// starting): the cache is cleared only when it still holds this future (Dispose
// may have replaced it), before done is closed so a waiter that sees the error
// can already retry. A panic in the provider becomes an error, because this
// goroutine has no caller to recover it.
func (h *SandboxHub) create(ctx context.Context, fut *sharedFuture, p Provider, spec Spec, id string) {
	sb, err := safeCreate(ctx, p, spec, id)

	var orphan Sandbox
	h.mu.Lock()
	switch {
	case err != nil:
		if h.shared == fut {
			h.shared = nil
		}
		fut.err = kernel.AsSandboxError(err, id)
	default:
		fut.sb = sb
		if fut.orphaned {
			orphan = sb
		}
	}
	// Closed under the lock: Dispose checks done and sets orphaned under the same
	// lock, so exactly one of the two disposes the sandbox.
	close(fut.done)
	h.mu.Unlock()

	if orphan != nil {
		// Nobody waits for this dispose, so it is bounded and its error is logged.
		dctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), orphanDisposeTimeout)
		defer cancel()
		h.disposeLogged(dctx, orphan, "creation finished after Dispose gave up")
	}
}

func safeCreate(ctx context.Context, p Provider, spec Spec, id string) (sb Sandbox, err error) {
	defer func() {
		if r := recover(); r != nil {
			sb, err = nil, fmt.Errorf("provider panicked: %v", r)
		}
	}()
	sb, err = p.Create(ctx, spec, CreateContext{ID: id, Tenancy: TenancyShared})
	if err == nil && nilcheck.IsNil(sb) {
		return nil, errors.New("provider returned a nil sandbox")
	}
	return sb, err
}
