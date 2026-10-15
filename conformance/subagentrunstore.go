package conformance

import (
	"testing"

	"github.com/Phong95/aec-agent-go/components/agent/subagent"
	"github.com/Phong95/aec-agent-go/components/persistence"
	"github.com/Phong95/aec-agent-go/conformance/internal/check"
	"github.com/Phong95/aec-agent-go/kernel"
)

// SubagentRunStoreFactory returns a persistence.SubagentRunStore. See the package comment
// for the freshness rules of the store suites.
type SubagentRunStoreFactory func(t *testing.T) persistence.SubagentRunStore

// subagentRunFixture builds a finished run with unique ids, changed by mut.
func subagentRunFixture(mut func(*subagent.Run)) subagent.Run {
	r := subagent.Run{
		ID:              kernel.NewID(),
		Kind:            subagent.KindAgent,
		ParentAgentID:   uniqueAgentID(),
		ParentSessionID: kernel.NewID(),
		ParentRunID:     kernel.NewID(),
		ChildAgentID:    uniqueAgentID(),
		ChildAgentName:  "Researcher",
		Access:          subagent.AccessReadOnly,
		Task:            "Find the fire rating of wall W1",
		Status:          subagent.StatusCompleted,
		Usage:           kernel.TokenUsage{InputTokens: 10, OutputTokens: 5},
		StartedAt:       "2026-10-13T00:00:00.000Z",
	}
	if mut != nil {
		mut(&r)
	}
	return r
}

func subagentRunIDs(rs []subagent.Run) []string {
	out := make([]string, len(rs))
	for i, r := range rs {
		out[i] = r.ID
	}
	return out
}

