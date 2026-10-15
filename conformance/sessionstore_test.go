package conformance_test

import (
	"testing"

	"github.com/Phong95/aec-agent-go/aecmock"
	"github.com/Phong95/aec-agent-go/components/persistence"
	"github.com/Phong95/aec-agent-go/conformance"
)

// TestSessionStoreInMemory runs the suite on a fresh
// aecmock.InMemorySessionStore per subtest.
func TestSessionStoreInMemory(t *testing.T) {
	conformance.SessionStore(t, func(_ *testing.T) persistence.SessionStore {
		return aecmock.NewInMemorySessionStore(nil)
	})
}

// TestSessionStoreDefault runs the suite on the persistence default.
func TestSessionStoreDefault(t *testing.T) {
	conformance.SessionStore(t, func(_ *testing.T) persistence.SessionStore {
		return persistence.NewMemorySessionStore()
	})
}

// TestSessionStoreDurabilityShared runs the durability suite on aecmock stores
// that share one backing for the whole run, so each new store sees the same
// sessions.
func TestSessionStoreDurabilityShared(t *testing.T) {
	backing := persistence.NewSessionBacking()
	conformance.SessionStoreDurability(t, func(_ *testing.T) persistence.SessionStore {
		return aecmock.NewInMemorySessionStore(backing)
	})
}

// TestSessionStoreDurabilityDefault runs the durability suite on the persistence
// default over one shared backing.
func TestSessionStoreDurabilityDefault(t *testing.T) {
	backing := persistence.NewSessionBacking()
	conformance.SessionStoreDurability(t, func(_ *testing.T) persistence.SessionStore {
		return persistence.NewMemorySessionStore(persistence.WithSessionBacking(backing))
	})
}
