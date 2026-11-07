package mcpoauth_test

import (
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Phong95/aec-agent-go/components/events"
	"github.com/Phong95/aec-agent-go/components/mcphub"
	"github.com/Phong95/aec-agent-go/components/mcphub/mcpoauth"
	"github.com/Phong95/aec-agent-go/components/mcphub/mcpserver"
	"github.com/Phong95/aec-agent-go/components/persistence"
	"github.com/Phong95/aec-agent-go/internal/mcptest"
	"github.com/Phong95/aec-agent-go/kernel"
)

// ---------------------------------------------------------------------------
// A-214: OAuth sign-in of MCP servers against the fake authorization server of
// internal/mcptest - the Go form of core/ts tests/unit/mcp-oauth.test.ts (the hub side;
// the MCP manager side is in components/agent).
// ---------------------------------------------------------------------------

const redirect = "http://127.0.0.1:1/kernel/mcp-servers/auth/callback"

// fakeSealer marks what it sealed and checks the aad on the way back.
type fakeSealer struct{}

func (fakeSealer) Seal(_ context.Context, plain, aad string) (string, error) {
	return "sealed:" + aad + ":" + base64.StdEncoding.EncodeToString([]byte(plain)), nil
}

func (fakeSealer) Open(_ context.Context, stored, aad string) (string, error) {
	prefix := "sealed:" + aad + ":"
	if !strings.HasPrefix(stored, prefix) {
		return "", errors.New("wrong aad")
	}
	b, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(stored, prefix))
	return string(b), err
}

func startAS(t *testing.T, opts mcptest.OAuthOptions) *mcptest.OAuthServer {
	t.Helper()
	as := mcptest.StartOAuth(opts)
	t.Cleanup(as.Close)
	return as
}

type rig struct {
	hub  *mcphub.McpHub
	auth *persistence.MemoryMcpAuthStore
	bus  *events.Dispatcher
}

func newRig(t *testing.T, sealed bool) *rig {
	t.Helper()
	servers := persistence.NewMemoryMcpServerStore()
	r := &rig{auth: persistence.NewMemoryMcpAuthStore(), bus: events.New()}
	opts := []mcphub.Option{mcphub.WithEvents(r.bus), mcphub.WithAuthStore(func() persistence.McpAuthStore { return r.auth })}
	if sealed {
		opts = append(opts, mcphub.WithSealer(fakeSealer{}))
	}
	r.hub = mcphub.NewMcpHub(func() persistence.McpServerStore { return servers }, opts...)
	return r
}

func httpDef(id, u string) mcpserver.Definition {
	return mcpserver.Definition{ID: id, Transport: mcpserver.Transport{Type: mcpserver.TransportHTTP, URL: u}}
}

// browserSignIn runs a browser sign-in as the user would: follow the authorization URL,
// hand the redirect to FinishSignIn.
func browserSignIn(t *testing.T, hub *mcphub.McpHub, serverID string) mcpoauth.BrowserSignIn {
	t.Helper()
	started, err := hub.OAuth().StartSignIn(t.Context(), serverID, redirect)
	if err != nil {
		t.Fatalf("StartSignIn: %v", err)
	}
	noFollow := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, started.AuthorizationURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := noFollow.Do(req)
	if err != nil {
		t.Fatalf("authorize: %v", err)
	}
	_ = resp.Body.Close()
	back, err := url.Parse(resp.Header.Get("Location"))
	if err != nil || back.Scheme+"://"+back.Host+back.Path != redirect {
		t.Fatalf("redirect = %q", resp.Header.Get("Location"))
	}
	id, err := hub.OAuth().FinishSignIn(t.Context(), mcpoauth.Callback{State: back.Query().Get("state"), Code: back.Query().Get("code")})
	if err != nil || id != serverID {
		t.Fatalf("FinishSignIn = %q, %v", id, err)
	}
	return started
}

func authEvents(bus events.Bus) (func() []events.McpAuthChangedPayload, events.Unsubscribe) {
	var mu sync.Mutex
	var got []events.McpAuthChangedPayload
	off := events.Subscribe(bus, events.McpAuthChanged, func(_ kernel.Event, p events.McpAuthChangedPayload) {
		mu.Lock()
		got = append(got, p)
		mu.Unlock()
	})
	return func() []events.McpAuthChangedPayload {
		mu.Lock()
		defer mu.Unlock()
		return append([]events.McpAuthChangedPayload(nil), got...)
	}, off
}

