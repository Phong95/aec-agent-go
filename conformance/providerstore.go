package conformance

import (
	"fmt"
	"slices"
	"sync"
	"testing"

	"github.com/Phong95/aec-agent-go/components/llmhub"
	"github.com/Phong95/aec-agent-go/components/persistence"
	"github.com/Phong95/aec-agent-go/conformance/internal/check"
	"github.com/Phong95/aec-agent-go/kernel"
)

// ProviderStoreFactory returns a persistence.ProviderStore. See the package
// comment for the freshness rules of the store suites.
type ProviderStoreFactory func(t *testing.T) persistence.ProviderStore

// providerModels builds two model records with every kind of optional part: the
// pointers, the lists and the nested capabilities and pricing. Values are fixed,
// so records built twice are equal.
func providerModels(providerName string) []llmhub.ModelRecord {
	window, maxOut := 200000, 8192
	yes, no := true, false
	in, out := 3.0, 15.0
	return []llmhub.ModelRecord{
		{
			ID: providerName + "-large", Provider: providerName,
			DisplayName: "Large", Description: "The large model.", OwnedBy: "acme", CreatedAt: "2026-01-01T00:00:00.000Z",
			ContextWindowTokens: &window, MaxOutputTokens: &maxOut,
			InputModalities: []string{"text", "image"}, OutputModalities: []string{"text"},
			Capabilities: &llmhub.ModelCapabilities{Vision: &yes, ToolCalling: &yes, Reasoning: &no},
			Pricing:      &llmhub.ModelPricing{InputUSDPerMillionTokens: &in, OutputUSDPerMillionTokens: &out},
		},
		{ID: providerName + "-small", Provider: providerName},
	}
}

// providerRecordFor builds a plain API-key provider; the credential is an
// obviously fake value.
func providerRecordFor(name string) persistence.ProviderRecord {
	return persistence.ProviderRecord{
		Name: name, Type: persistence.ProviderOpenAICompatible,
		Models:  providerModels(name),
		APIKey:  "test-api-key-not-real",
		BaseURL: "https://llm.example.invalid/v1",
	}
}

// sameProviderRecord compares two records: the scalar fields exactly, the models
// by their decoded JSON, and the expiry by value. A nil and an empty Models list
// are the same value.
func sameProviderRecord(a, b persistence.ProviderRecord) bool {
	if a.Name != b.Name || a.Type != b.Type || a.APIKey != b.APIKey || a.BaseURL != b.BaseURL ||
		a.SecretKey != b.SecretKey || a.Region != b.Region || a.AuthType != b.AuthType ||
		a.OAuthAccessToken != b.OAuthAccessToken || a.RefreshToken != b.RefreshToken ||
		a.ChatgptAccountID != b.ChatgptAccountID {
		return false
	}
	if (a.TokenExpiresAt == nil) != (b.TokenExpiresAt == nil) ||
		(a.TokenExpiresAt != nil && *a.TokenExpiresAt != *b.TokenExpiresAt) {
		return false
	}
	if len(a.Models) != len(b.Models) {
		return false
	}
	for i := range a.Models {
		if !jsonEqual(a.Models[i], b.Models[i]) {
			return false
		}
	}
	return true
}

func saveProvider(t *testing.T, s persistence.ProviderStore, p persistence.ProviderRecord) {
	t.Helper()
	check.NoErr(t, s.Save(t.Context(), p), "save %s", p.Name)
}

// loadProvider returns the record and fails the test when it is absent.
func loadProvider(t *testing.T, s persistence.ProviderStore, name string) persistence.ProviderRecord {
	t.Helper()
	got, err := s.Load(t.Context(), name)
	check.NoErr(t, err, "load %s", name)
	if got == nil {
		t.Fatalf("provider %s is absent, want it stored", name)
	}
	return *got
}

// providerNames lists the names of every stored record.
func providerNames(t *testing.T, s persistence.ProviderStore) []string {
	t.Helper()
	list, err := s.List(t.Context())
	check.NoErr(t, err, "list")
	names := make([]string, 0, len(list))
	for _, p := range list {
		names = append(names, p.Name)
	}
	return names
}

