package mcpoauth

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Phong95/aec-agent-go/components/events"
	"github.com/Phong95/aec-agent-go/components/mcphub/mcpclient"
	"github.com/Phong95/aec-agent-go/components/mcphub/mcpserver"
	"github.com/Phong95/aec-agent-go/components/persistence"
	"github.com/Phong95/aec-agent-go/internal/jsonx"
	"github.com/Phong95/aec-agent-go/internal/nilcheck"
)

// Sealer seals the stored sign-ins (the SecretHub's Seal / Open).
type Sealer interface {
	Seal(ctx context.Context, plain, aad string) (string, error)
	Open(ctx context.Context, stored, aad string) (string, error)
}

// Options configure an OAuth.
type Options struct {
	// GetServer looks a server up (McpHub.Get).
	GetServer func(ctx context.Context, id string) (mcpserver.Server, bool, error)
	// GetStore is resolved on every load, so AgentCore.UsePersistence can swap the store.
	GetStore func() persistence.McpAuthStore
	// Sealer seals the sign-ins; nil = stored as plain JSON (tests, hosts without a
	// SecretHub).
	Sealer Sealer
	// Events receives mcp-server:auth-changed; nil = no event.
	Events events.Bus
	// Client is the HTTP client of the OAuth requests (default http.DefaultClient).
	Client *http.Client
	// ClientName is the client_name sent at dynamic registration (default "AEC Agent").
	ClientName string
	// FlowTTL is how long a browser sign-in may take (default 10 minutes).
	FlowTTL time.Duration
	Logger  *slog.Logger
}

const (
	deviceGrant = "urn:ietf:params:oauth:grant-type:device_code"
	// refreshEarly: an access token this close to expiry is refreshed before it is used.
	refreshEarly   = 60 * time.Second
	defaultFlowTTL = 10 * time.Minute
	requestTimeout = 15 * time.Second
)

var errNoStore = errors.New("mcpoauth: the persistence MCP auth store is not configured")

type flow struct {
	info         FlowInfo
	serverID     string
	url          string
	discovery    Discovery
	client       Client
	redirectURI  string
	verifier     string
	deviceCode   string
	interval     time.Duration
	expires      time.Time
	cancel       context.CancelFunc
	done         bool
	pollFinished chan struct{}
}

type cacheEntry struct{ data *Data }

type refreshCall struct {
	done chan struct{}
	ok   bool
}

// OAuth is the OAuth sign-ins of the McpHub's HTTP servers (A-214) - McpHub.OAuth. One
// sign-in per server, used by every agent that connects to it: the MCP manager hands each
// HTTP connection AuthFor(server), which puts the access token on every request, refreshes
// it before it expires, and once after a 401. A sign-in that can't be refreshed any more
// is dropped and announced (mcp-server:auth-changed, SignedIn false); a new one is
// announced too, and each MCP manager reconnects the server. Hosts drive the sign-in:
// StartSignIn (browser: send the user to AuthorizationURL, pass the redirect's query to
// FinishSignIn) or StartDeviceSignIn (show the code; this polls until it is approved).
// It is safe for concurrent use.
type OAuth struct {
	o Options

	mu         sync.Mutex
	cache      map[string]cacheEntry
	generation uint64
	flows      map[string]*flow
	refreshing map[string]*refreshCall
	writeLocks map[string]*sync.Mutex
}

