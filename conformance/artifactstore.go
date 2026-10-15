package conformance

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/Phong95/aec-agent-go/components/artifacthub/artifact"
	"github.com/Phong95/aec-agent-go/components/persistence"
	"github.com/Phong95/aec-agent-go/conformance/internal/check"
	"github.com/Phong95/aec-agent-go/kernel"
)

// ArtifactStoreFactory returns a persistence.ArtifactStore. See the package
// comment for the freshness rules of the store suites.
type ArtifactStoreFactory func(t *testing.T) persistence.ArtifactStore

// artifactISOLayout is the shape of JS toISOString.
const artifactISOLayout = "2006-01-02T15:04:05.000Z"

// artOpts are the overrides of the artifact fixture; an empty field takes the
// fixture default.
type artOpts struct {
	id, agentID, name, typ, description, sessionID string
	sandboxPath, sandboxID, createdAt, updatedAt   string
	tags                                           []string
}

// uniqueAgentID returns a fresh agent id, so a shared store can run the suite.
func uniqueAgentID() string {
	return "agent-" + kernel.NewID()
}

// artifactFixture builds a head record and its version 1: unique ids per call,
// content "artifact body".
func artifactFixture(o artOpts) (artifact.Artifact, artifact.Version) {
	id := o.id
	if id == "" {
		id = "art-" + kernel.NewID()
	}
	agentID := o.agentID
	if agentID == "" {
		agentID = uniqueAgentID()
	}
	createdAt := o.createdAt
	if createdAt == "" {
		createdAt = "2026-01-01T00:00:00.000Z"
	}
	sandboxPath := o.sandboxPath
	if sandboxPath == "" {
		sandboxPath = "/workspace/agents/" + agentID + "/artifacts/" + id + ".md"
	}
	sandboxID := o.sandboxID
	if sandboxID == "" {
		sandboxID = "docker:shared"
	}
	name := o.name
	if name == "" {
		name = "artifact-" + id + ".md"
	}
	typ := o.typ
	if typ == "" {
		typ = "text/markdown"
	}
	updatedAt := o.updatedAt
	if updatedAt == "" {
		updatedAt = createdAt
	}
	v := artifactVersionFor(id, 1, "artifact body", agentID, sandboxPath, sandboxID)
	a := artifact.Artifact{
		ID: id, AgentID: agentID, Name: name, Type: typ,
		Description: o.description, Tags: slices.Clone(o.tags), SessionID: o.sessionID,
		Version: 1, SandboxPath: sandboxPath, SandboxID: sandboxID,
		Bytes: v.Bytes, SHA256: v.SHA256, CreatedAt: createdAt, UpdatedAt: updatedAt,
	}
	return a, v
}

// artifactVersionFor builds one content-less version entry. The creation time is
// midnight UTC on January <version> 2026.
func artifactVersionFor(artifactID string, version int, content, agentID, sandboxPath, sandboxID string) artifact.Version {
	sum := sha256.Sum256([]byte(content))
	return artifact.Version{
		ArtifactID: artifactID,
		Version:    version,
		Bytes:      len(content),
		SHA256:     hex.EncodeToString(sum[:]),
		CreatedAt:  time.Date(2026, time.January, version, 0, 0, 0, 0, time.UTC).Format(artifactISOLayout),
		Note:       "version " + strconv.Itoa(version),
		Origin:     artifact.Origin{AgentID: agentID, SessionID: "s1", Tool: "save_artifact", SandboxPath: sandboxPath, SandboxID: sandboxID},
	}
}

// laterThan returns the timestamp one second after the given ISO timestamp.
func laterThan(t *testing.T, timestamp string) string {
	t.Helper()
	at, err := time.Parse(artifactISOLayout, timestamp)
	check.NoErr(t, err, "parse timestamp %q", timestamp)
	return at.Add(time.Second).UTC().Format(artifactISOLayout)
}

