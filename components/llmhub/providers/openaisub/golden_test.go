package openaisub

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"
	"unicode/utf16"

	"github.com/Phong95/aec-agent-go/components/llmhub"
	"github.com/Phong95/aec-agent-go/internal/jsonx"
	"github.com/Phong95/aec-agent-go/internal/llmtest"
	"github.com/Phong95/aec-agent-go/internal/wiretest"
	"github.com/Phong95/aec-agent-go/kernel"
)

// The goldens of this package are recorded from a reference provider
// (tools/wiregen/llm/subscription.cases.mjs) with a stubbed fetch: the endpoint is a
// constant there, so the recording cannot point it at a loopback server. Each test
// serves the recorded server side to the provider on an llmtest.Upstream and compares
// what the provider sent and produced with the recording. No test
// reaches chatgpt.com: the clients are built from the upstream's loopback-only client
// and the endpoints are the upstream's.

// subscriptionFixtures lists every fixture of this package, so
// TestSubscriptionFixturesCovered fails when one is added to wiregen without a test.
var subscriptionFixtures = []string{
	"llm-subscription-requests.json",
	"llm-subscription-streams.json",
	"llm-subscription-errors.json",
	"llm-subscription-models.json",
}

const (
	responsesPath = "/backend-api/codex/responses"
	modelsPath    = "/backend-api/codex/models"
)

// caseConfig is the token, the account id and the default model of a recorded case.
type caseConfig struct {
	Token   string `json:"token"`
	Account string `json:"account"`
	Model   string `json:"model"`
}