// New returns an OAuth.
func New(o Options) *OAuth {
	if o.Client == nil {
		o.Client = http.DefaultClient
	}
	if o.ClientName == "" {
		o.ClientName = "AEC Agent"
	}
	if o.FlowTTL <= 0 {
		o.FlowTTL = defaultFlowTTL
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	return &OAuth{
		o:          o,
		cache:      map[string]cacheEntry{},
		flows:      map[string]*flow{},
		refreshing: map[string]*refreshCall{},
		writeLocks: map[string]*sync.Mutex{},
	}
}

// Status returns whether the server is signed in, and the running sign-in. It returns a
// *mcpserver.Error ("not_found", "invalid_argument").
func (a *OAuth) Status(ctx context.Context, serverID string) (Status, error) {
	server, err := a.requireHTTP(ctx, serverID)
	if err != nil {
		return Status{}, err
	}
	data, err := a.load(ctx, serverID)
	if err != nil {
		return Status{}, err
	}
	out := Status{ServerID: serverID}
	var tokens *Tokens
	if data != nil && data.Tokens != nil && data.URL == server.Transport.URL {
		tokens = data.Tokens
	}
	if tokens != nil {
		out.SignedIn = true
		out.Issuer = data.Issuer
		out.Scope = tokens.Scope
		if out.Scope == "" {
			out.Scope = data.Scope
		}
		out.ExpiresAt = tokens.ExpiresAt
		refreshable := tokens.RefreshToken != ""
		out.Refreshable = &refreshable
	}
	if s := server.Transport.OAuth; s != nil && s.ClientID != "" {
		out.ClientID = s.ClientID
	} else if data != nil && data.Client != nil {
		out.ClientID = data.Client.ClientID
	}
	if data != nil {
		out.UpdatedAt = data.UpdatedAt
		out.Error = data.Error
	}
	if f := a.flowOf(serverID); f != nil {
		info := f.info
		out.Pending = &info
	}
	return out, nil
}

// StartSignIn starts a browser sign-in: discovery, client registration (if needed),
// PKCE. The authorization server sends the browser back to redirectURI with ?code&state,
// which the host passes to FinishSignIn. A new sign-in replaces a running one.
func (a *OAuth) StartSignIn(ctx context.Context, serverID, redirectURI string) (BrowserSignIn, error) {
	server, err := a.requireHTTP(ctx, serverID)
	if err != nil {
		return BrowserSignIn{}, err
	}
	redirect, err := checkRedirect(redirectURI)
	if err != nil {
		return BrowserSignIn{}, err
	}
	d, err := Discover(ctx, a.o.Client, server.Transport.URL, server.Transport.OAuth)
	if err != nil {
		return BrowserSignIn{}, err
	}
	client, err := a.ensureClient(ctx, server, d, redirect, []string{"authorization_code", "refresh_token"})
	if err != nil {
		return BrowserSignIn{}, err
	}
	verifier := randomString(48)
	state := randomString(24)
	au, err := url.Parse(d.AuthorizationEndpoint)
	if err != nil {
		return BrowserSignIn{}, authFailed("the authorization endpoint is not a valid URL")
	}
	q := au.Query()
	q.Set("response_type", "code")
	q.Set("client_id", client.ClientID)
	q.Set("redirect_uri", redirect)
	q.Set("code_challenge", challengeOf(verifier))
	q.Set("code_challenge_method", "S256")
	q.Set("state", state)
	q.Set("resource", d.Resource)
	if d.Scope != "" {
		q.Set("scope", d.Scope)
	}
	au.RawQuery = q.Encode()
	now := time.Now()
	expires := now.Add(a.o.FlowTTL)
	f := &flow{
		info:        FlowInfo{FlowID: state, Kind: "browser", Status: "pending", StartedAt: iso(now), ExpiresAt: iso(expires)},
		serverID:    serverID,
		url:         server.Transport.URL,
		discovery:   d,
		client:      client,
		redirectURI: redirect,
		verifier:    verifier,
		expires:     expires,
	}
	a.CancelSignIn(serverID)
	a.mu.Lock()
	a.flows[state] = f
	a.mu.Unlock()
	return BrowserSignIn{FlowID: state, ServerID: serverID, AuthorizationURL: au.String(), ExpiresAt: iso(expires)}, nil
}

// FinishSignIn exchanges the code the redirect came back with for tokens and returns the
// server id. It returns a *mcpserver.Error ("not_found", "auth_failed").
func (a *OAuth) FinishSignIn(ctx context.Context, cb Callback) (string, error) {
	a.mu.Lock()
	a.sweepLocked()
	f := a.flows[cb.State]
	if cb.State == "" || f == nil || f.info.Kind != "browser" || f.done {
		a.mu.Unlock()
		return "", mcpserver.NewError("this sign-in is unknown or has expired; start it again", mcpserver.CodeNotFound)
	}
	f.done = true
	a.mu.Unlock()
	if cb.Error != "" {
		msg := "the authorization server refused the sign-in: " + cb.Error
		if cb.ErrorDescription != "" {
			msg += " (" + cb.ErrorDescription + ")"
		}
		a.failFlow(f, msg)
		return "", authFailed(msg)
	}
	if cb.Code == "" {
		a.failFlow(f, "the sign-in came back without a code")
		return "", authFailed("the sign-in came back without a code")
	}
	answer, err := a.postToken(ctx, f.discovery.Resource, f.discovery.TokenEndpoint, f.client, url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {cb.Code},
		"redirect_uri":  {f.redirectURI},
		"code_verifier": {f.verifier},
	})
	if err == nil && !answer.ok {
		err = authFailed("the token exchange failed: " + answer.describe())
	}
	var tokens Tokens
	if err == nil {
		tokens, err = toTokens(answer.json, "")
	}
	if err == nil {
		err = a.saveTokens(ctx, f, tokens)
	}
	if err != nil {
		a.failFlow(f, err.Error())
		var me *mcpserver.Error
		if errors.As(err, &me) {
			return "", me
		}
		return "", authFailed(err.Error())
	}
	a.mu.Lock()
	delete(a.flows, f.info.FlowID)
	a.mu.Unlock()
	a.announce(f.serverID, true)
	return f.serverID, nil
}

