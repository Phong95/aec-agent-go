package slack_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/Phong95/aec-agent-go/components/channelplatformhub/channel"
	"github.com/Phong95/aec-agent-go/components/channelplatformhub/slack"
	"github.com/Phong95/aec-agent-go/internal/jsonx"
)

// A-100 phase 3 - the Slack adapter against a scripted Web API and Socket Mode server
// (httptest, no network): the slack cases of core/ts tests/unit/channel-platforms.test.ts.

type call struct {
	method string
	auth   string
	body   map[string]any
}

// serverConn is the server side of one socket: frames the adapter sent are in received.
type serverConn struct {
	url      string
	conn     *websocket.Conn
	mu       sync.Mutex
	received []map[string]any
}

func (c *serverConn) push(t *testing.T, frame any) {
	t.Helper()
	raw, err := jsonx.Marshal(frame)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.conn.Write(context.Background(), websocket.MessageText, raw); err != nil {
		t.Fatal(err)
	}
}

func (c *serverConn) frames() []map[string]any {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]map[string]any(nil), c.received...)
}

type fakeSlack struct {
	srv *httptest.Server

	mu          sync.Mutex
	calls       []call
	connections int
	conns       []*serverConn
	authError   string
	openError   string
}

func newFakeSlack(t *testing.T) *fakeSlack {
	t.Helper()
	f := &fakeSlack{}
	f.srv = httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeSlack) handle(w http.ResponseWriter, r *http.Request) {
	if strings.HasPrefix(r.URL.Path, "/link/") {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		sc := &serverConn{url: "ws://" + r.Host + r.URL.Path, conn: c}
		f.mu.Lock()
		f.conns = append(f.conns, sc)
		f.mu.Unlock()
		for {
			_, data, err := c.Read(context.WithoutCancel(r.Context()))
			if err != nil {
				return
			}
			var m map[string]any
			_ = json.Unmarshal(data, &m)
			sc.mu.Lock()
			sc.received = append(sc.received, m)
			sc.mu.Unlock()
		}
	}
	method := strings.TrimPrefix(r.URL.Path, "/api/")
	raw, _ := io.ReadAll(r.Body)
	var body map[string]any
	_ = json.Unmarshal(raw, &body)
	f.mu.Lock()
	f.calls = append(f.calls, call{method, r.Header.Get("Authorization"), body})
	authError, openError := f.authError, f.openError
	if method == "apps.connections.open" && openError == "" {
		f.connections++
	}
	n := f.connections
	f.mu.Unlock()
	switch method {
	case "auth.test":
		if authError != "" {
			_, _ = io.WriteString(w, `{"ok":false,"error":"`+authError+`"}`)
			return
		}
		_, _ = io.WriteString(w, `{"ok":true,"user_id":"UBOT","user":"aecbot","team":"Site"}`)
	case "apps.connections.open":
		if openError != "" {
			_, _ = io.WriteString(w, `{"ok":false,"error":"`+openError+`"}`)
			return
		}
		_, _ = io.WriteString(w, `{"ok":true,"url":"ws://`+r.Host+`/link/`+strconv.Itoa(n)+`"}`)
	case "users.info":
		// A read method: form-encoded, as Slack requires.
		form, _ := url.ParseQuery(string(raw))
		if r.Header.Get("Content-Type") != "application/x-www-form-urlencoded" || form.Get("user") == "" {
			_, _ = io.WriteString(w, `{"ok":false,"error":"invalid_arguments"}`)
			return
		}
		_, _ = io.WriteString(w, `{"ok":true,"user":{"name":"ann","real_name":"Ann Lee","id":"`+form.Get("user")+`"}}`)
	case "chat.postMessage":
		_, _ = io.WriteString(w, `{"ok":true,"channel":"`+body["channel"].(string)+`","ts":"111.222"}`)
	default:
		_, _ = io.WriteString(w, `{"ok":true}`)
	}
}

func (f *fakeSlack) callsOf(method string) []call {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []call
	for _, c := range f.calls {
		if c.method == method {
			out = append(out, c)
		}
	}
	return out
}

func (f *fakeSlack) lastCall() call {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[len(f.calls)-1]
}

func (f *fakeSlack) socket(t *testing.T, n int) *serverConn {
	t.Helper()
	var sc *serverConn
	waitFor(t, func() bool {
		f.mu.Lock()
		defer f.mu.Unlock()
		if len(f.conns) >= n {
			sc = f.conns[n-1]
			return true
		}
		return false
	})
	return sc
}