// SubagentRunStore runs the conformance suite for persistence.SubagentRunStore
// implementations (A-105), the testSubagentRunStoreConformance suite of core/ts. It
// checks the invariants the subagent manager and the parent's chat rely on: runs
// round-trip with every field (usage, output, error), save replaces (a run is saved when
// it starts and again when it ends), lists are scoped per parent session and ordered
// oldest first, records are copied in and out, and DeleteByAgent removes only the runs
// that agent started. An implementation that passes it can be plugged in through
// persistence.WithSubagentRuns without changing anything else.
func SubagentRunStore(t *testing.T, newStore SubagentRunStoreFactory) {
	t.Helper()

	fresh := func(t *testing.T) persistence.SubagentRunStore {
		t.Helper()
		s := newStore(t)
		check.NoErr(t, kernel.Initialize(t.Context(), s))
		return s
	}
	save := func(t *testing.T, s persistence.SubagentRunStore, r subagent.Run) {
		t.Helper()
		check.NoErr(t, s.Save(t.Context(), r), "save %s", r.ID)
	}
	get := func(t *testing.T, s persistence.SubagentRunStore, id string) *subagent.Run {
		t.Helper()
		got, err := s.Get(t.Context(), id)
		check.NoErr(t, err, "get %s", id)
		return got
	}
	list := func(t *testing.T, s persistence.SubagentRunStore, parentSessionID string) []subagent.Run {
		t.Helper()
		out, err := s.ListByParentSession(t.Context(), parentSessionID)
		check.NoErr(t, err, "list %s", parentSessionID)
		return out
	}

	t.Run("initialize and dispose return no error", func(t *testing.T) {
		s := newStore(t)
		check.NoErr(t, kernel.Initialize(t.Context(), s))
		check.NoErr(t, kernel.Dispose(t.Context(), s))
	})

	t.Run("save() + get() round-trip every field", func(t *testing.T) {
		s := fresh(t)
		cacheRead, cacheWrite := 7, 3
		duration := int64(5000)
		full := subagentRunFixture(func(r *subagent.Run) {
			r.Kind = subagent.KindType
			r.ParentToolCallID = "tc-1"
			r.ChildSessionID = kernel.NewID()
			r.Access = subagent.AccessWrite
			r.Status = subagent.StatusPartial
			r.Output = "half of the report"
			r.Usage = kernel.TokenUsage{InputTokens: 120, OutputTokens: 40, CacheReadTokens: &cacheRead, CacheWriteTokens: &cacheWrite}
			r.ToolCalls = 4
			r.Error = "the subagent reached its turn limit (maxIterations)"
			r.FinishedAt = "2026-10-13T00:00:05.000Z"
			r.DurationMs = &duration
		})
		bare := subagentRunFixture(nil)
		save(t, s, full)
		save(t, s, bare)
		for _, want := range []subagent.Run{full, bare} {
			got := get(t, s, want.ID)
			if got == nil {
				t.Fatalf("run %s is absent", want.ID)
			}
			check.DeepEqual(t, *got, want, "round-trip of %s", want.ID)
		}
	})

	t.Run("get() returns nil for an unknown id", func(t *testing.T) {
		s := fresh(t)
		check.True(t, get(t, s, kernel.NewID()) == nil, "unknown id must be nil")
	})

	t.Run("save() replaces a running record with its finished one, dropping fields it no longer has", func(t *testing.T) {
		s := fresh(t)
		running := subagentRunFixture(func(r *subagent.Run) {
			r.Status = subagent.StatusRunning
			r.Error = "stale"
		})
		save(t, s, running)
		duration := int64(60_000)
		finished := running.Clone()
		finished.Error = ""
		finished.Status = subagent.StatusCompleted
		finished.Output = "done"
		finished.FinishedAt = "2026-10-13T00:01:00.000Z"
		finished.DurationMs = &duration
		save(t, s, finished)

		got := get(t, s, running.ID)
		if got == nil {
			t.Fatal("run is absent")
		}
		check.DeepEqual(t, *got, finished, "replaced run")
		check.Len(t, list(t, s, running.ParentSessionID), 1)
	})

	t.Run("listByParentSession() returns only that session's runs, oldest started first", func(t *testing.T) {
		s := fresh(t)
		parentSessionID := kernel.NewID()
		second := subagentRunFixture(func(r *subagent.Run) {
			r.ParentSessionID = parentSessionID
			r.StartedAt = "2026-10-13T00:00:02.000Z"
		})
		first := subagentRunFixture(func(r *subagent.Run) {
			r.ParentSessionID = parentSessionID
			r.StartedAt = "2026-10-13T00:00:01.000Z"
		})
		other := subagentRunFixture(nil)
		for _, r := range []subagent.Run{second, first, other} {
			save(t, s, r)
		}
		check.DeepEqual(t, subagentRunIDs(list(t, s, parentSessionID)), []string{first.ID, second.ID}, "runs of the session")
		check.Len(t, list(t, s, kernel.NewID()), 0)
	})

	t.Run("returned runs are copies: mutating one does not change the store", func(t *testing.T) {
		s := fresh(t)
		cacheRead := 2
		original := subagentRunFixture(func(r *subagent.Run) { r.Usage.CacheReadTokens = &cacheRead })
		save(t, s, original)

		got := get(t, s, original.ID)
		if got == nil {
			t.Fatal("run is absent")
		}
		got.Usage.InputTokens = 999
		*got.Usage.CacheReadTokens = 999
		got.Task = "changed"
		listed := list(t, s, original.ParentSessionID)
		check.Len(t, listed, 1)
		listed[0].Task = "changed too"

		again := get(t, s, original.ID)
		if again == nil {
			t.Fatal("run is absent")
		}
		check.DeepEqual(t, *again, original, "stored run after mutating copies")
	})

	t.Run("deleteByAgent() removes only the runs that agent started", func(t *testing.T) {
		s := fresh(t)
		parentAgentID := uniqueAgentID()
		mine := subagentRunFixture(func(r *subagent.Run) { r.ParentAgentID = parentAgentID })
		asChild := subagentRunFixture(func(r *subagent.Run) { r.ChildAgentID = parentAgentID })
		save(t, s, mine)
		save(t, s, asChild)

		check.NoErr(t, s.DeleteByAgent(t.Context(), parentAgentID))

		check.True(t, get(t, s, mine.ID) == nil, "the agent's run must be deleted")
		got := get(t, s, asChild.ID)
		if got == nil {
			t.Fatal("a run where the agent was the child must stay")
		}
		check.DeepEqual(t, *got, asChild, "the other run")
	})
}
