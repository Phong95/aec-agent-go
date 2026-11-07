package mcpoauth

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/Phong95/aec-agent-go/components/mcphub/mcpclient"
	"github.com/Phong95/aec-agent-go/components/mcphub/mcpserver"
)

// ---------------------------------------------------------------------------
// Finding an MCP server's authorization server (MCP authorization, 2025-06-18) - the
// discovery.ts of core/ts.
//  1. Ask the server without a token: a 401's WWW-Authenticate may name its protected
//     resource metadata (resource_metadata="...") and the scope it wants.
//  2. RFC 9728 protected resource metadata (that URL, else /.well-known/oauth-protected-resource
//     with the endpoint's path, then without): the resource and its authorization servers.
//  3. RFC 8414 / OpenID Connect metadata of the first authorization server.
//  4. A server with no metadata at all (the 2025-03-26 spec): /authorize, /token and
//     /register on the MCP server's origin.
// Every endpoint must be https (http only on a loopback host, for local servers and tests).
// ---------------------------------------------------------------------------

// Discovery is what discovery found - enough to register, sign in and refresh.
type Discovery struct {
	// Resource is the RFC 8707 resource indicator: the metadata's resource, else the
	// canonical MCP url.
	Resource                    string `json:"resource"`
	Issuer                      string `json:"issuer"`
	AuthorizationEndpoint       string `json:"authorizationEndpoint"`
	TokenEndpoint               string `json:"tokenEndpoint"`
	DeviceAuthorizationEndpoint string `json:"deviceAuthorizationEndpoint,omitempty"`
	RegistrationEndpoint        string `json:"registrationEndpoint,omitempty"`
	RevocationEndpoint          string `json:"revocationEndpoint,omitempty"`
	// Scope is the scopes to ask for (settings, else the 401's scope, else the resource's
	// scopes_supported); "" = none.
	Scope string `json:"scope,omitempty"`
	// GrantTypes and AuthMethods are what the authorization server says it supports (nil
	// = it didn't say).
	GrantTypes  []string `json:"grantTypes,omitempty"`
	AuthMethods []string `json:"authMethods,omitempty"`
}

const discoveryTimeout = 10 * time.Second

// maxDoc bounds a metadata document or token answer (1 MiB).
const maxDoc = 1 << 20

// CanonicalResource returns the url as an RFC 8707 resource: lower-case scheme and host,
// no fragment, no trailing "/" on a bare origin.
func CanonicalResource(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	u.Fragment = ""
	u.RawFragment = ""
	u.Scheme = strings.ToLower(u.Scheme)
	u.Host = strings.ToLower(u.Host)
	s := u.String()
	if (u.Path == "" || u.Path == "/") && u.RawQuery == "" {
		s = strings.TrimSuffix(s, "/")
	}
	return s
}

// CheckEndpoint returns the url when it is https (or http on a loopback host), else a
// *mcpserver.Error ("auth_failed").
func CheckEndpoint(raw, what string) (string, error) {
	if raw == "" {
		return "", authFailed("the authorization server has no " + what)
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return "", authFailed("the " + what + ` "` + raw + `" is not a valid URL`)
	}
	host := u.Hostname()
	loopback := host == "localhost" || host == "127.0.0.1" || host == "::1"
	if u.Scheme != "https" && (u.Scheme != "http" || !loopback) {
		return "", authFailed("the " + what + " must use https: " + raw)
	}
	return u.String(), nil
}

