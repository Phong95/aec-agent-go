// Package mcpoauth is the OAuth sign-in of MCP servers over HTTP (A-214; the MCP
// authorization spec, 2025-06-18) - the McpOAuth of core/ts, reached as McpHub.OAuth.
//
// A server that answers 401 needs a sign-in: the OAuth finds its authorization server
// (RFC 9728 protected-resource metadata, then RFC 8414 / OpenID metadata), registers this
// app as a client (RFC 7591) unless the definition names one, and signs in with the
// authorization code flow + PKCE S256 (a browser) or the device flow (RFC 8628, a code
// typed on another device). The tokens are kept per server (one sign-in for every agent
// that uses it), sealed with the SecretHub, and refreshed before they expire and after a
// 401. Token values never reach events, logs, states or the model.
package mcpoauth

import "slices"

// FlowInfo is a running sign-in, as Status shows it (no secrets) - McpSignInFlowInfo.
type FlowInfo struct {
	FlowID string `json:"flowId"`
	// Kind is "browser" or "device".
	Kind string `json:"kind"`
	// Status is "pending" or "failed".
	Status string `json:"status"`
	// Error is why it failed ("failed").
	Error     string `json:"error,omitempty"`
	StartedAt string `json:"startedAt"`
	ExpiresAt string `json:"expiresAt"`
	// UserCode is the code the user types at VerificationURI ("device").
	UserCode        string `json:"userCode,omitempty"`
	VerificationURI string `json:"verificationUri,omitempty"`
	// VerificationURIComplete is VerificationURI with the code already filled in ("device").
	VerificationURIComplete string `json:"verificationUriComplete,omitempty"`
}

// Status is whether a server has a sign-in, and what is known about it (never the
// tokens) - McpAuthStatus.
type Status struct {
	ServerID string `json:"serverId"`
	SignedIn bool   `json:"signedIn"`
	// Issuer is the authorization server the sign-in is with.
	Issuer string `json:"issuer,omitempty"`
	// Scope is the scopes the tokens were granted.
	Scope string `json:"scope,omitempty"`
	// ExpiresAt is when the access token expires (it is refreshed before, when there is a
	// refresh token).
	ExpiresAt string `json:"expiresAt,omitempty"`
	// Refreshable says whether the tokens can be refreshed without a new sign-in (set when
	// signed in).
	Refreshable *bool `json:"refreshable,omitempty"`
	// ClientID is the OAuth client id in use (registered or from the definition).
	ClientID string `json:"clientId,omitempty"`
	// UpdatedAt is when the sign-in was last changed (signed in, refreshed).
	UpdatedAt string `json:"updatedAt,omitempty"`
	// Pending is the sign-in that is running, or the last one that failed.
	Pending *FlowInfo `json:"pending,omitempty"`
	// Error is why the last sign-in or refresh failed.
	Error string `json:"error,omitempty"`
}

// BrowserSignIn is what StartSignIn returns: send the user's browser to
// AuthorizationURL; the server comes back to the redirect URI with ?code&state.
type BrowserSignIn struct {
	FlowID           string `json:"flowId"`
	ServerID         string `json:"serverId"`
	AuthorizationURL string `json:"authorizationUrl"`
	ExpiresAt        string `json:"expiresAt"`
}

// DeviceSignIn is what StartDeviceSignIn returns: show UserCode and VerificationURI;
// the OAuth polls until the user approves.
type DeviceSignIn struct {
	FlowID                  string `json:"flowId"`
	ServerID                string `json:"serverId"`
	UserCode                string `json:"userCode"`
	VerificationURI         string `json:"verificationUri"`
	VerificationURIComplete string `json:"verificationUriComplete,omitempty"`
	ExpiresAt               string `json:"expiresAt"`
	// IntervalSec is the seconds between polls the server asked for.
	IntervalSec float64 `json:"intervalSec"`
}

// Callback is what the redirect URI received (the query of the callback).
type Callback struct {
	State            string `json:"state"`
	Code             string `json:"code,omitempty"`
	Error            string `json:"error,omitempty"`
	ErrorDescription string `json:"errorDescription,omitempty"`
}

// ---------------------------------------------------------------------------
// What is stored (sealed) per server.
// ---------------------------------------------------------------------------

// Client is the OAuth client this app is to the authorization server.
type Client struct {
	ClientID string `json:"clientId"`
	// ClientSecret is set only when the server issued one at registration.
	ClientSecret string `json:"clientSecret,omitempty"`
	// AuthMethod is how the client proves itself at the token endpoint: "none",
	// "client_secret_basic" or "client_secret_post".
	AuthMethod string `json:"authMethod"`
	// RedirectURIs are the redirect URIs it was registered with (empty for a client id
	// from the definition).
	RedirectURIs []string `json:"redirectUris"`
	GrantTypes   []string `json:"grantTypes"`
	// Registered is true when the OAuth registered it (dynamic registration), so it can
	// register again.
	Registered bool `json:"registered"`
}

// Tokens are the tokens of a sign-in.
type Tokens struct {
	AccessToken  string `json:"accessToken"`
	TokenType    string `json:"tokenType"`
	RefreshToken string `json:"refreshToken,omitempty"`
	// ExpiresAt is an ISO time ("" = unknown).
	ExpiresAt string `json:"expiresAt,omitempty"`
	Scope     string `json:"scope,omitempty"`
}

// Data is the plain JSON sealed into persistence.McpAuthRecord.Data - McpAuthData.
type Data struct {
	// URL is the MCP url the sign-in is for: a definition with another url is not signed in.
	URL string `json:"url"`
	// Resource is the resource indicator (RFC 8707) sent with every authorization and
	// token request.
	Resource                    string  `json:"resource"`
	Issuer                      string  `json:"issuer"`
	AuthorizationEndpoint       string  `json:"authorizationEndpoint"`
	TokenEndpoint               string  `json:"tokenEndpoint"`
	DeviceAuthorizationEndpoint string  `json:"deviceAuthorizationEndpoint,omitempty"`
	RegistrationEndpoint        string  `json:"registrationEndpoint,omitempty"`
	RevocationEndpoint          string  `json:"revocationEndpoint,omitempty"`
	Scope                       string  `json:"scope,omitempty"`
	Client                      *Client `json:"client,omitempty"`
	Tokens                      *Tokens `json:"tokens,omitempty"`
	// Error is why the last refresh failed (shown by Status).
	Error     string `json:"error,omitempty"`
	UpdatedAt string `json:"updatedAt,omitempty"`
}

// Clone returns a deep copy (nil stays nil).
func (d *Data) Clone() *Data {
	if d == nil {
		return nil
	}
	c := *d
	if d.Client != nil {
		cl := *d.Client
		cl.RedirectURIs = slices.Clone(d.Client.RedirectURIs)
		cl.GrantTypes = slices.Clone(d.Client.GrantTypes)
		c.Client = &cl
	}
	if d.Tokens != nil {
		t := *d.Tokens
		c.Tokens = &t
	}
	return &c
}
