package openaisub

import (
	"context"
	"net/http"
	"sync"
	"time"

	"github.com/Phong95/aec-agent-go/components/llmhub"
	"github.com/Phong95/aec-agent-go/internal/llmtransport"
	"github.com/Phong95/aec-agent-go/kernel"
)

// Compile-time proof that Provider is an llmhub.Provider, an llmhub.ModelLister and an
// llmhub.TokenUpdater.
var (
	_ llmhub.Provider     = (*Provider)(nil)
	_ llmhub.ModelLister  = (*Provider)(nil)
	_ llmhub.TokenUpdater = (*Provider)(nil)
)

const (
	// ResponsesURL is the Responses endpoint.
	ResponsesURL = "https://chatgpt.com/backend-api/codex/responses"
	// ModelsURL is the model list endpoint. ListModels adds the client_version query.
	ModelsURL = "https://chatgpt.com/backend-api/codex/models"
	// CodexClientVersion is the Codex client version ListModels sends as the
	// client_version query: the endpoint requires it (a request without it is
	// rejected) and lists the models whose minimal_client_version it meets. Raise it
	// when newer models stop being listed.
	CodexClientVersion = "1.0.0"

	// providerName is the name the provider reports and registers under.
	providerName = "openai-subscription"
	// defaultModel is the model a provider built without cfg.Model uses.
	defaultModel = "gpt-5.6-terra"
)

// creds is the OAuth access token and the ChatGPT account id it belongs to. The two
// always travel together: they are read and written as one value.
type creds struct {
	token   string
	account string
}

// options collects what the Option functions set before New builds the provider.
type options struct {
	httpClient   *http.Client
	responsesURL string
	modelsURL    string
	timeout      *time.Duration
	idle         *time.Duration
}

// Option changes how New builds the provider. Options exist for tests and advanced
// use; a plain New(cfg) is what a registry factory calls.
type Option func(*options)

// WithEndpoints makes the provider call responsesURL and modelsURL instead of the
// ChatGPT endpoints. It exists for tests, which point the provider at a loopback
// server. The URLs are never derived from llmhub.ProviderConfig: the bearer token and
// the account id go wherever these URLs point, so only code that builds the
// provider (not a configuration file) may choose them. An empty URL keeps the
// default of that endpoint.
func WithEndpoints(responsesURL, modelsURL string) Option {
	return func(o *options) {
		o.responsesURL = responsesURL
		o.modelsURL = modelsURL
	}
}

// WithHTTPClient makes the provider send its requests through a copy of c instead
// of its own client. A test passes the client of a loopback upstream. The copy gets
// the provider's redirect policy (the credentials are dropped on a redirect to
// another host); a CheckRedirect of c still runs after it. The response body bounds,
// the request timeout and the stream idle timeout are put around c's transport.
func WithHTTPClient(c *http.Client) Option {
	return func(o *options) { o.httpClient = c }
}

// WithRequestTimeout sets how long a request may wait for the response headers
// before it is abandoned (a retryable timeout error). Without this option the 10
// minutes the other providers use apply (llmtransport.DefaultRequestTimeout). Zero
// disables the timeout. The timeout never cuts a response that has started; a
// context deadline of the caller wins whenever it is earlier.
func WithRequestTimeout(d time.Duration) Option {
	return func(o *options) { o.timeout = &d }
}

// WithStreamIdleTimeout sets the longest silence between two reads of the event
// stream; when it expires the stream ends with a retryable timeout error chunk.
// Without this option llmtransport.DefaultStreamIdleTimeout (5 minutes) applies.
// Zero disables it, so a stream that stalls lives as long as the caller's context.
func WithStreamIdleTimeout(d time.Duration) Option {
	return func(o *options) { o.idle = &d }
}

// Provider is the ChatGPT-subscription llmhub.Provider. It is safe for concurrent use,
// including UpdateTokens while requests are in flight.
//
// One RWMutex guards the token and the account id together. A request takes one
// snapshot of the pair and builds every header from it, so a request never carries
// a token with another token's account id.
type Provider struct {
	mu    sync.RWMutex
	creds creds

	client       *http.Client
	responsesURL string
	modelsURL    string
	model        string
	// timeout and idle are the request and stream idle timeouts (zero: none), kept
	// so a test can see the defaults.
	timeout time.Duration
	idle    time.Duration
}

