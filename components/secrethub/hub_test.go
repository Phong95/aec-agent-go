package secrethub_test

import (
	"io"
	"log/slog"
	"slices"
	"strings"
	"testing"

	"github.com/Phong95/aec-agent-go/components/events"
	"github.com/Phong95/aec-agent-go/components/persistence"
	"github.com/Phong95/aec-agent-go/components/secrethub"
	"github.com/Phong95/aec-agent-go/components/secrethub/secret"
	"github.com/Phong95/aec-agent-go/kernel"
)

func newHub(opts ...secrethub.Option) (*persistence.MemorySecretStore, *secrethub.SecretHub) {
	store := persistence.NewMemorySecretStore()
	opts = append([]secrethub.Option{secrethub.WithSecretHubProvider(secrethub.NewEphemeralSecretHubProvider())}, opts...)
	return store, secrethub.NewSecretHub(func() persistence.SecretStore { return store }, opts...)
}

func strPtr(s string) *string { return &s }

func TestHubCreatesASealedSecretWithSafeDefaults(t *testing.T) {
	ctx := t.Context()
	store, h := newHub()
	info, err := h.Create(ctx, secrethub.CreateInput{Definition: secret.Definition{Name: "WEATHER_KEY", Description: strPtr("weather")}, Value: apiKey})
	if err != nil {
		t.Fatal(err)
	}
	if info.Name != "WEATHER_KEY" || !slices.Equal(info.AllowedTools, []string{"http_request"}) || len(info.AllowedHosts) != 0 || info.CreatedBy != secret.CreatorUser {
		t.Fatalf("info = %+v", info)
	}
	raw, _ := kernel.EncodeJSON(info)
	if strings.Contains(string(raw), apiKey) {
		t.Fatal("the info carries the value")
	}
	stored, _ := store.Get(ctx, "", "WEATHER_KEY")
	if stored == nil || !strings.HasPrefix(stored.Value, "enc:v2:") || stored.KeyID != h.Provider().CurrentKeyID() {
		t.Fatalf("stored = %+v", stored)
	}
}

func TestHubValidatesNamesHostsToolsAndDuplicates(t *testing.T) {
	ctx := t.Context()
	_, h := newHub()
	bad := []secrethub.CreateInput{
		{Definition: secret.Definition{Name: "lower"}, Value: "v"},
		{Definition: secret.Definition{Name: "K"}, Value: ""},
		{Definition: secret.Definition{Name: "K", AllowedHosts: []string{"not a host"}}, Value: "v"},
		{Definition: secret.Definition{Name: "K", AllowedTools: []string{"Bad-Tool"}}, Value: "v"},
	}
	for _, in := range bad {
		if _, err := h.Create(ctx, in); !isCode(err, secret.CodeInvalidArgument) {
			t.Errorf("Create(%+v) = %v, want invalid_argument", in, err)
		}
	}
	ok, err := h.Create(ctx, secrethub.CreateInput{Definition: secret.Definition{Name: "K", AllowedHosts: []string{"https://API.example.com/v1"}}, Value: "v"})
	if err != nil || !slices.Equal(ok.AllowedHosts, []string{"api.example.com"}) {
		t.Fatalf("Create = %+v, %v", ok, err)
	}
	if _, err := h.Create(ctx, secrethub.CreateInput{Definition: secret.Definition{Name: "K"}, Value: "v"}); !isCode(err, secret.CodeAlreadyExists) {
		t.Fatalf("duplicate: %v", err)
	}
	// The same name for an agent is a different secret.
	if s, err := h.Create(ctx, secrethub.CreateInput{Definition: secret.Definition{Name: "K"}, Value: "v", OwnerAgentID: "agent-a"}); err != nil || s.OwnerAgentID != "agent-a" {
		t.Fatalf("agent secret = %+v, %v", s, err)
	}
}

