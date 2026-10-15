package llmhub_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"github.com/Phong95/aec-agent-go/components/llmhub"
)

func factoryOf(p llmhub.Provider) llmhub.Factory {
	return func(llmhub.ProviderConfig) (llmhub.Provider, error) { return p, nil }
}

func TestRegistryListsBuiltinsInGoldenOrderAfterReverseRegistration(t *testing.T) {
	want := loadHubErrors(t).BuiltinOrder
	r := llmhub.NewRegistry()
	for i := len(want) - 1; i >= 0; i-- {
		r.Register(want[i], factoryOf(&fakeProvider{name: want[i]}))
	}
	if got := r.List(); !equalStrings(got, want) {
		t.Fatalf("List = %v, want TS order %v", got, want)
	}

	// Custom names follow the built-ins in registration order, whatever the order
	// they were registered in relative to the built-ins.
	r.Register("zeta", factoryOf(&fakeProvider{name: "zeta"}))
	r.Register("alpha", factoryOf(&fakeProvider{name: "alpha"}))
	wantWithCustom := append(append([]string(nil), want...), "zeta", "alpha")
	if got := r.List(); !equalStrings(got, wantWithCustom) {
		t.Fatalf("List = %v, want %v", got, wantWithCustom)
	}
}

func TestRegistryCustomBeforeBuiltinsStillListsBuiltinsFirst(t *testing.T) {
	order := loadHubErrors(t).BuiltinOrder
	r := llmhub.NewRegistry()
	r.Register("custom", factoryOf(&fakeProvider{name: "custom"}))
	r.Register(order[2], factoryOf(&fakeProvider{name: order[2]}))
	r.Register(order[0], factoryOf(&fakeProvider{name: order[0]}))
	want := []string{order[0], order[2], "custom"}
	if got := r.List(); !equalStrings(got, want) {
		t.Fatalf("List = %v, want %v", got, want)
	}
}

func TestRegistryReRegisterKeepsPosition(t *testing.T) {
	r := llmhub.NewRegistry()
	first := &fakeProvider{name: "x", tag: "first"}
	replacement := &fakeProvider{name: "x", tag: "replacement"}
	r.Register("x", factoryOf(first))
	r.Register("y", factoryOf(&fakeProvider{name: "y"}))
	r.Register("x", factoryOf(replacement))
	if got := r.List(); !equalStrings(got, []string{"x", "y"}) {
		t.Fatalf("List = %v, want [x y]", got)
	}
	p, err := r.Resolve("x", llmhub.ProviderConfig{})
	if err != nil || p != llmhub.Provider(replacement) {
		t.Fatalf("Resolve(x) = %v, %v; want the replacement (last registration wins)", p, err)
	}

	// A built-in name keeps its place too.
	order := loadHubErrors(t).BuiltinOrder
	for _, n := range order {
		r.Register(n, factoryOf(&fakeProvider{name: n}))
	}
	r.Register(order[1], factoryOf(&fakeProvider{name: order[1], tag: "again"}))
	want := append(append([]string(nil), order...), "x", "y")
	if got := r.List(); !equalStrings(got, want) {
		t.Fatalf("List = %v, want %v", got, want)
	}
}

func TestRegistryResolveUnknownTextWithBuiltins(t *testing.T) {
	fx := loadHubErrors(t)
	r := llmhub.NewRegistry()
	for _, n := range fx.BuiltinOrder {
		r.Register(n, factoryOf(&fakeProvider{name: n}))
	}
	// The fixture was produced for the name "nope".
	_, err := r.Resolve("nope", llmhub.ProviderConfig{APIKey: "k"})
	if err == nil || err.Error() != fx.UnknownProvider {
		t.Fatalf("Resolve error = %v, want text %q", err, fx.UnknownProvider)
	}
	if !errors.Is(err, llmhub.ErrUnknownProvider) {
		t.Fatalf("error %v does not match ErrUnknownProvider", err)
	}
}

