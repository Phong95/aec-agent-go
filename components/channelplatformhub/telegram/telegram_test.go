package telegram_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Phong95/aec-agent-go/components/channelplatformhub/channel"
	"github.com/Phong95/aec-agent-go/components/channelplatformhub/telegram"
)

// A-100 - the Telegram adapter against a scripted Bot API (httptest, no network).

type call struct {
	method string
	body   map[string]any
}

// fakeBotAPI answers getMe from me (or meError), hands out the queued getUpdates batches
// (then holds the long poll until the request ends), and answers other methods from
// replies or a default.
type fakeBotAPI struct {
	srv *httptest.Server

	mu      sync.Mutex
	calls   []call
	batches []string
	replies map[string][]string
	meError string
}

func newFakeBotAPI(t *testing.T) *fakeBotAPI {
	t.Helper()
	f := &fakeBotAPI{replies: map[string][]string{}}
	f.srv = httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeBotAPI) handle(w http.ResponseWriter, r *http.Request) {
	method := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
	raw, _ := io.ReadAll(r.Body)
	var body map[string]any
	_ = json.Unmarshal(raw, &body)
	f.mu.Lock()
	f.calls = append(f.calls, call{method, body})
	var reply string
	if q := f.replies[method]; len(q) > 0 {
		reply, f.replies[method] = q[0], q[1:]
	}
	var batch string
	if method == "getUpdates" && reply == "" && len(f.batches) > 0 {
		batch, f.batches = f.batches[0], f.batches[1:]
	}
	meError := f.meError
	f.mu.Unlock()

	switch {
	case method == "getMe" && meError != "":
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, meError)
	case method == "getMe":
		_, _ = io.WriteString(w, `{"ok":true,"result":{"id":42,"is_bot":true,"first_name":"AEC","last_name":"Bot","username":"aec_bot"}}`)
	case reply != "":
		_, _ = io.WriteString(w, reply)
	case batch != "":
		_, _ = io.WriteString(w, batch)
	case method == "getUpdates":
		// Long poll: hold until the client gives up.
		<-r.Context().Done()
	case method == "sendMessage":
		_, _ = io.WriteString(w, `{"ok":true,"result":{"message_id":900,"chat":{"id":5}}}`)
	default:
		_, _ = io.WriteString(w, `{"ok":true,"result":true}`)
	}
}

func (f *fakeBotAPI) callsOf(method string) []call {
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

func (f *fakeBotAPI) adapter(t *testing.T) *telegram.Adapter {
	t.Helper()
	a := telegram.NewAdapter("123:secret", telegram.Options{APIBaseURL: f.srv.URL + "/", PollTimeoutSeconds: 1})
	t.Cleanup(func() { _ = a.Stop(context.Background()) })
	return a
}

type sink struct {
	mu       sync.Mutex
	messages []channel.InboundMessage
	presses  []channel.ButtonPress
	statuses []string
}

func (s *sink) Button(p channel.ButtonPress) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.presses = append(s.presses, p)
}

func (s *sink) Message(m channel.InboundMessage) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.messages = append(s.messages, m)
}

func (s *sink) Status(st channel.AdapterStatus, errText string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.statuses = append(s.statuses, string(st)+"|"+errText)
}

