// Package artifacthub is the artifact hub: the per-agent artifact view over
// persistence.ArtifactStore and a sandboxhub.View.
//
// An artifact is a metadata tracker over one sandbox file: the file is the
// deliverable and the store keeps only ids, hashes, versions and provenance. The
// package touches files only through sandboxhub.View, never through the host (the
// arch test TestNoHostAccessInStoresAndHubs enforces it). It lives apart from
// artifact because it imports persistence, and persistence imports artifact.
package artifacthub

import (
	"context"
	"log/slog"
	"sync"

	"github.com/Phong95/aec-agent-go/components/artifacthub/artifact"
	"github.com/Phong95/aec-agent-go/components/events"
	"github.com/Phong95/aec-agent-go/components/persistence"
	"github.com/Phong95/aec-agent-go/internal/jsstr"
	"github.com/Phong95/aec-agent-go/internal/nilcheck"
	"github.com/Phong95/aec-agent-go/kernel"
)

// Compile-time proof that ArtifactHub is a plugin.
var _ kernel.Plugin = (*ArtifactHub)(nil)

// ArtifactHub is the global hub for artifacts (A-62) - like the tool hub and the sandbox
// hub, AgentCore owns one.
//
//	ArtifactHub (agent scoping)
//	  persistence.ArtifactHub.Artifacts   the durable store (in-memory by default, Postgres on the server)
//
// The hub itself keeps no state about artifacts: it hands out a per-agent
// artifact.View, which is what enforces ownership on every call. The store is
// resolved lazily through the getStore function, so a later
// Kernel.UsePersistence swap reaches views handed out earlier.
//
// Save, Update and Delete of one artifact (one agent id and name) are serialised,
// so two concurrent saves of one name cannot both pass the name check and a Delete
// cannot be undone by an Update that was already writing (sequential callers see
// no difference). ReleaseAgentData waits for the operations in flight on that
// agent. Different agents, and different names of one agent, run in parallel, so
// a slow sandbox write holds up only its own artifact - except while a
// ReleaseAgentData of that agent is pending: it waits for the slowest write in
// flight and the agent's new operations queue behind it.
// Every wait honours the context of its caller. The store is resolved once the
// locks are held, so an operation that queued across a UsePersistence swap uses the
// current store. See locks.go.
//
// Build a ArtifactHub with New: the zero value has no clock, id source or locks.
type ArtifactHub struct {
	getStore func() persistence.ArtifactStore
	now      func() int64
	newID    func() string
	// logger reports a failed release of the agent:removed subscription (WithEvents);
	// nil means slog.Default() (WithLogger).
	logger *slog.Logger
	// bus is set by WithEvents; New subscribes on it once every option is applied.
	bus events.Bus
	// unsubscribe ends that subscription (Dispose); nil without one.
	unsubscribe events.Unsubscribe

	// locks serialise the conflicting operations (see locks.go).
	locks *locks

	// callMu makes the calls of now and newID one at a time: the operations of
	// different artifacts run in parallel, and an injected clock or id source
	// (WithClock, WithIDs) need not be safe for concurrent use. It is never held
	// across store or sandbox I/O.
	callMu sync.Mutex
}

// Option configures a ArtifactHub.
type Option func(*ArtifactHub)

// WithClock sets the timestamp source in epoch milliseconds (default
// kernel.NowMillis). The hub formats it with kernel.MillisToISO (an ISO 8601 UTC
// timestamp with milliseconds), and reads it once per Save and once per Update. A
// nil clock is ignored.
func WithClock(now func() int64) Option {
	return func(h *ArtifactHub) {
		if now != nil {
			h.now = now
		}
	}
}

// WithIDs sets the artifact id source (default kernel.NewID, a random UUID). A nil
// function is ignored.
func WithIDs(next func() string) Option {
	return func(h *ArtifactHub) {
		if next != nil {
			h.newID = next
		}
	}
}

// NewArtifactHub returns a hub. getStore is resolved on every call, so Kernel.UsePersistence
// can swap the store; a nil getStore, or one that returns no store (or a typed
// nil), makes every call fail with the not_configured ArtifactError.
func NewArtifactHub(getStore func() persistence.ArtifactStore, opts ...Option) *ArtifactHub {
	h := &ArtifactHub{getStore: getStore, now: kernel.NowMillis, newID: kernel.NewID, locks: newLocks()}
	for _, o := range opts {
		if o != nil {
			o(h)
		}
	}
	if h.bus != nil {
		h.unsubscribe = events.SubscribeCtx(h.bus, events.AgentRemoved, func(ctx context.Context, _ kernel.Event, p events.AgentRemovedPayload) {
			// Same for its artifacts: they belong to the agent, so they go with it. Best effort:
			// a store that is down must not leave the agent half-deleted.
			if err := h.ReleaseAgentData(ctx, p.AgentID); err != nil {
				h.log().Warn("artifacts: could not delete the agent's artifacts", "agent", p.AgentID, "error", err)
			}
		})
	}
	return h
}

