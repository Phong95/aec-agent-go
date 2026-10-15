package web

import (
	"context"
	"net/http"
	"net/netip"
)

// ---------------------------------------------------------------------------
// Web search provider: the pluggable backend behind the web_search tool.
// ---------------------------------------------------------------------------

// SearchResult is one hit of a search: a title, the page URL and a short snippet.
type SearchResult struct {
	Title   string
	URL     string
	Snippet string
}

// SearchQuery is one search request.
type SearchQuery struct {
	Query      string
	MaxResults int
	// Region is an optional region code understood by the provider (for example "us-en",
	// "vn-vi"). Empty means the provider's default.
	Region string
}

// SearchProvider is a search backend, the swap point of web_search. Implementations
// return a shortlist (title, url, snippet), never full documents, so the agent picks what
// to open with web_extract. They return an error on a transport or provider failure; the
// tool turns it into an error result.
type SearchProvider interface {
	// Name is the provider's name, shown in the tool output (for example "duckduckgo").
	Name() string
	// Search runs the query. ctx carries the run's cancellation.
	Search(ctx context.Context, q SearchQuery) ([]SearchResult, error)
}

// DuckDuckGoSearchProviderOptions configure the provider. A zero field means the default.
type DuckDuckGoSearchProviderOptions struct {
	// Endpoint defaults to https://html.duckduckgo.com/html/. It is set by the developer
	// and is not SSRF-guarded; it is never model input.
	Endpoint string
	// TimeoutMs is the request timeout in milliseconds. Default 10 000.
	TimeoutMs int
	// Region is the default region (DuckDuckGo kl, for example "us-en").
	Region    string
	UserAgent string
	// Transport carries the request. Nil means a plain transport that reads no proxy
	// variable. Tests inject a recording round tripper.
	Transport http.RoundTripper
}

// ExtractToolOptions configure web_extract. A zero field means the default: a zero cannot
// be told from an absent field, so only MaxRedirects, where an explicit 0 matters, is a
// pointer.
type ExtractToolOptions struct {
	// MaxUrls is how many URLs one call accepts. Default 5.
	MaxUrls int
	// TimeoutMs is the per-page timeout in milliseconds, covering every redirect hop and
	// the body read. Default 15 000.
	TimeoutMs int
	// MaxBytes is how many raw bytes of a page are read before the body is cut. Default
	// 2 000 000.
	MaxBytes int
	// DefaultMaxChars is how many readable characters a page returns when the model does not
	// ask. Default 20 000.
	DefaultMaxChars int
	// MaxCharsLimit is the hard ceiling on the characters returned per page. Default 50 000
	// (at least 1 000).
	MaxCharsLimit int
	// MaxRedirects is how many redirect hops are followed per page, each one re-validated
	// by the SSRF guard. Nil means 5; a pointer to 0 follows none.
	MaxRedirects *int
	// UserAgent is the User-Agent header. Default "AECAgent/1.0 (+web_extract)".
	UserAgent string
	// AllowPrivateNetwork allows private and loopback targets. Trusted deployments only: it
	// disables the pre-check and the dial-time check of the default transport.
	AllowPrivateNetwork bool
	// Transport carries every request. Nil means the hardened transport (it refuses a
	// non-public address at dial time). A custom Transport replaces that dial-time check, so
	// it is for tests and trusted hosts only; the pre-check of every URL and redirect hop
	// still runs.
	Transport http.RoundTripper
	// Resolver is the name lookup of the pre-check. Nil means net.DefaultResolver.
	Resolver Resolver
}

// SearchToolOptions configure web_search. A zero field means the default, because a zero
// cannot be told from an absent field.
type SearchToolOptions struct {
	// Provider is the search backend. Nil means DuckDuckGo (free, no API key).
	Provider SearchProvider
	// DefaultMaxResults is the result count when the model does not ask for one. Default 5.
	DefaultMaxResults int
	// MaxResultsLimit is the hard ceiling on results per call. Default 10.
	MaxResultsLimit int
}

// Resolver is the injectable name lookup of the guard. *net.Resolver
// satisfies it.
type Resolver interface {
	LookupNetIP(ctx context.Context, network, host string) ([]netip.Addr, error)
}

// HTTPRequestToolOptions configure http_request (A-101). A zero field means the default;
// only MaxRedirects, where an explicit 0 matters, is a pointer.
type HTTPRequestToolOptions struct {
	// TimeoutMs is the whole-request timeout in milliseconds, covering redirects and the
	// body read. Default 30 000.
	TimeoutMs int
	// MaxBytes is how many response bytes are read before the body is cut. Default
	// 2 000 000.
	MaxBytes int
	// MaxChars is how many response characters are returned to the model. Default 20 000.
	MaxChars int
	// MaxBodyBytes is the largest request body accepted. Default 1 000 000.
	MaxBodyBytes int
	// MaxRedirects is how many redirect hops are followed, each one re-validated by the
	// SSRF guard. Nil means 5; a pointer to 0 follows none.
	MaxRedirects *int
	// UserAgent is the User-Agent header. Default "aec-agent/1.0 (+http_request)".
	UserAgent string
	// AllowPrivateNetwork allows private and loopback targets. Trusted deployments only.
	AllowPrivateNetwork bool
	// Transport carries every request. Nil means the hardened transport (it refuses a
	// non-public address at dial time); a custom one is for tests and trusted hosts only
	// (the pre-check of every URL and redirect hop still runs).
	Transport http.RoundTripper
	// Resolver is the name lookup of the pre-check. Nil means net.DefaultResolver.
	Resolver Resolver
}
