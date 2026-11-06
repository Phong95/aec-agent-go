package openaicompat

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/Phong95/aec-agent-go/components/llmhub"
	"github.com/Phong95/aec-agent-go/kernel"
)

// A-207: prompt_cache_key for the endpoints that take it, OpenRouter cache_control parts
// for Claude and Gemini models, and none of it while caching is off.

var cacheConversation = []kernel.Message{
	{Role: kernel.RoleSystem, Content: "You are a builder."},
	{Role: kernel.RoleUser, Content: "Make a game."},
	{Role: kernel.RoleAssistant, ToolCalls: []kernel.ToolCall{{ID: "c1", Name: "read_file", Arguments: kernel.Arguments(`{"path":"a.txt"}`)}}},
	{Role: kernel.RoleAssistant, ToolResults: []kernel.ToolResult{{ToolCallID: "c1", Content: "file text"}}},
}

func cacheBody(t *testing.T, opts []Option, msgs []kernel.Message, mo llmhub.ModelOptions) string {
	t.Helper()
	p, err := New(llmhub.ProviderConfig{APIKey: "k"}, opts...)
	if err != nil {
		t.Fatal(err)
	}
	b, err := p.buildRequest(msgs, mo, false)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestPromptCacheKey(t *testing.T) {
	off := false
	hi := []kernel.Message{{Role: kernel.RoleUser, Content: "hi"}}
	if b := cacheBody(t, []Option{WithPromptCacheKey(true)}, hi, llmhub.ModelOptions{PromptCacheKey: "s-1"}); !strings.Contains(b, `"prompt_cache_key":"s-1"`) {
		t.Errorf("with the option: %s", b)
	}
	if b := cacheBody(t, nil, hi, llmhub.ModelOptions{PromptCacheKey: "s-1"}); strings.Contains(b, "prompt_cache_key") {
		t.Errorf("without the option: %s", b)
	}
	if b := cacheBody(t, []Option{WithPromptCacheKey(true)}, hi, llmhub.ModelOptions{PromptCacheKey: "s-1", PromptCaching: &off}); strings.Contains(b, "prompt_cache_key") {
		t.Errorf("caching off: %s", b)
	}
}

func TestRegistryEntriesCaching(t *testing.T) {
	for name, want := range map[string]bool{"openai": true, "novita": true, "openrouter": false} {
		p, err := llmhub.ResolveProvider(name, llmhub.ProviderConfig{APIKey: "k"})
		if err != nil {
			t.Fatal(err)
		}
		b, err := p.(*Provider).buildRequest([]kernel.Message{{Role: kernel.RoleUser, Content: "hi"}}, llmhub.ModelOptions{PromptCacheKey: "s-1"}, false)
		if err != nil {
			t.Fatal(err)
		}
		if got := strings.Contains(string(b), "prompt_cache_key"); got != want {
			t.Errorf("%s sends prompt_cache_key = %v, want %v", name, got, want)
		}
		// OpenRouter's sticky-routing key: the session id as session_id, openrouter only.
		if got := strings.Contains(string(b), `"session_id":"s-1"`); got != (name == "openrouter") {
			t.Errorf("%s sends session_id = %v", name, got)
		}
	}
}

func TestOpenRouterCacheControl(t *testing.T) {
	or := []Option{WithCacheControl(OpenRouterCacheControl)}
	b := cacheBody(t, or, cacheConversation, llmhub.ModelOptions{Model: "anthropic/claude-haiku-5.5"})
	var body struct {
		Messages []map[string]any `json:"messages"`
	}
	if err := json.Unmarshal([]byte(b), &body); err != nil {
		t.Fatal(err)
	}
	first, _ := json.Marshal(body.Messages[0])
	last, _ := json.Marshal(body.Messages[len(body.Messages)-1])
	if string(first) != `{"content":[{"cache_control":{"type":"ephemeral"},"text":"You are a builder.","type":"text"}],"role":"system"}` {
		t.Errorf("system = %s", first)
	}
	if string(last) != `{"content":[{"cache_control":{"type":"ephemeral"},"text":"file text","type":"text"}],"role":"tool","tool_call_id":"c1"}` {
		t.Errorf("last = %s", last)
	}
	if strings.Count(b, "cache_control") != 2 {
		t.Errorf("markers = %d, want 2: %s", strings.Count(b, "cache_control"), b)
	}

	off := false
	for _, mo := range []llmhub.ModelOptions{{Model: "deepseek/deepseek-v4.1-flash"}, {Model: "anthropic/claude-haiku-5.5", PromptCaching: &off}} {
		if b := cacheBody(t, or, cacheConversation, mo); strings.Contains(b, "cache_control") {
			t.Errorf("%+v: %s", mo, b)
		}
	}
	// Another endpoint never sends the parts.
	if b := cacheBody(t, nil, cacheConversation, llmhub.ModelOptions{Model: "anthropic/claude-haiku-5.5"}); strings.Contains(b, "cache_control") {
		t.Errorf("without the option: %s", b)
	}
}
