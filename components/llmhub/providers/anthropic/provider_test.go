package anthropic

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/Phong95/aec-agent-go/components/llmhub"
	"github.com/Phong95/aec-agent-go/internal/jsonx"
	"github.com/Phong95/aec-agent-go/internal/llmtest"
	"github.com/Phong95/aec-agent-go/internal/llmtransport"
	"github.com/Phong95/aec-agent-go/kernel"
)

// secret is a credential placed in the base URL, in the query and in a path
// segment (a tenant or token a gateway URL can carry). ProviderConfig treats the
// query of BaseURL as secret, and an SDK error string embeds the request URL, so
// the secret must never reach an error text. The SDK drops the base URL query from
// the requests it builds, so it is the path segment that proves the check is sharp.
const secret = "SECRET-ANTH"

// secretBase returns base with the secret in its path and its query.
func secretBase(base string) string {
	return base + "/tenant-" + secret + "/?key=" + secret
}

// leaks reports what in text must not be there: the secret, the loopback address
// the test upstream listens on, and the "POST \"<url>\"" form of an SDK error.
func leaks(text string) []string {
	var found []string
	for _, bad := range []string{secret, "127.0.0.1", `POST "`, "localhost"} {
		if strings.Contains(text, bad) {
			found = append(found, bad)
		}
	}
	return found
}

// TestAnthropicErrorTextNeverCarriesTheBaseURL puts a secret in the path and the
// query of the base URL and requires that no error text, for any status or for a refused
// connection, through Chat, Stream or ListModels, contains it, the address of the
// upstream or the SDK's request line (T-08-09).
func TestAnthropicErrorTextNeverCarriesTheBaseURL(t *testing.T) {
	msgs := []kernel.Message{{Role: kernel.RoleUser, Content: "hello"}}
	check := func(t *testing.T, what, text string) {
		t.Helper()
		if text == "" {
			t.Errorf("%s: the error text is empty", what)
		}
		if found := leaks(text); len(found) > 0 {
			t.Errorf("%s: the error text %q contains %q", what, text, found)
		}
	}

	for _, status := range []int{400, 401, 403, 404, 408, 429, 500, 503, 529} {
		t.Run(fmt.Sprintf("status %d", status), func(t *testing.T) {
			up := llmtest.NewUpstream(t, func(llmtest.Request) llmtest.Reply {
				return llmtest.Reply{Status: status, Header: map[string]string{"Content-Type": "application/json", "Retry-After": "7"}, Chunks: []string{`{"type":"error","error":{"type":"x_error","message":"boom"}}`}}
			})
			p, err := New(llmhub.ProviderConfig{APIKey: "k", BaseURL: secretBase(up.URL)}, WithHTTPClient(up.Client()), WithMaxRetries(0))
			if err != nil {
				t.Fatalf("New: %v", err)
			}

			_, err = p.Chat(t.Context(), msgs, llmhub.ModelOptions{})
			me := failureOf(t, err)
			check(t, "Chat", me.Message)
			// The status and the Retry-After of the response are kept on the error.
			if me.StatusCode != status || me.RetryAfter != 7*time.Second {
				t.Errorf("Chat StatusCode %d RetryAfter %v, want %d 7s", me.StatusCode, me.RetryAfter, status)
			}

			chunks := drain(t, p, msgs, llmhub.ModelOptions{})
			if len(chunks) != 1 || chunks[0].Type != kernel.ChunkError {
				t.Fatalf("stream gave %+v, want one error chunk", chunks)
			}
			check(t, "Stream", chunks[0].Error)

			_, err = p.ListModels(t.Context())
			me = failureOf(t, err)
			check(t, "ListModels", me.Message)
			if me.StatusCode != status {
				t.Errorf("ListModels StatusCode %d, want %d", me.StatusCode, status)
			}

			// The check is only sharp when the SDK really put the secret in the URL it
			// requested, which is what its own Error() string would then print.
			reqs := up.Requests()
			if len(reqs) == 0 || !strings.Contains(reqs[0].Path, secret) {
				t.Fatalf("the secret did not reach the upstream path, so the check proves nothing: %+v", reqs)
			}
		})
	}

	t.Run("refused connection", func(t *testing.T) {
		up := llmtest.NewUpstream(t, func(llmtest.Request) llmtest.Reply { return llmtest.Reply{} })
		p, err := New(llmhub.ProviderConfig{APIKey: "k", BaseURL: secretBase(llmtest.ClosedURL(t))}, WithHTTPClient(up.Client()), WithMaxRetries(0))
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		_, err = p.Chat(t.Context(), msgs, llmhub.ModelOptions{})
		me := failureOf(t, err)
		check(t, "Chat", me.Message)
		if me.Message != "Connection error." {
			t.Errorf("Chat text = %q, want %q", me.Message, "Connection error.")
		}
		// The transport error is the cause: reachable, but not in the text.
		var urlErr *url.Error
		if !errors.As(err, &urlErr) || me.StatusCode != 0 {
			t.Errorf("Chat error: errors.As *url.Error = %v, StatusCode %d; want the cause and no status", urlErr != nil, me.StatusCode)
		}
		chunks := drain(t, p, msgs, llmhub.ModelOptions{})
		if len(chunks) != 1 {
			t.Fatalf("stream gave %+v, want one error chunk", chunks)
		}
		check(t, "Stream", chunks[0].Error)
		_, err = p.ListModels(t.Context())
		check(t, "ListModels", failureOf(t, err).Message)
	})
}

