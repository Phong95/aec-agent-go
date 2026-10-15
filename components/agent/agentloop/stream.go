package agentloop

import (
	"context"
	"errors"
	"iter"

	"github.com/Phong95/aec-agent-go/components/events"
	"github.com/Phong95/aec-agent-go/components/llmhub"
	"github.com/Phong95/aec-agent-go/kernel"
)

// maxIterationsStreamError is the text of the error chunk Stream yields when the
// loop runs out of iterations. It differs from the Run fallback message on
// purpose.
const maxIterationsStreamError = "Reached maximum reasoning steps."

// Stream runs one agent turn and yields the model's output as it arrives. It does what
// Run does, with the same stop-reason rule (runsTools): the collected reply is
// post-processed by the afterModel middlewares like Run's (after its chunks were
// yielded, so the processed reply is what is saved and executed), ModelResponded
// carries the usage when the provider's done chunk reported one, and every response
// that does not run tools ends the turn. Before A-88 a stream chunk carried no stop
// reason, so a provider max_tokens reached the loop as end_turn (or tool_use when tool
// calls were collected); an error chunk still reaches it as error. Since A-88 the
// provider's done chunk carries the stop reason (kernel.StreamChunk.StopReason): a
// reported max_tokens reaches the loop as max_tokens, with or without tool calls
// (streamModelResponse), and a max_tokens response with tool calls runs them as Run
// does (actOnToolCalls). The loop ends the stream with its own done chunk (the
// provider's done chunk is never forwarded).
//
// What the consumer sees:
//   - text, tool_call_start, tool_call_delta, tool_call and error chunks of the
//     model, in order;
//   - one tool_result chunk per tool result, in call order, after the tools ran
//     (also the output-token-limit errors of cut-off calls, A-88);
//   - one done chunk with the run's total usage when the turn ends (and with Aborted
//     true when the run was stopped, and with StopReason max_tokens or error when the
//     turn ended because the output token limit was hit or the response stopped with
//     an error, A-88; a normal end leaves StopReason empty);
//   - at the iteration cap, one error chunk "Reached maximum reasoning steps." and
//     no done chunk;
//   - when responses were cut off at the output token limit while writing tool calls
//     maxConsecutiveTruncations times in a row (A-88), the tool_result chunks of the
//     last one, then one error chunk with the consecutive-truncation message and no
//     done chunk; the run completes (RunCompleted) as in Run.
//
// The error slot carries only preconditions and port failures: ErrNoActiveSession,
// ErrSessionNotActive (sessionID is not the active session's id), a conversation
// write that failed or timed out, a failed Context or Skills port.
// A model failure is an error chunk, and a stop is the done chunk with Aborted
// true. After an error the sequence ends.
//
// Cancelling ctx or letting its deadline pass stops the run at the next safe point.
// The consumer may also stop iterating (a break, or a panic in the loop body). The
// run is then cleaned up as if it was stopped: the partial reply is saved when it
// is not empty, every open span ends marked aborted and RunCompleted is emitted with
// Aborted true. The cleanup never yields again, as range-over-func forbids.
//
// The sequence is produced on the consumer's goroutine: Stream starts no goroutine
// and uses no channel, so abandoning it cannot leak one. For the same reason the
// provider's Stream sequence is iterated directly and cannot be raced against ctx:
// a stop ends the response at the provider's next chunk (the llmhub.Provider contract
// makes the provider watch ctx). The calls before and after the model call (the
// context middlewares, the compaction summary, the memory recall, the tools) are
// raced against ctx as in Run.
func (l *Loop) Stream(ctx context.Context, userMessage, sessionID string, overrides llmhub.ModelOptions, ro RunOptions) iter.Seq2[kernel.StreamChunk, error] {
	// No other per-run options yet (A-97: ro.Conversation, see Run); cancellation
	// comes through the ctx.
	return func(yield func(kernel.StreamChunk, error) bool) {
		r, err := l.beginRun(ctx, userMessage, sessionID, ro)
		if err != nil {
			yield(kernel.StreamChunk{}, err) // preconditions only
			return
		}
		// Every exit that did not end the run itself, such as a consumer that stopped
		// iterating, ends up here. It does nothing once a terminal path ran, and it
		// never yields.
		defer r.closeAborted(ctx)
		l.stream(ctx, r, overrides, yield)
	}
}

