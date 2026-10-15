package kernel

import (
	"bytes"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/Phong95/aec-agent-go/internal/jsonx"
	"github.com/Phong95/aec-agent-go/internal/wiretest"
	"github.com/Phong95/aec-agent-go/kernel/ordered"
)

// wireCases maps every kernel golden fixture to the Go type it must round-trip
// through. newValue returns a pointer to a zero Go value.
var wireCases = []struct {
	file     string
	newValue func() any
}{
	{"token-usage-basic.json", func() any { return new(TokenUsage) }},
	{"token-usage-cache-zero.json", func() any { return new(TokenUsage) }},
	{"token-usage-cache-set.json", func() any { return new(TokenUsage) }},
	{"message-user.json", func() any { return new(Message) }},
	{"message-assistant-toolcalls.json", func() any { return new(Message) }},
	{"message-tool-results.json", func() any { return new(Message) }},
	{"message-toolcalls-empty-array.json", func() any { return new(Message) }},
	{"message-edge-strings.json", func() any { return new(Message) }},
	{"message-assistant-toolcalls-argerror.json", func() any { return new(Message) }},
	{"tool-call.json", func() any { return new(ToolCall) }},
	{"tool-call-arguments-error.json", func() any { return new(ToolCall) }},
	{"tool-result.json", func() any { return new(ToolResult) }},
	{"stream-chunk-text.json", func() any { return new(StreamChunk) }},
	{"stream-chunk-text-empty.json", func() any { return new(StreamChunk) }},
	{"stream-chunk-tool-call-start.json", func() any { return new(StreamChunk) }},
	{"stream-chunk-tool-call-delta.json", func() any { return new(StreamChunk) }},
	{"stream-chunk-tool-call.json", func() any { return new(StreamChunk) }},
	{"stream-chunk-tool-result.json", func() any { return new(StreamChunk) }},
	{"stream-chunk-done.json", func() any { return new(StreamChunk) }},
	{"stream-chunk-done-usage.json", func() any { return new(StreamChunk) }},
	{"stream-chunk-done-aborted.json", func() any { return new(StreamChunk) }},
	{"stream-chunk-done-stop-reason.json", func() any { return new(StreamChunk) }},
	{"stream-chunk-error.json", func() any { return new(StreamChunk) }},
	{"model-response.json", func() any { return new(ModelResponse) }},
	{"session-minimal.json", func() any { return new(Session) }},
	{"session-full.json", func() any { return new(Session) }},
	{"event-basic.json", func() any { return new(Event) }},
}

func TestWireFixtures(t *testing.T) {
	for _, tc := range wireCases {
		t.Run(strings.TrimSuffix(tc.file, ".json"), func(t *testing.T) {
			wiretest.AssertRoundTrip(t, tc.file, tc.newValue())
		})
	}
}

// kernelFixturePrefixes are the file name prefixes owned by package kernel.
var kernelFixturePrefixes = []string{
	"token-usage-", "message-", "tool-call", "tool-result", "stream-chunk-",
	"model-response", "session-", "event-basic",
}

// TestKernelFixturesCovered fails if a kernel-owned fixture has no table entry,
// so a fixture added to wiregen cannot go unchecked.
func TestKernelFixturesCovered(t *testing.T) {
	entries, err := os.ReadDir(wiretest.Dir(t))
	if err != nil {
		t.Fatalf("read fixture dir: %v", err)
	}
	covered := map[string]bool{}
	for _, tc := range wireCases {
		covered[tc.file] = true
	}
	for _, e := range entries {
		for _, p := range kernelFixturePrefixes {
			if strings.HasPrefix(e.Name(), p) && !covered[e.Name()] {
				t.Errorf("fixture %s has no entry in wireCases", e.Name())
			}
		}
	}
	for f := range covered {
		if _, err := os.Stat(wiretest.Dir(t) + "/" + f); err != nil {
			t.Errorf("wireCases entry %s has no fixture file: %v", f, err)
		}
	}
}

func TestEncodeJSONMatchesJsonx(t *testing.T) {
	in := map[string]string{"s": `<a>&` + string([]byte{0xe2, 0x80, 0xa8})}
	want, err := jsonx.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	got, err := EncodeJSON(in)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("EncodeJSON differs from jsonx.Marshal: %q vs %q", got, want)
	}
}

