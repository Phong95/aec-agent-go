package agentloop_test

import (
	"strings"
	"testing"

	"github.com/Phong95/aec-agent-go/aecmock"
	"github.com/Phong95/aec-agent-go/components/agent/agentloop"
	"github.com/Phong95/aec-agent-go/components/events"
	"github.com/Phong95/aec-agent-go/components/llmhub"
	"github.com/Phong95/aec-agent-go/components/toolhub"
	"github.com/Phong95/aec-agent-go/kernel"
	"github.com/Phong95/aec-agent-go/kernel/ordered"
)

// The A-88 tests: a model response cut off at the output token limit (max_tokens).
// Every behaviour is pinned for Run and for Stream, which share the rule.

// loopModes are the two ways a turn is played.
var loopModes = []string{"Run", "Stream"}

// turnOutcome is what one turn produced: the result of Run, or the chunks of Stream.
type turnOutcome struct {
	res    agentloop.RunResult
	chunks []kernel.StreamChunk
}

// playTurn plays one turn on r through mode with the given option overrides.
func playTurn(t *testing.T, r *rig, mode string, opts llmhub.ModelOptions) turnOutcome {
	t.Helper()
	switch mode {
	case "Run":
		res, err := r.loop.Run(t.Context(), "write the report", "s-1", opts, agentloop.RunOptions{})
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		return turnOutcome{res: res}
	default:
		chunks, err := drain(r.loop.Stream(t.Context(), "write the report", "s-1", opts, agentloop.RunOptions{}))
		if err != nil {
			t.Fatalf("Stream: %v", err)
		}
		if len(chunks) == 0 {
			t.Fatal("Stream yielded no chunk")
		}
		return turnOutcome{chunks: chunks}
	}
}

// last is the last chunk of a Stream outcome.
func (o turnOutcome) last() kernel.StreamChunk { return o.chunks[len(o.chunks)-1] }

// streamedResults are the tool_result chunks of a Stream outcome, by call id.
func (o turnOutcome) streamedResults() map[string]kernel.ToolResult {
	out := map[string]kernel.ToolResult{}
	for _, c := range o.chunks {
		if c.Type == kernel.ChunkToolResult && c.ToolResult != nil {
			out[c.ToolResult.ToolCallID] = *c.ToolResult
		}
	}
	return out
}

// savedResults are the tool results saved in the conversation, by call id. It fails
// the test when a saved tool call has no saved result (a dangling call).
func savedResults(t *testing.T, r *rig) map[string]kernel.ToolResult {
	t.Helper()
	results := map[string]kernel.ToolResult{}
	var calls []string
	for _, m := range r.conv.Messages() {
		for _, c := range m.ToolCalls {
			calls = append(calls, c.ID)
		}
		for _, res := range m.ToolResults {
			results[res.ToolCallID] = res
		}
	}
	for _, id := range calls {
		if _, ok := results[id]; !ok {
			t.Fatalf("tool call %s has no saved result: %+v", id, r.conv.Messages())
		}
	}
	return results
}

// attrOf is the value of a span attribute, nil when absent.
func attrOf(o ordered.Object, key string) any {
	v, _ := o.Get(key)
	return v
}

// cutOffCall is a tool call whose arguments were cut off mid-JSON.
func cutOffCall(id string) kernel.ToolCall {
	return kernel.ToolCall{
		ID:             id,
		Name:           "write_file",
		Arguments:      kernel.Arguments(`{}`),
		ArgumentsError: `the arguments are not valid JSON (unexpected end of JSON input): {"path":"a.md","content":"abc`,
	}
}

// truncationRig is a rig with a write_file and an echo tool that count their runs.
func truncationRig(t *testing.T, turns ...aecmock.ScriptedTurn) (*rig, *fakeTool, *fakeTool) {
	t.Helper()
	write := constTool("write_file", toolhub.Output{Content: "written"})
	echo := constTool("echo", toolhub.Output{Content: "ok"})
	return newRig(t, rigOptions{handlers: []toolhub.Handler{write, echo}}, turns...), write, echo
}

// requireOutputLimitError requires res to be the output-token-limit error of the
// write_file call; withLimit is the "(maxTokens=N)" part it must carry, or "" when it
// must carry none.
func requireOutputLimitError(t *testing.T, res kernel.ToolResult, withLimit string) {
	t.Helper()
	if res.IsError == nil || !*res.IsError {
		t.Fatalf("result %+v is not an error", res)
	}
	if !strings.Contains(res.Content, "output token limit") || !strings.Contains(res.Content, `"write_file"`) ||
		!strings.Contains(res.Content, "so this call was not run") {
		t.Fatalf("result %q is not the output-token-limit error", res.Content)
	}
	if withLimit == "" && strings.Contains(res.Content, "maxTokens=") {
		t.Fatalf("result %q names a maxTokens that is not configured", res.Content)
	}
	if withLimit != "" && !strings.Contains(res.Content, withLimit) {
		t.Fatalf("result %q does not name %s", res.Content, withLimit)
	}
}

