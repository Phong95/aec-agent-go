package openaicompat

import (
	"context"
	"net/http"
	"time"

	sdk "github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"

	"github.com/Phong95/aec-agent-go/components/llmhub"
	"github.com/Phong95/aec-agent-go/internal/llmtransport"
	"github.com/Phong95/aec-agent-go/kernel"
)

// Compile-time proof that Provider is an llmhub.Provider and an llmhub.ModelLister.
var (
	_ llmhub.Provider    = (*Provider)(nil)
	_ llmhub.ModelLister = (*Provider)(nil)
)

const (
	// defaultName is the name a provider built without WithName reports.
	defaultName = "openai"
	// defaultBaseURL is the host the provider talks to when it is given no base URL.
	defaultBaseURL = "https://api.openai.com/v1"
	// defaultModel is the model a provider built without cfg.Model uses.
	defaultModel = "gpt-4o-mini"
)

// An empty API key is accepted at construction, as the other three providers do
// (see New); the package never reads OPENAI_API_KEY or OPENAI_ADMIN_KEY.

// options collects what the Option functions set before New builds the services.
type options struct {
	name       string
	httpClient *http.Client
	maxRetries *int
	timeout    *time.Duration
}

// credentialHeaders are the headers that carry the credential of a request and must
// not follow a redirect to another host. net/http drops Authorization on its own;
// the organisation and project headers are not secrets of the same rank but are
// private to the account, and a gateway may add its own key header next to them.
var credentialHeaders = []string{"Authorization", "OpenAI-Organization", "OpenAI-Project"}

// Option changes how New builds the provider. Options exist for tests and advanced
// use; a plain New(cfg) is what a registry factory calls.
type Option func(*options)

// WithName sets the name the provider reports, which is also the provider name
// its model listing uses. The default is "openai"; the "novita" registry entry
// passes "novita".
func WithName(name string) Option {
	return func(o *options) { o.name = name }
}

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

// Provider is the OpenAI-compatible llmhub.Provider. It is immutable after New and
// safe for concurrent use.
type Provider struct {
	name         string
	baseURL      string
	defaultModel string
	// timeout is how long a request waits for the response headers (zero: no
	// timeout), kept so a test can see the default.
	timeout time.Duration
	chat    sdk.ChatCompletionService
	models  sdk.ModelService
}

// New builds the provider from cfg.
//
// An empty cfg.APIKey is accepted: a local OpenAI-compatible server (Ollama, LM Studio,
// llama.cpp, vLLM) needs no key, and the request then carries no Authorization
// header at all rather than an empty bearer. An empty cfg.BaseURL selects
// https://api.openai.com/v1 and an empty cfg.Model selects gpt-4o-mini.
//
// The services are built directly from the explicit options and not from
// openai.NewClient, which applies the environment defaults of the SDK (see the
// package comment): OPENAI_* variables never change what is sent.
//
// The services always get a client built by llmtransport.NewClient, never
// http.DefaultClient: a redirect to another host drops the credential headers, an
// error body is read up to 1 MiB and any other body that is not an event stream up
// to 32 MiB, and a request that gets no response headers within 10 minutes
// (WithRequestTimeout) is abandoned.
func New(cfg llmhub.ProviderConfig, opts ...Option) (*Provider, error) {
	var o options
	for _, opt := range opts {
		opt(&o)
	}
	name := o.name
	if name == "" {
		name = defaultName
	}
	base := cfg.BaseURL
	if base == "" {
		base = defaultBaseURL
	}
	ro := []option.RequestOption{option.WithBaseURL(base)}
	if cfg.APIKey != "" {
		ro = append(ro, option.WithAPIKey(cfg.APIKey))
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
	return &Provider{
		name:         name,
		baseURL:      base,
		defaultModel: model,
		timeout:      timeout,
		chat:         sdk.NewChatCompletionService(ro...),
		models:       sdk.NewModelService(ro...),
	}, nil
}

// Name returns the name given by WithName, "openai" by default.
func (p *Provider) Name() string { return p.name }

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
