package persistence

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"strconv"
	"strings"

	"github.com/Phong95/aec-agent-go/components/llmhub"
	"github.com/Phong95/aec-agent-go/internal/jsonx"
	"github.com/Phong95/aec-agent-go/internal/redact"
)

// ---------------------------------------------------------------------------
// Provider store - persistence for provider configurations
// ---------------------------------------------------------------------------

// ProviderType is the kind of a stored provider. It is not validated on decode:
// a newer writer may add values.
type ProviderType string

// The known provider types.
const (
	ProviderAnthropic          ProviderType = "anthropic"
	ProviderOpenAICompatible   ProviderType = "openai-compatible"
	ProviderBedrock            ProviderType = "bedrock"
	ProviderOpenAISubscription ProviderType = "openai-subscription"
	ProviderGemini             ProviderType = "gemini"
	ProviderOpenRouter         ProviderType = "openrouter"
)

// ProviderRecord is a stored provider configuration.
//
// It carries four credentials (APIKey, SecretKey, OAuthAccessToken,
// RefreshToken), so every text rendering of it redacts them, the same way
// llmhub.ProviderConfig does: it implements slog.LogValuer (LogValue), fmt.Formatter
// (Format, which serves every verb, so %v, %+v, %#v and %s alike), fmt.Stringer
// and fmt.GoStringer. Each non-empty secret is replaced with [REDACTED] and
// BaseURL shows only its scheme and host (userinfo, path, query and fragment are
// removed; an opaque or scheme-less value is replaced whole). The JSON form is NOT
// redacted: it is the store wire shape and must round-trip unchanged. fmt cannot
// call Format on an unexported struct field, so a ProviderRecord held in an
// unexported field of another struct is printed field by field; hold it in an
// exported field, or log it directly.
//
// What is redacted, exactly (re-review RR-01 of Phase 6): a record passed
// directly as a slog attribute value (slog resolves a LogValuer only for the
// attribute value itself, and for the members of a group), and a record printed
// with any fmt verb. A record inside a slice, a map, a pointer slice or a struct
// field is a plain slog.KindAny value: slog.JSONHandler encodes it with
// json.Marshal, which calls MarshalJSON, and that writes the credentials because
// it is the wire shape. ProviderStore.List returns exactly such a slice, so log it
// through RedactedProviders (slog.Any("providers", persistence.RedactedProviders(recs)))
// or log each record as its own attribute. The JSON handler test pins both sides.
type ProviderRecord struct {
	Name string       `json:"name"`
	Type ProviderType `json:"type"`
	// Models holds the list of available llm models. Each entry keeps the model's
	// normalized metadata (context window, max output, capabilities, ...) as
	// listed by LLMModelProvider.listModels(), minus the raw provider payload
	// (A-54). It is required on the wire: nil encodes as [].
	Models []llmhub.ModelRecord `json:"models"`
	// APIKey is required and always written; it may be empty.
	APIKey    string `json:"apiKey"`
	BaseURL   string `json:"baseUrl,omitempty"`
	SecretKey string `json:"secretKey,omitempty"`
	Region    string `json:"region,omitempty"`
	// OpenAI subscription (ChatGPT Plus/Pro/Team) OAuth fields (A-43)
	AuthType         llmhub.AuthType `json:"authType,omitempty"`
	OAuthAccessToken string          `json:"oauthAccessToken,omitempty"`
	RefreshToken     string          `json:"refreshToken,omitempty"`
	// TokenExpiresAt is an epoch value in milliseconds.
	TokenExpiresAt   *int64 `json:"tokenExpiresAt,omitempty"`
	ChatgptAccountID string `json:"chatgptAccountId,omitempty"`
}

// MarshalJSON writes a nil Models as [] because the field is required. It
// takes a value receiver so a ProviderRecord held by value encodes correctly, and
// it never writes the normalised slice back into the caller's value.
func (p ProviderRecord) MarshalJSON() ([]byte, error) {
	type plain ProviderRecord // drops methods, avoids recursion
	if p.Models == nil {
		p.Models = []llmhub.ModelRecord{}
	}
	return jsonx.Marshal(plain(p))
}

// Clone returns a deep copy: every model record, the Models slice and the
// TokenExpiresAt pointer are copied (a nil Models stays nil), so a caller and a
// store never share a mutable part of a provider record. Add a line here when a
// pointer, slice or map field is added to ProviderRecord; the clonecheck test
// fails until you do.
func (p ProviderRecord) Clone() ProviderRecord {
	if p.Models != nil {
		models := make([]llmhub.ModelRecord, len(p.Models))
		for i, m := range p.Models {
			models[i] = m.Clone()
		}
		p.Models = models
	}
	if p.TokenExpiresAt != nil {
		v := *p.TokenExpiresAt
		p.TokenExpiresAt = &v
	}
	return p
}

