package persistence_test

import (
	"encoding/json"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/Phong95/aec-agent-go/components/agent/memorysystem/memory"
	"github.com/Phong95/aec-agent-go/components/agent/profile"
	"github.com/Phong95/aec-agent-go/components/artifacthub/artifact"
	"github.com/Phong95/aec-agent-go/components/persistence"
	"github.com/Phong95/aec-agent-go/internal/wiretest"
	"github.com/Phong95/aec-agent-go/kernel"
	"github.com/Phong95/aec-agent-go/kernel/ordered"
)

// storeSessionScenario is the recorded MemorySessionStore scenario
// of the store-session fixture, with the fields tagged in fixture order.
type storeSessionScenario struct {
	Created      kernel.Session   `json:"created"`
	Got          kernel.Session   `json:"got"`
	Other        kernel.Session   `json:"other"`
	ListAll      []kernel.Session `json:"listAll"`
	ListFiltered []kernel.Session `json:"listFiltered"`
	ListNone     []kernel.Session `json:"listNone"`
}

// storeFixtures lists every store-*.json fixture this package asserts. Plan
// 06-02 adds the other stores.
var storeFixtures = []string{"store-session.json", "store-message.json", "store-memory.json", "store-agent.json", "store-provider.json", "store-skill.json", "store-artifact.json"}

// A session record in the recorded fixture, with nested and non-sorted
// metadata keys, decodes strictly and re-encodes byte-identical.
func TestStoreSessionFixture(t *testing.T) {
	var v storeSessionScenario
	wiretest.AssertRoundTrip(t, "store-session.json", &v)
	if got := v.Created.Metadata.Keys(); !slices.Equal(got, []string{"zeta", "agentId", "nested", "flag", "count"}) {
		t.Fatalf("metadata key order lost: %v", got)
	}
	if v.Got.ConsolidatedMessageCount == nil || *v.Got.ConsolidatedMessageCount != 3 {
		t.Fatalf("monotonic checkpoint lost: %v", v.Got.ConsolidatedMessageCount)
	}
}

// The same scenario replayed through the Go store with the deterministic clock
// and ids reproduces every byte of the fixture, which proves Create reads the
// clock twice, the checkpoint is monotonic and List filters by strict equality.
func TestStoreSessionReplay(t *testing.T) {
	ctx := t.Context()
	var fixture struct {
		Created struct {
			Metadata json.RawMessage `json:"metadata"`
		} `json:"created"`
	}
	if err := json.Unmarshal(wiretest.Load(t, "store-session.json"), &fixture); err != nil {
		t.Fatal(err)
	}
	var meta ordered.Object
	if err := meta.UnmarshalJSON(fixture.Created.Metadata); err != nil { // the key order comes from the fixture
		t.Fatal(err)
	}

	store := persistence.NewMemorySessionStore(
		persistence.WithSessionClock(wiretest.Clock(1700000000000, 1)),
		persistence.WithSessionIDs(wiretest.IDs()),
	)
	created, err := store.Create(ctx, meta)
	if err != nil {
		t.Fatal(err)
	}
	upd := created.Clone()
	upd.LLMModel = "claude-sonnet"
	cacheRead, cacheWrite, three := 10, 20, 3
	upd.CurrentTokenUsage = &kernel.TokenUsage{InputTokens: 1000, OutputTokens: 200, CacheReadTokens: &cacheRead, CacheWriteTokens: &cacheWrite}
	upd.CounterCalibration = &kernel.CounterCalibration{CharsPerToken: 3.5, Samples: 12}
	upd.ConsolidatedMessageCount = &three
	if err := store.Update(ctx, upd); err != nil {
		t.Fatal(err)
	}
	if err := store.SetConsolidatedMessageCount(ctx, created.ID, 2); err != nil { // the max keeps 3
		t.Fatal(err)
	}
	got, err := store.Get(ctx, created.ID)
	if err != nil || got == nil {
		t.Fatalf("Get = %v, %v", got, err)
	}
	var otherMeta ordered.Object
	otherMeta.Set("agentId", "a2")
	other, err := store.Create(ctx, otherMeta)
	if err != nil {
		t.Fatal(err)
	}
	listAll, err := store.List(ctx, kernel.SessionFilter{})
	if err != nil {
		t.Fatal(err)
	}
	var f1 kernel.SessionFilter
	f1.Metadata.Set("agentId", "a1")
	f1.Metadata.Set("count", 0)
	listFiltered, err := store.List(ctx, f1)
	if err != nil {
		t.Fatal(err)
	}
	var f2 kernel.SessionFilter
	f2.Metadata.Set("agentId", "nobody")
	listNone, err := store.List(ctx, f2)
	if err != nil {
		t.Fatal(err)
	}

	out, err := kernel.EncodeJSON(storeSessionScenario{
		Created: created, Got: *got, Other: other,
		ListAll: listAll, ListFiltered: listFiltered, ListNone: listNone,
	})
	if err != nil {
		t.Fatal(err)
	}
	wiretest.AssertBytes(t, "store-session.json", out)
}

