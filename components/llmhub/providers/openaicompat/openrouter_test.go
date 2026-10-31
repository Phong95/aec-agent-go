package openaicompat

import (
	"errors"
	"math"
	"strings"
	"testing"

	"github.com/Phong95/aec-agent-go/components/llmhub"
	"github.com/Phong95/aec-agent-go/internal/llmtest"
	"github.com/Phong95/aec-agent-go/kernel"
)

// A-177: the "openrouter" provider — OpenAI-compatible, with the reasoning effort in
// reasoning:{effort}, AppURL / AppTitle as attribution headers, and SSE keep-alive
// comments.

func TestOpenRouterIsRegistered(t *testing.T) {
	p, err := llmhub.ResolveProvider("openrouter", llmhub.ProviderConfig{APIKey: "k"})
	if err != nil {
		t.Fatal(err)
	}
	op, ok := p.(*Provider)
	if !ok {
		t.Fatalf("provider is %T", p)
	}
	if op.Name() != "openrouter" || op.baseURL != OpenRouterBaseURL || op.reasoningParam != ReasoningObjectParam {
		t.Fatalf("name %q, base %q, reasoning %q", op.Name(), op.baseURL, op.reasoningParam)
	}
	if OpenRouterBaseURL != "https://openrouter.ai/api/v1" {
		t.Fatalf("OpenRouterBaseURL = %q", OpenRouterBaseURL)
	}
}

func TestOpenRouterRequestCarriesReasoningObject(t *testing.T) {
	msgs := []kernel.Message{{Role: kernel.RoleUser, Content: "hi"}}
	p, err := New(llmhub.ProviderConfig{APIKey: "k"}, WithReasoningParam(ReasoningObjectParam))
	if err != nil {
		t.Fatal(err)
	}
	b, err := p.buildRequest(msgs, llmhub.ModelOptions{ReasoningEffort: "high"}, true)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(b); !strings.Contains(got, `"reasoning":{"effort":"high"},"stream":true`) || strings.Contains(got, "reasoning_effort") {
		t.Fatalf("body = %s", got)
	}
	b, err = p.buildRequest(msgs, llmhub.ModelOptions{}, false)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "reasoning") {
		t.Fatalf("body without an effort = %s", b)
	}
}

