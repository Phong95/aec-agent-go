package anthropic

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/Phong95/aec-agent-go/components/llmhub"
	"github.com/Phong95/aec-agent-go/components/toolhub"
	"github.com/Phong95/aec-agent-go/kernel"
)

// A-207: the request marks the system prompt and the last message block for the prompt
// cache, the last tool instead of the system prompt when there is none, and nothing when
// ModelOptions.PromptCaching is false. Mirrors core/ts tests/unit/prompt-caching.test.ts.

var cacheTools = []toolhub.Definition{
	{Name: "read_file", Description: "Read a file", Parameters: toolhub.JSONSchema{Type: "object", Properties: toolhub.NewProperties()}},
	{Name: "write_file", Description: "Write a file", Parameters: toolhub.JSONSchema{Type: "object", Properties: toolhub.NewProperties()}},
}

var cacheConversation = []kernel.Message{
	{Role: kernel.RoleSystem, Content: "You are a builder."},
	{Role: kernel.RoleUser, Content: "Make a game."},
	{Role: kernel.RoleAssistant, ToolCalls: []kernel.ToolCall{{ID: "c1", Name: "read_file", Arguments: kernel.Arguments(`{"path":"a.txt"}`)}}},
	{Role: kernel.RoleAssistant, ToolResults: []kernel.ToolResult{{ToolCallID: "c1", Content: "file text"}}},
}

func cacheBody(t *testing.T, msgs []kernel.Message, opts llmhub.ModelOptions) map[string]any {
	t.Helper()
	p, err := New(llmhub.ProviderConfig{APIKey: "k"})
	if err != nil {
		t.Fatal(err)
	}
	b, _, err := p.buildRequest(msgs, opts)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func hasMarker(v any) bool {
	m, ok := v.(map[string]any)
	return ok && m["cache_control"] != nil
}

func TestCacheMarkers_SystemAndLastBlock(t *testing.T) {
	body := cacheBody(t, cacheConversation, llmhub.ModelOptions{Tools: cacheTools})
	system, ok := body["system"].([]any)
	if !ok || len(system) != 1 || !hasMarker(system[0]) || system[0].(map[string]any)["text"] != "You are a builder." {
		t.Fatalf("system = %v, want one marked text block", body["system"])
	}
	for _, tool := range body["tools"].([]any) {
		if hasMarker(tool) {
			t.Errorf("a tool is marked although the system marker covers the tools: %v", tool)
		}
	}
	msgs := body["messages"].([]any)
	marked := 0
	for _, m := range msgs {
		if blocks, ok := m.(map[string]any)["content"].([]any); ok {
			for _, b := range blocks {
				if hasMarker(b) {
					marked++
				}
			}
		}
	}
	last := msgs[len(msgs)-1].(map[string]any)["content"].([]any)
	end := last[len(last)-1].(map[string]any)
	if marked != 1 || end["type"] != "tool_result" || !hasMarker(end) {
		t.Fatalf("marked %d blocks, last block %v; want only the last tool_result marked", marked, end)
	}
}

func TestCacheMarkers_NoSystemMarksTheLastToolAndAPlainMessage(t *testing.T) {
	body := cacheBody(t, []kernel.Message{{Role: kernel.RoleUser, Content: "hello"}}, llmhub.ModelOptions{Tools: cacheTools})
	if _, ok := body["system"]; ok {
		t.Fatalf("system = %v, want none", body["system"])
	}
	tools := body["tools"].([]any)
	if hasMarker(tools[0]) || !hasMarker(tools[1]) {
		t.Fatalf("tools = %v, want only the last marked", tools)
	}
	want := `[{"content":[{"cache_control":{"type":"ephemeral"},"text":"hello","type":"text"}],"role":"user"}]`
	got, _ := json.Marshal(body["messages"])
	if string(got) != want {
		t.Fatalf("messages = %s, want %s", got, want)
	}
}

func TestCacheMarkers_OffSendsNone(t *testing.T) {
	off := false
	p, err := New(llmhub.ProviderConfig{APIKey: "k"})
	if err != nil {
		t.Fatal(err)
	}
	b, _, err := p.buildRequest(cacheConversation, llmhub.ModelOptions{Tools: cacheTools, PromptCaching: &off})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "cache_control") || !strings.Contains(string(b), `"system":"You are a builder."`) {
		t.Fatalf("body = %s, want a plain system string and no markers", b)
	}
}
