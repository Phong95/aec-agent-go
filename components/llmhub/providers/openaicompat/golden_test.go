package openaicompat

import (
	"encoding/json"
	"errors"
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

// The goldens of this package are recorded from a reference provider and its SDK
// over a loopback server (tools/wiregen/llm/openai.cases.mjs). Each test serves the
// recorded server side to the provider on an llmtest.Upstream and compares what
// the provider sent and produced with the recorded values. No test reaches
// api.openai.com or api.novita.ai: the clients are built from the upstream's
// loopback-only client.

// openaiFixtures lists every fixture of this package, so TestOpenAIFixturesCovered
// fails when one is added to wiregen without a test.
var openaiFixtures = []string{
	"llm-openai-requests.json",
	"llm-openai-streams.json",
	"llm-openai-chat.json",
	"llm-openai-errors.json",
	"llm-openai-models.json",
}

const (
	// minimalCompletion and minimalStream are the replies served when a test only
	// needs the call to finish (the request is what is compared).
	minimalCompletion = `{"id":"chatcmpl-1","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`
	minimalStream     = `data: {"id":"chatcmpl-1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"role":"assistant","content":"ok"},"finish_reason":null}]}` + "\n\n" +
		`data: {"id":"chatcmpl-1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}` + "\n\n" +
		`data: {"id":"chatcmpl-1","object":"chat.completion.chunk","created":1,"model":"m","choices":[],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}` + "\n\n" +
		"data: [DONE]\n\n"
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
// which wins because later options are applied later. The base URL ends in /v1 as
// the registry default does.
func newTestProvider(t *testing.T, up *llmtest.Upstream, opts ...Option) *Provider {
	t.Helper()
	all := append([]Option{WithHTTPClient(up.Client()), WithMaxRetries(0)}, opts...)
	p, err := New(llmhub.ProviderConfig{APIKey: "k", BaseURL: up.URL + "/v1"}, all...)
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
	Name         string              `json:"name"`
	Method       string              `json:"method"`
	ProviderName string              `json:"providerName"`
	Messages     []kernel.Message    `json:"messages"`
	Options      llmhub.ModelOptions `json:"options"`
	Request      recordedRequest     `json:"request"`
}

type requestFixture struct {
	Entries []requestEntry `json:"entries"`
}

// divergence is a recorded value the provider deliberately does not reproduce: the
// function turns the recorded text into the text the provider must produce. Each
// replacement must find its recorded text, so a golden that changes cannot make a
// divergence a silent no-op.
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
// recording: an assistant message with toolResults keeps its text and tool calls
// before its tool messages, the toolResults of a tool message are sent as tool
// messages (the recording sends {role:"user",content:""}), and an assistant message
// with an empty toolResults list is sent as itself. The key is the entry name; it
// applies to the chat and the stream body alike.
var requestDivergences = map[string]divergence{
	"assistant-calls-text-and-results-drops-calls-and-text": swap(
		`{"role":"user","content":"look"},`,
		`{"role":"user","content":"look"},{"role":"assistant","content":"some text","tool_calls":[{"id":"c1","type":"function","function":{"name":"read_file","arguments":"{\"b\":1,\"a\":{\"z\":1,\"y\":[1,2]},\"c\":1.5,\"d\":0,\"e\":null,\"f\":true}"}}]},`),
	"role-tool-with-results-is-sent-empty": swap(
		`{"role":"user","content":""}`,
		`{"role":"tool","tool_call_id":"c1","content":"boom"},{"role":"tool","tool_call_id":"c2","content":"ok"},{"role":"tool","tool_call_id":"c3","content":"no flag"}`),
	"assistant-with-empty-tool-results-array-sends-nothing": swap(
		`{"role":"user","content":"look"},`,
		`{"role":"user","content":"look"},{"role":"assistant","content":"kept?"},`),
}

// TestOpenAIRequestsMatchGolden requires the request the provider sends to equal the
// recorded one, byte for byte in the body, for chat and for stream,
// except for the named divergences (requestDivergences).
func TestOpenAIRequestsMatchGolden(t *testing.T) {
	var f requestFixture
	loadFixture(t, "llm-openai-requests.json", &f)
	if len(f.Entries) == 0 {
		t.Fatal("the request fixture has no entries")
	}
	used := map[string]bool{}
	defer checkDivergencesUsed(t, requestDivergences, used)
	for _, e := range f.Entries {
		t.Run(e.Name+"/"+e.Method, func(t *testing.T) {
			reply := jsonReply(minimalCompletion)
			if e.Method == "stream" {
				reply = sseReply(minimalStream)
			}
			up := llmtest.NewUpstream(t, func(llmtest.Request) llmtest.Reply { return reply })
			p := newTestProvider(t, up, WithName(e.ProviderName))
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
	Name        string            `json:"name"`
	SSE         string            `json:"sse"`
	Chunks      []json.RawMessage `json:"chunks"`
	GoException string            `json:"goException"`
}

// streamExceptions names the recorded stream entries where the provider differs from
// the recording on purpose, with the chunks it must produce instead (written here,
// not derived from the recorded ones). Every key must be used by an entry: a stale
// exception fails the test.
var streamExceptions = map[string][]string{
	// A chunk with no choices member fails the whole recorded stream. The provider
	// skips such a chunk and goes on.
	"chunk-without-choices-is-skipped": {
		`{"type":"text","content":"a"}`,
		`{"type":"done"}`,
	},
}

// streamDivergences are the recorded transcripts where the provider fixes a bug of
// the recording, keyed by entry name, with the chunks the provider must produce
// (written here, not derived):
//   - every buffered tool call is emitted once, at the first chunk with a
//     finish_reason; the recording repeats them all at every such chunk;
//   - a finish_reason on a choice without a delta (or with a null delta) still
//     flushes the buffered calls; the recording skips the choice whole and loses
//     them;
//   - a stream that ends with neither [DONE] nor a finish_reason was cut short
//     and ends with one terminal error chunk; the recording ends it with a plain
//     done.
var streamDivergences = map[string][]string{
	"tool-calls-then-stop-repeat-every-call": {
		`{"type":"tool_call_start","toolCallProgress":{"id":"call_a","name":"read_file"}}`,
		`{"type":"tool_call_delta","toolCallProgress":{"id":"call_a","name":"read_file","argumentsDelta":"{\"p\":\"x\"}"}}`,
		`{"type":"tool_call_start","toolCallProgress":{"id":"call_b","name":"list_dir"}}`,
		`{"type":"tool_call_delta","toolCallProgress":{"id":"call_b","name":"list_dir","argumentsDelta":"{}"}}`,
		`{"type":"tool_call","toolCall":{"id":"call_a","name":"read_file","arguments":{"p":"x"}}}`,
		`{"type":"tool_call","toolCall":{"id":"call_b","name":"list_dir","arguments":{}}}`,
		`{"type":"done","usage":{"inputTokens":5,"outputTokens":6},"stopReason":"end_turn"}`,
	},
	"the-same-finish-reason-three-times": {
		`{"type":"tool_call_start","toolCallProgress":{"id":"call_a","name":"read_file"}}`,
		`{"type":"tool_call_delta","toolCallProgress":{"id":"call_a","name":"read_file","argumentsDelta":"{}"}}`,
		`{"type":"tool_call","toolCall":{"id":"call_a","name":"read_file","arguments":{}}}`,
		`{"type":"done","stopReason":"max_tokens"}`,
	},
	"choice-without-delta-is-skipped": {
		`{"type":"tool_call_start","toolCallProgress":{"id":"call_a","name":"read_file"}}`,
		`{"type":"tool_call","toolCall":{"id":"call_a","name":"read_file","arguments":{}}}`,
		`{"type":"text","content":"z"}`,
		`{"type":"done","stopReason":"tool_use"}`,
	},
	"choice-with-null-delta-is-skipped": {
		`{"type":"tool_call_start","toolCallProgress":{"id":"call_a","name":"read_file"}}`,
		`{"type":"tool_call","toolCall":{"id":"call_a","name":"read_file","arguments":{}}}`,
		`{"type":"done","stopReason":"end_turn"}`,
	},
	"empty-stream": {
		`{"type":"error","error":"` + errTruncated + `"}`,
	},
}

type streamFixture struct {
	Entries []streamEntry `json:"entries"`
}

const notValidJSONPrefix = "the arguments are not valid JSON ("

// tsPreview is the expected preview rule of the arguments error, written independently of
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
// (it differs between the recording and encoding/json), so the
// reason is replaced by a marker in both chunk lists after checking that the text
// has the expected prefix and ends with "): " plus the expected preview of the
// arguments text
// that was streamed. Everything else must match byte for byte.
//
// The streamed text of a call is rebuilt from its tool_call_delta chunks; the
// OpenAI provider repeats a call at every finish chunk, and the text is the same
// each time because the buffers are never cleared.
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

// TestOpenAIStreamsMatchGolden serves each recorded transcript to the Go provider and
// requires the chunks to equal the recorded chunks, one by one, except for the
// documented exceptions (streamExceptions) and the named divergences (streamDivergences).
func TestOpenAIStreamsMatchGolden(t *testing.T) {
	var f streamFixture
	loadFixture(t, "llm-openai-streams.json", &f)
	if len(f.Entries) < 8 {
		t.Fatalf("the stream fixture has %d entries, want at least 8", len(f.Entries))
	}
	used := map[string]bool{}
	divergent := map[string]bool{}
	for _, e := range f.Entries {
		t.Run(e.Name, func(t *testing.T) {
			up := llmtest.NewUpstream(t, func(llmtest.Request) llmtest.Reply { return sseReply(e.SSE) })
			p := newTestProvider(t, up)
			got := llmtest.ChunkTexts(t, drain(t, p, []kernel.Message{{Role: kernel.RoleUser, Content: "work"}}, llmhub.ModelOptions{}))
			want := make([]string, len(e.Chunks))
			for i, c := range e.Chunks {
				want[i] = string(c)
			}
			if e.GoException != "" {
				goWant, ok := streamExceptions[e.GoException]
				if !ok {
					t.Fatalf("entry names the unknown exception %q", e.GoException)
				}
				used[e.GoException] = true
				want = goWant
			}
			if goWant, ok := streamDivergences[e.Name]; ok {
				divergent[e.Name] = true
				want = goWant
			}
			got, want = withoutJSONReasons(t, got, want)
			if !slices.Equal(got, want) {
				t.Errorf("chunks differ\n got: %s\nwant: %s", strings.Join(got, "\n      "), strings.Join(want, "\n      "))
			}
		})
	}
	for name := range streamExceptions {
		if !used[name] {
			t.Errorf("stream exception %q is used by no entry", name)
		}
	}
	checkDivergencesUsed(t, streamDivergences, divergent)
}

// ---------------------------------------------------------------------------
// Chat responses
// ---------------------------------------------------------------------------

type recordedError struct {
	Message   string `json:"message"`
	Code      string `json:"code"`
	Retryable bool   `json:"retryable"`
}

type chatEntry struct {
	Name        string          `json:"name"`
	Body        string          `json:"body"`
	Result      json.RawMessage `json:"result"`
	Error       *recordedError  `json:"error"`
	GoException string          `json:"goException"`
}

type chatFixture struct {
	Entries []chatEntry `json:"entries"`
}

// emptyChoicesText is the provider's text for a response with no choice; the
// recorded text is not reproduced.
const emptyChoicesText = "the response has no choices"

// failureOf returns the *kernel.ModelError a failed call returned.
func failureOf(t *testing.T, err error) *kernel.ModelError {
	t.Helper()
	var me *kernel.ModelError
	if !errors.As(err, &me) {
		t.Fatalf("the call returned %v (%T), want a *kernel.ModelError", err, err)
	}
	return me
}

// chatWithoutJSONReasons is withoutJSONReasons for a ModelResponse: the reason inside
// "not valid JSON (<reason>)" of each tool call is replaced by a marker in both
// encodings after checking the expected prefix and that the text ends with "): "
// plus the expected preview of the arguments text the server sent for that call (read
// from body).
func chatWithoutJSONReasons(t *testing.T, body string, got, want []byte) (string, string) {
	t.Helper()
	var served struct {
		Choices []struct {
			Message struct {
				ToolCalls []struct {
					ID       string `json:"id"`
					Function struct {
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal([]byte(body), &served); err != nil {
		t.Fatalf("decode the served body: %v", err)
	}
	raws := map[string]string{}
	for _, ch := range served.Choices {
		for _, tc := range ch.Message.ToolCalls {
			raws[tc.ID] = tc.Function.Arguments
		}
	}
	normalize := func(text []byte) string {
		var r kernel.ModelResponse
		if err := json.Unmarshal(text, &r); err != nil {
			return string(text)
		}
		for i, c := range r.Message.ToolCalls {
			if !strings.HasPrefix(c.ArgumentsError, notValidJSONPrefix) {
				continue
			}
			suffix := "): " + tsPreview(raws[c.ID])
			if !strings.HasSuffix(c.ArgumentsError, suffix) {
				continue
			}
			r.Message.ToolCalls[i].ArgumentsError = notValidJSONPrefix + "<reason>" + suffix
		}
		b, err := kernel.EncodeJSON(r)
		if err != nil {
			t.Fatalf("encode the normalised response: %v", err)
		}
		return string(b)
	}
	return normalize(got), normalize(want)
}

// chatDivergences are the recorded answers where the provider fixes a bug of the
// recording: the legacy function_call finish reason is tool_use and an absent
// (null) finish reason is end_turn, where the recording turns every reason it does
// not know into error.
var chatDivergences = map[string]divergence{
	"finish-reason-function_call": swap(`"stopReason":"error"`, `"stopReason":"tool_use"`),
	"finish-reason-null":          swap(`"stopReason":"error"`, `"stopReason":"end_turn"`),
}

// TestOpenAIChatMatchesGolden serves each recorded JSON answer to Chat and requires the
// ModelResponse to encode to the recorded text: text only, null
// content, filtered tool calls, bad arguments, missing usage and the finish reasons.
// The one documented exception is the empty choices list, where the code and the
// retryability are the recorded ones and the text is the provider's own.
func TestOpenAIChatMatchesGolden(t *testing.T) {
	var f chatFixture
	loadFixture(t, "llm-openai-chat.json", &f)
	if len(f.Entries) == 0 {
		t.Fatal("the chat fixture has no entries")
	}
	usedException := false
	divergent := map[string]bool{}
	defer checkDivergencesUsed(t, chatDivergences, divergent)
	for _, e := range f.Entries {
		t.Run(e.Name, func(t *testing.T) {
			up := llmtest.NewUpstream(t, func(llmtest.Request) llmtest.Reply { return jsonReply(e.Body) })
			p := newTestProvider(t, up)
			resp, err := p.Chat(t.Context(), []kernel.Message{{Role: kernel.RoleUser, Content: "hello"}}, llmhub.ModelOptions{})
			if e.Error != nil {
				me := failureOf(t, err)
				want := *e.Error
				switch e.GoException {
				case "empty-choices-text":
					usedException = true
					want.Message = emptyChoicesText
				case "":
				default:
					t.Fatalf("entry names the unknown exception %q", e.GoException)
				}
				got := recordedError{Message: me.Message, Code: string(me.Code), Retryable: me.Retryable}
				if got != want {
					t.Errorf("error differs\n got: %+v\nwant: %+v", got, want)
				}
				return
			}
			if err != nil {
				t.Fatalf("Chat: %v", err)
			}
			got, err := kernel.EncodeJSON(resp)
			if err != nil {
				t.Fatalf("encode the response: %v", err)
			}
			result := e.Result
			if d, ok := chatDivergences[e.Name]; ok {
				divergent[e.Name] = true
				result = []byte(d(t, string(result)))
			}
			gotText, wantText := chatWithoutJSONReasons(t, e.Body, got, result)
			if gotText != wantText {
				t.Errorf("response differs\n got: %s\nwant: %s", got, result)
			}
		})
	}
	if !usedException {
		t.Error("the documented exception empty-choices-text is used by no entry")
	}
}

// ---------------------------------------------------------------------------
// Errors and retries
// ---------------------------------------------------------------------------

type errorEntry struct {
	Name        string            `json:"name"`
	Kind        string            `json:"kind"`
	Method      string            `json:"method"`
	Status      int               `json:"status"`
	Body        string            `json:"body"`
	Headers     map[string]string `json:"headers"`
	Attempts    int               `json:"attempts"`
	Chunks      []json.RawMessage `json:"chunks"`
	Error       *recordedError    `json:"error"`
	Constructor string            `json:"constructor"`
	OpenAI      string            `json:"openai"`
	Novita      string            `json:"novita"`
}

type errorFixture struct {
	Entries []errorEntry `json:"entries"`
}

// errorDivergences are the recorded Chat errors where the provider fixes the
// recorded mapping (error mapping review): a 403 is an auth failure, not a provider
// one, and a refused connection is retryable. The function turns the recorded error
// into the expected one;
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

// TestOpenAIErrorsMatchGolden requires every recorded error to come
// out of the provider with the same code, retryability and text, for Chat and as
// one in-band error chunk for Stream: the status table, the response bodies of every
// shape (JSON envelopes, plain text, empty, arrays, scalars) and a refused connection.
// Even a body that is not JSON keeps its text: the Go SDK drops it from its error
// value, and the provider reads it back from the response.
func TestOpenAIErrorsMatchGolden(t *testing.T) {
	var f errorFixture
	loadFixture(t, "llm-openai-errors.json", &f)
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
				up := llmtest.NewUpstream(t, func(llmtest.Request) llmtest.Reply { return jsonReply(minimalCompletion) })
				p, err := New(llmhub.ProviderConfig{APIKey: "k", BaseURL: llmtest.ClosedURL(t) + "/v1"}, WithHTTPClient(up.Client()), WithMaxRetries(0))
				if err != nil {
					t.Fatalf("New: %v", err)
				}
				used[e.Name+"/"+e.Method] = checkFailure(t, p, e)
			})
		}
	}
	for _, kind := range []string{"status", "connection"} {
		if seen[kind] == 0 {
			t.Errorf("the error fixture has no %q entries", kind)
		}
	}
}

// TestOpenAIRetriesMatchGolden counts the requests the default client sends for each
// status, with a 1 ms retry-after so the test does not wait: the SDK defaults
// (CONTEXT A3) must give the recorded attempts, one for 400, 401, 403, 404 and
// 422 and three for 408, 409, 429 and every 5xx.
func TestOpenAIRetriesMatchGolden(t *testing.T) {
	var f errorFixture
	loadFixture(t, "llm-openai-errors.json", &f)
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
			p, err := New(llmhub.ProviderConfig{APIKey: "k", BaseURL: up.URL + "/v1"}, WithHTTPClient(up.Client()))
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

// TestOpenAIMissingCredentialsMatchesGolden checks the recorded text for an empty API
// key and pins the divergence: the recording throws "Missing credentials ..." from
// the constructor and from the openai and novita registry factories, the provider
// accepts an empty key (a local OpenAI-compatible server needs none) and sends no
// Authorization header.
func TestOpenAIMissingCredentialsMatchesGolden(t *testing.T) {
	var f errorFixture
	loadFixture(t, "llm-openai-errors.json", &f)
	var rec *errorEntry
	for i := range f.Entries {
		if f.Entries[i].Kind == "missingCredentials" {
			rec = &f.Entries[i]
		}
	}
	if rec == nil {
		t.Fatal("the error fixture has no missingCredentials entry")
	}
	if rec.Constructor == "" || rec.OpenAI != rec.Constructor || rec.Novita != rec.Constructor {
		t.Fatalf("the recorded texts are not one text: %q %q %q", rec.Constructor, rec.OpenAI, rec.Novita)
	}
	if !strings.HasPrefix(rec.Constructor, "Missing credentials") {
		t.Fatalf("the recorded TS text %q is not the missing-credentials error", rec.Constructor)
	}
	if _, err := New(llmhub.ProviderConfig{}); err != nil {
		t.Errorf("New with an empty key: %v, want a provider (Q15, Go divergence)", err)
	}
	if _, err := New(llmhub.ProviderConfig{BaseURL: "http://127.0.0.1:1/v1"}, WithName("novita")); err != nil {
		t.Errorf("New with an empty key and a name: %v, want a provider", err)
	}
	for _, name := range []string{"openai", "novita"} {
		p, err := llmhub.ResolveProvider(name, llmhub.ProviderConfig{})
		if err != nil || p == nil {
			t.Errorf("ResolveProvider(%q) with an empty key: %v, want a provider", name, err)
		}
	}

	// The request of a provider without a key carries no Authorization header.
	up := llmtest.NewUpstream(t, func(llmtest.Request) llmtest.Reply { return jsonReply(minimalCompletion) })
	p, err := New(llmhub.ProviderConfig{BaseURL: up.URL}, WithHTTPClient(up.Client()), WithMaxRetries(0))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := p.Chat(t.Context(), []kernel.Message{{Role: kernel.RoleUser, Content: "hello"}}, llmhub.ModelOptions{}); err != nil {
		t.Fatalf("Chat without a key: %v", err)
	}
	if h, ok := up.Requests()[0].Header["Authorization"]; ok {
		t.Errorf("a request without a key sent Authorization %q", h)
	}
}

// ---------------------------------------------------------------------------
// ListModels
// ---------------------------------------------------------------------------

type modelsEntry struct {
	Name         string            `json:"name"`
	ProviderName string            `json:"providerName"`
	Status       int               `json:"status"`
	Replies      []string          `json:"replies"`
	Requests     []recordedRequest `json:"requests"`
	Result       json.RawMessage   `json:"result"`
	Error        *recordedError    `json:"error"`
}

type modelsFixture struct {
	Entries []modelsEntry `json:"entries"`
}

// TestOpenAIListModelsMatchesGolden serves the recorded listings and requires the same
// request (path and the authorization header) and the same []llmhub.ModelInfo as the
// recording for plain OpenAI, Novita, OpenRouter and Groq entries, and the mapped
// ModelError for an API error.
func TestOpenAIListModelsMatchesGolden(t *testing.T) {
	var f modelsFixture
	loadFixture(t, "llm-openai-models.json", &f)
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
			p := newTestProvider(t, up, WithName(e.ProviderName))
			models, err := p.ListModels(t.Context())
			reqs := up.Requests()
			if len(reqs) != len(e.Requests) {
				t.Fatalf("the upstream saw %d requests, TS sent %d", len(reqs), len(e.Requests))
			}
			for i, want := range e.Requests {
				got := reqs[i]
				if got.Method != want.Method || got.Path != want.Path || got.RawQuery != want.Query {
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

// TestOpenAIFixturesCovered fails when a fixture with the llm-openai- prefix has no
// test above, and when a listed fixture has no file.
func TestOpenAIFixturesCovered(t *testing.T) {
	entries, err := os.ReadDir(wiretest.Dir(t))
	if err != nil {
		t.Fatalf("read fixture dir: %v", err)
	}
	present := map[string]bool{}
	for _, e := range entries {
		present[e.Name()] = true
		if strings.HasPrefix(e.Name(), "llm-openai-") && !slices.Contains(openaiFixtures, e.Name()) {
			t.Errorf("fixture %s has no entry in openaiFixtures", e.Name())
		}
	}
	for _, name := range openaiFixtures {
		if !present[name] {
			t.Errorf("openaiFixtures entry %s has no fixture file", name)
		}
	}
}
