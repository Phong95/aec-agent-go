package gemini

import (
	"context"
	"net/http"
	"strings"
	"time"

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
	// APIURL is the default Gemini API endpoint; ProviderConfig.BaseURL replaces it.
	APIURL = "https://generativelanguage.googleapis.com/v1beta"
	// SkipSignature is the thought signature sent with a function call that has none
	// (a call another provider wrote, or one saved before A-162). Gemini 3 rejects a
	// function call without a signature; Google documents this value as the one that
	// skips the check.
	SkipSignature = "skip_thought_signature_validator"

	// providerName is the name the provider reports and registers under.
	providerName = "gemini"
	// defaultModel is the model a provider built without cfg.Model uses.
	defaultModel = "gemini-2.5-flash"
)

// options collects what the Option functions set before New builds the provider.
type options struct {
	httpClient *http.Client
	timeout    *time.Duration
	idle       *time.Duration
}

// Option changes how New builds the provider. Options exist for tests and advanced
// use; a plain New(cfg) is what a registry factory calls.
type Option func(*options)

// WithHTTPClient makes the provider send its requests through a copy of c instead
// of its own client. A test passes the client of a loopback upstream. The copy gets
// the provider's redirect policy (the API key is dropped on a redirect to another
// host); a CheckRedirect of c still runs after it.
func WithHTTPClient(c *http.Client) Option {
	return func(o *options) { o.httpClient = c }
}

// WithRequestTimeout sets how long a request may wait for the response headers
// before it is abandoned (a retryable timeout error). Without this option
// llmtransport.DefaultRequestTimeout applies. Zero disables the timeout.
func WithRequestTimeout(d time.Duration) Option {
	return func(o *options) { o.timeout = &d }
}

// WithStreamIdleTimeout sets the longest silence between two reads of the event
// stream; when it expires the stream ends with a retryable timeout error chunk.
// Without this option llmtransport.DefaultStreamIdleTimeout applies. Zero disables it.
func WithStreamIdleTimeout(d time.Duration) Option {
	return func(o *options) { o.idle = &d }
}

// Provider is the Gemini llmhub.Provider. It is safe for concurrent use: nothing in
// it changes after New.
type Provider struct {
	client  *http.Client
	apiKey  string
	baseURL string
	model   string
}

// New builds the provider from cfg: the API key is cfg.APIKey, the endpoint
// cfg.BaseURL (APIURL when empty, a trailing slash dropped) and an empty cfg.Model
// selects gemini-2.5-flash. An empty key is accepted: the API answers with an
// authentication error.
func New(cfg llmhub.ProviderConfig, opts ...Option) (*Provider, error) {
	var o options
	for _, opt := range opts {
		opt(&o)
	}
	p := &Provider{
		apiKey:  cfg.APIKey,
		baseURL: strings.TrimRight(cfg.BaseURL, "/"),
		model:   cfg.Model,
	}
	if p.baseURL == "" {
		p.baseURL = APIURL
	}
	if p.model == "" {
		p.model = defaultModel
	}
	timeout := llmtransport.DefaultRequestTimeout
	if o.timeout != nil {
		timeout = *o.timeout
	}
	idle := llmtransport.DefaultStreamIdleTimeout
	if o.idle != nil {
		idle = *o.idle
	}
	p.client = llmtransport.NewClient(o.httpClient, llmtransport.Config{
		CredentialHeaders: credentialHeaders,
		RequestTimeout:    timeout,
		IdleTimeout:       idle,
	})
	return p, nil
}

// credentialHeaders are the headers that carry the credentials and must not follow
// a redirect to another host.
var credentialHeaders = []string{"X-Goog-Api-Key"}

// Name returns "gemini".
func (*Provider) Name() string { return providerName }

// Version returns "1.0.0".
func (*Provider) Version() string { return "1.0.0" }

// Initialize does nothing.
func (*Provider) Initialize(context.Context) error { return nil }

// Dispose does nothing.
func (*Provider) Dispose(context.Context) error { return nil }

// CountTokens returns the shared estimate (llmhub.EstimateTokens), computed without
// calling the API. A done context returns the bare ctx.Err() first, as
// llmhub.Provider requires.
func (*Provider) CountTokens(ctx context.Context, msgs []kernel.Message) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	return llmhub.EstimateTokens(msgs), nil
}

// modelURL is the URL of method on the model of the call: "models/x" and
// "tunedModels/x" are full resource names, a bare id is a base model.
func (p *Provider) modelURL(model, method string) string {
	if model == "" {
		model = p.model
	}
	if !strings.Contains(model, "/") {
		model = "models/" + model
	}
	return p.baseURL + "/" + model + ":" + method
}

// headers are the headers of a request: the API key, and the JSON content type when
// the request has a body.
func (p *Provider) headers(body bool) http.Header {
	h := http.Header{}
	h.Set("X-Goog-Api-Key", p.apiKey)
	if body {
		h.Set("Content-Type", "application/json")
	}
	return h
}
