package persistence_test

import (
	"bytes"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"github.com/Phong95/aec-agent-go/components/llmhub"
	"github.com/Phong95/aec-agent-go/components/persistence"
	"github.com/Phong95/aec-agent-go/components/skillhub/skill"
	"github.com/Phong95/aec-agent-go/internal/clonecheck"
	"github.com/Phong95/aec-agent-go/kernel"
)

// The four secrets of the test record: obviously fake values that are unique
// enough to search for in rendered output.
var providerSecrets = map[string]string{
	"apiKey":           "FAKE-API-KEY-VALUE-0001",
	"secretKey":        "FAKE-SECRET-KEY-VALUE-0002",
	"oauthAccessToken": "FAKE-ACCESS-TOKEN-VALUE-0003",
	"refreshToken":     "FAKE-REFRESH-TOKEN-VALUE-0004",
}

func secretRecord() persistence.ProviderRecord {
	expires := int64(1900000000000)
	return persistence.ProviderRecord{
		Name: "chatgpt", Type: persistence.ProviderOpenAISubscription,
		Models:           []llmhub.ModelRecord{{ID: "m1", Provider: "chatgpt"}, {ID: "m2", Provider: "chatgpt"}},
		APIKey:           providerSecrets["apiKey"],
		BaseURL:          "https://user:FAKE-URL-PASSWORD-0005@api.example.test/v1/FAKE-URL-PATH-0007?key=FAKE-URL-QUERY-0006#FAKE-URL-FRAGMENT-0008",
		SecretKey:        providerSecrets["secretKey"],
		Region:           "us-east-1",
		AuthType:         llmhub.AuthOAuth,
		OAuthAccessToken: providerSecrets["oauthAccessToken"],
		RefreshToken:     providerSecrets["refreshToken"],
		TokenExpiresAt:   &expires,
		ChatgptAccountID: "acct-test-0001",
	}
}

// assertNoSecret fails when out holds a secret. It names only the rendering and
// the secret key, never the secret value.
func assertNoSecret(t *testing.T, rendering, out string) {
	t.Helper()
	for key, secret := range providerSecrets {
		if strings.Contains(out, secret) {
			t.Errorf("%s printed the secret %s", rendering, key)
		}
	}
	for _, leak := range []string{"FAKE-URL-PASSWORD-0005", "FAKE-URL-QUERY-0006", "FAKE-URL-PATH-0007", "FAKE-URL-FRAGMENT-0008", "acct-test-0001"} {
		if strings.Contains(out, leak) {
			t.Errorf("%s printed a credential of the base URL", rendering)
		}
	}
	if !strings.Contains(out, "[REDACTED]") {
		t.Errorf("%s does not show [REDACTED]", rendering)
	}
}

// slog, every fmt verb, String and GoString never show a secret, whether the
// record is held by value, by pointer or in a slice; the non-secret fields stay
// visible.
func TestProviderRecordRedaction(t *testing.T) {
	rec := secretRecord()

	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))
	logger.Info("provider", "record", rec)
	assertNoSecret(t, "slog JSON handler", buf.String())
	if !strings.Contains(buf.String(), "chatgpt") || !strings.Contains(buf.String(), "api.example.test") {
		t.Errorf("slog output lost a non-secret field: %s", buf.String())
	}
	buf.Reset()
	slog.New(slog.NewTextHandler(&buf, nil)).Info("provider", "record", rec)
	assertNoSecret(t, "slog text handler", buf.String())

	for _, verb := range []string{"%v", "%+v", "%#v", "%s", "%q", "%d", "%x"} {
		assertNoSecret(t, "fmt "+verb, fmt.Sprintf(verb, rec))
		assertNoSecret(t, "fmt "+verb+" of a pointer", fmt.Sprintf(verb, &rec))
	}
	assertNoSecret(t, "fmt %v of a slice", fmt.Sprintf("%v", []persistence.ProviderRecord{rec}))
	assertNoSecret(t, "String", rec.String())
	assertNoSecret(t, "GoString", rec.GoString())
	assertNoSecret(t, "fmt.Sprint", fmt.Sprint(rec))

	str := rec.String()
	for _, want := range []string{"name=", "chatgpt", "models=", "region=", "authType=", "tokenExpiresAt=", "chatgptAccountId="} {
		if !strings.Contains(str, want) {
			t.Errorf("String lost the non-secret field %q: %s", want, str)
		}
	}
}