// storeMessageScenario is the recorded MemoryMessageStore scenario
// of the store-message fixture, with the fields tagged in fixture order.
type storeMessageScenario struct {
	Inputs        []kernel.Message         `json:"inputs"`
	Got           []kernel.Message         `json:"got"`
	Tail          persistence.MessageSlice `json:"tail"`
	Mid           persistence.MessageSlice `json:"mid"`
	Over          persistence.MessageSlice `json:"over"`
	Zero          persistence.MessageSlice `json:"zero"`
	Count         int                      `json:"count"`
	AfterSet      []kernel.Message         `json:"afterSet"`
	CountAfterSet int                      `json:"countAfterSet"`
	Unknown       []kernel.Message         `json:"unknown"`
	UnknownCount  int                      `json:"unknownCount"`
}

// Messages in the recorded fixture, with tool-call arguments in non-sorted
// key order and raw <, > and &, decode strictly and re-encode byte-identical.
func TestStoreMessageFixture(t *testing.T) {
	var v storeMessageScenario
	wiretest.AssertRoundTrip(t, "store-message.json", &v)
	if v.Tail.Total != 3 || len(v.Tail.Messages) != 2 || len(v.Zero.Messages) != 0 {
		t.Fatalf("unexpected fixture shape: tail=%d/%d zero=%d", len(v.Tail.Messages), v.Tail.Total, len(v.Zero.Messages))
	}
	if v.Unknown == nil || len(v.Unknown) != 0 || v.UnknownCount != 0 {
		t.Fatalf("an unknown session must read as [] and count 0: %#v %d", v.Unknown, v.UnknownCount)
	}
	if got := string(v.Got[1].ToolCalls[0].Arguments); got != `{"path":"a<b>&c.csv","limit":10,"encoding":"utf8"}` {
		t.Fatalf("argument bytes changed: %s", got)
	}
}

// The same scenario replayed through the Go store (the fixture inputs appended in
// order, the same reads) reproduces every byte of the fixture.
func TestStoreMessageReplay(t *testing.T) {
	ctx := t.Context()
	var fixture storeMessageScenario
	if err := json.Unmarshal(wiretest.Load(t, "store-message.json"), &fixture); err != nil {
		t.Fatal(err)
	}
	store := persistence.NewMemoryMessageStore()
	for _, m := range fixture.Inputs {
		if err := store.Append(ctx, "s1", m); err != nil {
			t.Fatal(err)
		}
	}
	var out storeMessageScenario
	out.Inputs = fixture.Inputs
	var err error
	if out.Got, err = store.Get(ctx, "s1"); err != nil {
		t.Fatal(err)
	}
	if out.Tail, err = store.GetRange(ctx, "s1", -1, 2); err != nil {
		t.Fatal(err)
	}
	if out.Mid, err = store.GetRange(ctx, "s1", 2, 1); err != nil {
		t.Fatal(err)
	}
	if out.Over, err = store.GetRange(ctx, "s1", 10, 2); err != nil {
		t.Fatal(err)
	}
	if out.Zero, err = store.GetRange(ctx, "s1", 2, 0); err != nil {
		t.Fatal(err)
	}
	if out.Count, err = store.Count(ctx, "s1"); err != nil {
		t.Fatal(err)
	}
	if err = store.Set(ctx, "s1", fixture.Inputs[:1]); err != nil {
		t.Fatal(err)
	}
	if out.AfterSet, err = store.Get(ctx, "s1"); err != nil {
		t.Fatal(err)
	}
	if out.CountAfterSet, err = store.Count(ctx, "s1"); err != nil {
		t.Fatal(err)
	}
	if out.Unknown, err = store.Get(ctx, "nope"); err != nil {
		t.Fatal(err)
	}
	if out.UnknownCount, err = store.Count(ctx, "nope"); err != nil {
		t.Fatal(err)
	}
	b, err := kernel.EncodeJSON(out)
	if err != nil {
		t.Fatal(err)
	}
	wiretest.AssertBytes(t, "store-message.json", b)
}