func (s *sink) snapshot() ([]channel.InboundMessage, []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]channel.InboundMessage(nil), s.messages...), append([]string(nil), s.statuses...)
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestStartReadsTheAccountAndDeliversMessages(t *testing.T) {
	f := newFakeBotAPI(t)
	f.batches = []string{`{"ok":true,"result":[
		{"update_id":7,"message":{"message_id":1,"from":{"id":5,"first_name":"Ann","username":"ann"},"chat":{"id":5,"type":"private"},"text":"hello"}},
		{"update_id":8,"edited_message":{}}]}`}
	s := &sink{}
	account, err := f.adapter(t).Start(t.Context(), s)
	if err != nil {
		t.Fatal(err)
	}
	if account != (channel.AccountInfo{ID: "42", Username: "aec_bot", DisplayName: "AEC Bot"}) {
		t.Fatalf("account = %+v", account)
	}
	waitFor(t, "a message", func() bool { m, _ := s.snapshot(); return len(m) == 1 })
	m, _ := s.snapshot()
	want := channel.InboundMessage{MessageID: "1", Chat: channel.Chat{ID: "5", Type: channel.ChatDM}, Sender: channel.Sender{ID: "5", Name: "Ann", Username: "ann"}, Text: "hello"}
	if m[0] != want {
		t.Fatalf("message = %+v", m[0])
	}
	// The next poll confirms the batch through its offset and asks only for messages.
	waitFor(t, "a second poll", func() bool { return len(f.callsOf("getUpdates")) >= 2 })
	polls := f.callsOf("getUpdates")
	if polls[0].body["offset"] != float64(0) || polls[0].body["timeout"] != float64(1) || polls[1].body["offset"] != float64(9) {
		t.Fatalf("polls = %+v", polls)
	}
	if got := polls[0].body["allowed_updates"].([]any); len(got) != 2 || got[0] != "message" || got[1] != "callback_query" {
		t.Fatalf("allowed_updates = %v", got)
	}
}

func TestReadsGroupsTopicsCaptionsMentionsAndReplies(t *testing.T) {
	f := newFakeBotAPI(t)
	f.batches = []string{`{"ok":true,"result":[{"update_id":1,"message":{
		"message_id":3,"message_thread_id":11,"is_topic_message":true,
		"from":{"id":6,"first_name":"Bo"},"chat":{"id":-100,"type":"supergroup","title":"Site team"},
		"caption":"hey @AEC_bot look","caption_entities":[{"type":"mention","offset":4,"length":8}],
		"reply_to_message":{"from":{"id":42}}}}]}`}
	s := &sink{}
	if _, err := f.adapter(t).Start(t.Context(), s); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "a message", func() bool { m, _ := s.snapshot(); return len(m) == 1 })
	m, _ := s.snapshot()
	got := m[0]
	if got.Chat != (channel.Chat{ID: "-100", Type: channel.ChatGroup, ThreadID: "11", Title: "Site team"}) ||
		got.Text != "hey @AEC_bot look" || !got.MentionsBot || !got.ReplyToBot {
		t.Fatalf("message = %+v", got)
	}
}

func TestStartRejectsABadTokenWithoutLeakingIt(t *testing.T) {
	f := newFakeBotAPI(t)
	f.meError = `{"ok":false,"error_code":401,"description":"Unauthorized"}`
	_, err := f.adapter(t).Start(t.Context(), &sink{})
	if err == nil || !strings.Contains(err.Error(), "Telegram rejected the bot token") || strings.Contains(err.Error(), "secret") {
		t.Fatalf("err = %v", err)
	}
	if n := len(f.callsOf("getUpdates")); n != 0 {
		t.Fatalf("polled %d times after a bad token", n)
	}
	// A network failure must not leak the token through the URL either.
	down := telegram.NewAdapter("123:secret", telegram.Options{APIBaseURL: "http://127.0.0.1:1"})
	if _, err := down.Start(t.Context(), &sink{}); err == nil || strings.Contains(err.Error(), "secret") {
		t.Fatalf("network err = %v", err)
	}
}

func TestConflictIsRetriedAndRevokedTokenFails(t *testing.T) {
	f := newFakeBotAPI(t)
	f.replies["getUpdates"] = []string{
		`{"ok":false,"error_code":409,"description":"Conflict","parameters":{"retry_after":0.01}}`,
		`{"ok":true,"result":[]}`,
		`{"ok":false,"error_code":401,"description":"Unauthorized"}`,
	}
	s := &sink{}
	if _, err := f.adapter(t).Start(t.Context(), s); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "three statuses", func() bool { _, st := s.snapshot(); return len(st) == 3 })
	_, st := s.snapshot()
	if !strings.HasPrefix(st[0], "retrying|Another process") || st[1] != "running|" || !strings.HasPrefix(st[2], "failed|Telegram rejected the bot token") {
		t.Fatalf("statuses = %q", st)
	}
	polls := len(f.callsOf("getUpdates"))
	time.Sleep(30 * time.Millisecond)
	if n := len(f.callsOf("getUpdates")); n != polls {
		t.Fatal("polling went on after the token was revoked")
	}
}