func mustEncode(t *testing.T, v any) string {
	t.Helper()
	b, err := EncodeJSON(v)
	if err != nil {
		t.Fatalf("EncodeJSON(%T): %v", v, err)
	}
	return string(b)
}

// A zero Metadata object must encode as {} (the field is required), never null.
func TestSessionNilMetadata(t *testing.T) {
	got := mustEncode(t, Session{ID: "s"})
	want := `{"id":"s","metadata":{},"createdAt":0,"updatedAt":0}`
	if got != want {
		t.Fatalf("got %s, want %s", got, want)
	}
	// Also through a pointer.
	if got := mustEncode(t, &Session{ID: "s"}); got != want {
		t.Fatalf("pointer: got %s want %s", got, want)
	}
}

// Session metadata keeps the authored (non-sorted) key order at every nesting
// level, so a persisted record is byte-identical to the recorded fixture.
func TestSessionMetadataKeyOrder(t *testing.T) {
	var s Session
	wiretest.AssertRoundTrip(t, "session-full.json", &s)
	if got := s.Metadata.Keys(); !slices.Equal(got, []string{"tags", "agentId", "nested"}) {
		t.Fatalf("metadata keys = %v, want [tags agentId nested]", got)
	}
	v, _ := s.Metadata.Get("nested")
	nested, ok := v.(ordered.Object)
	if !ok || !slices.Equal(nested.Keys(), []string{"k", "b"}) {
		t.Fatalf("nested = %#v, want an ordered.Object with keys [k b]", v)
	}
	w, _ := nested.Get("b")
	inner, ok := w.(ordered.Object)
	if !ok || !slices.Equal(inner.Keys(), []string{"y", "x"}) {
		t.Fatalf("nested.b = %#v, want an ordered.Object with keys [y x]", w)
	}
}

// SessionFilter drops an absent metadata object (omitzero) and keeps an explicit {}.
func TestSessionFilterOmitZero(t *testing.T) {
	if got := mustEncode(t, SessionFilter{}); got != `{}` {
		t.Fatalf("zero filter = %s, want {}", got)
	}
	if got := mustEncode(t, SessionFilter{Metadata: ordered.NewObject()}); got != `{"metadata":{}}` {
		t.Fatalf("explicit empty filter = %s", got)
	}
	var f SessionFilter
	f.Metadata.Set("b", 1)
	f.Metadata.Set("a", 2)
	if got := mustEncode(t, f); got != `{"metadata":{"b":1,"a":2}}` {
		t.Fatalf("filter = %s", got)
	}
}

// content is always present; empty/absent collections follow omitzero.
func TestMessageZeroValues(t *testing.T) {
	if got, want := mustEncode(t, Message{Role: RoleUser}), `{"role":"user","content":""}`; got != want {
		t.Fatalf("got %s want %s", got, want)
	}
	m := Message{Role: RoleAssistant, ToolCalls: []ToolCall{}, ToolResults: []ToolResult{}}
	if got, want := mustEncode(t, m), `{"role":"assistant","content":"","toolCalls":[],"toolResults":[]}`; got != want {
		t.Fatalf("got %s want %s", got, want)
	}
}

func TestArgumentsEmptyAndExact(t *testing.T) {
	if got, want := mustEncode(t, ToolCall{ID: "c", Name: "n"}), `{"id":"c","name":"n","arguments":{}}`; got != want {
		t.Fatalf("empty: got %s want %s", got, want)
	}

	// Exact bytes: key order, html characters and number spellings survive.
	raw := `{"z":1,"a":"<>&","n":1.50,"big":1e21,"nested":{"b":[2,1]}}`
	var tc ToolCall
	in := `{"id":"c","name":"n","arguments":` + raw + `}`
	if err := jsonx.UnmarshalStrict([]byte(in), &tc); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got := mustEncode(t, tc); got != in {
		t.Fatalf("exact: got %s want %s", got, in)
	}

	// UnmarshalJSON must copy: the decoder reuses its buffer.
	buf := []byte(raw)
	var a Arguments
	if err := a.UnmarshalJSON(buf); err != nil {
		t.Fatalf("UnmarshalJSON: %v", err)
	}
	for i := range buf {
		buf[i] = 'X'
	}
	if string(a) != raw {
		t.Fatalf("Arguments aliases the input buffer: %s", a)
	}

	// Malformed input must be an error from the surrounding decoder, never a panic.
	var bad ToolCall
	if err := jsonx.UnmarshalStrict([]byte(`{"id":"c","name":"n","arguments":{`), &bad); err == nil {
		t.Fatal("expected an error for truncated arguments")
	}
}

