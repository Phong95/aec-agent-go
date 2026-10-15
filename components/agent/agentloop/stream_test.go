package agentloop_test

import (
	"context"
	"errors"
	"iter"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/Phong95/aec-agent-go/aecmock"
	"github.com/Phong95/aec-agent-go/components/agent/agentloop"
	"github.com/Phong95/aec-agent-go/components/agent/profile"
	"github.com/Phong95/aec-agent-go/components/events"
	"github.com/Phong95/aec-agent-go/components/llmhub"
	"github.com/Phong95/aec-agent-go/components/toolhub"
	"github.com/Phong95/aec-agent-go/kernel"
)

// seq returns the Stream sequence of a rig for one user message.
func (r *rig) seq(ctx context.Context, userMessage string) iter.Seq2[kernel.StreamChunk, error] {
	return r.loop.Stream(ctx, userMessage, "s-1", llmhub.ModelOptions{}, agentloop.RunOptions{})
}

// stream drains Stream and returns the chunks and the first error of the error slot.
func (r *rig) stream(t *testing.T, userMessage string) ([]kernel.StreamChunk, error) {
	t.Helper()
	return drain(r.seq(t.Context(), userMessage))
}

// drain collects a sequence until it ends or reports an error.
func drain(seq iter.Seq2[kernel.StreamChunk, error]) ([]kernel.StreamChunk, error) {
	var chunks []kernel.StreamChunk
	for c, err := range seq {
		if err != nil {
			return chunks, err
		}
		chunks = append(chunks, c)
	}
	return chunks, nil
}

// chunkTypes lists the kinds of the chunks, in order.
func chunkTypes(chunks []kernel.StreamChunk) []kernel.ChunkType {
	out := make([]kernel.ChunkType, len(chunks))
	for i, c := range chunks {
		out[i] = c.Type
	}
	return out
}

// textOf is the content of a text chunk.
func textOf(c kernel.StreamChunk) string {
	if c.Content == nil {
		return ""
	}
	return *c.Content
}

// zeroUsage is the total of a run no done usage ever reached: zero counts and no
// cache counters.
func zeroUsage() kernel.TokenUsage { return kernel.TokenUsage{} }

// TestStream_EndTurn is the tracer of Stream (conformance cases C2 and C6): a
// streamed end_turn turn yields the text chunks and then the loop's own done chunk
// with the run's total usage, the events are the expected ones and ModelResponded
// carries the done chunk's usage as Run's does, every span is closed and
// the stored session carries the usage of the done chunk.
func TestStream_EndTurn(t *testing.T) {
	usage := kernel.TokenUsage{InputTokens: 10, OutputTokens: 5}
	r := newRig(t, rigOptions{}, aecmock.ScriptedTurn{Content: "Hello back!", Usage: &usage})
	chunks, err := r.stream(t, "Hello")
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}

	if got, want := chunkTypes(chunks), []kernel.ChunkType{kernel.ChunkText, kernel.ChunkText, kernel.ChunkUsage, kernel.ChunkDone}; !slices.Equal(got, want) {
		t.Fatalf("chunk types = %v, want %v", got, want)
	}
	if textOf(chunks[0]) != "Hello " || textOf(chunks[1]) != "back! " {
		t.Fatalf("text chunks = %q %q", textOf(chunks[0]), textOf(chunks[1]))
	}
	// The response's own usage, as the provider reported it, and saved with its message.
	if u := chunks[2].Usage; u == nil || !reflect.DeepEqual(*u, usage) {
		t.Fatalf("usage chunk = %+v, want %+v", chunks[2], usage)
	}
	if msgs := r.conv.Messages(); len(msgs) != 2 || msgs[1].Usage == nil || !reflect.DeepEqual(*msgs[1].Usage, usage) {
		t.Fatalf("history = %+v, want the answer saved with its usage %+v", msgs, usage)
	}
	done := chunks[3]
	wantTotal := kernel.TokenUsage{InputTokens: 10, OutputTokens: 5, CacheReadTokens: zeroInt(), CacheWriteTokens: zeroInt()}
	if done.Usage == nil || !reflect.DeepEqual(*done.Usage, wantTotal) || done.Aborted {
		t.Fatalf("done chunk = %+v, want the run total %+v and no aborted flag", done, wantTotal)
	}

	wantTypes := []string{"agent-loop:message", "agent-loop:iteration", "agent-loop:model-request", "agent-loop:model-response", "agent-loop:context-usage", "agent-loop:complete"}
	if got := r.log.types(); !slices.Equal(got, wantTypes) {
		t.Fatalf("event types = %v, want %v", got, wantTypes)
	}
	if got, want := r.log.payload(t, events.ModelResponded.Name), `{"agentId":"test-agent","stopReason":"end_turn","usage":{"inputTokens":10,"outputTokens":5},"hasToolCalls":false}`; got != want {
		t.Fatalf("ModelResponded = %s, want %s (the usage on the stream side too)", got, want)
	}
	if got, want := r.log.payload(t, events.RunCompleted.Name), `{"agentId":"test-agent","iterations":1,"totalUsage":{"inputTokens":10,"outputTokens":5,"cacheReadTokens":0,"cacheWriteTokens":0}}`; got != want {
		t.Fatalf("RunCompleted = %s, want %s", got, want)
	}

	// The stream side of the model-call end attributes: stop reason, flags and preview
	// first, the usage keys last (present only when the done chunk had usage).
	assertAttrKeys(t, r.sink, "end", "model-call", []string{"iteration", "toolCount", "toolNames", "inputMessageCount", "options", "stopReason", "hasToolCalls", "responsePreview", "inputTokens", "outputTokens"})
	if started, ended := r.sink.startedAndEnded(); started != ended {
		t.Fatalf("spans started %d, ended %d", started, ended)
	}

	msgs := r.conv.Messages()
	if len(msgs) != 2 || msgs[1].Role != kernel.RoleAssistant || msgs[1].Content != "Hello back! " || msgs[1].ToolCalls != nil {
		t.Fatalf("persisted messages = %+v", msgs)
	}
	stored, err := r.hub.Sessions.Get(t.Context(), mustSession(t, r.conv).ID)
	if err != nil || stored == nil || stored.CurrentTokenUsage == nil || stored.CurrentTokenUsage.InputTokens != 10 || stored.CurrentTokenUsage.OutputTokens != 5 {
		t.Fatalf("stored session = %+v (%v), want the usage of the done chunk", stored, err)
	}
}

