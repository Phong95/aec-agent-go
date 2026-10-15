package subagenthub_test

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/Phong95/aec-agent-go/components/agent/subagent"
	"github.com/Phong95/aec-agent-go/components/events"
	"github.com/Phong95/aec-agent-go/components/persistence"
	"github.com/Phong95/aec-agent-go/components/subagenthub"
	"github.com/Phong95/aec-agent-go/components/subagenthub/subagenttype"
	"github.com/Phong95/aec-agent-go/kernel"
)

// The "SubagentHub" part of tests/unit/subagent-types.test.ts of core/ts.

func codeOf(err error) subagent.ErrorCode {
	var se *subagent.Error
	if errors.As(err, &se) {
		return se.Code
	}
	return ""
}

func intPtr(n int) *int { return &n }

func TestSubagentHubValidatesKeepsRegisteredTypesReadOnlyAndPersistsCustomOnes(t *testing.T) {
	ctx := t.Context()
	store := persistence.NewMemorySubagentTypeStore()
	hub := subagenthub.NewSubagentHub(func() persistence.SubagentTypeStore { return store })
	if _, err := hub.Register(subagenttype.Definition{Name: "builtin", Description: "d", SystemPrompt: "p"}, subagenttype.SourceBuiltIn); err != nil {
		t.Fatal(err)
	}
	if _, err := hub.Create(ctx, subagenttype.Definition{Name: "Bad Name", Description: "d", SystemPrompt: "p"}); codeOf(err) != subagent.CodeInvalidArgument {
		t.Fatalf("bad name: %v", err)
	}
	if _, err := hub.Create(ctx, subagenttype.Definition{Name: "x", Description: "", SystemPrompt: "p"}); err == nil || !strings.Contains(err.Error(), "description") {
		t.Fatalf("no description: %v", err)
	}
	if _, err := hub.Create(ctx, subagenttype.Definition{Name: "builtin", Description: "d", SystemPrompt: "p"}); codeOf(err) != subagent.CodeAlreadyExists {
		t.Fatalf("taken name: %v", err)
	}
	if _, err := hub.Update(ctx, "builtin", subagenttype.Definition{Description: "d", SystemPrompt: "p"}); err == nil || !strings.Contains(err.Error(), "can't be changed") {
		t.Fatalf("update built-in: %v", err)
	}
	if _, err := hub.Create(ctx, subagenttype.Definition{Name: "custom", Description: "d", SystemPrompt: "p", MaxIterations: intPtr(500), Tools: []string{"a", "a"}}); err != nil {
		t.Fatal(err)
	}
	stored, err := store.List(ctx)
	if err != nil || len(stored) != 1 {
		t.Fatalf("stored = %+v (%v)", stored, err)
	}
	if s := stored[0]; s.Name != "custom" || s.Access != subagent.AccessReadOnly || s.MaxIterations == nil || *s.MaxIterations != 100 ||
		!slices.Equal(s.Tools, []string{"a"}) || s.Source != subagenttype.SourceCustom || s.CreatedAt == "" {
		t.Fatalf("stored = %+v", s)
	}
	updated, err := hub.Update(ctx, "custom", subagenttype.Definition{Description: "new", SystemPrompt: "p2", Access: subagent.AccessWrite})
	if err != nil || updated.Name != "custom" || updated.Description != "new" || updated.Access != subagent.AccessWrite || updated.CreatedAt != stored[0].CreatedAt {
		t.Fatalf("updated = %+v (%v)", updated, err)
	}
	all, err := hub.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, x := range all {
		names = append(names, string(x.Source)+":"+x.Name)
	}
	if !slices.Equal(names, []string{"built-in:builtin", "custom:custom"}) {
		t.Fatalf("list = %v", names)
	}
	// A fresh hub over the same store sees it (after a restart).
	fresh, ok, err := subagenthub.NewSubagentHub(func() persistence.SubagentTypeStore { return store }).Get(ctx, "custom")
	if err != nil || !ok || fresh.Description != "new" {
		t.Fatalf("fresh hub: %+v %v %v", fresh, ok, err)
	}
	if err := hub.Delete(ctx, "custom"); err != nil {
		t.Fatal(err)
	}
	if left, _ := store.List(ctx); len(left) != 0 {
		t.Fatalf("store after delete = %+v", left)
	}
	if err := hub.Delete(ctx, "custom"); codeOf(err) != subagent.CodeNotFound {
		t.Fatalf("second delete: %v", err)
	}
}