func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestParseChallengeAndCanonicalResource(t *testing.T) {
	if got := mcpoauth.CanonicalResource("https://MCP.example.com/mcp#x"); got != "https://mcp.example.com/mcp" {
		t.Errorf("CanonicalResource = %q", got)
	}
	if got := mcpoauth.CanonicalResource("https://mcp.example.com/"); got != "https://mcp.example.com" {
		t.Errorf("CanonicalResource root = %q", got)
	}
}

func TestDiscoverFromResourceMetadata(t *testing.T) {
	as := startAS(t, mcptest.OAuthOptions{})
	d, err := mcpoauth.Discover(t.Context(), nil, as.URL, nil)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	want := mcpoauth.Discovery{
		Resource: as.URL, Issuer: as.Base + "/as",
		AuthorizationEndpoint: as.Base + "/as/authorize", TokenEndpoint: as.Base + "/as/token",
		RegistrationEndpoint: as.Base + "/as/register", DeviceAuthorizationEndpoint: as.Base + "/as/device",
		// The 401's scope, plus offline_access because the server supports it.
		Scope:       "mcp.tools offline_access",
		GrantTypes:  []string{"authorization_code", "refresh_token", "urn:ietf:params:oauth:grant-type:device_code"},
		AuthMethods: []string{"none"},
	}
	if !reflect.DeepEqual(d, want) {
		t.Fatalf("Discover =\n%+v\nwant\n%+v", d, want)
	}
}

func TestDiscoverFallsBackWithoutMetadata(t *testing.T) {
	as := startAS(t, mcptest.OAuthOptions{NoMetadata: true})
	d, err := mcpoauth.Discover(t.Context(), nil, as.URL, &mcpserver.OAuthSettings{Scopes: "x  y"})
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if d.Issuer != as.Base || d.AuthorizationEndpoint != as.Base+"/authorize" || d.TokenEndpoint != as.Base+"/token" || d.Scope != "x  y" {
		t.Fatalf("Discover = %+v", d)
	}
}

func TestDiscoverRefusesPlainHTTP(t *testing.T) {
	_, err := mcpoauth.Discover(t.Context(), nil, "https://mcp.invalid/mcp", &mcpserver.OAuthSettings{AuthServerMetadataURL: "http://example.com/meta"})
	if !errors.Is(err, &mcpserver.Error{Code: mcpserver.CodeAuthFailed}) {
		t.Fatalf("err = %v, want auth_failed", err)
	}
}

func TestHubChecksOAuthSettings(t *testing.T) {
	hub := mcphub.NewMcpHub(func() persistence.McpServerStore { return persistence.NewMemoryMcpServerStore() })
	s, err := hub.Register(mcpserver.Definition{ID: "a", Transport: mcpserver.Transport{Type: mcpserver.TransportHTTP, URL: "https://x.example/mcp", OAuth: &mcpserver.OAuthSettings{ClientID: " my-client ", Scopes: "a   b"}}})
	if err != nil || s.Transport.OAuth == nil || *s.Transport.OAuth != (mcpserver.OAuthSettings{ClientID: "my-client", Scopes: "a b"}) {
		t.Fatalf("Register = %+v, %v", s.Transport.OAuth, err)
	}
	s, _ = hub.Register(mcpserver.Definition{ID: "b", Transport: mcpserver.Transport{Type: mcpserver.TransportHTTP, URL: "https://x.example/mcp", OAuth: &mcpserver.OAuthSettings{}}})
	if s.Transport.OAuth != nil {
		t.Fatalf("an empty oauth is kept: %+v", s.Transport.OAuth)
	}
	_, err = hub.Register(mcpserver.Definition{ID: "c", Transport: mcpserver.Transport{Type: mcpserver.TransportHTTP, URL: "https://x.example/mcp", OAuth: &mcpserver.OAuthSettings{AuthServerMetadataURL: "http://x.example/m"}}})
	if !errors.Is(err, &mcpserver.Error{Code: mcpserver.CodeInvalidArgument}) {
		t.Fatalf("plain http metadata url: err = %v", err)
	}
}

func TestOnlyHTTPServersWithoutOwnAuthorizationSignIn(t *testing.T) {
	r := newRig(t, false)
	_, _ = r.hub.Register(mcpserver.Definition{ID: "local", Transport: mcpserver.Transport{Type: mcpserver.TransportStdio, Command: "node"}})
	_, _ = r.hub.Register(mcpserver.Definition{ID: "keyed", Transport: mcpserver.Transport{Type: mcpserver.TransportHTTP, URL: "https://x.example/mcp", Headers: map[string]string{"Authorization": "Bearer {{secret:K}}"}}})
	for id, code := range map[string]mcpserver.ErrorCode{"local": mcpserver.CodeInvalidArgument, "keyed": mcpserver.CodeInvalidArgument, "nope": mcpserver.CodeNotFound} {
		if _, err := r.hub.OAuth().Status(t.Context(), id); !errors.Is(err, &mcpserver.Error{Code: code}) {
			t.Errorf("Status(%s) err = %v, want %s", id, err, code)
		}
	}
}

