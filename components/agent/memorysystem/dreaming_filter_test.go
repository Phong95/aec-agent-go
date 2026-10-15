package memorysystem

import (
	"testing"
	"time"

	"github.com/Phong95/aec-agent-go/components/persistence"
	"github.com/Phong95/aec-agent-go/kernel"
)

// ---------------------------------------------------------------------------
// Consolidation drops tool calls and tool results before the memory extraction. These
// are its two message-filter cases. The test runs the real memory component over a
// fake model and reads the extraction chat input, so the all-tool-messages case shows
// as no model call.
// ---------------------------------------------------------------------------

const filterSession = "session-1"

var filterMessages = []kernel.Message{
	{Role: kernel.RoleUser, Content: "I prefer metric units"},
	{Role: kernel.RoleAssistant, Content: "Let me check", ToolCalls: []kernel.ToolCall{{ID: "c1", Name: "lookup"}}},
	{Role: kernel.RoleTool, Content: "lookup result", ToolResults: []kernel.ToolResult{{ToolCallID: "c1", Content: "lookup result"}}},
	{Role: kernel.RoleUser, Content: "", ToolResults: []kernel.ToolResult{{ToolCallID: "c2", Content: "inline result"}}},
	{Role: kernel.RoleAssistant, Content: "Noted, metric it is"},
}

func TestDreamingMessageFilter(t *testing.T) {
	setup := func(t *testing.T, stored []kernel.Message) (*dreaming, *fakeModel, func() int) {
		t.Helper()
		ctx := t.Context()
		messages := persistence.NewMemoryMessageStore()
		if err := messages.Set(ctx, filterSession, stored); err != nil {
			t.Fatal(err)
		}
		sessions := persistence.NewMemorySessionStore()
		if err := sessions.Update(ctx, kernel.Session{ID: filterSession, CreatedAt: 1000, UpdatedAt: 2000}); err != nil {
			t.Fatal(err)
		}
		store := seedStore(t)
		model := newFakeModel()
		d := newTestDeps("agent-1", WithClock(fixedClock(expiryNowMs)), WithLocation(time.UTC))
		dr := newDreaming(d, messages, sessions, store, newMemory(d, model, store), nil)
		checkpoint := func() int {
			n, err := dr.consolidatedMessageCount(ctx, filterSession)
			if err != nil {
				t.Fatal(err)
			}
			return n
		}
		return dr, model, checkpoint
	}

	t.Run("sends only messages without tool calls or tool results to extraction", func(t *testing.T) {
		dr, model, checkpoint := setup(t, filterMessages)

		if _, err := dr.consolidate(t.Context(), filterSession); err != nil {
			t.Fatal(err)
		}

		calls := model.extractionCalls()
		if len(calls) != 1 {
			t.Fatalf("%d extraction calls, want 1", len(calls))
		}
		want := "[user]: I prefer metric units\n[assistant]: Noted, metric it is"
		if got := calls[0][1].Content; got != want {
			t.Errorf("chat input = %q, want %q", got, want)
		}
		// The checkpoint still moves past the dropped tool messages.
		if got := checkpoint(); got != len(filterMessages) {
			t.Errorf("checkpoint = %d, want %d", got, len(filterMessages))
		}
	})

	t.Run("advances the checkpoint when every new message is a tool message", func(t *testing.T) {
		dr, model, checkpoint := setup(t, filterMessages[1:4])

		if _, err := dr.consolidate(t.Context(), filterSession); err != nil {
			t.Fatal(err)
		}

		if calls := model.extractionCalls(); len(calls) != 0 {
			t.Errorf("%d extraction calls, want 0", len(calls))
		}
		if got := checkpoint(); got != 3 {
			t.Errorf("checkpoint = %d, want 3", got)
		}
	})
}
