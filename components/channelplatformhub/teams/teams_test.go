package teams_test

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
	"net/url"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Phong95/aec-agent-go/components/channelplatformhub/channel"
	"github.com/Phong95/aec-agent-go/components/channelplatformhub/teams"
	"github.com/Phong95/aec-agent-go/internal/jsonx"
)

// A-100 phase 3 - the Microsoft Teams adapter against a scripted Bot Framework (httptest,
// no network) with a generated signing key: the teams cases of core/ts
// tests/unit/channel-platforms.test.ts.

// key is the signing key of the fake Bot Framework, made once for the package.
var key = sync.OnceValue(func() *rsa.PrivateKey {
	k, _ := rsa.GenerateKey(rand.Reader, 2048)
	return k
})

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

func token(t *testing.T, claims map[string]any, kid string) string {
	t.Helper()
	head, err := jsonx.Marshal(map[string]any{"alg": "RS256", "kid": kid, "typ": "JWT"})
	if err != nil {
		t.Fatal(err)
	}
	body, err := jsonx.Marshal(claims)
	if err != nil {
		t.Fatal(err)
	}
	signed := b64(head) + "." + b64(body)
	digest := sha256.Sum256([]byte(signed))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key(), crypto.SHA256, digest[:])
	if err != nil {
		t.Fatal(err)
	}
	return signed + "." + b64(sig)
}

type call struct {
	method string
	path   string
	auth   string
	body   map[string]any
}

type fakeBotFramework struct {
	srv   *httptest.Server
	mu    sync.Mutex
	calls []call
}

func newFake(t *testing.T) *fakeBotFramework {
	t.Helper()
	f := &fakeBotFramework{}
	f.srv = httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeBotFramework) handle(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(r.Body)
	var body map[string]any
	_ = json.Unmarshal(raw, &body)
	f.mu.Lock()
	f.calls = append(f.calls, call{r.Method, r.URL.EscapedPath(), r.Header.Get("Authorization"), body})
	f.mu.Unlock()
	switch path := r.URL.EscapedPath(); {
	case path == "/openid":
		_, _ = io.WriteString(w, `{"jwks_uri":"`+f.srv.URL+`/keys"}`)
	case path == "/keys":
		e := big.NewInt(int64(key().E)).Bytes()
		_, _ = io.WriteString(w, `{"keys":[{"kty":"RSA","kid":"k1","n":"`+b64(key().N.Bytes())+`","e":"`+b64(e)+`","endorsements":["msteams"]}]}`)
	case path == "/token":
		form, _ := url.ParseQuery(string(raw))
		if form.Get("client_secret") != "secret" {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, `{"error":"invalid_client"}`)
			return
		}
		_, _ = io.WriteString(w, `{"access_token":"bot-token","expires_in":3600}`)
	case r.Method == http.MethodPost && path == "/teams/v3/conversations/a%3A1/activities":
		_, _ = io.WriteString(w, `{"id":"act-1"}`)
	default:
		_, _ = io.WriteString(w, `{}`)
	}
}

func (f *fakeBotFramework) lastCall() call {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[len(f.calls)-1]
}

func (f *fakeBotFramework) options() teams.Options {
	return teams.Options{OpenIDMetadataURL: f.srv.URL + "/openid", TokenURL: f.srv.URL + "/token", Client: f.srv.Client()}
}

type recordingSink struct {
	mu       sync.Mutex
	messages []channel.InboundMessage
	presses  []channel.ButtonPress
}

func (s *recordingSink) Message(m channel.InboundMessage) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.messages = append(s.messages, m)
}

func (s *recordingSink) Button(p channel.ButtonPress) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.presses = append(s.presses, p)
}

func (s *recordingSink) Status(channel.AdapterStatus, string) {}

type rig struct {
	f      *fakeBotFramework
	a      *teams.Adapter
	sink   *recordingSink
	claims map[string]any
}

