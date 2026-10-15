package skillhub_test

import (
	"slices"
	"testing"

	"github.com/Phong95/aec-agent-go/components/events"
	"github.com/Phong95/aec-agent-go/components/persistence"
	"github.com/Phong95/aec-agent-go/components/skillhub"
	"github.com/Phong95/aec-agent-go/components/skillhub/skill"
	"github.com/Phong95/aec-agent-go/kernel"
)

// TestSkillHubWithEventsEmitsSkillDeleted: with WithEvents, Delete emits skill:deleted
// (SessionID "") after the OnDeleted listeners; without it, nothing is emitted.
func TestSkillHubWithEventsEmitsSkillDeleted(t *testing.T) {
	t.Run("emits after the OnDeleted listeners", func(t *testing.T) {
		bus := events.New()
		hub := newHub(persistence.NewMemorySkillStore(), skillhub.WithEvents(bus))
		var order []string
		hub.OnDeleted(func(name string) { order = append(order, "listener:"+name) })
		events.Subscribe(bus, events.SkillDeleted, func(e kernel.Event, p events.SkillDeletedPayload) {
			if e.SessionID != "" {
				t.Errorf("SessionID = %q, want \"\"", e.SessionID)
			}
			order = append(order, "event:"+p.Name)
		})
		mustCreate(t, hub, skill.Package{Content: md("site-diary")}, skillhub.CreateOptions{})

		if err := hub.Delete(t.Context(), "site-diary"); err != nil {
			t.Fatalf("Delete: %v", err)
		}
		if want := []string{"listener:site-diary", "event:site-diary"}; !slices.Equal(order, want) {
			t.Fatalf("order = %v, want %v", order, want)
		}
	})

	t.Run("a failed delete emits nothing", func(t *testing.T) {
		bus := events.New()
		hub := newHub(persistence.NewMemorySkillStore(), skillhub.WithEvents(bus))
		var got int
		bus.OnAny(func(kernel.Event) { got++ })

		if err := hub.Delete(t.Context(), "missing"); err == nil {
			t.Fatal("Delete of a missing skill succeeded")
		}
		if got != 0 {
			t.Fatalf("%d events emitted, want 0", got)
		}
	})

	t.Run("without WithEvents nothing is emitted", func(t *testing.T) {
		bus := events.New()
		hub := newHub(persistence.NewMemorySkillStore())
		var got int
		bus.OnAny(func(kernel.Event) { got++ })
		mustCreate(t, hub, skill.Package{Content: md("site-diary")}, skillhub.CreateOptions{})

		if err := hub.Delete(t.Context(), "site-diary"); err != nil {
			t.Fatalf("Delete: %v", err)
		}
		if got != 0 {
			t.Fatalf("%d events emitted, want 0", got)
		}
	})
}