// requireCleanedUp checks what a run that was cut short from outside (a consumer
// that stopped, a panic in its loop body) must leave behind: the partial reply as
// ONE assistant message, every span closed with the aborted marks, one
// RunCompleted{aborted}, and no ModelResponded.
func requireCleanedUp(t *testing.T, r *rig, wantPartial string) {
	t.Helper()
	msgs := r.conv.Messages()
	if len(msgs) != 2 || msgs[0].Role != kernel.RoleUser || msgs[1].Role != kernel.RoleAssistant || msgs[1].Content != wantPartial || msgs[1].ToolCalls != nil {
		t.Fatalf("persisted messages = %+v, want the user message and one assistant message %q", msgs, wantPartial)
	}
	if got := countOf(r, events.RunCompleted.Name); got != 1 {
		t.Fatalf("%d RunCompleted events, want 1", got)
	}
	if got, want := r.log.payload(t, events.RunCompleted.Name), `{"agentId":"test-agent","iterations":1,"totalUsage":{"inputTokens":0,"outputTokens":0},"aborted":true}`; got != want {
		t.Fatalf("RunCompleted = %s, want %s", got, want)
	}
	if countOf(r, events.ModelResponded.Name) != 0 {
		t.Fatal("ModelResponded was emitted for a response that never finished")
	}
	if started, ended := r.sink.startedAndEnded(); started != ended {
		t.Fatalf("spans started %d, ended %d: a span was left open", started, ended)
	}
	attrs, ok := r.sink.endAttrs("model-call")
	if !ok {
		t.Fatal("the model-call span never ended")
	}
	if v, _ := attrs.Get("aborted"); v != true {
		t.Fatalf("model-call end attributes = %v, want aborted true", attrs.Keys())
	}
	if v, _ := attrs.Get("responsePreview"); v != wantPartial {
		t.Fatalf("model-call responsePreview = %v, want %q", v, wantPartial)
	}
	for _, name := range []string{"iteration-1", "agent-run"} {
		a, ok := r.sink.endAttrs(name)
		if !ok {
			t.Fatalf("the %s span never ended", name)
		}
		if v, _ := a.Get("aborted"); v != true {
			t.Fatalf("%s end attributes = %v, want aborted true", name, a.Keys())
		}
	}
}

// TestStream_ConsumerBreakRunsCleanup: the consumer stops after the second text
// chunk. The deferred cleanup saves exactly what the consumer received as one
// assistant message, closes every span marked aborted, emits one RunCompleted with
// aborted, and does not yield again (the runtime would panic with "range function
// continued iteration"; the mutation proof is in the SUMMARY).
func TestStream_ConsumerBreakRunsCleanup(t *testing.T) {
	r := newRig(t, rigOptions{}, aecmock.ScriptedTurn{Content: "Hello big world"})
	var got []kernel.StreamChunk
	for c, err := range r.seq(t.Context(), "hi") {
		if err != nil {
			t.Fatalf("Stream: %v", err)
		}
		got = append(got, c)
		if len(got) == 2 {
			break
		}
	}
	if len(got) != 2 || textOf(got[0]) != "Hello " || textOf(got[1]) != "big " {
		t.Fatalf("chunks received = %+v", got)
	}
	requireCleanedUp(t, r, "Hello big ")
}

