package toolhub_test

// The tracer test of Phase 5: one scripted provider stream carries one tool
// call, whose bytes flow through kernel.NewArguments, ToolHub.Authorize with the
// default policy, Authorized.Execute and kernel.EncodeJSON. The bytes are
// checked at every stage (TLS-07) and a denied call is shown never to reach the
// handler (TLS-01).
//
// Special characters are built from numeric bytes: this source contains no
// non-ASCII text and no backslash-u escape text.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"iter"
	"sync/atomic"
	"testing"

	"github.com/Phong95/aec-agent-go/components/llmhub"
	"github.com/Phong95/aec-agent-go/components/toolhub"
	"github.com/Phong95/aec-agent-go/kernel"
)

// lineSeparator is U+2028 as raw UTF-8 bytes.
var lineSeparator = []byte{0xE2, 0x80, 0xA8}

// rawModelArguments is what a model might send: insignificant whitespace, a
// deliberately non-sorted key order, raw < > &, an escaped quote, a raw U+2028
// and number spellings Go would not write itself.
func rawModelArguments() []byte {
	var b bytes.Buffer
	b.WriteString("{ \"zeta\" : 0,\n  \"alpha\" : { \"b\" : false , \"a\" : \"\" },\n")
	b.WriteString("  \"html\" : \"<b>&\\\"x\\\"</b>\",\n")
	b.WriteString("  \"sep\" : \"")
	b.Write(lineSeparator)
	b.WriteString("\",\n  \"n\" : 1.50,\n  \"big\" : 1e21,\n  \"list\" : [ 3, 1, 2 ] }")
	return b.Bytes()
}

// compactModelArguments is the same value with insignificant whitespace
// removed, written out independently of json.Compact.
func compactModelArguments() []byte {
	var b bytes.Buffer
	b.WriteString("{\"zeta\":0,\"alpha\":{\"b\":false,\"a\":\"\"},")
	b.WriteString("\"html\":\"<b>&\\\"x\\\"</b>\",")
	b.WriteString("\"sep\":\"")
	b.Write(lineSeparator)
	b.WriteString("\",\"n\":1.50,\"big\":1e21,\"list\":[3,1,2]}")
	return b.Bytes()
}

// scriptedProvider is a test-local llmhub.Provider that streams one tool call.
type scriptedProvider struct {
	raw []byte
}

func (*scriptedProvider) Name() string    { return "provider:scripted" }
func (*scriptedProvider) Version() string { return "1.0.0" }

func (*scriptedProvider) Chat(context.Context, []kernel.Message, llmhub.ModelOptions) (kernel.ModelResponse, error) {
	return kernel.ModelResponse{}, errors.New("not used")
}

func (*scriptedProvider) CountTokens(context.Context, []kernel.Message) (int, error) { return 0, nil }

func (p *scriptedProvider) Stream(ctx context.Context, _ []kernel.Message, _ llmhub.ModelOptions) iter.Seq2[kernel.StreamChunk, error] {
	return func(yield func(kernel.StreamChunk, error) bool) {
		emit := func(c kernel.StreamChunk) bool {
			if err := ctx.Err(); err != nil {
				yield(kernel.StreamChunk{}, err)
				return false
			}
			return yield(c, nil)
		}
		if !emit(kernel.StreamChunk{Type: kernel.ChunkToolCallStart, ToolCallProgress: &kernel.ToolCallProgress{ID: "call_1", Name: "capture"}}) {
			return
		}
		if !emit(kernel.StreamChunk{Type: kernel.ChunkToolCallDelta, ToolCallProgress: &kernel.ToolCallProgress{ID: "call_1", Name: "capture", ArgumentsDelta: string(p.raw[:10])}}) {
			return
		}
		args, err := kernel.NewArguments(p.raw)
		if err != nil {
			emit(kernel.StreamChunk{Type: kernel.ChunkError, Error: err.Error()})
			return
		}
		if !emit(kernel.StreamChunk{Type: kernel.ChunkToolCall, ToolCall: &kernel.ToolCall{ID: "call_1", Name: "capture", Arguments: args}}) {
			return
		}
		emit(kernel.StreamChunk{Type: kernel.ChunkDone})
	}
}

var _ llmhub.Provider = (*scriptedProvider)(nil)

// captureHandler records the arguments it receives and counts its calls.
type captureHandler struct {
	calls    atomic.Int32
	received kernel.Arguments
}

func (*captureHandler) Name() string    { return "tool:capture" }
func (*captureHandler) Version() string { return "1.0.0" }