func started(t *testing.T) *rig {
	t.Helper()
	f := newFake(t)
	a := teams.NewAdapter("app-1", "secret", "", f.options())
	sink := &recordingSink{}
	account, err := a.Start(context.Background(), sink)
	if err != nil {
		t.Fatal(err)
	}
	if account != (channel.AccountInfo{ID: "app-1", DisplayName: "Teams bot"}) {
		t.Fatalf("account = %+v", account)
	}
	t.Cleanup(func() { _ = a.Stop(context.Background()) })
	now := time.Now().Unix()
	claims := map[string]any{"iss": "https://api.botframework.com", "aud": "app-1", "exp": now + 3600, "nbf": now - 10, "serviceurl": f.srv.URL + "/teams/"}
	return &rig{f: f, a: a, sink: sink, claims: claims}
}

func (r *rig) activity(extra map[string]any) map[string]any {
	act := map[string]any{
		"type": "message", "id": "msg-1", "serviceUrl": r.f.srv.URL + "/teams/",
		"from":         map[string]any{"id": "29:u1", "aadObjectId": "aad-1", "name": "Ann"},
		"recipient":    map[string]any{"id": "28:app-1", "name": "AEC"},
		"conversation": map[string]any{"id": "a:1", "conversationType": "personal"},
		"text":         "hello",
	}
	for k, v := range extra {
		if v == nil {
			delete(act, k)
		} else {
			act[k] = v
		}
	}
	return act
}

func (r *rig) post(t *testing.T, act map[string]any, auth string) channel.WebhookResponse {
	t.Helper()
	raw, err := jsonx.Marshal(act)
	if err != nil {
		t.Fatal(err)
	}
	return r.a.HandleWebhook(context.Background(), channel.WebhookRequest{Method: "POST", Headers: map[string]string{"authorization": auth}, Body: string(raw)})
}

func (r *rig) with(changes map[string]any) map[string]any {
	c := map[string]any{}
	for k, v := range r.claims {
		c[k] = v
	}
	for k, v := range changes {
		c[k] = v
	}
	return c
}

func TestStartRejectsAWrongSecret(t *testing.T) {
	f := newFake(t)
	a := teams.NewAdapter("app-1", "wrong", "", f.options())
	_, err := a.Start(context.Background(), &recordingSink{})
	if err == nil || err.Error() != "Microsoft rejected the app id or client secret (invalid_client)" {
		t.Fatalf("err = %v", err)
	}
}

func TestAcceptsASignedActivityAndDeliversTheMessage(t *testing.T) {
	r := started(t)
	res := r.post(t, r.activity(nil), "Bearer "+token(t, r.claims, "k1"))
	if res.Status != 200 || !reflect.DeepEqual(res.Body, map[string]any{}) {
		t.Fatalf("response = %+v", res)
	}
	want := []channel.InboundMessage{{MessageID: "msg-1", Chat: channel.Chat{ID: "a:1", Type: channel.ChatDM}, Sender: channel.Sender{ID: "aad-1", Name: "Ann"}, Text: "hello"}}
	if !reflect.DeepEqual(r.sink.messages, want) {
		t.Fatalf("messages = %+v", r.sink.messages)
	}
}

func TestRejectsBadTokensAndAForeignServiceURL(t *testing.T) {
	r := started(t)
	now := time.Now().Unix()
	good := token(t, r.claims, "k1")
	cases := map[string]string{
		"missing":     "",
		"forged":      "Bearer " + good[:len(good)-4] + "AAAA",
		"audience":    "Bearer " + token(t, r.with(map[string]any{"aud": "other-app"}), "k1"),
		"issuer":      "Bearer " + token(t, r.with(map[string]any{"iss": "https://evil.test"}), "k1"),
		"expired":     "Bearer " + token(t, r.with(map[string]any{"exp": now - 3600}), "k1"),
		"unknown kid": "Bearer " + token(t, r.claims, "unknown-kid"),
	}
	for name, auth := range cases {
		if res := r.post(t, r.activity(nil), auth); res.Status != 401 {
			t.Errorf("%s: status = %d", name, res.Status)
		}
	}
	if res := r.post(t, r.activity(map[string]any{"serviceUrl": "https://evil.test/"}), "Bearer "+good); res.Status != 401 {
		t.Errorf("serviceUrl mismatch: status = %d", res.Status)
	}
	if len(r.sink.messages) != 0 {
		t.Fatalf("messages = %+v", r.sink.messages)
	}
}