// WithEvents makes the hub delete a removed agent's artifacts itself: it subscribes
// to events.AgentRemoved on b and calls ReleaseAgentData with the publisher's ctx.
// The release is best effort: a failure is logged (agent id and error), never
// returned. Dispose ends the subscription. A nil bus is ignored.
func WithEvents(b events.Bus) Option {
	return func(h *ArtifactHub) {
		if b != nil {
			h.bus = b
		}
	}
}

// WithLogger sets the logger a failed release of the WithEvents subscription is
// reported to (default slog.Default()). A nil logger is ignored.
func WithLogger(l *slog.Logger) Option {
	return func(h *ArtifactHub) {
		if l != nil {
			h.logger = l
		}
	}
}

// log is the hub's logger, slog.Default() when none is set.
func (h *ArtifactHub) log() *slog.Logger {
	if h.logger != nil {
		return h.logger
	}
	return slog.Default()
}

// Dispose ends the agent:removed subscription of WithEvents. It is idempotent and
// never fails; the store is not touched.
func (h *ArtifactHub) Dispose(context.Context) error {
	h.callMu.Lock()
	unsubscribe := h.unsubscribe
	h.unsubscribe = nil
	h.callMu.Unlock()
	if unsubscribe != nil {
		unsubscribe()
	}
	return nil
}

// Name returns the plugin name, artifact-hub.
func (*ArtifactHub) Name() string { return "artifact-hub" }

// Version returns the plugin version, 1.0.0.
func (*ArtifactHub) Version() string { return "1.0.0" }

// ForAgent returns the view one agent works through. sessionID is stamped on
// versions written through it; "" leaves the view unscoped. A blank agent id
// fails with an invalid_argument *kernel.ArtifactError.
func (h *ArtifactHub) ForAgent(agentID, sessionID string) (artifact.View, error) {
	if jsstr.Trim(agentID) == "" {
		return nil, kernel.NewArtifactError("An agent id is required to reach artifacts", kernel.ArtifactCodeInvalidArgument, "")
	}
	return &view{hub: h, agentID: agentID, sessionID: sessionID}, nil
}

// ReleaseAgentData is the retention rule: an agent's artifacts live until the
// agent is deleted, then this removes them. It waits for the Save, Update and
// Delete of that agent that are already running, and gives up with the context
// error when ctx ends while it is still waiting (nothing is removed then; a ctx
// that is already done when the agent is idle does not stop the removal, because
// the gate is taken without waiting). While it waits it holds the
// agent's new Save, Update and Delete back, each of which honours its own context.
func (h *ArtifactHub) ReleaseAgentData(ctx context.Context, agentID string) error {
	// Fail fast, before any wait, when no store is configured.
	if _, err := h.store(); err != nil {
		return err
	}
	// Wait for the Save, Update and Delete of this agent in flight, and hold new
	// ones back until the cleanup is done, so none of them writes a record that
	// this call has just removed (or removes one that it is still writing).
	unlock, err := h.locks.lockAgent(ctx, agentID)
	if err != nil {
		return err
	}
	defer unlock()
	// Resolve the store only now: the wait can be long, and a UsePersistence swap
	// during it must be cleaned in the current store, not the one of before.
	store, err := h.store()
	if err != nil {
		return err
	}
	return store.DeleteByAgent(ctx, agentID)
}

// timestamp reads the clock and formats it as the ISO text of new Date().toISOString().
func (h *ArtifactHub) timestamp() string {
	h.callMu.Lock()
	defer h.callMu.Unlock()
	return kernel.MillisToISO(h.now())
}

// nextID draws an artifact id.
func (h *ArtifactHub) nextID() string {
	h.callMu.Lock()
	defer h.callMu.Unlock()
	return h.newID()
}

// store resolves the artifact store, or fails with not_configured.
func (h *ArtifactHub) store() (persistence.ArtifactStore, error) {
	if h.getStore != nil {
		if s := h.getStore(); !nilcheck.IsNil(s) {
			return s, nil
		}
	}
	return nil, kernel.NewArtifactError("No artifact store is configured on this server", kernel.ArtifactCodeNotConfigured, "")
}