func (*captureHandler) Definition() toolhub.Definition {
	num := toolhub.JSONSchema{Type: "number"}
	str := toolhub.JSONSchema{Type: "string"}
	return toolhub.Definition{
		Name:        "capture",
		Description: "Records the arguments it receives",
		Parameters: toolhub.JSONSchema{
			Type: "object",
			Properties: toolhub.NewProperties(
				toolhub.Property{Name: "zeta", Schema: num},
				toolhub.Property{Name: "alpha", Schema: toolhub.JSONSchema{Type: "object"}},
				toolhub.Property{Name: "html", Schema: str},
				toolhub.Property{Name: "sep", Schema: str},
				toolhub.Property{Name: "n", Schema: num},
				toolhub.Property{Name: "big", Schema: num},
				toolhub.Property{Name: "list", Schema: toolhub.JSONSchema{Type: "array", Items: &num}},
			),
			Required: []string{"zeta"},
		},
	}
}

func (h *captureHandler) Execute(_ context.Context, args kernel.Arguments, _ toolhub.Context) (toolhub.Output, error) {
	h.calls.Add(1)
	h.received = bytes.Clone(args)
	return toolhub.Output{Content: "ok"}, nil
}

// collectToolCall consumes the stream, failing on any error, and returns the
// tool_call chunk's call.
func collectToolCall(t *testing.T, p llmhub.Provider) kernel.ToolCall {
	t.Helper()
	var call *kernel.ToolCall
	done := 0
	for chunk, err := range p.Stream(t.Context(), nil, llmhub.ModelOptions{}) {
		if err != nil {
			t.Fatalf("stream yielded an error: %v", err)
		}
		if chunk.Type == kernel.ChunkError {
			t.Fatalf("stream yielded an error chunk: %s", chunk.Error)
		}
		if done > 0 {
			t.Fatalf("chunk %q after done", chunk.Type)
		}
		switch chunk.Type {
		case kernel.ChunkToolCall:
			call = chunk.ToolCall
		case kernel.ChunkDone:
			done++
		default:
		}
	}
	if call == nil || done != 1 {
		t.Fatalf("want one tool_call and one done chunk, got call=%v done=%d", call, done)
	}
	return *call
}

func TestExactArgumentBytesEndToEnd(t *testing.T) {
	want := compactModelArguments()
	call := collectToolCall(t, &scriptedProvider{raw: rawModelArguments()})

	// Stage 1: the provider boundary compacts and keeps every byte that matters.
	if !bytes.Equal(call.Arguments, want) {
		t.Fatalf("NewArguments changed the bytes\n got: %s\nwant: %s", call.Arguments, want)
	}
	for name, needle := range map[string][]byte{
		"raw <":         []byte("<"),
		"raw >":         []byte(">"),
		"raw &":         []byte("&"),
		"raw U+2028":    lineSeparator,
		"1.50 as sent":  []byte("1.50"),
		"1e21 as sent":  []byte("1e21"),
		"zero number":   []byte(`"zeta":0,`),
		"false":         []byte(`"b":false`),
		"empty string":  []byte(`"a":""`),
		"list in order": []byte(`[3,1,2]`),
	} {
		if !bytes.Contains(call.Arguments, needle) {
			t.Errorf("compacted arguments lost %s: %s", name, call.Arguments)
		}
	}
	if bytes.Contains(call.Arguments, []byte("\\"+"u003c")) {
		t.Errorf("compacted arguments HTML-escaped <: %s", call.Arguments)
	}
	if bytes.Index(call.Arguments, []byte(`"zeta"`)) > bytes.Index(call.Arguments, []byte(`"alpha"`)) {
		t.Errorf("key order changed: %s", call.Arguments)
	}

	// Stage 2: policy-gated execution hands the handler the very same bytes.
	handler := &captureHandler{}
	hub := toolhub.NewToolHub()
	if err := hub.Register(handler); err != nil {
		t.Fatalf("register: %v", err)
	}
	authorized, decision, err := hub.Authorize(t.Context(), call.Name, call.Arguments, toolhub.Context{SessionID: "s1"}, []string{"capture"})
	if err != nil {
		t.Fatalf("authorize: %v", err)
	}
	if !decision.Allowed {
		reason := ""
		if decision.Reason != nil {
			reason = *decision.Reason
		}
		t.Fatalf("policy denied the call: %s", reason)
	}
	out, err := authorized.Execute(t.Context())
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if handler.calls.Load() != 1 {
		t.Fatalf("handler ran %d times, want 1", handler.calls.Load())
	}
	if !bytes.Equal(handler.received, want) || !bytes.Equal(handler.received, call.Arguments) {
		t.Fatalf("handler received different bytes\n got: %s\nwant: %s", handler.received, want)
	}

	// Stage 3: the persisted messages carry the same bytes.
	assistant := kernel.Message{Role: kernel.RoleAssistant, ToolCalls: []kernel.ToolCall{call}}
	encoded, err := kernel.EncodeJSON(assistant)
	if err != nil {
		t.Fatalf("encode assistant message: %v", err)
	}
	if !bytes.Contains(encoded, append([]byte(`"arguments":`), want...)) {
		t.Fatalf("persisted message does not hold the exact argument bytes\n got: %s", encoded)
	}
	var decoded kernel.Message
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("decode persisted message: %v", err)
	}
	if len(decoded.ToolCalls) != 1 || !bytes.Equal(decoded.ToolCalls[0].Arguments, want) {
		t.Fatalf("round trip changed the arguments: %+v", decoded.ToolCalls)
	}
	result := kernel.Message{Role: kernel.RoleTool, ToolResults: []kernel.ToolResult{{ToolCallID: call.ID, Content: out.Content}}}
	if _, err := kernel.EncodeJSON(result); err != nil {
		t.Fatalf("encode tool message: %v", err)
	}
}