// StartDeviceSignIn starts a device sign-in (RFC 8628): show UserCode and
// VerificationURI to the user; this polls the token endpoint until they approve (Status
// shows the flow meanwhile). redirectURI ("" = none) is only used when a client has to be
// registered (some servers want one). The poll runs until the flow ends, CancelSignIn or
// Forget - it does not end with ctx.
func (a *OAuth) StartDeviceSignIn(ctx context.Context, serverID, redirectURI string) (DeviceSignIn, error) {
	server, err := a.requireHTTP(ctx, serverID)
	if err != nil {
		return DeviceSignIn{}, err
	}
	redirect := ""
	if redirectURI != "" {
		if redirect, err = checkRedirect(redirectURI); err != nil {
			return DeviceSignIn{}, err
		}
	}
	d, err := Discover(ctx, a.o.Client, server.Transport.URL, server.Transport.OAuth)
	if err != nil {
		return DeviceSignIn{}, err
	}
	if d.DeviceAuthorizationEndpoint == "" {
		return DeviceSignIn{}, mcpserver.NewError(`the authorization server of "`+serverID+`" doesn't support device sign-in; use the browser sign-in`, mcpserver.CodeInvalidArgument)
	}
	client, err := a.ensureClient(ctx, server, d, redirect, []string{deviceGrant, "refresh_token"})
	if err != nil {
		return DeviceSignIn{}, err
	}
	form := url.Values{"resource": {d.Resource}}
	if d.Scope != "" {
		form.Set("scope", d.Scope)
	}
	status, doc, err := a.postForm(ctx, d.DeviceAuthorizationEndpoint, client, form)
	if err != nil {
		return DeviceSignIn{}, authFailed("the device sign-in could not start: " + err.Error())
	}
	if status < 200 || status > 299 {
		return DeviceSignIn{}, authFailed("the device sign-in could not start: " + errorOf(status, doc).describe())
	}
	deviceCode, userCode := toString(doc["device_code"]), toString(doc["user_code"])
	verification := toString(doc["verification_uri"])
	if verification == "" {
		verification = toString(doc["verification_url"])
	}
	if deviceCode == "" || userCode == "" || verification == "" {
		return DeviceSignIn{}, authFailed("the device authorization answer has no device_code, user_code or verification_uri")
	}
	complete := toString(doc["verification_uri_complete"])
	expiresIn := defaultFlowTTL
	if v, ok := number(doc["expires_in"]); ok && v > 0 {
		expiresIn = time.Duration(v * float64(time.Second))
	}
	intervalSec := 5.0
	if v, ok := number(doc["interval"]); ok && v > 0 {
		intervalSec = v
	}
	now := time.Now()
	expires := now.Add(expiresIn)
	flowID := randomString(16)
	// The poll outlives this request: it ends with the flow, CancelSignIn or Forget.
	pctx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	f := &flow{
		info: FlowInfo{
			FlowID: flowID, Kind: "device", Status: "pending", StartedAt: iso(now), ExpiresAt: iso(expires),
			UserCode: userCode, VerificationURI: verification, VerificationURIComplete: complete,
		},
		serverID:     serverID,
		url:          server.Transport.URL,
		discovery:    d,
		client:       client,
		deviceCode:   deviceCode,
		interval:     time.Duration(intervalSec * float64(time.Second)),
		expires:      expires,
		cancel:       cancel,
		pollFinished: make(chan struct{}),
	}
	a.CancelSignIn(serverID)
	a.mu.Lock()
	a.flows[flowID] = f
	a.mu.Unlock()
	go a.poll(pctx, f)
	return DeviceSignIn{
		FlowID: flowID, ServerID: serverID, UserCode: userCode, VerificationURI: verification,
		VerificationURIComplete: complete, ExpiresAt: iso(expires), IntervalSec: intervalSec,
	}, nil
}

