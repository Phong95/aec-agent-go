package anthropic

import (
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"unicode/utf16"

	"github.com/Phong95/aec-agent-go/components/llmhub"
	"github.com/Phong95/aec-agent-go/internal/jsonx"
	"github.com/Phong95/aec-agent-go/internal/llmtest"
	"github.com/Phong95/aec-agent-go/internal/wiretest"
	"github.com/Phong95/aec-agent-go/kernel"
)

// The goldens of this package are recorded from a reference provider over a
// loopback server (tools/wiregen/llm/anthropic.cases.mjs). Each test serves the
// recorded server side to the provider on an llmtest.Upstream and compares what the
// provider sent and produced with the recording. No test reaches
// api.anthropic.com: the clients are built from the upstream's loopback-only client.

// anthropicFixtures lists every fixture of this package, so TestAnthropicFixturesCovered
// fails when one is added to wiregen without a test.
var anthropicFixtures = []string{
	"llm-anthropic-requests.json",
	"llm-anthropic-streams.json",
	"llm-anthropic-chat.json",
	"llm-anthropic-errors.json",
	"llm-anthropic-models.json",
}

const (
	// minimalMessage and minimalStream are the replies served when a test only needs
	// the call to finish (the request is what is compared).
	minimalMessage = `{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"text","text":"ok"}],"model":"claude-test","stop_reason":"end_turn","stop_sequence":null,"usage":{"input_tokens":1,"output_tokens":1}}`
	minimalStream  = "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"type\":\"message\",\"role\":\"assistant\",\"content\":[],\"model\":\"claude-test\",\"stop_reason\":null,\"stop_sequence\":null,\"usage\":{\"input_tokens\":5,\"output_tokens\":1}}}\n\n" +
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n" +
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"ok\"}}\n\n" +
		"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n" +
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\",\"stop_sequence\":null},\"usage\":{\"output_tokens\":1}}\n\n" +
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
)

// jsonReply is a 200 reply with a JSON body.
func jsonReply(body string) llmtest.Reply {
	return llmtest.Reply{Header: map[string]string{"Content-Type": "application/json"}, Chunks: []string{body}}
}

// sseReply is a 200 reply with an event stream, served as one network write.
func sseReply(text string) llmtest.Reply {
	return llmtest.Reply{Header: map[string]string{"Content-Type": "text/event-stream"}, Chunks: []string{text}}
}

// newTestProvider builds a provider that talks to up through its loopback-only
// client. The SDK retries are off unless a test passes its own WithMaxRetries,
// which wins because later options are applied later.
func newTestProvider(t *testing.T, up *llmtest.Upstream, opts ...Option) *Provider {
	t.Helper()
	all := append([]Option{WithHTTPClient(up.Client()), WithMaxRetries(0)}, opts...)
	p, err := New(llmhub.ProviderConfig{APIKey: "k", BaseURL: up.URL}, all...)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return p
}

// drain collects a stream and fails the test on a yielded error: a provider failure
// is an in-band chunk, never the error slot.
func drain(t *testing.T, p *Provider, msgs []kernel.Message, opts llmhub.ModelOptions) []kernel.StreamChunk {
	t.Helper()
	var out []kernel.StreamChunk
	for c, err := range p.Stream(t.Context(), msgs, opts) {
		if err != nil {
			t.Fatalf("stream yielded an error: %v", err)
		}
		out = append(out, c)
	}
	return out
}

// loadFixture decodes a fixture strictly into v.
func loadFixture(t *testing.T, name string, v any) {
	t.Helper()
	if err := jsonx.UnmarshalStrict(wiretest.Load(t, name), v); err != nil {
		t.Fatalf("decode %s: %v", name, err)
	}
}

// ---------------------------------------------------------------------------
// Requests
// ---------------------------------------------------------------------------

type recordedRequest struct {
	Method  string            `json:"method"`
	Path    string            `json:"path"`
	Query   string            `json:"query"`
	Headers map[string]string `json:"headers"`
	Body    string            `json:"body"`
}

type requestEntry struct {
	Name     string              `json:"name"`
	Method   string              `json:"method"`
	Messages []kernel.Message    `json:"messages"`
	Options  llmhub.ModelOptions `json:"options"`
	Request  recordedRequest     `json:"request"`
}

