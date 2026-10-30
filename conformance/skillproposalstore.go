package conformance

import (
	"testing"

	"github.com/Phong95/aec-agent-go/components/agent/skilllearning/learning"
	"github.com/Phong95/aec-agent-go/components/persistence"
	"github.com/Phong95/aec-agent-go/components/skillhub/skill"
	"github.com/Phong95/aec-agent-go/conformance/internal/check"
	"github.com/Phong95/aec-agent-go/kernel"
)

// SkillProposalStoreFactory returns a persistence.SkillProposalStore. See the package
// comment for the freshness rules of the store suites.
type SkillProposalStoreFactory func(t *testing.T) persistence.SkillProposalStore

// skillProposalFixture builds a pending create proposal with unique ids, changed by mut.
func skillProposalFixture(mut func(*learning.Proposal)) learning.Proposal {
	p := learning.Proposal{
		ID:        kernel.NewID(),
		AgentID:   uniqueAgentID(),
		Action:    learning.ActionCreate,
		SkillName: "ifc-door-schedule",
		Summary:   "Export a door schedule from an IFC model to Excel.",
		Draft: &skill.Package{
			Content: "---\nname: ifc-door-schedule\ndescription: Export a door schedule.\n---\n\n## Steps\n1. Do it.\n",
			Files:   []skill.File{{Path: "references/notes.md", Content: "notes"}},
		},
		Episode: learning.EpisodeRef{
			SessionID: "session-1", MessageStart: 4, MessageEnd: 30, ToolCalls: 12, FailedCalls: 3,
			SkillsUsed: []string{"plan"}, Signal: learning.SignalNextMessage,
			Request: "Export a door schedule from tower-B.ifc", EndedAt: "2026-11-02T10:00:00.000Z",
		},
		Status:    learning.StatusPending,
		CreatedAt: "2026-11-02T10:01:00.000Z",
		UpdatedAt: "2026-11-02T10:01:00.000Z",
	}
	if mut != nil {
		mut(&p)
	}
	return p
}

func learnedSkillFixture(agentID, name string) learning.LearnedSkill {
	return learning.LearnedSkill{
		AgentID: agentID, SkillName: name, ProposalID: "p-1", Health: 3,
		CreatedAt: "2026-11-02T10:05:00.000Z", UpdatedAt: "2026-11-02T10:05:00.000Z",
	}
}

func proposalIDs(ps []learning.Proposal) []string {
	out := make([]string, len(ps))
	for i, p := range ps {
		out[i] = p.ID
	}
	return out
}