func TestHubRevealsOnlyForAnAllowedToolAndHost(t *testing.T) {
	ctx := t.Context()
	store, h := newHub()
	if _, err := h.Create(ctx, secrethub.CreateInput{Definition: secret.Definition{Name: "K", AllowedHosts: []string{"api.example.com"}}, Value: apiKey}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Reveal(ctx, "", "K", secret.Use{Tool: "terminal"}, secrethub.RevealContext{}); !isCode(err, secret.CodeNotAllowed) {
		t.Fatalf("terminal: %v", err)
	}
	if _, err := h.Reveal(ctx, "", "K", secret.Use{Tool: "http_request", Host: "evil.com"}, secrethub.RevealContext{}); !isCode(err, secret.CodeNotAllowed) {
		t.Fatalf("evil.com: %v", err)
	}
	v, err := h.Reveal(ctx, "", "K", secret.Use{Tool: "http_request", Host: "api.example.com"}, secrethub.RevealContext{})
	if err != nil || v != apiKey {
		t.Fatalf("Reveal = %q, %v", v, err)
	}
	if s, _ := store.Get(ctx, "", "K"); s == nil || s.LastUsedAt == "" {
		t.Fatal("the use was not recorded")
	}
	if got := h.Redactor().Redact("got " + apiKey); got != "got [secret:K]" {
		t.Fatalf("Redact = %q", got)
	}
}

func TestHubSecretWithNoHostsCannotBeSentAnywhere(t *testing.T) {
	ctx := t.Context()
	_, h := newHub()
	_, _ = h.Create(ctx, secrethub.CreateInput{Definition: secret.Definition{Name: "K"}, Value: apiKey})
	_, err := h.Reveal(ctx, "", "K", secret.Use{Tool: "http_request", Host: "api.example.com"}, secrethub.RevealContext{})
	if err == nil || !strings.Contains(err.Error(), "no allowed hosts yet") {
		t.Fatalf("Reveal = %v", err)
	}
}

func TestHubStatusRotateAndSelfCheck(t *testing.T) {
	ctx := t.Context()
	store := persistence.NewMemorySecretStore()
	h := secrethub.NewSecretHub(func() persistence.SecretStore { return store }, secrethub.WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))))
	if _, err := h.Create(ctx, secrethub.CreateInput{Definition: secret.Definition{Name: "PLAIN"}, Value: "p-value"}); err != nil {
		t.Fatal(err)
	}
	h.UseSecretHubProvider(localProvider(t, "old"))
	if _, err := h.Create(ctx, secrethub.CreateInput{Definition: secret.Definition{Name: "OLD"}, Value: "o-value"}); err != nil {
		t.Fatal(err)
	}
	h.UseSecretHubProvider(localProvider(t, "new", "old"))
	before, err := h.Status(ctx)
	if err != nil || before.Plain != 1 || len(before.ByKeyID) != 1 {
		t.Fatalf("status before = %+v, %v", before, err)
	}
	result, err := h.Rotate(ctx)
	if err != nil || result.Resealed != 2 || result.Unchanged != 0 || len(result.Failed) != 0 {
		t.Fatalf("Rotate = %+v, %v", result, err)
	}
	after, _ := h.Status(ctx)
	if after.Plain != 0 || after.Legacy != 0 || after.ByKeyID[h.Provider().CurrentKeyID()] != 2 {
		t.Fatalf("status after = %+v", after)
	}
	if v, err := h.Reveal(ctx, "", "OLD", secret.Use{Tool: "http_request"}, secrethub.RevealContext{}); err != nil || v != "o-value" {
		t.Fatalf("Reveal = %q, %v", v, err)
	}
	if problems, err := h.SelfCheck(ctx); err != nil || len(problems) != 0 {
		t.Fatalf("SelfCheck = %v, %v", problems, err)
	}
	h.UseSecretHubProvider(localProvider(t, "unrelated"))
	problems, _ := h.SelfCheck(ctx)
	if !strings.Contains(strings.Join(problems, ","), "can't be opened") {
		t.Fatalf("SelfCheck = %v", problems)
	}
}

func TestHubPublishesChangesWithNamesOnly(t *testing.T) {
	ctx := t.Context()
	bus := events.New()
	var seen []kernel.Event
	bus.OnAny(func(e kernel.Event) { seen = append(seen, e) })
	_, h := newHub(secrethub.WithEvents(bus))
	if _, err := h.Create(ctx, secrethub.CreateInput{Definition: secret.Definition{Name: "K"}, Value: apiKey}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Update(ctx, "", "K", secret.Definition{AllowedHosts: []string{"a.example.com"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.SetValue(ctx, "", "K", "another-long-value"); err != nil {
		t.Fatal(err)
	}
	if err := h.Delete(ctx, "", "K"); err != nil {
		t.Fatal(err)
	}
	var kinds []string
	var changed []string
	for _, e := range seen {
		if !strings.HasPrefix(e.Type, "secret:") {
			continue
		}
		kinds = append(kinds, e.Type)
		p, _ := events.PayloadAs[events.SecretChangedPayload](e)
		switch {
		case p.ValueChanged == nil:
			changed = append(changed, "-")
		case *p.ValueChanged:
			changed = append(changed, "true")
		default:
			changed = append(changed, "false")
		}
	}
	want := []string{events.SecretCreated.Name, events.SecretUpdated.Name, events.SecretUpdated.Name, events.SecretDeleted.Name}
	if !slices.Equal(kinds, want) || !slices.Equal(changed, []string{"-", "false", "true", "-"}) {
		t.Fatalf("kinds = %v, valueChanged = %v", kinds, changed)
	}
	raw, _ := kernel.EncodeJSON(seen)
	if strings.Contains(string(raw), apiKey) {
		t.Fatal("an event carries the value")
	}
}

func TestAAD(t *testing.T) {
	if secrethub.AAD("", "K") != "secret:global:K" || secrethub.AAD("agent-x", "K") != "secret:agent-x:K" {
		t.Fatal("AAD")
	}
}