func TestReadsGroupMentionsAndCardButtonPresses(t *testing.T) {
	r := started(t)
	auth := "Bearer " + token(t, r.claims, "k1")
	r.post(t, r.activity(map[string]any{
		"conversation": map[string]any{"id": "19:chan;messageid=1", "conversationType": "channel", "name": "General"},
		"text":         "<at>AEC</at> run the report",
		"entities":     []any{map[string]any{"type": "mention", "text": "<at>AEC</at>", "mentioned": map[string]any{"id": "28:app-1", "name": "AEC"}}},
	}), auth)
	r.post(t, r.activity(map[string]any{"text": nil, "value": map[string]any{"aec": "p:2:n"}, "replyToId": "act-9"}), auth)
	wantMsg := channel.InboundMessage{
		MessageID: "msg-1", Chat: channel.Chat{ID: "19:chan;messageid=1", Type: channel.ChatGroup, Title: "General"},
		Sender: channel.Sender{ID: "aad-1", Name: "Ann"}, Text: "run the report", MentionsBot: true,
	}
	if !reflect.DeepEqual(r.sink.messages, []channel.InboundMessage{wantMsg}) {
		t.Fatalf("messages = %+v", r.sink.messages)
	}
	wantPress := channel.ButtonPress{Chat: channel.Chat{ID: "a:1", Type: channel.ChatDM}, Sender: channel.Sender{ID: "aad-1", Name: "Ann"}, MessageID: "act-9", Data: "p:2:n"}
	if !reflect.DeepEqual(r.sink.presses, []channel.ButtonPress{wantPress}) {
		t.Fatalf("presses = %+v", r.sink.presses)
	}
}

func TestRepliesThroughTheServiceURLWithAnAppTokenAndButtonsAsACard(t *testing.T) {
	r := started(t)
	ctx := context.Background()
	if _, err := r.a.Send(ctx, channel.Target{ChatID: "a:1"}, channel.OutboundMessage{Text: "hi"}); err == nil || !strings.Contains(err.Error(), "has not sent the bot a message") {
		t.Fatalf("err = %v", err)
	}
	r.post(t, r.activity(nil), "Bearer "+token(t, r.claims, "k1"))
	ref, err := r.a.Send(ctx, channel.Target{ChatID: "a:1"}, channel.OutboundMessage{
		Text: "Approve?", Format: channel.FormatMarkdown, Buttons: [][]channel.Button{{{Label: "Approve", Data: "p:1:y"}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if ref != (channel.MessageRef{ChatID: "a:1", MessageID: "act-1"}) {
		t.Fatalf("ref = %+v", ref)
	}
	sent := r.f.lastCall()
	if sent.auth != "Bearer bot-token" || sent.path != "/teams/v3/conversations/a%3A1/activities" {
		t.Fatalf("sent = %+v", sent)
	}
	card := sent.body["attachments"].([]any)[0].(map[string]any)
	content := card["content"].(map[string]any)
	if card["contentType"] != "application/vnd.microsoft.card.adaptive" || content["type"] != "AdaptiveCard" ||
		!reflect.DeepEqual(content["body"], []any{map[string]any{"type": "TextBlock", "text": "Approve?", "wrap": true}}) ||
		!reflect.DeepEqual(content["actions"], []any{map[string]any{"type": "Action.Submit", "title": "Approve", "data": map[string]any{"aec": "p:1:y"}}}) {
		t.Fatalf("card = %v", card)
	}
	if err := r.a.Edit(ctx, ref, channel.OutboundMessage{Text: "**done**", Format: channel.FormatMarkdown}); err != nil {
		t.Fatal(err)
	}
	last := r.f.lastCall()
	want := map[string]any{"type": "message", "id": "act-1", "text": "**done**", "textFormat": "markdown", "attachments": []any{}}
	if last.method != http.MethodPut || !reflect.DeepEqual(last.body, want) {
		t.Fatalf("edit = %+v", last)
	}
}