// TestAnthropicIgnoresEnvironment sets the three variables the vendor SDK reads and
// requires that they change nothing the upstream sees (T-08-10, CONTEXT A3): the
// explicit key is sent, no authorization header is added, and a trap upstream named
// by ANTHROPIC_BASE_URL is never called, neither when the base URL is explicit nor
// when it is left empty (then the SDK default host is the target, which the
// loopback-only test client refuses before dialing).
func TestAnthropicIgnoresEnvironment(t *testing.T) {
	trap := llmtest.NewUpstream(t, func(r llmtest.Request) llmtest.Reply {
		t.Errorf("the environment base URL was used: %s %s", r.Method, r.Path)
		return llmtest.Reply{Status: 500}
	})
	t.Setenv("ANTHROPIC_API_KEY", "ENV-KEY-LEAK")
	t.Setenv("ANTHROPIC_BASE_URL", trap.URL)
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "ENV-TOKEN-LEAK")

	msgs := []kernel.Message{{Role: kernel.RoleUser, Content: "hello"}}

	t.Run("explicit base URL", func(t *testing.T) {
		up := llmtest.NewUpstream(t, func(llmtest.Request) llmtest.Reply { return jsonReply(minimalMessage) })
		p, err := New(llmhub.ProviderConfig{APIKey: "explicit-key", BaseURL: up.URL}, WithHTTPClient(up.Client()), WithMaxRetries(0))
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		if _, err := p.Chat(t.Context(), msgs, llmhub.ModelOptions{}); err != nil {
			t.Fatalf("Chat: %v", err)
		}
		reqs := up.Requests()
		if len(reqs) != 1 {
			t.Fatalf("the upstream saw %d requests, want 1", len(reqs))
		}
		if got := reqs[0].Header.Get("X-Api-Key"); got != "explicit-key" {
			t.Errorf("x-api-key = %q, want the explicit key", got)
		}
		if got := reqs[0].Header.Get("Authorization"); got != "" {
			t.Errorf("an authorization header %q was sent from ANTHROPIC_AUTH_TOKEN", got)
		}
	})

	t.Run("empty base URL does not read ANTHROPIC_BASE_URL", func(t *testing.T) {
		// The upstream only lends its loopback-only client: the SDK default host is
		// api.anthropic.com, and that client refuses it before any dial or lookup.
		up := llmtest.NewUpstream(t, func(llmtest.Request) llmtest.Reply { return jsonReply(minimalMessage) })
		p, err := New(llmhub.ProviderConfig{APIKey: "explicit-key"}, WithHTTPClient(up.Client()), WithMaxRetries(0))
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		_, err = p.Chat(t.Context(), msgs, llmhub.ModelOptions{})
		if me := failureOf(t, err); me.Message != "Connection error." {
			t.Errorf("Chat text = %q, want the refused default host", me.Message)
		}
		if n := len(up.Requests()); n != 0 {
			t.Errorf("the explicit upstream saw %d requests, want 0", n)
		}
	})

	if n := len(trap.Requests()); n != 0 {
		t.Errorf("the trap upstream saw %d requests", n)
	}
}

