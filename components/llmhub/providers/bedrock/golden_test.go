package bedrock

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"slices"
	"strings"
	"testing"
	"unicode/utf16"

	"github.com/aws/aws-sdk-go-v2/aws"
	awshttp "github.com/aws/aws-sdk-go-v2/aws/transport/http"
	"github.com/aws/aws-sdk-go-v2/service/bedrock"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime/types"
	"github.com/aws/smithy-go"
	smithyhttp "github.com/aws/smithy-go/transport/http"

	"github.com/Phong95/aec-agent-go/components/llmhub"
	"github.com/Phong95/aec-agent-go/internal/jsonx"
	"github.com/Phong95/aec-agent-go/internal/llmtest"
	"github.com/Phong95/aec-agent-go/internal/wiretest"
	"github.com/Phong95/aec-agent-go/kernel"
)

// The goldens of this package are recorded from a REAL reference provider and the
// real AWS SDK for JavaScript (tools/wiregen/llm/bedrock.cases.mjs). A stream, a
// response or an error is served to the reference provider by replacing the client's
// send method, and a request body is captured on a loopback server that answers 400
// after recording it. No test reaches AWS: the Go provider runs on an injected
// ConverseAPI, or on the real client pointed at an httptest server whose transport
// refuses any other host.

// bedrockFixtures lists every fixture of this package, so TestBedrockFixturesCovered
// fails when one is added to wiregen without a test.
var bedrockFixtures = []string{
	"llm-bedrock-requests.json",
	"llm-bedrock-streams.json",
	"llm-bedrock-chat.json",
	"llm-bedrock-errors.json",
	"llm-bedrock-models.json",
}

// providerConfig is the static credentials every test uses. They are placeholders.
func providerConfig() llmhub.ProviderConfig {
	return llmhub.ProviderConfig{APIKey: "AKIA-TEST", SecretKey: "secret-test", Region: "us-east-1"}
}

// realProvider builds a provider on the REAL AWS clients, pointed at up through its
// loopback-only HTTP client, with the SDK retries off (one attempt).
func realProvider(t *testing.T, up *llmtest.Upstream, cfg llmhub.ProviderConfig, opts ...Option) *Provider {
	t.Helper()
	all := append([]Option{WithBaseEndpoint(up.URL), WithHTTPClient(up.Client()), WithRetryMaxAttempts(1)}, opts...)
	p, err := New(cfg, all...)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return p
}