func TestOpenRouterSendsAttributionHeaders(t *testing.T) {
	up := llmtest.NewUpstream(t, func(llmtest.Request) llmtest.Reply { return jsonReply(minimalCompletion) })
	msgs := []kernel.Message{{Role: kernel.RoleUser, Content: "hi"}}

	// What openRouterFactory builds, with the loopback client of the test.
	cfg := llmhub.ProviderConfig{APIKey: "k", BaseURL: up.URL + "/api/v1", AppURL: "https://aec.example", AppTitle: "AEC Agent"}
	for _, c := range []llmhub.ProviderConfig{cfg, {APIKey: "k", BaseURL: up.URL + "/api/v1"}} {
		p, err := New(c, WithName("openrouter"), WithHTTPClient(up.Client()), WithMaxRetries(0), WithHeaders(openRouterHeaders(c)))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := p.Chat(t.Context(), msgs, llmhub.ModelOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	reqs := up.Requests()
	if len(reqs) != 2 || reqs[0].Path != "/api/v1/chat/completions" {
		t.Fatalf("requests = %+v", reqs)
	}
	if got := reqs[0].Header.Get("HTTP-Referer"); got != "https://aec.example" {
		t.Fatalf("HTTP-Referer = %q", got)
	}
	if got := reqs[0].Header.Get("X-Title"); got != "AEC Agent" {
		t.Fatalf("X-Title = %q", got)
	}
	if reqs[1].Header.Get("HTTP-Referer") != "" || reqs[1].Header.Get("X-Title") != "" {
		t.Fatalf("headers without AppURL / AppTitle = %v", reqs[1].Header)
	}
}

func TestOpenRouterStreamSkipsKeepAliveComments(t *testing.T) {
	events := []string{
		": OPENROUTER PROCESSING",
		`data: {"choices":[{"index":0,"delta":{"content":"Hel"}}]}`,
		": OPENROUTER PROCESSING",
		`data: {"choices":[{"index":0,"delta":{"content":"lo"}}]}`,
		`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"read_file","arguments":"{\"path\":"}}]}}]}`,
		`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"a.txt\"}"}}]}}]}`,
		`data: {"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
		`data: {"choices":[],"usage":{"prompt_tokens":5,"completion_tokens":2}}`,
		"data: [DONE]",
	}
	up := llmtest.NewUpstream(t, func(llmtest.Request) llmtest.Reply { return sseReply(strings.Join(events, "\n\n") + "\n\n") })
	p := newTestProvider(t, up, WithName("openrouter"), WithReasoningParam(ReasoningObjectParam))

	chunks := drain(t, p, []kernel.Message{{Role: kernel.RoleUser, Content: "hi"}}, llmhub.ModelOptions{})
	var text strings.Builder
	var call *kernel.ToolCall
	for _, c := range chunks {
		switch c.Type {
		case kernel.ChunkText:
			if c.Content != nil {
				text.WriteString(*c.Content)
			}
		case kernel.ChunkToolCall:
			call = c.ToolCall
		case kernel.ChunkError:
			t.Fatalf("error chunk: %s", c.Error)
		case kernel.ChunkToolCallStart, kernel.ChunkToolCallDelta, kernel.ChunkToolResult, kernel.ChunkDone, kernel.ChunkUsage, kernel.ChunkReasoning:
		}
	}
	if text.String() != "Hello" {
		t.Fatalf("text = %q", text.String())
	}
	if call == nil || call.ID != "call_1" || call.Name != "read_file" || string(call.Arguments) != `{"path":"a.txt"}` {
		t.Fatalf("tool call = %+v", call)
	}
	last := chunks[len(chunks)-1]
	if last.Type != kernel.ChunkDone || last.Usage == nil || last.Usage.InputTokens != 5 || last.StopReason != kernel.StopToolUse {
		t.Fatalf("done = %+v", last)
	}
}

func TestOpenRouterErrors(t *testing.T) {
	status := 402
	up := llmtest.NewUpstream(t, func(llmtest.Request) llmtest.Reply {
		r := jsonReply(`{"error":{"code":` + map[int]string{402: "402", 401: "401"}[status] + `,"message":"Insufficient credits"}}`)
		r.Status = status
		return r
	})
	p := newTestProvider(t, up, WithName("openrouter"))
	msgs := []kernel.Message{{Role: kernel.RoleUser, Content: "hi"}}

	// 402: the account is out of credits; a retry cannot help.
	_, err := p.Chat(t.Context(), msgs, llmhub.ModelOptions{})
	var me *kernel.ModelError
	if !errors.As(err, &me) || me.Code != kernel.ModelCodeProvider || me.Retryable || !strings.Contains(me.Message, "Insufficient credits") {
		t.Fatalf("402 = %#v", err)
	}

	status = 401
	_, err = p.Chat(t.Context(), msgs, llmhub.ModelOptions{})
	if !errors.As(err, &me) || me.Code != kernel.ModelCodeAuth || me.Retryable {
		t.Fatalf("401 = %#v", err)
	}
}

func TestOpenRouterListModels(t *testing.T) {
	catalog := `{"data":[{"id":"anthropic/claude-sonnet-4.5","name":"Anthropic: Claude Sonnet 4.5","created":1758000000,` +
		`"context_length":1000000,"architecture":{"input_modalities":["text","image"],"output_modalities":["text"]},` +
		`"pricing":{"prompt":"0.000003","completion":"0.000015"},"top_provider":{"context_length":1000000,"max_completion_tokens":64000},` +
		`"supported_parameters":["tools","tool_choice","reasoning","structured_outputs"]}]}`
	up := llmtest.NewUpstream(t, func(llmtest.Request) llmtest.Reply { return jsonReply(catalog) })
	p := newTestProvider(t, up, WithName("openrouter"))

	models, err := p.ListModels(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 1 {
		t.Fatalf("models = %+v", models)
	}
	m := models[0]
	if m.ID != "anthropic/claude-sonnet-4.5" || m.Provider != "openrouter" || m.DisplayName != "Anthropic: Claude Sonnet 4.5" {
		t.Fatalf("model = %+v", m)
	}
	if m.ContextWindowTokens == nil || *m.ContextWindowTokens != 1000000 || m.MaxOutputTokens == nil || *m.MaxOutputTokens != 64000 {
		t.Fatalf("limits = %v, %v", m.ContextWindowTokens, m.MaxOutputTokens)
	}
	c := m.Capabilities
	if c == nil || !isTrue(c.Vision) || !isTrue(c.ToolCalling) || !isTrue(c.Reasoning) || !isTrue(c.StructuredOutputs) {
		t.Fatalf("capabilities = %+v", c)
	}
	if m.Pricing == nil || m.Pricing.InputUSDPerMillionTokens == nil || math.Abs(*m.Pricing.InputUSDPerMillionTokens-3) > 1e-9 ||
		m.Pricing.OutputUSDPerMillionTokens == nil || math.Abs(*m.Pricing.OutputUSDPerMillionTokens-15) > 1e-9 {
		t.Fatalf("pricing = %+v", m.Pricing)
	}
}

func isTrue(b *bool) bool { return b != nil && *b }
