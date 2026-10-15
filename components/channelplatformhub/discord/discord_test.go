package discord_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/Phong95/aec-agent-go/components/channelplatformhub/channel"
	"github.com/Phong95/aec-agent-go/components/channelplatformhub/discord"
	"github.com/Phong95/aec-agent-go/internal/jsonx"
)

// A-100 phase 3 - the Discord adapter against a scripted REST API and gateway (httptest,
// no network): the discord cases of core/ts tests/unit/channel-platforms.test.ts.

type call struct {
	method string
	path   string
	auth   string
	body   map[string]any
}

type serverConn struct {
	path     string
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

type fakeDiscord struct {
	srv   *httptest.Server
	mu    sync.Mutex
	calls []call
	conns []*serverConn
}

func newFakeDiscord(t *testing.T) *fakeDiscord {
	t.Helper()
	f := &fakeDiscord{}
	f.srv = httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeDiscord) handle(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/gw/" || r.URL.Path == "/resume/" {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		sc := &serverConn{path: r.URL.Path, conn: c}
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
	raw, _ := io.ReadAll(r.Body)
	var body map[string]any
	_ = json.Unmarshal(raw, &body)
	path := strings.TrimPrefix(r.URL.Path, "/api/v10")
	f.mu.Lock()
	f.calls = append(f.calls, call{r.Method, path, r.Header.Get("Authorization"), body})
	f.mu.Unlock()
	switch {
	case r.Method == http.MethodGet && path == "/users/@me":
		_, _ = io.WriteString(w, `{"id":"B1","username":"aecbot","global_name":"AEC Bot"}`)
	case r.Method == http.MethodGet && path == "/gateway/bot":
		_, _ = io.WriteString(w, `{"url":"ws://`+r.Host+`/gw"}`)
	case r.Method == http.MethodPost && path == "/channels/C1/messages":
		_, _ = io.WriteString(w, `{"id":"M1","channel_id":"C1"}`)
	default:
		w.WriteHeader(http.StatusNoContent)
	}
}

func (f *fakeDiscord) lastCall() call {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[len(f.calls)-1]
}

func (f *fakeDiscord) find(pred func(call) bool) (call, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.calls {
		if pred(c) {
			return c, true
		}
	}
	return call{}, false
}

func (f *fakeDiscord) socket(t *testing.T, n int) *serverConn {
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

// handshake answers hello and READY (or the given frame) on socket n.
func (f *fakeDiscord) handshake(t *testing.T, n int, ready any) *serverConn {
	t.Helper()
	ws := f.socket(t, n)
	ws.push(t, map[string]any{"op": 10, "d": map[string]any{"heartbeat_interval": 60000}})
	waitFor(t, func() bool { return len(ws.frames()) >= 1 })
	ws.push(t, ready)
	return ws
}

func readyFrame(host string) map[string]any {
	return map[string]any{"op": 0, "s": 1, "t": "READY", "d": map[string]any{"session_id": "S1", "resume_gateway_url": "ws://" + host + "/resume"}}
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

type startResult struct {
	account channel.AccountInfo
	err     error
}

func startAsync(a *discord.Adapter, sink channel.Sink) chan startResult {
	out := make(chan startResult, 1)
	go func() {
		acc, err := a.Start(context.Background(), sink)
		out <- startResult{acc, err}
	}()
	return out
}

func started(t *testing.T, f *fakeDiscord) (*discord.Adapter, *recordingSink, channel.AccountInfo, *serverConn) {
	t.Helper()
	a := discord.NewAdapter("tok", discord.Options{APIBaseURL: f.srv.URL + "/api/v10", Client: f.srv.Client()})
	sink := &recordingSink{}
	res := startAsync(a, sink)
	host := strings.TrimPrefix(f.srv.URL, "http://")
	ws := f.handshake(t, 1, readyFrame(host))
	r := <-res
	if r.err != nil {
		t.Fatal(r.err)
	}
	t.Cleanup(func() { _ = a.Stop(context.Background()) })
	return a, sink, r.account, ws
}

func TestStartIdentifiesWithTheMessageIntentsAndWaitsForReady(t *testing.T) {
	f := newFakeDiscord(t)
	_, _, account, ws := started(t, f)
	if account != (channel.AccountInfo{ID: "B1", Username: "aecbot", DisplayName: "AEC Bot"}) {
		t.Fatalf("account = %+v", account)
	}
	if ws.path != "/gw/" {
		t.Fatalf("gateway path = %s", ws.path)
	}
	want := map[string]any{"op": float64(2), "d": map[string]any{"token": "tok", "intents": float64(37376), "properties": map[string]any{"os": "linux", "browser": "aec-agent", "device": "aec-agent"}}}
	if got := ws.frames()[0]; !reflect.DeepEqual(got, want) {
		t.Fatalf("identify = %v", got)
	}
	if c, _ := f.find(func(c call) bool { return c.path == "/users/@me" }); c.auth != "Bot tok" {
		t.Fatalf("auth = %q", c.auth)
	}
}

func TestDeliversDMsAndGuildMessagesWithTheBotsMentionAsName(t *testing.T) {
	f := newFakeDiscord(t)
	_, sink, _, ws := started(t, f)
	ws.push(t, map[string]any{"op": 0, "s": 2, "t": "MESSAGE_CREATE", "d": map[string]any{"id": "1", "type": 0, "channel_id": "D1", "author": map[string]any{"id": "U1", "username": "ann", "global_name": "Ann"}, "content": "hi"}})
	ws.push(t, map[string]any{"op": 0, "s": 3, "t": "MESSAGE_CREATE", "d": map[string]any{
		"id": "2", "type": 19, "channel_id": "C1", "guild_id": "G1", "author": map[string]any{"id": "U1", "username": "ann"}, "content": "<@B1> look",
		"mentions": []any{map[string]any{"id": "B1", "username": "aecbot"}}, "referenced_message": map[string]any{"author": map[string]any{"id": "B1"}},
	}})
	ws.push(t, map[string]any{"op": 0, "s": 4, "t": "MESSAGE_CREATE", "d": map[string]any{"id": "3", "type": 7, "channel_id": "C1", "guild_id": "G1", "author": map[string]any{"id": "U2", "username": "bob"}, "content": ""}})
	waitFor(t, func() bool { m, _, _ := sink.snapshot(); return len(m) == 2 })
	time.Sleep(20 * time.Millisecond)
	msgs, _, _ := sink.snapshot()
	want := []channel.InboundMessage{
		{MessageID: "1", Chat: channel.Chat{ID: "D1", Type: channel.ChatDM}, Sender: channel.Sender{ID: "U1", Name: "Ann", Username: "ann"}, Text: "hi"},
		{MessageID: "2", Chat: channel.Chat{ID: "C1", Type: channel.ChatGroup}, Sender: channel.Sender{ID: "U1", Name: "ann", Username: "ann"}, Text: "@aecbot look", MentionsBot: true, ReplyToBot: true},
	}
	if !reflect.DeepEqual(msgs, want) {
		t.Fatalf("messages = %+v", msgs)
	}
}

func TestAcknowledgesAButtonPressAndHandsItToTheSink(t *testing.T) {
	f := newFakeDiscord(t)
	_, sink, _, ws := started(t, f)
	ws.push(t, map[string]any{"op": 0, "s": 2, "t": "INTERACTION_CREATE", "d": map[string]any{
		"id": "I1", "token": "itok", "type": 3, "data": map[string]any{"custom_id": "p:1:y"}, "channel_id": "C1", "guild_id": "G1",
		"member": map[string]any{"user": map[string]any{"id": "U1", "username": "ann"}}, "message": map[string]any{"id": "M1"},
	}})
	waitFor(t, func() bool { _, p, _ := sink.snapshot(); return len(p) == 1 })
	_, presses, _ := sink.snapshot()
	want := channel.ButtonPress{Chat: channel.Chat{ID: "C1", Type: channel.ChatGroup}, Sender: channel.Sender{ID: "U1", Name: "ann", Username: "ann"}, MessageID: "M1", Data: "p:1:y"}
	if !reflect.DeepEqual(presses[0], want) {
		t.Fatalf("press = %+v", presses[0])
	}
	cb, ok := f.find(func(c call) bool { return strings.Contains(c.path, "/interactions/I1/itok/callback") })
	if !ok || !reflect.DeepEqual(cb.body, map[string]any{"type": float64(6)}) {
		t.Fatalf("callback = %+v", cb)
	}
}

func TestSendPostsMarkdownAsItIsWithComponentsAndNoMentions(t *testing.T) {
	f := newFakeDiscord(t)
	a, _, _, _ := started(t, f)
	_, err := a.Send(context.Background(), channel.Target{ChatID: "C1"}, channel.OutboundMessage{
		Text: "**ok** @everyone", Format: channel.FormatMarkdown,
		Buttons: [][]channel.Button{{{Label: "Approve", Data: "p:1:y"}, {Label: "Deny", Data: "p:1:n"}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]any{
		"content":          "**ok** @everyone",
		"allowed_mentions": map[string]any{"parse": []any{}},
		"components": []any{map[string]any{"type": float64(1), "components": []any{
			map[string]any{"type": float64(2), "style": float64(3), "label": "Approve", "custom_id": "p:1:y"},
			map[string]any{"type": float64(2), "style": float64(4), "label": "Deny", "custom_id": "p:1:n"},
		}}},
	}
	if got := f.lastCall().body; !reflect.DeepEqual(got, want) {
		t.Fatalf("body = %v", got)
	}
}

func TestResumesTheSessionAfterTheConnectionDrops(t *testing.T) {
	f := newFakeDiscord(t)
	_, sink, _, ws := started(t, f)
	_ = ws.conn.Close(websocket.StatusGoingAway, "going away")
	next := f.handshake(t, 2, map[string]any{"op": 0, "s": 2, "t": "RESUMED", "d": map[string]any{}})
	if next.path != "/resume/" {
		t.Fatalf("resume path = %s", next.path)
	}
	want := map[string]any{"op": float64(6), "d": map[string]any{"token": "tok", "session_id": "S1", "seq": float64(1)}}
	if got := next.frames()[0]; !reflect.DeepEqual(got, want) {
		t.Fatalf("resume = %v", got)
	}
	waitFor(t, func() bool { _, _, s := sink.snapshot(); return len(s) > 0 && s[len(s)-1] == "running:" })
}

func TestStartFailsClearlyWhenTheMessageContentIntentIsNotEnabled(t *testing.T) {
	f := newFakeDiscord(t)
	a := discord.NewAdapter("tok", discord.Options{APIBaseURL: f.srv.URL + "/api/v10", Client: f.srv.Client()})
	res := startAsync(a, &recordingSink{})
	ws := f.socket(t, 1)
	ws.push(t, map[string]any{"op": 10, "d": map[string]any{"heartbeat_interval": 60000}})
	_ = ws.conn.Close(websocket.StatusCode(4014), "Disallowed intent(s).")
	r := <-res
	if r.err == nil || !strings.Contains(r.err.Error(), "enable it under Bot -> Privileged Gateway Intents") {
		t.Fatalf("err = %v", r.err)
	}
}
