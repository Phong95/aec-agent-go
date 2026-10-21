package channel_test

import (
	"encoding/json"
	"fmt"
	"iter"
	"slices"
	"strings"
	"testing"
	"unicode/utf16"

	"github.com/Phong95/aec-agent-go/components/channelplatformhub/channel"
	"github.com/Phong95/aec-agent-go/internal/clonecheck"
	"github.com/Phong95/aec-agent-go/kernel"
)

// Channel.Clone must share nothing with the original; a pointer, slice or map field
// added to Channel without a Clone line fails here.
func TestChannelCloneIsDeep(t *testing.T) {
	sample := channel.Channel{
		ID: "c1", AgentID: "a1", Platform: "telegram", Name: "n",
		Credentials: map[string]string{"botToken": "t"},
		Access:      channel.Access{DmPolicy: channel.DmAllowlist, AllowFrom: []string{"1"}, GroupPolicy: channel.GroupOpen, Groups: []string{"g"}, RequireMention: true},
		Settings:    channel.Settings{Streaming: true, ToolProgress: true},
		Enabled:     true, Account: &channel.AccountInfo{ID: "42", Username: "u", DisplayName: "d"},
		Home:      &channel.Home{ChatID: "c9", ThreadID: "t1", Title: "Site", SetBy: "1", SetAt: "s"},
		CreatedAt: "c", UpdatedAt: "u",
	}
	clonecheck.AssertDeep(t, sample, channel.Channel.Clone)
}

func TestSplitMessage(t *testing.T) {
	cases := []struct {
		text string
		max  int
		want []string
	}{
		{"  hello  ", 10, []string{"hello"}},
		{"aaaa bbbb\n\ncccc dddd", 12, []string{"aaaa bbbb", "cccc dddd"}},
		{"aaaa bbbb cccc", 10, []string{"aaaa bbbb", "cccc"}},
		{"abcdefghij", 4, []string{"abcd", "efgh", "ij"}},
		{"", 4, nil},
	}
	for _, c := range cases {
		if got := channel.SplitMessage(c.text, c.max); !slices.Equal(got, c.want) {
			t.Errorf("SplitMessage(%q, %d) = %q, want %q", c.text, c.max, got, c.want)
		}
	}
}

// Lengths are UTF-16 units, as in core/ts, and a surrogate pair is never split.
func TestSplitMessageSurrogates(t *testing.T) {
	parts := channel.SplitMessage("ab😀cd", 3)
	if strings.Join(parts, "") != "ab😀cd" {
		t.Fatalf("parts %q lost text", parts)
	}
	for _, p := range parts {
		if n := len(utf16.Encode([]rune(p))); n > 3 {
			t.Errorf("part %q is %d units", p, n)
		}
	}
}

func TestParseCommand(t *testing.T) {
	if c, ok := channel.ParseCommand("/NEW"); !ok || c != (channel.Command{Name: "new"}) {
		t.Errorf("/NEW = %+v %v", c, ok)
	}
	if c, ok := channel.ParseCommand("/model@my_bot  gpt-5 "); !ok || c != (channel.Command{Name: "model", Args: "gpt-5", Target: "my_bot"}) {
		t.Errorf("/model@my_bot = %+v %v", c, ok)
	}
	// A-136: names may contain "-"; a path is not a command.
	if c, ok := channel.ParseCommand("/Web-Research roofs"); !ok || c != (channel.Command{Name: "web-research", Args: "roofs"}) {
		t.Errorf("/Web-Research = %+v %v", c, ok)
	}
	for _, text := range []string{"hello /new", "/", "", "/path/to/file is broken"} {
		if _, ok := channel.ParseCommand(text); ok {
			t.Errorf("%q parsed as a command", text)
		}
	}
}