// TestStream_PanicInConsumerBodyRunsCleanup: a panic in the consumer's loop body
// unwinds through the sequence, and the cleanup still runs.
func TestStream_PanicInConsumerBodyRunsCleanup(t *testing.T) {
	r := newRig(t, rigOptions{}, aecmock.ScriptedTurn{Content: "Hello big world"})
	var recovered any
	func() {
		defer func() { recovered = recover() }()
		n := 0
		for _, err := range r.seq(t.Context(), "hi") {
			if err != nil {
				t.Errorf("Stream: %v", err)
				return
			}
			n++
			if n == 2 {
				panic("consumer failed")
			}
		}
	}()
	if recovered != "consumer failed" {
		t.Fatalf("recovered %v, want the consumer's panic to propagate", recovered)
	}
	requireCleanedUp(t, r, "Hello big ")
}

// TestStream_ToolTurnYieldsToolResultChunks (conformance case C4 shape): a tool_use
// turn runs the tool, yields one tool_result chunk per result in call order, and
// loops. The history holds the call message, ONE results message and the answer.
func TestStream_ToolTurnYieldsToolResultChunks(t *testing.T) {
	echo := constTool("echo", toolhub.Output{Content: "ok"})
	usage := &kernel.TokenUsage{InputTokens: 10, OutputTokens: 5}
	r := newRig(t, rigOptions{handlers: []toolhub.Handler{echo}},
		aecmock.ScriptedTurn{ToolCalls: []kernel.ToolCall{callOf("tc-1", "echo", "{}"), callOf("tc-2", "echo", "{}")}, Usage: usage},
		aecmock.ScriptedTurn{Content: "Done.", Usage: usage},
	)
	chunks, err := r.stream(t, "Use the tool")
	if err != nil {
		t.Fatal(err)
	}
	want := []kernel.ChunkType{kernel.ChunkToolCall, kernel.ChunkToolCall, kernel.ChunkUsage, kernel.ChunkToolResult, kernel.ChunkToolResult, kernel.ChunkText, kernel.ChunkUsage, kernel.ChunkDone}
	if got := chunkTypes(chunks); !slices.Equal(got, want) {
		t.Fatalf("chunk types = %v, want %v", got, want)
	}
	if chunks[3].ToolResult.ToolCallID != "tc-1" || chunks[4].ToolResult.ToolCallID != "tc-2" || chunks[3].ToolResult.Content != "ok" {
		t.Fatalf("tool results = %+v %+v, want one per call in call order", chunks[3].ToolResult, chunks[4].ToolResult)
	}
	if echo.executions() != 2 {
		t.Fatalf("%d executions, want 2", echo.executions())
	}
	msgs := r.conv.Messages()
	if len(msgs) != 4 || len(msgs[1].ToolCalls) != 2 || len(msgs[2].ToolResults) != 2 || msgs[2].Content != "" || msgs[3].Content != "Done. " {
		t.Fatalf("history = %+v, want user, call message, one results message, answer", msgs)
	}
	if done := chunks[len(chunks)-1]; done.Usage == nil || done.Usage.InputTokens != 20 || done.Usage.OutputTokens != 10 {
		t.Fatalf("done = %+v, want the usage of both turns", done)
	}
}

// TestStream_UnknownToolIsAnErrorResult (conformance case C4): a tool that is not
// registered does not crash the stream: one tool_result chunk with isError, and the
// turn ends with the model's next answer.
func TestStream_UnknownToolIsAnErrorResult(t *testing.T) {
	p := loopProfile(nil, zeroInt())
	r := newRig(t, rigOptions{prof: p},
		toolTurn(callOf("tc-1", "ghost", "{}")),
		aecmock.ScriptedTurn{Content: "I cannot use that tool."},
	)
	chunks, err := r.stream(t, "Use a missing tool")
	if err != nil {
		t.Fatal(err)
	}
	var results []kernel.ToolResult
	for _, c := range chunks {
		if c.Type == kernel.ChunkToolResult {
			results = append(results, *c.ToolResult)
		}
	}
	if len(results) != 1 || results[0].IsError == nil || !*results[0].IsError || results[0].Content != `Tool "ghost" not available for this agent` {
		t.Fatalf("tool_result chunks = %+v, want one error result", results)
	}
	if last := chunks[len(chunks)-1]; last.Type != kernel.ChunkDone {
		t.Fatalf("the stream ended with %+v, want done", last)
	}
}

