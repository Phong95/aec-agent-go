package gemini

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/Phong95/aec-agent-go/components/llmhub"
	"github.com/Phong95/aec-agent-go/components/toolhub"
	"github.com/Phong95/aec-agent-go/internal/llmtest"
	"github.com/Phong95/aec-agent-go/kernel"
)

// The same cases as core/ts/tests/unit/gemini-provider.test.ts (A-162), against a
// loopback upstream.

var helloMsgs = []kernel.Message{{Role: kernel.RoleUser, Content: "hi"}}

// jsonReply answers with body as JSON and the given status (zero: 200).
func jsonReply(status int, body string) llmtest.Reply {
	return llmtest.Reply{Status: status, Header: map[string]string{"Content-Type": "application/json"}, Chunks: []string{body}}
}

// sseReply answers with one SSE event per payload, CRLF framed.
func sseReply(payloads ...string) llmtest.Reply {
	chunks := make([]string, len(payloads))
	for i, p := range payloads {
		chunks[i] = "data: " + p + "\r\n\r\n"
	}
	return llmtest.Reply{Header: map[string]string{"Content-Type": "text/event-stream"}, Chunks: chunks}
}

// newTestProvider builds a provider whose endpoint is up, with model as cfg.Model.
func newTestProvider(t *testing.T, up *llmtest.Upstream, model string) *Provider {
	t.Helper()
	p, err := New(llmhub.ProviderConfig{APIKey: "secret-key", BaseURL: up.URL + "/v1beta/", Model: model}, WithHTTPClient(up.Client()))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return p
}

func drain(t *testing.T, p *Provider, msgs []kernel.Message, opts llmhub.ModelOptions) []kernel.StreamChunk {
	t.Helper()
	var out []kernel.StreamChunk
	for chunk, err := range p.Stream(t.Context(), msgs, opts) {
		if err != nil {
			t.Fatalf("stream error: %v", err)
		}
		out = append(out, chunk)
	}
	return out
}

// assertJSON fails when got and want are not the same JSON value.
func assertJSON(t *testing.T, got []byte, want string) {
	t.Helper()
	var g, w any
	if err := json.Unmarshal(got, &g); err != nil {
		t.Fatalf("decode got: %v (%s)", err, got)
	}
	if err := json.Unmarshal([]byte(want), &w); err != nil {
		t.Fatalf("decode want: %v", err)
	}
	if !reflect.DeepEqual(g, w) {
		t.Fatalf("JSON differs:\n got  %s\n want %s", got, want)
	}
}

// asModelError is err as a *kernel.ModelError.
func asModelError(err error) (*kernel.ModelError, bool) {
	var me *kernel.ModelError
	ok := errors.As(err, &me)
	return me, ok
}

func chunkTypes(chunks []kernel.StreamChunk) []kernel.ChunkType {
	out := make([]kernel.ChunkType, len(chunks))
	for i, c := range chunks {
		out[i] = c.Type
	}
	return out
}

func TestGeminiIsRegistered(t *testing.T) {
	p, err := llmhub.ResolveProvider("gemini", llmhub.ProviderConfig{APIKey: "k"})
	if err != nil {
		t.Fatalf("ResolveProvider: %v", err)
	}
	if _, ok := p.(*Provider); !ok || p.Name() != "gemini" {
		t.Fatalf("resolved %T named %q, want *gemini.Provider named gemini", p, p.Name())
	}
}