func TestBrowserSignInIsSealedAndReusesTheClient(t *testing.T) {
	as := startAS(t, mcptest.OAuthOptions{})
	r := newRig(t, true)
	if _, err := r.hub.Register(httpDef("remote", as.URL)); err != nil {
		t.Fatal(err)
	}
	got, off := authEvents(r.bus)
	defer off()
	started := browserSignIn(t, r.hub, "remote")
	u, _ := url.Parse(started.AuthorizationURL)
	q := u.Query()
	for k, v := range map[string]string{"response_type": "code", "client_id": "client-1", "code_challenge_method": "S256", "resource": as.URL, "scope": "mcp.tools offline_access"} {
		if q.Get(k) != v {
			t.Errorf("authorization %s = %q, want %q", k, q.Get(k), v)
		}
	}
	if e := got(); len(e) != 1 || e[0] != (events.McpAuthChangedPayload{ServerID: "remote", SignedIn: true}) {
		t.Fatalf("auth events = %+v", e)
	}
	reg := as.Clients()["client-1"]
	if uris, _ := reg["redirect_uris"].([]any); len(uris) != 1 || uris[0] != redirect || reg["token_endpoint_auth_method"] != "none" {
		t.Fatalf("registration = %+v", reg)
	}
	st, err := r.hub.OAuth().Status(t.Context(), "remote")
	if err != nil || !st.SignedIn || st.Issuer != as.Base+"/as" || st.Scope != "mcp.tools offline_access" || st.Refreshable == nil || !*st.Refreshable || st.ClientID != "client-1" {
		t.Fatalf("Status = %+v, %v", st, err)
	}
	rec, ok, _ := r.auth.Get(t.Context(), "remote")
	if !ok || !strings.HasPrefix(rec.Data, "sealed:mcp-auth:remote:") || strings.Contains(rec.Data, "at-") {
		t.Fatalf("stored record = %+v", rec)
	}
	for _, res := range as.Resources() {
		if res != as.URL {
			t.Fatalf("token request resource = %q", res)
		}
	}
	// A second sign-in reuses the registered client.
	browserSignIn(t, r.hub, "remote")
	if n := len(as.Clients()); n != 1 {
		t.Fatalf("registrations = %d, want 1", n)
	}
}

func TestRefreshBeforeExpiry(t *testing.T) {
	as := startAS(t, mcptest.OAuthOptions{ExpiresIn: 30}) // under the 60 s margin
	r := newRig(t, false)
	_, _ = r.hub.Register(httpDef("remote", as.URL))
	browserSignIn(t, r.hub, "remote")
	as.ResetGrants()
	tok, err := r.hub.OAuth().AccessToken(t.Context(), "remote", as.URL)
	if err != nil || tok != "at-3" { // code-1 → at-2 at sign-in; the refresh issues at-3
		t.Fatalf("AccessToken = %q, %v", tok, err)
	}
	if g := as.Grants(); len(g) != 1 || g[0] != "refresh_token" {
		t.Fatalf("grants = %v", g)
	}
}

func TestRefusedRefreshDropsTheSignIn(t *testing.T) {
	as := startAS(t, mcptest.OAuthOptions{})
	r := newRig(t, false)
	_, _ = r.hub.Register(httpDef("remote", as.URL))
	browserSignIn(t, r.hub, "remote")
	got, off := authEvents(r.bus)
	defer off()
	as.SetRefuseRefresh(true)
	tok, _ := r.hub.OAuth().AccessToken(t.Context(), "remote", as.URL)
	if r.hub.OAuth().Refresh(t.Context(), "remote", as.URL, tok) {
		t.Fatal("a refused refresh reported a new token")
	}
	st, _ := r.hub.OAuth().Status(t.Context(), "remote")
	if st.SignedIn || !strings.Contains(st.Error, "invalid_grant") {
		t.Fatalf("Status = %+v", st)
	}
	if e := got(); len(e) != 1 || e[0].SignedIn {
		t.Fatalf("auth events = %+v", e)
	}
}