// SkillProposalStore runs the conformance suite for persistence.SkillProposalStore
// implementations (A-166 skill learning), the testSkillProposalStoreConformance suite of
// core/ts. It checks round-tripping (with the A-174 verification), per-agent listing
// (newest first, status filter), learned-skill records and deletion by agent. Each test
// uses fresh agent ids, so a shared database works. An implementation that passes it can
// be plugged in through persistence.WithSkillProposals.
func SkillProposalStore(t *testing.T, newStore SkillProposalStoreFactory) {
	t.Helper()

	fresh := func(t *testing.T) persistence.SkillProposalStore {
		t.Helper()
		s := newStore(t)
		check.NoErr(t, kernel.Initialize(t.Context(), s))
		return s
	}
	save := func(t *testing.T, s persistence.SkillProposalStore, p learning.Proposal) {
		t.Helper()
		check.NoErr(t, s.Save(t.Context(), p), "save %s", p.ID)
	}
	get := func(t *testing.T, s persistence.SkillProposalStore, id string) *learning.Proposal {
		t.Helper()
		got, err := s.Get(t.Context(), id)
		check.NoErr(t, err, "get %s", id)
		return got
	}
	list := func(t *testing.T, s persistence.SkillProposalStore, agentID string, status learning.ProposalStatus) []learning.Proposal {
		t.Helper()
		out, err := s.List(t.Context(), agentID, learning.ProposalFilter{Status: status})
		check.NoErr(t, err, "list %s", agentID)
		return out
	}

	t.Run("save() then get() round-trips every field", func(t *testing.T) {
		s := fresh(t)
		prev := "old"
		p := skillProposalFixture(func(p *learning.Proposal) {
			p.PreviousContent = &prev
			p.Auto = true
			p.Reason = "x"
			p.DecidedAt = "2026-11-02T11:00:00.000Z"
			p.Verification = &learning.Verification{
				Passed: false, Solved: true, SkillUsed: true, ChangedEnvironment: false, Reason: "nothing changed",
				ToolCalls: 4, FailedCalls: 1, At: "2026-11-02T10:30:00.000Z",
			}
		})
		save(t, s, p)
		got := get(t, s, p.ID)
		if got == nil {
			t.Fatal("proposal is absent")
		}
		check.DeepEqual(t, *got, p, "round-trip")
	})

	t.Run("round-trips a retire proposal without a draft", func(t *testing.T) {
		s := fresh(t)
		prev := "old body"
		p := skillProposalFixture(func(p *learning.Proposal) {
			p.Action = learning.ActionRetire
			p.Draft = nil
			p.PreviousContent = &prev
		})
		save(t, s, p)
		got := get(t, s, p.ID)
		if got == nil {
			t.Fatal("proposal is absent")
		}
		check.DeepEqual(t, *got, p, "round-trip")
	})

	t.Run("get() returns nil for an unknown id", func(t *testing.T) {
		s := fresh(t)
		check.True(t, get(t, s, kernel.NewID()) == nil, "unknown id must be nil")
	})

	t.Run("save() replaces the proposal with the same id", func(t *testing.T) {
		s := fresh(t)
		p := skillProposalFixture(nil)
		save(t, s, p)
		decided := p.Clone()
		decided.Status = learning.StatusRejected
		decided.Reason = "too specific"
		decided.DecidedAt = "2026-11-03T00:00:00.000Z"
		decided.UpdatedAt = "2026-11-03T00:00:00.000Z"
		save(t, s, decided)
		got := get(t, s, p.ID)
		if got == nil {
			t.Fatal("proposal is absent")
		}
		check.DeepEqual(t, *got, decided, "replaced")
		check.Len(t, list(t, s, p.AgentID, ""), 1)
	})

	t.Run("list() returns one agent's proposals newest first, filtered by status", func(t *testing.T) {
		s := fresh(t)
		agentID := uniqueAgentID()
		older := skillProposalFixture(func(p *learning.Proposal) { p.AgentID = agentID; p.CreatedAt = "2026-11-01T00:00:00.000Z" })
		newer := skillProposalFixture(func(p *learning.Proposal) {
			p.AgentID = agentID
			p.CreatedAt = "2026-11-05T00:00:00.000Z"
			p.Status = learning.StatusAccepted
		})
		other := skillProposalFixture(nil)
		for _, p := range []learning.Proposal{older, newer, other} {
			save(t, s, p)
		}
		check.DeepEqual(t, proposalIDs(list(t, s, agentID, "")), []string{newer.ID, older.ID}, "all")
		check.DeepEqual(t, proposalIDs(list(t, s, agentID, learning.StatusPending)), []string{older.ID}, "pending")
		check.DeepEqual(t, proposalIDs(list(t, s, agentID, learning.StatusAccepted)), []string{newer.ID}, "accepted")
	})

	t.Run("delete() removes the proposal and ignores unknown ids", func(t *testing.T) {
		s := fresh(t)
		p := skillProposalFixture(nil)
		save(t, s, p)
		check.NoErr(t, s.Delete(t.Context(), p.ID))
		check.True(t, get(t, s, p.ID) == nil, "deleted proposal must be nil")
		check.NoErr(t, s.Delete(t.Context(), kernel.NewID()))
	})

	t.Run("learned-skill records round-trip, upsert by (agent, name) and list by name", func(t *testing.T) {
		s := fresh(t)
		ctx := t.Context()
		agentID := uniqueAgentID()
		b := learnedSkillFixture(agentID, "b-skill")
		b.LastUsedAt = "2026-11-04T00:00:00.000Z"
		a := learnedSkillFixture(agentID, "a-skill")
		check.NoErr(t, s.SaveLearnedSkill(ctx, b))
		check.NoErr(t, s.SaveLearnedSkill(ctx, a))
		got, err := s.GetLearnedSkill(ctx, agentID, "b-skill")
		check.NoErr(t, err)
		if got == nil {
			t.Fatal("record is absent")
		}
		check.DeepEqual(t, *got, b, "round-trip")
		used := b
		used.Health, used.Uses, used.SmoothUses, used.UpdatedAt = 4, 1, 1, "2026-11-05T00:00:00.000Z"
		check.NoErr(t, s.SaveLearnedSkill(ctx, used))
		got, err = s.GetLearnedSkill(ctx, agentID, "b-skill")
		check.NoErr(t, err)
		if got == nil {
			t.Fatal("record is absent")
		}
		check.DeepEqual(t, *got, used, "upsert")
		all, err := s.ListLearnedSkills(ctx, agentID)
		check.NoErr(t, err)
		names := []string{}
		for _, r := range all {
			names = append(names, r.SkillName)
		}
		check.DeepEqual(t, names, []string{"a-skill", "b-skill"}, "listed by name")
		none, err := s.GetLearnedSkill(ctx, uniqueAgentID(), "b-skill")
		check.NoErr(t, err)
		check.True(t, none == nil, "another agent's record must be nil")
		check.NoErr(t, s.DeleteLearnedSkill(ctx, agentID, "a-skill"))
		gone, err := s.GetLearnedSkill(ctx, agentID, "a-skill")
		check.NoErr(t, err)
		check.True(t, gone == nil, "deleted record must be nil")
		check.NoErr(t, s.DeleteLearnedSkill(ctx, agentID, "never"))
	})

	t.Run("deleteByAgent() removes only that agent's proposals and records", func(t *testing.T) {
		s := fresh(t)
		ctx := t.Context()
		goneID, keptID := uniqueAgentID(), uniqueAgentID()
		save(t, s, skillProposalFixture(func(p *learning.Proposal) { p.AgentID = goneID }))
		check.NoErr(t, s.SaveLearnedSkill(ctx, learnedSkillFixture(goneID, "x")))
		keep := skillProposalFixture(func(p *learning.Proposal) { p.AgentID = keptID })
		save(t, s, keep)
		check.NoErr(t, s.SaveLearnedSkill(ctx, learnedSkillFixture(keptID, "x")))
		check.NoErr(t, s.DeleteByAgent(ctx, goneID))
		check.Len(t, list(t, s, goneID, ""), 0)
		recs, err := s.ListLearnedSkills(ctx, goneID)
		check.NoErr(t, err)
		check.Len(t, recs, 0)
		got := get(t, s, keep.ID)
		if got == nil {
			t.Fatal("the other agent's proposal must stay")
		}
		check.DeepEqual(t, *got, keep, "kept proposal")
		recs, err = s.ListLearnedSkills(ctx, keptID)
		check.NoErr(t, err)
		check.Len(t, recs, 1)
	})

	t.Run("returned values are copies: mutating them does not change the store", func(t *testing.T) {
		s := fresh(t)
		p := skillProposalFixture(nil)
		save(t, s, p)
		got := get(t, s, p.ID)
		if got == nil {
			t.Fatal("proposal is absent")
		}
		got.Episode.SkillsUsed = append(got.Episode.SkillsUsed, "x")
		got.Draft.Content = "changed"
		again := get(t, s, p.ID)
		if again == nil {
			t.Fatal("proposal is absent")
		}
		check.DeepEqual(t, *again, p, "stored proposal after mutating a copy")
	})
}