// nextVersion returns the head record moved to a new version, and that version.
func nextVersion(t *testing.T, a artifact.Artifact, version int, content string) (artifact.Artifact, artifact.Version) {
	t.Helper()
	v := artifactVersionFor(a.ID, version, content, a.AgentID, a.SandboxPath, a.SandboxID)
	head := a
	head.Version = version
	head.Bytes = v.Bytes
	head.SHA256 = v.SHA256
	head.UpdatedAt = laterThan(t, a.UpdatedAt)
	return head, v
}

func createArtifact(t *testing.T, s persistence.ArtifactStore, a artifact.Artifact, v artifact.Version) {
	t.Helper()
	check.NoErr(t, s.Create(t.Context(), a, v), "create %s", a.ID)
}

func addArtifactVersion(t *testing.T, s persistence.ArtifactStore, a artifact.Artifact, v artifact.Version) {
	t.Helper()
	check.NoErr(t, s.AddVersion(t.Context(), a, v), "addVersion %s", a.ID)
}

// getArtifact returns the head record and fails the test when it is absent.
func getArtifact(t *testing.T, s persistence.ArtifactStore, id string) artifact.Artifact {
	t.Helper()
	got, err := s.Get(t.Context(), id)
	check.NoErr(t, err, "get %s", id)
	if got == nil {
		t.Fatalf("artifact %s is absent, want it stored", id)
	}
	return *got
}

func listArtifactVersions(t *testing.T, s persistence.ArtifactStore, id string) []artifact.Version {
	t.Helper()
	out, err := s.ListVersions(t.Context(), id)
	check.NoErr(t, err, "listVersions %s", id)
	return out
}

func queryArtifacts(t *testing.T, s persistence.ArtifactStore, f persistence.ArtifactFilter) []artifact.Artifact {
	t.Helper()
	out, err := s.Query(t.Context(), f)
	check.NoErr(t, err, "query")
	return out
}

func artifactNames(list []artifact.Artifact) []string {
	out := make([]string, 0, len(list))
	for _, a := range list {
		out = append(out, a.Name)
	}
	return out
}

func versionNumbers(list []artifact.Version) []int {
	out := make([]int, 0, len(list))
	for _, v := range list {
		out = append(out, v.Version)
	}
	return out
}

// findVersion returns the entry with the version number, or false.
func findVersion(list []artifact.Version, n int) (artifact.Version, bool) {
	for _, v := range list {
		if v.Version == n {
			return v, true
		}
	}
	return artifact.Version{}, false
}