// Discover finds the authorization server of the MCP endpoint mcpURL.
func Discover(ctx context.Context, client *http.Client, mcpURL string, settings *mcpserver.OAuthSettings) (Discovery, error) {
	if client == nil {
		client = http.DefaultClient
	}
	u, err := url.Parse(mcpURL)
	if err != nil || u.Host == "" {
		return Discovery{}, authFailed(`the MCP url "` + mcpURL + `" is not a valid URL`)
	}
	origin := u.Scheme + "://" + u.Host
	challenge := probe(ctx, client, mcpURL)

	// Protected resource metadata (RFC 9728).
	path := strings.TrimRight(u.Path, "/")
	var candidates []string
	if hint := challenge["resource_metadata"]; hint != "" {
		candidates = append(candidates, hint)
	}
	if path != "" {
		candidates = append(candidates, origin+"/.well-known/oauth-protected-resource"+path)
	}
	candidates = append(candidates, origin+"/.well-known/oauth-protected-resource")
	var prm map[string]any
	for _, c := range candidates {
		if _, err := CheckEndpoint(c, "protected resource metadata URL"); err != nil {
			continue
		}
		doc := getJSON(ctx, client, c)
		if servers, ok := doc["authorization_servers"].([]any); ok && len(servers) > 0 {
			prm = doc
			break
		}
	}

	// Authorization server metadata (RFC 8414 / OpenID Connect).
	var meta map[string]any
	var issuer string
	if settings != nil && settings.AuthServerMetadataURL != "" {
		metaURL, err := CheckEndpoint(settings.AuthServerMetadataURL, "authorization server metadata URL")
		if err != nil {
			return Discovery{}, err
		}
		meta = getJSON(ctx, client, metaURL)
		if meta == nil {
			return Discovery{}, authFailed("no authorization server metadata at " + settings.AuthServerMetadataURL)
		}
		if s, ok := meta["issuer"].(string); ok && s != "" {
			issuer = s
		} else if mu, err := url.Parse(metaURL); err == nil {
			issuer = mu.Scheme + "://" + mu.Host
		}
	} else {
		issuer = origin
		if prm != nil {
			issuer = toString(prm["authorization_servers"].([]any)[0])
		}
		urls, err := metadataURLs(issuer)
		if err != nil {
			return Discovery{}, err
		}
		for _, c := range urls {
			doc := getJSON(ctx, client, c)
			if _, ok := doc["token_endpoint"].(string); ok {
				meta = doc
				break
			}
		}
	}

	d := Discovery{Issuer: issuer}
	switch {
	case meta != nil:
		if methods, ok := meta["code_challenge_methods_supported"].([]any); ok && !slices.Contains(strings1(methods), "S256") {
			return Discovery{}, authFailed("the authorization server doesn't support PKCE S256")
		}
		if d.AuthorizationEndpoint, err = CheckEndpoint(toString(meta["authorization_endpoint"]), "authorization endpoint"); err != nil {
			return Discovery{}, err
		}
		if d.TokenEndpoint, err = CheckEndpoint(toString(meta["token_endpoint"]), "token endpoint"); err != nil {
			return Discovery{}, err
		}
		for _, o := range []struct {
			key, what string
			out       *string
		}{
			{"device_authorization_endpoint", "device authorization endpoint", &d.DeviceAuthorizationEndpoint},
			{"registration_endpoint", "registration endpoint", &d.RegistrationEndpoint},
			{"revocation_endpoint", "revocation endpoint", &d.RevocationEndpoint},
		} {
			if s, ok := meta[o.key].(string); ok && s != "" {
				if *o.out, err = CheckEndpoint(s, o.what); err != nil {
					return Discovery{}, err
				}
			}
		}
		if s, ok := meta["issuer"].(string); ok && s != "" {
			d.Issuer = s
		}
	case prm == nil:
		// The 2025-03-26 fallback: the default endpoints on the MCP server's origin.
		if d.AuthorizationEndpoint, err = CheckEndpoint(origin+"/authorize", "authorization endpoint"); err != nil {
			return Discovery{}, err
		}
		d.TokenEndpoint, _ = CheckEndpoint(origin+"/token", "token endpoint")
		d.RegistrationEndpoint, _ = CheckEndpoint(origin+"/register", "registration endpoint")
	default:
		return Discovery{}, authFailed("no authorization server metadata found for " + issuer)
	}

	scope := ""
	if settings != nil {
		scope = strings.TrimSpace(settings.Scopes)
	}
	if scope == "" {
		scope = challenge["scope"]
	}
	if scope == "" {
		if s, ok := prm["scopes_supported"].([]any); ok {
			scope = strings.Join(strings1(s), " ")
		}
	}
	if scope != "" {
		if s, ok := meta["scopes_supported"].([]any); ok && slices.Contains(strings1(s), "offline_access") && !slices.Contains(strings.Fields(scope), "offline_access") {
			scope += " offline_access"
		}
	}
	d.Scope = scope
	if g, ok := meta["grant_types_supported"].([]any); ok {
		d.GrantTypes = strings1(g)
	}
	if a, ok := meta["token_endpoint_auth_methods_supported"].([]any); ok {
		d.AuthMethods = strings1(a)
	}
	if r, ok := prm["resource"].(string); ok && r != "" {
		d.Resource = r
	} else {
		d.Resource = CanonicalResource(mcpURL)
	}
	return d, nil
}

// metadataURLs are where an issuer's metadata may be (RFC 8414 with path insertion, then
// OpenID Connect).
func metadataURLs(issuer string) ([]string, error) {
	u, err := url.Parse(issuer)
	if err != nil || u.Host == "" {
		return nil, authFailed(`the authorization server "` + issuer + `" is not a valid URL`)
	}
	if _, err := CheckEndpoint(u.String(), "authorization server"); err != nil {
		return nil, err
	}
	origin := u.Scheme + "://" + u.Host
	path := strings.TrimRight(u.Path, "/")
	if path != "" {
		return []string{
			origin + "/.well-known/oauth-authorization-server" + path,
			origin + "/.well-known/openid-configuration" + path,
			origin + path + "/.well-known/openid-configuration",
		}, nil
	}
	return []string{origin + "/.well-known/oauth-authorization-server", origin + "/.well-known/openid-configuration"}, nil
}

// probe sends one unauthenticated request to read the 401 challenge; any failure = no hints.
func probe(ctx context.Context, client *http.Client, mcpURL string) map[string]string {
	ctx, cancel := context.WithTimeout(ctx, discoveryTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, mcpURL, bytes.NewReader([]byte(`{"jsonrpc":"2.0","id":0,"method":"ping"}`)))
	if err != nil {
		return map[string]string{}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	resp, err := client.Do(req)
	if err != nil {
		return map[string]string{}
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxDoc))
	if resp.StatusCode != http.StatusUnauthorized && resp.StatusCode != http.StatusForbidden {
		return map[string]string{}
	}
	return mcpclient.ParseWWWAuthenticate(resp.Header.Get("WWW-Authenticate"))
}

// getJSON GETs a JSON object; any failure = nil.
func getJSON(ctx context.Context, client *http.Client, u string) map[string]any {
	ctx, cancel := context.WithTimeout(ctx, discoveryTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil
	}
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return nil
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil
	}
	var doc map[string]any
	if json.NewDecoder(io.LimitReader(resp.Body, maxDoc)).Decode(&doc) != nil {
		return nil
	}
	return doc
}

func strings1(v []any) []string {
	out := make([]string, 0, len(v))
	for _, x := range v {
		if s, ok := x.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

func toString(v any) string {
	s, _ := v.(string)
	return s
}

func authFailed(msg string) *mcpserver.Error {
	return mcpserver.NewError(msg, mcpserver.CodeAuthFailed)
}