type recordingSink struct {
	mu       sync.Mutex
	messages []channel.InboundMessage
	presses  []channel.ButtonPress
	statuses []string
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

func (s *recordingSink) Status(st channel.AdapterStatus, errText string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.statuses = append(s.statuses, string(st)+":"+errText)
}

func (s *recordingSink) snapshot() ([]channel.InboundMessage, []channel.ButtonPress, []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]channel.InboundMessage(nil), s.messages...), append([]channel.ButtonPress(nil), s.presses...), append([]string(nil), s.statuses...)
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met in time")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func started(t *testing.T, f *fakeSlack) (*slack.Adapter, *recordingSink, channel.AccountInfo) {
	t.Helper()
	a := slack.NewAdapter("xoxb-1", "xapp-1", slack.Options{APIBaseURL: f.srv.URL + "/api", Client: f.srv.Client()})
	sink := &recordingSink{}
	account, err := a.Start(context.Background(), sink)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.Stop(context.Background()) })
	return a, sink, account
}

func TestStartChecksTokensOpensSocketAndAcksEvents(t *testing.T) {
	f := newFakeSlack(t)
	_, sink, account := started(t, f)
	if want := (channel.AccountInfo{ID: "UBOT", Username: "aecbot", DisplayName: "aecbot (Site)"}); account != want {
		t.Fatalf("account = %+v", account)
	}
	if got := f.callsOf("apps.connections.open")[0].auth; got != "Bearer xapp-1" {
		t.Fatalf("connections.open auth = %q", got)
	}
	if got := f.callsOf("auth.test")[0].auth; got != "Bearer xoxb-1" {
		t.Fatalf("auth.test auth = %q", got)
	}
	ws := f.socket(t, 1)
	if !strings.HasSuffix(ws.url, "/link/1") {
		t.Fatalf("socket url = %s", ws.url)
	}
	ws.push(t, map[string]any{"type": "events_api", "envelope_id": "e1", "payload": map[string]any{"event": map[string]any{
		"type": "message", "channel": "D1", "channel_type": "im", "user": "U1", "text": "hi &amp; bye", "ts": "1.1",
	}}})
	waitFor(t, func() bool { m, _, _ := sink.snapshot(); return len(m) == 1 })
	if got := ws.frames(); !reflect.DeepEqual(got, []map[string]any{{"envelope_id": "e1"}}) {
		t.Fatalf("acks = %v", got)
	}
	msgs, _, _ := sink.snapshot()
	want := channel.InboundMessage{MessageID: "1.1", Chat: channel.Chat{ID: "D1", Type: channel.ChatDM}, Sender: channel.Sender{ID: "U1", Name: "Ann Lee", Username: "ann"}, Text: "hi & bye"}
	if !reflect.DeepEqual(msgs[0], want) {
		t.Fatalf("message = %+v", msgs[0])
	}
}

func TestReadsMentionsThreadsRepliesAndSkipsBots(t *testing.T) {
	f := newFakeSlack(t)
	_, sink, _ := started(t, f)
	ws := f.socket(t, 1)
	ws.push(t, map[string]any{"type": "events_api", "envelope_id": "e1", "payload": map[string]any{"event": map[string]any{"type": "message", "subtype": "message_changed", "channel": "C1", "ts": "1"}}})
	ws.push(t, map[string]any{"type": "events_api", "envelope_id": "e2", "payload": map[string]any{"event": map[string]any{"type": "message", "bot_id": "B9", "user": "U9", "channel": "C1", "ts": "2", "text": "x"}}})
	ws.push(t, map[string]any{"type": "events_api", "envelope_id": "e3", "payload": map[string]any{"event": map[string]any{
		"type": "message", "channel": "C1", "channel_type": "channel", "user": "U1", "text": "<@UBOT> look", "ts": "3", "thread_ts": "0.5", "parent_user_id": "UBOT",
	}}})
	waitFor(t, func() bool { m, _, _ := sink.snapshot(); return len(m) == 1 })
	msgs, _, _ := sink.snapshot()
	m := msgs[0]
	if m.Chat != (channel.Chat{ID: "C1", Type: channel.ChatGroup, ThreadID: "0.5"}) || m.Text != "@aecbot look" || !m.MentionsBot || !m.ReplyToBot {
		t.Fatalf("message = %+v", m)
	}
}

func TestButtonPressesArriveAsInteractiveEnvelopes(t *testing.T) {
	f := newFakeSlack(t)
	_, sink, _ := started(t, f)
	f.socket(t, 1).push(t, map[string]any{"type": "interactive", "envelope_id": "e1", "payload": map[string]any{
		"type": "block_actions", "user": map[string]any{"id": "U1", "name": "ann"}, "channel": map[string]any{"id": "D1"},
		"message": map[string]any{"ts": "5.5"}, "actions": []any{map[string]any{"action_id": "aec_0_p:1:y", "value": "p:1:y"}},
	}})
	waitFor(t, func() bool { _, p, _ := sink.snapshot(); return len(p) == 1 })
	_, presses, _ := sink.snapshot()
	want := channel.ButtonPress{Chat: channel.Chat{ID: "D1", Type: channel.ChatDM}, Sender: channel.Sender{ID: "U1", Name: "Ann Lee", Username: "ann"}, MessageID: "5.5", Data: "p:1:y"}
	if !reflect.DeepEqual(presses[0], want) {
		t.Fatalf("press = %+v", presses[0])
	}
}

