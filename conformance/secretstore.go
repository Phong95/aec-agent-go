package conformance

import (
	"testing"

	"github.com/Phong95/aec-agent-go/components/persistence"
	"github.com/Phong95/aec-agent-go/components/secrethub/secret"
	"github.com/Phong95/aec-agent-go/conformance/internal/check"
	"github.com/Phong95/aec-agent-go/kernel"
)

// SecretStoreFactory returns a persistence.SecretStore. The suite empties the store before
// each case (it deletes every listed secret), so do not point it at a store that holds
// data you need.
type SecretStoreFactory func(t *testing.T) persistence.SecretStore

// secretFixture builds a stored secret named name, changed by mut.
func secretFixture(name string, mut func(*secret.Stored)) secret.Stored {
	s := secret.Stored{
		Info: secret.Info{
			Name:         name,
			AllowedHosts: []string{"api.example.com"},
			AllowedTools: []string{"http_request"},
			KeyID:        "local:000000000000",
			CreatedBy:    secret.CreatorUser,
			CreatedAt:    "2026-10-13T00:00:00.000Z",
			UpdatedAt:    "2026-10-13T00:00:01.000Z",
		},
		Value: "enc:v2:sealed",
	}
	if mut != nil {
		mut(&s)
	}
	return s
}

func secretKeys(ss []secret.Stored) []string {
	out := make([]string, len(ss))
	for i, s := range ss {
		out[i] = s.OwnerAgentID + "/" + s.Name
	}
	return out
}