// TestRegistryResolveUnknownTemplate derives the template from the fixture text, so the
// custom-name and empty-list variants stay tied to the fixture.
func TestRegistryResolveUnknownTemplate(t *testing.T) {
	fx := loadHubErrors(t)
	list := strings.Join(fx.BuiltinOrder, ", ")
	if strings.Count(fx.UnknownProvider, list) != 1 {
		t.Fatalf("fixture text %q does not hold the built-in list %q exactly once", fx.UnknownProvider, list)
	}
	template := func(names ...string) string {
		return strings.Replace(fx.UnknownProvider, list, strings.Join(names, ", "), 1)
	}

	r := llmhub.NewRegistry()
	if _, err := r.Resolve("nope", llmhub.ProviderConfig{}); err == nil || err.Error() != template() {
		t.Fatalf("empty registry error = %v, want %q", err, template())
	}

	for _, n := range fx.BuiltinOrder {
		r.Register(n, factoryOf(&fakeProvider{name: n}))
	}
	r.Register("mine", factoryOf(&fakeProvider{name: "mine"}))
	want := template(append(append([]string(nil), fx.BuiltinOrder...), "mine")...)
	if _, err := r.Resolve("nope", llmhub.ProviderConfig{}); err == nil || err.Error() != want {
		t.Fatalf("error with a custom name = %v, want %q", err, want)
	}
}

func TestRegistryResolvePassesConfigAndResults(t *testing.T) {
	r := llmhub.NewRegistry()
	var got llmhub.ProviderConfig
	want := &fakeProvider{name: "p"}
	r.Register("p", func(cfg llmhub.ProviderConfig) (llmhub.Provider, error) {
		got = cfg
		return want, nil
	})
	cfg := llmhub.ProviderConfig{APIKey: "k", Model: "m", Region: "r"}
	p, err := r.Resolve("p", cfg)
	if err != nil || p != llmhub.Provider(want) {
		t.Fatalf("Resolve = %v, %v", p, err)
	}
	if got != cfg {
		t.Fatalf("factory received %v, want %v", got, cfg)
	}

	factoryErr := errors.New("boom")
	r.Register("bad", func(llmhub.ProviderConfig) (llmhub.Provider, error) { return nil, factoryErr })
	if _, err := r.Resolve("bad", cfg); !errors.Is(err, factoryErr) {
		t.Fatalf("Resolve error = %v, want the factory error unchanged", err)
	}
}

func TestRegistryNilFactoryDoesNotPanic(t *testing.T) {
	r := llmhub.NewRegistry()
	r.Register("nilf", nil)
	p, err := r.Resolve("nilf", llmhub.ProviderConfig{})
	if err == nil || p != nil {
		t.Fatalf("Resolve = %v, %v; want an error", p, err)
	}
	if errors.Is(err, llmhub.ErrUnknownProvider) {
		t.Fatalf("a registered nil factory is not an unknown provider: %v", err)
	}
}

func TestRegistryFactoryRunsOutsideTheLock(t *testing.T) {
	r := llmhub.NewRegistry()
	r.Register("outer", func(llmhub.ProviderConfig) (llmhub.Provider, error) {
		// A factory that registers or lists would deadlock if Resolve held the lock.
		r.Register("inner", factoryOf(&fakeProvider{name: "inner"}))
		_ = r.List()
		return &fakeProvider{name: "outer"}, nil
	})
	if _, err := r.Resolve("outer", llmhub.ProviderConfig{}); err != nil {
		t.Fatal(err)
	}
	if got := r.List(); !equalStrings(got, []string{"outer", "inner"}) {
		t.Fatalf("List = %v", got)
	}
}