// jsonReply is a 200 reply with a JSON body.
func jsonReply(body string) llmtest.Reply {
	return llmtest.Reply{Header: map[string]string{"Content-Type": "application/json"}, Chunks: []string{body}}
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

// failureOf returns the *kernel.ModelError a failed call returned.
func failureOf(t *testing.T, err error) *kernel.ModelError {
	t.Helper()
	var me *kernel.ModelError
	if !errors.As(err, &me) {
		t.Fatalf("the call returned %v (%T), want a *kernel.ModelError", err, err)
	}
	return me
}

type recordedError struct {
	Message   string `json:"message"`
	Code      string `json:"code"`
	Retryable bool   `json:"retryable"`
}

// ---------------------------------------------------------------------------
// Requests
// ---------------------------------------------------------------------------

type recordedRequest struct {
	Method string `json:"method"`
	Path   string `json:"path"`
	Body   string `json:"body"`
}

type requestEntry struct {
	Name        string              `json:"name"`
	Method      string              `json:"method"`
	Region      string              `json:"region"`
	ConfigModel string              `json:"configModel"`
	Messages    []kernel.Message    `json:"messages"`
	Options     llmhub.ModelOptions `json:"options"`
	Request     recordedRequest     `json:"request"`
}

type requestFixture struct {
	Entries []requestEntry `json:"entries"`
}

// divergence is a recorded fixture value that Go deliberately does not reproduce: the
// function turns the recorded text into the text Go must produce. Each replacement
// must find its recorded text, so a golden that changes cannot make a divergence a
// silent no-op.
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

// replaceWith returns a divergence that ignores the recorded text and gives goText.
func replaceWith(goText string) divergence {
	return func(*testing.T, string) string { return goText }
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

// requestPathDivergences are the request paths where the Go model id resolution
// differs from the recorded fixture: an id with a geographic prefix (global., apac.,
// jp., us-gov.) is sent as it is, an ap- region uses apac., a us-gov- region uses
// us-gov., and a region with no inference-profile prefix (ca-central-1) sends the
// raw id. The key is the entry name; it applies to /converse and /converse-stream.
var requestPathDivergences = map[string]divergence{
	"option-model-global-gets-us-prefix":        swap("/model/us.global.", "/model/global."),
	"region-ap-default-model":                   swap("/model/ap.anthropic.", "/model/apac.anthropic."),
	"region-ap-global-model-is-double-prefixed": swap("/model/ap.global.", "/model/global."),
	"region-ap-apac-model-is-double-prefixed":   swap("/model/ap.apac.", "/model/apac."),
	"region-ap-jp-model-is-double-prefixed":     swap("/model/ap.jp.", "/model/jp."),
	"region-gov-uses-us-prefix":                 swap("/model/us.anthropic.", "/model/us-gov.anthropic."),
	"region-gov-model-is-double-prefixed":       swap("/model/us.us-gov.", "/model/us-gov."),
	"region-canada-uses-us-prefix":              swap("/model/us.anthropic.", "/model/anthropic."),
}

// requestBodyDivergences are the request bodies where the Go message mapping differs
// from the recorded fixture, with the body Go must send: an assistant message with
// toolResults keeps its text and toolUse blocks in an assistant message before the
// results, a user message with toolResults keeps its text after them, and an empty
// toolResults list sends the assistant message as itself (the recorded body has an
// empty user content array, which Bedrock rejects).
var requestBodyDivergences = map[string]divergence{
	"assistant-calls-text-and-results-keeps-only-results": replaceWith(`{"inferenceConfig":{"maxTokens":16384},"messages":[{"content":[{"text":"look"}],"role":"user"},{"content":[{"text":"some text"},{"toolUse":{"input":{"a":{"y":[1,2],"z":1},"b":1,"c":1.5,"d":0,"e":null,"f":true},"name":"read_file","toolUseId":"c1"}}],"role":"assistant"},{"content":[{"toolResult":{"content":[{"text":"boom"}],"status":"error","toolUseId":"c1"}}],"role":"user"}]}`),
	"role-user-with-results":                              replaceWith(`{"inferenceConfig":{"maxTokens":16384},"messages":[{"content":[{"toolResult":{"content":[{"text":"ok"}],"status":"success","toolUseId":"c2"}},{"text":"text is dropped"}],"role":"user"}]}`),
	"tool-results-empty-list":                             replaceWith(`{"inferenceConfig":{"maxTokens":16384},"messages":[{"content":[{"text":"look"}],"role":"user"},{"content":[{"text":"kept?"}],"role":"assistant"},{"content":[{"text":"after"}],"role":"user"}]}`),
}

// TestBedrockRequestsMatchGolden sends every corpus entry through the REAL runtime
// client to a loopback server and requires the wire request to equal the one the
// reference provider sent: the same path (the resolved, URL-escaped model id) and
// the same JSON body once decoded. The bodies are compared decoded, not as text:
// the AWS Go encoder writes the keys of a request in alphabetical order, and tool
// inputs and schemas with sorted keys, while the recorded bodies keep input order.
func TestBedrockRequestsMatchGolden(t *testing.T) {
	var f requestFixture
	loadFixture(t, "llm-bedrock-requests.json", &f)
	if len(f.Entries) < 50 {
		t.Fatalf("the request fixture has %d entries, want at least 50", len(f.Entries))
	}
	usedPath, usedBody := map[string]bool{}, map[string]bool{}
	defer checkDivergencesUsed(t, requestPathDivergences, usedPath)
	defer checkDivergencesUsed(t, requestBodyDivergences, usedBody)
	for _, e := range f.Entries {
		t.Run(e.Name+"/"+e.Method, func(t *testing.T) {
			up := llmtest.NewUpstream(t, func(llmtest.Request) llmtest.Reply {
				return llmtest.Reply{
					Status: 400,
					Header: map[string]string{"Content-Type": "application/json", "X-Amzn-Errortype": "ValidationException"},
					Chunks: []string{`{"message":"captured"}`},
				}
			})
			cfg := providerConfig()
			cfg.Region = e.Region
			cfg.Model = e.ConfigModel
			p := realProvider(t, up, cfg)
			switch e.Method {
			case "chat":
				_, err := p.Chat(t.Context(), e.Messages, e.Options)
				if me := failureOf(t, err); me.Code != kernel.ModelCodeInvalidRequest {
					t.Fatalf("the 400 gave code %q, want invalid_request", me.Code)
				}
			case "stream":
				chunks := drain(t, p, e.Messages, e.Options)
				if len(chunks) != 1 || chunks[0].Type != kernel.ChunkError {
					t.Fatalf("the 400 gave chunks %v, want one error chunk", chunks)
				}
			default:
				t.Fatalf("unknown method %q", e.Method)
			}
			reqs := up.Requests()
			if len(reqs) != 1 {
				t.Fatalf("the upstream saw %d requests, want 1", len(reqs))
			}
			got := reqs[0]
			wantPath, wantBody := e.Request.Path, e.Request.Body
			if d, ok := requestPathDivergences[e.Name]; ok {
				usedPath[e.Name] = true
				wantPath = d(t, wantPath)
			}
			if d, ok := requestBodyDivergences[e.Name]; ok {
				usedBody[e.Name] = true
				wantBody = d(t, wantBody)
			}
			if got.Method != e.Request.Method || got.Path != wantPath {
				t.Errorf("request line: got %s %s, want %s %s", got.Method, got.Path, e.Request.Method, wantPath)
			}
			llmtest.AssertJSONEqual(t, got.Body, []byte(wantBody))
		})
	}
}

// ---------------------------------------------------------------------------
// Streams
// ---------------------------------------------------------------------------

type streamEntry struct {
	Name        string            `json:"name"`
	Events      []json.RawMessage `json:"events"`
	StreamError *struct {
		Name    string `json:"name"`
		Message string `json:"message"`
	} `json:"streamError"`
	NoStream bool              `json:"noStream"`
	Chunks   []json.RawMessage `json:"chunks"`
}

type streamFixture struct {
	Entries []streamEntry `json:"entries"`
}

// truncatedStreams are the recorded transcripts that end with neither messageStop
// nor metadata (crafted short transcripts, which a real Bedrock stream never is:
// it ends every answer with both). The recorded chunks end them with a plain done;
// Go treats such a stream as cut short and ends it with one terminal error chunk in
// place of the done.
var truncatedStreams = map[string]bool{
	"no-events":                   true,
	"missing-block-index-is-zero": true,
	"mixed-missing-and-zero-index-share-a-buffer":   true,
	"tool-block-without-deltas-has-empty-arguments": true,
	"tool-start-without-id-or-name":                 true,
	"empty-delta-input-gives-no-progress":           true,
	"delta-without-a-start-is-ignored":              true,
	"block-start-without-a-tool-is-ignored":         true,
	"tool-block-reused-after-stop":                  true,
	"second-start-replaces-an-unfinished-buffer":    true,
	"truncated-arguments":                           true,
	"arguments-that-are-an-array":                   true,
	"arguments-that-are-null":                       true,
	"arguments-in-pieces-with-whitespace":           true,
	"special-characters":                            true,
}

// truncatedEnding replaces the final done of the recorded chunks with the Go error chunk.
func truncatedEnding(t *testing.T, ts []string) []string {
	t.Helper()
	if len(ts) == 0 || !strings.HasPrefix(ts[len(ts)-1], `{"type":"done"`) {
		t.Fatalf("the TS transcript does not end with done: %v", ts)
	}
	return append(slices.Clone(ts[:len(ts)-1]), `{"type":"error","error":"`+errTruncated+`"}`)
}

const notValidJSONPrefix = "the arguments are not valid JSON ("

// tsPreview is the preview rule of the arguments error, written independently of
// the code under test: the first 200 UTF-16 units of raw, then an ellipsis when it
// is longer.
func tsPreview(raw string) string {
	units := utf16.Encode([]rune(raw))
	if len(units) <= 200 {
		return raw
	}
	return string(utf16.Decode(units[:200])) + "\u2026"
}

// withoutJSONReasons applies the documented exception of the arguments error: the
// "not valid JSON (<reason>)" text has a reason that is the JSON parser's own (V8's
// message in the recorded fixture, encoding/json's in Go), so the reason is
// replaced by a marker in both chunk lists after checking that the text has the
// expected prefix and ends with "): " plus the preview of the arguments text that was
// streamed for the call. Everything else must match byte for byte.
//
// The streamed text of a call is rebuilt from its tool_call_delta chunks.
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

// awsError builds an error the way the Go SDK delivers one, from the name, message
// and status of a recorded error: the typed exception of the runtime for the names that
// have one, the generic API error for any other name, a plain error for an unnamed
// one; wrapped in an awshttp.ResponseError when there is a status, and in a
// smithy.OperationError as every operation error is.
func awsError(name, message string, status int) error {
	var inner error
	switch name {
	case "", "Error":
		inner = errors.New(message)
	case "AccessDeniedException":
		inner = &types.AccessDeniedException{Message: aws.String(message)}
	case "ThrottlingException":
		inner = &types.ThrottlingException{Message: aws.String(message)}
	case "ValidationException":
		inner = &types.ValidationException{Message: aws.String(message)}
	case "InternalServerException":
		inner = &types.InternalServerException{Message: aws.String(message)}
	case "ModelTimeoutException":
		inner = &types.ModelTimeoutException{Message: aws.String(message)}
	case "ModelStreamErrorException":
		inner = &types.ModelStreamErrorException{Message: aws.String(message)}
	default:
		inner = &smithy.GenericAPIError{Code: name, Message: message, Fault: smithy.FaultClient}
	}
	if status != 0 {
		inner = &awshttp.ResponseError{
			ResponseError: &smithyhttp.ResponseError{
				Response: &smithyhttp.Response{Response: &http.Response{StatusCode: status}},
				Err:      inner,
			},
			RequestID: "req-1",
		}
	}
	if status == 0 && (name == "" || name == "Error") {
		// A failure with no status and no AWS name (a dropped connection) carries its
		// own text in both languages; it is not an operation error of the SDK here.
		return inner
	}
	return &smithy.OperationError{ServiceID: "Bedrock Runtime", OperationName: "Converse", Err: inner}
}

// TestBedrockStreamsMatchGolden replays each recorded event array through an injected
// event stream and requires the chunks to equal the recorded chunks, one by one: two
// interleaved tool-call blocks (the regression transcript), invalid arguments, a
// missing block index, usage in every shape, a response with no stream, and an
// error after some events.
func TestBedrockStreamsMatchGolden(t *testing.T) {
	var f streamFixture
	loadFixture(t, "llm-bedrock-streams.json", &f)
	if len(f.Entries) < 25 {
		t.Fatalf("the stream fixture has %d entries, want at least 25", len(f.Entries))
	}
	used := map[string]bool{}
	defer checkDivergencesUsed(t, truncatedStreams, used)
	for _, e := range f.Entries {
		t.Run(e.Name, func(t *testing.T) {
			var api *fakeAPI
			switch {
			case e.NoStream:
				api = &fakeAPI{stream: func(context.Context, *bedrockruntime.ConverseStreamInput) (EventStream, error) { return nil, nil }}
			case e.StreamError != nil:
				api = streamOf(eventsFromFixture(t, e.Events), awsError(e.StreamError.Name, e.StreamError.Message, 0))
			default:
				api = streamOf(eventsFromFixture(t, e.Events), nil)
			}
			p := newFakeProvider(t, api)
			got := llmtest.ChunkTexts(t, drain(t, p, []kernel.Message{{Role: kernel.RoleUser, Content: "work"}}, llmhub.ModelOptions{}))
			want := make([]string, len(e.Chunks))
			for i, c := range e.Chunks {
				want[i] = string(c)
			}
			if truncatedStreams[e.Name] {
				used[e.Name] = true
				want = truncatedEnding(t, want)
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
	Name     string          `json:"name"`
	Response json.RawMessage `json:"response"`
	Result   json.RawMessage `json:"result"`
}

type chatFixture struct {
	Entries []chatEntry `json:"entries"`
}

// chatExceptions names the recorded chat entries where Go differs from the fixture
// on purpose, with the response Go must produce instead (written here, not derived
// from the recorded one). Every key must be used by an entry: a stale exception fails the
// test.
var chatExceptions = map[string]string{
	// The fixture passes an input that is an array through as the arguments. Go's
	// Arguments is always one JSON object, so the call carries empty arguments and
	// an ArgumentsError, as every provider does for an input that is not an object
	// (see ParseToolArguments).
	"tool-use-with-array-input": `{"message":{"role":"assistant","content":"","toolCalls":[{"id":"tu_1","name":"ping","arguments":{},"argumentsError":"the arguments must be one JSON object: [1,2]"}]},"usage":{"inputTokens":11,"outputTokens":7},"stopReason":"tool_use"}`,
}

// chatDivergences are the recorded answers where the Go stop-reason mapping differs
// from the fixture: stop_sequence, an unknown reason and an absent reason (also the
// one of an empty response) are end_turn, where the fixture records error.
var chatDivergences = map[string]divergence{
	"empty-response":              swap(`"stopReason":"error"`, `"stopReason":"end_turn"`),
	"stop-stop-sequence-is-error": swap(`"stopReason":"error"`, `"stopReason":"end_turn"`),
	"stop-unknown-is-error":       swap(`"stopReason":"error"`, `"stopReason":"end_turn"`),
	"stop-absent-is-error":        swap(`"stopReason":"error"`, `"stopReason":"end_turn"`),
}

// TestBedrockChatMatchesGolden serves each recorded Converse response through the REAL
// runtime client and requires the ModelResponse to equal the recorded one: text blocks
// joined, tool uses with an id and name of "" when missing, a missing or null input
// as {}, numbers read as doubles, usage or 0, no cache counts, and the stop reason
// mapped to the three known values or "error", except for the named Go
// divergences (chatDivergences).
func TestBedrockChatMatchesGolden(t *testing.T) {
	var f chatFixture
	loadFixture(t, "llm-bedrock-chat.json", &f)
	if len(f.Entries) < 20 {
		t.Fatalf("the chat fixture has %d entries, want at least 20", len(f.Entries))
	}
	used := map[string]bool{}
	divergent := map[string]bool{}
	defer checkDivergencesUsed(t, chatDivergences, divergent)
	for _, e := range f.Entries {
		t.Run(e.Name, func(t *testing.T) {
			up := llmtest.NewUpstream(t, func(llmtest.Request) llmtest.Reply { return jsonReply(string(e.Response)) })
			p := realProvider(t, up, providerConfig())
			res, err := p.Chat(t.Context(), []kernel.Message{{Role: kernel.RoleUser, Content: "hi"}}, llmhub.ModelOptions{})
			if err != nil {
				t.Fatalf("Chat: %v", err)
			}
			got, err := kernel.EncodeJSON(res)
			if err != nil {
				t.Fatalf("encode the response: %v", err)
			}
			want := string(e.Result)
			if exc, ok := chatExceptions[e.Name]; ok {
				used[e.Name] = true
				want = exc
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
	for name := range chatExceptions {
		if !used[name] {
			t.Errorf("chat exception %q is used by no entry", name)
		}
	}
}

// ---------------------------------------------------------------------------
// Errors, region prefixes, model ids
// ---------------------------------------------------------------------------

type errorEntry struct {
	Name   string `json:"name"`
	Method string `json:"method"`
	Model  string `json:"model"`
	Error  struct {
		Name    string `json:"name"`
		Message string `json:"message"`
		Status  int    `json:"status"`
	} `json:"error"`
	Out json.RawMessage `json:"out"`
}

type errorFixture struct {
	Entries      []errorEntry `json:"entries"`
	RegionPrefix []struct {
		Region string `json:"region"`
		Prefix string `json:"prefix"`
	} `json:"regionPrefix"`
	ModelIDs []struct {
		Region   string `json:"region"`
		ID       string `json:"id"`
		Resolved string `json:"resolved"`
	} `json:"modelIds"`
}

// TestBedrockErrorsMatchGolden rebuilds each crafted recorded error in Go from real
// smithy and awshttp types and requires the ModelError to equal the recorded
// mapping: message, code and retryability, for chat, for the in-band error chunk of
// stream, and for listModels (which names no model). This is the error-mapping
// table and more.
func TestBedrockErrorsMatchGolden(t *testing.T) {
	var f errorFixture
	loadFixture(t, "llm-bedrock-errors.json", &f)
	if len(f.Entries) < 30 {
		t.Fatalf("the error fixture has %d entries, want at least 30", len(f.Entries))
	}
	for _, e := range f.Entries {
		t.Run(e.Name, func(t *testing.T) {
			cfg := providerConfig()
			cfg.Model = e.Model
			failure := awsError(e.Error.Name, e.Error.Message, e.Error.Status)
			var conv ConverseAPI = &fakeAPI{}
			var models ModelsAPI = fakeModels{}
			switch e.Method {
			case "chat":
				conv = &fakeAPI{converse: func(context.Context, *bedrockruntime.ConverseInput) (*bedrockruntime.ConverseOutput, error) {
					return nil, failure
				}}
			case "stream":
				conv = &fakeAPI{stream: func(context.Context, *bedrockruntime.ConverseStreamInput) (EventStream, error) {
					return nil, failure
				}}
			case "listModels":
				models = fakeModels{list: func(context.Context, *bedrock.ListFoundationModelsInput) (*bedrock.ListFoundationModelsOutput, error) {
					return nil, failure
				}}
			default:
				t.Fatalf("unknown method %q", e.Method)
			}
			p, err := New(cfg, WithConverseAPI(conv), WithModelsAPI(models))
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			switch e.Method {
			case "chat":
				var want recordedError
				if err := json.Unmarshal(e.Out, &want); err != nil {
					t.Fatalf("decode the expected error: %v", err)
				}
				_, err := p.Chat(t.Context(), []kernel.Message{{Role: kernel.RoleUser, Content: "hey"}}, llmhub.ModelOptions{})
				me := failureOf(t, err)
				if got := (recordedError{Message: me.Message, Code: string(me.Code), Retryable: me.Retryable}); got != want {
					t.Errorf("error differs\n got: %+v\nwant: %+v", got, want)
				}
			case "listModels":
				var want recordedError
				if err := json.Unmarshal(e.Out, &want); err != nil {
					t.Fatalf("decode the expected error: %v", err)
				}
				_, err := p.ListModels(t.Context())
				me := failureOf(t, err)
				if got := (recordedError{Message: me.Message, Code: string(me.Code), Retryable: me.Retryable}); got != want {
					t.Errorf("error differs\n got: %+v\nwant: %+v", got, want)
				}
			case "stream":
				var want struct {
					Chunks []json.RawMessage `json:"chunks"`
				}
				if err := json.Unmarshal(e.Out, &want); err != nil {
					t.Fatalf("decode the expected chunks: %v", err)
				}
				got := llmtest.ChunkTexts(t, drain(t, p, []kernel.Message{{Role: kernel.RoleUser, Content: "hey"}}, llmhub.ModelOptions{}))
				wantTexts := make([]string, len(want.Chunks))
				for i, c := range want.Chunks {
					wantTexts[i] = string(c)
				}
				if !slices.Equal(got, wantTexts) {
					t.Errorf("chunks differ\n got: %v\nwant: %v", got, wantTexts)
				}
			}
		})
	}
}

// goRegionPrefixes are the regions of the recorded table where Go gives another
// prefix: us-gov- regions use us-gov., ap- regions use apac. ("ap." is no Bedrock
// prefix), the region is compared without regard to case, and a region with no known
// inference-profile prefix (ca-, sa-, me-, af-, cn-, a partial or empty name) gets
// none instead of "us.".
var goRegionPrefixes = map[string]string{
	"us-gov-west-1":  "us-gov.",
	"ap-southeast-2": "apac.",
	"ap-northeast-1": "apac.",
	"ap-":            "apac.",
	"ca-central-1":   "",
	"sa-east-1":      "",
	"me-south-1":     "",
	"af-south-1":     "",
	"cn-north-1":     "",
	"Eu-West-1":      "eu.",
	"us":             "",
	"":               "",
}

// goResolvedModelID is the Go rule of resolveModelID, written independently of the
// code under test: an ARN, the empty string and an id with a geographic prefix are
// kept as they are, whatever their case; any other id gets the Go prefix of the
// region (goRegionPrefixes, else the recorded one).
func goResolvedModelID(region, tsPrefix, id string) string {
	lower := strings.ToLower(id)
	if id == "" || strings.HasPrefix(lower, "arn:") {
		return id
	}
	for _, geo := range []string{"us.", "us-gov.", "eu.", "apac.", "ap.", "jp.", "au.", "ca.", "global."} {
		if strings.HasPrefix(lower, geo) {
			return id
		}
	}
	prefix, ok := goRegionPrefixes[region]
	if !ok {
		prefix = tsPrefix
	}
	return prefix + id
}

// TestBedrockRegionPrefixMatchesGolden compares deriveRegionPrefix with the recorded
// prefixes over eighteen regions (including upper-case, partial and empty ones), and
// resolveModelID with the recorded ids over seventeen ids in five regions. The
// recorded fixture keeps only the ids that already carry us., eu., ap. or an ARN and
// gives every other id, global., apac., jp. and us-gov. ones included, a second
// prefix; Go keeps every id with a geographic prefix and maps the regions as
// goRegionPrefixes says, so those entries are named divergences: the expected Go
// value comes from goRegionPrefixes and goResolvedModelID, and the other entries
// are compared with the recorded value.
func TestBedrockRegionPrefixMatchesGolden(t *testing.T) {
	var f errorFixture
	loadFixture(t, "llm-bedrock-errors.json", &f)
	if len(f.RegionPrefix) < 15 || len(f.ModelIDs) < 50 {
		t.Fatalf("the fixture has %d regions and %d model ids", len(f.RegionPrefix), len(f.ModelIDs))
	}
	tsPrefix := map[string]string{}
	usedRegions := map[string]bool{}
	for _, r := range f.RegionPrefix {
		tsPrefix[r.Region] = r.Prefix
		want := r.Prefix
		if goWant, ok := goRegionPrefixes[r.Region]; ok {
			usedRegions[r.Region] = true
			want = goWant
		}
		if got := deriveRegionPrefix(r.Region); got != want {
			t.Errorf("deriveRegionPrefix(%q) = %q, want %q (TS gives %q)", r.Region, got, want, r.Prefix)
		}
	}
	checkDivergencesUsed(t, goRegionPrefixes, usedRegions)
	divergent := 0
	providers := map[string]*Provider{}
	for _, m := range f.ModelIDs {
		p, ok := providers[m.Region]
		if !ok {
			cfg := providerConfig()
			cfg.Region = m.Region
			var err error
			p, err = New(cfg, WithConverseAPI(&fakeAPI{}), WithModelsAPI(fakeModels{}))
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			providers[m.Region] = p
		}
		want := goResolvedModelID(m.Region, tsPrefix[m.Region], m.ID)
		if want != m.Resolved {
			divergent++
		}
		if got := p.resolveModelID(m.ID); got != want {
			t.Errorf("resolveModelID(%q) in %s = %q, want %q (TS gives %q)", m.ID, m.Region, got, want, m.Resolved)
		}
	}
	if divergent == 0 {
		t.Error("no model id differs from TS: the Q11 divergence is stale")
	}
}

// ---------------------------------------------------------------------------
// ListModels
// ---------------------------------------------------------------------------

type modelsEntry struct {
	Name         string          `json:"name"`
	ProviderName string          `json:"providerName"`
	Wire         string          `json:"wire"`
	Result       json.RawMessage `json:"result"`
}

type modelsFixture struct {
	Entries []modelsEntry `json:"entries"`
}

// TestBedrockListModelsMatchesGolden serves each recorded ListFoundationModels body
// through the REAL control-plane client and requires the []llmhub.ModelInfo to equal
// the recorded listing: entries without a modelId dropped, the mapped metadata, and Raw
// (built by Go from the typed summary, in the member order of the AWS
// documentation, which is the order the recorded responses use).
func TestBedrockListModelsMatchesGolden(t *testing.T) {
	var f modelsFixture
	loadFixture(t, "llm-bedrock-models.json", &f)
	if len(f.Entries) < 8 {
		t.Fatalf("the models fixture has %d entries, want at least 8", len(f.Entries))
	}
	for _, e := range f.Entries {
		t.Run(e.Name, func(t *testing.T) {
			up := llmtest.NewUpstream(t, func(llmtest.Request) llmtest.Reply { return jsonReply(e.Wire) })
			var opts []Option
			if e.ProviderName != "" {
				opts = append(opts, WithName(e.ProviderName))
			}
			p := realProvider(t, up, providerConfig(), opts...)
			models, err := p.ListModels(t.Context())
			if err != nil {
				t.Fatalf("ListModels: %v", err)
			}
			if reqs := up.Requests(); len(reqs) != 1 || reqs[0].Method != http.MethodGet || reqs[0].Path != "/foundation-models" {
				t.Errorf("requests: %+v, want one GET /foundation-models", reqs)
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

// TestBedrockFixturesCovered fails when a fixture with the llm-bedrock- prefix has no
// test above, and when a listed fixture has no file.
func TestBedrockFixturesCovered(t *testing.T) {
	entries, err := os.ReadDir(wiretest.Dir(t))
	if err != nil {
		t.Fatalf("read fixture dir: %v", err)
	}
	present := map[string]bool{}
	for _, e := range entries {
		present[e.Name()] = true
		if strings.HasPrefix(e.Name(), "llm-bedrock-") && !slices.Contains(bedrockFixtures, e.Name()) {
			t.Errorf("fixture %s has no entry in bedrockFixtures", e.Name())
		}
	}
	for _, name := range bedrockFixtures {
		if !present[name] {
			t.Errorf("bedrockFixtures entry %s has no fixture file", name)
		}
	}
}