// TestProviderRecordNestedUnderJSONHandler pins re-review RR-01 of Phase 6. slog
// resolves a LogValuer only for the attribute value itself, so a record inside a
// slice, a map, a pointer slice or a struct field reaches slog.JSONHandler as a
// plain value and is encoded by json.Marshal, i.e. by MarshalJSON, which writes the
// wire shape with the credentials (the wire shape must round-trip unchanged, so it
// cannot be redacted). The documented guarantee is therefore: redacted when
// logged directly or through RedactedProviders, NOT redacted when nested. The test
// holds both halves, so a change in either direction is noticed.
func TestProviderRecordNestedUnderJSONHandler(t *testing.T) {
	rec := secretRecord()
	log := func(key string, value any) string {
		var buf bytes.Buffer
		slog.New(slog.NewJSONHandler(&buf, nil)).Info("providers", key, value)
		return buf.String()
	}

	// The safe forms: a direct value, a pointer and the RedactedProviders wrapper.
	for name, value := range map[string]any{
		"direct":             rec,
		"pointer":            &rec,
		"RedactedProviders":  persistence.RedactedProviders{rec, rec},
		"RedactedProviders*": persistence.RedactedProviders{rec},
	} {
		out := log("p", value)
		assertNoSecret(t, "slog JSON handler, "+name, out)
		if !strings.Contains(out, "[REDACTED]") || !strings.Contains(out, "api.example.test") {
			t.Errorf("%s: output lost the marker or a non-secret field: %s", name, out)
		}
	}
	list := log("p", persistence.RedactedProviders{rec, rec})
	if !strings.Contains(list, `"0":`) || !strings.Contains(list, `"1":`) {
		t.Errorf("RedactedProviders is not logged by index: %s", list)
	}
	if got := fmt.Sprint(persistence.RedactedProviders{rec}); strings.Contains(got, providerSecrets["apiKey"]) || !strings.Contains(got, "ProviderRecord{") {
		t.Errorf("RedactedProviders.String is not redacted: %s", got)
	}

	// The documented limitation: nested records are encoded through MarshalJSON.
	type holder struct{ R persistence.ProviderRecord }
	for name, value := range map[string]any{
		"slice":    []persistence.ProviderRecord{rec},
		"ptrslice": []*persistence.ProviderRecord{&rec},
		"map":      map[string]persistence.ProviderRecord{"a": rec},
		"struct":   holder{R: rec},
	} {
		if out := log("p", value); !strings.Contains(out, providerSecrets["apiKey"]) {
			t.Errorf("a record nested in a %s is now redacted by slog.JSONHandler: update the type doc and this test (it no longer holds the documented limitation)", name)
		}
	}
}

// TestProviderRecordRedactsOpaqueBaseURL pins WR-201: a base URL typed without a
// scheme parses as scheme = host and Opaque = port/path (Path stays empty), and its
// path used to be printed in full. It is replaced whole now, in every rendering.
func TestProviderRecordRedactsOpaqueBaseURL(t *testing.T) {
	for _, raw := range []string{"localhost:8080/v1/TOKEN-secret", "gw.example:8443/TOKEN-secret", "https:TOKEN-secret", "host/path/TOKEN-secret", "mailto:TOKEN-secret"} {
		rec := persistence.ProviderRecord{Name: "p", Type: persistence.ProviderOpenAICompatible, BaseURL: raw}
		var buf bytes.Buffer
		slog.New(slog.NewTextHandler(&buf, nil)).Info("m", "record", rec)
		for what, out := range map[string]string{"String": rec.String(), "%v": fmt.Sprintf("%v", rec), "slog": buf.String()} {
			if strings.Contains(out, "TOKEN-secret") {
				t.Errorf("BaseURL %q leaked into the %s rendering %q", raw, what, out)
			}
			if !strings.Contains(out, "[REDACTED]") {
				t.Errorf("BaseURL %q: the %s rendering %q shows no marker", raw, what, out)
			}
		}
	}
}