// requireNormalEnd requires the turn to have ended normally after two iterations.
func requireNormalEnd(t *testing.T, mode string, o turnOutcome) {
	t.Helper()
	if mode == "Run" {
		if o.res.Iterations != 2 || o.res.StopReason != kernel.StopEndTurn || o.res.Message.Content != "done" {
			t.Fatalf("Run result = %+v, want the end_turn answer of iteration 2", o.res)
		}
		return
	}
	if last := o.last(); last.Type != kernel.ChunkDone || last.StopReason != "" || last.Aborted {
		t.Fatalf("Stream ended with %+v, want a plain done chunk", last)
	}
}

// TestMaxTokensCutOffCallIsNotRun: a call cut off mid-arguments in a max_tokens
// response is never run; it is answered with the output-token-limit error, and the
// loop continues.
func TestMaxTokensCutOffCallIsNotRun(t *testing.T) {
	for _, mode := range loopModes {
		t.Run(mode, func(t *testing.T) {
			r, write, _ := truncationRig(t,
				aecmock.ScriptedTurn{ToolCalls: []kernel.ToolCall{cutOffCall("c1")}, StopReason: kernel.StopMaxTokens},
				aecmock.ScriptedTurn{Content: "done"},
			)
			o := playTurn(t, r, mode, llmhub.ModelOptions{})
			if write.executions() != 0 {
				t.Fatalf("write_file ran %d times, want never", write.executions())
			}
			requireOutputLimitError(t, savedResults(t, r)["c1"], "")
			if mode == "Stream" {
				requireOutputLimitError(t, o.streamedResults()["c1"], "")
			}
			requireNormalEnd(t, mode, o)
			if got := len(r.models.Calls()); got != 2 {
				t.Fatalf("the model was asked %d times, want 2", got)
			}
		})
	}
}

// TestMaxTokensErrorNamesConfiguredMaxTokens: the error names maxTokens when the
// options configure it.
func TestMaxTokensErrorNamesConfiguredMaxTokens(t *testing.T) {
	for _, mode := range loopModes {
		t.Run(mode, func(t *testing.T) {
			r, _, _ := truncationRig(t,
				aecmock.ScriptedTurn{ToolCalls: []kernel.ToolCall{cutOffCall("c1")}, StopReason: kernel.StopMaxTokens},
				aecmock.ScriptedTurn{Content: "done"},
			)
			playTurn(t, r, mode, llmhub.ModelOptions{MaxTokens: llmhub.Int(512)})
			requireOutputLimitError(t, savedResults(t, r)["c1"], "(maxTokens=512)")
		})
	}
}

// TestMaxTokensLastCallIsCutOffAndEarlierCallsRun: the LAST call of a max_tokens
// response counts as cut off even when its arguments parsed (a non-streaming provider
// hands over pre-parsed inputs); an earlier complete call of the same response runs.
func TestMaxTokensLastCallIsCutOffAndEarlierCallsRun(t *testing.T) {
	for _, mode := range loopModes {
		t.Run(mode, func(t *testing.T) {
			r, write, echo := truncationRig(t,
				aecmock.ScriptedTurn{
					ToolCalls: []kernel.ToolCall{
						callOf("c1", "echo", `{}`),
						callOf("c2", "write_file", `{"path":"a.md"}`),
					},
					StopReason: kernel.StopMaxTokens,
				},
				aecmock.ScriptedTurn{Content: "done"},
			)
			o := playTurn(t, r, mode, llmhub.ModelOptions{})
			if echo.executions() != 1 || write.executions() != 0 {
				t.Fatalf("echo ran %d times and write_file %d times, want 1 and 0", echo.executions(), write.executions())
			}
			saved := savedResults(t, r)
			if res := saved["c1"]; res.Content != "ok" || (res.IsError != nil && *res.IsError) {
				t.Fatalf("echo result = %+v, want its output", res)
			}
			requireOutputLimitError(t, saved["c2"], "")
			requireNormalEnd(t, mode, o)
		})
	}
}