// CR-01: decoding a second payload into the same struct must not overwrite the
// bytes of an earlier copy (the old code appended into the receiver's backing
// array).
func TestArgumentsUnmarshalDoesNotCorruptEarlierCopies(t *testing.T) {
	first := `{"a":1,"b":"xxxxxxxx"}`
	second := `{"c":2,"d":"yyyyyyyy"}`
	var tc ToolCall
	if err := jsonx.UnmarshalStrict([]byte(`{"id":"c","name":"n","arguments":`+first+`}`), &tc); err != nil {
		t.Fatalf("first decode: %v", err)
	}
	saved := tc
	if err := jsonx.UnmarshalStrict([]byte(`{"id":"c","name":"n","arguments":`+second+`}`), &tc); err != nil {
		t.Fatalf("second decode: %v", err)
	}
	if string(saved.Arguments) != first {
		t.Fatalf("earlier copy corrupted: got %s want %s", saved.Arguments, first)
	}
	if string(tc.Arguments) != second {
		t.Fatalf("second decode: got %s want %s", tc.Arguments, second)
	}
}

// WR-05: null is treated as empty (re-encodes as {}); non-object JSON is rejected.
func TestArgumentsNullAndNonObject(t *testing.T) {
	var tc ToolCall
	if err := jsonx.UnmarshalStrict([]byte(`{"id":"c","name":"n","arguments":null}`), &tc); err != nil {
		t.Fatalf("null arguments: %v", err)
	}
	if len(tc.Arguments) != 0 {
		t.Fatalf("null must decode to empty, got %s", tc.Arguments)
	}
	if got, want := mustEncode(t, tc), `{"id":"c","name":"n","arguments":{}}`; got != want {
		t.Fatalf("got %s want %s", got, want)
	}

	// A previously filled value is reset by null.
	tc.Arguments = Arguments(`{"k":1}`)
	if err := jsonx.UnmarshalStrict([]byte(`{"id":"c","name":"n","arguments":null}`), &tc); err != nil {
		t.Fatalf("null over filled: %v", err)
	}
	if len(tc.Arguments) != 0 {
		t.Fatalf("null must reset, got %s", tc.Arguments)
	}

	for _, bad := range []string{`[]`, `[1]`, `"s"`, `1`, `true`} {
		var x ToolCall
		in := `{"id":"c","name":"n","arguments":` + bad + `}`
		if err := jsonx.UnmarshalStrict([]byte(in), &x); err == nil {
			t.Errorf("arguments %s must be rejected", bad)
		}
	}

	// Surrounding whitespace on a direct call is trimmed.
	var a Arguments
	if err := a.UnmarshalJSON([]byte(" {\"k\":1} \n")); err != nil || string(a) != `{"k":1}` {
		t.Fatalf("whitespace: %q, %v", a, err)
	}
	if err := a.UnmarshalJSON(nil); err == nil {
		t.Fatal("empty input must be rejected")
	}
}

// WR-01: an explicit empty content survives, and absent stays absent.
func TestStreamChunkContentEmptyVersusAbsent(t *testing.T) {
	empty := ""
	if got, want := mustEncode(t, StreamChunk{Type: ChunkText, Content: &empty}), `{"type":"text","content":""}`; got != want {
		t.Fatalf("got %s want %s", got, want)
	}
	if got, want := mustEncode(t, StreamChunk{Type: ChunkText}), `{"type":"text"}`; got != want {
		t.Fatalf("got %s want %s", got, want)
	}
	var c StreamChunk
	if err := jsonx.UnmarshalStrict([]byte(`{"type":"text","content":""}`), &c); err != nil {
		t.Fatal(err)
	}
	if c.Content == nil || *c.Content != "" {
		t.Fatalf("explicit empty content decoded as %v", c.Content)
	}
}