// The JSON form is the store wire shape, so it is NOT redacted; a nil Models
// encodes as [] without changing the caller's value.
func TestProviderRecordJSONUnredacted(t *testing.T) {
	rec := secretRecord()
	b, err := kernel.EncodeJSON(rec)
	if err != nil {
		t.Fatal(err)
	}
	for key, secret := range providerSecrets {
		if !strings.Contains(string(b), secret) {
			t.Errorf("the JSON form lost the %s value", key)
		}
	}
	if strings.Contains(string(b), "[REDACTED]") {
		t.Error("the JSON form must not be redacted")
	}

	empty := persistence.ProviderRecord{Name: "p", Type: persistence.ProviderAnthropic}
	out, err := kernel.EncodeJSON(empty)
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"name":"p","type":"anthropic","models":[],"apiKey":""}`; string(out) != want {
		t.Fatalf("nil Models = %s, want %s", out, want)
	}
	if empty.Models != nil {
		t.Fatal("MarshalJSON wrote the normalised slice back into the caller's value")
	}

	skillOut, err := kernel.EncodeJSON(persistence.SkillRecord{Name: "s", Content: "c", Version: 1, CreatedAt: "t", UpdatedAt: "t"})
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"name":"s","content":"c","files":[],"version":1,"createdAt":"t","updatedAt":"t"}`; string(skillOut) != want {
		t.Fatalf("nil Files = %s, want %s", skillOut, want)
	}
}

// ProviderRecord.Clone must share nothing with the original: every model record,
// the slices and the token expiry pointer are copied.
func TestProviderRecordCloneIsDeep(t *testing.T) {
	yes := true
	window, maxOut := 200000, 8192
	in, out := 3.0, 15.0
	expires := int64(1900000000000)
	sample := secretRecord()
	sample.Models = []llmhub.ModelRecord{{
		ID: "claude-sonnet", Provider: "anthropic", DisplayName: "d", Description: "d", OwnedBy: "o", CreatedAt: "c",
		ContextWindowTokens: &window, MaxContextWindowTokens: &window, MaxOutputTokens: &maxOut,
		InputModalities: []string{"text"}, OutputModalities: []string{"text"},
		ReasoningEfforts: []string{"low", "high"}, DefaultReasoningEffort: "low",
		Capabilities: &llmhub.ModelCapabilities{Vision: &yes, PDFInput: &yes, ToolCalling: &yes, Reasoning: &yes, StructuredOutputs: &yes, Streaming: &yes},
		Pricing:      &llmhub.ModelPricing{InputUSDPerMillionTokens: &in, OutputUSDPerMillionTokens: &out},
	}}
	sample.TokenExpiresAt = &expires
	clonecheck.AssertDeep(t, sample, persistence.ProviderRecord.Clone)
}

// SkillRecord.Clone must share nothing with the original: the files are copied.
func TestSkillRecordCloneIsDeep(t *testing.T) {
	sample := persistence.SkillRecord{
		Name: "s", Content: "c", Files: []skill.File{{Path: "references/format.md", Content: "x"}},
		CreatedBy: "agent-1", Version: 1, CreatedAt: "2026-03-01T00:00:00.000Z", UpdatedAt: "2026-03-01T00:00:00.000Z",
	}
	clonecheck.AssertDeep(t, sample, persistence.SkillRecord.Clone)
}