func TestGeminiRequestMapping(t *testing.T) {
	up := llmtest.NewUpstream(t, func(llmtest.Request) llmtest.Reply {
		return jsonReply(0, `{"candidates":[{"content":{"parts":[{"text":"ok"}]},"finishReason":"STOP"}]}`)
	})
	p := newTestProvider(t, up, "")
	isErr := true
	msgs := []kernel.Message{
		{Role: kernel.RoleSystem, Content: "Be brief."},
		{Role: kernel.RoleUser, Content: "Read two files"},
		{Role: kernel.RoleAssistant, Content: "Reading.", ToolCalls: []kernel.ToolCall{
			{ID: "c1", Name: "read_file", Arguments: kernel.Arguments(`{"path":"a"}`), Signature: "sig-1"},
			{ID: "c2", Name: "read_file", Arguments: kernel.Arguments(`{"path":"b"}`)},
		}},
		{Role: kernel.RoleTool, ToolResults: []kernel.ToolResult{
			{ToolCallID: "c1", Content: "A"},
			{ToolCallID: "c2", Content: "missing", IsError: &isErr},
		}},
		{Role: kernel.RoleUser, Content: "Thanks"},
	}
	opts := llmhub.ModelOptions{
		SystemPrompt: "You are an agent.",
		MaxTokens:    llmhub.Int(100),
		Temperature:  llmhub.Float(0.2),
		Tools: []toolhub.Definition{{
			Name:        "read_file",
			Description: "Read a file",
			Parameters:  toolhub.JSONSchema{Type: "object", Properties: toolhub.NewProperties(toolhub.Property{Name: "path", Schema: toolhub.JSONSchema{Type: "string"}}), Required: []string{"path"}},
		}},
	}
	if _, err := p.Chat(t.Context(), msgs, opts); err != nil {
		t.Fatalf("Chat: %v", err)
	}
	reqs := up.Requests()
	if len(reqs) != 1 {
		t.Fatalf("%d requests, want 1", len(reqs))
	}
	r := reqs[0]
	if r.Path != "/v1beta/models/gemini-2.5-flash:generateContent" {
		t.Errorf("path %q", r.Path)
	}
	if got := r.Header.Get("X-Goog-Api-Key"); got != "secret-key" {
		t.Errorf("x-goog-api-key %q", got)
	}
	if strings.Contains(r.RawQuery, "secret-key") {
		t.Errorf("the key is in the query %q", r.RawQuery)
	}
	assertJSON(t, r.Body, `{
		"contents": [
			{"role":"user","parts":[{"text":"Read two files"}]},
			{"role":"model","parts":[
				{"text":"Reading."},
				{"functionCall":{"name":"read_file","args":{"path":"a"}},"thoughtSignature":"sig-1"},
				{"functionCall":{"name":"read_file","args":{"path":"b"}}}
			]},
			{"role":"user","parts":[
				{"functionResponse":{"name":"read_file","response":{"output":"A"}}},
				{"functionResponse":{"name":"read_file","response":{"error":"missing"}}},
				{"text":"Thanks"}
			]}
		],
		"systemInstruction":{"parts":[{"text":"You are an agent."},{"text":"Be brief."}]},
		"tools":[{"functionDeclarations":[{"name":"read_file","description":"Read a file","parametersJsonSchema":{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}}]}],
		"generationConfig":{"maxOutputTokens":100,"temperature":0.2,"thinkingConfig":{"includeThoughts":true}}
	}`)
}

func TestGeminiSkipSignatureAndNoGenerationConfig(t *testing.T) {
	up := llmtest.NewUpstream(t, func(llmtest.Request) llmtest.Reply {
		return jsonReply(0, `{"candidates":[{"content":{"parts":[{"text":"ok"}]},"finishReason":"STOP"}]}`)
	})
	// A model before Gemini 2.5 (A-178: a thinking model always gets
	// thinkingConfig.includeThoughts).
	p := newTestProvider(t, up, "gemini-2.0-flash")
	msgs := []kernel.Message{
		{Role: kernel.RoleUser, Content: "hi"},
		{Role: kernel.RoleAssistant, ToolCalls: []kernel.ToolCall{{ID: "x", Name: "now"}}},
		{Role: kernel.RoleUser, ToolResults: []kernel.ToolResult{{ToolCallID: "x", Content: "noon"}}},
	}
	if _, err := p.Chat(t.Context(), msgs, llmhub.ModelOptions{}); err != nil {
		t.Fatalf("Chat: %v", err)
	}
	r := up.Requests()[0]
	if r.Path != "/v1beta/models/gemini-2.0-flash:generateContent" {
		t.Errorf("path %q", r.Path)
	}
	assertJSON(t, r.Body, `{"contents":[
		{"role":"user","parts":[{"text":"hi"}]},
		{"role":"model","parts":[{"functionCall":{"name":"now","args":{}},"thoughtSignature":"skip_thought_signature_validator"}]},
		{"role":"user","parts":[{"functionResponse":{"name":"now","response":{"output":"noon"}}}]}
	]}`)
}