// storeMemoryQuery is one query of the store-memory fixture. The filter is kept
// raw so an explicit empty string, empty list or zero limit (all "no filter" in
// the store) round-trips untouched.
type storeMemoryQuery struct {
	Name   string          `json:"name"`
	Filter json.RawMessage `json:"filter"`
	Result []memory.Record `json:"result"`
}

// storeMemoryScenario is the recorded MemoryMemoryStore scenario of
// the store-memory fixture, with the fields tagged in fixture order.
type storeMemoryScenario struct {
	Records          []memory.Record    `json:"records"`
	Got              memory.Record      `json:"got"`
	GetMiss          *memory.Record     `json:"getMiss"`
	Queries          []storeMemoryQuery `json:"queries"`
	Found            memory.Record      `json:"found"`
	FoundMiss        *memory.Record     `json:"foundMiss"`
	AfterUpdate      memory.Record      `json:"afterUpdate"`
	AfterDelete      []memory.Record    `json:"afterDelete"`
	FoundAfterDelete *memory.Record     `json:"foundAfterDelete"`
}

// Memory records in the recorded fixture, full (every optional field) and
// minimal, decode strictly and re-encode byte-identical.
func TestStoreMemoryFixture(t *testing.T) {
	var v storeMemoryScenario
	wiretest.AssertRoundTrip(t, "store-memory.json", &v)
	if v.GetMiss != nil || v.FoundMiss != nil || v.FoundAfterDelete != nil {
		t.Fatal("a miss must decode as a nil pointer")
	}
	if v.Got.UpdatedAt == "" || v.Got.LastRecalledAt == "" || v.Got.SupersededBy == "" || v.Got.ExpiresAt == "" || len(v.Got.Tags) != 2 {
		t.Fatalf("the full record lost an optional field: %+v", v.Got)
	}
	if v.Found.ID != "m1" {
		t.Fatalf("findByKey must skip the superseded duplicate and return m1, got %s", v.Found.ID)
	}
	byName := map[string][]memory.Record{}
	for _, q := range v.Queries {
		byName[q.Name] = q.Result
	}
	if len(byName["since"]) == 0 || byName["since"][0].ID != "m2" {
		t.Fatalf("since must be inclusive: %+v", byName["since"])
	}
	for _, r := range byName["before"] {
		if r.ID == "m2" {
			t.Fatal("before must be exclusive")
		}
	}
}

