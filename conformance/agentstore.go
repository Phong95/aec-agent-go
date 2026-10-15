package conformance

import (
	"fmt"
	"slices"
	"sync"
	"testing"

	"github.com/Phong95/aec-agent-go/components/agent/profile"
	"github.com/Phong95/aec-agent-go/components/persistence"
	"github.com/Phong95/aec-agent-go/conformance/internal/check"
	"github.com/Phong95/aec-agent-go/kernel"
)

// AgentStoreFactory returns a persistence.AgentStore. See the package comment for
// the freshness rules of the store suites.
type AgentStoreFactory func(t *testing.T) persistence.AgentStore

// agentProfileFor builds a profile with the fields every agent store keeps. The
// compactor and memory configuration are left at their zero values: a Postgres
// store fills missing configuration with defaults, so no suite compares them.
func agentProfileFor(id string) profile.AgentProfile {
	return profile.AgentProfile{
		ID: id, Name: "Agent " + id, SystemPrompt: "You are " + id + ".",
		Provider: "anthropic", LLMModel: "model-" + id,
		Tools:          []string{"read_file", "write_file"},
		Skills:         []string{"summarise"},
		InitialContext: &profile.InitialContext{UserMd: "user " + id, SoulMd: "soul " + id},
	}
}

// sameAgentProfile compares the fields every agent store round-trips. Nil and
// empty lists are the same value.
func sameAgentProfile(a, b profile.AgentProfile) bool {
	same := a.ID == b.ID && a.Name == b.Name && a.SystemPrompt == b.SystemPrompt &&
		a.Provider == b.Provider && a.LLMModel == b.LLMModel &&
		slices.Equal(a.Tools, b.Tools) && slices.Equal(a.Skills, b.Skills)
	if !same {
		return false
	}
	if (a.InitialContext == nil) != (b.InitialContext == nil) {
		return false
	}
	return a.InitialContext == nil || *a.InitialContext == *b.InitialContext
}

func saveAgent(t *testing.T, s persistence.AgentStore, p profile.AgentProfile) {
	t.Helper()
	check.NoErr(t, s.Save(t.Context(), p), "save %s", p.ID)
}

// loadAgent returns the profile and fails the test when it is absent.
func loadAgent(t *testing.T, s persistence.AgentStore, id string) profile.AgentProfile {
	t.Helper()
	got, err := s.Load(t.Context(), id)
	check.NoErr(t, err, "load %s", id)
	if got == nil {
		t.Fatalf("agent %s is absent, want it stored", id)
	}
	return *got
}

// agentIDs lists the ids of every stored profile.
func agentIDs(t *testing.T, s persistence.AgentStore) []string {
	t.Helper()
	list, err := s.List(t.Context())
	check.NoErr(t, err, "list")
	ids := make([]string, 0, len(list))
	for _, p := range list {
		ids = append(ids, p.ID)
	}
	return ids
}