// streamedResponse is what one streamed model response collected. The text it
// carried is in the run state (partial), not here.
type streamedResponse struct {
	toolCalls  []kernel.ToolCall
	stopReason kernel.StopReason
	// usage is the usage of the provider's done chunk, when it carried one.
	usage *kernel.TokenUsage
	// aborted is true when the run was stopped while the response was streaming.
	aborted bool
	// errText is the text of the last error chunk, when the provider streamed one.
	errText string
}

// modelErrorStop is the model-call span's error when a response stopped with
// StopError and gave no error text.
const modelErrorStop = "the model stopped with stopReason error"

// stream is the iteration loop of Stream, after beginRun. It returns
// when the sequence is over, including when the consumer stopped it (a yield that
// returned false); the caller's deferred cleanup handles that case.
func (l *Loop) stream(ctx context.Context, r *runState, overrides llmhub.ModelOptions, yield func(kernel.StreamChunk, error) bool) {
	iteration := 0
	for iteration < r.maxIterations {
		// Safe point A: before the next iteration starts. It covers a ctx that was
		// already done and a stop during the tools of the previous iteration.
		if ctx.Err() != nil {
			l.yieldAborted(ctx, r, yield)
			return
		}

		iteration++
		r.startIteration(iteration)

		in, err := l.assembleModelInput(ctx, r, overrides)
		if errors.Is(err, errRunStopped) {
			l.yieldAborted(ctx, r, yield)
			return
		}
		if err != nil {
			l.yieldFailure(r, err, yield)
			return
		}

		// Safe point B: after context assembly and compaction, before the model is
		// asked for anything.
		if ctx.Err() != nil {
			l.yieldAborted(ctx, r, yield)
			return
		}

		l.startModelCall(r, iteration, in)
		streamed, ok := l.streamModelResponse(ctx, r, in, yield)
		if !ok {
			return // the consumer stopped iterating
		}

		// Stopped mid-stream: this check comes before the normal persistence below. The
		// partial text is saved only when it is not empty, and the tool calls collected
		// so far never are: they were not dispatched and would dangle without a result.
		if streamed.aborted || ctx.Err() != nil {
			l.yieldAborted(ctx, r, yield)
			return
		}

		content := r.partial.String()
		message := kernel.Message{Role: kernel.RoleAssistant, Content: content}
		if len(streamed.toolCalls) > 0 {
			message.ToolCalls = streamed.toolCalls
		}

		// Run the afterModel middlewares on the collected response, raced against ctx,
		// as Run does. A stop here saves the partial text the consumer received.
		raw := kernel.ModelResponse{Message: message, StopReason: streamed.stopReason}
		if streamed.usage != nil {
			raw.Usage = cloneUsage(*streamed.usage)
		}
		mc := l.middlewareContext(r)
		resp, stopped, err := raceAbort(ctx, func() (kernel.ModelResponse, error) {
			return l.deps.Context.ProcessResponse(ctx, raw, mc)
		})
		if stopped || (err != nil && ctx.Err() != nil) {
			l.yieldAborted(ctx, r, yield)
			return
		}
		if err != nil {
			l.yieldFailure(r, err, yield)
			return
		}
		message = resp.Message
		if streamed.usage != nil {
			message.Usage = usagePtr(resp.Usage) // saved with the message it produced
		}
		r.stopReason = resp.StopReason
		hasToolCalls := len(message.ToolCalls) > 0

		end := obj("stopReason", string(resp.StopReason))
		// An error chunk (or a refusal or filter stop) ends the span with status error,
		// not ok: the error chunk's text when there was one.
		if resp.StopReason == kernel.StopError {
			if streamed.errText != "" {
				end.Set("error", streamed.errText)
			} else {
				end.Set("error", modelErrorStop)
			}
		}
		end.Set("hasToolCalls", hasToolCalls)
		end.Set("responsePreview", message.Content)
		if hasToolCalls {
			end.Set("toolCalls", toolCallsAttr(message.ToolCalls))
		}
		if streamed.usage != nil {
			end.Set("inputTokens", resp.Usage.InputTokens)
			end.Set("outputTokens", resp.Usage.OutputTokens)
		}
		r.endModelSpan(end)

		// The usage rides along when the provider reported one. It was recorded at the
		// done chunk, so a stop before this point still saves it.
		responded := events.ModelRespondedPayload{
			AgentID:      r.agentID,
			StopReason:   string(resp.StopReason),
			HasToolCalls: hasToolCalls,
		}
		if streamed.usage != nil {
			usage := cloneUsage(resp.Usage)
			responded.Usage = &usage
		}
		emit(r, events.ModelResponded, responded)
		l.emitContextUsage(r, iteration, in, message, responded.Usage)

		if err := r.addMessage(ctx, message); err != nil {
			l.yieldFailure(r, err, yield)
			return
		}
		r.partial.Reset() // saved: an early exit from here on must not save it again
		// The usage the done chunk carried reaches the stored session right after the
		// message write; an iteration whose done chunk carried none writes nothing.
		if err := r.updateSession(ctx); err != nil {
			l.yieldFailure(r, err, yield)
			return
		}

		// Every response that does not run tools ends the turn (the rule Run uses).
		if !runsTools(resp.StopReason, message.ToolCalls) {
			r.completeRun(iteration, resp.StopReason)
			done := kernel.StreamChunk{Type: kernel.ChunkDone, Usage: usagePtr(r.totalUsage)}
			// A-88: the done chunk says why the turn ended when it is not a normal end.
			if resp.StopReason == kernel.StopMaxTokens || resp.StopReason == kernel.StopError {
				done.StopReason = resp.StopReason
			}
			yield(done, nil)
			return
		}

		results, limitReached, err := l.actOnToolCalls(ctx, r, in, resp.StopReason, message.ToolCalls)
		if err != nil {
			l.yieldFailure(r, err, yield)
			return
		}
		// Yield the results so the consumer can show them, also the cancelled ones of
		// a stopped run.
		for i := range results {
			chunk := kernel.StreamChunk{Type: kernel.ChunkToolResult, ToolResult: &results[i]}
			if !yield(chunk, nil) {
				return
			}
		}

		// A-88: cut off mid tool call too many times in a row: stop with an error chunk
		// (the convention of the iteration cap: no done after it). A stop during the
		// tools is a stop, not the guard.
		if limitReached {
			if ctx.Err() != nil {
				l.yieldAborted(ctx, r, yield)
				return
			}
			r.completeRun(iteration, kernel.StopMaxTokens)
			yield(kernel.StreamChunk{Type: kernel.ChunkError, Error: truncationLimitMessage(l.resolvedMaxTokens(in))}, nil)
			return
		}

		r.endIteration()
	}

	// A stop during the last iteration's tools is a stop, not the cap.
	if ctx.Err() != nil {
		l.yieldAborted(ctx, r, yield)
		return
	}

	r.closeMaxIterations()
	yield(kernel.StreamChunk{Type: kernel.ChunkError, Error: maxIterationsStreamError}, nil)
}