func TestDeviceSignIn(t *testing.T) {
	as := startAS(t, mcptest.OAuthOptions{})
	r := newRig(t, false)
	_, _ = r.hub.Register(httpDef("remote", as.URL))
	got, off := authEvents(r.bus)
	defer off()
	d, err := r.hub.OAuth().StartDeviceSignIn(t.Context(), "remote", "")
	if err != nil || d.UserCode != "ABCD-EFGH" || d.VerificationURI != as.Base+"/as/activate" {
		t.Fatalf("StartDeviceSignIn = %+v, %v", d, err)
	}
	st, _ := r.hub.OAuth().Status(t.Context(), "remote")
	if st.Pending == nil || st.Pending.Kind != "device" || st.Pending.Status != "pending" || st.Pending.UserCode != "ABCD-EFGH" {
		t.Fatalf("pending = %+v", st.Pending)
	}
	// Registered without a redirect: only the device grant.
	reg := as.Clients()["client-1"]
	if uris, _ := reg["redirect_uris"].([]any); len(uris) != 0 {
		t.Fatalf("registration redirect_uris = %v", reg["redirect_uris"])
	}
	if gt, _ := reg["grant_types"].([]any); len(gt) != 2 || gt[0] != "urn:ietf:params:oauth:grant-type:device_code" || gt[1] != "refresh_token" {
		t.Fatalf("registration grant_types = %v", reg["grant_types"])
	}
	waitFor(t, "polls while pending", func() bool { return len(as.Grants()) >= 2 })
	as.ApproveDevice()
	waitFor(t, "the device sign-in", func() bool { return len(got()) == 1 })
	st, _ = r.hub.OAuth().Status(t.Context(), "remote")
	if !st.SignedIn || st.Pending != nil {
		t.Fatalf("Status = %+v", st)
	}
}

func TestDeviceSignInStopsOnCancel(t *testing.T) {
	as := startAS(t, mcptest.OAuthOptions{})
	r := newRig(t, false)
	_, _ = r.hub.Register(httpDef("remote", as.URL))
	if _, err := r.hub.OAuth().StartDeviceSignIn(t.Context(), "remote", ""); err != nil {
		t.Fatal(err)
	}
	r.hub.OAuth().CancelSignIn("remote") // waits for the poll (goleak checks it is gone)
	if st, _ := r.hub.OAuth().Status(t.Context(), "remote"); st.Pending != nil {
		t.Fatalf("pending after cancel = %+v", st.Pending)
	}
}

func TestSignOutForgetAndURLChange(t *testing.T) {
	as := startAS(t, mcptest.OAuthOptions{})
	r := newRig(t, false)
	ctx := t.Context()
	if _, err := r.hub.Create(ctx, httpDef("remote", as.URL)); err != nil {
		t.Fatal(err)
	}
	browserSignIn(t, r.hub, "remote")
	got, off := authEvents(r.bus)
	defer off()
	if err := r.hub.OAuth().SignOut(ctx, "remote"); err != nil {
		t.Fatal(err)
	}
	if e := got(); len(e) != 1 || e[0] != (events.McpAuthChangedPayload{ServerID: "remote"}) {
		t.Fatalf("auth events = %+v", e)
	}
	if st, _ := r.hub.OAuth().Status(ctx, "remote"); st.SignedIn || st.ClientID != "client-1" {
		t.Fatalf("Status after sign out = %+v", st)
	}

	if _, err := r.hub.OAuth().FinishSignIn(ctx, mcpoauth.Callback{State: "nope", Code: "x"}); !errors.Is(err, &mcpserver.Error{Code: mcpserver.CodeNotFound}) {
		t.Fatalf("unknown state: err = %v", err)
	}
	started, err := r.hub.OAuth().StartSignIn(ctx, "remote", "http://127.0.0.1:1/cb")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.hub.OAuth().FinishSignIn(ctx, mcpoauth.Callback{State: started.FlowID, Error: "access_denied"}); !errors.Is(err, &mcpserver.Error{Code: mcpserver.CodeAuthFailed}) {
		t.Fatalf("refused: err = %v", err)
	}
	if st, _ := r.hub.OAuth().Status(ctx, "remote"); st.Pending == nil || st.Pending.Status != "failed" || !strings.Contains(st.Pending.Error, "access_denied") {
		t.Fatalf("pending = %+v", st.Pending)
	}

	// A changed url is a different resource: not signed in any more.
	browserSignIn(t, r.hub, "remote")
	if _, err := r.hub.Update(ctx, "remote", httpDef("remote", as.URL+"?v=2")); err != nil {
		t.Fatal(err)
	}
	if st, _ := r.hub.OAuth().Status(ctx, "remote"); st.SignedIn {
		t.Fatal("still signed in after the url changed")
	}
	if err := r.hub.Delete(ctx, "remote"); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := r.auth.Get(ctx, "remote"); ok {
		t.Fatal("the sign-in survived Delete")
	}
}