type requestFixture struct {
	Entries []requestEntry `json:"entries"`
}

// divergence is a recorded fixture value the provider deliberately does not
// reproduce: the function turns the recorded text into the text the provider must
// produce. Each replacement must find its recorded text, so a golden that changes
// cannot make a divergence a silent
// no-op.
type divergence func(t *testing.T, ts string) string

// swap returns a divergence that replaces each old text (pairs of old, new) once.
func swap(pairs ...string) divergence {
	return func(t *testing.T, ts string) string {
		t.Helper()
		for i := 0; i+1 < len(pairs); i += 2 {
			if !strings.Contains(ts, pairs[i]) {
				t.Fatalf("divergence: the TS text has no %q", pairs[i])
			}
			ts = strings.Replace(ts, pairs[i], pairs[i+1], 1)
		}
		return ts
	}
}

// checkDivergencesUsed fails when a divergence was applied by no entry.
func checkDivergencesUsed[V any](t *testing.T, all map[string]V, used map[string]bool) {
	t.Helper()
	for name := range all {
		if !used[name] {
			t.Errorf("Go divergence %q is used by no entry", name)
		}
	}
}

// requestDivergences are the request bodies where the provider fixes a bug of the
// recording: every system text is kept (newline-joined, after systemPrompt) instead of
// the last one winning, an assistant message with toolResults keeps its text and
// tool calls before the results, toolResults of a tool message are sent, and an
// empty toolResults list sends nothing. The key is the entry name; it applies to
// the chat and the stream body alike.
var requestDivergences = map[string]divergence{
	"two-system-messages-the-last-wins":     swap(`"system":"Second."`, `"system":"First.\nSecond."`),
	"system-message-replaces-system-prompt": swap(`"system":"From a message."`, `"system":"From the option.\nFrom a message."`),
	"all-options":                           swap(`"system":"S"`, `"system":"P\nS"`),
	"assistant-calls-text-and-results-drops-calls-and-text": swap(
		`{"role":"user","content":"look"},`,
		`{"role":"user","content":"look"},{"role":"assistant","content":[{"type":"text","text":"some text"},{"type":"tool_use","id":"c1","name":"read_file","input":{"b":1,"a":{"z":1,"y":[1,2]},"c":1.5,"d":0,"e":null,"f":true}}]},`),
	"role-tool-with-results-is-sent-empty": swap(
		`{"role":"user","content":""}`,
		`{"role":"user","content":[{"type":"tool_result","tool_use_id":"c1","content":"boom","is_error":true},{"type":"tool_result","tool_use_id":"c2","content":"ok","is_error":false},{"type":"tool_result","tool_use_id":"c3","content":"no flag"}]}`),
	"assistant-with-empty-tool-results-array": swap(
		`{"role":"user","content":[]}`,
		`{"role":"assistant","content":[{"type":"text","text":"kept?"}]}`),
}