// redactedSecret replaces a secret in log output.
const redactedSecret = "[REDACTED]"

// redactBaseURL removes the credentials from a base URL. It is the shared
// internal/redact.URL, the same rule as llmhub.ProviderConfig (review IN-01, WR-201):
// only the scheme and the host are shown as they are, userinfo, query and path are
// replaced and the fragment is dropped, and a value that does not parse, has no
// scheme or is an opaque URL is replaced whole, because it could hold anything.
func redactBaseURL(raw string) string { return redact.URL(raw) }

// attrs lists the fields with the secrets replaced and the base URL cleaned.
func (p ProviderRecord) attrs() []slog.Attr {
	attrs := make([]slog.Attr, 0, 10)
	secret := func(key, value string) {
		if value != "" {
			attrs = append(attrs, slog.String(key, redactedSecret))
		}
	}
	plain := func(key, value string) {
		if value != "" {
			attrs = append(attrs, slog.String(key, value))
		}
	}
	plain("name", p.Name)
	plain("type", string(p.Type))
	attrs = append(attrs, slog.Int("models", len(p.Models)))
	secret("apiKey", p.APIKey)
	if p.BaseURL != "" {
		plain("baseUrl", redactBaseURL(p.BaseURL))
	}
	secret("secretKey", p.SecretKey)
	plain("region", p.Region)
	plain("authType", string(p.AuthType))
	secret("oauthAccessToken", p.OAuthAccessToken)
	secret("refreshToken", p.RefreshToken)
	if p.TokenExpiresAt != nil {
		attrs = append(attrs, slog.Int64("tokenExpiresAt", *p.TokenExpiresAt))
	}
	secret("chatgptAccountId", p.ChatgptAccountID)
	return attrs
}

// LogValue implements slog.LogValuer. It shows the non-secret fields and replaces
// each non-empty secret (apiKey, secretKey, oauthAccessToken, refreshToken,
// chatgptAccountId) with [REDACTED]. It protects a record logged as an attribute
// value of its own; slog never calls it for a record nested in a slice, a map or a
// struct field (see the type doc and RedactedProviders).
func (p ProviderRecord) LogValue() slog.Value {
	return slog.GroupValue(p.attrs()...)
}

// RedactedProviders is a list of provider records that logs with every secret
// replaced, also under slog.JSONHandler: slog resolves a LogValuer for the
// attribute value, so wrapping the slice makes each element go through
// ProviderRecord.LogValue instead of through MarshalJSON. Use it for the result of
// ProviderStore.List:
//
//	logger.Info("providers", "list", persistence.RedactedProviders(recs))
//
// The records are logged as a group keyed by their index ("0", "1", ...). An empty
// list is an empty group, which a handler leaves out. It is a log wrapper only: it
// is not the wire form of a list and has no JSON method of its own.
type RedactedProviders []ProviderRecord

// LogValue implements slog.LogValuer.
func (r RedactedProviders) LogValue() slog.Value {
	attrs := make([]slog.Attr, len(r))
	for i, p := range r {
		attrs[i] = slog.Any(strconv.Itoa(i), p) // p is a LogValuer, resolved inside the group
	}
	return slog.GroupValue(attrs...)
}

// String renders every record redacted (the fmt verbs of a ProviderRecord slice are
// redacted already; this keeps the named type consistent with them).
func (r RedactedProviders) String() string {
	parts := make([]string, len(r))
	for i, p := range r {
		parts[i] = p.String()
	}
	return "[" + strings.Join(parts, " ") + "]"
}

// String renders the redacted fields as ProviderRecord{key=value ...}.
func (p ProviderRecord) String() string {
	var b strings.Builder
	b.WriteString("ProviderRecord{")
	for i, a := range p.attrs() {
		if i > 0 {
			b.WriteByte(' ')
		}
		b.WriteString(a.Key)
		b.WriteByte('=')
		b.WriteString(strconv.Quote(a.Value.String()))
	}
	b.WriteByte('}')
	return b.String()
}

// GoString is the %#v rendering; it is the same redacted text as String.
func (p ProviderRecord) GoString() string { return p.String() }

// Format implements fmt.Formatter. It writes the redacted String for every verb,
// so no fmt verb can print a raw field.
func (p ProviderRecord) Format(f fmt.State, _ rune) {
	_, _ = io.WriteString(f, p.String())
}

// ProviderStore persists provider configurations.
type ProviderStore interface {
	Save(ctx context.Context, p ProviderRecord) error
	// Load returns the record, or a nil pointer and a nil error when the name is
	// unknown.
	Load(ctx context.Context, name string) (*ProviderRecord, error)
	List(ctx context.Context) ([]ProviderRecord, error)
	Delete(ctx context.Context, name string) error
}