// CancelSignIn stops the server's running sign-in (if any) and waits for its poll.
func (a *OAuth) CancelSignIn(serverID string) {
	a.mu.Lock()
	var waits []chan struct{}
	for id, f := range a.flows {
		if f.serverID != serverID {
			continue
		}
		f.done = true
		if f.cancel != nil {
			f.cancel()
			waits = append(waits, f.pollFinished)
		}
		delete(a.flows, id)
	}
	a.mu.Unlock()
	for _, w := range waits {
		<-w
	}
}

// SignOut forgets the server's tokens (the client registration is kept for the next
// sign-in).
func (a *OAuth) SignOut(ctx context.Context, serverID string) error {
	a.CancelSignIn(serverID)
	data, err := a.load(ctx, serverID)
	if err != nil || data == nil {
		return err
	}
	wasSignedIn := data.Tokens != nil
	if err := a.write(ctx, serverID, func(d *Data) *Data {
		if d == nil {
			return nil
		}
		d.Tokens, d.Error = nil, ""
		return d
	}); err != nil {
		return err
	}
	if wasSignedIn {
		a.announce(serverID, false)
	}
	return nil
}

// Forget drops everything about the server (it was deleted).
func (a *OAuth) Forget(ctx context.Context, serverID string) error {
	a.CancelSignIn(serverID)
	lock := a.writeLock(serverID)
	lock.Lock()
	defer lock.Unlock()
	a.mu.Lock()
	delete(a.cache, serverID)
	a.mu.Unlock()
	store, err := a.store()
	if err != nil {
		return err
	}
	return store.Delete(ctx, serverID)
}

// Reload drops the cache (the store was swapped).
func (a *OAuth) Reload() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.generation++
	a.cache = map[string]cacheEntry{}
}

// AuthFor is how an HTTP connection to the server gets its token (MCP manager → HTTP
// transport).
func (a *OAuth) AuthFor(server mcpserver.Server) mcpclient.HTTPAuth {
	return serverAuth{a: a, id: server.ID, url: server.Transport.URL}
}

type serverAuth struct {
	a       *OAuth
	id, url string
}

func (s serverAuth) Token(ctx context.Context) (string, error) {
	return s.a.AccessToken(ctx, s.id, s.url)
}

func (s serverAuth) Refresh(ctx context.Context, rejected string) bool {
	return s.a.Refresh(ctx, s.id, s.url, rejected)
}

// AccessToken returns the access token for requests to rawURL ("" = not signed in),
// refreshed first when it is about to expire.
func (a *OAuth) AccessToken(ctx context.Context, serverID, rawURL string) (string, error) {
	data, err := a.load(ctx, serverID)
	if err != nil || data == nil || data.Tokens == nil || data.URL != rawURL {
		return "", err
	}
	t := data.Tokens
	if t.ExpiresAt != "" && t.RefreshToken != "" {
		if exp, perr := time.Parse(time.RFC3339Nano, t.ExpiresAt); perr == nil && time.Until(exp) < refreshEarly {
			a.Refresh(ctx, serverID, rawURL, t.AccessToken)
			if data, err = a.load(ctx, serverID); err != nil || data == nil || data.Tokens == nil || data.URL != rawURL {
				return "", err
			}
		}
	}
	return data.Tokens.AccessToken, nil
}

// Refresh gets a new access token after rejected was refused (or ran out); it reports
// whether there is a new one to try. One refresh per server at a time; a refresh token the
// server refuses drops the sign-in (announced).
func (a *OAuth) Refresh(ctx context.Context, serverID, rawURL, rejected string) bool {
	a.mu.Lock()
	if call := a.refreshing[serverID]; call != nil {
		a.mu.Unlock()
		select {
		case <-call.done:
			return call.ok
		case <-ctx.Done():
			return false
		}
	}
	call := &refreshCall{done: make(chan struct{})}
	a.refreshing[serverID] = call
	a.mu.Unlock()
	call.ok = a.doRefresh(ctx, serverID, rawURL, rejected)
	a.mu.Lock()
	delete(a.refreshing, serverID)
	a.mu.Unlock()
	close(call.done)
	return call.ok
}