// ArtifactStore runs the conformance suite for persistence.ArtifactStore
// implementations (A-62).
//
// It checks the invariants the artifact hub relies on: the head record and its
// content-less version log stay consistent, versions are contiguous, names and
// queries are isolated per agent, ordering is newest first, and deletes cascade.
// Content is never stored here: every fixture's version carries only metadata
// plus the sandboxPath and sandboxId of the file it tracked, and the tests assert
// that shape holds through every write path.
//
// The suite uses unique agent and artifact ids, and it asserts order only where
// the contract defines one (versions newest first, queries newest updated
// first). The Go-only subtests
// prove that records are copied in and out and that concurrent Create calls keep
// one whole record per id.
func ArtifactStore(t *testing.T, newStore ArtifactStoreFactory) {
	t.Helper()

	// -- Lifecycle --

	t.Run("initialize() resolves without error", func(t *testing.T) {
		store := newStore(t)
		check.NoErr(t, kernel.Initialize(t.Context(), store))
	})

	t.Run("dispose() resolves without error", func(t *testing.T) {
		store := newStore(t)
		check.NoErr(t, kernel.Initialize(t.Context(), store))
		check.NoErr(t, kernel.Dispose(t.Context(), store))
	})

	// -- create() / get() --

	t.Run("create() stores the head record and version 1", func(t *testing.T) {
		store := newStore(t)
		check.NoErr(t, kernel.Initialize(t.Context(), store))
		a, v := artifactFixture(artOpts{name: "report.md"})

		createArtifact(t, store, a, v)

		got := getArtifact(t, store, a.ID)
		check.Equal(t, got.ID, a.ID)
		check.Equal(t, got.Name, "report.md")
		check.Equal(t, got.Version, 1)
		check.Equal(t, got.SandboxPath, a.SandboxPath)
		check.Equal(t, got.SandboxID, a.SandboxID)
		versions := listArtifactVersions(t, store, a.ID)
		check.True(t, len(versions) > 0, "the version log is empty")
		if len(versions) > 0 {
			check.Equal(t, versions[0].Version, 1)
			check.Equal(t, versions[0].SHA256, v.SHA256)
		}
	})

	t.Run("get() returns null for an unknown id", func(t *testing.T) {
		store := newStore(t)
		check.NoErr(t, kernel.Initialize(t.Context(), store))

		got, err := store.Get(t.Context(), "missing-"+kernel.NewID())
		check.NoErr(t, err)
		check.True(t, got == nil, "an unknown id must give a nil artifact")
	})

	// -- findByName() --

	t.Run("findByName() is scoped per agent - the same name in two agents is two artifacts", func(t *testing.T) {
		store := newStore(t)
		check.NoErr(t, kernel.Initialize(t.Context(), store))
		agentA, agentB, agentC := uniqueAgentID(), uniqueAgentID(), uniqueAgentID()
		a, av := artifactFixture(artOpts{agentID: agentA, name: "shared-name.md"})
		b, bv := artifactFixture(artOpts{agentID: agentB, name: "shared-name.md"})
		createArtifact(t, store, a, av)
		createArtifact(t, store, b, bv)

		foundA, err := store.FindByName(t.Context(), agentA, "shared-name.md")
		check.NoErr(t, err)
		foundB, err := store.FindByName(t.Context(), agentB, "shared-name.md")
		check.NoErr(t, err)
		foundC, err := store.FindByName(t.Context(), agentC, "shared-name.md")
		check.NoErr(t, err)

		if foundA == nil || foundB == nil {
			t.Fatalf("findByName missed an artifact: agent A %v, agent B %v", foundA, foundB)
		}
		check.Equal(t, foundA.ID, a.ID)
		check.Equal(t, foundB.ID, b.ID)
		check.True(t, foundC == nil, "an agent without the artifact must get a nil result")
	})

	// -- addVersion() --

	t.Run("addVersion() moves the head and keeps every earlier version in the log", func(t *testing.T) {
		store := newStore(t)
		check.NoErr(t, kernel.Initialize(t.Context(), store))
		a, v := artifactFixture(artOpts{})
		createArtifact(t, store, a, v)

		head, second := nextVersion(t, a, 2, "v2 body")
		addArtifactVersion(t, store, head, second)

		got := getArtifact(t, store, a.ID)
		check.Equal(t, got.Version, 2)
		check.Equal(t, got.SHA256, second.SHA256)
		versions := listArtifactVersions(t, store, a.ID)
		check.DeepEqual(t, versionNumbers(versions), []int{2, 1})
		if first, ok := findVersion(versions, 1); ok {
			check.Equal(t, first.SHA256, v.SHA256)
		} else {
			t.Errorf("version 1 is missing from the log")
		}
	})

	t.Run("addVersion() persists changed head metadata (description, tags)", func(t *testing.T) {
		store := newStore(t)
		check.NoErr(t, kernel.Initialize(t.Context(), store))
		a, v := artifactFixture(artOpts{})
		createArtifact(t, store, a, v)

		head, second := nextVersion(t, a, 2, "next")
		head.Description = "now described"
		head.Tags = []string{"x", "y"}
		addArtifactVersion(t, store, head, second)

		got := getArtifact(t, store, a.ID)
		check.Equal(t, got.Description, "now described")
		check.DeepEqual(t, got.Tags, []string{"x", "y"})
	})

	t.Run("head and origin keep sandboxPath and sandboxId across addVersion()", func(t *testing.T) {
		store := newStore(t)
		check.NoErr(t, kernel.Initialize(t.Context(), store))
		a, v := artifactFixture(artOpts{sandboxPath: "/workspace/agents/a1/artifacts/x.md", sandboxID: "docker:shared"})
		createArtifact(t, store, a, v)

		newPath := "/workspace/agents/a1/out/x.md"
		second := artifactVersionFor(a.ID, 2, "moved", a.AgentID, newPath, "host:shared")
		head := a
		head.Version = 2
		head.Bytes = second.Bytes
		head.SHA256 = second.SHA256
		head.SandboxPath = newPath
		head.SandboxID = "host:shared"
		head.UpdatedAt = laterThan(t, a.UpdatedAt)
		addArtifactVersion(t, store, head, second)

		got := getArtifact(t, store, a.ID)
		check.Equal(t, got.SandboxPath, newPath)
		check.Equal(t, got.SandboxID, "host:shared")
		versions := listArtifactVersions(t, store, a.ID)
		if first, ok := findVersion(versions, 1); ok {
			check.Equal(t, first.Origin.SandboxPath, a.SandboxPath)
			check.Equal(t, first.Origin.SandboxID, a.SandboxID)
		} else {
			t.Errorf("version 1 is missing from the log")
		}
		if moved, ok := findVersion(versions, 2); ok {
			check.Equal(t, moved.Origin.SandboxPath, newPath)
			check.Equal(t, moved.Origin.SandboxID, "host:shared")
		} else {
			t.Errorf("version 2 is missing from the log")
		}
	})

	// -- listVersions() --

	t.Run("listVersions() returns newest first", func(t *testing.T) {
		store := newStore(t)
		check.NoErr(t, kernel.Initialize(t.Context(), store))
		a, v := artifactFixture(artOpts{})
		createArtifact(t, store, a, v)
		head, second := nextVersion(t, a, 2, "second")
		addArtifactVersion(t, store, head, second)

		versions := listArtifactVersions(t, store, a.ID)

		check.DeepEqual(t, versionNumbers(versions), []int{2, 1})
		for _, entry := range versions {
			check.Equal(t, entry.Origin.AgentID, a.AgentID)
		}
	})

	t.Run("listVersions() records carry no content", func(t *testing.T) {
		store := newStore(t)
		check.NoErr(t, kernel.Initialize(t.Context(), store))
		a, v := artifactFixture(artOpts{})
		createArtifact(t, store, a, v)

		versions := listArtifactVersions(t, store, a.ID)

		for _, entry := range versions {
			wire, err := kernel.EncodeJSON(entry)
			check.NoErr(t, err)
			var keys map[string]json.RawMessage
			check.NoErr(t, json.Unmarshal(wire, &keys))
			_, hasContent := keys["content"]
			check.False(t, hasContent, "version %d carries a content key", entry.Version)
		}
	})

	t.Run("listVersions() returns an empty list for an unknown artifact", func(t *testing.T) {
		store := newStore(t)
		check.NoErr(t, kernel.Initialize(t.Context(), store))

		versions := listArtifactVersions(t, store, "missing-"+kernel.NewID())

		check.Len(t, versions, 0)
	})

	// -- query() --

	t.Run("query() never returns another agent's artifacts", func(t *testing.T) {
		store := newStore(t)
		check.NoErr(t, kernel.Initialize(t.Context(), store))
		mineAgent, theirAgent := uniqueAgentID(), uniqueAgentID()
		mine, mineV := artifactFixture(artOpts{agentID: mineAgent, name: "mine.md"})
		theirs, theirsV := artifactFixture(artOpts{agentID: theirAgent, name: "theirs.md"})
		createArtifact(t, store, mine, mineV)
		createArtifact(t, store, theirs, theirsV)

		found := queryArtifacts(t, store, persistence.ArtifactFilter{AgentID: mineAgent})

		check.DeepEqual(t, artifactNames(found), []string{"mine.md"})
	})

	t.Run("query() orders by updatedAt, newest first", func(t *testing.T) {
		store := newStore(t)
		check.NoErr(t, kernel.Initialize(t.Context(), store))
		agentID := uniqueAgentID()
		older, olderV := artifactFixture(artOpts{agentID: agentID, name: "older.md", updatedAt: "2026-01-01T00:00:00.000Z"})
		newer, newerV := artifactFixture(artOpts{agentID: agentID, name: "newer.md", updatedAt: "2026-06-01T00:00:00.000Z"})
		createArtifact(t, store, older, olderV)
		createArtifact(t, store, newer, newerV)

		found := queryArtifacts(t, store, persistence.ArtifactFilter{AgentID: agentID})

		check.DeepEqual(t, artifactNames(found), []string{"newer.md", "older.md"})
	})

	t.Run("query() filters by type, name, tag and session", func(t *testing.T) {
		store := newStore(t)
		check.NoErr(t, kernel.Initialize(t.Context(), store))
		agentID := uniqueAgentID()
		md, mdV := artifactFixture(artOpts{agentID: agentID, name: "a.md", typ: "text/markdown", tags: []string{"report"}, sessionID: "s1"})
		csv, csvV := artifactFixture(artOpts{agentID: agentID, name: "b.csv", typ: "text/csv", tags: []string{"takeoff"}, sessionID: "s2"})
		createArtifact(t, store, md, mdV)
		createArtifact(t, store, csv, csvV)

		check.DeepEqual(t, artifactNames(queryArtifacts(t, store, persistence.ArtifactFilter{AgentID: agentID, Types: []string{"text/csv"}})), []string{"b.csv"})
		check.DeepEqual(t, artifactNames(queryArtifacts(t, store, persistence.ArtifactFilter{AgentID: agentID, Name: "a.md"})), []string{"a.md"})
		check.DeepEqual(t, artifactNames(queryArtifacts(t, store, persistence.ArtifactFilter{AgentID: agentID, Tags: []string{"takeoff"}})), []string{"b.csv"})
		check.DeepEqual(t, artifactNames(queryArtifacts(t, store, persistence.ArtifactFilter{AgentID: agentID, SessionID: "s1"})), []string{"a.md"})
	})

	t.Run("query() honours limit", func(t *testing.T) {
		store := newStore(t)
		check.NoErr(t, kernel.Initialize(t.Context(), store))
		agentID := uniqueAgentID()
		first, firstV := artifactFixture(artOpts{agentID: agentID, name: "one.md", updatedAt: "2026-01-01T00:00:00.000Z"})
		second, secondV := artifactFixture(artOpts{agentID: agentID, name: "two.md", updatedAt: "2026-02-01T00:00:00.000Z"})
		createArtifact(t, store, first, firstV)
		createArtifact(t, store, second, secondV)

		found := queryArtifacts(t, store, persistence.ArtifactFilter{AgentID: agentID, Limit: 1})

		check.Len(t, found, 1)
		if len(found) == 1 {
			check.Equal(t, found[0].Name, "two.md")
		}
	})

	// -- delete() --

	t.Run("delete() removes the artifact and all of its versions", func(t *testing.T) {
		store := newStore(t)
		check.NoErr(t, kernel.Initialize(t.Context(), store))
		a, v := artifactFixture(artOpts{})
		createArtifact(t, store, a, v)
		head, second := nextVersion(t, a, 2, "second")
		addArtifactVersion(t, store, head, second)

		check.NoErr(t, store.Delete(t.Context(), a.ID))

		got, err := store.Get(t.Context(), a.ID)
		check.NoErr(t, err)
		check.True(t, got == nil, "a deleted artifact must be absent")
		check.Len(t, listArtifactVersions(t, store, a.ID), 0)
	})

	t.Run("delete() of an unknown id resolves without error", func(t *testing.T) {
		store := newStore(t)
		check.NoErr(t, kernel.Initialize(t.Context(), store))

		check.NoErr(t, store.Delete(t.Context(), "missing-"+kernel.NewID()))
	})

	t.Run("deleteByAgent() removes only that agent's artifacts", func(t *testing.T) {
		store := newStore(t)
		check.NoErr(t, kernel.Initialize(t.Context(), store))
		mineAgent, theirAgent := uniqueAgentID(), uniqueAgentID()
		mine, mineV := artifactFixture(artOpts{agentID: mineAgent})
		theirs, theirsV := artifactFixture(artOpts{agentID: theirAgent})
		createArtifact(t, store, mine, mineV)
		createArtifact(t, store, theirs, theirsV)

		check.NoErr(t, store.DeleteByAgent(t.Context(), mineAgent))

		gone, err := store.Get(t.Context(), mine.ID)
		check.NoErr(t, err)
		check.True(t, gone == nil, "the deleted agent's artifact must be absent")
		kept, err := store.Get(t.Context(), theirs.ID)
		check.NoErr(t, err)
		check.True(t, kept != nil, "the other agent's artifact must stay")
		check.Len(t, queryArtifacts(t, store, persistence.ArtifactFilter{AgentID: mineAgent}), 0)
	})

	// -- Go-only guarantees --

	t.Run("mutating a returned artifact or version does not change the store (Go-only)", func(t *testing.T) {
		store := newStore(t)
		check.NoErr(t, kernel.Initialize(t.Context(), store))
		agentID := uniqueAgentID()
		a, v := artifactFixture(artOpts{agentID: agentID, name: "copies.md", description: "original", tags: []string{"one", "two"}})
		head, second := nextVersion(t, a, 2, "second")
		head.Tags = []string{"one", "two"}

		given, givenV := a, v
		given.Tags = []string{"one", "two"}
		createArtifact(t, store, given, givenV)
		givenHead, givenSecond := head, second
		givenHead.Tags = []string{"one", "two"}
		addArtifactVersion(t, store, givenHead, givenSecond)

		// What the caller passed in is the caller's again.
		given.Tags[0] = "changed after create"
		givenHead.Tags[0] = "changed after addVersion"
		givenHead.Description = "changed"
		givenV.SHA256 = "changed"
		givenSecond.SHA256 = "changed"

		verify := func(label string) {
			t.Helper()
			got := getArtifact(t, store, a.ID)
			check.DeepEqual(t, got.Tags, []string{"one", "two"}, "%s: head tags", label)
			check.Equal(t, got.Description, "original", "%s: head description", label)
			check.Equal(t, got.Version, 2, "%s: head version", label)
			check.Equal(t, got.SHA256, second.SHA256, "%s: head sha256", label)
			versions := listArtifactVersions(t, store, a.ID)
			check.DeepEqual(t, versionNumbers(versions), []int{2, 1}, "%s: version log", label)
			if first, ok := findVersion(versions, 1); ok {
				check.Equal(t, first.SHA256, v.SHA256, "%s: version 1 sha256", label)
				check.Equal(t, first.Note, v.Note, "%s: version 1 note", label)
			}
			if last, ok := findVersion(versions, 2); ok {
				check.Equal(t, last.SHA256, second.SHA256, "%s: version 2 sha256", label)
			}
			found := queryArtifacts(t, store, persistence.ArtifactFilter{AgentID: agentID})
			check.Len(t, found, 1, "%s: query", label)
			if len(found) == 1 {
				check.DeepEqual(t, found[0].Tags, []string{"one", "two"}, "%s: queried tags", label)
			}
		}
		verify("after mutating the values given to Create and AddVersion")

		// Change every mutable part of a head record returned by Get.
		got := getArtifact(t, store, a.ID)
		if len(got.Tags) > 0 {
			got.Tags[0] = "changed after get"
		}
		got.Description = "changed after get"
		verify("after mutating a head record returned by Get")

		// Change the entries and the slice returned by ListVersions.
		versions := listArtifactVersions(t, store, a.ID)
		for i := range versions {
			versions[i].SHA256 = "changed after listVersions"
			versions[i].Note = "changed after listVersions"
			versions[i].Origin.AgentID = "changed after listVersions"
		}
		slices.Reverse(versions)
		verify("after mutating the versions returned by ListVersions")

		// Change every mutable part of a head record returned by Query.
		for _, q := range queryArtifacts(t, store, persistence.ArtifactFilter{AgentID: agentID}) {
			if len(q.Tags) > 0 {
				q.Tags[0] = "changed after query"
			}
		}
		verify("after mutating a head record returned by Query")
	})

	t.Run("concurrent Create of distinct and same ids keeps one record per id (Go-only)", func(t *testing.T) {
		const writers = 16
		store := newStore(t)
		check.NoErr(t, kernel.Initialize(t.Context(), store))

		// Distinct ids share one run-unique agent, so a query by that agent sees
		// exactly this subtest's records. The same-id records get their own agent
		// and differ in description and note, so a torn or duplicated record can
		// be told from a whole one.
		distinctAgent, sameAgent := uniqueAgentID(), uniqueAgentID()
		sameID := "art-" + kernel.NewID()
		distinct := make([]artifact.Artifact, writers)
		distinctV := make([]artifact.Version, writers)
		same := make([]artifact.Artifact, writers)
		sameV := make([]artifact.Version, writers)
		descriptions := make([]string, writers)
		for w := range writers {
			distinct[w], distinctV[w] = artifactFixture(artOpts{agentID: distinctAgent, name: fmt.Sprintf("conc%d.md", w)})
			descriptions[w] = fmt.Sprintf("writer %d", w)
			same[w], sameV[w] = artifactFixture(artOpts{id: sameID, agentID: sameAgent, name: "same.md", description: descriptions[w]})
			sameV[w].Note = descriptions[w]
		}
		var wg sync.WaitGroup
		for w := range writers {
			wg.Go(func() {
				if err := store.Create(t.Context(), distinct[w], distinctV[w]); err != nil {
					t.Errorf("create distinct from writer %d: %v", w, err)
				}
				if err := store.Create(t.Context(), same[w], sameV[w]); err != nil {
					t.Errorf("create same id from writer %d: %v", w, err)
				}
			})
		}
		wg.Wait()

		found := queryArtifacts(t, store, persistence.ArtifactFilter{AgentID: distinctAgent})
		check.Len(t, found, writers, "records of the distinct agent")
		for w := range writers {
			got := getArtifact(t, store, distinct[w].ID)
			check.Equal(t, got.Name, distinct[w].Name, "name of %s", distinct[w].ID)
			check.True(t, slices.Contains(artifactNames(found), distinct[w].Name), "%s is missing from the query", distinct[w].Name)
			check.DeepEqual(t, versionNumbers(listArtifactVersions(t, store, distinct[w].ID)), []int{1}, "version log of %s", distinct[w].ID)
		}

		// One id holds exactly one head record and, as Create replaces the log, one
		// version; both are one writer's, whole.
		check.Len(t, queryArtifacts(t, store, persistence.ArtifactFilter{AgentID: sameAgent}), 1, "records of the same-id agent")
		got := getArtifact(t, store, sameID)
		check.True(t, slices.Contains(descriptions, got.Description), "head description %q, want one writer's", got.Description)
		versions := listArtifactVersions(t, store, sameID)
		check.DeepEqual(t, versionNumbers(versions), []int{1}, "version log of %s", sameID)
		if len(versions) == 1 {
			check.True(t, slices.Contains(descriptions, versions[0].Note), "version note %q, want one writer's", versions[0].Note)
		}
	})
}