// streamModelResponse streams one model response: it yields each chunk to the
// consumer (except done) and collects the text into the run state, the tool calls
// and the stop reason. The second result is false when the
// consumer stopped iterating; the caller then returns without touching the result.
// The stop reason is resolved at done from the provider's reported one and the
// collected calls (A-88), and is error after an error chunk.
//
// The provider's sequence is ranged over directly. Returning from the loop body
// when yield reports false releases the provider (its yield returns false and it
// returns), so no goroutine is left behind.
func (l *Loop) streamModelResponse(ctx context.Context, r *runState, in modelInput, yield func(kernel.StreamChunk, error) bool) (streamedResponse, bool) {
	r.partial.Reset()
	out := streamedResponse{stopReason: kernel.StopEndTurn}

	for chunk, err := range l.deps.Models.Stream(ctx, in.messages, in.options) {
		// Safe point C: mid-stream. A chunk the provider produced right as the run was
		// stopped is dropped, so a provider error caused by the stop is never yielded.
		if ctx.Err() != nil {
			out.aborted = true
			break
		}
		if err != nil {
			// The provider contract keeps the error slot for the ctx error, so this is a
			// decorator that failed on its own: surface it as a failed response.
			chunk = kernel.StreamChunk{Type: kernel.ChunkError, Error: err.Error()}
		}

		switch chunk.Type {
		case kernel.ChunkDone:
			// A-88: the provider's reported max_tokens wins. Otherwise collected tool
			// calls mean tool_use (also for a provider that reports nothing, or a
			// reported end_turn or error); a reported error stays error; anything else
			// ends the turn.
			switch {
			case chunk.StopReason == kernel.StopMaxTokens:
				out.stopReason = kernel.StopMaxTokens
			case len(out.toolCalls) > 0:
				out.stopReason = kernel.StopToolUse
			case chunk.StopReason == kernel.StopError:
				out.stopReason = kernel.StopError
			default:
				out.stopReason = kernel.StopEndTurn
			}
			if chunk.Usage != nil {
				u := cloneUsage(*chunk.Usage)
				out.usage = &u
				r.recordUsage(in.messages, in.toolDefs, u)
				// The response's usage reaches the consumer as a usage chunk (shown with
				// the response's message, which saves it too).
				if !yield(kernel.StreamChunk{Type: kernel.ChunkUsage, Usage: usagePtr(u)}, nil) {
					return out, false
				}
			}
			continue // the loop yields its own done chunk
		case kernel.ChunkText:
			// Appended before the yield: what the consumer received is what is saved.
			if chunk.Content != nil {
				r.partial.WriteString(*chunk.Content)
			}
		case kernel.ChunkToolCall:
			if chunk.ToolCall != nil {
				out.toolCalls = append(out.toolCalls, cloneToolCall(*chunk.ToolCall))
			}
		case kernel.ChunkError:
			out.stopReason = kernel.StopError
			if chunk.Error != "" {
				out.errText = chunk.Error
			}
		case kernel.ChunkToolCallStart, kernel.ChunkToolCallDelta, kernel.ChunkToolResult:
			// Forwarded as they are; only the complete tool_call chunk is executed.
		default:
			// An unknown kind is forwarded too.
		}

		if !yield(chunk, nil) {
			return out, false
		}
	}
	return out, true
}