func TestDeniedCallNeverExecutes(t *testing.T) {
	call := collectToolCall(t, &scriptedProvider{raw: rawModelArguments()})
	handler := &captureHandler{}
	hub := toolhub.NewToolHub()
	if err := hub.Register(handler); err != nil {
		t.Fatalf("register: %v", err)
	}

	authorized, decision, err := hub.Authorize(t.Context(), call.Name, call.Arguments, toolhub.Context{}, []string{"other"})
	if err != nil {
		t.Fatalf("authorize: %v", err)
	}
	if decision.Allowed {
		t.Fatal("the allowlist must deny a tool that is not listed")
	}
	if decision.Reason == nil || *decision.Reason != `Tool "capture" is not in the allowed list` {
		t.Fatalf("unexpected denial reason: %v", decision.Reason)
	}
	if handler.calls.Load() != 0 {
		t.Fatalf("handler ran %d times after a denial", handler.calls.Load())
	}

	if _, err := authorized.Execute(t.Context()); !errors.Is(err, toolhub.ErrNotAuthorized) {
		t.Fatalf("Execute on the zero Authorized: want ErrNotAuthorized, got %v", err)
	}
	if handler.calls.Load() != 0 {
		t.Fatalf("handler ran %d times through the zero Authorized", handler.calls.Load())
	}
}

// TestEscapedLineSeparatorsEndToEnd pins WR-01: a model that sends U+2028 and
// U+2029 as 6-character escapes must not see its handler receive different bytes
// from the ones that are persisted. The escapes are normalised once, at the
// provider boundary, to the raw characters JSON.stringify writes; an escaped
// backslash followed by the text u2028 is a different value and stays untouched.
func TestEscapedLineSeparatorsEndToEnd(t *testing.T) {
	bs := "\\" // one backslash; this source holds no backslash-u escape text
	paraSeparator := []byte{0xE2, 0x80, 0xA9}
	var raw, want bytes.Buffer
	raw.WriteString(`{"zeta":0,"sep":"a` + bs + `u2028b","html":"` + bs + `u2029","lit":"` + bs + bs + `u2028"}`)
	want.WriteString(`{"zeta":0,"sep":"a`)
	want.Write(lineSeparator)
	want.WriteString(`b","html":"`)
	want.Write(paraSeparator)
	want.WriteString(`","lit":"` + bs + bs + `u2028"}`)

	call := collectToolCall(t, &scriptedProvider{raw: raw.Bytes()})
	if !bytes.Equal(call.Arguments, want.Bytes()) {
		t.Fatalf("NewArguments\n got: %q\nwant: %q", call.Arguments, want.Bytes())
	}

	handler := &captureHandler{}
	hub := toolhub.NewToolHub()
	if err := hub.Register(handler); err != nil {
		t.Fatal(err)
	}
	authorized, decision, err := hub.Authorize(t.Context(), call.Name, call.Arguments, toolhub.Context{}, []string{"capture"})
	if err != nil || !decision.Allowed {
		t.Fatalf("authorize: %+v %v", decision, err)
	}
	if _, err := authorized.Execute(t.Context()); err != nil {
		t.Fatal(err)
	}

	encoded, err := kernel.EncodeJSON(kernel.Message{Role: kernel.RoleAssistant, ToolCalls: []kernel.ToolCall{call}})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(encoded, append([]byte(`"arguments":`), handler.received...)) {
		t.Fatalf("persisted bytes differ from the executed bytes\n executed: %q\n persisted: %q", handler.received, encoded)
	}
	var decoded kernel.Message
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(decoded.ToolCalls[0].Arguments, handler.received) {
		t.Fatalf("round trip changed the bytes: %q vs %q", decoded.ToolCalls[0].Arguments, handler.received)
	}
}