func (a *OAuth) doRefresh(ctx context.Context, serverID, rawURL, rejected string) bool {
	data, err := a.load(ctx, serverID)
	if err != nil || data == nil || data.Tokens == nil || data.URL != rawURL {
		return false
	}
	// Another connection refreshed it already.
	if rejected != "" && data.Tokens.AccessToken != rejected {
		return true
	}
	if data.Tokens.RefreshToken == "" || data.Client == nil {
		a.dropTokens(ctx, serverID, "the server refused the access token and it can't be refreshed; sign in again")
		return false
	}
	answer, err := a.postToken(ctx, data.Resource, data.TokenEndpoint, *data.Client, url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {data.Tokens.RefreshToken},
	})
	if err != nil {
		// The authorization server is unreachable: keep the sign-in, try again later.
		return false
	}
	if !answer.ok {
		if answer.status >= 400 && answer.status < 500 {
			a.dropTokens(ctx, serverID, "refreshing the sign-in failed: "+answer.describe()+"; sign in again")
		}
		return false
	}
	tokens, err := toTokens(answer.json, data.Tokens.RefreshToken)
	if err != nil {
		return false
	}
	return a.write(ctx, serverID, func(d *Data) *Data {
		if d == nil {
			return nil
		}
		d.Tokens, d.Error = &tokens, ""
		return d
	}) == nil
}

// ── Internals ──

func (a *OAuth) store() (persistence.McpAuthStore, error) {
	if a.o.GetStore == nil {
		return nil, errNoStore
	}
	s := a.o.GetStore()
	if nilcheck.IsNil(s) {
		return nil, errNoStore
	}
	return s, nil
}

func (a *OAuth) requireHTTP(ctx context.Context, serverID string) (mcpserver.Server, error) {
	if a.o.GetServer == nil {
		return mcpserver.Server{}, mcpserver.NewError(`No MCP server with id "`+serverID+`"`, mcpserver.CodeNotFound)
	}
	server, ok, err := a.o.GetServer(ctx, serverID)
	if err != nil {
		return mcpserver.Server{}, err
	}
	if !ok {
		return mcpserver.Server{}, mcpserver.NewError(`No MCP server with id "`+serverID+`"`, mcpserver.CodeNotFound)
	}
	if server.Transport.Type != mcpserver.TransportHTTP {
		return mcpserver.Server{}, mcpserver.NewError(`"`+serverID+`" is a stdio MCP server; only HTTP servers sign in with OAuth`, mcpserver.CodeInvalidArgument)
	}
	if server.Transport.HasOwnAuthorization() {
		return mcpserver.Server{}, mcpserver.NewError(`"`+serverID+`" sends its own Authorization header; remove it to sign in with OAuth`, mcpserver.CodeInvalidArgument)
	}
	return server, nil
}