func TestGeminiChatMapsTheResponse(t *testing.T) {
	up := llmtest.NewUpstream(t, func(llmtest.Request) llmtest.Reply {
		return jsonReply(0, `{"candidates":[{"content":{"parts":[
			{"text":"thinking...","thought":true},
			{"text":"Let me check."},
			{"functionCall":{"id":"fc-1","name":"read_file","args":{"path":"a"}},"thoughtSignature":"sig"},
			{"functionCall":{"name":"read_file","args":{"path":"b"}}}
		]},"finishReason":"STOP"}],
		"usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":5,"thoughtsTokenCount":7}}`)
	})
	resp, err := newTestProvider(t, up, "").Chat(t.Context(), helloMsgs, llmhub.ModelOptions{})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if resp.Message.Content != "Let me check." || resp.StopReason != kernel.StopToolUse {
		t.Errorf("content %q stop %q", resp.Message.Content, resp.StopReason)
	}
	if resp.Usage.InputTokens != 10 || resp.Usage.OutputTokens != 12 {
		t.Errorf("usage %+v, want 10 in / 12 out", resp.Usage)
	}
	calls := resp.Message.ToolCalls
	if len(calls) != 2 {
		t.Fatalf("%d calls", len(calls))
	}
	if calls[0].ID != "fc-1" || calls[0].Signature != "sig" || string(calls[0].Arguments) != `{"path":"a"}` {
		t.Errorf("first call %+v", calls[0])
	}
	if !strings.HasPrefix(calls[1].ID, "call_") || calls[1].Signature != "" {
		t.Errorf("second call %+v", calls[1])
	}
}

func TestGeminiFinishReasons(t *testing.T) {
	for reason, want := range map[string]kernel.StopReason{
		"MAX_TOKENS":              kernel.StopMaxTokens,
		"SAFETY":                  kernel.StopError,
		"RECITATION":              kernel.StopError,
		"MALFORMED_FUNCTION_CALL": kernel.StopError,
	} {
		up := llmtest.NewUpstream(t, func(llmtest.Request) llmtest.Reply {
			return jsonReply(0, `{"candidates":[{"content":{"parts":[{"text":"x"}]},"finishReason":"`+reason+`"}]}`)
		})
		resp, err := newTestProvider(t, up, "").Chat(t.Context(), helloMsgs, llmhub.ModelOptions{})
		if err != nil || resp.StopReason != want {
			t.Errorf("%s: stop %q err %v, want %q", reason, resp.StopReason, err, want)
		}
	}
	up := llmtest.NewUpstream(t, func(llmtest.Request) llmtest.Reply {
		return jsonReply(0, `{"promptFeedback":{"blockReason":"SAFETY"}}`)
	})
	resp, err := newTestProvider(t, up, "").Chat(t.Context(), helloMsgs, llmhub.ModelOptions{})
	if err != nil || resp.StopReason != kernel.StopError || resp.Message.Content != "" {
		t.Errorf("blocked prompt: %+v %v", resp, err)
	}
}