// TestStream_ForwardsToolCallProgress (tool-call streaming case S1):
// tool_call_start and three tool_call_delta chunks reach the consumer before the
// complete tool_call, the tool runs once, and the history holds only the complete call.
func TestStream_ForwardsToolCallProgress(t *testing.T) {
	write := constTool("write_file", toolhub.Output{Content: "written"})
	full := `{"path":"notes/report.md","content":"# Report\nLine two"}`
	progress := func(delta string) *kernel.ToolCallProgress {
		return &kernel.ToolCallProgress{ID: "tc-1", Name: "write_file", ArgumentsDelta: delta}
	}
	call := callOf("tc-1", "write_file", full)
	text := "Saved."
	r := newRig(t, rigOptions{handlers: []toolhub.Handler{write}},
		aecmock.ScriptedTurn{Chunks: []kernel.StreamChunk{
			{Type: kernel.ChunkToolCallStart, ToolCallProgress: progress("")},
			{Type: kernel.ChunkToolCallDelta, ToolCallProgress: progress(`{"path": "notes/report.md", `)},
			{Type: kernel.ChunkToolCallDelta, ToolCallProgress: progress(`"content": "# Report\n`)},
			{Type: kernel.ChunkToolCallDelta, ToolCallProgress: progress(`Line two"}`)},
			{Type: kernel.ChunkToolCall, ToolCall: &call},
			{Type: kernel.ChunkDone},
		}},
		aecmock.ScriptedTurn{Chunks: []kernel.StreamChunk{{Type: kernel.ChunkText, Content: &text}, {Type: kernel.ChunkDone}}},
	)
	chunks, err := r.stream(t, "write the report")
	if err != nil {
		t.Fatal(err)
	}
	want := []kernel.ChunkType{
		kernel.ChunkToolCallStart, kernel.ChunkToolCallDelta, kernel.ChunkToolCallDelta, kernel.ChunkToolCallDelta,
		kernel.ChunkToolCall, kernel.ChunkToolResult, kernel.ChunkText, kernel.ChunkDone,
	}
	if got := chunkTypes(chunks); !slices.Equal(got, want) {
		t.Fatalf("chunk types = %v, want %v", got, want)
	}
	if write.executions() != 1 {
		t.Fatalf("the tool ran %d times, want once (progress chunks never become calls)", write.executions())
	}
	msgs := r.conv.Messages()
	if len(msgs) != 4 || len(msgs[1].ToolCalls) != 1 || msgs[1].ToolCalls[0].ID != "tc-1" || string(msgs[1].ToolCalls[0].Arguments) != full {
		t.Fatalf("history = %+v, want only the complete call", msgs)
	}
}

// TestStream_ErrorChunkEndsTheTurn: an error chunk mid-stream ends the turn with
// RunCompleted and a done chunk whose usage is zero (no done usage ever arrived, so
// the cache counters stay absent), ModelResponded says error, the partial text is
// persisted and the stored session is not given usage.
func TestStream_ErrorChunkEndsTheTurn(t *testing.T) {
	partial := "partial "
	r := newRig(t, rigOptions{}, aecmock.ScriptedTurn{Chunks: []kernel.StreamChunk{
		{Type: kernel.ChunkText, Content: &partial},
		{Type: kernel.ChunkError, Error: "boom"},
	}})
	chunks, err := r.stream(t, "Hello")
	if err != nil {
		t.Fatal(err)
	}
	want := []kernel.ChunkType{kernel.ChunkText, kernel.ChunkError, kernel.ChunkDone}
	if got := chunkTypes(chunks); !slices.Equal(got, want) {
		t.Fatalf("chunk types = %v, want %v", got, want)
	}
	if chunks[1].Error != "boom" {
		t.Fatalf("error chunk = %+v", chunks[1])
	}
	done := chunks[2]
	if done.Usage == nil || !reflect.DeepEqual(*done.Usage, zeroUsage()) {
		t.Fatalf("done usage = %+v, want {0 0} without cache fields", done.Usage)
	}
	if got, want := r.log.payload(t, events.ModelResponded.Name), `{"agentId":"test-agent","stopReason":"error","hasToolCalls":false}`; got != want {
		t.Fatalf("ModelResponded = %s, want %s", got, want)
	}
	if countOf(r, events.RunCompleted.Name) != 1 {
		t.Fatalf("event types = %v, want RunCompleted", r.log.types())
	}
	msgs := r.conv.Messages()
	if len(msgs) != 2 || msgs[1].Content != "partial " {
		t.Fatalf("persisted messages = %+v, want the partial text", msgs)
	}
	stored, err := r.hub.Sessions.Get(t.Context(), mustSession(t, r.conv).ID)
	if err != nil || stored == nil || stored.CurrentTokenUsage != nil {
		t.Fatalf("stored session = %+v (%v), want no usage: no done usage arrived", stored, err)
	}
}

