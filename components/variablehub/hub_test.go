package variablehub_test

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/Phong95/aec-agent-go/components/events"
	"github.com/Phong95/aec-agent-go/components/persistence"
	"github.com/Phong95/aec-agent-go/components/secrethub"
	"github.com/Phong95/aec-agent-go/components/secrethub/secret"
	"github.com/Phong95/aec-agent-go/components/variablehub"
	"github.com/Phong95/aec-agent-go/components/variablehub/variable"
	"github.com/Phong95/aec-agent-go/kernel"
)

func strPtr(s string) *string { return &s }

func isCode(err error, code variable.ErrorCode) bool {
	return errors.Is(err, &variable.Error{Code: code})
}

func TestHubCreatesListsUpdatesAndDeletesWithEvents(t *testing.T) {
	ctx := t.Context()
	store := persistence.NewMemoryVariableStore()
	bus := events.New()
	var seen []string
	for _, k := range []events.Kind[events.VariableChangedPayload]{events.VariableCreated, events.VariableUpdated, events.VariableDeleted} {
		kind := k
		events.Subscribe(bus, kind, func(_ kernel.Event, p events.VariableChangedPayload) {
			seen = append(seen, kind.Name+" "+p.Name)
		})
	}
	h := variablehub.NewVariableHub(func() persistence.VariableStore { return store }, variablehub.WithEvents(bus))
	if _, err := h.Create(ctx, variablehub.CreateInput{Definition: variable.Definition{Name: "API_BASE", Value: "https://api.example.com", Description: " The API "}}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Create(ctx, variablehub.CreateInput{Definition: variable.Definition{Name: "API_BASE", Value: "https://agent.example.com"}, OwnerAgentID: "a1", CreatedBy: variable.CreatorAgent}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Create(ctx, variablehub.CreateInput{Definition: variable.Definition{Name: "API_BASE", Value: "x"}}); !isCode(err, variable.CodeAlreadyExists) {
		t.Fatalf("duplicate = %v", err)
	}
	g, ok, _ := h.Get(ctx, "", "API_BASE")
	if !ok || g.Description != "The API" || g.CreatedBy != variable.CreatorUser {
		t.Fatalf("get = %+v", g)
	}
	all, _ := h.ListAll(ctx)
	if len(all) != 2 || all[0].OwnerAgentID != "" || all[1].OwnerAgentID != "a1" {
		t.Fatalf("all = %+v", all)
	}
	u, err := h.Update(ctx, "", "API_BASE", variable.Patch{Value: strPtr("https://v2.example.com"), Description: strPtr("")})
	if err != nil || u.Value != "https://v2.example.com" || u.Description != "" {
		t.Fatalf("update = %+v, %v", u, err)
	}
	if err := h.Delete(ctx, "", "API_BASE"); err != nil {
		t.Fatal(err)
	}
	if err := h.DeleteByOwner(ctx, "a1"); err != nil {
		t.Fatal(err)
	}
	if all, _ := h.ListAll(ctx); len(all) != 0 {
		t.Fatalf("left = %+v", all)
	}
	want := []string{"variable:created API_BASE", "variable:created API_BASE", "variable:updated API_BASE", "variable:deleted API_BASE"}
	if !slices.Equal(seen, want) {
		t.Fatalf("events = %v", seen)
	}
}

func TestHubChecksNamesValuesAndDescriptions(t *testing.T) {
	ctx := t.Context()
	store := persistence.NewMemoryVariableStore()
	h := variablehub.NewVariableHub(func() persistence.VariableStore { return store })
	for _, n := range []string{"api_base", "1X", "A-B", "", strings.Repeat("A", 65)} {
		if _, err := h.Create(ctx, variablehub.CreateInput{Definition: variable.Definition{Name: n, Value: "x"}}); !isCode(err, variable.CodeInvalidArgument) {
			t.Errorf("Create(%q) = %v", n, err)
		}
	}
	if _, err := h.Create(ctx, variablehub.CreateInput{Definition: variable.Definition{Name: "BIG", Value: strings.Repeat("x", 4097)}}); !isCode(err, variable.CodeInvalidArgument) {
		t.Fatalf("big = %v", err)
	}
	if _, err := h.Create(ctx, variablehub.CreateInput{Definition: variable.Definition{Name: "DESC", Value: "x", Description: strings.Repeat("d", 501)}}); !isCode(err, variable.CodeInvalidArgument) {
		t.Fatalf("desc = %v", err)
	}
	if v, err := h.Create(ctx, variablehub.CreateInput{Definition: variable.Definition{Name: "EMPTY"}}); err != nil || v.Value != "" {
		t.Fatalf("empty = %+v, %v", v, err)
	}
	if _, err := h.Update(ctx, "", "NOPE", variable.Patch{Value: strPtr("x")}); !isCode(err, variable.CodeNotFound) {
		t.Fatalf("update unknown = %v", err)
	}
}

func TestHubRefusesASecretsValue(t *testing.T) {
	ctx := t.Context()
	secrets := persistence.NewMemorySecretStore()
	sh := secrethub.NewSecretHub(func() persistence.SecretStore { return secrets }, secrethub.WithSecretHubProvider(secrethub.NewEphemeralSecretHubProvider()))
	const key = "sk-live-0123456789abcdef"
	if _, err := sh.Create(ctx, secrethub.CreateInput{Definition: secret.Definition{Name: "API_KEY", AllowedHosts: []string{"api.example.com"}}, Value: key}); err != nil {
		t.Fatal(err)
	}
	if _, err := sh.Create(ctx, secrethub.CreateInput{Definition: secret.Definition{Name: "OWN_KEY"}, Value: "agent-only-secret-1", OwnerAgentID: "a1"}); err != nil {
		t.Fatal(err)
	}
	store := persistence.NewMemoryVariableStore()
	h := variablehub.NewVariableHub(func() persistence.VariableStore { return store }, variablehub.WithSecretValueCheck(sh.IsSecretValue))
	if _, err := h.Create(ctx, variablehub.CreateInput{Definition: variable.Definition{Name: "LEAK", Value: key}}); !isCode(err, variable.CodeInvalidArgument) {
		t.Fatalf("global secret = %v", err)
	}
	if _, err := h.Create(ctx, variablehub.CreateInput{Definition: variable.Definition{Name: "LEAK", Value: "agent-only-secret-1"}, OwnerAgentID: "a1"}); !isCode(err, variable.CodeInvalidArgument) {
		t.Fatalf("own secret = %v", err)
	}
	// Another agent's secret is not its business.
	if _, err := h.Create(ctx, variablehub.CreateInput{Definition: variable.Definition{Name: "OK", Value: "agent-only-secret-1"}, OwnerAgentID: "a2"}); err != nil {
		t.Fatalf("other agent = %v", err)
	}
	if _, err := h.Create(ctx, variablehub.CreateInput{Definition: variable.Definition{Name: "LATER", Value: "fine"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Update(ctx, "", "LATER", variable.Patch{Value: strPtr(key)}); !isCode(err, variable.CodeInvalidArgument) {
		t.Fatalf("update to secret = %v", err)
	}
}