func TestGeminiStream(t *testing.T) {
	up := llmtest.NewUpstream(t, func(llmtest.Request) llmtest.Reply {
		return sseReply(
			`{"candidates":[{"content":{"role":"model","parts":[{"text":"Hel"}]}}]}`,
			`{"candidates":[{"content":{"role":"model","parts":[{"text":"lo"}]}}]}`,
			`{"candidates":[{"content":{"role":"model","parts":[{"functionCall":{"name":"write_file","args":{"path":"r.md","content":"# R"}},"thoughtSignature":"s"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":3,"candidatesTokenCount":4}}`,
		)
	})
	chunks := drain(t, newTestProvider(t, up, ""), helloMsgs, llmhub.ModelOptions{})
	r := up.Requests()[0]
	if r.Path != "/v1beta/models/gemini-2.5-flash:streamGenerateContent" || r.RawQuery != "alt=sse" {
		t.Errorf("request %s?%s", r.Path, r.RawQuery)
	}
	want := []kernel.ChunkType{kernel.ChunkText, kernel.ChunkText, kernel.ChunkToolCallStart, kernel.ChunkToolCallDelta, kernel.ChunkToolCall, kernel.ChunkDone}
	if got := chunkTypes(chunks); !reflect.DeepEqual(got, want) {
		t.Fatalf("chunks %v, want %v", got, want)
	}
	call := chunks[4].ToolCall
	if call.Name != "write_file" || call.Signature != "s" || chunks[2].ToolCallProgress.ID != call.ID {
		t.Errorf("call %+v, start %+v", call, chunks[2].ToolCallProgress)
	}
	assertJSON(t, []byte(chunks[3].ToolCallProgress.ArgumentsDelta), `{"path":"r.md","content":"# R"}`)
	done := chunks[5]
	if done.StopReason != kernel.StopToolUse || done.Usage == nil || *done.Usage != (kernel.TokenUsage{InputTokens: 3, OutputTokens: 4}) {
		t.Errorf("done %+v", done)
	}
}

func TestGeminiStreamEnds(t *testing.T) {
	cases := map[string]struct {
		reply llmtest.Reply
		want  []kernel.ChunkType
		last  string
	}{
		"a last event without a blank line": {
			reply: llmtest.Reply{Chunks: []string{
				"data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"Hi\"}]}}]}\n\n",
				"data: {\"candidates\":[{\"finishReason\":\"MAX_TOKENS\"}]}",
			}},
			want: []kernel.ChunkType{kernel.ChunkText, kernel.ChunkDone},
		},
		"no finish reason": {
			reply: sseReply(`{"candidates":[{"content":{"parts":[{"text":"Hi"}]}}]}`),
			want:  []kernel.ChunkType{kernel.ChunkText, kernel.ChunkError},
			last:  "incomplete",
		},
		"an error event": {
			reply: sseReply(`{"error":{"code":500,"message":"Internal error","status":"INTERNAL"}}`),
			want:  []kernel.ChunkType{kernel.ChunkError},
			last:  "Internal error",
		},
		"an HTTP error": {
			reply: jsonReply(429, `{"error":{"code":429,"message":"quota"}}`),
			want:  []kernel.ChunkType{kernel.ChunkError},
			last:  "Rate limited",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			up := llmtest.NewUpstream(t, func(llmtest.Request) llmtest.Reply { return tc.reply })
			chunks := drain(t, newTestProvider(t, up, ""), helloMsgs, llmhub.ModelOptions{})
			if got := chunkTypes(chunks); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("chunks %v, want %v", got, tc.want)
			}
			last := chunks[len(chunks)-1]
			if tc.last != "" && !strings.Contains(last.Error, tc.last) {
				t.Errorf("error %q, want it to contain %q", last.Error, tc.last)
			}
			if tc.last == "" && last.StopReason != kernel.StopMaxTokens {
				t.Errorf("stop %q", last.StopReason)
			}
		})
	}
}

