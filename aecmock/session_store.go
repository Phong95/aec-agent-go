package aecmock

import (
	"context"

	"github.com/Phong95/aec-agent-go/components/persistence"
	"github.com/Phong95/aec-agent-go/kernel"
)

// Compile-time proof that InMemorySessionStore is a session store and a plugin
// with both optional lifecycle hooks.
var (
	_ persistence.SessionStore = (*InMemorySessionStore)(nil)
	_ kernel.Plugin            = (*InMemorySessionStore)(nil)
	_ kernel.Initializer       = (*InMemorySessionStore)(nil)
	_ kernel.Disposer          = (*InMemorySessionStore)(nil)
)

// InMemorySessionStore is a mock session store for tests: a
// session store that Phase 7 and later tests wire into an agent without a
// database. It is a thin wrapper over persistence.MemorySessionStore, so it keeps
// that store's behaviour (deep copies in and out, the monotonic consolidation
// checkpoint, safe for concurrent use) and adds the plugin identity.
//
// Pass the same persistence.SessionBacking to several instances to make them
// share their data, which is what the durability conformance suite needs.
type InMemorySessionStore struct {
	*persistence.MemorySessionStore
}

// NewInMemorySessionStore returns a store over the given backing. A nil backing
// gives the store fresh data of its own.
func NewInMemorySessionStore(backing *persistence.SessionBacking) *InMemorySessionStore {
	return &InMemorySessionStore{
		MemorySessionStore: persistence.NewMemorySessionStore(persistence.WithSessionBacking(backing)),
	}
}

// Name returns the plugin name, in-memory-session-store.
func (*InMemorySessionStore) Name() string { return "in-memory-session-store" }

// Version returns the plugin version, 1.0.0.
func (*InMemorySessionStore) Version() string { return mockVersion }

// Initialize does nothing and returns nil.
func (*InMemorySessionStore) Initialize(context.Context) error { return nil }

// Dispose does nothing and returns nil. The data stays in the backing, so a new
// instance over the same backing still sees it.
func (*InMemorySessionStore) Dispose(context.Context) error { return nil }