// TestStream_CapEndsWithAnErrorChunk (case L9): at the iteration cap the stream yields
// the tool results and then one error chunk; MaxIterationsReached is emitted, and
// there is no done chunk and no RunCompleted.
func TestStream_CapEndsWithAnErrorChunk(t *testing.T) {
	echo := constTool("echo", toolhub.Output{Content: "ok"})
	p := testProfile("test-agent")
	p.LoopConfig = &profile.LoopConfig{MaxIterations: intPtr(1)}
	r := newRig(t, rigOptions{prof: &p, handlers: []toolhub.Handler{echo}},
		aecmock.ScriptedTurn{ToolCalls: []kernel.ToolCall{callOf("tc-1", "echo", "{}")}, Usage: &kernel.TokenUsage{InputTokens: 10, OutputTokens: 5}},
	)
	chunks, err := r.stream(t, "Use the tool")
	if err != nil {
		t.Fatal(err)
	}
	want := []kernel.ChunkType{kernel.ChunkToolCall, kernel.ChunkUsage, kernel.ChunkToolResult, kernel.ChunkError}
	if got := chunkTypes(chunks); !slices.Equal(got, want) {
		t.Fatalf("chunk types = %v, want %v", got, want)
	}
	if chunks[3].Error != "Reached maximum reasoning steps." {
		t.Fatalf("error chunk = %q", chunks[3].Error)
	}
	if countOf(r, events.Iteration.Name) != 1 || countOf(r, events.MaxIterationsReached.Name) != 1 || countOf(r, events.RunCompleted.Name) != 0 {
		t.Fatalf("event types = %v, want one Iteration, MaxIterationsReached and no RunCompleted", r.log.types())
	}
	if got, want := r.log.payload(t, events.MaxIterationsReached.Name), `{"agentId":"test-agent","iterations":1,"totalUsage":{"inputTokens":10,"outputTokens":5,"cacheReadTokens":0,"cacheWriteTokens":0}}`; got != want {
		t.Fatalf("MaxIterationsReached = %s, want %s", got, want)
	}
	if started, ended := r.sink.startedAndEnded(); started != ended {
		t.Fatalf("spans started %d, ended %d", started, ended)
	}
}

// stopCase is one row of TestRunVsStreamStopReasons: how the same stop reason is
// scripted on each path, and what each path must do.
type stopCase struct {
	name       string
	runTurn    aecmock.ScriptedTurn
	streamTurn aecmock.ScriptedTurn
	// Run: whether it ends the turn on the first iteration (RunCompleted), or loops
	// to the cap (MaxIterationsReached).
	runEndsTurn bool
	// Stream: whether it ends the turn on the first iteration with a done chunk, or
	// loops to the cap and ends with the cap error chunk.
	streamEndsTurn bool
	// runStop is the RunResult.StopReason Run reports.
	runStop kernel.StopReason
}

