package mcptest

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"

	"github.com/Phong95/aec-agent-go/components/mcphub/mcpclient"
	"github.com/Phong95/aec-agent-go/internal/jsonx"
)

// OAuthOptions configure an OAuthServer.
type OAuthOptions struct {
	// ExpiresIn is the expires_in of the tokens it issues, in seconds (0 = 3600).
	ExpiresIn float64
	// NoMetadata makes every metadata URL answer 404 (the 2025-03-26 fallback).
	NoMetadata bool
}

// OAuthServer is a fake OAuth authorization server (/as/...) and an MCP endpoint (/mcp)
// that wants its bearer token, on one loopback port (A-214) - the startAuthServer of core/ts
// tests/unit/mcp-oauth.test.ts. The "user" approves every browser sign-in at once
// (/as/authorize redirects back with a code). It is safe for concurrent use.
type OAuthServer struct {
	// Base is http://127.0.0.1:port; URL is the MCP endpoint (Base + "/mcp").
	Base, URL string

	srv  *httptest.Server
	mcp  *Server
	opts OAuthOptions

	mu             sync.Mutex
	valid          map[string]bool
	refresh        map[string]string
	codes          map[string]oauthCode
	clients        map[string]map[string]any
	grants         []string
	resources      []string
	seenAuth       []string
	deviceApproved bool
	refuseRefresh  bool
	n              int
}

type oauthCode struct{ challenge, redirect, clientID string }

// StartOAuth starts the fake; Close it when done.
func StartOAuth(opts OAuthOptions) *OAuthServer {
	if opts.ExpiresIn == 0 {
		opts.ExpiresIn = 3600
	}
	s := &OAuthServer{
		mcp: New(func(mcpclient.Message) {}), opts: opts,
		valid: map[string]bool{}, refresh: map[string]string{}, codes: map[string]oauthCode{}, clients: map[string]map[string]any{},
	}
	s.srv = httptest.NewServer(http.HandlerFunc(s.serve))
	s.Base = s.srv.URL
	s.URL = s.Base + "/mcp"
	return s
}

// Close stops the fake.
func (s *OAuthServer) Close() { s.srv.Close() }

// RevokeAll makes every access token invalid (the next MCP request gets 401).
func (s *OAuthServer) RevokeAll() { s.mu.Lock(); s.valid = map[string]bool{}; s.mu.Unlock() }

// SetRefuseRefresh makes refresh_token grants answer invalid_grant.
func (s *OAuthServer) SetRefuseRefresh(v bool) { s.mu.Lock(); s.refuseRefresh = v; s.mu.Unlock() }

// ApproveDevice approves the pending device sign-in.
func (s *OAuthServer) ApproveDevice() { s.mu.Lock(); s.deviceApproved = true; s.mu.Unlock() }

// Grants is the grant_type of every token request, in order.
func (s *OAuthServer) Grants() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.grants...)
}

// ResetGrants forgets the grants seen so far.
func (s *OAuthServer) ResetGrants() { s.mu.Lock(); s.grants = nil; s.mu.Unlock() }

// Resources is the resource parameter of every token request.
func (s *OAuthServer) Resources() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.resources...)
}

// LastAuth is the Authorization header of the last MCP request.
func (s *OAuthServer) LastAuth() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.seenAuth) == 0 {
		return ""
	}
	return s.seenAuth[len(s.seenAuth)-1]
}

// Clients is the registrations so far, by client id ("client-1", ...).
func (s *OAuthServer) Clients() map[string]map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]map[string]any, len(s.clients))
	for k, v := range s.clients {
		out[k] = v
	}
	return out
}

func (s *OAuthServer) issueLocked() map[string]any {
	s.n++
	access := "at-" + strconv.Itoa(s.n)
	refresh := "rt-" + strconv.Itoa(s.n)
	s.valid[access] = true
	s.refresh[refresh] = access
	return map[string]any{"access_token": access, "token_type": "Bearer", "expires_in": s.opts.ExpiresIn, "refresh_token": refresh, "scope": "mcp.tools offline_access"}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	b, err := jsonx.Marshal(v)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(b)
}