// ProviderStore runs the conformance suite for persistence.ProviderStore
// implementations.
//
// It is written for server/go's Postgres stores: save is an upsert by name, load of an
// unknown name is nil, list contains every saved record, delete removes one
// record, and the models, the secrets and the OAuth fields survive a round trip.
// Every provider name carries a per-run prefix and every credential is an
// obviously fake value. Models are compared by their decoded JSON, never byte for
// byte (a JSONB column reorders keys), and the authentication type is only
// compared for the OAuth record, because a Postgres store defaults it. No subtest
// assumes a list order. The Go-only subtests prove that records are copied in and
// out and that concurrent saves keep every record.
func ProviderStore(t *testing.T, newStore ProviderStoreFactory) {
	t.Helper()

	t.Run("save() then load() round-trips a provider with models", func(t *testing.T) {
		store := newStore(t)
		check.NoErr(t, kernel.Initialize(t.Context(), store))
		p := providerRecordFor("prov-" + runPrefix())

		saveProvider(t, store, p)

		got := loadProvider(t, store, p.Name)
		// A Postgres store may add a default authType; the plain record does not
		// set one, so it is not part of this comparison.
		got.AuthType = p.AuthType
		check.True(t, sameProviderRecord(got, p), "loaded %+v, want %+v", got, p)
	})

	t.Run("models survive a round trip with every optional part", func(t *testing.T) {
		store := newStore(t)
		check.NoErr(t, kernel.Initialize(t.Context(), store))
		p := providerRecordFor("prov-" + runPrefix())

		saveProvider(t, store, p)

		got := loadProvider(t, store, p.Name)
		check.Len(t, got.Models, len(p.Models))
		for i := range min(len(got.Models), len(p.Models)) {
			check.True(t, jsonEqual(got.Models[i], p.Models[i]), "model %d is %+v, want %+v", i, got.Models[i], p.Models[i])
		}
		if len(got.Models) > 0 && got.Models[0].ContextWindowTokens != nil {
			check.Equal(t, *got.Models[0].ContextWindowTokens, 200000)
		} else {
			t.Errorf("the explicit contextWindowTokens of the first model was lost")
		}
	})

	t.Run("a provider without models round-trips with an empty list", func(t *testing.T) {
		store := newStore(t)
		check.NoErr(t, kernel.Initialize(t.Context(), store))
		p := providerRecordFor("prov-" + runPrefix())
		p.Models = nil

		saveProvider(t, store, p)

		got := loadProvider(t, store, p.Name)
		check.Len(t, got.Models, 0)
	})

	t.Run("secrets and region survive a round trip", func(t *testing.T) {
		store := newStore(t)
		check.NoErr(t, kernel.Initialize(t.Context(), store))
		p := persistence.ProviderRecord{
			Name: "prov-" + runPrefix(), Type: persistence.ProviderBedrock,
			APIKey: "test-access-key-not-real", SecretKey: "test-secret-key-not-real", Region: "eu-west-1",
		}

		saveProvider(t, store, p)

		got := loadProvider(t, store, p.Name)
		check.Equal(t, got.Type, persistence.ProviderBedrock)
		check.Equal(t, got.APIKey, "test-access-key-not-real")
		check.Equal(t, got.SecretKey, "test-secret-key-not-real")
		check.Equal(t, got.Region, "eu-west-1")
	})

	t.Run("OAuth fields and tokenExpiresAt survive a round trip", func(t *testing.T) {
		store := newStore(t)
		check.NoErr(t, kernel.Initialize(t.Context(), store))
		expires := int64(1893456000000)
		p := persistence.ProviderRecord{
			Name: "prov-" + runPrefix(), Type: persistence.ProviderOpenAISubscription,
			AuthType: llmhub.AuthOAuth, OAuthAccessToken: "test-access-token-not-real", RefreshToken: "test-refresh-token-not-real",
			TokenExpiresAt: &expires, ChatgptAccountID: "acct-not-real",
		}

		saveProvider(t, store, p)

		got := loadProvider(t, store, p.Name)
		check.Equal(t, got.Type, persistence.ProviderOpenAISubscription)
		check.Equal(t, got.AuthType, llmhub.AuthOAuth)
		check.Equal(t, got.OAuthAccessToken, "test-access-token-not-real")
		check.Equal(t, got.RefreshToken, "test-refresh-token-not-real")
		check.Equal(t, got.ChatgptAccountID, "acct-not-real")
		if got.TokenExpiresAt == nil {
			t.Fatalf("tokenExpiresAt was lost")
		}
		check.Equal(t, *got.TokenExpiresAt, expires)
	})

	t.Run("load() returns null for an unknown name", func(t *testing.T) {
		store := newStore(t)
		check.NoErr(t, kernel.Initialize(t.Context(), store))

		got, err := store.Load(t.Context(), "missing-"+kernel.NewID())
		check.NoErr(t, err)
		check.True(t, got == nil, "an unknown name must give a nil record")
	})

	t.Run("list() contains every saved provider", func(t *testing.T) {
		store := newStore(t)
		check.NoErr(t, kernel.Initialize(t.Context(), store))
		p := runPrefix()
		a, b := providerRecordFor("prov-"+p+"-a"), providerRecordFor("prov-"+p+"-b")

		saveProvider(t, store, a)
		saveProvider(t, store, b)

		names := providerNames(t, store)
		check.True(t, slices.Contains(names, a.Name), "%s is missing from the list", a.Name)
		check.True(t, slices.Contains(names, b.Name), "%s is missing from the list", b.Name)
	})

	t.Run("save() overwrites an existing provider with the same name", func(t *testing.T) {
		store := newStore(t)
		check.NoErr(t, kernel.Initialize(t.Context(), store))
		p := providerRecordFor("prov-" + runPrefix())
		saveProvider(t, store, p)

		changed := p
		changed.APIKey = "test-other-key-not-real"
		changed.BaseURL = "https://other.example.invalid/v2"
		changed.Models = providerModels(p.Name)[1:]
		saveProvider(t, store, changed)

		got := loadProvider(t, store, p.Name)
		got.AuthType = changed.AuthType
		check.True(t, sameProviderRecord(got, changed), "loaded %+v, want %+v", got, changed)
		same := 0
		for _, name := range providerNames(t, store) {
			if name == p.Name {
				same++
			}
		}
		check.Equal(t, same, 1, "records named %s in the list", p.Name)
	})

	t.Run("delete() removes the provider and ignores unknown names", func(t *testing.T) {
		store := newStore(t)
		check.NoErr(t, kernel.Initialize(t.Context(), store))
		p := runPrefix()
		gone, kept := providerRecordFor("prov-"+p+"-gone"), providerRecordFor("prov-"+p+"-kept")
		saveProvider(t, store, gone)
		saveProvider(t, store, kept)

		check.NoErr(t, store.Delete(t.Context(), gone.Name))

		got, err := store.Load(t.Context(), gone.Name)
		check.NoErr(t, err)
		check.True(t, got == nil, "a deleted provider must be absent")
		check.False(t, slices.Contains(providerNames(t, store), gone.Name), "a deleted provider is still listed")
		loadProvider(t, store, kept.Name)
		check.NoErr(t, store.Delete(t.Context(), "missing-"+kernel.NewID()))
	})

	// -- Go-only guarantees --

	t.Run("mutating a returned or given record does not change the store (Go-only)", func(t *testing.T) {
		store := newStore(t)
		check.NoErr(t, kernel.Initialize(t.Context(), store))
		name := "prov-" + runPrefix()
		// build makes an independent record on every call: the pointers are not
		// shared between the expected value and the one given to the store.
		build := func() persistence.ProviderRecord {
			expires := int64(1893456000000)
			p := providerRecordFor(name)
			p.TokenExpiresAt = &expires
			return p
		}
		want := build()

		given := build()
		saveProvider(t, store, given)
		given.Models[0].ID = "changed after save"
		given.Models[0].InputModalities[0] = "changed after save"
		*given.Models[0].ContextWindowTokens = 1
		given.Models[0].Capabilities.Vision = nil
		*given.Models[0].Pricing.InputUSDPerMillionTokens = 99
		*given.TokenExpiresAt = 1
		given.Models = given.Models[:1]

		verify := func(label string) {
			t.Helper()
			got := loadProvider(t, store, name)
			got.AuthType = want.AuthType
			check.True(t, sameProviderRecord(got, want), "%s: loaded %+v, want %+v", label, got, want)
		}
		verify("after mutating the record given to Save")

		got := loadProvider(t, store, name)
		got.Models[0].ID = "changed after load"
		got.Models[0].InputModalities[0] = "changed after load"
		if got.Models[0].ContextWindowTokens != nil {
			*got.Models[0].ContextWindowTokens = 1
		}
		if got.Models[0].Capabilities != nil && got.Models[0].Capabilities.ToolCalling != nil {
			*got.Models[0].Capabilities.ToolCalling = false
		}
		if got.Models[0].Pricing != nil && got.Models[0].Pricing.OutputUSDPerMillionTokens != nil {
			*got.Models[0].Pricing.OutputUSDPerMillionTokens = 99
		}
		if got.TokenExpiresAt != nil {
			*got.TokenExpiresAt = 1
		}
		got.Models = got.Models[:1]
		verify("after mutating the record returned by Load")

		list, err := store.List(t.Context())
		check.NoErr(t, err)
		for _, p := range list {
			if p.Name != name || len(p.Models) == 0 {
				continue
			}
			p.Models[0].ID = "changed after list"
			if p.Models[0].ContextWindowTokens != nil {
				*p.Models[0].ContextWindowTokens = 1
			}
			if p.TokenExpiresAt != nil {
				*p.TokenExpiresAt = 1
			}
		}
		verify("after mutating the records returned by List")
	})

	t.Run("concurrent Save of distinct providers keeps all of them (Go-only)", func(t *testing.T) {
		const writers = 16
		store := newStore(t)
		check.NoErr(t, kernel.Initialize(t.Context(), store))
		p := runPrefix()

		names := make([]string, writers)
		var wg sync.WaitGroup
		for w := range writers {
			names[w] = fmt.Sprintf("prov-%s-conc%d", p, w)
			wg.Go(func() {
				if err := store.Save(t.Context(), providerRecordFor(names[w])); err != nil {
					t.Errorf("save from writer %d: %v", w, err)
				}
			})
		}
		wg.Wait()

		listed := providerNames(t, store)
		for _, name := range names {
			check.True(t, slices.Contains(listed, name), "%s is missing from the list", name)
			loadProvider(t, store, name)
		}
	})
}
