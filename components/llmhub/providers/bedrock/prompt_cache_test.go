package bedrock

import (
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime/types"

	"github.com/Phong95/aec-agent-go/components/llmhub"
	"github.com/Phong95/aec-agent-go/components/toolhub"
	"github.com/Phong95/aec-agent-go/kernel"
)

// A-207: Converse requests carry a cachePoint after the system blocks and at the end of
// the last message, for Claude and Nova only, and none while caching is off.

var cacheConversation = []kernel.Message{
	{Role: kernel.RoleSystem, Content: "You are a builder."},
	{Role: kernel.RoleUser, Content: "Make a game."},
	{Role: kernel.RoleAssistant, ToolCalls: []kernel.ToolCall{{ID: "c1", Name: "read_file", Arguments: kernel.Arguments(`{"path":"a.txt"}`)}}},
	{Role: kernel.RoleAssistant, ToolResults: []kernel.ToolResult{{ToolCallID: "c1", Content: "file text"}}},
}

var cacheTools = []toolhub.Definition{{Name: "read_file", Description: "Read a file", Parameters: toolhub.JSONSchema{Type: "object", Properties: toolhub.NewProperties()}}}

func cacheRequest(t *testing.T, msgs []kernel.Message, opts llmhub.ModelOptions) request {
	t.Helper()
	api, _ := captureConverse(t)
	r, err := newFakeProvider(t, api).buildRequest(msgs, opts)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func countPoints(r request) (system, tools, last int) {
	for _, b := range r.system {
		if _, ok := b.(*types.SystemContentBlockMemberCachePoint); ok {
			system++
		}
	}
	if r.tools != nil {
		for _, b := range r.tools.Tools {
			if _, ok := b.(*types.ToolMemberCachePoint); ok {
				tools++
			}
		}
	}
	if n := len(r.messages); n > 0 {
		content := r.messages[n-1].Content
		if _, ok := content[len(content)-1].(*types.ContentBlockMemberCachePoint); ok {
			last++
		}
	}
	return
}

func TestCachePoints_Claude(t *testing.T) {
	r := cacheRequest(t, cacheConversation, llmhub.ModelOptions{Model: "anthropic.claude-haiku-5-5", Tools: cacheTools})
	if s, tl, l := countPoints(r); s != 1 || tl != 0 || l != 1 {
		t.Fatalf("cache points: system %d, tools %d, last message %d; want 1, 0, 1", s, tl, l)
	}
	// Without a system prompt the point goes after the tools (Claude).
	r = cacheRequest(t, cacheConversation[1:], llmhub.ModelOptions{Model: "anthropic.claude-haiku-5-5", Tools: cacheTools})
	if s, tl, l := countPoints(r); s != 0 || tl != 1 || l != 1 {
		t.Fatalf("no system: cache points system %d, tools %d, last %d; want 0, 1, 1", s, tl, l)
	}
}

func TestCachePoints_OtherModelsAndOff(t *testing.T) {
	r := cacheRequest(t, cacheConversation, llmhub.ModelOptions{Model: "deepseek.v3-v1:0", Tools: cacheTools})
	if s, tl, l := countPoints(r); s+tl+l != 0 {
		t.Fatalf("a model without cache points got %d, %d, %d", s, tl, l)
	}
	off := false
	r = cacheRequest(t, cacheConversation, llmhub.ModelOptions{Model: "anthropic.claude-haiku-5-5", Tools: cacheTools, PromptCaching: &off})
	if s, tl, l := countPoints(r); s+tl+l != 0 {
		t.Fatalf("caching off still sent %d, %d, %d", s, tl, l)
	}
}