func TestSubagentHubToTypeNormalises(t *testing.T) {
	got, err := subagenthub.ToType(subagenttype.Definition{
		Name: "  checker ", Description: " d ", SystemPrompt: " p ", Tools: []string{}, Provider: " ", Model: " m ", MaxIterations: intPtr(0),
	}, subagenttype.SourceApp)
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "checker" || got.Description != "d" || got.SystemPrompt != "p" || got.Tools == nil || len(got.Tools) != 0 ||
		got.Provider != "" || got.Model != "m" || *got.MaxIterations != 1 || got.Access != subagent.AccessReadOnly || got.Source != subagenttype.SourceApp {
		t.Fatalf("type = %+v", got)
	}
	for _, def := range []subagenttype.Definition{
		{Name: "x", Description: "d", SystemPrompt: ""},
		{Name: "x", Description: "d", SystemPrompt: "p", Tools: []string{" "}},
		{Name: "x", Description: "d", SystemPrompt: "p", Access: "admin"},
		{Name: strings.Repeat("a", 65), Description: "d", SystemPrompt: "p"},
		{Name: "-x", Description: "d", SystemPrompt: "p"},
	} {
		if _, err := subagenthub.ToType(def, subagenttype.SourceCustom); codeOf(err) != subagent.CodeInvalidArgument {
			t.Errorf("ToType(%+v) = %v, want invalid_argument", def, err)
		}
	}
	if _, err := subagenthub.NewSubagentHub(nil).Register(subagenttype.Definition{Name: "x", Description: "d", SystemPrompt: "p"}, subagenttype.SourceCustom); err == nil {
		t.Fatal("registered a custom type")
	}
}

func TestSubagentHubDeletePublishesAndReloadReadsTheNewStore(t *testing.T) {
	ctx := t.Context()
	bus := events.New()
	var deleted []string
	events.SubscribeCtx(bus, events.SubagentTypeDeleted, func(_ context.Context, e kernel.Event, p events.SubagentTypeDeletedPayload) {
		deleted = append(deleted, e.SessionID+"|"+p.Name)
	})
	first, second := persistence.NewMemorySubagentTypeStore(), persistence.NewMemorySubagentTypeStore()
	current := first
	hub := subagenthub.NewSubagentHub(func() persistence.SubagentTypeStore { return current }, subagenthub.WithEvents(bus))
	if _, err := hub.Create(ctx, subagenttype.Definition{Name: "temp", Description: "d", SystemPrompt: "p"}); err != nil {
		t.Fatal(err)
	}
	if err := hub.Delete(ctx, "temp"); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(deleted, []string{"|temp"}) {
		t.Fatalf("deleted events = %v", deleted)
	}
	if err := second.Save(ctx, subagenttype.Type{Definition: subagenttype.Definition{Name: "other", Description: "d", SystemPrompt: "p", Access: subagent.AccessReadOnly}}); err != nil {
		t.Fatal(err)
	}
	current = second
	if _, ok, _ := hub.Get(ctx, "other"); ok {
		t.Fatal("the cache saw the new store before Reload")
	}
	hub.Reload()
	got, ok, err := hub.Get(ctx, "other")
	if err != nil || !ok || got.Source != subagenttype.SourceCustom {
		t.Fatalf("after Reload: %+v %v %v", got, ok, err)
	}
	// Built-in types are what every server has.
	builtIns := subagenthub.BuiltInTypes()
	if len(builtIns) != 2 || builtIns[0].Name != "general-purpose" || builtIns[1].Name != "researcher" {
		t.Fatalf("built-ins = %+v", builtIns)
	}
}

func TestSubagentTypeWireShape(t *testing.T) {
	raw, err := json.Marshal(subagenttype.Type{
		Definition: subagenttype.Definition{Name: "x", Description: "d", SystemPrompt: "p", Tools: []string{}, Access: subagent.AccessReadOnly, MaxIterations: intPtr(3)},
		Source:     subagenttype.SourceCustom,
		CreatedAt:  "c",
	})
	if err != nil {
		t.Fatal(err)
	}
	// The SubagentType of core/ts: an empty tools list stays (no tools), nil is left out (all).
	want := `{"name":"x","description":"d","systemPrompt":"p","tools":[],"access":"read-only","maxIterations":3,"source":"custom","createdAt":"c"}`
	if string(raw) != want {
		t.Fatalf("json = %s\nwant   %s", raw, want)
	}
	var back subagenttype.Type
	if err := json.Unmarshal([]byte(`{"name":"y","description":"d","systemPrompt":"p","access":"write","source":"built-in"}`), &back); err != nil {
		t.Fatal(err)
	}
	if back.Name != "y" || back.Tools != nil || back.Access != subagent.AccessWrite || back.Source != subagenttype.SourceBuiltIn {
		t.Fatalf("decoded = %+v", back)
	}
}
