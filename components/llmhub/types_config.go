package llmhub

import (
	"fmt"
	"io"
	"log/slog"
	"strconv"
	"strings"

	"github.com/Phong95/aec-agent-go/internal/redact"
)

// AuthType selects how a provider authenticates.
type AuthType string

const (
	// AuthAPIKey authenticates with ProviderConfig.APIKey.
	AuthAPIKey AuthType = "api_key"
	// AuthOAuth authenticates with the OAuth fields of ProviderConfig.
	AuthOAuth AuthType = "oauth"
)

// ProviderConfig is what a Factory receives to build a provider. AgentConfig is
// not part of this package: it lives in the root package as aec.AgentConfig, next
// to aec.CreateAgent (Phase 11), which builds a ProviderConfig from it.
//
// ProviderConfig holds credentials, so every text rendering of it redacts them:
// it implements slog.LogValuer (LogValue), fmt.Formatter (Format, which serves
// every verb, so %v, %+v, %#v and %s alike), fmt.Stringer and fmt.GoStringer. The
// five secrets (apiKey, secretKey, oauthAccessToken, refreshToken and the ChatGPT
// account id, the second half of the credential pair of the subscription provider)
// are replaced with [REDACTED], and BaseURL shows only its scheme and host
// (userinfo, path, query and fragment are removed: a gateway URL can carry a key in
// any of them).
//
// Two renderings are NOT redacted, on purpose or by necessity. json.Marshal
// writes every field, because the JSON form is the decode shape of a host
// configuration file and the wire fixture round-trips it. It must keep the secrets
// for that reason, so never hand a ProviderConfig to a JSON logger or an HTTP
// response; aec.AgentConfig, which is only
// ever built in code, redacts its JSON form (TestProviderConfigJSONKeepsEveryField pins
// this behaviour). And fmt cannot call Format on an unexported
// struct field, so a ProviderConfig held in an unexported field of another struct
// is printed field by field; hold it in an exported field, or log it directly.
//
// The slog redaction covers a configuration passed directly as a slog attribute
// value, which is the only place slog resolves a LogValuer (re-review RR-01 of
// Phase 6). A ProviderConfig inside a slice, a map or a struct field is a plain
// slog.KindAny value that slog.JSONHandler encodes with json.Marshal, and that
// writes the credentials: log such a value field by field, or log each
// configuration as an attribute of its own.
type ProviderConfig struct {
	APIKey  string `json:"apiKey"`
	Model   string `json:"model,omitempty"`
	BaseURL string `json:"baseUrl,omitempty"`
	// AWS Bedrock credentials
	SecretKey string `json:"secretKey,omitempty"`
	Region    string `json:"region,omitempty"`
	// OpenAI subscription (ChatGPT Plus/Pro/Team) OAuth fields (A-43)
	AuthType         AuthType `json:"authType,omitempty"`
	OAuthAccessToken string   `json:"oauthAccessToken,omitempty"`
	RefreshToken     string   `json:"refreshToken,omitempty"`
	// TokenExpiresAt is the expiry of OAuthAccessToken as epoch milliseconds, an
	// *int64 so an absent value stays absent. The server computes it as
	// now + expires_in * 1000 (in the OAuth route), where
	// expires_in is in seconds; the SDK only carries the number and never
	// refreshes the token itself.
	TokenExpiresAt   *int64 `json:"tokenExpiresAt,omitempty"`
	ChatgptAccountID string `json:"chatgptAccountId,omitempty"`
}

// redacted replaces a secret in log output.
const redacted = "[REDACTED]"

// redactURL removes the credentials from a base URL. It is the shared
// internal/redact.URL, the same rule persistence.ProviderRecord uses: only the
// scheme and the host are shown as they are, userinfo, query and path are replaced
// and the fragment is dropped, and a value that does not parse, has no scheme or is
// an opaque URL (a scheme-less host:port/path) is replaced whole, because it could
// hold anything. A gateway can carry its key in the path (https://gw.example/v1/<token>/)
// or in a fragment, and a redaction cannot tell a token segment from a version
// segment, so no path is shown (review IN-01, WR-201). A bare "/" is not a secret
// and stays.
func redactURL(raw string) string { return redact.URL(raw) }

// attrs lists the fields with the secrets replaced and the base URL cleaned.
func (c ProviderConfig) attrs() []slog.Attr {
	attrs := make([]slog.Attr, 0, 10)
	secret := func(key, value string) {
		if value != "" {
			attrs = append(attrs, slog.String(key, redacted))
		}
	}
	plain := func(key, value string) {
		if value != "" {
			attrs = append(attrs, slog.String(key, value))
		}
	}
	secret("apiKey", c.APIKey)
	plain("model", c.Model)
	if c.BaseURL != "" {
		plain("baseUrl", redactURL(c.BaseURL))
	}
	secret("secretKey", c.SecretKey)
	plain("region", c.Region)
	plain("authType", string(c.AuthType))
	secret("oauthAccessToken", c.OAuthAccessToken)
	secret("refreshToken", c.RefreshToken)
	if c.TokenExpiresAt != nil {
		attrs = append(attrs, slog.Int64("tokenExpiresAt", *c.TokenExpiresAt))
	}
	secret("chatgptAccountId", c.ChatgptAccountID)
	return attrs
}

// LogValue implements slog.LogValuer. It shows the non-secret fields and replaces
// each non-empty secret (apiKey, secretKey, oauthAccessToken, refreshToken,
// chatgptAccountId) with [REDACTED]. It protects a configuration logged as an
// attribute value of its own; slog never calls it for one nested in a slice, a map
// or a struct field (see the type doc).
func (c ProviderConfig) LogValue() slog.Value {
	return slog.GroupValue(c.attrs()...)
}

// String renders the redacted fields as ProviderConfig{key=value ...}.
func (c ProviderConfig) String() string {
	var b strings.Builder
	b.WriteString("ProviderConfig{")
	for i, a := range c.attrs() {
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
func (c ProviderConfig) GoString() string { return c.String() }

// Format implements fmt.Formatter. It writes the redacted String for every verb,
// so no fmt verb can print a raw field.
func (c ProviderConfig) Format(f fmt.State, _ rune) {
	_, _ = io.WriteString(f, c.String())
}

// Factory builds a provider from a configuration. Building can fail, so a
// factory returns an error.
type Factory func(cfg ProviderConfig) (Provider, error)
