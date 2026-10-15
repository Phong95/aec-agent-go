package persistence_test

import (
	"slices"
	"testing"

	"github.com/Phong95/aec-agent-go/components/agent/memorysystem/memory"
	"github.com/Phong95/aec-agent-go/components/agent/profile"
	"github.com/Phong95/aec-agent-go/components/artifacthub/artifact"
	"github.com/Phong95/aec-agent-go/components/llmhub"
	"github.com/Phong95/aec-agent-go/components/persistence"
	"github.com/Phong95/aec-agent-go/components/sandboxhub"
	"github.com/Phong95/aec-agent-go/components/skillhub/skill"
)

// memRec builds a record of agent a with the given id; the observedAt values the
// tests choose sit on the since/before cut.
func memRec(id, agent string, kind memory.Kind, scope memory.Scope, observed string) memory.Record {
	return memory.Record{
		ID: id, AgentID: agent, Kind: kind, Scope: scope, Subject: "subj-" + id, Key: "key-" + id, Value: "v",
		Status: memory.StatusActive, Confidence: memory.ConfidenceVerified, Source: "chat",
		SourceTrust: memory.TrustVerifiedInternal, ObservedAt: observed,
	}
}

func seedMemories(t *testing.T) *persistence.MemoryMemoryStore {
	t.Helper()
	ctx := t.Context()
	store := persistence.NewMemoryMemoryStore()
	recs := []memory.Record{
		memRec("m1", "a1", memory.KindFact, memory.ScopeProject, "2026-01-01T00:00:00.000Z"),
		memRec("m2", "a1", memory.KindDecision, memory.ScopeUser, "2026-01-02T00:00:00.000Z"),
		memRec("m3", "a2", memory.KindFact, memory.ScopeProject, "2026-01-03T00:00:00.000Z"),
		memRec("m4", "a1", memory.KindProcedure, memory.ScopeTask, "2026-01-04T00:00:00.000Z"),
	}
	recs[0].Tags = []string{"x", "y"}
	recs[1].Tags = []string{"y"}
	recs[3].Tags = []string{"z"}
	recs[1].Status = memory.StatusSuperseded
	for _, r := range recs {
		if err := store.Save(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	return store
}

func memIDs(rs []memory.Record) []string {
	out := []string{}
	for _, r := range rs {
		out = append(out, r.ID)
	}
	return out
}

// Each filter alone selects exactly the expected records, in insertion order; an empty
// string or list means no filter, a limit of 0 means no limit, a positive limit
// keeps the first n and a negative one drops that many from the end (JS slice).
func TestMemoryQueryFilters(t *testing.T) {
	ctx := t.Context()
	store := seedMemories(t)
	cases := []struct {
		name string
		f    persistence.MemoryFilter
		want []string
	}{
		{"none", persistence.MemoryFilter{}, []string{"m1", "m2", "m3", "m4"}},
		{"agentId", persistence.MemoryFilter{AgentID: "a1"}, []string{"m1", "m2", "m4"}},
		{"kinds", persistence.MemoryFilter{Kinds: []memory.Kind{memory.KindDecision, memory.KindProcedure}}, []string{"m2", "m4"}},
		{"scopes", persistence.MemoryFilter{Scopes: []memory.Scope{memory.ScopeProject}}, []string{"m1", "m3"}},
		{"statuses", persistence.MemoryFilter{Statuses: []memory.Status{memory.StatusSuperseded}}, []string{"m2"}},
		{"subject", persistence.MemoryFilter{Subject: "subj-m3"}, []string{"m3"}},
		{"key", persistence.MemoryFilter{Key: "key-m4"}, []string{"m4"}},
		{"tags any-of", persistence.MemoryFilter{Tags: []string{"y", "z"}}, []string{"m1", "m2", "m4"}},
		{"tags none match", persistence.MemoryFilter{Tags: []string{"nope"}}, []string{}},
		{"since inclusive", persistence.MemoryFilter{Since: "2026-01-03T00:00:00.000Z"}, []string{"m3", "m4"}},
		{"before exclusive", persistence.MemoryFilter{Before: "2026-01-03T00:00:00.000Z"}, []string{"m1", "m2"}},
		{"since and before", persistence.MemoryFilter{Since: "2026-01-02T00:00:00.000Z", Before: "2026-01-04T00:00:00.000Z"}, []string{"m2", "m3"}},
		{"combined", persistence.MemoryFilter{AgentID: "a1", Scopes: []memory.Scope{memory.ScopeProject}, Tags: []string{"x"}}, []string{"m1"}},
		{"limit 2 keeps the first two", persistence.MemoryFilter{Limit: 2}, []string{"m1", "m2"}},
		{"limit above the count", persistence.MemoryFilter{Limit: 99}, []string{"m1", "m2", "m3", "m4"}},
		{"negative limit drops from the end", persistence.MemoryFilter{Limit: -1}, []string{"m1", "m2", "m3"}},
		{"negative limit below zero length", persistence.MemoryFilter{Limit: -99}, []string{}},
		{"empty values are no filter", persistence.MemoryFilter{Kinds: []memory.Kind{}, Scopes: []memory.Scope{}, Statuses: []memory.Status{}, Tags: []string{}}, []string{"m1", "m2", "m3", "m4"}},
		{"limit applies after the filters", persistence.MemoryFilter{AgentID: "a1", Limit: 2}, []string{"m1", "m2"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := store.Query(ctx, c.f)
			if err != nil {
				t.Fatal(err)
			}
			if got == nil {
				t.Fatal("Query returned a nil slice, want a non-nil one")
			}
			if !slices.Equal(memIDs(got), c.want) {
				t.Fatalf("Query(%+v) = %v, want %v", c.f, memIDs(got), c.want)
			}
		})
	}
}

// FindByKey returns the first ACTIVE record with the same agent, scope and key,
// ignores superseded ones and other agents, and returns nil with a nil error when
// nothing matches.
func TestFindByKey(t *testing.T) {
	ctx := t.Context()
	store := persistence.NewMemoryMemoryStore()
	dup := func(id, agent string, status memory.Status) memory.Record {
		r := memRec(id, agent, memory.KindFact, memory.ScopeProject, "2026-01-01T00:00:00.000Z")
		r.Key = "grid"
		r.Status = status
		return r
	}
	for _, r := range []memory.Record{
		dup("old", "a1", memory.StatusSuperseded),
		dup("other", "a2", memory.StatusActive),
		dup("first", "a1", memory.StatusActive),
		dup("second", "a1", memory.StatusActive),
	} {
		if err := store.Save(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	got, err := store.FindByKey(ctx, "a1", memory.ScopeProject, "grid")
	if err != nil || got == nil || got.ID != "first" {
		t.Fatalf("FindByKey = %+v, %v; want the first active record of a1", got, err)
	}
	for _, miss := range []struct {
		agent string
		scope memory.Scope
		key   string
	}{
		{"a3", memory.ScopeProject, "grid"},
		{"a1", memory.ScopeUser, "grid"},
		{"a1", memory.ScopeProject, "nope"},
	} {
		got, err := store.FindByKey(ctx, miss.agent, miss.scope, miss.key)
		if err != nil || got != nil {
			t.Fatalf("FindByKey(%v) = %+v, %v; want nil, nil", miss, got, err)
		}
	}
	if err := store.Delete(ctx, "first"); err != nil {
		t.Fatal(err)
	}
	if err := store.Delete(ctx, "second"); err != nil {
		t.Fatal(err)
	}
	if got, err := store.FindByKey(ctx, "a1", memory.ScopeProject, "grid"); err != nil || got != nil {
		t.Fatalf("only a superseded record is left: FindByKey = %+v, %v; want nil, nil", got, err)
	}
	if got, err := store.Get(ctx, "nope"); err != nil || got != nil {
		t.Fatalf("Get of an unknown id = %+v, %v; want nil, nil", got, err)
	}
}

func agentProfile(id string) profile.AgentProfile {
	return profile.AgentProfile{ID: id, Name: id, SystemPrompt: "p", Tools: []string{"t1"}, Skills: []string{"s1"}}
}

// Save of an existing id keeps its list position, Delete then Save appends, and
// Load of an unknown id returns nil with a nil error.
func TestAgentStoreOrdering(t *testing.T) {
	ctx := t.Context()
	store := persistence.NewMemoryAgentStore()
	for _, id := range []string{"a", "b", "c"} {
		if err := store.Save(ctx, agentProfile(id)); err != nil {
			t.Fatal(err)
		}
	}
	renamed := agentProfile("a")
	renamed.Name = "renamed"
	if err := store.Save(ctx, renamed); err != nil {
		t.Fatal(err)
	}
	order := func() []string {
		list, err := store.List(ctx)
		if err != nil {
			t.Fatal(err)
		}
		out := []string{}
		for _, p := range list {
			out = append(out, p.ID)
		}
		return out
	}
	if got := order(); !slices.Equal(got, []string{"a", "b", "c"}) {
		t.Fatalf("a re-save moved the profile: %v", got)
	}
	if got, _ := store.Load(ctx, "a"); got == nil || got.Name != "renamed" {
		t.Fatalf("re-save did not replace the profile: %+v", got)
	}
	if err := store.Delete(ctx, "a"); err != nil {
		t.Fatal(err)
	}
	if err := store.Save(ctx, agentProfile("a")); err != nil {
		t.Fatal(err)
	}
	if got := order(); !slices.Equal(got, []string{"b", "c", "a"}) {
		t.Fatalf("delete then save must append: %v", got)
	}
	if got, err := store.Load(ctx, "nope"); err != nil || got != nil {
		t.Fatalf("Load of an unknown id = %+v, %v; want nil, nil", got, err)
	}
}

// Mutating a returned record or profile, or a value passed into the store, never
// changes what the store holds.
func TestMemoryAndAgentStoreCopies(t *testing.T) {
	ctx := t.Context()

	mem := persistence.NewMemoryMemoryStore()
	in := memRec("m1", "a1", memory.KindFact, memory.ScopeProject, "2026-01-01T00:00:00.000Z")
	in.Tags = []string{"x", "y"}
	if err := mem.Save(ctx, in); err != nil {
		t.Fatal(err)
	}
	in.Tags[0] = "mutated-after-save"
	got, _ := mem.Get(ctx, "m1")
	got.Tags[1] = "mutated-after-get"
	queried, _ := mem.Query(ctx, persistence.MemoryFilter{})
	queried[0].Tags[0] = "mutated-after-query"
	found, _ := mem.FindByKey(ctx, "a1", memory.ScopeProject, "key-m1")
	found.Tags = append(found.Tags[:0], "mutated-after-find")
	again, _ := mem.Get(ctx, "m1")
	if !slices.Equal(again.Tags, []string{"x", "y"}) {
		t.Fatalf("memory store aliases its callers: %v", again.Tags)
	}

	agents := persistence.NewMemoryAgentStore()
	lean, maxTokens := 7, 100
	p := agentProfile("agent")
	p.Compactor.LeanTailTokenBudget = &lean
	p.InferenceConfig = &profile.InferenceConfig{MaxTokens: &maxTokens, StopSequences: []string{"END"}}
	p.Sandbox = sandboxhub.Selection{Mode: sandboxhub.SelectionOwn, Own: &sandboxhub.OwnConfig{Image: "node:22", Limits: &sandboxhub.LimitsOverride{Pids: &maxTokens}}}
	if err := agents.Save(ctx, p); err != nil {
		t.Fatal(err)
	}
	p.Tools[0] = "mutated-after-save"
	lean = 999
	loaded, _ := agents.Load(ctx, "agent")
	loaded.Tools[0] = "mutated-after-load"
	loaded.Skills = nil
	loaded.InferenceConfig.StopSequences[0] = "mutated-after-load"
	*loaded.InferenceConfig.MaxTokens = 1
	loaded.Sandbox.Own.Image = "mutated"
	*loaded.Sandbox.Own.Limits.Pids = 1
	list, _ := agents.List(ctx)
	list[0].Tools[0] = "mutated-after-list"
	*list[0].Compactor.LeanTailTokenBudget = 1

	again2, _ := agents.Load(ctx, "agent")
	switch {
	case again2.Tools[0] != "t1", len(again2.Skills) != 1:
		t.Fatalf("agent store aliases its callers: %+v", again2)
	case again2.InferenceConfig.StopSequences[0] != "END", *again2.InferenceConfig.MaxTokens != 100:
		t.Fatalf("inference config aliased: %+v", again2.InferenceConfig)
	case again2.Sandbox.Own.Image != "node:22", *again2.Sandbox.Own.Limits.Pids != 100:
		t.Fatalf("sandbox selection aliased: %+v", again2.Sandbox.Own)
	case *again2.Compactor.LeanTailTokenBudget != 7:
		t.Fatalf("compactor pointer aliased: %d", *again2.Compactor.LeanTailTokenBudget)
	}
}

// artifactRec builds a head record; updated sits on the sort and since/before cut
// the tests choose.
func artifactRec(id, agent, name, updated string) artifact.Artifact {
	return artifact.Artifact{
		ID: id, AgentID: agent, Name: name, Type: "text/markdown", Version: 1,
		SandboxPath: "artifacts/" + name, SandboxID: "docker:shared", Bytes: 1, SHA256: "s",
		CreatedAt: updated, UpdatedAt: updated,
	}
}

func artifactIDs(as []artifact.Artifact) []string {
	out := []string{}
	for _, a := range as {
		out = append(out, a.ID)
	}
	return out
}

func versionOf(a artifact.Artifact, n int) artifact.Version {
	return artifact.Version{
		ArtifactID: a.ID, Version: n, Bytes: n, SHA256: "s", CreatedAt: a.UpdatedAt,
		Origin: artifact.Origin{AgentID: a.AgentID, SandboxPath: a.SandboxPath, SandboxID: a.SandboxID},
	}
}

// ListVersions is newest first; Create replaces any earlier version log of that
// id; AddVersion of an existing number overwrites it; Query is newest updated
// first with a STABLE sort (a tie keeps insertion order) before the limit;
// FindByName returns the first insertion-order match of that agent.
func TestArtifactStoreOrdering(t *testing.T) {
	ctx := t.Context()
	store := persistence.NewMemoryArtifactStore()
	a1 := artifactRec("a1", "ag", "one", "2026-03-01T00:00:00.000Z")
	a2 := artifactRec("a2", "ag", "two", "2026-03-02T00:00:00.000Z")
	a3 := artifactRec("a3", "ag", "three", "2026-03-02T00:00:00.000Z") // ties with a2
	a4 := artifactRec("a4", "other", "one", "2026-03-04T00:00:00.000Z")
	for _, a := range []artifact.Artifact{a1, a2, a3, a4} {
		if err := store.Create(ctx, a, versionOf(a, 1)); err != nil {
			t.Fatal(err)
		}
	}

	got, err := store.Query(ctx, persistence.ArtifactFilter{AgentID: "ag"})
	if err != nil || !slices.Equal(artifactIDs(got), []string{"a2", "a3", "a1"}) {
		t.Fatalf("Query order = %v, %v; want newest first with the tie in insertion order", artifactIDs(got), err)
	}
	got, _ = store.Query(ctx, persistence.ArtifactFilter{Limit: 2})
	if !slices.Equal(artifactIDs(got), []string{"a4", "a2"}) {
		t.Fatalf("limit applies after the sort: %v", artifactIDs(got))
	}
	got, _ = store.Query(ctx, persistence.ArtifactFilter{Since: "2026-03-02T00:00:00.000Z", Before: "2026-03-04T00:00:00.000Z"})
	if !slices.Equal(artifactIDs(got), []string{"a2", "a3"}) {
		t.Fatalf("since is inclusive and before exclusive: %v", artifactIDs(got))
	}
	got, _ = store.Query(ctx, persistence.ArtifactFilter{Limit: -1})
	if !slices.Equal(artifactIDs(got), []string{"a4", "a2", "a3"}) {
		t.Fatalf("a negative limit drops from the end: %v", artifactIDs(got))
	}
	got, _ = store.Query(ctx, persistence.ArtifactFilter{Types: []string{"nope"}})
	if got == nil || len(got) != 0 {
		t.Fatalf("no match must be an empty non-nil slice: %#v", got)
	}

	for _, n := range []int{3, 2} {
		if err := store.AddVersion(ctx, a1, versionOf(a1, n)); err != nil {
			t.Fatal(err)
		}
	}
	overwrite := versionOf(a1, 2)
	overwrite.Note = "overwritten"
	if err := store.AddVersion(ctx, a1, overwrite); err != nil {
		t.Fatal(err)
	}
	vs, err := store.ListVersions(ctx, "a1")
	if err != nil || len(vs) != 3 || vs[0].Version != 3 || vs[1].Version != 2 || vs[2].Version != 1 || vs[1].Note != "overwritten" {
		t.Fatalf("ListVersions = %+v, %v; want 3,2(overwritten),1", vs, err)
	}

	if err := store.Create(ctx, a1, versionOf(a1, 1)); err != nil { // Create replaces the log
		t.Fatal(err)
	}
	if vs, _ = store.ListVersions(ctx, "a1"); len(vs) != 1 || vs[0].Version != 1 {
		t.Fatalf("Create must replace the version log: %+v", vs)
	}
	if vs, err = store.ListVersions(ctx, "nope"); err != nil || vs == nil || len(vs) != 0 {
		t.Fatalf("an unknown artifact lists []: %#v, %v", vs, err)
	}

	byName, err := store.FindByName(ctx, "ag", "one")
	if err != nil || byName == nil || byName.ID != "a1" {
		t.Fatalf("FindByName = %+v, %v; want a1 of agent ag", byName, err)
	}
	if byName, err = store.FindByName(ctx, "ag", "nope"); err != nil || byName != nil {
		t.Fatalf("FindByName miss = %+v, %v; want nil, nil", byName, err)
	}
	if head, err := store.Get(ctx, "nope"); err != nil || head != nil {
		t.Fatalf("Get miss = %+v, %v; want nil, nil", head, err)
	}
}

// Delete removes one artifact and its versions; DeleteByAgent removes only that
// agent's artifacts and their versions.
func TestArtifactStoreDeletion(t *testing.T) {
	ctx := t.Context()
	store := persistence.NewMemoryArtifactStore()
	a1 := artifactRec("a1", "ag", "one", "2026-03-01T00:00:00.000Z")
	a2 := artifactRec("a2", "ag", "two", "2026-03-02T00:00:00.000Z")
	a3 := artifactRec("a3", "other", "three", "2026-03-03T00:00:00.000Z")
	for _, a := range []artifact.Artifact{a1, a2, a3} {
		if err := store.Create(ctx, a, versionOf(a, 1)); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.Delete(ctx, "a1"); err != nil {
		t.Fatal(err)
	}
	if head, _ := store.Get(ctx, "a1"); head != nil {
		t.Fatalf("Delete left the head record: %+v", head)
	}
	if vs, _ := store.ListVersions(ctx, "a1"); len(vs) != 0 {
		t.Fatalf("Delete left versions: %+v", vs)
	}
	if err := store.DeleteByAgent(ctx, "ag"); err != nil {
		t.Fatal(err)
	}
	rest, _ := store.Query(ctx, persistence.ArtifactFilter{})
	if !slices.Equal(artifactIDs(rest), []string{"a3"}) {
		t.Fatalf("DeleteByAgent removed the wrong artifacts: %v", artifactIDs(rest))
	}
	if vs, _ := store.ListVersions(ctx, "a2"); len(vs) != 0 {
		t.Fatalf("DeleteByAgent left versions of a2: %+v", vs)
	}
	if vs, _ := store.ListVersions(ctx, "a3"); len(vs) != 1 {
		t.Fatalf("DeleteByAgent removed the versions of another agent: %+v", vs)
	}
	if err := store.Delete(ctx, "nope"); err != nil {
		t.Fatalf("Delete of an unknown id must be a no-op: %v", err)
	}
}

// Mutating a returned artifact, version, skill record or provider record, or a
// value passed into the store, never changes what the store holds.
func TestArtifactSkillProviderStoreCopies(t *testing.T) {
	ctx := t.Context()

	arts := persistence.NewMemoryArtifactStore()
	a := artifactRec("a1", "ag", "one", "2026-03-01T00:00:00.000Z")
	a.Tags = []string{"x", "y"}
	if err := arts.Create(ctx, a, versionOf(a, 1)); err != nil {
		t.Fatal(err)
	}
	a.Tags[0] = "mutated-after-create"
	got, _ := arts.Get(ctx, "a1")
	got.Tags[1] = "mutated-after-get"
	queried, _ := arts.Query(ctx, persistence.ArtifactFilter{})
	queried[0].Tags[0] = "mutated-after-query"
	byName, _ := arts.FindByName(ctx, "ag", "one")
	byName.Tags[0] = "mutated-after-find"
	vs, _ := arts.ListVersions(ctx, "a1")
	vs[0].Origin.AgentID = "mutated"
	vs[0].Note = "mutated"
	again, _ := arts.Get(ctx, "a1")
	againV, _ := arts.ListVersions(ctx, "a1")
	if !slices.Equal(again.Tags, []string{"x", "y"}) || againV[0].Origin.AgentID != "ag" || againV[0].Note != "" {
		t.Fatalf("artifact store aliases its callers: %+v %+v", again, againV)
	}

	skills := persistence.NewMemorySkillStore()
	rec := persistence.SkillRecord{Name: "s", Content: "c", Files: []skill.File{{Path: "p", Content: "x"}}, Version: 1}
	if err := skills.Save(ctx, rec); err != nil {
		t.Fatal(err)
	}
	rec.Files[0].Content = "mutated-after-save"
	loaded, _ := skills.Load(ctx, "s")
	loaded.Files[0].Path = "mutated-after-load"
	list, _ := skills.List(ctx)
	list[0].Files[0].Content = "mutated-after-list"
	list[0].Files = nil
	skillAgain, _ := skills.Load(ctx, "s")
	if len(skillAgain.Files) != 1 || skillAgain.Files[0].Path != "p" || skillAgain.Files[0].Content != "x" {
		t.Fatalf("skill store aliases its callers: %+v", skillAgain)
	}

	providers := persistence.NewMemoryProviderStore()
	window, expires := 1000, int64(5)
	pr := persistence.ProviderRecord{
		Name: "p", Type: persistence.ProviderAnthropic, APIKey: "test-api-key-not-real", TokenExpiresAt: &expires,
		Models: []llmhub.ModelRecord{{ID: "m", Provider: "p", ContextWindowTokens: &window, InputModalities: []string{"text"}}},
	}
	if err := providers.Save(ctx, pr); err != nil {
		t.Fatal(err)
	}
	*pr.Models[0].ContextWindowTokens = 1
	pr.Models[0].InputModalities[0] = "mutated-after-save"
	expires = 99
	pl, _ := providers.Load(ctx, "p")
	*pl.Models[0].ContextWindowTokens = 2
	pl.Models[0].InputModalities[0] = "mutated-after-load"
	*pl.TokenExpiresAt = 7
	pl.Models = nil
	plist, _ := providers.List(ctx)
	plist[0].Models[0].ID = "mutated-after-list"
	pAgain, _ := providers.Load(ctx, "p")
	if *pAgain.Models[0].ContextWindowTokens != 1000 || pAgain.Models[0].InputModalities[0] != "text" || pAgain.Models[0].ID != "m" || *pAgain.TokenExpiresAt != 5 {
		t.Fatalf("provider store aliases its callers: %+v", pAgain)
	}
}
