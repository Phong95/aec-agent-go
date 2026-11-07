package agent_test

import (
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/Phong95/aec-agent-go/components/events"
	"github.com/Phong95/aec-agent-go/components/mcphub"
	"github.com/Phong95/aec-agent-go/components/mcphub/mcpoauth"
	"github.com/Phong95/aec-agent-go/components/mcphub/mcpserver"
	"github.com/Phong95/aec-agent-go/internal/mcptest"
)

// ---------------------------------------------------------------------------
// A-214: the MCP manager with OAuth - a server that answers 401 is needs_auth (not
// retried) until it is signed in, the bearer token goes on every request, a 401 refreshes
// it once, and a refused refresh drops the sign-in and goes back to needs_auth. The Go
// form of the McpManager part of core/ts tests/unit/mcp-oauth.test.ts.
// ---------------------------------------------------------------------------

func oauthRig(t *testing.T) (*mcptest.OAuthServer, *mcpRig) {
	t.Helper()
	as := mcptest.StartOAuth(mcptest.OAuthOptions{})
	t.Cleanup(as.Close)
	r := newMcpRig(t, mcpserver.Definition{ID: "remote", Transport: mcpserver.Transport{Type: mcpserver.TransportHTTP, URL: as.URL}})
	return as, r
}

func signIn(t *testing.T, hub *mcphub.McpHub) {
	t.Helper()
	started, err := hub.OAuth().StartSignIn(t.Context(), "remote", "http://127.0.0.1:1/kernel/mcp-servers/auth/callback")
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
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	back, _ := url.Parse(resp.Header.Get("Location"))
	if _, err := hub.OAuth().FinishSignIn(t.Context(), mcpoauth.Callback{State: back.Query().Get("state"), Code: back.Query().Get("code")}); err != nil {
		t.Fatalf("FinishSignIn: %v", err)
	}
}

func TestMcpNeedsAuthThenSignsIn(t *testing.T) {
	as, r := oauthRig(t)
	failed := waitKind(t, r.env.bus, events.McpFailed, nil)
	a := r.spawn(t, "a1", []string{"remote"})
	if err := a.StartMcp(t.Context()); err != nil {
		t.Fatal(err)
	}
	f := recv(t, failed, "mcp:error")
	if !f.NeedsAuth || f.WillRetry {
		t.Fatalf("mcp:error = %+v", f)
	}
	st, _ := a.Mcp().Get("remote")
	if st.Status != mcpserver.StatusNeedsAuth || st.Attempts != 1 || !strings.Contains(st.Error, "needs a sign-in (HTTP 401)") {
		t.Fatalf("state = %+v", st)
	}

	connected := waitKind(t, r.env.bus, events.McpConnected, nil)
	signIn(t, r.hub)
	recv(t, connected, "mcp:connected")
	res, err := a.Mcp().CallTool(t.Context(), "remote", "echo", map[string]any{"text": "signed in"}, mcpserver.CallOptions{})
	if err != nil || textOf(t, res) != "signed in" {
		t.Fatalf("CallTool = %+v, %v", res, err)
	}
	if !strings.HasPrefix(as.LastAuth(), "Bearer at-") {
		t.Fatalf("Authorization = %q", as.LastAuth())
	}
}

func TestMcpRefreshesAfterA401(t *testing.T) {
	as, r := oauthRig(t)
	signIn(t, r.hub)
	a := r.spawn(t, "a1", []string{"remote"})
	if err := a.StartMcp(t.Context()); err != nil {
		t.Fatal(err)
	}
	if st, _ := a.Mcp().Get("remote"); st.Status != mcpserver.StatusConnected {
		t.Fatalf("state = %+v", st)
	}
	// The server revokes the access token: the next call refreshes and goes through.
	as.RevokeAll()
	res, err := a.Mcp().CallTool(t.Context(), "remote", "echo", map[string]any{"text": "again"}, mcpserver.CallOptions{})
	if err != nil || textOf(t, res) != "again" {
		t.Fatalf("CallTool = %+v, %v", res, err)
	}
	refreshes := 0
	for _, g := range as.Grants() {
		if g == "refresh_token" {
			refreshes++
		}
	}
	if refreshes != 1 {
		t.Fatalf("refreshes = %d (%v)", refreshes, as.Grants())
	}
}

func TestMcpRefusedRefreshGoesBackToNeedsAuth(t *testing.T) {
	as, r := oauthRig(t)
	signIn(t, r.hub)
	a := r.spawn(t, "a1", []string{"remote"})
	if err := a.StartMcp(t.Context()); err != nil {
		t.Fatal(err)
	}
	as.RevokeAll()
	as.SetRefuseRefresh(true)
	signedOut := waitKind(t, r.env.bus, events.McpAuthChanged, func(p events.McpAuthChangedPayload) bool { return !p.SignedIn })
	_, err := a.Mcp().CallTool(t.Context(), "remote", "echo", map[string]any{"text": "x"}, mcpserver.CallOptions{})
	if !errors.Is(err, &mcpserver.Error{Code: mcpserver.CodeUnauthorized}) {
		t.Fatalf("CallTool err = %v, want unauthorized", err)
	}
	recv(t, signedOut, "mcp-server:auth-changed")
	waitUntil(t, "needs_auth", func() bool {
		st, _ := a.Mcp().Get("remote")
		return st.Status == mcpserver.StatusNeedsAuth
	})
	st, _ := r.hub.OAuth().Status(t.Context(), "remote")
	if st.SignedIn || !strings.Contains(st.Error, "invalid_grant") {
		t.Fatalf("auth status = %+v", st)
	}
}

func TestMcpDeviceSignInConnects(t *testing.T) {
	as, r := oauthRig(t)
	a := r.spawn(t, "a1", []string{"remote"})
	if err := a.StartMcp(t.Context()); err != nil {
		t.Fatal(err)
	}
	if st, _ := a.Mcp().Get("remote"); st.Status != mcpserver.StatusNeedsAuth {
		t.Fatalf("state = %+v", st)
	}
	connected := waitKind(t, r.env.bus, events.McpConnected, nil)
	if _, err := r.hub.OAuth().StartDeviceSignIn(t.Context(), "remote", ""); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, "polls while pending", func() bool { return len(as.Grants()) >= 2 })
	as.ApproveDevice()
	recv(t, connected, "mcp:connected")
	if st, _ := a.Mcp().Get("remote"); st.Status != mcpserver.StatusConnected {
		t.Fatalf("state = %+v", st)
	}
}