// TestProvidersKeepTheResponseAliveOnInvalidToolArguments is the Anthropic case of
// "providers keep the response alive on invalid tool arguments": a tool call whose streamed arguments are not valid
// JSON must not fail the stream; it becomes a call with empty arguments and an
// ArgumentsError the harness can answer.
func TestProvidersKeepTheResponseAliveOnInvalidToolArguments(t *testing.T) {
	t.Run("Anthropic", func(t *testing.T) {
		const badJSON = `{"path": "notes/report.md", "content": "He said "hi""}`
		transcript := "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":5}}}\n\n" +
			"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"tool_use\",\"id\":\"toolu_1\",\"name\":\"write_file\",\"input\":{}}}\n\n" +
			"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":" + jsonString(badJSON) + "}}\n\n" +
			"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n" +
			"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"tool_use\"},\"usage\":{\"output_tokens\":40}}\n\n" +
			"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
		up := llmtest.NewUpstream(t, func(llmtest.Request) llmtest.Reply { return sseReply(transcript) })
		p := newTestProvider(t, up)

		chunks := drain(t, p, []kernel.Message{{Role: kernel.RoleUser, Content: "write the report"}}, llmhub.ModelOptions{})

		var call *kernel.ToolCall
		for _, c := range chunks {
			if c.Type == kernel.ChunkError {
				t.Fatalf("the stream failed: %s", c.Error)
			}
			if c.Type == kernel.ChunkToolCall {
				call = c.ToolCall
			}
		}
		if len(chunks) == 0 || chunks[len(chunks)-1].Type != kernel.ChunkDone {
			t.Fatalf("the stream did not end with done: %+v", chunks)
		}
		if call == nil {
			t.Fatal("no tool_call chunk")
		}
		if call.ID != "toolu_1" || call.Name != "write_file" {
			t.Errorf("call = %s %s, want toolu_1 write_file", call.ID, call.Name)
		}
		if len(call.Arguments) != 0 {
			t.Errorf("arguments = %s, want empty", call.Arguments)
		}
		if !strings.Contains(call.ArgumentsError, "not valid JSON") || !strings.Contains(call.ArgumentsError, `"path": "notes/report.md"`) {
			t.Errorf("ArgumentsError = %q", call.ArgumentsError)
		}
	})
}

// jsonString quotes s as a JSON string.
func jsonString(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"', '\\':
			b.WriteByte('\\')
			b.WriteRune(r)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}