// The same scenario replayed through the Go store (the fixture records saved in
// order, the same queries with the fixture filters) reproduces every byte.
func TestStoreMemoryReplay(t *testing.T) {
	ctx := t.Context()
	var fixture storeMemoryScenario
	if err := json.Unmarshal(wiretest.Load(t, "store-memory.json"), &fixture); err != nil {
		t.Fatal(err)
	}
	store := persistence.NewMemoryMemoryStore()
	for _, r := range fixture.Records {
		if err := store.Save(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	out := storeMemoryScenario{Records: fixture.Records}
	var err error
	out.Got = mustGetMemory(t, store, "m1")
	if out.GetMiss, err = store.Get(ctx, "nope"); err != nil {
		t.Fatal(err)
	}
	for _, q := range fixture.Queries {
		var f persistence.MemoryFilter
		if err := json.Unmarshal(q.Filter, &f); err != nil {
			t.Fatalf("filter %s: %v", q.Name, err)
		}
		res, err := store.Query(ctx, f)
		if err != nil {
			t.Fatal(err)
		}
		out.Queries = append(out.Queries, storeMemoryQuery{Name: q.Name, Filter: q.Filter, Result: res})
	}
	found, err := store.FindByKey(ctx, "a1", memory.ScopeProject, "grid")
	if err != nil || found == nil {
		t.Fatalf("FindByKey = %v, %v", found, err)
	}
	out.Found = *found
	if out.FoundMiss, err = store.FindByKey(ctx, "a2", memory.ScopeUser, "grid"); err != nil {
		t.Fatal(err)
	}
	changed := fixture.Records[2].Clone()
	changed.Value = "changed"
	if err := store.Update(ctx, changed); err != nil {
		t.Fatal(err)
	}
	out.AfterUpdate = mustGetMemory(t, store, "m2")
	if err := store.Delete(ctx, "m1"); err != nil {
		t.Fatal(err)
	}
	if out.AfterDelete, err = store.Query(ctx, persistence.MemoryFilter{AgentID: "a1"}); err != nil {
		t.Fatal(err)
	}
	if out.FoundAfterDelete, err = store.FindByKey(ctx, "a1", memory.ScopeProject, "grid"); err != nil {
		t.Fatal(err)
	}
	b, err := kernel.EncodeJSON(out)
	if err != nil {
		t.Fatal(err)
	}
	wiretest.AssertBytes(t, "store-memory.json", b)
}

func mustGetMemory(t *testing.T, s persistence.MemoryStore, id string) memory.Record {
	t.Helper()
	r, err := s.Get(t.Context(), id)
	if err != nil || r == nil {
		t.Fatalf("Get(%s) = %v, %v", id, r, err)
	}
	return *r
}

// storeAgentScenario is the recorded MemoryAgentStore scenario of
// the store-agent fixture, with the fields tagged in fixture order.
type storeAgentScenario struct {
	Inputs          []profile.AgentProfile `json:"inputs"`
	Loaded          profile.AgentProfile   `json:"loaded"`
	Listed          []profile.AgentProfile `json:"listed"`
	Resaved         profile.AgentProfile   `json:"resaved"`
	ListAfterResave []profile.AgentProfile `json:"listAfterResave"`
	ListAfterDelete []profile.AgentProfile `json:"listAfterDelete"`
	ListAfterReadd  []profile.AgentProfile `json:"listAfterReadd"`
	LoadMiss        *profile.AgentProfile  `json:"loadMiss"`
}

// Agent profiles in the recorded fixture, a full and a minimal one, decode
// strictly and re-encode byte-identical.
func TestStoreAgentFixture(t *testing.T) {
	var v storeAgentScenario
	wiretest.AssertRoundTrip(t, "store-agent.json", &v)
	if v.LoadMiss != nil {
		t.Fatal("a miss must decode as a nil pointer")
	}
	ids := func(ps []profile.AgentProfile) []string {
		var out []string
		for _, p := range ps {
			out = append(out, p.ID)
		}
		return out
	}
	if got := ids(v.ListAfterResave); !slices.Equal(got, []string{"agent-full", "agent-min"}) {
		t.Fatalf("a re-save must keep the list position: %v", got)
	}
	if got := ids(v.ListAfterReadd); !slices.Equal(got, []string{"agent-min", "agent-full"}) {
		t.Fatalf("delete then save must append: %v", got)
	}
}

// The same scenario replayed through the Go store reproduces every byte.
func TestStoreAgentReplay(t *testing.T) {
	ctx := t.Context()
	var fixture storeAgentScenario
	if err := json.Unmarshal(wiretest.Load(t, "store-agent.json"), &fixture); err != nil {
		t.Fatal(err)
	}
	store := persistence.NewMemoryAgentStore()
	for _, p := range fixture.Inputs {
		if err := store.Save(ctx, p); err != nil {
			t.Fatal(err)
		}
	}
	out := storeAgentScenario{Inputs: fixture.Inputs}
	loaded, err := store.Load(ctx, "agent-full")
	if err != nil || loaded == nil {
		t.Fatalf("Load = %v, %v", loaded, err)
	}
	out.Loaded = *loaded
	if out.Listed, err = store.List(ctx); err != nil {
		t.Fatal(err)
	}
	out.Resaved = fixture.Inputs[0].Clone()
	out.Resaved.Name = "Full renamed"
	if err := store.Save(ctx, out.Resaved); err != nil {
		t.Fatal(err)
	}
	if out.ListAfterResave, err = store.List(ctx); err != nil {
		t.Fatal(err)
	}
	if err := store.Delete(ctx, "agent-full"); err != nil {
		t.Fatal(err)
	}
	if out.ListAfterDelete, err = store.List(ctx); err != nil {
		t.Fatal(err)
	}
	if err := store.Save(ctx, fixture.Inputs[0]); err != nil {
		t.Fatal(err)
	}
	if out.ListAfterReadd, err = store.List(ctx); err != nil {
		t.Fatal(err)
	}
	if out.LoadMiss, err = store.Load(ctx, "nope"); err != nil {
		t.Fatal(err)
	}
	b, err := kernel.EncodeJSON(out)
	if err != nil {
		t.Fatal(err)
	}
	wiretest.AssertBytes(t, "store-agent.json", b)
}

// storeProviderScenario is the recorded MemoryProviderStore
// scenario of the store-provider fixture, with the fields tagged in fixture order.
type storeProviderScenario struct {
	Inputs          []persistence.ProviderRecord `json:"inputs"`
	Loaded          persistence.ProviderRecord   `json:"loaded"`
	Listed          []persistence.ProviderRecord `json:"listed"`
	LoadMiss        *persistence.ProviderRecord  `json:"loadMiss"`
	ListAfterDelete []persistence.ProviderRecord `json:"listAfterDelete"`
}

// Provider records in the recorded fixture (a plain one with two models, a
// Bedrock one and an OAuth one) decode strictly and re-encode byte-identical; the
// secrets are fake values.
func TestStoreProviderFixture(t *testing.T) {
	var v storeProviderScenario
	wiretest.AssertRoundTrip(t, "store-provider.json", &v)
	if v.LoadMiss != nil {
		t.Fatal("a miss must decode as a nil pointer")
	}
	anthropic := v.Loaded
	if len(anthropic.Models) != 2 || anthropic.Models[0].Capabilities == nil || anthropic.Models[1].Pricing == nil ||
		anthropic.Models[1].Pricing.InputUSDPerMillionTokens == nil || *anthropic.Models[1].Pricing.InputUSDPerMillionTokens != 0 {
		t.Fatalf("model metadata lost: %+v", anthropic.Models)
	}
	oauth := v.Inputs[2]
	if oauth.APIKey != "" || oauth.Models == nil || len(oauth.Models) != 0 || oauth.TokenExpiresAt == nil || oauth.AuthType != "oauth" {
		t.Fatalf("OAuth record lost a field: %+v", oauth)
	}
}

// The same scenario replayed through the Go store reproduces every byte.
func TestStoreProviderReplay(t *testing.T) {
	ctx := t.Context()
	var fixture storeProviderScenario
	if err := json.Unmarshal(wiretest.Load(t, "store-provider.json"), &fixture); err != nil {
		t.Fatal(err)
	}
	store := persistence.NewMemoryProviderStore()
	for _, p := range fixture.Inputs {
		if err := store.Save(ctx, p); err != nil {
			t.Fatal(err)
		}
	}
	out := storeProviderScenario{Inputs: fixture.Inputs}
	loaded, err := store.Load(ctx, "anthropic")
	if err != nil || loaded == nil {
		t.Fatalf("Load = %v, %v", loaded, err)
	}
	out.Loaded = *loaded
	if out.Listed, err = store.List(ctx); err != nil {
		t.Fatal(err)
	}
	if out.LoadMiss, err = store.Load(ctx, "nope"); err != nil {
		t.Fatal(err)
	}
	if err := store.Delete(ctx, "anthropic"); err != nil {
		t.Fatal(err)
	}
	if out.ListAfterDelete, err = store.List(ctx); err != nil {
		t.Fatal(err)
	}
	b, err := kernel.EncodeJSON(out)
	if err != nil {
		t.Fatal(err)
	}
	wiretest.AssertBytes(t, "store-provider.json", b)
}

// storeSkillScenario is the recorded MemorySkillStore scenario of
// the store-skill fixture, with the fields tagged in fixture order.
type storeSkillScenario struct {
	Inputs          []persistence.SkillRecord `json:"inputs"`
	Loaded          persistence.SkillRecord   `json:"loaded"`
	Listed          []persistence.SkillRecord `json:"listed"`
	ListAfterDelete []persistence.SkillRecord `json:"listAfterDelete"`
	LoadMiss        *persistence.SkillRecord  `json:"loadMiss"`
}

// Skill records in the recorded fixture decode strictly and re-encode
// byte-identical, including an explicit empty files list.
func TestStoreSkillFixture(t *testing.T) {
	var v storeSkillScenario
	wiretest.AssertRoundTrip(t, "store-skill.json", &v)
	if v.LoadMiss != nil {
		t.Fatal("a miss must decode as a nil pointer")
	}
	if v.Inputs[0].CreatedBy != "agent-1" || len(v.Inputs[0].Files) != 1 {
		t.Fatalf("the first record lost createdBy or its file: %+v", v.Inputs[0])
	}
	if v.Inputs[1].CreatedBy != "" || v.Inputs[1].Files == nil || len(v.Inputs[1].Files) != 0 {
		t.Fatalf("an explicit empty files list must decode as empty and non-nil: %+v", v.Inputs[1])
	}
}

// The same scenario replayed through the Go store reproduces every byte.
func TestStoreSkillReplay(t *testing.T) {
	ctx := t.Context()
	var fixture storeSkillScenario
	if err := json.Unmarshal(wiretest.Load(t, "store-skill.json"), &fixture); err != nil {
		t.Fatal(err)
	}
	store := persistence.NewMemorySkillStore()
	for _, r := range fixture.Inputs {
		if err := store.Save(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	out := storeSkillScenario{Inputs: fixture.Inputs}
	loaded, err := store.Load(ctx, "quantity-takeoff")
	if err != nil || loaded == nil {
		t.Fatalf("Load = %v, %v", loaded, err)
	}
	out.Loaded = *loaded
	if out.Listed, err = store.List(ctx); err != nil {
		t.Fatal(err)
	}
	if err := store.Delete(ctx, "quantity-takeoff"); err != nil {
		t.Fatal(err)
	}
	if out.ListAfterDelete, err = store.List(ctx); err != nil {
		t.Fatal(err)
	}
	if out.LoadMiss, err = store.Load(ctx, "quantity-takeoff"); err != nil {
		t.Fatal(err)
	}
	b, err := kernel.EncodeJSON(out)
	if err != nil {
		t.Fatal(err)
	}
	wiretest.AssertBytes(t, "store-skill.json", b)
}

// artifactPair is one head record with the version written with it.
type artifactPair struct {
	Artifact artifact.Artifact `json:"artifact"`
	Version  artifact.Version  `json:"version"`
}

// storeArtifactQuery is one query of the store-artifact fixture. The filter is
// kept raw so explicit empty values round-trip untouched.
type storeArtifactQuery struct {
	Name   string              `json:"name"`
	Filter json.RawMessage     `json:"filter"`
	Result []artifact.Artifact `json:"result"`
}

// storeArtifactScenario is the recorded MemoryArtifactStore
// scenario of the store-artifact fixture, with the fields tagged in fixture order.
type storeArtifactScenario struct {
	Creates                    []artifactPair       `json:"creates"`
	Added                      artifactPair         `json:"added"`
	Got                        artifact.Artifact    `json:"got"`
	GetMiss                    *artifact.Artifact   `json:"getMiss"`
	FoundByName                artifact.Artifact    `json:"foundByName"`
	FoundOther                 artifact.Artifact    `json:"foundOther"`
	FoundMiss                  *artifact.Artifact   `json:"foundMiss"`
	Versions                   []artifact.Version   `json:"versions"`
	VersionsUnknown            []artifact.Version   `json:"versionsUnknown"`
	Queries                    []storeArtifactQuery `json:"queries"`
	AfterDelete                []artifact.Artifact  `json:"afterDelete"`
	VersionsAfterDelete        []artifact.Version   `json:"versionsAfterDelete"`
	AfterDeleteByAgent         []artifact.Artifact  `json:"afterDeleteByAgent"`
	VersionsAfterDeleteByAgent []artifact.Version   `json:"versionsAfterDeleteByAgent"`
}

// Artifact head records and versions in the recorded fixture decode strictly
// and re-encode byte-identical; the versions come back newest first and the query
// results newest updated first, with the tie in insertion order.
func TestStoreArtifactFixture(t *testing.T) {
	var v storeArtifactScenario
	wiretest.AssertRoundTrip(t, "store-artifact.json", &v)
	if v.GetMiss != nil || v.FoundMiss != nil {
		t.Fatal("a miss must decode as a nil pointer")
	}
	if len(v.Versions) != 2 || v.Versions[0].Version != 2 || v.Versions[1].Version != 1 {
		t.Fatalf("versions must be newest first: %+v", v.Versions)
	}
	if v.Got.Version != 2 || v.Got.Description == "" || len(v.Got.Tags) != 2 {
		t.Fatalf("the head record lost a field: %+v", v.Got)
	}
	byName := map[string][]artifact.Artifact{}
	for _, q := range v.Queries {
		byName[q.Name] = q.Result
	}
	got := []string{}
	for _, a := range byName["byAgent"] {
		got = append(got, a.ID)
	}
	if !slices.Equal(got, []string{"art-1", "art-2", "art-3"}) {
		t.Fatalf("query order (newest updated first, ties in insertion order) = %v", got)
	}
}

// The same scenario replayed through the Go store (the fixture creates and version in
// order, the same queries with the fixture filters) reproduces every byte.
func TestStoreArtifactReplay(t *testing.T) {
	ctx := t.Context()
	var fixture storeArtifactScenario
	if err := json.Unmarshal(wiretest.Load(t, "store-artifact.json"), &fixture); err != nil {
		t.Fatal(err)
	}
	store := persistence.NewMemoryArtifactStore()
	for _, c := range fixture.Creates {
		if err := store.Create(ctx, c.Artifact, c.Version); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.AddVersion(ctx, fixture.Added.Artifact, fixture.Added.Version); err != nil {
		t.Fatal(err)
	}
	out := storeArtifactScenario{Creates: fixture.Creates, Added: fixture.Added}
	var err error
	mustArtifact := func(a *artifact.Artifact, err error) artifact.Artifact {
		t.Helper()
		if err != nil || a == nil {
			t.Fatalf("lookup = %v, %v", a, err)
		}
		return *a
	}
	out.Got = mustArtifact(store.Get(ctx, "art-1"))
	if out.GetMiss, err = store.Get(ctx, "nope"); err != nil {
		t.Fatal(err)
	}
	out.FoundByName = mustArtifact(store.FindByName(ctx, "a1", "report.md"))
	out.FoundOther = mustArtifact(store.FindByName(ctx, "a2", "report.md"))
	if out.FoundMiss, err = store.FindByName(ctx, "a1", "nope"); err != nil {
		t.Fatal(err)
	}
	if out.Versions, err = store.ListVersions(ctx, "art-1"); err != nil {
		t.Fatal(err)
	}
	if out.VersionsUnknown, err = store.ListVersions(ctx, "nope"); err != nil {
		t.Fatal(err)
	}
	for _, q := range fixture.Queries {
		var f persistence.ArtifactFilter
		if err := json.Unmarshal(q.Filter, &f); err != nil {
			t.Fatalf("filter %s: %v", q.Name, err)
		}
		res, err := store.Query(ctx, f)
		if err != nil {
			t.Fatal(err)
		}
		out.Queries = append(out.Queries, storeArtifactQuery{Name: q.Name, Filter: q.Filter, Result: res})
	}
	if err := store.Delete(ctx, "art-2"); err != nil {
		t.Fatal(err)
	}
	if out.AfterDelete, err = store.Query(ctx, persistence.ArtifactFilter{}); err != nil {
		t.Fatal(err)
	}
	if out.VersionsAfterDelete, err = store.ListVersions(ctx, "art-2"); err != nil {
		t.Fatal(err)
	}
	if err := store.DeleteByAgent(ctx, "a2"); err != nil {
		t.Fatal(err)
	}
	if out.AfterDeleteByAgent, err = store.Query(ctx, persistence.ArtifactFilter{}); err != nil {
		t.Fatal(err)
	}
	if out.VersionsAfterDeleteByAgent, err = store.ListVersions(ctx, "art-4"); err != nil {
		t.Fatal(err)
	}
	b, err := kernel.EncodeJSON(out)
	if err != nil {
		t.Fatal(err)
	}
	wiretest.AssertBytes(t, "store-artifact.json", b)
}

// Every store-*.json fixture must be listed in storeFixtures so a new store
// fixture cannot be added without a Go assertion.
func TestStoreFixturesCovered(t *testing.T) {
	entries, err := os.ReadDir(wiretest.Dir(t))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		name := e.Name()
		if strings.HasPrefix(name, "store-") && strings.HasSuffix(name, ".json") && !slices.Contains(storeFixtures, name) {
			t.Errorf("fixture %s has no Go assertion: add it to storeFixtures and cover it", name)
		}
	}
	for _, name := range storeFixtures {
		if _, err := os.Stat(wiretest.Dir(t) + "/" + name); err != nil {
			t.Errorf("listed fixture %s is missing: %v", name, err)
		}
	}
}