// ensureClient returns the client to sign in with: the definition's, the registered one,
// or a new registration.
func (a *OAuth) ensureClient(ctx context.Context, server mcpserver.Server, d Discovery, redirectURI string, grants []string) (Client, error) {
	if s := server.Transport.OAuth; s != nil && s.ClientID != "" {
		return Client{ClientID: s.ClientID, AuthMethod: "none", RedirectURIs: []string{}, GrantTypes: slices.Clone(grants)}, nil
	}
	data, err := a.load(ctx, server.ID)
	if err != nil {
		return Client{}, err
	}
	var known *Client
	if data != nil && data.Client != nil && data.Client.Registered && data.Issuer == d.Issuer {
		known = data.Client
	}
	if known != nil && (redirectURI == "" || slices.Contains(known.RedirectURIs, redirectURI)) && containsAll(known.GrantTypes, grants) {
		return *known, nil
	}
	if d.RegistrationEndpoint == "" {
		return Client{}, authFailed(`the authorization server of "` + server.ID + `" doesn't support dynamic client registration; register a client there and set oauth.clientId on the server`)
	}
	redirects := []string{}
	if known != nil {
		redirects = append(redirects, known.RedirectURIs...)
	}
	if redirectURI != "" && !slices.Contains(redirects, redirectURI) {
		redirects = append(redirects, redirectURI)
	}
	var wanted []string
	if known != nil {
		wanted = append(wanted, known.GrantTypes...)
	}
	wanted = append(wanted, grants...)
	if len(redirects) > 0 {
		wanted = append(wanted, "authorization_code")
	}
	wanted = append(wanted, "refresh_token")
	grantTypes := []string{}
	for _, g := range wanted {
		if slices.Contains(grantTypes, g) {
			continue
		}
		if slices.Contains(grants, g) || d.GrantTypes == nil || slices.Contains(d.GrantTypes, g) {
			grantTypes = append(grantTypes, g)
		}
	}
	method := "client_secret_post"
	switch {
	case d.AuthMethods == nil || slices.Contains(d.AuthMethods, "none"):
		method = "none"
	case slices.Contains(d.AuthMethods, "client_secret_basic"):
		method = "client_secret_basic"
	}
	reg := map[string]any{
		"client_name":                a.o.ClientName,
		"redirect_uris":              redirects,
		"grant_types":                grantTypes,
		"response_types":             []string{},
		"token_endpoint_auth_method": method,
	}
	if slices.Contains(grantTypes, "authorization_code") {
		reg["response_types"] = []string{"code"}
	}
	if d.Scope != "" {
		reg["scope"] = d.Scope
	}
	body, err := jsonx.Marshal(reg)
	if err != nil {
		return Client{}, err
	}
	rctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(rctx, http.MethodPost, d.RegistrationEndpoint, bytes.NewReader(body))
	if err != nil {
		return Client{}, authFailed("registering with the authorization server failed: " + err.Error())
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := a.o.Client.Do(req)
	if err != nil {
		return Client{}, authFailed("registering with the authorization server failed: " + err.Error())
	}
	defer func() { _ = resp.Body.Close() }()
	doc := decodeObject(resp.Body)
	clientID := toString(doc["client_id"])
	if resp.StatusCode < 200 || resp.StatusCode > 299 || clientID == "" {
		return Client{}, authFailed("registering with the authorization server failed: " + errorOf(resp.StatusCode, doc).describe())
	}
	secret := toString(doc["client_secret"])
	answered := toString(doc["token_endpoint_auth_method"])
	switch {
	case answered == "none" || answered == "client_secret_basic" || answered == "client_secret_post":
		method = answered
	case secret != "":
		method = "client_secret_basic"
	}
	client := Client{ClientID: clientID, ClientSecret: secret, AuthMethod: method, RedirectURIs: redirects, GrantTypes: grantTypes, Registered: true}
	if err := a.write(ctx, server.ID, func(cur *Data) *Data {
		next := merge(cur, server.Transport.URL, d)
		c := client
		next.Client = &c
		return next
	}); err != nil {
		return Client{}, err
	}
	return client, nil
}

func (a *OAuth) saveTokens(ctx context.Context, f *flow, tokens Tokens) error {
	return a.write(ctx, f.serverID, func(cur *Data) *Data {
		next := merge(cur, f.url, f.discovery)
		if f.client.Registered {
			c := f.client
			next.Client = &c
		}
		next.Tokens = &tokens
		next.Error = ""
		return next
	})
}

func (a *OAuth) dropTokens(ctx context.Context, serverID, msg string) {
	err := a.write(ctx, serverID, func(d *Data) *Data {
		if d == nil {
			return nil
		}
		d.Tokens, d.Error = nil, msg
		return d
	})
	if err != nil {
		a.o.Logger.Warn("[mcp] could not drop a refused sign-in", "server", serverID, "error", err)
	}
	a.announce(serverID, false)
}

type tokenAnswer struct {
	ok          bool
	json        map[string]any
	status      int
	err         string
	description string
}

func (t tokenAnswer) describe() string {
	if t.description != "" {
		return t.err + " (" + t.description + ")"
	}
	return t.err
}

func errorOf(status int, doc map[string]any) tokenAnswer {
	e := toString(doc["error"])
	if e == "" {
		e = "HTTP " + strconv.Itoa(status)
	}
	return tokenAnswer{status: status, err: e, description: toString(doc["error_description"])}
}

func (a *OAuth) postToken(ctx context.Context, resource, endpoint string, client Client, form url.Values) (tokenAnswer, error) {
	form.Set("resource", resource)
	status, doc, err := a.postForm(ctx, endpoint, client, form)
	if err != nil {
		return tokenAnswer{}, err
	}
	if status >= 200 && status <= 299 && toString(doc["access_token"]) != "" {
		return tokenAnswer{ok: true, json: doc, status: status}, nil
	}
	if status >= 200 && status <= 299 {
		status = http.StatusBadRequest
	}
	return errorOf(status, doc), nil
}

// postForm POSTs a form with the client's proof (client_id in the body, Basic, or the
// secret in the body) and decodes the JSON answer.
func (a *OAuth) postForm(ctx context.Context, endpoint string, client Client, form url.Values) (int, map[string]any, error) {
	basic := client.AuthMethod == "client_secret_basic" && client.ClientSecret != ""
	if !basic {
		form.Set("client_id", client.ClientID)
		if client.AuthMethod == "client_secret_post" && client.ClientSecret != "" {
			form.Set("client_secret", client.ClientSecret)
		}
	}
	rctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(rctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	if basic {
		req.SetBasicAuth(url.QueryEscape(client.ClientID), url.QueryEscape(client.ClientSecret))
	}
	resp, err := a.o.Client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	return resp.StatusCode, decodeObject(resp.Body), nil
}

func (a *OAuth) poll(ctx context.Context, f *flow) {
	defer close(f.pollFinished)
	for {
		a.mu.Lock()
		interval := f.interval
		a.mu.Unlock()
		select {
		case <-ctx.Done():
			return
		case <-time.After(interval):
		}
		a.mu.Lock()
		stop := f.done || a.flows[f.info.FlowID] != f
		a.mu.Unlock()
		if stop {
			return
		}
		if !time.Now().Before(f.expires) {
			a.failFlow(f, "the device code expired before it was approved")
			return
		}
		answer, err := a.postToken(ctx, f.discovery.Resource, f.discovery.TokenEndpoint, f.client, url.Values{
			"grant_type":  {deviceGrant},
			"device_code": {f.deviceCode},
		})
		if err != nil {
			continue
		}
		if !answer.ok {
			switch answer.err {
			case "authorization_pending":
				continue
			case "slow_down":
				a.mu.Lock()
				f.interval += 5 * time.Second
				a.mu.Unlock()
				continue
			case "access_denied":
				a.failFlow(f, "the sign-in was refused")
			default:
				a.failFlow(f, "the device sign-in failed: "+answer.describe())
			}
			return
		}
		a.mu.Lock()
		if f.done {
			a.mu.Unlock()
			return
		}
		f.done = true
		a.mu.Unlock()
		tokens, err := toTokens(answer.json, "")
		if err == nil {
			err = a.saveTokens(ctx, f, tokens)
		}
		if err != nil {
			a.failFlow(f, "could not store the sign-in: "+err.Error())
			return
		}
		a.mu.Lock()
		if a.flows[f.info.FlowID] == f {
			delete(a.flows, f.info.FlowID)
		}
		a.mu.Unlock()
		a.announce(f.serverID, true)
		return
	}
}

// failFlow marks a flow failed; Status shows it until it would have expired.
func (a *OAuth) failFlow(f *flow, msg string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	f.done = true
	f.info.Status = "failed"
	f.info.Error = msg
}

func (a *OAuth) flowOf(serverID string) *flow {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.sweepLocked()
	var found *flow
	for _, f := range a.flows {
		if f.serverID == serverID && (found == nil || f.info.StartedAt > found.info.StartedAt) {
			found = f
		}
	}
	if found == nil {
		return nil
	}
	c := *found
	return &c
}

func (a *OAuth) sweepLocked() {
	now := time.Now()
	for id, f := range a.flows {
		if now.Before(f.expires) {
			continue
		}
		if f.cancel != nil {
			f.cancel()
		}
		delete(a.flows, id)
	}
}

func (a *OAuth) announce(serverID string, signedIn bool) {
	if a.o.Events != nil {
		events.Emit(a.o.Events, events.McpAuthChanged, "", events.McpAuthChangedPayload{ServerID: serverID, SignedIn: signedIn})
	}
}

func (a *OAuth) writeLock(serverID string) *sync.Mutex {
	a.mu.Lock()
	defer a.mu.Unlock()
	l := a.writeLocks[serverID]
	if l == nil {
		l = &sync.Mutex{}
		a.writeLocks[serverID] = l
	}
	return l
}

// load returns a copy of the server's sign-in (nil = none).
func (a *OAuth) load(ctx context.Context, serverID string) (*Data, error) {
	a.mu.Lock()
	if e, ok := a.cache[serverID]; ok {
		a.mu.Unlock()
		return e.data.Clone(), nil
	}
	generation := a.generation
	a.mu.Unlock()
	store, err := a.store()
	if err != nil {
		return nil, err
	}
	rec, ok, err := store.Get(ctx, serverID)
	if err != nil {
		return nil, err
	}
	var data *Data
	if ok {
		plain := rec.Data
		if a.o.Sealer != nil {
			plain, err = a.o.Sealer.Open(ctx, rec.Data, aad(serverID))
		}
		if err == nil {
			var d Data
			if err = json.Unmarshal([]byte(plain), &d); err == nil {
				data = &d
			}
		}
		if err != nil {
			a.o.Logger.Warn(`[mcp] the stored sign-in of "`+serverID+`" can't be read and is ignored`, "error", err)
			data = nil
		}
	}
	a.mu.Lock()
	if a.generation == generation {
		if e, hit := a.cache[serverID]; hit {
			data = e.data
		} else {
			a.cache[serverID] = cacheEntry{data: data}
		}
	}
	a.mu.Unlock()
	return data.Clone(), nil
}

// write reads, changes and stores one server's record, one at a time; fn returning nil
// writes nothing. fn gets a copy it may change.
func (a *OAuth) write(ctx context.Context, serverID string, fn func(*Data) *Data) error {
	lock := a.writeLock(serverID)
	lock.Lock()
	defer lock.Unlock()
	cur, err := a.load(ctx, serverID)
	if err != nil {
		return err
	}
	next := fn(cur)
	if next == nil {
		return nil
	}
	next.UpdatedAt = iso(time.Now())
	plain, err := jsonx.Marshal(next)
	if err != nil {
		return err
	}
	sealed := string(plain)
	if a.o.Sealer != nil {
		if sealed, err = a.o.Sealer.Seal(ctx, sealed, aad(serverID)); err != nil {
			return err
		}
	}
	store, err := a.store()
	if err != nil {
		return err
	}
	if err := store.Save(ctx, persistence.McpAuthRecord{ServerID: serverID, Data: sealed, UpdatedAt: next.UpdatedAt}); err != nil {
		return err
	}
	a.mu.Lock()
	a.cache[serverID] = cacheEntry{data: next.Clone()}
	a.mu.Unlock()
	return nil
}

// merge is the record with the discovery's endpoints; tokens survive only for the same
// url and issuer, the client for the same issuer.
func merge(d *Data, rawURL string, disc Discovery) *Data {
	out := &Data{
		URL:                         rawURL,
		Resource:                    disc.Resource,
		Issuer:                      disc.Issuer,
		AuthorizationEndpoint:       disc.AuthorizationEndpoint,
		TokenEndpoint:               disc.TokenEndpoint,
		DeviceAuthorizationEndpoint: disc.DeviceAuthorizationEndpoint,
		RegistrationEndpoint:        disc.RegistrationEndpoint,
		RevocationEndpoint:          disc.RevocationEndpoint,
		Scope:                       disc.Scope,
	}
	if d != nil {
		sameIssuer := d.Issuer == disc.Issuer
		if sameIssuer && d.Client != nil {
			out.Client = d.Client
		}
		if sameIssuer && d.URL == rawURL && d.Tokens != nil {
			out.Tokens = d.Tokens
		}
		out.Error = d.Error
	}
	return out
}

func toTokens(doc map[string]any, previousRefresh string) (Tokens, error) {
	access := toString(doc["access_token"])
	if access == "" {
		return Tokens{}, authFailed("the token answer has no access_token")
	}
	t := Tokens{AccessToken: access, TokenType: toString(doc["token_type"]), RefreshToken: toString(doc["refresh_token"]), Scope: toString(doc["scope"])}
	if t.TokenType == "" {
		t.TokenType = "Bearer"
	}
	if t.RefreshToken == "" {
		t.RefreshToken = previousRefresh
	}
	if v, ok := number(doc["expires_in"]); ok && v > 0 {
		t.ExpiresAt = iso(time.Now().Add(time.Duration(v * float64(time.Second))))
	}
	return t, nil
}

func checkRedirect(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return "", mcpserver.NewError(`redirectUri "`+raw+`" is not a valid URL`, mcpserver.CodeInvalidArgument)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", mcpserver.NewError("redirectUri must be http(s)", mcpserver.CodeInvalidArgument)
	}
	if u.Fragment != "" {
		return "", mcpserver.NewError("redirectUri must not have a fragment", mcpserver.CodeInvalidArgument)
	}
	return u.String(), nil
}

func decodeObject(r io.Reader) map[string]any {
	var doc map[string]any
	if json.NewDecoder(io.LimitReader(r, maxDoc)).Decode(&doc) != nil || doc == nil {
		return map[string]any{}
	}
	return doc
}

func number(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case string:
		f, err := strconv.ParseFloat(n, 64)
		return f, err == nil
	}
	return 0, false
}

func containsAll(have, want []string) bool {
	for _, w := range want {
		if !slices.Contains(have, w) {
			return false
		}
	}
	return true
}

func aad(serverID string) string { return "mcp-auth:" + serverID }

func iso(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05.000Z") }

func randomString(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

func challengeOf(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}