// SecretStore runs the conformance suite for persistence.SecretStore implementations
// (A-101), the testSecretStoreConformance suite of core/ts. It checks the invariants the
// SecretHub relies on: secrets round-trip with every field, (owner, name) is the key - a
// global secret ("" owner) and an agent's secret of the same name are different rows -
// Save replaces, lists are scoped and sorted, MarkUsed records only the time, and
// DeleteByOwner removes only that agent's secrets. An implementation that passes it can be
// plugged in through persistence.WithSecrets.
func SecretStore(t *testing.T, newStore SecretStoreFactory) {
	t.Helper()

	fresh := func(t *testing.T) persistence.SecretStore {
		t.Helper()
		s := newStore(t)
		check.NoErr(t, kernel.Initialize(t.Context(), s))
		all, err := s.ListAll(t.Context())
		check.NoErr(t, err, "listAll")
		for _, v := range all {
			check.NoErr(t, s.Delete(t.Context(), v.OwnerAgentID, v.Name), "empty the store")
		}
		return s
	}
	save := func(t *testing.T, s persistence.SecretStore, v secret.Stored) {
		t.Helper()
		check.NoErr(t, s.Save(t.Context(), v), "save %s/%s", v.OwnerAgentID, v.Name)
	}
	get := func(t *testing.T, s persistence.SecretStore, owner, name string) *secret.Stored {
		t.Helper()
		got, err := s.Get(t.Context(), owner, name)
		check.NoErr(t, err, "get %s/%s", owner, name)
		return got
	}
	list := func(t *testing.T, s persistence.SecretStore, owner string) []secret.Stored {
		t.Helper()
		out, err := s.List(t.Context(), owner)
		check.NoErr(t, err, "list %q", owner)
		return out
	}
	listAll := func(t *testing.T, s persistence.SecretStore) []secret.Stored {
		t.Helper()
		out, err := s.ListAll(t.Context())
		check.NoErr(t, err, "listAll")
		return out
	}

	t.Run("initialize and dispose return no error", func(t *testing.T) {
		s := newStore(t)
		check.NoErr(t, kernel.Initialize(t.Context(), s))
		check.NoErr(t, kernel.Dispose(t.Context(), s))
	})

	t.Run("save() + get() round-trip every field", func(t *testing.T) {
		s := fresh(t)
		full := secretFixture("FULL_KEY", func(v *secret.Stored) {
			v.OwnerAgentID = "agent-a"
			v.Description = "the weather API"
			v.AllowedHosts = []string{"api.example.com", "*.example.org"}
			v.AllowedTools = []string{"http_request", "terminal"}
			v.KeyID = "local:abc123abc123"
			v.CreatedBy = secret.CreatorAgent
			v.LastUsedAt = "2026-10-13T00:00:09.000Z"
			v.Value = "enc:v2:xyz"
		})
		bare := secretFixture("BARE_KEY", func(v *secret.Stored) {
			v.AllowedHosts = []string{}
			v.AllowedTools = []string{}
			v.KeyID = ""
		})
		save(t, s, full)
		save(t, s, bare)
		for _, want := range []secret.Stored{full, bare} {
			got := get(t, s, want.OwnerAgentID, want.Name)
			if got == nil {
				t.Fatalf("secret %s is absent", want.Name)
			}
			check.DeepEqual(t, *got, want, "round-trip of %s", want.Name)
		}
	})

	t.Run("get() returns nil for an unknown secret, or the right name under another owner", func(t *testing.T) {
		s := fresh(t)
		save(t, s, secretFixture("ONLY_GLOBAL", nil))
		check.True(t, get(t, s, "", "MISSING") == nil, "unknown secret must be nil")
		check.True(t, get(t, s, "agent-a", "ONLY_GLOBAL") == nil, "another owner's name must be nil")
	})

	t.Run("a global and an agent secret with the same name are separate", func(t *testing.T) {
		s := fresh(t)
		save(t, s, secretFixture("SHARED", func(v *secret.Stored) { v.Value = "global-value" }))
		save(t, s, secretFixture("SHARED", func(v *secret.Stored) { v.OwnerAgentID = "agent-a"; v.Value = "agent-value" }))
		check.Equal(t, get(t, s, "", "SHARED").Value, "global-value")
		check.Equal(t, get(t, s, "agent-a", "SHARED").Value, "agent-value")
	})

	t.Run("save() replaces by (owner, name)", func(t *testing.T) {
		s := fresh(t)
		save(t, s, secretFixture("KEY", func(v *secret.Stored) { v.Value = "one" }))
		save(t, s, secretFixture("KEY", func(v *secret.Stored) { v.Value = "two"; v.Description = "changed" }))
		got := get(t, s, "", "KEY")
		check.Equal(t, got.Value, "two")
		check.Equal(t, got.Description, "changed")
		check.Len(t, list(t, s, ""), 1)
	})

	t.Run("list(owner) is scoped and sorted by name; listAll() returns all, globals first", func(t *testing.T) {
		s := fresh(t)
		save(t, s, secretFixture("ZED", func(v *secret.Stored) { v.OwnerAgentID = "agent-b" }))
		save(t, s, secretFixture("BETA", nil))
		save(t, s, secretFixture("ALPHA", nil))
		save(t, s, secretFixture("MID", func(v *secret.Stored) { v.OwnerAgentID = "agent-a" }))
		check.DeepEqual(t, secretKeys(list(t, s, "")), []string{"/ALPHA", "/BETA"}, "globals")
		check.DeepEqual(t, secretKeys(list(t, s, "agent-a")), []string{"agent-a/MID"}, "agent-a")
		check.DeepEqual(t, secretKeys(listAll(t, s)), []string{"/ALPHA", "/BETA", "agent-a/MID", "agent-b/ZED"}, "all")
	})

	t.Run("markUsed() sets lastUsedAt only, and ignores a missing secret", func(t *testing.T) {
		s := fresh(t)
		v := secretFixture("USED", func(v *secret.Stored) { v.OwnerAgentID = "agent-a" })
		save(t, s, v)
		check.NoErr(t, s.MarkUsed(t.Context(), "agent-a", "USED", "2026-10-13T01:02:03.000Z"))
		check.NoErr(t, s.MarkUsed(t.Context(), "agent-a", "GONE", "2026-10-13T01:02:03.000Z"))
		want := v.Clone()
		want.LastUsedAt = "2026-10-13T01:02:03.000Z"
		got := get(t, s, "agent-a", "USED")
		if got == nil {
			t.Fatal("secret is absent")
		}
		check.DeepEqual(t, *got, want, "marked secret")
		check.True(t, get(t, s, "agent-a", "GONE") == nil, "markUsed must not create a secret")
	})

	t.Run("delete() removes one secret; deleteByOwner() removes only that agent's", func(t *testing.T) {
		s := fresh(t)
		save(t, s, secretFixture("G1", nil))
		save(t, s, secretFixture("A1", func(v *secret.Stored) { v.OwnerAgentID = "agent-a" }))
		save(t, s, secretFixture("A2", func(v *secret.Stored) { v.OwnerAgentID = "agent-a" }))
		save(t, s, secretFixture("B1", func(v *secret.Stored) { v.OwnerAgentID = "agent-b" }))
		check.NoErr(t, s.Delete(t.Context(), "agent-a", "A1"))
		check.DeepEqual(t, secretKeys(list(t, s, "agent-a")), []string{"agent-a/A2"}, "after delete")
		check.NoErr(t, s.DeleteByOwner(t.Context(), "agent-a"))
		check.Len(t, list(t, s, "agent-a"), 0)
		check.DeepEqual(t, secretKeys(listAll(t, s)), []string{"/G1", "agent-b/B1"}, "the others stay")
	})

	t.Run("returned secrets are copies", func(t *testing.T) {
		s := fresh(t)
		save(t, s, secretFixture("COPY", func(v *secret.Stored) { v.AllowedHosts = []string{"a.example.com"} }))
		got := get(t, s, "", "COPY")
		got.AllowedHosts[0] = "evil.example.com"
		got.AllowedHosts = append(got.AllowedHosts, "more.example.com")
		listed := list(t, s, "")
		listed[0].AllowedHosts[0] = "evil.example.com"
		check.DeepEqual(t, get(t, s, "", "COPY").AllowedHosts, []string{"a.example.com"}, "stored hosts")
	})
}