// AgentStore runs the conformance suite for persistence.AgentStore
// implementations.
//
// It is written for server/go's Postgres stores: save is an upsert by id, load of an
// unknown id is nil, list contains every saved profile, delete removes one
// profile. Every profile id carries a per-run prefix; the suite compares the
// fields every store keeps (identity, prompt, provider, model, tools, skills and
// the initial context) and never the compactor or memory configuration, which a
// Postgres store defaults, nor list order. The Go-only subtests prove that
// profiles are copied in and out and that concurrent saves keep every profile.
func AgentStore(t *testing.T, newStore AgentStoreFactory) {
	t.Helper()

	t.Run("save() then load() round-trips the profile", func(t *testing.T) {
		store := newStore(t)
		check.NoErr(t, kernel.Initialize(t.Context(), store))
		p := agentProfileFor("agent-" + runPrefix())

		saveAgent(t, store, p)

		got := loadAgent(t, store, p.ID)
		check.True(t, sameAgentProfile(got, p), "loaded %+v, want %+v", got, p)
	})

	t.Run("load() returns null for an unknown id", func(t *testing.T) {
		store := newStore(t)
		check.NoErr(t, kernel.Initialize(t.Context(), store))

		got, err := store.Load(t.Context(), "missing-"+kernel.NewID())
		check.NoErr(t, err)
		check.True(t, got == nil, "an unknown id must give a nil profile")
	})

	t.Run("list() contains every saved profile", func(t *testing.T) {
		store := newStore(t)
		check.NoErr(t, kernel.Initialize(t.Context(), store))
		p := runPrefix()
		a, b := agentProfileFor("agent-"+p+"-a"), agentProfileFor("agent-"+p+"-b")

		saveAgent(t, store, a)
		saveAgent(t, store, b)

		ids := agentIDs(t, store)
		check.True(t, slices.Contains(ids, a.ID), "%s is missing from the list", a.ID)
		check.True(t, slices.Contains(ids, b.ID), "%s is missing from the list", b.ID)
	})

	t.Run("save() overwrites an existing profile with the same id", func(t *testing.T) {
		store := newStore(t)
		check.NoErr(t, kernel.Initialize(t.Context(), store))
		p := agentProfileFor("agent-" + runPrefix())
		saveAgent(t, store, p)

		changed := p
		changed.Name = "Renamed"
		changed.SystemPrompt = "A different prompt."
		changed.Tools = []string{"only_tool"}
		changed.Skills = []string{}
		changed.InitialContext = nil
		saveAgent(t, store, changed)

		got := loadAgent(t, store, p.ID)
		check.True(t, sameAgentProfile(got, changed), "loaded %+v, want %+v", got, changed)
		same := 0
		for _, id := range agentIDs(t, store) {
			if id == p.ID {
				same++
			}
		}
		check.Equal(t, same, 1, "profiles with id %s in the list", p.ID)
	})

	t.Run("delete() removes the profile and ignores unknown ids", func(t *testing.T) {
		store := newStore(t)
		check.NoErr(t, kernel.Initialize(t.Context(), store))
		p := runPrefix()
		gone, kept := agentProfileFor("agent-"+p+"-gone"), agentProfileFor("agent-"+p+"-kept")
		saveAgent(t, store, gone)
		saveAgent(t, store, kept)

		check.NoErr(t, store.Delete(t.Context(), gone.ID))

		got, err := store.Load(t.Context(), gone.ID)
		check.NoErr(t, err)
		check.True(t, got == nil, "a deleted profile must be absent")
		check.False(t, slices.Contains(agentIDs(t, store), gone.ID), "a deleted profile is still listed")
		loadAgent(t, store, kept.ID)
		check.NoErr(t, store.Delete(t.Context(), "missing-"+kernel.NewID()))
	})

	// -- Go-only guarantees --

	t.Run("mutating a returned or given profile does not change the store (Go-only)", func(t *testing.T) {
		store := newStore(t)
		check.NoErr(t, kernel.Initialize(t.Context(), store))
		id := "agent-" + runPrefix()
		// withExtras builds an independent profile on every call: the pointers are
		// not shared between the expected value and the one given to the store.
		withExtras := func() profile.AgentProfile {
			maxTokens, maxIterations := 100, 5
			p := agentProfileFor(id)
			p.InferenceConfig = &profile.InferenceConfig{MaxTokens: &maxTokens, StopSequences: []string{"END"}}
			p.LoopConfig = &profile.LoopConfig{MaxIterations: &maxIterations}
			return p
		}
		want := withExtras()

		given := withExtras()
		saveAgent(t, store, given)
		given.Tools[0] = "changed after save"
		given.Skills[0] = "changed after save"
		given.InitialContext.UserMd = "changed after save"
		given.InferenceConfig.StopSequences[0] = "changed after save"
		*given.InferenceConfig.MaxTokens = 1

		verify := func(label string) {
			t.Helper()
			got := loadAgent(t, store, id)
			check.True(t, sameAgentProfile(got, want), "%s: loaded %+v, want %+v", label, got, want)
			if got.InferenceConfig == nil || got.InferenceConfig.MaxTokens == nil || got.LoopConfig == nil || got.LoopConfig.MaxIterations == nil {
				t.Fatalf("%s: the stored inference or loop configuration is missing: %+v", label, got)
			}
			check.Equal(t, *got.InferenceConfig.MaxTokens, 100, "%s: maxTokens", label)
			check.True(t, slices.Equal(got.InferenceConfig.StopSequences, []string{"END"}), "%s: stopSequences %v", label, got.InferenceConfig.StopSequences)
			check.Equal(t, *got.LoopConfig.MaxIterations, 5, "%s: maxIterations", label)
		}
		verify("after mutating the profile given to Save")

		got := loadAgent(t, store, id)
		got.Tools[0] = "changed after load"
		got.Skills = nil
		got.InitialContext.UserMd = "changed after load"
		got.InferenceConfig.StopSequences[0] = "changed after load"
		*got.InferenceConfig.MaxTokens = 1
		*got.LoopConfig.MaxIterations = 1
		verify("after mutating the profile returned by Load")

		list, err := store.List(t.Context())
		check.NoErr(t, err)
		for _, p := range list {
			if p.ID != id {
				continue
			}
			p.Tools[0] = "changed after list"
			p.InitialContext.SoulMd = "changed after list"
			p.InferenceConfig.StopSequences[0] = "changed after list"
			*p.InferenceConfig.MaxTokens = 1
		}
		verify("after mutating the profiles returned by List")
	})

	t.Run("concurrent Save of distinct profiles keeps all of them (Go-only)", func(t *testing.T) {
		const writers = 16
		store := newStore(t)
		check.NoErr(t, kernel.Initialize(t.Context(), store))
		p := runPrefix()

		ids := make([]string, writers)
		var wg sync.WaitGroup
		for w := range writers {
			ids[w] = fmt.Sprintf("agent-%s-conc%d", p, w)
			wg.Go(func() {
				if err := store.Save(t.Context(), agentProfileFor(ids[w])); err != nil {
					t.Errorf("save from writer %d: %v", w, err)
				}
			})
		}
		wg.Wait()

		listed := agentIDs(t, store)
		for _, id := range ids {
			check.True(t, slices.Contains(listed, id), "%s is missing from the list", id)
			loadAgent(t, store, id)
		}
	})
}
