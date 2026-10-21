package conformance

import (
	"testing"

	"github.com/Phong95/aec-agent-go/components/grouphub/group"
	"github.com/Phong95/aec-agent-go/components/persistence"
	"github.com/Phong95/aec-agent-go/conformance/internal/check"
	"github.com/Phong95/aec-agent-go/kernel"
)

// GroupStoreFactory returns a persistence.GroupStore. See the package comment for the
// freshness rules of the store suites.
type GroupStoreFactory func(t *testing.T) persistence.GroupStore

// GroupMessageStoreFactory returns a persistence.GroupMessageStore. See the package
// comment for the freshness rules of the store suites.
type GroupMessageStoreFactory func(t *testing.T) persistence.GroupMessageStore

// groupFixture builds a group with a unique id, changed by mut.
func groupFixture(mut func(*group.Group)) group.Group {
	g := group.Group{
		ID:         kernel.NewID(),
		Name:       "Façade war room",
		Topic:      "",
		Members:    []string{"a", "b", "c"},
		LeadID:     "a",
		ReplyMode:  group.ReplyLead,
		Muted:      []string{},
		MemberView: group.ViewFull,
		Limits:     group.Limits{},
		CreatedAt:  "2026-10-21T00:00:00.000Z",
		UpdatedAt:  "2026-10-21T00:00:00.000Z",
	}
	if mut != nil {
		mut(&g)
	}
	return g
}

// groupMessageFixture builds a message of sessionID with a unique id, changed by mut.
func groupMessageFixture(sessionID string, kind group.MessageKind, text string, mut func(*group.Message)) group.Message {
	m := group.Message{
		ID:        kernel.NewID(),
		SessionID: sessionID,
		GroupID:   "g1",
		Kind:      kind,
		Text:      text,
		CreatedAt: "2026-10-21T00:00:00.000Z",
		UpdatedAt: "2026-10-21T00:00:00.000Z",
	}
	if mut != nil {
		mut(&m)
	}
	return m
}

// GroupStore runs the conformance suite for persistence.GroupStore implementations
// (A-106), the testGroupStoreConformance suite of core/ts. It checks the invariants the
// grouphub.Hub relies on: groups round-trip with every field, Get of an unknown id is
// nil, Save replaces, List is oldest first and Delete removes one group. An
// implementation that passes it can be plugged in through persistence.WithGroups.
func GroupStore(t *testing.T, newStore GroupStoreFactory) {
	t.Helper()

	fresh := func(t *testing.T) persistence.GroupStore {
		t.Helper()
		s := newStore(t)
		check.NoErr(t, kernel.Initialize(t.Context(), s))
		return s
	}
	get := func(t *testing.T, s persistence.GroupStore, id string) *group.Group {
		t.Helper()
		g, err := s.Get(t.Context(), id)
		check.NoErr(t, err, "get %s", id)
		return g
	}

	t.Run("save() + get() round-trip every field; get() of an unknown id is nil", func(t *testing.T) {
		s := fresh(t)
		full := groupFixture(func(g *group.Group) {
			g.Topic = "East elevation"
			g.Muted = []string{"b"}
			g.ReplyMode = group.ReplyAll
			g.MemberView = group.ViewAddressed
			g.Limits = group.Limits{MaxMembersPerRound: 3, MaxTokensPerTurn: 1000}
		})
		check.NoErr(t, s.Save(t.Context(), full))
		got := get(t, s, full.ID)
		if got == nil {
			t.Fatal("group is absent")
		}
		check.DeepEqual(t, *got, full, "round-trip")
		check.True(t, get(t, s, kernel.NewID()) == nil, "unknown id must be nil")
	})

	t.Run("save() replaces, list() is oldest first, delete() removes", func(t *testing.T) {
		s := fresh(t)
		first := groupFixture(func(g *group.Group) { g.CreatedAt = "2026-10-01T00:00:00.000Z" })
		second := groupFixture(func(g *group.Group) { g.CreatedAt = "2026-10-02T00:00:00.000Z" })
		check.NoErr(t, s.Save(t.Context(), second))
		check.NoErr(t, s.Save(t.Context(), first))
		renamed := first.Clone()
		renamed.Name = "renamed"
		renamed.Muted = []string{}
		check.NoErr(t, s.Save(t.Context(), renamed))
		all, err := s.List(t.Context())
		check.NoErr(t, err, "list")
		var listed []group.Group
		for _, g := range all {
			if g.ID == first.ID || g.ID == second.ID {
				listed = append(listed, g)
			}
		}
		check.Len(t, listed, 2)
		check.DeepEqual(t, []string{listed[0].ID, listed[1].ID}, []string{first.ID, second.ID}, "list order")
		check.Equal(t, listed[0].Name, "renamed", "replaced name")
		check.NoErr(t, s.Delete(t.Context(), first.ID))
		check.True(t, get(t, s, first.ID) == nil, "deleted group must be gone")
		check.True(t, get(t, s, second.ID) != nil, "the other group must stay")
	})

	t.Run("returned groups are copies: mutating one does not change the store", func(t *testing.T) {
		s := fresh(t)
		original := groupFixture(nil)
		check.NoErr(t, s.Save(t.Context(), original))
		got := get(t, s, original.ID)
		if got == nil {
			t.Fatal("group is absent")
		}
		got.Members[0] = "changed"
		got.Name = "changed"
		again := get(t, s, original.ID)
		if again == nil {
			t.Fatal("group is absent")
		}
		check.DeepEqual(t, *again, original, "stored group after mutating a copy")
	})
}