func TestDefaultRegistryFunctions(t *testing.T) {
	// There is no unregister, so the name is unique to this test.
	const name = "test-default-registry-custom"
	want := &fakeProvider{name: name}
	llmhub.RegisterProvider(name, factoryOf(want))

	found := false
	for _, n := range llmhub.ListProviders() {
		found = found || n == name
	}
	if !found {
		t.Fatalf("ListProviders = %v, want it to hold %q", llmhub.ListProviders(), name)
	}
	p, err := llmhub.ResolveProvider(name, llmhub.ProviderConfig{APIKey: "k"})
	if err != nil || p != llmhub.Provider(want) {
		t.Fatalf("ResolveProvider = %v, %v", p, err)
	}
	if _, err := llmhub.ResolveProvider("test-default-registry-missing", llmhub.ProviderConfig{}); !errors.Is(err, llmhub.ErrUnknownProvider) {
		t.Fatalf("ResolveProvider of an unknown name = %v, want ErrUnknownProvider", err)
	}
}

func TestRegistryConcurrentAccess(t *testing.T) {
	r := llmhub.NewRegistry()
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Add(3)
		name := fmt.Sprintf("p%d", i)
		go func() {
			defer wg.Done()
			r.Register(name, factoryOf(&fakeProvider{name: name}))
		}()
		go func() {
			defer wg.Done()
			_, _ = r.Resolve(name, llmhub.ProviderConfig{})
		}()
		go func() {
			defer wg.Done()
			_ = r.List()
		}()
	}
	wg.Wait()
	if got := r.List(); len(got) != 8 {
		t.Fatalf("List = %v, want 8 names", got)
	}
}

func TestProviderConfigLogValueRedactsSecrets(t *testing.T) {
	expires := int64(1700000000000)
	cfg := llmhub.ProviderConfig{
		APIKey:           "sk-secret-api-key",
		Model:            "claude-sonnet-4",
		BaseURL:          "https://example.test/v1",
		SecretKey:        "secret-access-key-value",
		Region:           "eu-west-1",
		AuthType:         llmhub.AuthOAuth,
		OAuthAccessToken: "oauth-access-token-value",
		RefreshToken:     "refresh-token-value",
		TokenExpiresAt:   &expires,
		ChatgptAccountID: "acct-42",
	}
	for name, handler := range map[string]func(*bytes.Buffer) slog.Handler{
		"text": func(b *bytes.Buffer) slog.Handler { return slog.NewTextHandler(b, nil) },
		"json": func(b *bytes.Buffer) slog.Handler { return slog.NewJSONHandler(b, nil) },
	} {
		var buf bytes.Buffer
		slog.New(handler(&buf)).Info("provider configured", "config", cfg)
		out := buf.String()
		for _, secret := range []string{cfg.APIKey, cfg.SecretKey, cfg.OAuthAccessToken, cfg.RefreshToken} {
			if strings.Contains(out, secret) {
				t.Errorf("%s handler leaked a secret value in %q", name, out)
			}
		}
		if strings.Contains(out, cfg.ChatgptAccountID) {
			t.Errorf("%s handler printed the account id in %q", name, out)
		}
		for _, visible := range []string{cfg.Model, cfg.Region, "https://example.test/REDACTED", "[REDACTED]"} {
			if !strings.Contains(out, visible) {
				t.Errorf("%s handler output %q lacks %q", name, out, visible)
			}
		}
	}

	// A config without secrets logs no redaction marker.
	var buf bytes.Buffer
	slog.New(slog.NewTextHandler(&buf, nil)).Info("m", "config", llmhub.ProviderConfig{Model: "m1"})
	if strings.Contains(buf.String(), "REDACTED") {
		t.Errorf("an empty secret must not be reported as redacted: %s", buf.String())
	}
}

// secretConfig has a distinctive value in every secret place, BaseURL included.
func secretConfig() llmhub.ProviderConfig {
	expires := int64(1700000000000)
	return llmhub.ProviderConfig{
		APIKey:           "sk-secret-api-key",
		Model:            "claude-sonnet-4",
		BaseURL:          "https://bob:hunter2-pw@example.test/v1/path-token-secret/?key=query-secret#fragment-secret",
		SecretKey:        "secret-access-key-value",
		Region:           "eu-west-1",
		AuthType:         llmhub.AuthOAuth,
		OAuthAccessToken: "oauth-access-token-value",
		RefreshToken:     "refresh-token-value",
		TokenExpiresAt:   &expires,
		ChatgptAccountID: "acct-42",
	}
}

