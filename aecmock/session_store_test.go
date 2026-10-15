package aecmock_test

import (
	"testing"

	"github.com/Phong95/aec-agent-go/aecmock"
	"github.com/Phong95/aec-agent-go/components/persistence"
	"github.com/Phong95/aec-agent-go/kernel"
	"github.com/Phong95/aec-agent-go/kernel/ordered"
)

func TestInMemorySessionStoreIdentity(t *testing.T) {
	s := aecmock.NewInMemorySessionStore(nil)
	if s.Name() != "in-memory-session-store" {
		t.Errorf("Name() = %q, want in-memory-session-store", s.Name())
	}
	if s.Version() != "1.0.0" {
		t.Errorf("Version() = %q, want 1.0.0", s.Version())
	}
	if err := kernel.Initialize(t.Context(), s); err != nil {
		t.Errorf("Initialize: %v", err)
	}
	if err := kernel.Dispose(t.Context(), s); err != nil {
		t.Errorf("Dispose: %v", err)
	}
}

func TestInMemorySessionStoreSharedBacking(t *testing.T) {
	ctx := t.Context()
	backing := persistence.NewSessionBacking()
	a := aecmock.NewInMemorySessionStore(backing)
	b := aecmock.NewInMemorySessionStore(backing)

	md := ordered.NewObject()
	md.Set("scope", "shared")
	created, err := a.Create(ctx, md)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := a.Dispose(ctx); err != nil {
		t.Fatalf("Dispose: %v", err)
	}

	// A second instance over the same backing sees the session of the first,
	// even after the first was disposed.
	got, err := b.Get(ctx, created.ID)
	if err != nil || got == nil {
		t.Fatalf("shared backing: Get = %v, %v; want the created session", got, err)
	}

	// A store without a backing has data of its own.
	fresh := aecmock.NewInMemorySessionStore(nil)
	other, err := fresh.Get(ctx, created.ID)
	if err != nil || other != nil {
		t.Fatalf("nil backing: Get = %v, %v; want an empty store", other, err)
	}
	// Two stores without a backing do not see each other either.
	fresh2 := aecmock.NewInMemorySessionStore(nil)
	s, err := fresh.Create(ctx, ordered.Object{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if v, _ := fresh2.Get(ctx, s.ID); v != nil {
		t.Fatalf("two nil-backing stores share data")
	}
}