// yieldAborted ends a stopped run and yields its done chunk with Aborted true, or
// the error of a write the cleanup could not make. The caller returns right after.
func (l *Loop) yieldAborted(ctx context.Context, r *runState, yield func(kernel.StreamChunk, error) bool) {
	if err := r.abortRun(ctx); err != nil {
		yield(kernel.StreamChunk{}, err)
		return
	}
	yield(kernel.StreamChunk{Type: kernel.ChunkDone, Usage: usagePtr(r.totalUsage), Aborted: true}, nil)
}

// yieldFailure ends a run that failed on a port: every open span ends with the
// error and the trace context is removed, then the error is yielded once in the
// error slot. The caller returns right after.
func (l *Loop) yieldFailure(r *runState, err error, yield func(kernel.StreamChunk, error) bool) {
	r.fail(err)
	yield(kernel.StreamChunk{}, err)
}

// usagePtr returns a copy of u that a chunk can carry.
func usagePtr(u kernel.TokenUsage) *kernel.TokenUsage {
	c := cloneUsage(u)
	return &c
}

// cloneToolCall copies a tool call, so the history owns its own arguments.
func cloneToolCall(c kernel.ToolCall) kernel.ToolCall {
	return kernel.Message{ToolCalls: []kernel.ToolCall{c}}.Clone().ToolCalls[0]
}