// TestRunVsStreamStopReasons pins the one stop-reason rule of Run and Stream for
// every stop reason and for caps of 1 and 3: both end the turn on end_turn,
// max_tokens and error, and loop only on tool_use with calls. A stream chunk
// carries no stop reason, so Stream sees a provider max_tokens as end_turn; it ends
// the turn either way. Run reports the stop reason of its last response.
// Since A-88 the done chunk carries the stop reason (the scripted turn reports its
// StopReason), so Stream sees max_tokens as max_tokens; a text-only max_tokens still
// ends the turn (max_tokens_test.go pins a max_tokens response WITH tool calls).
func TestRunVsStreamStopReasons(t *testing.T) {
	boom := "boom"
	cases := []stopCase{
		{
			name:           "end_turn",
			runTurn:        aecmock.ScriptedTurn{Content: "done", StopReason: kernel.StopEndTurn},
			streamTurn:     aecmock.ScriptedTurn{Content: "done"},
			runEndsTurn:    true,
			streamEndsTurn: true,
			runStop:        kernel.StopEndTurn,
		},
		{
			name:       "tool_use",
			runTurn:    aecmock.ScriptedTurn{ToolCalls: []kernel.ToolCall{callOf("tc-1", "echo", "{}")}},
			streamTurn: aecmock.ScriptedTurn{ToolCalls: []kernel.ToolCall{callOf("tc-1", "echo", "{}")}},
			runStop:    kernel.StopToolUse,
		},
		{
			name:           "max_tokens",
			runTurn:        aecmock.ScriptedTurn{Content: "cut off", StopReason: kernel.StopMaxTokens},
			streamTurn:     aecmock.ScriptedTurn{Content: "cut off", StopReason: kernel.StopMaxTokens},
			runEndsTurn:    true,
			streamEndsTurn: true,
			runStop:        kernel.StopMaxTokens,
		},
		{
			name:           "error",
			runTurn:        aecmock.ScriptedTurn{Content: "failed", StopReason: kernel.StopError},
			streamTurn:     aecmock.ScriptedTurn{Chunks: []kernel.StreamChunk{{Type: kernel.ChunkError, Error: boom}}},
			runEndsTurn:    true,
			streamEndsTurn: true,
			runStop:        kernel.StopError,
		},
		{
			// A tool_use answer that carries no call has nothing to run: it ends the turn.
			name:           "tool_use without calls",
			runTurn:        aecmock.ScriptedTurn{Content: "nothing to call", StopReason: kernel.StopToolUse},
			streamTurn:     aecmock.ScriptedTurn{Content: "nothing to call"},
			runEndsTurn:    true,
			streamEndsTurn: true,
			runStop:        kernel.StopToolUse,
		},
	}
	for _, maxIter := range []int{1, 3} {
		for _, c := range cases {
			t.Run(c.name+" cap "+strconv.Itoa(maxIter), func(t *testing.T) {
				p := testProfile("test-agent")
				p.LoopConfig = &profile.LoopConfig{MaxIterations: intPtr(maxIter)}
				script := func(turn aecmock.ScriptedTurn) []aecmock.ScriptedTurn {
					turns := make([]aecmock.ScriptedTurn, maxIter)
					for i := range turns {
						turns[i] = turn
					}
					return turns
				}

				// Run.
				runRig := newRig(t, rigOptions{prof: &p, handlers: []toolhub.Handler{constTool("echo", toolhub.Output{Content: "ok"})}}, script(c.runTurn)...)
				res, err := runRig.run(t, "go")
				if err != nil {
					t.Fatalf("Run: %v", err)
				}
				wantIter := maxIter
				if c.runEndsTurn {
					wantIter = 1
				}
				if res.Iterations != wantIter || res.Aborted || res.StopReason != c.runStop {
					t.Fatalf("Run result = %+v, want %d iterations and stop reason %q", res, wantIter, c.runStop)
				}
				if got := countOf(runRig, events.RunCompleted.Name); got != btoi(c.runEndsTurn) {
					t.Fatalf("Run: %d RunCompleted events, want %d (%v)", got, btoi(c.runEndsTurn), runRig.log.types())
				}
				if got := countOf(runRig, events.MaxIterationsReached.Name); got != btoi(!c.runEndsTurn) {
					t.Fatalf("Run: %d MaxIterationsReached events, want %d", got, btoi(!c.runEndsTurn))
				}

				// Stream.
				streamRig := newRig(t, rigOptions{prof: &p, handlers: []toolhub.Handler{constTool("echo", toolhub.Output{Content: "ok"})}}, script(c.streamTurn)...)
				chunks, err := streamRig.stream(t, "go")
				if err != nil {
					t.Fatalf("Stream: %v", err)
				}
				last := chunks[len(chunks)-1]
				wantStreamIter := maxIter
				if c.streamEndsTurn {
					wantStreamIter = 1
					if last.Type != kernel.ChunkDone || last.Aborted {
						t.Fatalf("Stream ended with %+v, want a done chunk", last)
					}
				} else if last.Type != kernel.ChunkError || last.Error != "Reached maximum reasoning steps." {
					t.Fatalf("Stream ended with %+v, want the cap error chunk", last)
				}
				if got := countOf(streamRig, events.Iteration.Name); got != wantStreamIter {
					t.Fatalf("Stream: %d iterations, want %d", got, wantStreamIter)
				}
				if got := countOf(streamRig, events.RunCompleted.Name); got != btoi(c.streamEndsTurn) {
					t.Fatalf("Stream: %d RunCompleted events, want %d", got, btoi(c.streamEndsTurn))
				}
				if got := countOf(streamRig, events.MaxIterationsReached.Name); got != btoi(!c.streamEndsTurn) {
					t.Fatalf("Stream: %d MaxIterationsReached events, want %d", got, btoi(!c.streamEndsTurn))
				}
			})
		}
	}
}

func btoi(b bool) int {
	if b {
		return 1
	}
	return 0
}