var secretNeedles = []string{
	"sk-secret-api-key", "secret-access-key-value", "oauth-access-token-value", "refresh-token-value",
	"hunter2-pw", "bob", "query-secret", "acct-42", "path-token-secret", "fragment-secret",
}

func assertNoSecrets(t *testing.T, what, out string) {
	t.Helper()
	for _, secret := range secretNeedles {
		if strings.Contains(out, secret) {
			t.Errorf("%s leaked %q in %q", what, secret, out)
		}
	}
}

// TestProviderConfigRedactsInEveryFormatVerb pins WR-02: fmt verbs, errors, a
// pointer, an exported nested field and slog's text handler on a wrapping struct
// all go through the redaction, and a URL's credentials are removed.
func TestProviderConfigRedactsInEveryFormatVerb(t *testing.T) {
	cfg := secretConfig()
	type wrap struct{ Config llmhub.ProviderConfig }

	for name, out := range map[string]string{
		"%v":       fmt.Sprintf("%v", cfg),
		"%+v":      fmt.Sprintf("%+v", cfg),
		"%#v":      fmt.Sprintf("%#v", cfg),
		"%s":       fmt.Sprintf("%s", cfg),
		"%q":       fmt.Sprintf("%q", cfg),
		"%d":       fmt.Sprintf("%d", cfg),
		"pointer":  fmt.Sprintf("%+v", &cfg),
		"Println":  fmt.Sprintln(cfg),
		"nested":   fmt.Sprintf("%+v", wrap{Config: cfg}),
		"nested #": fmt.Sprintf("%#v", wrap{Config: cfg}),
		"slice":    fmt.Sprintf("%v", []llmhub.ProviderConfig{cfg}),
		"map":      fmt.Sprintf("%v", map[string]llmhub.ProviderConfig{"k": cfg}),
		"Errorf":   fmt.Errorf("resolve failed: %v", cfg).Error(),
		"Errorf w": fmt.Errorf("resolve failed: %w", fmt.Errorf("%+v", cfg)).Error(),
		"String":   cfg.String(),
		"GoString": cfg.GoString(),
	} {
		assertNoSecrets(t, name, out)
		if !strings.Contains(out, "[REDACTED]") || !strings.Contains(out, "claude-sonnet-4") || !strings.Contains(out, "eu-west-1") {
			t.Errorf("%s output %q must keep the non-secret fields and show the marker", name, out)
		}
		if !strings.Contains(out, "example.test/REDACTED") {
			t.Errorf("%s output %q lost the host of the base URL or the path marker", name, out)
		}
	}

	var buf bytes.Buffer
	slog.New(slog.NewTextHandler(&buf, nil)).Info("y", "w", wrap{Config: cfg})
	assertNoSecrets(t, "slog text handler on a wrapping struct", buf.String())
}

// TestProviderConfigNestedUnderJSONHandler pins re-review RR-01 of Phase 6 for
// llmhub.ProviderConfig. slog resolves a LogValuer only for the attribute value
// itself, so a configuration inside a slice, a map or a struct field reaches
// slog.JSONHandler as a plain value and is encoded by json.Marshal, which writes
// every field (the JSON form is the decode shape of a host configuration file and
// must round-trip). The documented guarantee is therefore: redacted when logged
// directly (value or pointer), NOT redacted when nested. Both halves are held here,
// so a change in either direction is noticed and the type doc must follow it.
func TestProviderConfigNestedUnderJSONHandler(t *testing.T) {
	cfg := secretConfig()
	type wrap struct{ Config llmhub.ProviderConfig }
	log := func(value any) string {
		var buf bytes.Buffer
		slog.New(slog.NewJSONHandler(&buf, nil)).Info("m", "config", value)
		return buf.String()
	}
	for name, value := range map[string]any{"value": cfg, "pointer": &cfg} {
		assertNoSecrets(t, "slog JSON handler, "+name, log(value))
	}
	for name, value := range map[string]any{
		"slice":  []llmhub.ProviderConfig{cfg},
		"map":    map[string]llmhub.ProviderConfig{"k": cfg},
		"struct": wrap{Config: cfg},
	} {
		if !strings.Contains(log(value), cfg.APIKey) {
			t.Errorf("a configuration nested in a %s is now redacted by slog.JSONHandler: update the ProviderConfig doc and this test", name)
		}
	}
}