// GroupMessageStore runs the conformance suite for persistence.GroupMessageStore
// implementations (A-106), the testGroupMessageStoreConformance suite of core/ts. It
// checks the invariants the grouphub.Hub relies on: a thread's messages get Seq 1, 2,
// 3... per session in append order, Update replaces a message in place (keeping its Seq
// and session, dropping fields it no longer has), afterSeq keeps only later messages,
// and DeleteBySession removes one thread. An implementation that passes it can be
// plugged in through persistence.WithGroupMessages.
func GroupMessageStore(t *testing.T, newStore GroupMessageStoreFactory) {
	t.Helper()

	fresh := func(t *testing.T) persistence.GroupMessageStore {
		t.Helper()
		s := newStore(t)
		check.NoErr(t, kernel.Initialize(t.Context(), s))
		return s
	}
	appendMsg := func(t *testing.T, s persistence.GroupMessageStore, m group.Message) group.Message {
		t.Helper()
		out, err := s.Append(t.Context(), m)
		check.NoErr(t, err, "append %s", m.ID)
		return out
	}
	list := func(t *testing.T, s persistence.GroupMessageStore, sessionID string, afterSeq int) []group.Message {
		t.Helper()
		out, err := s.List(t.Context(), sessionID, afterSeq)
		check.NoErr(t, err, "list %s", sessionID)
		return out
	}
	ids := func(ms []group.Message) []string {
		out := []string{}
		for _, m := range ms {
			out = append(out, m.ID)
		}
		return out
	}
	texts := func(ms []group.Message) []string {
		out := []string{}
		for _, m := range ms {
			out = append(out, m.Text)
		}
		return out
	}

	t.Run("append() numbers each session's messages from 1, in order", func(t *testing.T) {
		s := fresh(t)
		a, b := kernel.NewID(), kernel.NewID()
		m1 := appendMsg(t, s, groupMessageFixture(a, group.KindUser, "hi", func(m *group.Message) { m.To = []string{"x"} }))
		other := appendMsg(t, s, groupMessageFixture(b, group.KindEvent, "note", nil))
		m2 := appendMsg(t, s, groupMessageFixture(a, group.KindAgent, "", func(m *group.Message) {
			m.AgentID = "x"
			m.Status = group.StatusRunning
			m.RoundID = "r1"
		}))
		check.DeepEqual(t, []int{m1.Seq, m2.Seq, other.Seq}, []int{1, 2, 1}, "seqs")
		check.DeepEqual(t, ids(list(t, s, a, 0)), []string{m1.ID, m2.ID}, "thread of a")
		got, err := s.Get(t.Context(), m2.ID)
		check.NoErr(t, err, "get")
		if got == nil {
			t.Fatal("message is absent")
		}
		check.DeepEqual(t, *got, m2, "get")
		unknown, err := s.Get(t.Context(), kernel.NewID())
		check.NoErr(t, err, "get unknown")
		check.True(t, unknown == nil, "unknown id must be nil")
	})

	t.Run("round-trips every field", func(t *testing.T) {
		s := fresh(t)
		sessionID := kernel.NewID()
		cacheRead := 5
		duration := int64(620)
		full := appendMsg(t, s, groupMessageFixture(sessionID, group.KindAgent, "U-value deviates", func(m *group.Message) {
			m.AgentID = "spec"
			m.RoundID = "r1"
			m.Summary = true
			m.Status = group.StatusPartial
			m.SubagentRunID = kernel.NewID()
			m.Tools = []group.ToolStep{{
				ID: "t1", Name: "read_spec", Status: group.ToolDone, Arguments: kernel.Arguments(`{"section":"08 44 13"}`),
				ResultPreview: "§2.3", DurationMs: &duration,
			}}
			m.Usage = &kernel.TokenUsage{InputTokens: 100, OutputTokens: 20, CacheReadTokens: &cacheRead}
			m.Error = "the reply hit the output token limit"
		}))
		handoff := appendMsg(t, s, groupMessageFixture(sessionID, group.KindHandoff, "check it", func(m *group.Message) {
			m.AgentID = "rfi"
			m.ToAgentID = "spec"
			m.Status = group.StatusRunning
		}))
		read := list(t, s, sessionID, 0)
		check.Len(t, read, 2)
		check.DeepEqual(t, read[0], full, "full message")
		check.DeepEqual(t, read[1], handoff, "hand-off")
	})

	t.Run("update() replaces in place, keeping seq and session, dropping fields it no longer has", func(t *testing.T) {
		s := fresh(t)
		sessionID := kernel.NewID()
		first := appendMsg(t, s, groupMessageFixture(sessionID, group.KindAgent, "", func(m *group.Message) {
			m.AgentID = "x"
			m.Status = group.StatusRunning
			m.Error = "stale"
		}))
		second := appendMsg(t, s, groupMessageFixture(sessionID, group.KindEvent, "later", nil))
		done := first.Clone()
		done.Error = ""
		done.Text = "answer"
		done.Status = group.StatusCompleted
		done.Seq = 99
		done.SessionID = "elsewhere"
		done.UpdatedAt = "2026-10-21T00:00:09.000Z"
		check.NoErr(t, s.Update(t.Context(), done))
		read := list(t, s, sessionID, 0)
		check.Len(t, read, 2)
		want := done.Clone()
		want.Seq = first.Seq
		want.SessionID = sessionID
		check.DeepEqual(t, read[0], want, "updated message")
		check.Equal(t, read[0].Error, "", "dropped error")
		check.DeepEqual(t, read[1], second, "the other message")
		unknown := done.Clone()
		unknown.ID = kernel.NewID()
		check.NoErr(t, s.Update(t.Context(), unknown)) // unknown id: no-op
		check.Len(t, list(t, s, sessionID, 0), 2)
	})

	t.Run("list() with afterSeq keeps only later messages; deleteBySession() removes one thread", func(t *testing.T) {
		s := fresh(t)
		a, b := kernel.NewID(), kernel.NewID()
		for _, text := range []string{"1", "2", "3"} {
			appendMsg(t, s, groupMessageFixture(a, group.KindEvent, text, nil))
		}
		appendMsg(t, s, groupMessageFixture(b, group.KindEvent, "kept", nil))
		check.DeepEqual(t, texts(list(t, s, a, 1)), []string{"2", "3"}, "after 1")
		check.Len(t, list(t, s, a, 3), 0)
		check.NoErr(t, s.DeleteBySession(t.Context(), a))
		check.Len(t, list(t, s, a, 0), 0)
		check.DeepEqual(t, texts(list(t, s, b, 0)), []string{"kept"}, "other thread")
		// A deleted thread starts again from 1.
		check.Equal(t, appendMsg(t, s, groupMessageFixture(a, group.KindEvent, "new", nil)).Seq, 1, "seq after delete")
	})

	t.Run("returned messages are copies: mutating one does not change the store", func(t *testing.T) {
		s := fresh(t)
		sessionID := kernel.NewID()
		original := appendMsg(t, s, groupMessageFixture(sessionID, group.KindUser, "hi", func(m *group.Message) { m.To = []string{"x"} }))
		got := list(t, s, sessionID, 0)
		got[0].To[0] = "changed"
		got[0].Text = "changed"
		check.DeepEqual(t, list(t, s, sessionID, 0)[0], original, "stored message after mutating a copy")
	})
}
