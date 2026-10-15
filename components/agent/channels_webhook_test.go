package agent_test

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/Phong95/aec-agent-go/components/channelplatformhub/channel"
	"github.com/Phong95/aec-agent-go/components/channelplatformhub/teams"
	"github.com/Phong95/aec-agent-go/internal/jsonx"
)

// A-100 phase 3 - a webhook reaches its channel through Channels.HandleWebhook: a Teams
// activity runs a turn and the reply goes back through the Bot Framework (a scripted
// one, httptest). The "AgentCore routes a webhook" case of core/ts
// tests/unit/channel-platforms.test.ts, at the agent's level.

func TestChannelWebhookRunsATurnAndRepliesThroughTeams(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	b64 := base64.RawURLEncoding.EncodeToString
	var mu sync.Mutex
	var replies []string
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/openid":
			_, _ = io.WriteString(w, `{"jwks_uri":"`+srv.URL+`/keys"}`)
		case "/keys":
			_, _ = io.WriteString(w, `{"keys":[{"kty":"RSA","kid":"k1","n":"`+b64(key.N.Bytes())+`","e":"`+b64(big.NewInt(int64(key.E)).Bytes())+`"}]}`)
		case "/token":
			_, _ = io.WriteString(w, `{"access_token":"bot-token","expires_in":3600}`)
		default:
			var body map[string]any
			raw, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(raw, &body)
			if text, ok := body["text"].(string); ok {
				mu.Lock()
				replies = append(replies, text)
				mu.Unlock()
			}
			_, _ = io.WriteString(w, `{"id":"act-1"}`)
		}
	}))
	t.Cleanup(srv.Close)

	r := newChannelRig(t, 4000)
	mustOK(t, r.hub.Register(teams.Platform(teams.Options{OpenIDMetadataURL: srv.URL + "/openid", TokenURL: srv.URL + "/token", Client: srv.Client()})))
	r.useRunner(func(_ context.Context, tc channel.TurnContext) (channel.TurnResult, error) {
		return channel.TurnResult{Output: "echo: " + tc.Text}, nil
	})
	open := channel.DmOpen
	ch, err := r.a.Channels().Create(t.Context(), channel.CreateInput{
		Platform: "teams", Credentials: map[string]string{"appId": "app-1", "appPassword": "secret"}, Access: &channel.AccessInput{DmPolicy: &open},
	})
	mustOK(t, err)
	fake, _ := r.open(t)

	head, _ := jsonx.Marshal(map[string]any{"alg": "RS256", "kid": "k1"})
	now := time.Now().Unix()
	claims, _ := jsonx.Marshal(map[string]any{"iss": "https://api.botframework.com", "aud": "app-1", "exp": now + 3600, "serviceurl": srv.URL + "/teams/"})
	signed := b64(head) + "." + b64(claims)
	digest := sha256.Sum256([]byte(signed))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
	mustOK(t, err)
	activity, _ := jsonx.Marshal(map[string]any{
		"type": "message", "id": "msg-1", "serviceUrl": srv.URL + "/teams/", "text": "hello",
		"from":         map[string]any{"id": "29:u1", "aadObjectId": "aad-1", "name": "Ann"},
		"recipient":    map[string]any{"id": "28:app-1"},
		"conversation": map[string]any{"id": "a:1", "conversationType": "personal"},
	})
	req := channel.WebhookRequest{Method: "POST", Headers: map[string]string{"authorization": "Bearer " + signed + "." + b64(sig)}, Body: string(activity)}

	if res := r.a.Channels().HandleWebhook(t.Context(), ch.ID, req); res.Status != 200 {
		t.Fatalf("status = %d, body = %v", res.Status, res.Body)
	}
	waitFor(t, "the reply through Teams", func() bool {
		mu.Lock()
		defer mu.Unlock()
		for _, text := range replies {
			if text == "echo: hello" {
				return true
			}
		}
		return false
	})

	if res := r.a.Channels().HandleWebhook(t.Context(), "missing", req); res.Status != 404 {
		t.Fatalf("missing channel: status = %d", res.Status)
	}
	if res := r.a.Channels().HandleWebhook(t.Context(), fake.ID, req); res.Status != 405 {
		t.Fatalf("fake platform: status = %d", res.Status)
	}
}