// New builds the provider from cfg. The access token is cfg.OAuthAccessToken and
// the account id cfg.ChatgptAccountID (an empty account id sends no header); an
// empty cfg.Model selects gpt-5.6-terra. An empty token is accepted: the endpoint
// answers with an authentication error.
//
// cfg.BaseURL is ignored: honouring it could send the bearer token somewhere else.
//
// The provider builds its client with llmtransport.NewClient, as the other providers
// do (OQ6): a redirect to
// another host drops the credential headers, an error body is read up to 1 MiB, a
// request that gets no response headers within 10 minutes (WithRequestTimeout) is
// abandoned, and a stream that sends nothing for 5 minutes (WithStreamIdleTimeout)
// is ended. There is no Client.Timeout: a stream that keeps sending stays open as
// long as the server writes, within the caller's context.
func New(cfg llmhub.ProviderConfig, opts ...Option) (*Provider, error) {
	var o options
	for _, opt := range opts {
		opt(&o)
	}
	p := &Provider{
		creds:        creds{token: cfg.OAuthAccessToken, account: cfg.ChatgptAccountID},
		responsesURL: ResponsesURL,
		modelsURL:    ModelsURL,
		model:        cfg.Model,
	}
	if p.model == "" {
		p.model = defaultModel
	}
	if o.responsesURL != "" {
		p.responsesURL = o.responsesURL
	}
	if o.modelsURL != "" {
		p.modelsURL = o.modelsURL
	}
	p.timeout = llmtransport.DefaultRequestTimeout
	if o.timeout != nil {
		p.timeout = *o.timeout
	}
	p.idle = llmtransport.DefaultStreamIdleTimeout
	if o.idle != nil {
		p.idle = *o.idle
	}
	// NewClient copies o.httpClient (a nil one gets a clone of the default
	// transport, which sets no timeout of its own) and installs the redirect policy
	// (llmtransport.CheckRedirect with credentialHeaders), the body bounds and both
	// timeouts.
	p.client = llmtransport.NewClient(o.httpClient, llmtransport.Config{
		CredentialHeaders: credentialHeaders,
		RequestTimeout:    p.timeout,
		IdleTimeout:       p.idle,
	})
	return p, nil
}

// maxRedirects is the redirect limit of net/http's default policy.
const maxRedirects = llmtransport.MaxRedirects

// credentialHeaders are the headers that carry the credentials (and the request's
// private context) and must not follow a redirect to another host. net/http drops
// Authorization on a redirect to another domain but forwards the others.
var credentialHeaders = []string{"Authorization", "ChatGPT-Account-Id", "OpenAI-Beta", "Originator"}

// The redirect policy is the shared one of the provider leaves
// (llmtransport.CheckRedirect, installed by llmtransport.NewClient) with the
// credential headers of this provider: it follows redirects as net/http does by
// default (at most ten), but removes every credential header when the target has
// another scheme or host than the first request: a redirect can come from anyone who
// answers the request, and the bearer token and the account id are for the ChatGPT
// endpoint only. The policy of a client given with WithHTTPClient runs after.

// UpdateTokens swaps the access token and the account id as one pair. It implements
// llmhub.TokenUpdater.
//
// Hot-swap OAuth tokens without reconstructing the provider - used by the server's
// token refresh middleware once a stale token has been refreshed. A request that
// starts after the call uses both new values; a request that already took its
// snapshot keeps the old pair.
func (p *Provider) UpdateTokens(accessToken, accountID string) {
	p.mu.Lock()
	p.creds = creds{token: accessToken, account: accountID}
	p.mu.Unlock()
}

// snapshot returns the current token and account id as one value. Every request
// calls it once and builds all its headers from the result: reading the two fields
// in separate steps could pair a token with the account id of another UpdateTokens
// call.
func (p *Provider) snapshot() creds {
	p.mu.RLock()
	c := p.creds
	p.mu.RUnlock()
	return c
}

// Name returns "openai-subscription".
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
