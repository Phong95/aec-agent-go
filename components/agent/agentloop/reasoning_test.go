package agentloop_test

import (
	"strings"
	"testing"

	"github.com/Phong95/aec-agent-go/aecmock"
	"github.com/Phong95/aec-agent-go/kernel"
)

// A-178: reasoning chunks are forwarded to the consumer, and their text is saved on
// the response's assistant message (Message.Reasoning).
func TestStream_SavesTheReasoning(t *testing.T) {
	str := func(s string) *string { return &s }
	r := newRig(t, rigOptions{}, aecmock.ScriptedTurn{Chunks: []kernel.StreamChunk{
		{Type: kernel.ChunkReasoning, Content: str("First ")},
		{Type: kernel.ChunkReasoning, Content: str("thought.")},
		{Type: kernel.ChunkText, Content: str("Reply.")},
		{Type: kernel.ChunkDone, StopReason: kernel.StopEndTurn},
	}})
	chunks, err := r.stream(t, "Hello")
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	var reasoning strings.Builder
	for _, c := range chunks {
		if c.Type == kernel.ChunkReasoning {
			reasoning.WriteString(textOf(c))
		}
	}
	if reasoning.String() != "First thought." {
		t.Fatalf("reasoning chunks = %q", reasoning.String())
	}
	msgs := r.conv.Messages()
	last := msgs[len(msgs)-1]
	if last.Role != kernel.RoleAssistant || last.Content != "Reply." || last.Reasoning != "First thought." {
		t.Fatalf("saved reply = %+v", last)
	}
}

// A-178: a response without reasoning saves no reasoning.
func TestStream_NoReasoningSavesNone(t *testing.T) {
	r := newRig(t, rigOptions{}, aecmock.ScriptedTurn{Content: "Hello back!"})
	if _, err := r.stream(t, "Hello"); err != nil {
		t.Fatalf("Stream: %v", err)
	}
	msgs := r.conv.Messages()
	if got := msgs[len(msgs)-1].Reasoning; got != "" {
		t.Fatalf("reasoning = %q", got)
	}
}