// TestAnthropicRequestsMatchGolden requires the request the Go provider sends to equal
// the recorded one, byte for byte in the body, for chat and for stream, except for
// the named divergences (requestDivergences).
func TestAnthropicRequestsMatchGolden(t *testing.T) {
	var f requestFixture
	loadFixture(t, "llm-anthropic-requests.json", &f)
	if len(f.Entries) == 0 {
		t.Fatal("the request fixture has no entries")
	}
	used := map[string]bool{}
	defer checkDivergencesUsed(t, requestDivergences, used)
	for _, e := range f.Entries {
		t.Run(e.Name+"/"+e.Method, func(t *testing.T) {
			reply := jsonReply(minimalMessage)
			if e.Method == "stream" {
				reply = sseReply(minimalStream)
			}
			up := llmtest.NewUpstream(t, func(llmtest.Request) llmtest.Reply { return reply })
			p := newTestProvider(t, up)
			switch e.Method {
			case "chat":
				if _, err := p.Chat(t.Context(), e.Messages, e.Options); err != nil {
					t.Fatalf("Chat: %v", err)
				}
			case "stream":
				drain(t, p, e.Messages, e.Options)
			default:
				t.Fatalf("unknown method %q", e.Method)
			}
			reqs := up.Requests()
			if len(reqs) != 1 {
				t.Fatalf("the upstream saw %d requests, want 1", len(reqs))
			}
			got := reqs[0]
			if got.Method != e.Request.Method || got.Path != e.Request.Path || got.RawQuery != e.Request.Query {
				t.Errorf("request line: got %s %s?%s, want %s %s?%s",
					got.Method, got.Path, got.RawQuery, e.Request.Method, e.Request.Path, e.Request.Query)
			}
			for name, want := range e.Request.Headers {
				if have := got.Header.Get(name); have != want {
					t.Errorf("header %s: got %q, want %q", name, have, want)
				}
			}
			want := e.Request.Body
			if d, ok := requestDivergences[e.Name]; ok {
				used[e.Name] = true
				want = d(t, want)
			}
			if string(got.Body) != want {
				t.Errorf("body differs\n got: %s\nwant: %s", got.Body, want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Streams
// ---------------------------------------------------------------------------

type streamEntry struct {
	Name   string            `json:"name"`
	SSE    string            `json:"sse"`
	Chunks []json.RawMessage `json:"chunks"`
}

type streamFixture struct {
	Entries []streamEntry `json:"entries"`
}

const notValidJSONPrefix = "the arguments are not valid JSON ("

// tsPreview is the preview rule of the arguments error, written independently of
// the code under test: the first 200 UTF-16 units of raw, then an ellipsis when it
// is longer. No entry cuts inside a surrogate pair.
func tsPreview(raw string) string {
	units := utf16.Encode([]rune(raw))
	if len(units) <= 200 {
		return raw
	}
	return string(utf16.Decode(units[:200])) + "\u2026"
}

// withoutJSONReasons applies the documented exception of the arguments error: the
// "not valid JSON (<reason>)" text has a reason that is the JSON parser's own
// (encoding/json's here, another parser's in the recording), so the reason is
// replaced by a marker in both chunk lists after checking that the text has the
// expected prefix and ends with "): " plus the preview of the arguments text
// that was streamed. Everything else must match byte for byte.
func withoutJSONReasons(t *testing.T, got, want []string) ([]string, []string) {
	t.Helper()
	raws := map[string]string{} // tool call id -> the arguments text streamed so far
	for _, text := range want {
		var c kernel.StreamChunk
		if err := json.Unmarshal([]byte(text), &c); err == nil && c.Type == kernel.ChunkToolCallDelta {
			raws[c.ToolCallProgress.ID] += c.ToolCallProgress.ArgumentsDelta
		}
	}
	normalize := func(texts []string) []string {
		out := make([]string, len(texts))
		for i, text := range texts {
			out[i] = text
			var c kernel.StreamChunk
			if err := json.Unmarshal([]byte(text), &c); err != nil || c.ToolCall == nil || !strings.HasPrefix(c.ToolCall.ArgumentsError, notValidJSONPrefix) {
				continue
			}
			suffix := "): " + tsPreview(raws[c.ToolCall.ID])
			if !strings.HasSuffix(c.ToolCall.ArgumentsError, suffix) {
				continue
			}
			c.ToolCall.ArgumentsError = notValidJSONPrefix + "<reason>" + suffix
			b, err := kernel.EncodeJSON(c)
			if err != nil {
				t.Fatalf("encode the normalised chunk: %v", err)
			}
			out[i] = string(b)
		}
		return out
	}
	return normalize(got), normalize(want)
}

// streamDivergences are the transcripts where the provider fixes a bug of the
// recording: a stream that ends before message_stop (a cut connection) ends with one
// terminal error chunk instead of ending quietly with no done and no error. The
// function adds the provider's extra chunks to the recorded ones.
var streamDivergences = map[string]func(ts []string) []string{
	"no-message-stop-ends-without-done": appendTruncated,
	"truncated-mid-event":               appendTruncated,
}

// appendTruncated adds the terminal error chunk of a stream cut before message_stop.
func appendTruncated(ts []string) []string {
	return append(slices.Clone(ts), `{"type":"error","error":"`+errTruncated+`"}`)
}

// TestAnthropicStreamsMatchGolden serves each recorded transcript to the Go provider
// and requires the chunks to equal the recorded chunks, one by one, except for the
// named divergences (streamDivergences).
func TestAnthropicStreamsMatchGolden(t *testing.T) {
	var f streamFixture
	loadFixture(t, "llm-anthropic-streams.json", &f)
	if len(f.Entries) == 0 {
		t.Fatal("the stream fixture has no entries")
	}
	used := map[string]bool{}
	defer checkDivergencesUsed(t, streamDivergences, used)
	for _, e := range f.Entries {
		t.Run(e.Name, func(t *testing.T) {
			up := llmtest.NewUpstream(t, func(llmtest.Request) llmtest.Reply { return sseReply(e.SSE) })
			p := newTestProvider(t, up)
			got := llmtest.ChunkTexts(t, drain(t, p, []kernel.Message{{Role: kernel.RoleUser, Content: "write the report"}}, llmhub.ModelOptions{}))
			want := make([]string, len(e.Chunks))
			for i, c := range e.Chunks {
				want[i] = string(c)
			}
			if d, ok := streamDivergences[e.Name]; ok {
				used[e.Name] = true
				want = d(want)
			}
			got, want = withoutJSONReasons(t, got, want)
			if !slices.Equal(got, want) {
				t.Errorf("chunks differ\n got: %s\nwant: %s", strings.Join(got, "\n      "), strings.Join(want, "\n      "))
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Chat responses
// ---------------------------------------------------------------------------

type chatEntry struct {
	Name        string          `json:"name"`
	Body        string          `json:"body"`
	Result      json.RawMessage `json:"result"`
	GoException string          `json:"goException"`
}

type chatFixture struct {
	Entries []chatEntry `json:"entries"`
}

// documentedExceptions names the recorded entries the provider differs from on
// purpose, and turns the recorded text into what it must produce instead. Every key must
// be used by an entry: a stale exception fails the test that applies it.
var documentedExceptions = map[string]func(tsText string) string{
	// A JSON null cache count: the recording passes the null through
	// ("cacheReadTokens":null), the usage holds pointers and leaves the field out.
	"usage-null-omitted": func(ts string) string {
		return strings.ReplaceAll(ts, `,"cacheReadTokens":null,"cacheWriteTokens":null`, "")
	},
}

// chatDivergences are the recorded answers where the provider fixes a bug of the
// recording: a normal stop_sequence end, a pause_turn and a null stop reason are
// end_turn, where the recording turns every reason it does not know into error.
var chatDivergences = map[string]divergence{
	"stop-reason-stop_sequence": swap(`"stopReason":"error"`, `"stopReason":"end_turn"`),
	"stop-reason-pause_turn":    swap(`"stopReason":"error"`, `"stopReason":"end_turn"`),
	"stop-reason-null":          swap(`"stopReason":"error"`, `"stopReason":"end_turn"`),
}

// TestAnthropicChatMatchesGolden serves each recorded JSON answer to Chat and requires
// the ModelResponse to encode to the recorded text, except for the named
// divergences (chatDivergences).
func TestAnthropicChatMatchesGolden(t *testing.T) {
	var f chatFixture
	loadFixture(t, "llm-anthropic-chat.json", &f)
	if len(f.Entries) == 0 {
		t.Fatal("the chat fixture has no entries")
	}
	used := map[string]bool{}
	divergent := map[string]bool{}
	for _, e := range f.Entries {
		t.Run(e.Name, func(t *testing.T) {
			up := llmtest.NewUpstream(t, func(llmtest.Request) llmtest.Reply { return jsonReply(e.Body) })
			p := newTestProvider(t, up)
			resp, err := p.Chat(t.Context(), []kernel.Message{{Role: kernel.RoleUser, Content: "hello"}}, llmhub.ModelOptions{})
			if err != nil {
				t.Fatalf("Chat: %v", err)
			}
			got, err := kernel.EncodeJSON(resp)
			if err != nil {
				t.Fatalf("encode the response: %v", err)
			}
			want := string(e.Result)
			if e.GoException != "" {
				fn, ok := documentedExceptions[e.GoException]
				if !ok {
					t.Fatalf("entry names the unknown exception %q", e.GoException)
				}
				used[e.GoException] = true
				want = fn(want)
			}
			if d, ok := chatDivergences[e.Name]; ok {
				divergent[e.Name] = true
				want = d(t, want)
			}
			if string(got) != want {
				t.Errorf("response differs\n got: %s\nwant: %s", got, want)
			}
		})
	}
	for name := range documentedExceptions {
		if !used[name] {
			t.Errorf("documented exception %q is used by no entry", name)
		}
	}
	checkDivergencesUsed(t, chatDivergences, divergent)
}

// ---------------------------------------------------------------------------
// Errors and retries
// ---------------------------------------------------------------------------

type recordedError struct {
	Message   string `json:"message"`
	Code      string `json:"code"`
	Retryable bool   `json:"retryable"`
}

type errorEntry struct {
	Name      string            `json:"name"`
	Kind      string            `json:"kind"`
	Method    string            `json:"method"`
	Status    int               `json:"status"`
	Body      string            `json:"body"`
	Headers   map[string]string `json:"headers"`
	Attempts  int               `json:"attempts"`
	MaxTokens int               `json:"maxTokens"`
	Requests  int               `json:"requests"`
	Chunks    []json.RawMessage `json:"chunks"`
	Error     *recordedError    `json:"error"`
}

type errorFixture struct {
	Entries []errorEntry `json:"entries"`
}

// failureOf returns the *kernel.ModelError a failed call returned.
func failureOf(t *testing.T, err error) *kernel.ModelError {
	t.Helper()
	var me *kernel.ModelError
	if !errors.As(err, &me) {
		t.Fatalf("the call returned %v (%T), want a *kernel.ModelError", err, err)
	}
	return me
}

// errorDivergences are the recorded Chat errors where the provider maps differently
// (error mapping review): a 403 is an auth failure, not a provider one, and a
// refused connection is retryable. The function turns the recorded error into the
// expected one;
// the text is unchanged. The key is "<entry name>/chat".
var errorDivergences = map[string]func(recordedError) recordedError{
	"status-403-json-body/chat": func(e recordedError) recordedError { e.Code = "auth"; return e },
	"connection-refused/chat":   func(e recordedError) recordedError { e.Retryable = true; return e },
}

// checkFailure compares what a call produced with the recording: the message,
// code and retryability of a Chat error, or the error chunk text of a stream.
// It reports whether a divergence (errorDivergences) was applied.
func checkFailure(t *testing.T, p *Provider, e errorEntry) bool {
	t.Helper()
	msgs := []kernel.Message{{Role: kernel.RoleUser, Content: "hello"}}
	if e.Method == "chat" {
		_, err := p.Chat(t.Context(), msgs, llmhub.ModelOptions{})
		me := failureOf(t, err)
		got := recordedError{Message: me.Message, Code: string(me.Code), Retryable: me.Retryable}
		want := *e.Error
		d, divergent := errorDivergences[e.Name+"/"+e.Method]
		if divergent {
			want = d(want)
		}
		if got != want {
			t.Errorf("error differs\n got: %+v\nwant: %+v", got, want)
		}
		return divergent
	}
	got := llmtest.ChunkTexts(t, drain(t, p, msgs, llmhub.ModelOptions{}))
	want := make([]string, len(e.Chunks))
	for i, c := range e.Chunks {
		want[i] = string(c)
	}
	if !slices.Equal(got, want) {
		t.Errorf("chunks differ\n got: %v\nwant: %v", got, want)
	}
	return false
}

// TestAnthropicErrorsMatchGolden requires every recorded error to come out of the
// provider with the same code, retryability and text, for Chat
// and as one in-band error chunk for Stream: the status table, the response bodies
// of every shape, a refused connection and the non-streaming guard.
func TestAnthropicErrorsMatchGolden(t *testing.T) {
	var f errorFixture
	loadFixture(t, "llm-anthropic-errors.json", &f)
	seen := map[string]int{}
	used := map[string]bool{}
	defer checkDivergencesUsed(t, errorDivergences, used)
	for _, e := range f.Entries {
		switch e.Kind {
		case "status":
			seen[e.Kind]++
			t.Run(e.Name+"/"+e.Method, func(t *testing.T) {
				up := llmtest.NewUpstream(t, func(llmtest.Request) llmtest.Reply {
					return llmtest.Reply{Status: e.Status, Header: e.Headers, Chunks: []string{e.Body}}
				})
				used[e.Name+"/"+e.Method] = checkFailure(t, newTestProvider(t, up), e)
				if n := len(up.Requests()); n != 1 {
					t.Errorf("the upstream saw %d requests, want 1", n)
				}
			})
		case "connection":
			seen[e.Kind]++
			t.Run(e.Name+"/"+e.Method, func(t *testing.T) {
				up := llmtest.NewUpstream(t, func(llmtest.Request) llmtest.Reply { return jsonReply(minimalMessage) })
				p, err := New(llmhub.ProviderConfig{APIKey: "k", BaseURL: llmtest.ClosedURL(t)}, WithHTTPClient(up.Client()), WithMaxRetries(0))
				if err != nil {
					t.Fatalf("New: %v", err)
				}
				used[e.Name+"/"+e.Method] = checkFailure(t, p, e)
			})
		case "guard":
			seen[e.Kind]++
			t.Run(e.Name, func(t *testing.T) {
				up := llmtest.NewUpstream(t, func(llmtest.Request) llmtest.Reply { return jsonReply(minimalMessage) })
				p := newTestProvider(t, up)
				maxTokens := e.MaxTokens
				_, err := p.Chat(t.Context(), []kernel.Message{{Role: kernel.RoleUser, Content: "hello"}}, llmhub.ModelOptions{MaxTokens: &maxTokens})
				me := failureOf(t, err)
				// The code and the retryability are the recorded ones. The text is the Go SDK's own
				// (documented exception: Provider error text), so only its subject is
				// checked.
				if string(me.Code) != e.Error.Code || me.Retryable != e.Error.Retryable {
					t.Errorf("guard error: got code %q retryable %v, want %q %v", me.Code, me.Retryable, e.Error.Code, e.Error.Retryable)
				}
				if !strings.Contains(strings.ToLower(me.Message), "streaming is required") {
					t.Errorf("guard error text %q does not say that streaming is required", me.Message)
				}
				if n := len(up.Requests()); n != e.Requests {
					t.Errorf("the upstream saw %d requests, TS sent %d", n, e.Requests)
				}
			})
		}
	}
	for _, kind := range []string{"status", "connection", "guard"} {
		if seen[kind] == 0 {
			t.Errorf("the error fixture has no %q entries", kind)
		}
	}
}

// TestAnthropicRetriesMatchGolden counts the requests the default client sends for each
// status, with a 1 ms retry-after so the test does not wait: the SDK defaults
// (CONTEXT A3) must give the recorded attempts, one for 400, 401, 403 and 404 and
// three for 408, 409, 429 and every 5xx.
func TestAnthropicRetriesMatchGolden(t *testing.T) {
	var f errorFixture
	loadFixture(t, "llm-anthropic-errors.json", &f)
	n := 0
	for _, e := range f.Entries {
		if e.Kind != "retry" {
			continue
		}
		n++
		t.Run(e.Name+"/"+e.Method, func(t *testing.T) {
			up := llmtest.NewUpstream(t, func(llmtest.Request) llmtest.Reply {
				return llmtest.Reply{Status: e.Status, Header: e.Headers, Chunks: []string{e.Body}}
			})
			// No WithMaxRetries: the SDK default applies.
			p, err := New(llmhub.ProviderConfig{APIKey: "k", BaseURL: up.URL}, WithHTTPClient(up.Client()))
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			msgs := []kernel.Message{{Role: kernel.RoleUser, Content: "hello"}}
			if e.Method == "chat" {
				if _, err := p.Chat(t.Context(), msgs, llmhub.ModelOptions{}); err == nil {
					t.Fatal("Chat succeeded against an error status")
				}
			} else {
				chunks := drain(t, p, msgs, llmhub.ModelOptions{})
				if len(chunks) != 1 || chunks[0].Type != kernel.ChunkError {
					t.Fatalf("stream gave %+v, want one error chunk", chunks)
				}
			}
			if got := len(up.Requests()); got != e.Attempts {
				t.Errorf("status %d %s: %d requests, TS sent %d", e.Status, e.Method, got, e.Attempts)
			}
		})
	}
	if n == 0 {
		t.Fatal("the error fixture has no retry entries")
	}
}

// ---------------------------------------------------------------------------
// ListModels
// ---------------------------------------------------------------------------

type modelsEntry struct {
	Name     string            `json:"name"`
	Status   int               `json:"status"`
	Replies  []string          `json:"replies"`
	Requests []recordedRequest `json:"requests"`
	Result   json.RawMessage   `json:"result"`
	Error    *recordedError    `json:"error"`
}

type modelsFixture struct {
	Entries []modelsEntry `json:"entries"`
}

// queryEqual reports whether two query strings hold the same parameters, whatever
// their order: the recording has limit before after_id, the Go SDK sorts them.
func queryEqual(a, b string) bool {
	qa, errA := url.ParseQuery(a)
	qb, errB := url.ParseQuery(b)
	return errA == nil && errB == nil && qa.Encode() == qb.Encode()
}

// TestAnthropicListModelsMatchesGolden serves the recorded catalog pages and requires
// the same requests (path, and the query as a set of parameters) and the same
// []llmhub.ModelInfo as the recording, and ModelError auth for a 401.
func TestAnthropicListModelsMatchesGolden(t *testing.T) {
	var f modelsFixture
	loadFixture(t, "llm-anthropic-models.json", &f)
	if len(f.Entries) == 0 {
		t.Fatal("the models fixture has no entries")
	}
	for _, e := range f.Entries {
		t.Run(e.Name, func(t *testing.T) {
			var mu sync.Mutex
			next := 0
			up := llmtest.NewUpstream(t, func(llmtest.Request) llmtest.Reply {
				mu.Lock()
				defer mu.Unlock()
				body := e.Replies[min(next, len(e.Replies)-1)]
				next++
				rep := jsonReply(body)
				if e.Status != 0 {
					rep.Status = e.Status
				}
				return rep
			})
			p := newTestProvider(t, up)
			models, err := p.ListModels(t.Context())
			reqs := up.Requests()
			if len(reqs) != len(e.Requests) {
				t.Fatalf("the upstream saw %d requests, TS sent %d", len(reqs), len(e.Requests))
			}
			for i, want := range e.Requests {
				got := reqs[i]
				if got.Method != want.Method || got.Path != want.Path || !queryEqual(got.RawQuery, want.Query) {
					t.Errorf("request %d: got %s %s?%s, want %s %s?%s", i, got.Method, got.Path, got.RawQuery, want.Method, want.Path, want.Query)
				}
				for name, v := range want.Headers {
					if have := got.Header.Get(name); have != v {
						t.Errorf("request %d header %s: got %q, want %q", i, name, have, v)
					}
				}
			}
			if e.Error != nil {
				me := failureOf(t, err)
				got := recordedError{Message: me.Message, Code: string(me.Code), Retryable: me.Retryable}
				if got != *e.Error {
					t.Errorf("error differs\n got: %+v\nwant: %+v", got, *e.Error)
				}
				return
			}
			if err != nil {
				t.Fatalf("ListModels: %v", err)
			}
			got, err := jsonx.Marshal(models)
			if err != nil {
				t.Fatalf("encode the models: %v", err)
			}
			if string(got) != string(e.Result) {
				t.Errorf("models differ\n got: %s\nwant: %s", got, e.Result)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Coverage
// ---------------------------------------------------------------------------

// TestAnthropicFixturesCovered fails when a fixture with the llm-anthropic- prefix
// has no test above, and when a listed fixture has no file.
func TestAnthropicFixturesCovered(t *testing.T) {
	entries, err := os.ReadDir(wiretest.Dir(t))
	if err != nil {
		t.Fatalf("read fixture dir: %v", err)
	}
	present := map[string]bool{}
	for _, e := range entries {
		present[e.Name()] = true
		if strings.HasPrefix(e.Name(), "llm-anthropic-") && !slices.Contains(anthropicFixtures, e.Name()) {
			t.Errorf("fixture %s has no entry in anthropicFixtures", e.Name())
		}
	}
	for _, name := range anthropicFixtures {
		if !present[name] {
			t.Errorf("anthropicFixtures entry %s has no fixture file", name)
		}
	}
}
