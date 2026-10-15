package anthropic

import (
	"context"
	"net/http"
	"time"

	sdk "github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"

	"github.com/Phong95/aec-agent-go/components/llmhub"
	"github.com/Phong95/aec-agent-go/internal/llmtransport"
	"github.com/Phong95/aec-agent-go/kernel"
)

// Compile-time proof that Provider is an llmhub.Provider and an llmhub.ModelLister.
var (
	_ llmhub.Provider    = (*Provider)(nil)
	_ llmhub.ModelLister = (*Provider)(nil)
)

// providerName is the name the provider reports and registers under.
const providerName = "anthropic"

// credentialHeaders are the headers that carry the credential of a request and must
// not follow a redirect to another host. net/http drops Authorization on its own but
// forwards X-Api-Key, which is where Anthropic expects the key.
var credentialHeaders = []string{"X-Api-Key", "Authorization"}

// options collects what the Option functions set before New builds the client.
type options struct {
	httpClient *http.Client
	maxRetries *int
	timeout    *time.Duration
}

// Option changes how New builds the provider. Options exist for tests and advanced
// use; a plain New(cfg) is what a registry factory calls.
type Option func(*options)

// WithHTTPClient makes the provider send its requests through a copy of c instead
// of its own client. A test passes the client of a loopback upstream. The copy gets
// the provider's redirect policy (the credentials are dropped on a redirect to
// another host; a CheckRedirect of c still runs after it), the response body bounds
// and the request timeout (see WithRequestTimeout) around c's transport.
func WithHTTPClient(c *http.Client) Option {
	return func(o *options) { o.httpClient = c }
}

// WithRequestTimeout sets how long a request may wait for the response headers
// before it is abandoned and reported as "Request timed out.". Without this option
// the default of 10 minutes applies. Zero disables the timeout, so only the
// caller's context governs a request. The timeout never cuts a response that has
// started: a long stream keeps running. A context deadline of the caller
// wins whenever it is earlier.
func WithRequestTimeout(d time.Duration) Option {
	return func(o *options) { o.timeout = &d }
}

// WithMaxRetries sets how many times the SDK retries a request that failed with a
// retryable status or a connection error. Without this option the SDK default
// applies (two retries, three attempts). Zero disables the
// retries.
func WithMaxRetries(n int) Option {
	return func(o *options) { o.maxRetries = &n }
}

// Provider is the Anthropic llmhub.Provider. It is immutable after New and safe for
// concurrent use.
type Provider struct {
	client       sdk.Client
	defaultModel string
	// timeout is how long a request waits for the response headers (zero: no
	// timeout), kept so a test can see the default.
	timeout time.Duration
}

// New builds the provider from cfg. It never fails today and returns an error
// only so that it matches llmhub.Factory.
//
// The client reads no environment variable (option.WithoutEnvironmentDefaults):
// the key and the base URL come from cfg alone. An empty cfg.BaseURL leaves the SDK
// default (https://api.anthropic.com), and an empty cfg.Model selects
// claude-sonnet-4-20250514. An empty APIKey is accepted: the SDK sends an
// empty x-api-key and the upstream answers with an authentication error.
//
// The SDK always gets a client built by llmtransport.NewClient, never
// http.DefaultClient: a redirect to another host drops the key, an error body is
// read up to 1 MiB and any other body that is not an event stream up to 32 MiB, and
// a request that gets no response headers within 10 minutes (WithRequestTimeout) is
// abandoned.
func New(cfg llmhub.ProviderConfig, opts ...Option) (*Provider, error) {
	var o options
	for _, opt := range opts {
		opt(&o)
	}
	ro := []option.RequestOption{
		option.WithoutEnvironmentDefaults(),
		option.WithAPIKey(cfg.APIKey),
	}
	if cfg.BaseURL != "" {
		ro = append(ro, option.WithBaseURL(cfg.BaseURL))
	}
	timeout := llmtransport.DefaultRequestTimeout
	if o.timeout != nil {
		timeout = *o.timeout
	}
	ro = append(ro, option.WithHTTPClient(llmtransport.NewClient(o.httpClient, llmtransport.Config{
		CredentialHeaders: credentialHeaders,
		RequestTimeout:    timeout,
	})))
	if o.maxRetries != nil {
		ro = append(ro, option.WithMaxRetries(*o.maxRetries))
	}
	model := cfg.Model
	if model == "" {
		model = defaultModel
	}
	return &Provider{client: sdk.NewClient(ro...), defaultModel: model, timeout: timeout}, nil
}

// Name returns "anthropic".
func (*Provider) Name() string { return providerName }

// Version returns "1.0.0".
func (*Provider) Version() string { return "1.0.0" }

// Initialize does nothing.
func (*Provider) Initialize(context.Context) error { return nil }

// Dispose does nothing.
func (*Provider) Dispose(context.Context) error { return nil }

// CountTokens returns the shared estimate (llmhub.EstimateTokens), computed without
// calling the API. A done context returns the bare
// ctx.Err() first, as llmhub.Provider requires.
func (*Provider) CountTokens(ctx context.Context, msgs []kernel.Message) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	return llmhub.EstimateTokens(msgs), nil
}