func TestSendPostsMrkdwnWithBlocksAndEditDropsButtons(t *testing.T) {
	f := newFakeSlack(t)
	a, _, _ := started(t, f)
	ref, err := a.Send(context.Background(), channel.Target{ChatID: "C1", ThreadID: "0.5"}, channel.OutboundMessage{
		Text: "**Run** it?", Format: channel.FormatMarkdown, Buttons: [][]channel.Button{{{Label: "Approve", Data: "p:1:y"}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if ref != (channel.MessageRef{ChatID: "C1", MessageID: "111.222"}) {
		t.Fatalf("ref = %+v", ref)
	}
	want := map[string]any{
		"channel": "C1", "thread_ts": "0.5", "text": "*Run* it?",
		"blocks": []any{
			map[string]any{"type": "section", "text": map[string]any{"type": "mrkdwn", "text": "*Run* it?"}},
			map[string]any{"type": "actions", "elements": []any{map[string]any{
				"type": "button", "text": map[string]any{"type": "plain_text", "text": "Approve", "emoji": true},
				"action_id": "aec_0_p:1:y", "value": "p:1:y", "style": "primary",
			}}},
		},
	}
	if got := f.callsOf("chat.postMessage")[0].body; !reflect.DeepEqual(got, want) {
		t.Fatalf("postMessage body = %v", got)
	}
	if err := a.Edit(context.Background(), ref, channel.OutboundMessage{Text: "a < b"}); err != nil {
		t.Fatal(err)
	}
	if got := f.lastCall().body; !reflect.DeepEqual(got, map[string]any{"channel": "C1", "ts": "111.222", "blocks": []any{}, "text": "a &lt; b"}) {
		t.Fatalf("update body = %v", got)
	}
}

func TestOpensANewSocketWhenSlackAsksToDisconnect(t *testing.T) {
	f := newFakeSlack(t)
	_, sink, _ := started(t, f)
	f.socket(t, 1).push(t, map[string]any{"type": "disconnect", "reason": "refresh_requested"})
	second := f.socket(t, 2)
	if !strings.HasSuffix(second.url, "/link/2") {
		t.Fatalf("second socket = %s", second.url)
	}
	if n := len(f.callsOf("apps.connections.open")); n != 2 {
		t.Fatalf("connections.open calls = %d", n)
	}
	waitFor(t, func() bool { _, _, s := sink.snapshot(); return len(s) == 1 })
	if _, _, s := sink.snapshot(); !reflect.DeepEqual(s, []string{"running:"}) {
		t.Fatalf("statuses = %v", s)
	}
}

func TestStartRejectsTokensSlackDoesNotAccept(t *testing.T) {
	f := newFakeSlack(t)
	f.authError = "invalid_auth"
	a := slack.NewAdapter("xoxb-bad", "xapp-1", slack.Options{APIBaseURL: f.srv.URL + "/api", Client: f.srv.Client()})
	if _, err := a.Start(context.Background(), &recordingSink{}); err == nil || !strings.Contains(err.Error(), "Slack rejected the bot token") {
		t.Fatalf("err = %v", err)
	}

	g := newFakeSlack(t)
	g.openError = "invalid_auth"
	b := slack.NewAdapter("xoxb-1", "xapp-bad", slack.Options{APIBaseURL: g.srv.URL + "/api", Client: g.srv.Client()})
	if _, err := b.Start(context.Background(), &recordingSink{}); err == nil || !strings.Contains(err.Error(), "Slack rejected the app-level token") {
		t.Fatalf("err = %v", err)
	}
}

func TestMarkdownToMrkdwn(t *testing.T) {
	bullet := string(rune(0x2022))
	got := slack.MarkdownToMrkdwn("# Plan\n**bold** and *it* ~~old~~ `a<b>` [docs](https://x.y/?a=1&b=2)\n- one")
	want := "*Plan*\n*bold* and _it_ ~old~ `a&lt;b&gt;` <https://x.y/?a=1&b=2|docs>\n" + bullet + " one"
	if got != want {
		t.Fatalf("got %q\nwant %q", got, want)
	}
	if got := slack.MarkdownToMrkdwn("```js\nif (a < b) {}\n```"); got != "```\nif (a &lt; b) {}\n```" {
		t.Fatalf("code block = %q", got)
	}
}