func TestProviderConfigRedactsBaseURLCredentialsInLogValue(t *testing.T) {
	cfg := llmhub.ProviderConfig{BaseURL: "https://bob:hunter2-pw@example.test/v1/path-token-secret/?key=query-secret#fragment-secret"}
	var buf bytes.Buffer
	slog.New(slog.NewJSONHandler(&buf, nil)).Info("m", "config", cfg)
	assertNoSecrets(t, "slog json handler", buf.String())
	if !strings.Contains(buf.String(), "https://REDACTED@example.test/REDACTED?REDACTED") {
		t.Errorf("the scheme and host must stay visible and the rest redacted (IN-01): %s", buf.String())
	}

	for _, raw := range []string{"user:pw-secret@host/v1", "http://[::1", "https://ok.test/%zz"} {
		out := llmhub.ProviderConfig{BaseURL: raw}.String()
		if strings.Contains(out, "pw-secret") || strings.Contains(out, raw) && strings.Contains(raw, "@") {
			t.Errorf("BaseURL %q leaked into %q", raw, out)
		}
	}
	// A path is never shown, whatever it looks like (IN-01); a URL with no path is.
	if out := (llmhub.ProviderConfig{BaseURL: "https://example.test/v1"}).String(); strings.Contains(out, "/v1") || !strings.Contains(out, "https://example.test/REDACTED") {
		t.Errorf("a base URL path must be redacted: %s", out)
	}
	if out := (llmhub.ProviderConfig{BaseURL: "https://example.test"}).String(); !strings.Contains(out, `"https://example.test"`) {
		t.Errorf("a base URL with no path must be shown as is: %s", out)
	}
}

// TestProviderConfigRedactsOpaqueBaseURL pins WR-201: a base URL typed without a
// scheme parses as scheme = host and Opaque = port/path (Path stays empty), and its
// path used to be printed in full. It is replaced whole now, in every rendering.
func TestProviderConfigRedactsOpaqueBaseURL(t *testing.T) {
	for _, raw := range []string{"localhost:8080/v1/TOKEN-secret", "gw.example:8443/TOKEN-secret", "https:TOKEN-secret", "host/path/TOKEN-secret", "mailto:TOKEN-secret"} {
		cfg := llmhub.ProviderConfig{BaseURL: raw}
		var buf bytes.Buffer
		slog.New(slog.NewTextHandler(&buf, nil)).Info("m", "config", cfg)
		for what, out := range map[string]string{"String": cfg.String(), "%v": fmt.Sprintf("%v", cfg), "slog": buf.String()} {
			if strings.Contains(out, "TOKEN-secret") {
				t.Errorf("BaseURL %q leaked into the %s rendering %q", raw, what, out)
			}
			if !strings.Contains(out, "[REDACTED]") {
				t.Errorf("BaseURL %q: the %s rendering %q shows no marker", raw, what, out)
			}
		}
	}
}

// TestProviderConfigJSONKeepsEveryField records the documented exception: the
// JSON form is the wire shape and is not redacted.
func TestProviderConfigJSONKeepsEveryField(t *testing.T) {
	out, err := json.Marshal(secretConfig())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "sk-secret-api-key") {
		t.Fatalf("json.Marshal is the wire form and must keep apiKey: %s", out)
	}
}