// TestAnthropicIsRegisteredAndNamed checks what a consumer sees after importing the
// package: the factory is registered as "anthropic", the provider reports its name
// and version, and the lifecycle hooks and the token estimate work.
func TestAnthropicIsRegisteredAndNamed(t *testing.T) {
	p, err := llmhub.ResolveProvider("anthropic", llmhub.ProviderConfig{APIKey: "k"})
	if err != nil {
		t.Fatalf("ResolveProvider: %v", err)
	}
	if _, ok := p.(*Provider); !ok {
		t.Fatalf("ResolveProvider returned %T, want *anthropic.Provider", p)
	}
	if _, ok := p.(llmhub.ModelLister); !ok {
		t.Error("the provider is not an llmhub.ModelLister")
	}
	if p.Name() != "anthropic" || p.Version() != "1.0.0" {
		t.Errorf("name %q version %q, want anthropic 1.0.0", p.Name(), p.Version())
	}
	if err := kernel.Initialize(t.Context(), p); err != nil {
		t.Errorf("Initialize: %v", err)
	}
	if err := kernel.Dispose(t.Context(), p); err != nil {
		t.Errorf("Dispose: %v", err)
	}
	msgs := []kernel.Message{{Role: kernel.RoleUser, Content: "abcde"}}
	n, err := p.CountTokens(t.Context(), msgs)
	if err != nil || n != llmhub.EstimateTokens(msgs) || n != 2 {
		t.Errorf("CountTokens = %d, %v; want 2, nil", n, err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := p.CountTokens(ctx, msgs); !errors.Is(err, context.Canceled) {
		t.Errorf("CountTokens on a cancelled context returned %v, want context.Canceled", err)
	}
}

// TestAnthropicDefaultsAndOverrides checks the model fallbacks: the provider default
// model, cfg.Model, and ModelOptions.Model per call, and the 4096 max_tokens default
// (16384, llmhub.DefaultMaxOutputTokens, since A-88).
func TestAnthropicDefaultsAndOverrides(t *testing.T) {
	model := func(t *testing.T, cfg llmhub.ProviderConfig, opts llmhub.ModelOptions) string {
		t.Helper()
		up := llmtest.NewUpstream(t, func(llmtest.Request) llmtest.Reply { return jsonReply(minimalMessage) })
		cfg.BaseURL = up.URL
		p, err := New(cfg, WithHTTPClient(up.Client()), WithMaxRetries(0))
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		if _, err := p.Chat(t.Context(), []kernel.Message{{Role: kernel.RoleUser, Content: "hi"}}, opts); err != nil {
			t.Fatalf("Chat: %v", err)
		}
		body := string(up.Requests()[0].Body)
		if !strings.Contains(body, `"max_tokens":16384`) && opts.MaxTokens == nil {
			t.Errorf("the default max_tokens is missing from %s", body)
		}
		return body
	}
	if body := model(t, llmhub.ProviderConfig{APIKey: "k"}, llmhub.ModelOptions{}); !strings.HasPrefix(body, `{"model":"claude-sonnet-4-20250514",`) {
		t.Errorf("default model: %s", body)
	}
	if body := model(t, llmhub.ProviderConfig{APIKey: "k", Model: "from-config"}, llmhub.ModelOptions{}); !strings.HasPrefix(body, `{"model":"from-config",`) {
		t.Errorf("cfg.Model: %s", body)
	}
	if body := model(t, llmhub.ProviderConfig{APIKey: "k", Model: "from-config"}, llmhub.ModelOptions{Model: "from-call"}); !strings.HasPrefix(body, `{"model":"from-call",`) {
		t.Errorf("ModelOptions.Model: %s", body)
	}
}

// TestMapErrorFallbacks covers the errors the goldens cannot produce on a loopback
// server: a request that timed out, transport errors of every shape (whose own text
// carries a URL or an address), and an error the SDK made up itself. A timeout and
// a network failure are retryable, and a long
// text is cut at llmtransport.MaxErrorMessage (error mapping review).
func TestMapErrorFallbacks(t *testing.T) {
	long := strings.Repeat("x", 3*llmtransport.MaxErrorMessage)
	cases := []struct {
		name      string
		err       error
		message   string
		code      kernel.ModelErrorCode
		retryable bool
	}{
		{"deadline", context.DeadlineExceeded, "Request timed out.", kernel.ModelCodeTimeout, true},
		{"deadline inside a url error", &url.Error{Op: "Post", URL: "https://x.example/?key=" + secret, Err: context.DeadlineExceeded}, "Request timed out.", kernel.ModelCodeTimeout, true},
		{"url error", &url.Error{Op: "Post", URL: "https://x.example/?key=" + secret, Err: errors.New("boom")}, "Connection error.", kernel.ModelCodeProvider, true},
		{"net op error", &net.OpError{Op: "read", Net: "tcp", Err: errors.New("connection reset by peer")}, "Connection error.", kernel.ModelCodeProvider, true},
		{"a body cut short", fmt.Errorf("read body: %w", io.ErrUnexpectedEOF), "Connection error.", kernel.ModelCodeProvider, true},
		{"anything else keeps its text", errors.New("something else"), "something else", kernel.ModelCodeProvider, false},
		{"a long text is cut", errors.New(long), llmtransport.TruncateMessage(long), kernel.ModelCodeProvider, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			me := mapError(c.err)
			if me.Message != c.message || me.Code != c.code || me.Retryable != c.retryable {
				t.Errorf("mapError = %q %q retryable %v, want %q %q retryable %v", me.Message, me.Code, me.Retryable, c.message, c.code, c.retryable)
			}
			if found := leaks(me.Message); len(found) > 0 {
				t.Errorf("the text %q leaks %q", me.Message, found)
			}
		})
	}
}

// TestAnthropicStopReasons pins the stop-reason table: only refusal is an error, the token limits are max_tokens, and every
// normal or unknown end is end_turn.
func TestAnthropicStopReasons(t *testing.T) {
	cases := map[string]kernel.StopReason{
		"end_turn":                      kernel.StopEndTurn,
		"stop_sequence":                 kernel.StopEndTurn,
		"pause_turn":                    kernel.StopEndTurn,
		"":                              kernel.StopEndTurn,
		"a_reason_added_later":          kernel.StopEndTurn,
		"tool_use":                      kernel.StopToolUse,
		"max_tokens":                    kernel.StopMaxTokens,
		"model_context_window_exceeded": kernel.StopMaxTokens,
		"refusal":                       kernel.StopError,
	}
	for reason, want := range cases {
		if got := mapStopReason(reason); got != want {
			t.Errorf("mapStopReason(%q) = %q, want %q", reason, got, want)
		}
	}
}

// TestAnthropicUserToolResultsKeepTheirText covers a user message
// that carries results and text: the results come first, then the text, in one user
// message; a tool message is its results only.
func TestAnthropicUserToolResultsKeepTheirText(t *testing.T) {
	msgs := []kernel.Message{
		{Role: kernel.RoleUser, Content: "here you go", ToolResults: []kernel.ToolResult{{ToolCallID: "c1", Content: "r1"}}},
		{Role: kernel.RoleTool, Content: "ignored", ToolResults: []kernel.ToolResult{{ToolCallID: "c2", Content: "r2"}}},
	}
	system, mapped := mapMessages(msgs, llmhub.ModelOptions{})
	if system != "" {
		t.Errorf("system = %q, want none", system)
	}
	got, err := jsonx.Marshal(mapped)
	if err != nil {
		t.Fatal(err)
	}
	want := `[{"role":"user","content":[{"type":"tool_result","tool_use_id":"c1","content":"r1"},{"type":"text","text":"here you go"}]},` +
		`{"role":"user","content":[{"type":"tool_result","tool_use_id":"c2","content":"r2"}]}]`
	if string(got) != want {
		t.Errorf("messages\n got: %s\nwant: %s", got, want)
	}
}

// anthropicStopStream is a text-only stream whose message_delta reports stop as its
// stop_reason (null when stop is empty).
func anthropicStopStream(stop string) string {
	reason := "null"
	if stop != "" {
		reason = `"` + stop + `"`
	}
	return "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m\",\"type\":\"message\",\"role\":\"assistant\",\"content\":[],\"model\":\"claude-test\",\"usage\":{\"input_tokens\":5,\"output_tokens\":1}}}\n\n" +
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n" +
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"hi\"}}\n\n" +
		"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n" +
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":" + reason + ",\"stop_sequence\":null},\"usage\":{\"output_tokens\":7}}\n\n" +
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
}

// TestAnthropicStreamDoneStopReason requires the done chunk to carry the stop reason of
// message_delta, mapped like Chat maps it, and none when the stream reported none
// (A-88).
func TestAnthropicStreamDoneStopReason(t *testing.T) {
	cases := map[string]kernel.StopReason{
		"end_turn":      kernel.StopEndTurn,
		"tool_use":      kernel.StopToolUse,
		"max_tokens":    kernel.StopMaxTokens,
		"stop_sequence": kernel.StopEndTurn,
		"refusal":       kernel.StopError,
		"":              "",
	}
	for reported, want := range cases {
		t.Run("reported "+reported, func(t *testing.T) {
			up := llmtest.NewUpstream(t, func(llmtest.Request) llmtest.Reply { return sseReply(anthropicStopStream(reported)) })
			chunks := drain(t, newTestProvider(t, up), []kernel.Message{{Role: kernel.RoleUser, Content: "go"}}, llmhub.ModelOptions{})
			done := chunks[len(chunks)-1]
			if done.Type != kernel.ChunkDone || done.StopReason != want {
				t.Fatalf("last chunk = %+v, want done with stop reason %q", done, want)
			}
			if done.Usage == nil || done.Usage.OutputTokens != 7 {
				t.Fatalf("done usage = %+v, want the message_delta output tokens", done.Usage)
			}
		})
	}
}

// TestAnthropicStreamHandsOverToolBlockOpenAtMessageStop: a response cut off at
// max_tokens can end without closing the tool_use block it was writing. The call is
// handed over before done, with the ArgumentsError of its partial JSON, instead of
// being dropped (A-88).
func TestAnthropicStreamHandsOverToolBlockOpenAtMessageStop(t *testing.T) {
	sse := "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m\",\"type\":\"message\",\"role\":\"assistant\",\"content\":[],\"model\":\"claude-test\",\"usage\":{\"input_tokens\":5,\"output_tokens\":1}}}\n\n" +
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"tool_use\",\"id\":\"tu_1\",\"name\":\"write_file\",\"input\":{}}}\n\n" +
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{\\\"path\\\":\\\"a.md\\\",\\\"content\\\":\\\"abc\"}}\n\n" +
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"max_tokens\",\"stop_sequence\":null},\"usage\":{\"output_tokens\":16384}}\n\n" +
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
	up := llmtest.NewUpstream(t, func(llmtest.Request) llmtest.Reply { return sseReply(sse) })
	chunks := drain(t, newTestProvider(t, up), []kernel.Message{{Role: kernel.RoleUser, Content: "go"}}, llmhub.ModelOptions{})
	types := make([]kernel.ChunkType, len(chunks))
	for i, c := range chunks {
		types[i] = c.Type
	}
	wantTypes := []kernel.ChunkType{kernel.ChunkToolCallStart, kernel.ChunkToolCallDelta, kernel.ChunkToolCall, kernel.ChunkDone}
	if fmt.Sprint(types) != fmt.Sprint(wantTypes) {
		t.Fatalf("chunk types = %v, want %v", types, wantTypes)
	}
	call := chunks[2].ToolCall
	if call == nil || call.ID != "tu_1" || call.Name != "write_file" || call.ArgumentsError == "" {
		t.Fatalf("tool call = %+v, want tu_1 write_file with an ArgumentsError", call)
	}
	if chunks[3].StopReason != kernel.StopMaxTokens {
		t.Fatalf("done stop reason = %q, want max_tokens", chunks[3].StopReason)
	}
}

// TestAnthropicDefaultMaxOutputTokens requires llmhub.DefaultMaxOutputTokens (16384)
// as max_tokens of Chat and Stream when ModelOptions.MaxTokens is nil, and the set
// value otherwise (A-88).
func TestAnthropicDefaultMaxOutputTokens(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, tc := range []struct {
			opts llmhub.ModelOptions
			want string
		}{
			{llmhub.ModelOptions{}, `"max_tokens":16384,`},
			{llmhub.ModelOptions{MaxTokens: llmhub.Int(100)}, `"max_tokens":100,`},
		} {
			up := llmtest.NewUpstream(t, func(llmtest.Request) llmtest.Reply {
				if stream {
					return sseReply(minimalStream)
				}
				return jsonReply(minimalMessage)
			})
			p := newTestProvider(t, up)
			msgs := []kernel.Message{{Role: kernel.RoleUser, Content: "hi"}}
			if stream {
				drain(t, p, msgs, tc.opts)
			} else if _, err := p.Chat(t.Context(), msgs, tc.opts); err != nil {
				t.Fatalf("Chat: %v", err)
			}
			if body := string(up.Requests()[0].Body); !strings.Contains(body, tc.want) {
				t.Errorf("stream=%v: body %s lacks %s", stream, body, tc.want)
			}
		}
	}
	if llmhub.DefaultMaxOutputTokens != 16384 {
		t.Fatalf("llmhub.DefaultMaxOutputTokens = %d, want 16384", llmhub.DefaultMaxOutputTokens)
	}
}