func (s *OAuthServer) serve(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.URL.Path == "/mcp":
		s.serveMCP(w, r)
		return
	case s.opts.NoMetadata:
		w.WriteHeader(http.StatusNotFound)
		return
	}
	switch r.URL.Path {
	case "/.well-known/oauth-protected-resource/mcp":
		writeJSON(w, 200, map[string]any{"resource": s.URL, "authorization_servers": []string{s.Base + "/as"}, "scopes_supported": []string{"mcp.tools"}})
	case "/.well-known/oauth-authorization-server/as":
		writeJSON(w, 200, map[string]any{
			"issuer":                                s.Base + "/as",
			"authorization_endpoint":                s.Base + "/as/authorize",
			"token_endpoint":                        s.Base + "/as/token",
			"registration_endpoint":                 s.Base + "/as/register",
			"device_authorization_endpoint":         s.Base + "/as/device",
			"code_challenge_methods_supported":      []string{"S256"},
			"scopes_supported":                      []string{"mcp.tools", "offline_access"},
			"grant_types_supported":                 []string{"authorization_code", "refresh_token", "urn:ietf:params:oauth:grant-type:device_code"},
			"token_endpoint_auth_methods_supported": []string{"none"},
		})
	case "/as/register":
		var reg map[string]any
		_ = json.NewDecoder(r.Body).Decode(&reg)
		s.mu.Lock()
		id := "client-" + strconv.Itoa(len(s.clients)+1)
		s.clients[id] = reg
		s.mu.Unlock()
		out := map[string]any{"client_id": id}
		for k, v := range reg {
			out[k] = v
		}
		writeJSON(w, 201, out)
	case "/as/authorize":
		q := r.URL.Query()
		s.mu.Lock()
		s.n++
		code := "code-" + strconv.Itoa(s.n)
		s.codes[code] = oauthCode{challenge: q.Get("code_challenge"), redirect: q.Get("redirect_uri"), clientID: q.Get("client_id")}
		s.mu.Unlock()
		back, _ := url.Parse(q.Get("redirect_uri"))
		bq := back.Query()
		bq.Set("code", code)
		bq.Set("state", q.Get("state"))
		back.RawQuery = bq.Encode()
		http.Redirect(w, r, back.String(), http.StatusFound) //nolint:gosec // a test fake: it sends the browser back to the redirect_uri it was given, like a real authorization server
	case "/as/device":
		writeJSON(w, 200, map[string]any{"device_code": "dev-1", "user_code": "ABCD-EFGH", "verification_uri": s.Base + "/as/activate", "expires_in": 60, "interval": 0.02})
	case "/as/token":
		s.serveToken(w, r)
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func (s *OAuthServer) serveToken(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	grant := r.PostForm.Get("grant_type")
	s.mu.Lock()
	defer s.mu.Unlock()
	s.grants = append(s.grants, grant)
	s.resources = append(s.resources, r.PostForm.Get("resource"))
	switch grant {
	case "authorization_code":
		c, ok := s.codes[r.PostForm.Get("code")]
		if !ok {
			writeJSON(w, 400, map[string]any{"error": "invalid_grant"})
			return
		}
		delete(s.codes, r.PostForm.Get("code"))
		sum := sha256.Sum256([]byte(r.PostForm.Get("code_verifier")))
		if base64.RawURLEncoding.EncodeToString(sum[:]) != c.challenge || r.PostForm.Get("redirect_uri") != c.redirect || r.PostForm.Get("client_id") != c.clientID {
			writeJSON(w, 400, map[string]any{"error": "invalid_grant", "error_description": "PKCE or redirect mismatch"})
			return
		}
		writeJSON(w, 200, s.issueLocked())
	case "refresh_token":
		old, ok := s.refresh[r.PostForm.Get("refresh_token")]
		if !ok || s.refuseRefresh {
			writeJSON(w, 400, map[string]any{"error": "invalid_grant"})
			return
		}
		delete(s.refresh, r.PostForm.Get("refresh_token"))
		delete(s.valid, old)
		writeJSON(w, 200, s.issueLocked())
	case "urn:ietf:params:oauth:grant-type:device_code":
		if !s.deviceApproved {
			writeJSON(w, 400, map[string]any{"error": "authorization_pending"})
			return
		}
		writeJSON(w, 200, s.issueLocked())
	default:
		writeJSON(w, 400, map[string]any{"error": "unsupported_grant_type"})
	}
}

func (s *OAuthServer) serveMCP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	auth := r.Header.Get("Authorization")
	body, _ := io.ReadAll(r.Body)
	s.mu.Lock()
	s.seenAuth = append(s.seenAuth, auth)
	ok := s.valid[strings.TrimPrefix(auth, "Bearer ")]
	s.mu.Unlock()
	if !ok {
		w.Header().Set("WWW-Authenticate", `Bearer resource_metadata="`+s.Base+`/.well-known/oauth-protected-resource/mcp", scope="mcp.tools"`)
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	var msg mcpclient.Message
	if json.Unmarshal(body, &msg) != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	reply, has := s.mcp.Handle(msg)
	if !has {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	writeJSON(w, 200, reply)
}