func TestErrorIs(t *testing.T) {
	err := channel.NewError("nope", channel.CodeNotFound, "c1")
	if !err.Is(&channel.Error{}) || !err.Is(&channel.Error{Code: channel.CodeNotFound}) || err.Is(&channel.Error{Code: channel.CodeConnectFailed}) {
		t.Fatal("Is does not match by class and code")
	}
	if err.Retryable || !channel.NewError("x", channel.CodeConnectFailed, "").Retryable {
		t.Fatal("only connect_failed and not_running are retryable")
	}
	if err.Name() != "ChannelError" {
		t.Fatal(err.Name())
	}
}

func TestSplitMarkdownClosesAndReopensCodeBlocks(t *testing.T) {
	var lines []string
	for i := range 30 {
		lines = append(lines, fmt.Sprintf("line %d", i))
	}
	parts := channel.SplitMarkdown("Intro\n\n```ts\n"+strings.Join(lines, "\n")+"\n```\nDone", 120)
	if len(parts) < 2 {
		t.Fatalf("parts = %q", parts)
	}
	for _, part := range parts {
		if len(part) > 120 || strings.Count(part, "```")%2 != 0 {
			t.Fatalf("part %q", part)
		}
	}
	if !strings.HasPrefix(parts[1], "```ts\n") {
		t.Fatalf("second part %q", parts[1])
	}
}

func TestTurnFromStreamReadsTheReplyAfterTheLastToolCall(t *testing.T) {
	text := func(s string) *string { return &s }
	chunks := []kernel.StreamChunk{
		{Type: kernel.ChunkText, Content: text("Let me check. ")},
		{Type: kernel.ChunkToolCall, ToolCall: &kernel.ToolCall{ID: "1", Name: "x"}},
		{Type: kernel.ChunkToolResult, ToolResult: &kernel.ToolResult{ToolCallID: "1", Content: "ok"}},
		{Type: kernel.ChunkText, Content: text("All ")},
		{Type: kernel.ChunkText, Content: text("good.")},
		{Type: kernel.ChunkDone, Usage: &kernel.TokenUsage{InputTokens: 3, OutputTokens: 2}},
	}
	seen := 0
	res, err := channel.TurnFromStream(seq(chunks), func(kernel.StreamChunk) { seen++ })
	if err != nil || res.Output != "All good." || res.Usage == nil || *res.Usage != (channel.Usage{InputTokens: 3, OutputTokens: 2}) || seen != 6 {
		t.Fatalf("res = %+v, err = %v, seen = %d", res, err, seen)
	}
	_, err = channel.TurnFromStream(seq([]kernel.StreamChunk{{Type: kernel.ChunkError, Error: "Reached maximum reasoning steps."}}), nil)
	if err == nil || err.Error() != "Reached maximum reasoning steps." {
		t.Fatalf("err = %v", err)
	}
}

func seq(chunks []kernel.StreamChunk) iter.Seq2[kernel.StreamChunk, error] {
	return func(yield func(kernel.StreamChunk, error) bool) {
		for _, c := range chunks {
			if !yield(c, nil) {
				return
			}
		}
	}
}

func TestAccessAndSettingsDecodeOlderRowsWithDefaults(t *testing.T) {
	var ch channel.Channel
	if err := json.Unmarshal([]byte(`{"id":"c","access":{"dmPolicy":"open","allowFrom":["1"]}}`), &ch); err != nil {
		t.Fatal(err)
	}
	if !ch.Access.RequireMention || ch.Access.GroupPolicy != channel.GroupDisabled || ch.Settings != channel.DefaultSettings() || ch.Access.DmPolicy != channel.DmOpen {
		t.Fatalf("channel = %+v", ch)
	}
	if err := json.Unmarshal([]byte(`{"access":{"requireMention":false},"settings":{"streaming":false}}`), &ch); err != nil {
		t.Fatal(err)
	}
	if ch.Access.RequireMention || ch.Settings != (channel.Settings{Streaming: false, ToolProgress: true}) {
		t.Fatalf("channel = %+v", ch)
	}
}