func TestGeminiHTTPErrors(t *testing.T) {
	cases := []struct {
		status    int
		body      string
		code      kernel.ModelErrorCode
		retryable bool
	}{
		{400, `{"error":{"code":400,"message":"API key not valid.","status":"INVALID_ARGUMENT","details":[{"reason":"API_KEY_INVALID"}]}}`, kernel.ModelCodeAuth, false},
		{429, `{"error":{"code":429,"message":"nope"}}`, kernel.ModelCodeRateLimit, true},
		{404, `{"error":{"code":404,"message":"nope"}}`, kernel.ModelCodeInvalidRequest, false},
		{400, `{"error":{"code":400,"message":"nope"}}`, kernel.ModelCodeInvalidRequest, false},
		{503, `{"error":{"code":503,"message":"nope"}}`, kernel.ModelCodeProvider, true},
		{504, `{"error":{"code":504,"message":"nope"}}`, kernel.ModelCodeTimeout, true},
	}
	for _, tc := range cases {
		up := llmtest.NewUpstream(t, func(llmtest.Request) llmtest.Reply { return jsonReply(tc.status, tc.body) })
		_, err := newTestProvider(t, up, "").Chat(t.Context(), helloMsgs, llmhub.ModelOptions{})
		me, ok := asModelError(err)
		if !ok || me.Code != tc.code || me.Retryable != tc.retryable {
			t.Errorf("HTTP %d: error %v, want code %s retryable %v", tc.status, err, tc.code, tc.retryable)
		}
		if err != nil && strings.Contains(err.Error(), "secret-key") {
			t.Errorf("HTTP %d: the error text carries the key: %v", tc.status, err)
		}
	}
}

func TestGeminiListModels(t *testing.T) {
	up := llmtest.NewUpstream(t, func(r llmtest.Request) llmtest.Reply {
		if strings.Contains(r.RawQuery, "pageToken=p2") {
			return jsonReply(0, `{"models":[{"name":"models/gemini-2.0-flash","supportedGenerationMethods":["generateContent"]}]}`)
		}
		return jsonReply(0, `{"models":[
			{"name":"models/gemini-2.5-pro","displayName":"Gemini 2.5 Pro","description":"Our most capable model","inputTokenLimit":1048576,"outputTokenLimit":65536,"supportedGenerationMethods":["generateContent","countTokens","streamGenerateContent"],"thinking":true},
			{"name":"models/text-embedding-004","supportedGenerationMethods":["embedContent"]}
		],"nextPageToken":"p2"}`)
	})
	models, err := newTestProvider(t, up, "").ListModels(t.Context())
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	reqs := up.Requests()
	if len(reqs) != 2 || reqs[0].Path != "/v1beta/models" || reqs[0].RawQuery != "pageSize=1000" || reqs[1].RawQuery != "pageSize=1000&pageToken=p2" {
		t.Fatalf("requests %+v", reqs)
	}
	if len(models) != 2 || models[0].ID != "gemini-2.5-pro" || models[1].ID != "gemini-2.0-flash" {
		t.Fatalf("models %+v", models)
	}
	m := models[0]
	if m.Provider != "gemini" || m.DisplayName != "Gemini 2.5 Pro" || m.OwnedBy != "google" ||
		m.ContextWindowTokens == nil || *m.ContextWindowTokens != 1048576 || m.MaxOutputTokens == nil || *m.MaxOutputTokens != 65536 {
		t.Errorf("first model %+v", m.ModelRecord)
	}
	if c := m.Capabilities; c == nil || c.Reasoning == nil || !*c.Reasoning || c.Streaming == nil || !*c.Streaming {
		t.Errorf("capabilities %+v", m.Capabilities)
	}
	if c := models[1].Capabilities; c == nil || c.Streaming == nil || *c.Streaming || c.Reasoning != nil {
		t.Errorf("second capabilities %+v", c)
	}

	denied := llmtest.NewUpstream(t, func(llmtest.Request) llmtest.Reply {
		return jsonReply(403, `{"error":{"code":403,"message":"denied"}}`)
	})
	_, err = newTestProvider(t, denied, "").ListModels(t.Context())
	if me, ok := asModelError(err); !ok || me.Code != kernel.ModelCodeAuth {
		t.Errorf("403: %v, want an auth error", err)
	}
}
