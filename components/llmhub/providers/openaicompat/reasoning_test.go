package openaicompat

import (
	"slices"
	"strings"
	"testing"

	"github.com/Phong95/aec-agent-go/components/llmhub"
	"github.com/Phong95/aec-agent-go/internal/llmtest"
	"github.com/Phong95/aec-agent-go/kernel"
)

// A-178: reasoning_content (DeepSeek, Novita) and reasoning (OpenRouter) come out as
// reasoning chunks, and as Message.Reasoning from Chat.
func TestStreamYieldsReasoningChunks(t *testing.T) {
	events := []string{
		`data: {"choices":[{"index":0,"delta":{"reasoning_content":"Let me "}}]}`,
		`data: {"choices":[{"index":0,"delta":{"reasoning":"think."}}]}`,
		`data: {"choices":[{"index":0,"delta":{"content":"Hi"}}]}`,
		`data: {"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
		"data: [DONE]",
	}
	up := llmtest.NewUpstream(t, func(llmtest.Request) llmtest.Reply { return sseReply(strings.Join(events, "\n\n") + "\n\n") })
	p := newTestProvider(t, up)
	chunks := drain(t, p, []kernel.Message{{Role: kernel.RoleUser, Content: "hi"}}, llmhub.ModelOptions{})
	var types []kernel.ChunkType
	var reasoning strings.Builder
	for _, c := range chunks {
		types = append(types, c.Type)
		if c.Type == kernel.ChunkReasoning && c.Content != nil {
			reasoning.WriteString(*c.Content)
		}
	}
	if want := []kernel.ChunkType{kernel.ChunkReasoning, kernel.ChunkReasoning, kernel.ChunkText, kernel.ChunkDone}; !slices.Equal(types, want) {
		t.Fatalf("chunk types = %v, want %v", types, want)
	}
	if reasoning.String() != "Let me think." {
		t.Fatalf("reasoning = %q", reasoning.String())
	}
}

func TestChatReturnsReasoningAndNeverSendsItBack(t *testing.T) {
	up := llmtest.NewUpstream(t, func(llmtest.Request) llmtest.Reply {
		return jsonReply(`{"choices":[{"index":0,"message":{"role":"assistant","content":"ok","reasoning_content":"because"},"finish_reason":"stop"}]}`)
	})
	p := newTestProvider(t, up)
	msgs := []kernel.Message{
		{Role: kernel.RoleUser, Content: "hi"},
		{Role: kernel.RoleAssistant, Content: "hello", Reasoning: "SECRET-THOUGHT"},
		{Role: kernel.RoleUser, Content: "again"},
	}
	res, err := p.Chat(t.Context(), msgs, llmhub.ModelOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Message.Reasoning != "because" {
		t.Fatalf("reasoning = %q", res.Message.Reasoning)
	}
	if body := string(up.Requests()[0].Body); strings.Contains(body, "SECRET-THOUGHT") {
		t.Fatalf("the request carries the reasoning: %s", body)
	}
}