func TestSendPostsTextAndRetriesOne429(t *testing.T) {
	f := newFakeBotAPI(t)
	a := f.adapter(t)
	if _, err := a.Start(t.Context(), &sink{}); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.replies["sendMessage"] = []string{`{"ok":false,"error_code":429,"description":"Too Many Requests","parameters":{"retry_after":0.01}}`}
	f.mu.Unlock()
	ref, err := a.Send(t.Context(), channel.Target{ChatID: "5", ThreadID: "11", ReplyToMessageID: "3"}, channel.OutboundMessage{Text: "hi *there*"})
	if err != nil {
		t.Fatal(err)
	}
	sends := f.callsOf("sendMessage")
	if len(sends) != 2 || ref.ChatID != "5" || ref.MessageID != "900" {
		t.Fatalf("sends = %d, ref = %+v", len(sends), ref)
	}
	body := sends[1].body
	reply := body["reply_parameters"].(map[string]any)
	if body["chat_id"] != "5" || body["text"] != "hi *there*" || body["message_thread_id"] != float64(11) || reply["message_id"] != float64(3) || reply["allow_sending_without_reply"] != true {
		t.Fatalf("body = %v", body)
	}
	if err := a.SendTyping(t.Context(), channel.Target{ChatID: "5"}); err != nil {
		t.Fatal(err)
	}
	if typing := f.callsOf("sendChatAction"); len(typing) != 1 || typing[0].body["action"] != "typing" {
		t.Fatalf("typing = %+v", typing)
	}
}

func TestSetCommandsPublishesTheCommandList(t *testing.T) {
	f := newFakeBotAPI(t)
	a := f.adapter(t)
	err := a.SetCommands(t.Context(), []channel.BotCommand{
		{Name: "new", Description: "Start a new conversation"},
		{Name: "stop", Description: "Stop the reply"},
	})
	if err != nil {
		t.Fatal(err)
	}
	calls := f.callsOf("setMyCommands")
	if len(calls) != 1 {
		t.Fatalf("setMyCommands calls = %d", len(calls))
	}
	got, _ := json.Marshal(calls[0].body)
	if want := `{"commands":[{"command":"new","description":"Start a new conversation"},{"command":"stop","description":"Stop the reply"}]}`; string(got) != want {
		t.Fatalf("body = %s", got)
	}
}

func TestStopEndsTheLongPoll(t *testing.T) {
	f := newFakeBotAPI(t)
	a := f.adapter(t)
	if _, err := a.Start(t.Context(), &sink{}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "a poll", func() bool { return len(f.callsOf("getUpdates")) > 0 })
	if err := a.Stop(t.Context()); err != nil {
		t.Fatal(err)
	}
	n := len(f.callsOf("getUpdates"))
	time.Sleep(20 * time.Millisecond)
	if len(f.callsOf("getUpdates")) != n {
		t.Fatal("polling went on after Stop")
	}
}

func TestPlatform(t *testing.T) {
	p := telegram.Platform()
	if p.Name() != "telegram" || p.DisplayName() != "Telegram" || len(p.CredentialKeys()) != 1 || p.CredentialKeys()[0] != "botToken" {
		t.Fatalf("platform = %s %s %v", p.Name(), p.DisplayName(), p.CredentialKeys())
	}
	a, err := p.New(channel.Channel{Credentials: map[string]string{"botToken": "1:x"}})
	if err != nil || a.Capabilities().MaxMessageLength != 4000 {
		t.Fatalf("adapter = %v %v", a, err)
	}
}
