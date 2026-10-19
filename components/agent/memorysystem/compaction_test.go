package memorysystem

import (
	"context"
	"strings"
	"testing"

	"github.com/Phong95/aec-agent-go/components/agent/memorysystem/memory"
	"github.com/Phong95/aec-agent-go/components/events"
	"github.com/Phong95/aec-agent-go/components/llmhub"
	"github.com/Phong95/aec-agent-go/components/persistence"
	"github.com/Phong95/aec-agent-go/kernel"
	"github.com/Phong95/aec-agent-go/kernel/ordered"
)

// After a consolidation pass: update compacted user memory (USER.md, gated by
// Config.UpdateUser) and update compacted memory (MEMORY.md).

const compactionExtraction = `[{"kind":"fact","scope":"user","subject":"user","key":"profession","value":"BIM specialist","confidence":"explicit"}]`

func newCompactionSystem(t *testing.T, cfg memory.Config, extraction string) (*System, *fakeModel, string, *CompactedMemoryFiles) {
	t.Helper()
	hub := persistence.NewPersistenceHub()
	session, err := hub.Sessions.Create(t.Context(), ordered.Object{})
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range []kernel.Message{
		{Role: kernel.RoleUser, Content: "I am a BIM specialist"},
		{Role: kernel.RoleAssistant, Content: "ok"},
	} {
		if err := hub.Messages.Append(t.Context(), session.ID, m); err != nil {
			t.Fatal(err)
		}
	}
	model := newFakeModel()
	userMd, memoryMd, soulMd := "old user", "old memory", "soul"
	got := &CompactedMemoryFiles{}
	scripted := &scriptedCompaction{fakeModel: model, extraction: extraction}
	sys, err := New(Deps{
		AgentID: "a1", Persistence: hub, Events: events.New(), Model: scripted,
		MemoryFiles: func() CompactedMemoryFiles {
			return CompactedMemoryFiles{UserMd: &userMd, MemoryMd: &memoryMd, SoulMd: &soulMd}
		},
		OnMemoryFilesCompacted: func(_ context.Context, files CompactedMemoryFiles) error {
			*got = files
			return nil
		},
	}, cfg)
	if err != nil {
		t.Fatal(err)
	}
	return sys, model, session.ID, got
}

// scriptedCompaction routes each call by its system prompt and records it in fakeModel.
type scriptedCompaction struct {
	*fakeModel
	extraction string
}

func (s *scriptedCompaction) Chat(ctx context.Context, msgs []kernel.Message, opts llmhub.ModelOptions) (kernel.ModelResponse, error) {
	resp, err := s.fakeModel.Chat(ctx, msgs, opts) // records the call
	if err != nil {
		return resp, err
	}
	switch {
	case isExtractionCall(msgs):
		resp.Message.Content = s.extraction
	case strings.Contains(msgs[0].Content, "Build a concise, useful profile of me"):
		resp.Message.Content = "```markdown\n# Profile\n- BIM specialist\n```"
	case strings.Contains(msgs[0].Content, "Refine the agent's soul"):
		resp.Message.Content = "You are concise."
	default:
		resp.Message.Content = "- profession: BIM specialist"
	}
	return resp, nil
}

func compactionPrompts(m *fakeModel) []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []string
	for _, c := range m.calls {
		if len(c) > 1 && !isExtractionCall(c) {
			out = append(out, c[1].Content)
		}
	}
	return out
}

func TestConsolidate_CompactsUserAndMemoryFiles(t *testing.T) {
	sys, model, sessionID, got := newCompactionSystem(t, memory.Config{Enabled: true, UpdateUser: true}, compactionExtraction)
	if _, err := sys.Consolidate(t.Context(), sessionID); err != nil {
		t.Fatal(err)
	}
	if got.UserMd == nil || *got.UserMd != "# Profile\n- BIM specialist" {
		t.Errorf("USER.md = %v, want the unfenced profile", deref(got.UserMd))
	}
	if got.MemoryMd == nil || *got.MemoryMd != "- profession: BIM specialist" {
		t.Errorf("MEMORY.md = %v, want the compacted row", deref(got.MemoryMd))
	}
	prompts := compactionPrompts(model)
	if len(prompts) != 2 || !strings.Contains(prompts[0], "CURRENT USER.md:\nold user") || !strings.Contains(prompts[0], "profession: BIM specialist") {
		t.Errorf("compaction prompts = %q, want USER.md then MEMORY.md with the current file and the saved memories", prompts)
	}
}

func TestConsolidate_LeavesUserFileAloneWhenUpdateUserIsOff(t *testing.T) {
	sys, _, sessionID, got := newCompactionSystem(t, memory.Config{Enabled: true}, compactionExtraction)
	if _, err := sys.Consolidate(t.Context(), sessionID); err != nil {
		t.Fatal(err)
	}
	if got.UserMd != nil {
		t.Errorf("USER.md = %q, want it not rebuilt", *got.UserMd)
	}
	if got.MemoryMd == nil {
		t.Error("MEMORY.md not rebuilt")
	}
}

func TestConsolidate_RefinesSoulOnlyWhenUpdateSoulIsOn(t *testing.T) {
	sys, _, sessionID, got := newCompactionSystem(t, memory.Config{Enabled: true}, compactionExtraction)
	if _, err := sys.Consolidate(t.Context(), sessionID); err != nil {
		t.Fatal(err)
	}
	if got.MemoryMd == nil || got.SoulMd != nil {
		t.Errorf("files = %+v, want MEMORY.md rebuilt and SOUL.md not", got)
	}

	sys, model, sessionID, got := newCompactionSystem(t, memory.Config{Enabled: true, UpdateSoul: true}, compactionExtraction)
	if _, err := sys.Consolidate(t.Context(), sessionID); err != nil {
		t.Fatal(err)
	}
	if got.SoulMd == nil || *got.SoulMd != "You are concise." {
		t.Errorf("SOUL.md = %v, want the refined soul", deref(got.SoulMd))
	}
	prompts := compactionPrompts(model)
	if len(prompts) != 2 || !strings.Contains(prompts[1], "CURRENT SOUL.md:\nsoul") {
		t.Errorf("compaction prompts = %q, want MEMORY.md then SOUL.md with the current soul", prompts)
	}
}

func TestConsolidate_DoesNotCompactWhenNothingChanged(t *testing.T) {
	sys, model, sessionID, got := newCompactionSystem(t, memory.Config{Enabled: true, UpdateUser: true}, "[]")
	if _, err := sys.Consolidate(t.Context(), sessionID); err != nil {
		t.Fatal(err)
	}
	if n := len(compactionPrompts(model)); n != 0 || got.UserMd != nil || got.MemoryMd != nil {
		t.Errorf("%d compaction calls, files %+v, want none", n, got)
	}
}

func TestStripMarkdownFence(t *testing.T) {
	for in, want := range map[string]string{
		"```markdown\n# A\n```": "# A",
		"```md\n# A\n```":       "# A",
		"```\n# A\n```":         "# A",
		"  # A  ":               "# A",
		"# A ```b```":           "# A ```b```",
	} {
		if got := stripMarkdownFence(in); got != want {
			t.Errorf("stripMarkdownFence(%q) = %q, want %q", in, got, want)
		}
	}
}
