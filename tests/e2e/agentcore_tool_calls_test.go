package e2e_test

import (
	"context"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/Phong95/aec-agent-go/aecmock"
	"github.com/Phong95/aec-agent-go/components/llmhub"
	"github.com/Phong95/aec-agent-go/components/toolhub"
	"github.com/Phong95/aec-agent-go/kernel"
)

// writeFileTool is the write_file test tool: it records the arguments of every
// execution.
type writeFileTool struct {
	*loopTool
	mu   sync.Mutex
	args []string
}

func newWriteFileTool() *writeFileTool {
	w := &writeFileTool{}
	w.loopTool = newLoopTool("write_file", func(_ context.Context, args kernel.Arguments, _ toolhub.Context) (toolhub.Output, error) {
		w.mu.Lock()
		defer w.mu.Unlock()
		w.args = append(w.args, string(args))
		return toolhub.Output{Content: "written"}, nil
	})
	w.def.Description = "Test write_file"
	w.def.Parameters = toolhub.JSONSchema{
		Type: "object",
		Properties: toolhub.NewProperties(
			toolhub.Property{Name: "path", Schema: toolhub.JSONSchema{Type: "string"}},
			toolhub.Property{Name: "content", Schema: toolhub.JSONSchema{Type: "string"}},
		),
	}
	return w
}

// executed lists the raw arguments of the executions.
func (w *writeFileTool) executed() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]string(nil), w.args...)
}

// TestAgentCoreToolCalls holds the streaming halves of the invalid-arguments and
// tool-call-streaming cases: the cases that run a streamed tool call through a real
// AgentCore and Agent.ChatStream. The other cases test the providers and the argument
// parser, not AgentCore, and are covered in the provider packages (Phase 8).
func TestAgentCoreToolCalls(t *testing.T) {
	t.Run("answers the broken call with a tool error, never runs it, and lets the model retry", func(t *testing.T) {
		const badJSON = `{"path": "notes/report.md", "content": "He said "hi""}`
		const goodArgs = `{"path":"notes/report.md","content":"He said \"hi\""}`
		writeFile := newWriteFileTool()

		// Turn 1: a broken write_file call. Turn 2: the corrected call. Turn 3: text.
		broken := toolCallOf("tc-1", "write_file", "{}")
		broken.Arguments, broken.ArgumentsError = llmhub.ParseToolArguments(badJSON)
		l := newLoopCore(t, loopOptions{tools: []toolhub.Handler{writeFile}}, []aecmock.ScriptedTurn{
			toolTurn(broken),
			toolTurn(toolCallOf("tc-2", "write_file", goodArgs)),
			{Content: "Saved."},
		})
		defer l.dispose(t)

		chunks, err := drainStream(l.stream(t.Context(), "write the report"))
		if err != nil {
			t.Fatalf("ChatStream: %v", err)
		}

		for _, c := range chunks {
			if c.Type == kernel.ChunkError {
				t.Fatalf("an error chunk was yielded: %+v", c)
			}
		}
		if last := chunks[len(chunks)-1]; last.Type != kernel.ChunkDone {
			t.Fatalf("last chunk = %+v, want done", last)
		}
		// Only the corrected call ran.
		if got := writeFile.executed(); !reflect.DeepEqual(got, []string{goodArgs}) {
			t.Fatalf("executions = %q, want only the corrected call %q", got, goodArgs)
		}
		// The model saw a tool error for the broken call that explains what to fix.
		calls := l.provider.Calls()
		if len(calls) < 2 {
			t.Fatalf("the provider got %d calls, want at least 2", len(calls))
		}
		var brokenResult *kernel.ToolResult
		for _, m := range calls[1].Messages {
			for i := range m.ToolResults {
				if m.ToolResults[i].ToolCallID == "tc-1" {
					brokenResult = &m.ToolResults[i]
				}
			}
		}
		if brokenResult == nil {
			t.Fatalf("the model input after the broken call has no result for tc-1: %+v", calls[1].Messages)
		}
		if brokenResult.IsError == nil || !*brokenResult.IsError {
			t.Fatalf("the result for tc-1 = %+v, want an error", brokenResult)
		}
		if !strings.HasPrefix(brokenResult.Content, `Invalid arguments for "write_file": the arguments are not valid JSON`) ||
			!strings.Contains(brokenResult.Content, "valid JSON object") {
			t.Fatalf("the result for tc-1 = %q, want the invalid-arguments answer that names a valid JSON object", brokenResult.Content)
		}
	})

	t.Run("forwards start/delta chunks before the call and executes the tool exactly once", func(t *testing.T) {
		const fullArgs = `{"path":"notes/report.md","content":"# Report\nLine two"}`
		progress := func(delta string) *kernel.ToolCallProgress {
			return &kernel.ToolCallProgress{ID: "tc-1", Name: "write_file", ArgumentsDelta: delta}
		}
		call := toolCallOf("tc-1", "write_file", fullArgs)
		writeFile := newWriteFileTool()

		// First turn streams a write_file call with progress; the second answers in text.
		l := newLoopCore(t, loopOptions{tools: []toolhub.Handler{writeFile}}, []aecmock.ScriptedTurn{
			{Chunks: []kernel.StreamChunk{
				{Type: kernel.ChunkToolCallStart, ToolCallProgress: progress("")},
				{Type: kernel.ChunkToolCallDelta, ToolCallProgress: progress(`{"path": "notes/report.md", `)},
				{Type: kernel.ChunkToolCallDelta, ToolCallProgress: progress(`"content": "# Report\n`)},
				{Type: kernel.ChunkToolCallDelta, ToolCallProgress: progress(`Line two"}`)},
				{Type: kernel.ChunkToolCall, ToolCall: &call},
				{Type: kernel.ChunkDone},
			}},
			{Chunks: []kernel.StreamChunk{textChunk("Saved."), {Type: kernel.ChunkDone}}},
		})
		defer l.dispose(t)

		chunks, err := drainStream(l.stream(t.Context(), "write the report"))
		if err != nil {
			t.Fatalf("ChatStream: %v", err)
		}

		want := []kernel.ChunkType{
			kernel.ChunkToolCallStart, kernel.ChunkToolCallDelta, kernel.ChunkToolCallDelta, kernel.ChunkToolCallDelta,
			kernel.ChunkToolCall, kernel.ChunkToolResult, kernel.ChunkText, kernel.ChunkDone,
		}
		if got := chunkTypesOf(chunks); !reflect.DeepEqual(got, want) {
			t.Fatalf("chunk types = %v, want %v", got, want)
		}
		// Progress chunks never become calls: the tool ran once, with the parsed arguments.
		if got := writeFile.executed(); !reflect.DeepEqual(got, []string{fullArgs}) {
			t.Fatalf("executions = %q, want exactly one with %q", got, fullArgs)
		}
		// The history the model sees next holds only the complete call.
		calls := l.provider.Calls()
		if len(calls) < 2 {
			t.Fatalf("the provider got %d calls, want at least 2", len(calls))
		}
		var assistant *kernel.Message
		for i := range calls[1].Messages {
			if len(calls[1].Messages[i].ToolCalls) > 0 {
				assistant = &calls[1].Messages[i]
				break
			}
		}
		if assistant == nil || len(assistant.ToolCalls) != 1 || assistant.ToolCalls[0].ID != "tc-1" ||
			assistant.ToolCalls[0].Name != "write_file" || string(assistant.ToolCalls[0].Arguments) != fullArgs {
			t.Fatalf("the assistant message with the call = %+v, want only the complete call tc-1", assistant)
		}
	})
}