// TestMaxTokensTextOnlyEndsTheTurn: a text reply cut off at max_tokens ends the turn,
// without asking again: Run reports StopReason max_tokens and Stream's done chunk
// carries it. The run completes with the stop reason on the run span.
func TestMaxTokensTextOnlyEndsTheTurn(t *testing.T) {
	for _, mode := range loopModes {
		t.Run(mode, func(t *testing.T) {
			r, _, _ := truncationRig(t,
				aecmock.ScriptedTurn{Content: "Part one", StopReason: kernel.StopMaxTokens},
				aecmock.ScriptedTurn{Content: "never asked"},
			)
			o := playTurn(t, r, mode, llmhub.ModelOptions{})
			if mode == "Run" {
				if o.res.StopReason != kernel.StopMaxTokens || o.res.Iterations != 1 || o.res.Message.Content != "Part one" {
					t.Fatalf("Run result = %+v, want the cut reply with stop reason max_tokens", o.res)
				}
			} else if last := o.last(); last.Type != kernel.ChunkDone || last.StopReason != kernel.StopMaxTokens {
				t.Fatalf("Stream ended with %+v, want done with stop reason max_tokens", last)
			}
			if got := len(r.models.Calls()); got != 1 {
				t.Fatalf("the model was asked %d times, want 1", got)
			}
			if got := countOf(r, events.RunCompleted.Name); got != 1 {
				t.Fatalf("%d RunCompleted events, want 1", got)
			}
			if attrs, ok := r.sink.endAttrs("agent-run"); !ok || attrOf(attrs, "stopReason") != "max_tokens" {
				t.Fatalf("run span end = %v, want stopReason max_tokens", attrs)
			}
		})
	}
}

// TestErrorStopEndsTheTurn: a response the provider stopped with error (a refusal, a
// content filter) ends the turn without asking again; Stream's done chunk carries
// StopReason error, also after an error chunk.
func TestErrorStopEndsTheTurn(t *testing.T) {
	cases := map[string]aecmock.ScriptedTurn{
		"reported error": {Content: "I can't help with that", StopReason: kernel.StopError},
		"error chunk":    {Chunks: []kernel.StreamChunk{{Type: kernel.ChunkError, Error: "boom"}}},
	}
	for name, turn := range cases {
		for _, mode := range loopModes {
			if name == "error chunk" && mode == "Run" {
				continue // a scripted error chunk is a Stream thing; Run's Chat ignores Chunks
			}
			t.Run(name+"/"+mode, func(t *testing.T) {
				r, _, _ := truncationRig(t, turn, aecmock.ScriptedTurn{Content: "never asked"})
				o := playTurn(t, r, mode, llmhub.ModelOptions{})
				if mode == "Run" {
					if o.res.StopReason != kernel.StopError || o.res.Iterations != 1 {
						t.Fatalf("Run result = %+v, want stop reason error after 1 iteration", o.res)
					}
				} else if last := o.last(); last.Type != kernel.ChunkDone || last.StopReason != kernel.StopError {
					t.Fatalf("Stream ended with %+v, want done with stop reason error", last)
				}
				if got := len(r.models.Calls()); got != 1 {
					t.Fatalf("the model was asked %d times, want 1", got)
				}
				if attrs, ok := r.sink.endAttrs("agent-run"); !ok || attrOf(attrs, "stopReason") != "error" {
					t.Fatalf("run span end = %v, want stopReason error", attrs)
				}
			})
		}
	}
}

// TestMaxTokensConsecutiveTruncationsStop: two responses in a row cut off mid tool call
// stop the run after their results are saved: Run returns the guard's message (not
// saved) with StopReason max_tokens, Stream ends with an error chunk carrying it and
// no done chunk. Every call has a saved result, and the model is not asked again.
func TestMaxTokensConsecutiveTruncationsStop(t *testing.T) {
	for _, mode := range loopModes {
		t.Run(mode, func(t *testing.T) {
			r, write, _ := truncationRig(t,
				aecmock.ScriptedTurn{ToolCalls: []kernel.ToolCall{cutOffCall("c1")}, StopReason: kernel.StopMaxTokens},
				aecmock.ScriptedTurn{ToolCalls: []kernel.ToolCall{cutOffCall("c2")}, StopReason: kernel.StopMaxTokens},
				aecmock.ScriptedTurn{Content: "never asked"},
			)
			o := playTurn(t, r, mode, llmhub.ModelOptions{MaxTokens: llmhub.Int(1024)})
			const want = "The model's output was cut off at the output token limit (maxTokens=1024) on 2 consecutive " +
				"responses while writing tool call arguments. Raise maxTokens or ask for the work in smaller steps."
			if mode == "Run" {
				if o.res.Message.Content != want || o.res.StopReason != kernel.StopMaxTokens || o.res.Iterations != 2 || o.res.Aborted {
					t.Fatalf("Run result = %+v, want the guard message with stop reason max_tokens after 2 iterations", o.res)
				}
			} else {
				last := o.last()
				if last.Type != kernel.ChunkError || last.Error != want {
					t.Fatalf("Stream ended with %+v, want the guard error chunk", last)
				}
				for _, c := range o.chunks {
					if c.Type == kernel.ChunkDone {
						t.Fatalf("Stream yielded a done chunk: %v", chunkTypes(o.chunks))
					}
				}
				requireOutputLimitError(t, o.streamedResults()["c2"], "(maxTokens=1024)")
			}
			saved := savedResults(t, r)
			requireOutputLimitError(t, saved["c1"], "(maxTokens=1024)")
			requireOutputLimitError(t, saved["c2"], "(maxTokens=1024)")
			if write.executions() != 0 {
				t.Fatalf("write_file ran %d times, want never", write.executions())
			}
			msgs := r.conv.Messages()
			if lastMsg := msgs[len(msgs)-1]; len(lastMsg.ToolResults) == 0 {
				t.Fatalf("the last saved message is %+v, want the tool results (the guard message is not saved)", lastMsg)
			}
			if got := len(r.models.Calls()); got != 2 {
				t.Fatalf("the model was asked %d times, want 2", got)
			}
			if got := countOf(r, events.RunCompleted.Name); got != 1 {
				t.Fatalf("%d RunCompleted events, want 1", got)
			}
			if got := countOf(r, events.MaxIterationsReached.Name); got != 0 {
				t.Fatalf("%d MaxIterationsReached events, want 0", got)
			}
			if attrs, ok := r.sink.endAttrs("agent-run"); !ok || attrOf(attrs, "stopReason") != "max_tokens" {
				t.Fatalf("run span end = %v, want stopReason max_tokens", attrs)
			}
		})
	}
}