// scriptedRunAndStream runs the same two-turn tool script through Run on one rig and
// through Stream on another, with equal tools and usage.
func scriptedRunAndStream(t *testing.T) (runRig, streamRig *rig) {
	t.Helper()
	usage := &kernel.TokenUsage{InputTokens: 10, OutputTokens: 5}
	turns := []aecmock.ScriptedTurn{
		{ToolCalls: []kernel.ToolCall{callOf("tc-1", "echo", "{}")}, Usage: usage},
		{Content: "Done.", Usage: usage},
	}
	runRig = newRig(t, rigOptions{handlers: []toolhub.Handler{constTool("echo", toolhub.Output{Content: "ok"})}}, turns...)
	if _, err := runRig.run(t, "Use the tool"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	streamRig = newRig(t, rigOptions{handlers: []toolhub.Handler{constTool("echo", toolhub.Output{Content: "ok"})}}, turns...)
	if _, err := streamRig.stream(t, "Use the tool"); err != nil {
		t.Fatalf("Stream: %v", err)
	}
	return runRig, streamRig
}

// TestRunAndStreamPersistSameMessages (conformance case C5): the same script persists
// the same messages on both paths, compared by role, whether there is content, and
// the number of calls and results (the stub streams words with a trailing space, so
// the text itself differs).
func TestRunAndStreamPersistSameMessages(t *testing.T) {
	type shape struct {
		role       kernel.Role
		hasContent bool
		calls      int
		results    int
	}
	shapes := func(msgs []kernel.Message) []shape {
		out := make([]shape, len(msgs))
		for i, m := range msgs {
			out[i] = shape{m.Role, m.Content != "", len(m.ToolCalls), len(m.ToolResults)}
		}
		return out
	}
	runRig, streamRig := scriptedRunAndStream(t)
	if got, want := shapes(streamRig.conv.Messages()), shapes(runRig.conv.Messages()); !slices.Equal(got, want) {
		t.Fatalf("stream persisted %+v, run persisted %+v", got, want)
	}
}

// TestRunAndStreamEmitSameEventTypes (conformance case C6): both paths emit the same
// event type sequence for the same script.
func TestRunAndStreamEmitSameEventTypes(t *testing.T) {
	runRig, streamRig := scriptedRunAndStream(t)
	if got, want := streamRig.log.types(), runRig.log.types(); !slices.Equal(got, want) {
		t.Fatalf("stream emitted %v, run emitted %v", got, want)
	}
}

// TestStream_InvalidArgumentsNeverExecuted (invalid-arguments case I1):
// a call whose arguments could not be parsed is answered with a tool error and never
// reaches the policy, the approval gate or the tool; the model then calls it again
// with valid arguments and that call runs once.
func TestStream_InvalidArgumentsNeverExecuted(t *testing.T) {
	write := constTool("write_file", toolhub.Output{Content: "written"})
	write.def.Risk = toolhub.RiskConsequential // would ask for approval if it got that far
	handler, reqs := recordingHandler(approve())
	broken := callOf("tc-1", "write_file", "{}")
	broken.ArgumentsError = "the arguments are not valid JSON (Unterminated string in JSON at position 52)"
	r := newRig(t, rigOptions{handlers: []toolhub.Handler{write}, interaction: handler},
		aecmock.ScriptedTurn{ToolCalls: []kernel.ToolCall{broken}},
		aecmock.ScriptedTurn{ToolCalls: []kernel.ToolCall{callOf("tc-2", "write_file", `{"path":"a"}`)}},
		aecmock.ScriptedTurn{Content: "Saved."},
	)
	chunks, err := r.stream(t, "write the report")
	if err != nil {
		t.Fatal(err)
	}
	var results []kernel.ToolResult
	for _, c := range chunks {
		if c.Type == kernel.ChunkToolResult {
			results = append(results, *c.ToolResult)
		}
	}
	if len(results) != 2 {
		t.Fatalf("%d tool_result chunks, want 2", len(results))
	}
	first := results[0]
	if first.IsError == nil || !*first.IsError ||
		!strings.HasPrefix(first.Content, `Invalid arguments for "write_file": the arguments are not valid JSON`) ||
		!strings.Contains(first.Content, "valid JSON object") {
		t.Fatalf("first result = %+v, want the invalid-arguments answer", first)
	}
	if write.executions() != 1 {
		t.Fatalf("the tool ran %d times, want only the corrected call", write.executions())
	}
	if countOf(r, events.ToolDenied.Name) != 0 {
		t.Fatal("a ToolDenied event was emitted for a call that never ran")
	}
	// Only the corrected call reached the approval gate: the broken one never did.
	if len(*reqs) != 1 || (*reqs)[0].ToolCallID != "tc-2" || countOf(r, events.ToolApprovalRequested.Name) != 1 {
		t.Fatalf("approval requests = %+v, want exactly one, for the corrected call tc-2", *reqs)
	}
	if got, want := r.log.payload(t, events.ToolFailed.Name), `{"agentId":"test-agent","name":"write_file","error":"invalid-arguments"}`; got != want {
		t.Fatalf("ToolFailed = %s, want %s", got, want)
	}
}

// failingStreamModels is a Models whose Stream puts an error in the error slot while
// the ctx is alive, which the provider contract never does but a decorator might.
type failingStreamModels struct {
	*aecmock.ScriptedModels
	err error
}

func (m failingStreamModels) Stream(context.Context, []kernel.Message, llmhub.ModelOptions) iter.Seq2[kernel.StreamChunk, error] {
	return func(yield func(kernel.StreamChunk, error) bool) {
		yield(kernel.StreamChunk{}, m.err)
	}
}

// TestStream_ErrorSlotWithLiveCtxBecomesAnErrorChunk: an error in the provider's
// error slot while the run's ctx is alive is a failed response, not a stop: a
// terminal error chunk with the error text, and the turn ends.
func TestStream_ErrorSlotWithLiveCtxBecomesAnErrorChunk(t *testing.T) {
	r := newRig(t, rigOptions{})
	deps := r.deps
	deps.Models = failingStreamModels{ScriptedModels: r.models, err: errors.New("decorator failed")}
	loop, err := agentloop.New(deps)
	if err != nil {
		t.Fatal(err)
	}
	chunks, err := drain(loop.Stream(t.Context(), "hi", "s-1", llmhub.ModelOptions{}, agentloop.RunOptions{}))
	if err != nil {
		t.Fatal(err)
	}
	want := []kernel.ChunkType{kernel.ChunkError, kernel.ChunkDone}
	if got := chunkTypes(chunks); !slices.Equal(got, want) || chunks[0].Error != "decorator failed" || chunks[1].Aborted {
		t.Fatalf("chunks = %+v, want an error chunk with the text and a done chunk", chunks)
	}
}

// TestStream_NoActiveSession: the precondition failure is the one error of the
// error slot, nothing else is yielded, and nothing was started.
func TestStream_NoActiveSession(t *testing.T) {
	r := newRig(t, rigOptions{noStart: true})
	n := 0
	var gotErr error
	for c, err := range r.seq(t.Context(), "hi") {
		n++
		if err == nil {
			t.Fatalf("a chunk %+v was yielded without a session", c)
		}
		gotErr = err
	}
	if n != 1 || !errors.Is(gotErr, agentloop.ErrNoActiveSession) {
		t.Fatalf("%d yields, error %v, want exactly one ErrNoActiveSession", n, gotErr)
	}
	if got := r.log.types(); len(got) != 0 {
		t.Fatalf("events = %v, want none", got)
	}
	if recs := r.sink.all(); len(recs) != 0 {
		t.Fatalf("%d span notifications, want none", len(recs))
	}
}

// TestStream_RejectsInactiveSessionID (IN-05): the mismatch is yielded once, in the
// error slot, before anything is persisted or emitted.
func TestStream_RejectsInactiveSessionID(t *testing.T) {
	r := newRig(t, rigOptions{}, aecmock.ScriptedTurn{Content: "never"})
	var pairs int
	var gotErr error
	for c, err := range r.loop.Stream(t.Context(), "hi", "some-other-session", llmhub.ModelOptions{}, agentloop.RunOptions{}) {
		pairs++
		gotErr = err
		if !reflect.DeepEqual(c, kernel.StreamChunk{}) {
			t.Fatalf("chunk = %+v, want the zero chunk", c)
		}
	}
	if pairs != 1 || !errors.Is(gotErr, agentloop.ErrSessionNotActive) {
		t.Fatalf("Stream yielded %d pairs with error %v, want exactly one pair matching ErrSessionNotActive", pairs, gotErr)
	}
	if n := len(r.conv.Messages()); n != 0 || len(r.log.types()) != 0 || len(r.models.Calls()) != 0 {
		t.Fatalf("a rejected stream persisted %d messages, emitted %v and called the model %d times", n, r.log.types(), len(r.models.Calls()))
	}
}

// The shared stop rule and post-processing of Stream.

// rewritingContext is a Context port whose afterModel step rewrites the reply: the
// content gets a prefix and a tool_use reply loses its calls.
type rewritingContext struct{ aecmock.PassthroughContext }

func (rewritingContext) ProcessResponse(_ context.Context, resp kernel.ModelResponse, _ agentloop.MiddlewareContext) (kernel.ModelResponse, error) {
	resp.Message = resp.Message.Clone()
	resp.Message.Content = "[checked] " + resp.Message.Content
	resp.Message.ToolCalls = nil
	if resp.StopReason == kernel.StopToolUse {
		resp.StopReason = kernel.StopEndTurn
	}
	return resp, nil
}

// TestStream_PostProcessesTheReplyLikeRun: Stream runs the
// afterModel step on the collected reply, as Run does, after the chunks were yielded.
// The processed reply is what is saved and what decides the turn: here it drops the
// tool call, so the tool never runs and the turn ends.
func TestStream_PostProcessesTheReplyLikeRun(t *testing.T) {
	echo := constTool("echo", toolhub.Output{Content: "echoed"})
	turn := aecmock.ScriptedTurn{Content: "calling", ToolCalls: []kernel.ToolCall{callOf("tc-1", "echo", "{}")}}
	for _, mode := range []string{"run", "stream"} {
		t.Run(mode, func(t *testing.T) {
			rg := newRig(t, rigOptions{handlers: []toolhub.Handler{echo}, context: rewritingContext{}}, turn)
			switch mode {
			case "run":
				res, err := rg.run(t, "go")
				if err != nil || res.Message.Content != "[checked] calling" || res.StopReason != kernel.StopEndTurn {
					t.Fatalf("Run = %+v, %v", res, err)
				}
			case "stream":
				chunks, err := rg.stream(t, "go")
				if err != nil {
					t.Fatal(err)
				}
				if last := chunks[len(chunks)-1]; last.Type != kernel.ChunkDone || last.Aborted {
					t.Fatalf("last chunk = %+v, want done", last)
				}
			}
			msgs := rg.conv.Messages()
			// The scripted stream sends "calling " (word chunks end with a space).
			if len(msgs) != 2 || strings.TrimSpace(msgs[1].Content) != "[checked] calling" || len(msgs[1].ToolCalls) != 0 {
				t.Fatalf("persisted = %+v, want the processed reply", msgs)
			}
			if echo.executions() != 0 {
				t.Fatal("the tool call the afterModel step dropped ran")
			}
			if got := rg.log.payload(t, events.ModelResponded.Name); !strings.Contains(got, `"stopReason":"end_turn"`) || !strings.Contains(got, `"hasToolCalls":false`) {
				t.Fatalf("ModelResponded = %s, want the processed stop reason", got)
			}
		})
	}
}
