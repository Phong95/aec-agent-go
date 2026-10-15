package telegram_test

import (
	"reflect"
	"testing"

	"github.com/Phong95/aec-agent-go/components/channelplatformhub/channel"
	"github.com/Phong95/aec-agent-go/components/channelplatformhub/telegram"
)

// A-100 phase 2 - Markdown as Telegram HTML, buttons, edits and button presses.

func TestSendRendersMarkdownWithButtonsAndFallsBackToPlainText(t *testing.T) {
	f := newFakeBotAPI(t)
	a := f.adapter(t)
	if _, err := a.Send(t.Context(), channel.Target{ChatID: "5"}, channel.OutboundMessage{
		Text: "**hi**", Format: channel.FormatMarkdown, Buttons: [][]channel.Button{{{Label: "Yes", Data: "p:1:y"}}},
	}); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{
		"chat_id": "5", "text": "<b>hi</b>", "parse_mode": "HTML",
		"reply_markup": map[string]any{"inline_keyboard": []any{[]any{map[string]any{"text": "Yes", "callback_data": "p:1:y"}}}},
	}
	if got := f.callsOf("sendMessage")[0].body; !reflect.DeepEqual(got, want) {
		t.Fatalf("body = %v", got)
	}

	f.mu.Lock()
	f.replies["sendMessage"] = []string{`{"ok":false,"error_code":400,"description":"Bad Request: can't parse entities: unclosed tag"}`}
	f.mu.Unlock()
	if _, err := a.Send(t.Context(), channel.Target{ChatID: "5"}, channel.OutboundMessage{Text: "**hi**", Format: channel.FormatMarkdown}); err != nil {
		t.Fatal(err)
	}
	sends := f.callsOf("sendMessage")
	if last := sends[len(sends)-1].body; len(sends) != 3 || !reflect.DeepEqual(last, map[string]any{"chat_id": "5", "text": "**hi**"}) {
		t.Fatalf("sends = %d, last = %v", len(sends), last)
	}
}

func TestEditReplacesTheTextAndRemovesTheButtons(t *testing.T) {
	f := newFakeBotAPI(t)
	a := f.adapter(t)
	ref := channel.MessageRef{ChatID: "5", MessageID: "77"}
	if err := a.Edit(t.Context(), ref, channel.OutboundMessage{Text: "done"}); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"chat_id": "5", "message_id": float64(77), "text": "done", "reply_markup": map[string]any{"inline_keyboard": []any{}}}
	if got := f.callsOf("editMessageText")[0].body; !reflect.DeepEqual(got, want) {
		t.Fatalf("body = %v", got)
	}
	f.mu.Lock()
	f.replies["editMessageText"] = []string{`{"ok":false,"error_code":400,"description":"Bad Request: message is not modified"}`}
	f.mu.Unlock()
	if err := a.Edit(t.Context(), ref, channel.OutboundMessage{Text: "done"}); err != nil {
		t.Fatalf("unchanged edit: %v", err)
	}
}

func TestAButtonPressIsAcknowledgedAndHandedToTheSink(t *testing.T) {
	f := newFakeBotAPI(t)
	f.batches = append(f.batches, `{"ok":true,"result":[{"update_id":3,"callback_query":{"id":"cb1","from":{"id":5,"first_name":"Ann"},"data":"p:1:y","message":{"message_id":70,"chat":{"id":-100,"type":"supergroup","title":"Site"}}}}]}`)
	s := &sink{}
	if _, err := f.adapter(t).Start(t.Context(), s); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the press", func() bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		return len(s.presses) == 1
	})
	s.mu.Lock()
	press := s.presses[0]
	s.mu.Unlock()
	want := channel.ButtonPress{Chat: channel.Chat{ID: "-100", Type: channel.ChatGroup, Title: "Site"}, Sender: channel.Sender{ID: "5", Name: "Ann"}, MessageID: "70", Data: "p:1:y"}
	if press != want {
		t.Fatalf("press = %+v", press)
	}
	if acks := f.callsOf("answerCallbackQuery"); len(acks) != 1 || acks[0].body["callback_query_id"] != "cb1" {
		t.Fatalf("acks = %+v", acks)
	}
}

func TestMarkdownToHTML(t *testing.T) {
	bulletSign := string(rune(0x2022))
	rule := ""
	for range 10 {
		rule += string(rune(0x2500))
	}
	cases := []struct{ in, want string }{
		{
			"**Bold** and *it* and `a<b>` & [link](https://x.y/?a=1&b=2)",
			`<b>Bold</b> and <i>it</i> and <code>a&lt;b&gt;</code> &amp; <a href="https://x.y/?a=1&amp;b=2">link</a>`,
		},
		{
			"# Title\n- one\n> quote\n```js\nif (a < b) {}\n```",
			"<b>Title</b>\n" + bulletSign + " one\n<blockquote>quote</blockquote>\n<pre><code class=\"language-js\">if (a &lt; b) {}</code></pre>",
		},
		{"snake_case_name and 2 * 3 * 4", "snake_case_name and 2 * 3 * 4"},
		{"**a** and **b** ~~gone~~ _x_ *y*z", "<b>a</b> and <b>b</b> <s>gone</s> <i>x</i> *y*z"},
		{"- - -", rule},
	}
	for _, c := range cases {
		if got := telegram.MarkdownToHTML(c.in); got != c.want {
			t.Errorf("MarkdownToHTML(%q)\n got %q\nwant %q", c.in, got, c.want)
		}
	}
}