// TestMaxTokensCountResetsAfterAResponseThatIsNotCutOff: a tool_use response between
// two cut-off responses resets the count, so the run goes on to its normal end.
func TestMaxTokensCountResetsAfterAResponseThatIsNotCutOff(t *testing.T) {
	for _, mode := range loopModes {
		t.Run(mode, func(t *testing.T) {
			r, write, echo := truncationRig(t,
				aecmock.ScriptedTurn{ToolCalls: []kernel.ToolCall{cutOffCall("c1")}, StopReason: kernel.StopMaxTokens},
				aecmock.ScriptedTurn{ToolCalls: []kernel.ToolCall{callOf("c2", "echo", `{}`)}},
				aecmock.ScriptedTurn{ToolCalls: []kernel.ToolCall{cutOffCall("c3")}, StopReason: kernel.StopMaxTokens},
				aecmock.ScriptedTurn{Content: "done"},
			)
			o := playTurn(t, r, mode, llmhub.ModelOptions{})
			if mode == "Run" {
				if o.res.Iterations != 4 || o.res.StopReason != kernel.StopEndTurn || o.res.Message.Content != "done" {
					t.Fatalf("Run result = %+v, want the end_turn answer of iteration 4", o.res)
				}
			} else if last := o.last(); last.Type != kernel.ChunkDone || last.StopReason != "" {
				t.Fatalf("Stream ended with %+v, want a plain done chunk", last)
			}
			if echo.executions() != 1 || write.executions() != 0 {
				t.Fatalf("echo ran %d times and write_file %d times, want 1 and 0", echo.executions(), write.executions())
			}
			saved := savedResults(t, r)
			requireOutputLimitError(t, saved["c1"], "")
			requireOutputLimitError(t, saved["c3"], "")
		})
	}
}

// TestMaxTokensCutOffCallSpanAndEvent: the dispatch span of a cut-off call ends with
// error output-truncated (and the argumentsError and maxTokens), and ToolFailed says
// output-truncated.
func TestMaxTokensCutOffCallSpanAndEvent(t *testing.T) {
	r, _, _ := truncationRig(t,
		aecmock.ScriptedTurn{ToolCalls: []kernel.ToolCall{cutOffCall("c1")}, StopReason: kernel.StopMaxTokens},
		aecmock.ScriptedTurn{Content: "done"},
	)
	playTurn(t, r, "Run", llmhub.ModelOptions{MaxTokens: llmhub.Int(256)})
	attrs, ok := r.sink.endAttrs("write_file")
	if !ok {
		t.Fatalf("no write_file span among %v", r.sink.endNames())
	}
	if attrOf(attrs, "error") != "output-truncated" || attrOf(attrs, "maxTokens") != 256 || attrOf(attrs, "argumentsError") != cutOffCall("c1").ArgumentsError {
		t.Fatalf("write_file span end = %v, want output-truncated with maxTokens and argumentsError", attrs)
	}
	if got := r.log.payload(t, events.ToolFailed.Name); !strings.Contains(got, `"error":"output-truncated"`) {
		t.Fatalf("ToolFailed payload = %s, want error output-truncated", got)
	}
}