// newTestProvider builds a provider that talks to up through its loopback-only
// client, with the endpoints of the upstream.
func newTestProvider(t *testing.T, up *llmtest.Upstream, c caseConfig, opts ...Option) *Provider {
	t.Helper()
	all := append([]Option{
		WithEndpoints(up.URL+responsesPath, up.URL+modelsPath),
		WithHTTPClient(up.Client()),
	}, opts...)
	p, err := New(llmhub.ProviderConfig{OAuthAccessToken: c.Token, ChatgptAccountID: c.Account, Model: c.Model}, all...)
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

// sseBytes is a 200 reply with an event stream, written as the given byte chunks with
// a flush after each, so the client sees every recorded boundary as its own read.
func sseBytes(chunks []string) llmtest.Reply {
	return llmtest.Reply{Header: map[string]string{"Content-Type": "text/event-stream"}, Chunks: chunks}
}

// minimalStream is the reply served when a test only needs the call to finish (the
// request is what is compared).
const minimalStream = "data: {\"type\":\"response.completed\",\"response\":{\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}}\n\n"

// recordedError is what a ModelError carries.
type recordedError struct {
	Message   string `json:"message"`
	Code      string `json:"code"`
	Retryable bool   `json:"retryable"`
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

// errorOf is the recordedError of a ModelError.
func errorOf(me *kernel.ModelError) recordedError {
	return recordedError{Message: me.Message, Code: string(me.Code), Retryable: me.Retryable}
}

// ---------------------------------------------------------------------------
// Requests
// ---------------------------------------------------------------------------

type recordedRequest struct {
	URL     string            `json:"url"`
	Method  string            `json:"method"`
	Headers map[string]string `json:"headers"`
	Body    string            `json:"body"`
}

type requestEntry struct {
	Name     string              `json:"name"`
	Method   string              `json:"method"`
	Messages []kernel.Message    `json:"messages"`
	Options  llmhub.ModelOptions `json:"options"`
	Config   caseConfig          `json:"config"`
	Request  recordedRequest     `json:"request"`
}

type requestFixture struct {
	Entries []requestEntry `json:"entries"`
}

// comparedHeaders are the request headers of the calls that the provider sets.
// Other headers are the transport's (user agent, content length, encoding).
var comparedHeaders = []string{"authorization", "content-type", "openai-beta", "chatgpt-account-id", "originator"}

// checkHeaders requires the headers the provider sent to equal the recorded headers:
// the same names (compared without regard to case, as servers do) and the same
// values, and no header the recording does not have.
func checkHeaders(t *testing.T, got http.Header, want map[string]string) {
	t.Helper()
	for _, name := range comparedHeaders {
		wantValue, wantOK := want[name]
		haveValues, haveOK := got[http.CanonicalHeaderKey(name)]
		if wantOK != haveOK {
			t.Errorf("header %s: sent %v, TS sent %v", name, haveOK, wantOK)
			continue
		}
		// An HTTP field value has its surrounding whitespace removed (net/http trims it
		// when it writes and when it reads), so "Bearer " of an empty token is "Bearer".
		if haveOK && (len(haveValues) != 1 || haveValues[0] != strings.TrimSpace(wantValue)) {
			t.Errorf("header %s: got %q, want %q", name, haveValues, wantValue)
		}
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

// requestDivergences are the request bodies where the expected input mapping differs
// from the recording, with the body the provider must send: an assistant message's
// text comes before its function calls (the recording puts the calls first and the
// text last, after the outputs too), and the toolResults of a tool message are sent
// as function_call_output items (the recording has a user message with empty text).
// The key is the entry name; chat and stream send the same body.
var requestDivergences = map[string]func(t *testing.T, ts string) string{
	"assistant-two-tool-calls-with-text-puts-calls-first": textBeforeCalls,
	"assistant-calls-results-and-text-in-that-order":      textBeforeCalls,
	"special-characters": textBeforeCalls,
	"all-options":        textBeforeCalls,
	"role-tool-with-results-is-sent-as-empty-user-text": func(t *testing.T, ts string) string {
		t.Helper()
		const tsItem = `{"role":"user","content":[{"type":"input_text","text":""}]}`
		if !strings.Contains(ts, tsItem) {
			t.Fatalf("the TS body has no %s", tsItem)
		}
		return strings.Replace(ts, tsItem, `{"type":"function_call_output","call_id":"c1","output":"boom"},{"type":"function_call_output","call_id":"c2","output":"ok"},{"type":"function_call_output","call_id":"c3","output":"no flag"}`, 1)
	},
}

// assistantText is the output_text message of an assistant in a recorded body.
var assistantText = regexp.MustCompile(`,\{"role":"assistant","content":\[\{"type":"output_text","text":"(?:[^"\\]|\\.)*"\}\]\}`)

// textBeforeCalls moves the one assistant output_text message of a recorded body in
// front of the first function_call item, where the provider writes it.
func textBeforeCalls(t *testing.T, ts string) string {
	t.Helper()
	loc := assistantText.FindStringIndex(ts)
	if loc == nil {
		t.Fatal("the TS body has no assistant output_text message")
	}
	item := ts[loc[0]+1 : loc[1]]
	rest := ts[:loc[0]] + ts[loc[1]:]
	at := strings.Index(rest, `{"type":"function_call"`)
	if at < 0 || at > loc[0] {
		t.Fatal("the TS body has no function_call before the assistant text")
	}
	return rest[:at] + item + "," + rest[at:]
}

// TestSubscriptionRequestsMatchGolden requires the request the provider sends to equal
// the recorded one: the same path, the same headers, and the body byte for byte, for
// chat and for stream, except for the named divergences (requestDivergences).
func TestSubscriptionRequestsMatchGolden(t *testing.T) {
	var f requestFixture
	loadFixture(t, "llm-subscription-requests.json", &f)
	if len(f.Entries) == 0 {
		t.Fatal("the request fixture has no entries")
	}
	used := map[string]bool{}
	defer checkDivergencesUsed(t, requestDivergences, used)
	for _, e := range f.Entries {
		t.Run(e.Name+"/"+e.Method, func(t *testing.T) {
			up := llmtest.NewUpstream(t, func(llmtest.Request) llmtest.Reply { return sseBytes([]string{minimalStream}) })
			p := newTestProvider(t, up, e.Config)
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
			want, err := url.Parse(e.Request.URL)
			if err != nil {
				t.Fatalf("recorded url: %v", err)
			}
			if got.Method != e.Request.Method || got.Path != want.EscapedPath() || got.RawQuery != want.RawQuery {
				t.Errorf("request line: got %s %s?%s, want %s %s?%s", got.Method, got.Path, got.RawQuery, e.Request.Method, want.EscapedPath(), want.RawQuery)
			}
			if e.Request.URL != ResponsesURL {
				t.Errorf("TS called %s, the Go constant is %s", e.Request.URL, ResponsesURL)
			}
			checkHeaders(t, got.Header, e.Request.Headers)
			wantBody := e.Request.Body
			if d, ok := requestDivergences[e.Name]; ok {
				used[e.Name] = true
				wantBody = d(t, wantBody)
			}
			if string(got.Body) != wantBody {
				t.Errorf("body differs\n got: %s\nwant: %s", got.Body, wantBody)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Streams
// ---------------------------------------------------------------------------

// recordedChat is what the recorded chat did with a transcript: the response, or the
// error it threw.
type recordedChat struct {
	Result json.RawMessage `json:"result"`
	Error  *recordedError  `json:"error"`
}

type streamEntry struct {
	Name string `json:"name"`
	// Chunks are the byte chunks of the transcript, base64.
	Chunks []string `json:"chunks"`
	// ReadError is the message of the error the stream failed with after its chunks.
	ReadError string            `json:"readError"`
	Out       []json.RawMessage `json:"out"`
	Chat      recordedChat      `json:"chat"`
	// GoLike names the entry whose recorded result the provider must produce here:
	// the provider also accepts CRLF framing, so for that entry the recording has a
	// different answer (documented exception).
	GoLike string `json:"goLike"`
}

type streamFixture struct {
	Entries []streamEntry `json:"entries"`
}

// goStream is what the provider must produce for a transcript where the expected
// output differs from the recording: the stream chunks (nil: the recorded ones) and
// the Chat result or error (both nil: the recorded one).
type goStream struct {
	out     []string
	chat    string
	chatErr *recordedError
}

// truncatedText is the text of the error chunk of a stream cut before its final
// event.
const truncatedText = "the stream ended before response.completed: the answer is incomplete"

// truncatedErr is the Chat error of such a stream: a retryable provider error.
var truncatedErr = &recordedError{Message: truncatedText, Code: "provider", Retryable: true}

// streamDivergences are the transcripts where the expected output differs from the
// recording, keyed by entry name:
//   - an event still open when the body ends (no blank line, or no newline at all,
//     after its last line) is read; the recording drops it;
//   - a stream that ends before response.completed (an empty one, one of blank
//     lines only, one cut after some text) ends with one terminal error chunk, and
//     Chat fails with a retryable provider error; the recording ends with a plain
//     done;
//   - response.failed ends the stream with an error chunk of its message, and Chat
//     fails with the mapped error (server_error: provider, retryable); the recording
//     ignores it and ends with a plain done;
//   - an incomplete response (max_output_tokens) is max_tokens in Chat; the
//     recording reports end_turn.
var streamDivergences = map[string]goStream{
	"unterminated-final-event-is-dropped": {
		out: []string{
			`{"type":"text","content":"kept"}`,
			`{"type":"text","content":"dropped"}`,
			`{"type":"done","usage":{"inputTokens":2,"outputTokens":3},"stopReason":"end_turn"}`,
		},
		chat: `{"message":{"role":"assistant","content":"keptdropped"},"usage":{"inputTokens":2,"outputTokens":3},"stopReason":"end_turn"}`,
	},
	"final-event-without-any-newline-is-dropped": {
		out: []string{
			`{"type":"text","content":"kept"}`,
			`{"type":"done","usage":{"inputTokens":8,"outputTokens":9},"stopReason":"end_turn"}`,
		},
		chat: `{"message":{"role":"assistant","content":"kept"},"usage":{"inputTokens":8,"outputTokens":9},"stopReason":"end_turn"}`,
	},
	"empty-stream": {
		out:     []string{`{"type":"error","error":"` + truncatedText + `"}`},
		chatErr: truncatedErr,
	},
	"only-blank-lines": {
		out:     []string{`{"type":"error","error":"` + truncatedText + `"}`},
		chatErr: truncatedErr,
	},
	"no-completed-event": {
		out:     []string{`{"type":"text","content":"a"}`, `{"type":"error","error":"` + truncatedText + `"}`},
		chatErr: truncatedErr,
	},
	"response-failed-and-error-events-end-in-a-plain-done": {
		out:     []string{`{"type":"text","content":"partial"}`, `{"type":"error","error":"boom"}`},
		chatErr: &recordedError{Message: "boom", Code: "provider", Retryable: true},
	},
	"incomplete-response-is-an-end-turn": {
		chat: `{"message":{"role":"assistant","content":"cut off"},"usage":{"inputTokens":5,"outputTokens":4096},"stopReason":"max_tokens"}`,
	},
}

// expected returns the entry whose recorded results the provider must produce for e:
// e itself, or the entry it is documented to be like, with the divergence of e applied
// (streamDivergences).
func (f streamFixture) expected(t *testing.T, e streamEntry) streamEntry {
	t.Helper()
	want := e
	if e.GoLike != "" {
		found := false
		for _, other := range f.Entries {
			if other.Name == e.GoLike {
				want, found = other, true
			}
		}
		if !found {
			t.Fatalf("entry %s is documented to be like %s, which is not in the fixture", e.Name, e.GoLike)
		}
	}
	d, ok := streamDivergences[e.Name]
	if !ok {
		return want
	}
	if d.out != nil {
		want.Out = make([]json.RawMessage, len(d.out))
		for i, c := range d.out {
			want.Out[i] = json.RawMessage(c)
		}
	}
	switch {
	case d.chatErr != nil:
		want.Chat = recordedChat{Error: d.chatErr}
	case d.chat != "":
		want.Chat = recordedChat{Result: json.RawMessage(d.chat)}
	}
	return want
}

// TestSubscriptionStreamDivergencesAreUsed fails when a divergence names no entry.
func TestSubscriptionStreamDivergencesAreUsed(t *testing.T) {
	var f streamFixture
	loadFixture(t, "llm-subscription-streams.json", &f)
	used := map[string]bool{}
	for _, e := range f.Entries {
		used[e.Name] = true
	}
	checkDivergencesUsed(t, streamDivergences, used)
}

// decodeChunks decodes the base64 byte chunks of an entry.
func decodeChunks(t *testing.T, e streamEntry) []string {
	t.Helper()
	out := make([]string, len(e.Chunks))
	for i, c := range e.Chunks {
		b, err := base64.StdEncoding.DecodeString(c)
		if err != nil {
			t.Fatalf("entry %s chunk %d: %v", e.Name, i, err)
		}
		out[i] = string(b)
	}
	return out
}

// entryProvider builds the provider an entry is served to: an upstream that writes
// the recorded chunks with a flush after each, or, for an entry that ends in a read
// error, a client whose response body fails after the chunks.
func entryProvider(t *testing.T, e streamEntry) *Provider {
	t.Helper()
	chunks := decodeChunks(t, e)
	if e.ReadError != "" {
		return providerOnBody(t, &failingBody{chunks: chunks, err: errors.New(e.ReadError)})
	}
	up := llmtest.NewUpstream(t, func(llmtest.Request) llmtest.Reply { return sseBytes(chunks) })
	return newTestProvider(t, up, caseConfig{Token: "tok", Account: "acct"})
}

// finalArguments reads the transcript the way the test expects and returns the
// final arguments text of every function call that finished, by call id. It is
// written apart from the parser under test.
func finalArguments(chunks []string) map[string]string {
	text := strings.ReplaceAll(strings.Join(chunks, ""), "\r\n", "\n")
	finals := map[string]string{}
	for _, block := range strings.Split(text, "\n\n") {
		for _, line := range strings.Split(block, "\n") {
			payload, ok := strings.CutPrefix(line, "data:")
			if !ok {
				continue
			}
			var ev struct {
				Type string `json:"type"`
				Item struct {
					Type      string `json:"type"`
					CallID    string `json:"call_id"`
					Arguments string `json:"arguments"`
				} `json:"item"`
			}
			if json.Unmarshal([]byte(strings.TrimSpace(payload)), &ev) == nil && ev.Type == "response.output_item.done" && ev.Item.Type == "function_call" {
				finals[ev.Item.CallID] = ev.Item.Arguments
			}
			break
		}
	}
	return finals
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

// withoutJSONReason applies the documented exception of the arguments error: the
// "not valid JSON (<reason>)" text has a reason that is the JSON parser's own
// (encoding/json's here, another parser's in the recording), so the reason is
// replaced by a marker after checking that the text has the expected prefix and ends
// with "): " plus the preview of the final arguments text of the call. Everything else
// must match byte for byte. It reports whether it changed the call.
func withoutJSONReason(call *kernel.ToolCall, finals map[string]string) bool {
	if !strings.HasPrefix(call.ArgumentsError, notValidJSONPrefix) {
		return false
	}
	suffix := "): " + tsPreview(finals[call.ID])
	if !strings.HasSuffix(call.ArgumentsError, suffix) {
		return false
	}
	call.ArgumentsError = notValidJSONPrefix + "<reason>" + suffix
	return true
}

// normalizeChunks returns the chunk texts with the JSON reason of every arguments
// error replaced (withoutJSONReason).
func normalizeChunks(t *testing.T, texts []string, finals map[string]string) []string {
	t.Helper()
	out := make([]string, len(texts))
	for i, text := range texts {
		out[i] = text
		var c kernel.StreamChunk
		if err := json.Unmarshal([]byte(text), &c); err != nil || c.ToolCall == nil || !withoutJSONReason(c.ToolCall, finals) {
			continue
		}
		b, err := kernel.EncodeJSON(c)
		if err != nil {
			t.Fatalf("encode the normalised chunk: %v", err)
		}
		out[i] = string(b)
	}
	return out
}

// normalizeResponse is the same for the encoded text of a ModelResponse.
func normalizeResponse(t *testing.T, text string, finals map[string]string) string {
	t.Helper()
	if !strings.Contains(text, notValidJSONPrefix) {
		return text
	}
	var resp kernel.ModelResponse
	if err := json.Unmarshal([]byte(text), &resp); err != nil {
		t.Fatalf("decode the response: %v", err)
	}
	for i := range resp.Message.ToolCalls {
		withoutJSONReason(&resp.Message.ToolCalls[i], finals)
	}
	b, err := kernel.EncodeJSON(resp)
	if err != nil {
		t.Fatalf("encode the normalised response: %v", err)
	}
	return string(b)
}

// rawTexts is the recorded chunks as strings.
func rawTexts(chunks []json.RawMessage) []string {
	out := make([]string, len(chunks))
	for i, c := range chunks {
		out[i] = string(c)
	}
	return out
}

// TestSubscriptionStreamsMatchGolden serves each recorded transcript to the Go provider
// as the recorded byte chunks (a flush after each, so every boundary, inside an event
// and inside a multi-byte character, reaches the parser as a read boundary) and
// requires the chunks to equal the recorded chunks, one by one. The one documented
// exception is the CRLF-framed transcript (see streamEntry.GoLike).
func TestSubscriptionStreamsMatchGolden(t *testing.T) {
	var f streamFixture
	loadFixture(t, "llm-subscription-streams.json", &f)
	if len(f.Entries) == 0 {
		t.Fatal("the stream fixture has no entries")
	}
	exceptions := 0
	for _, e := range f.Entries {
		want := f.expected(t, e)
		if e.GoLike != "" {
			exceptions++
		}
		t.Run(e.Name, func(t *testing.T) {
			p := entryProvider(t, e)
			got := llmtest.ChunkTexts(t, drain(t, p, []kernel.Message{{Role: kernel.RoleUser, Content: "go"}}, llmhub.ModelOptions{}))
			finals := finalArguments(decodeChunks(t, want))
			gotTexts, wantTexts := normalizeChunks(t, got, finals), normalizeChunks(t, rawTexts(want.Out), finals)
			if !slices.Equal(gotTexts, wantTexts) {
				t.Errorf("chunks differ\n got: %s\nwant: %s", strings.Join(gotTexts, "\n      "), strings.Join(wantTexts, "\n      "))
			}
		})
	}
	if exceptions == 0 {
		t.Error("no entry uses the documented CRLF exception: the exception is stale")
	}
}

// TestSubscriptionChatMatchesGolden serves the same transcripts to Chat and requires the
// ModelResponse to encode to the recorded text (or the same error: a read error ends
// in a retryable provider ModelError). The recording's stop reason is only tool_use
// or end_turn, also after an incomplete or failed response; the provider reads the
// final status, and the entries where that shows are named divergences
// (streamDivergences).
func TestSubscriptionChatMatchesGolden(t *testing.T) {
	var f streamFixture
	loadFixture(t, "llm-subscription-streams.json", &f)
	if len(f.Entries) == 0 {
		t.Fatal("the stream fixture has no entries")
	}
	for _, e := range f.Entries {
		want := f.expected(t, e)
		t.Run(e.Name, func(t *testing.T) {
			p := entryProvider(t, e)
			resp, err := p.Chat(t.Context(), []kernel.Message{{Role: kernel.RoleUser, Content: "go"}}, llmhub.ModelOptions{})
			if want.Chat.Error != nil {
				if got := errorOf(failureOf(t, err)); got != *want.Chat.Error {
					t.Errorf("error differs\n got: %+v\nwant: %+v", got, *want.Chat.Error)
				}
				return
			}
			if err != nil {
				t.Fatalf("Chat: %v", err)
			}
			b, err := kernel.EncodeJSON(resp)
			if err != nil {
				t.Fatalf("encode the response: %v", err)
			}
			finals := finalArguments(decodeChunks(t, want))
			got, wantText := normalizeResponse(t, string(b), finals), normalizeResponse(t, string(want.Chat.Result), finals)
			if got != wantText {
				t.Errorf("response differs\n got: %s\nwant: %s", got, wantText)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Errors
// ---------------------------------------------------------------------------

type errorEntry struct {
	Name       string            `json:"name"`
	Kind       string            `json:"kind"`
	Status     int               `json:"status"`
	StatusText string            `json:"statusText"`
	Body       string            `json:"body"`
	Stream     []json.RawMessage `json:"stream"`
	Chat       recordedError     `json:"chat"`
	Models     recordedError     `json:"models"`
}

type errorFixture struct {
	Entries []errorEntry `json:"entries"`
}

// streamErrorText is the text of the one recorded error chunk.
func streamErrorText(t *testing.T, e errorEntry) string {
	t.Helper()
	if len(e.Stream) != 1 {
		t.Fatalf("entry %s: TS yielded %d chunks, want one error chunk", e.Name, len(e.Stream))
	}
	var c kernel.StreamChunk
	if err := json.Unmarshal(e.Stream[0], &c); err != nil || c.Type != kernel.ChunkError {
		t.Fatalf("entry %s: the TS chunk %s is not an error chunk", e.Name, e.Stream[0])
	}
	return c.Error
}

// TestSubscriptionErrorsMatchGolden requires every recorded error to come out of the
// provider the same way:
//   - "http" entries are served by an upstream: Stream gives one in-band error chunk
//     with the recorded text and ListModels the recorded error (code, retryability,
//     text). The recorded Chat error is a retryable provider error with the stream's
//     text, even for a 401; the provider's Chat returns the same typed error as
//     ListModels (a divergence for every http entry), and the recorded Chat error is
//     only checked to have that retryable provider shape;
//   - "direct" entries have a status text a Go server cannot write ("" or a custom
//     reason), so they go straight through mapHTTPError;
//   - the "network" entry is a failure to connect: the code and retryability are the
//     recorded ones and the text is the net/http error's own (documented exception).
func TestSubscriptionErrorsMatchGolden(t *testing.T) {
	var f errorFixture
	loadFixture(t, "llm-subscription-errors.json", &f)
	seen := map[string]int{}
	msgs := []kernel.Message{{Role: kernel.RoleUser, Content: "go"}}
	for _, e := range f.Entries {
		seen[e.Kind]++
		switch e.Kind {
		case "http":
			t.Run(e.Name, func(t *testing.T) {
				var chunks []string
				if e.Body != "" {
					chunks = []string{e.Body}
				}
				up := llmtest.NewUpstream(t, func(llmtest.Request) llmtest.Reply {
					return llmtest.Reply{Status: e.Status, Chunks: chunks}
				})
				p := newTestProvider(t, up, caseConfig{Token: "tok", Account: "acct"})

				got := llmtest.ChunkTexts(t, drain(t, p, msgs, llmhub.ModelOptions{}))
				if want := rawTexts(e.Stream); !slices.Equal(got, want) {
					t.Errorf("stream chunks differ\n got: %v\nwant: %v", got, want)
				}
				_, err := p.Chat(t.Context(), msgs, llmhub.ModelOptions{})
				if got := errorOf(failureOf(t, err)); got != e.Models {
					t.Errorf("chat error differs\n got: %+v\nwant: %+v (the typed error, Q8 fixed in Go)", got, e.Models)
				}
				if e.Chat.Code != "provider" || !e.Chat.Retryable || e.Chat.Message != streamErrorText(t, e) {
					t.Errorf("TS chat error %+v is not the Q8 retryable provider error with the stream text", e.Chat)
				}
				_, err = p.ListModels(t.Context())
				if got := errorOf(failureOf(t, err)); got != e.Models {
					t.Errorf("models error differs\n got: %+v\nwant: %+v", got, e.Models)
				}
				// The recorded status text must be the one a Go server writes, or the
				// entry would not test what it says.
				if want := http.StatusText(e.Status); want != e.StatusText {
					t.Errorf("the entry's status text %q is not the Go server's %q", e.StatusText, want)
				}
			})
		case "direct":
			t.Run(e.Name, func(t *testing.T) {
				me := mapHTTPError(e.Status, e.StatusText, []byte(e.Body))
				if got := errorOf(me); got != e.Models {
					t.Errorf("mapHTTPError differs\n got: %+v\nwant: %+v", got, e.Models)
				}
				if me.Message != streamErrorText(t, e) {
					t.Errorf("stream text: got %q, want %q", me.Message, streamErrorText(t, e))
				}
				if e.Chat.Code != "provider" || !e.Chat.Retryable || e.Chat.Message != me.Message {
					t.Errorf("TS chat error %+v is not the retryable provider error with the stream text", e.Chat)
				}
			})
		case "network":
			t.Run(e.Name, func(t *testing.T) {
				up := llmtest.NewUpstream(t, func(llmtest.Request) llmtest.Reply { return sseBytes([]string{minimalStream}) })
				closed := llmtest.ClosedURL(t)
				p := newTestProvider(t, up, caseConfig{Token: "tok-secret", Account: "acct"}, WithEndpoints(closed+responsesPath, closed+modelsPath))

				chunks := drain(t, p, msgs, llmhub.ModelOptions{})
				if len(chunks) != 1 || chunks[0].Type != kernel.ChunkError || chunks[0].Error == "" {
					t.Fatalf("stream gave %+v, want one error chunk with a text", chunks)
				}
				_, err := p.Chat(t.Context(), msgs, llmhub.ModelOptions{})
				chat := failureOf(t, err)
				_, err = p.ListModels(t.Context())
				models := failureOf(t, err)
				if chat.Code != kernel.ModelErrorCode(e.Chat.Code) || chat.Retryable != e.Chat.Retryable {
					t.Errorf("chat error: got %q retryable %v, want %q %v", chat.Code, chat.Retryable, e.Chat.Code, e.Chat.Retryable)
				}
				if string(models.Code) != e.Models.Code || models.Retryable != e.Models.Retryable {
					t.Errorf("models error: got %q retryable %v, want %q %v", models.Code, models.Retryable, e.Models.Code, e.Models.Retryable)
				}
				// The text is Go's own and never carries a credential.
				for _, text := range []string{chunks[0].Error, chat.Message, models.Message} {
					if strings.Contains(text, "tok-secret") {
						t.Errorf("error text %q carries the token", text)
					}
				}
			})
		default:
			t.Errorf("entry %s has the unknown kind %q", e.Name, e.Kind)
		}
	}
	for _, kind := range []string{"http", "direct", "network"} {
		if seen[kind] == 0 {
			t.Errorf("the error fixture has no %q entries", kind)
		}
	}
}

// ---------------------------------------------------------------------------
// ListModels
// ---------------------------------------------------------------------------

type modelsEntry struct {
	Name       string          `json:"name"`
	Config     caseConfig      `json:"config"`
	Status     int             `json:"status"`
	StatusText string          `json:"statusText"`
	Body       string          `json:"body"`
	Request    recordedRequest `json:"request"`
	Result     json.RawMessage `json:"result"`
	Error      *recordedError  `json:"error"`
	// Raw is the name of the raw (non-ModelError) error the recording let escape.
	Raw string `json:"raw"`
}

type modelsFixture struct {
	Entries []modelsEntry `json:"entries"`
}

// TestSubscriptionListModelsMatchesGolden serves the recorded /models answers and requires
// the same request (a GET with authorization, originator and the account id, and no
// OpenAI-Beta and no content type) and the same []llmhub.ModelInfo as the recording:
// entries with visibility none dropped, the others through llmhub.MapCodexModel. A non-2xx
// answer is the recorded error. The one documented exception is a body that is not
// JSON: the recording has a raw SyntaxError, the provider returns a provider
// ModelError.
func TestSubscriptionListModelsMatchesGolden(t *testing.T) {
	var f modelsFixture
	loadFixture(t, "llm-subscription-models.json", &f)
	if len(f.Entries) == 0 {
		t.Fatal("the models fixture has no entries")
	}
	rawErrors, forbidden := 0, 0
	for _, e := range f.Entries {
		t.Run(e.Name, func(t *testing.T) {
			var chunks []string
			if e.Body != "" {
				chunks = []string{e.Body}
			}
			up := llmtest.NewUpstream(t, func(llmtest.Request) llmtest.Reply {
				return llmtest.Reply{Status: e.Status, Header: map[string]string{"Content-Type": "application/json"}, Chunks: chunks}
			})
			p := newTestProvider(t, up, e.Config)
			models, err := p.ListModels(t.Context())

			reqs := up.Requests()
			if len(reqs) != 1 {
				t.Fatalf("the upstream saw %d requests, want 1", len(reqs))
			}
			got := reqs[0]
			want, perr := url.Parse(e.Request.URL)
			if perr != nil {
				t.Fatalf("recorded url: %v", perr)
			}
			if got.Method != e.Request.Method || got.Path != want.EscapedPath() || got.RawQuery != want.RawQuery {
				t.Errorf("request line: got %s %s?%s, want %s %s?%s", got.Method, got.Path, got.RawQuery, e.Request.Method, want.EscapedPath(), want.RawQuery)
			}
			if e.Request.URL != ModelsURL+"?client_version="+CodexClientVersion {
				t.Errorf("TS called %s, the Go constants give %s?client_version=%s", e.Request.URL, ModelsURL, CodexClientVersion)
			}
			checkHeaders(t, got.Header, e.Request.Headers)
			if len(got.Body) != 0 {
				t.Errorf("the GET carried a body of %d bytes", len(got.Body))
			}

			switch {
			case e.Raw != "":
				rawErrors++
				me := failureOf(t, err)
				if me.Code != kernel.ModelCodeProvider || me.Retryable {
					t.Errorf("a body that is not JSON: got code %q retryable %v, want a non-retryable provider error", me.Code, me.Retryable)
				}
			case e.Error != nil:
				want := *e.Error
				if e.Status == http.StatusForbidden {
					// Divergence (error mapping review): a 403 is an auth failure, the
					// recording gives provider.
					want.Code = "auth"
					forbidden++
				}
				if got := errorOf(failureOf(t, err)); got != want {
					t.Errorf("error differs\n got: %+v\nwant: %+v", got, want)
				}
			default:
				if err != nil {
					t.Fatalf("ListModels: %v", err)
				}
				b, merr := jsonx.Marshal(models)
				if merr != nil {
					t.Fatalf("encode the models: %v", merr)
				}
				if string(b) != string(e.Result) {
					t.Errorf("models differ\n got: %s\nwant: %s", b, e.Result)
				}
			}
		})
	}
	if rawErrors == 0 {
		t.Error("no entry records the raw SyntaxError of TS: the documented exception is stale")
	}
	if forbidden == 0 {
		t.Error("no entry records a 403: the auth divergence is stale")
	}
}

// ---------------------------------------------------------------------------
// Coverage
// ---------------------------------------------------------------------------

// TestSubscriptionFixturesCovered fails when a fixture with the llm-subscription-
// prefix has no test above, and when a listed fixture has no file.
func TestSubscriptionFixturesCovered(t *testing.T) {
	entries, err := os.ReadDir(wiretest.Dir(t))
	if err != nil {
		t.Fatalf("read fixture dir: %v", err)
	}
	present := map[string]bool{}
	for _, e := range entries {
		present[e.Name()] = true
		if strings.HasPrefix(e.Name(), "llm-subscription-") && !slices.Contains(subscriptionFixtures, e.Name()) {
			t.Errorf("fixture %s has no entry in subscriptionFixtures", e.Name())
		}
	}
	for _, name := range subscriptionFixtures {
		if !present[name] {
			t.Errorf("subscriptionFixtures entry %s has no fixture file", name)
		}
	}
}
