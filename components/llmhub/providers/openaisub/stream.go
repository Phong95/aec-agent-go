package openaisub

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"io"
	"iter"
	"net/http"

	"github.com/Phong95/aec-agent-go/components/llmhub"
	"github.com/Phong95/aec-agent-go/internal/llmtransport"
	"github.com/Phong95/aec-agent-go/kernel"
)

// streamEvent is the part of a Responses stream event the adapter reads. An event
// whose fields have other types than these does not decode and is skipped, like an
// event that is not JSON.
type streamEvent struct {
	Type  string `json:"type"`
	Delta string `json:"delta"`
	// Response carries the usage of response.completed (and of response.incomplete
	// and response.failed), and also the final status and its reasons.
	Response *struct {
		Usage *struct {
			InputTokens  *float64 `json:"input_tokens"`
			OutputTokens *float64 `json:"output_tokens"`
		} `json:"usage"`
		// Status is completed, incomplete or failed in a final event.
		Status looseString `json:"status"`
		// IncompleteDetails says why an incomplete response stopped
		// (max_output_tokens, content_filter).
		IncompleteDetails *struct {
			Reason looseString `json:"reason"`
		} `json:"incomplete_details"`
		// Error is the failure of a failed response.
		Error *eventError `json:"error"`
	} `json:"response"`
	// Code and Message are the members of an "error" event.
	Code    looseString `json:"code"`
	Message looseString `json:"message"`
	// Error is the nested form of an "error" event ({"type":"error","error":{code,
	// message}}), read when the top-level members are empty.
	Error *eventError `json:"error"`
	// raw is the event as it arrived, put in a failure that has no message of its own
	// so the backend's reason is never lost.
	raw  string
	Item *outputItem `json:"item"`
	// ItemID is set on response.function_call_arguments.delta: the function_call item
	// being written. It and OutputIndex are pointers because "" and 0 are values.
	ItemID      *string  `json:"item_id"`
	OutputIndex *float64 `json:"output_index"`
}

// outputItem is an output item: the function_call fields are the ones read.
type outputItem struct {
	Type string `json:"type"`
	// ID is the output item id, which argument deltas reference as item_id.
	ID        string `json:"id"`
	CallID    string `json:"call_id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// pendingCall is the identity of a function call that is still being written.
type pendingCall struct {
	id   string
	name string
}

// looseString is an optional string member of an event. A value that is not a
// string reads as "" instead of failing the decode, so such a member never makes the
// whole event unreadable.
type looseString string

// UnmarshalJSON implements json.Unmarshaler.
func (l *looseString) UnmarshalJSON(b []byte) error {
	var s string
	if json.Unmarshal(b, &s) == nil {
		*l = looseString(s)
	} else {
		*l = ""
	}
	return nil
}

// eventError is {code, message}: the error of a failed response.
type eventError struct {
	Code    looseString `json:"code"`
	Message looseString `json:"message"`
}

// streamState is what the stream remembers between events.
type streamState struct {
	// Function calls still being written, keyed by output item id (and by
	// output_index as a fallback) so argument deltas can be attributed. The two maps
	// keep the string "0" and the number 0 apart as different keys.
	byItemID      map[string]pendingCall
	byOutputIndex map[float64]pendingCall
	usage         *kernel.TokenUsage
	// ended is set by a final event (response.completed, response.incomplete): the
	// answer is complete as far as the server is concerned.
	ended bool
	// stop is the stop reason the final event gives (empty: decided by the calls).
	stop kernel.StopReason
	// calls is set once a complete tool_call was given: a completed response's done
	// then says tool_use (A-88).
	calls bool
	// failure is set by response.failed or an error event: the stream ends with it.
	failure *kernel.ModelError
}

func newStreamState() *streamState {
	return &streamState{byItemID: map[string]pendingCall{}, byOutputIndex: map[float64]pendingCall{}}
}

// lookup finds the call an argument delta belongs to: by item_id when the event has
// one, and by output_index when that finds nothing.
func (s *streamState) lookup(ev streamEvent) (pendingCall, bool) {
	if ev.ItemID != nil {
		if call, ok := s.byItemID[*ev.ItemID]; ok {
			return call, true
		}
	}
	if ev.OutputIndex != nil {
		if call, ok := s.byOutputIndex[*ev.OutputIndex]; ok {
			return call, true
		}
	}
	return pendingCall{}, false
}

// handle turns one event into at most one chunk. It returns
// false when the consumer stopped.
func (s *streamState) handle(ev streamEvent, emit func(kernel.StreamChunk) bool) bool {
	switch ev.Type {
	case "response.output_text.delta":
		if ev.Delta != "" {
			text := ev.Delta
			return emit(kernel.StreamChunk{Type: kernel.ChunkText, Content: &text})
		}
	case "response.output_item.added":
		if ev.Item != nil && ev.Item.Type == "function_call" {
			// Announce the call now - the arguments can take a long time to generate
			// and the complete "tool_call" only follows at output_item.done.
			call := pendingCall{id: ev.Item.CallID, name: ev.Item.Name}
			if ev.Item.ID != "" {
				s.byItemID[ev.Item.ID] = call
			}
			if ev.OutputIndex != nil {
				s.byOutputIndex[*ev.OutputIndex] = call
			}
			return emit(kernel.StreamChunk{
				Type:             kernel.ChunkToolCallStart,
				ToolCallProgress: &kernel.ToolCallProgress{ID: call.id, Name: call.name},
			})
		}
	case "response.function_call_arguments.delta":
		if ev.Delta != "" {
			if call, ok := s.lookup(ev); ok {
				return emit(kernel.StreamChunk{
					Type:             kernel.ChunkToolCallDelta,
					ToolCallProgress: &kernel.ToolCallProgress{ID: call.id, Name: call.name, ArgumentsDelta: ev.Delta},
				})
			}
		}
	case "response.output_item.done":
		if ev.Item != nil && ev.Item.Type == "function_call" {
			// The call is built from the done item's own id, name and final arguments,
			// not from the deltas that were collected.
			args, argsErr := llmhub.ParseToolArguments(ev.Item.Arguments)
			call := kernel.ToolCall{ID: ev.Item.CallID, Name: ev.Item.Name, Arguments: args, ArgumentsError: argsErr}
			s.calls = true
			return emit(kernel.StreamChunk{Type: kernel.ChunkToolCall, ToolCall: &call})
		}
	case "response.completed", "response.incomplete":
		// Reads the usage and the final status, and accepts response.incomplete, the
		// event the API sends for an answer that was cut by the output token limit or a
		// filter.
		s.ended = true
		if ev.Response != nil {
			if u := ev.Response.Usage; u != nil {
				s.usage = &kernel.TokenUsage{InputTokens: tokenCount(u.InputTokens), OutputTokens: tokenCount(u.OutputTokens)}
			}
			s.stop = finalStopReason(ev)
		} else if ev.Type == "response.incomplete" {
			s.stop = kernel.StopMaxTokens
		}
	case "response.failed":
		// The stream ends with the failure.
		var e *eventError
		if ev.Response != nil {
			e = ev.Response.Error
		}
		if e == nil {
			e = &eventError{}
		}
		s.failure = eventFailure(string(e.Code), string(e.Message), withRaw("the response failed", ev.raw))
	case "error":
		// The stream ends with the reported error, top-level or nested.
		code, message := string(ev.Code), string(ev.Message)
		if ev.Error != nil {
			code = cmp.Or(code, string(ev.Error.Code))
			message = cmp.Or(message, string(ev.Error.Message))
		}
		s.failure = eventFailure(code, message, withRaw("the stream reported an error", ev.raw))
	}
	return true
}

// finalStopReason maps the status of a final event: an incomplete response stopped
// by max_output_tokens is max_tokens, one stopped by content_filter is error, and an
// incomplete response with another or no reason is max_tokens (the answer is cut,
// and a token limit is the usual cause). A completed response leaves the reason to
// the calls (tool_use when there is one, else end_turn).
func finalStopReason(ev streamEvent) kernel.StopReason {
	if ev.Response.Status != "incomplete" && ev.Type != "response.incomplete" {
		return ""
	}
	reason := ""
	if d := ev.Response.IncompleteDetails; d != nil {
		reason = string(d.Reason)
	}
	if reason == "content_filter" {
		return kernel.StopError
	}
	return kernel.StopMaxTokens
}

// withRaw is fallback followed by the raw event, so a failure the backend sent without
// a message still says what it was.
func withRaw(fallback, raw string) string {
	if raw == "" {
		return fallback
	}
	return fallback + ": " + raw
}

// eventFailure maps the code of a failed response or an error event: a rate limit
// is rate_limit and retryable, a server error is a retryable provider error, a
// request the server refused is invalid_request, and anything else is a provider
// error that is not retryable. The text is the message, else fallback.
func eventFailure(code, message, fallback string) *kernel.ModelError {
	if message == "" {
		message = fallback
	}
	message = llmtransport.TruncateMessage(message)
	switch code {
	case "rate_limit_exceeded", "rate_limit_error":
		return kernel.NewModelError(message, kernel.ModelCodeRateLimit, true)
	case "server_error", "internal_error", "server_is_overloaded", "service_unavailable":
		return kernel.NewModelError(message, kernel.ModelCodeProvider, true)
	case "invalid_prompt", "invalid_request_error", "context_length_exceeded", "invalid_request":
		return kernel.NewModelError(message, kernel.ModelCodeInvalidRequest, false)
	default:
		return kernel.NewModelError(message, kernel.ModelCodeProvider, false)
	}
}

// errTruncated is the error of a stream that ended before its final event.
// It is built per call, so a caller that changes the error it got changes no other.
func errTruncated() *kernel.ModelError {
	return kernel.NewModelError("the stream ended before response.completed: the answer is incomplete", kernel.ModelCodeProvider, true)
}

// tokenCount is n ?? 0.
func tokenCount(n *float64) int {
	if n == nil {
		return 0
	}
	return int(*n)
}

// errorChunk is the terminal chunk of a failed stream.
func errorChunk(message string) kernel.StreamChunk {
	return kernel.StreamChunk{Type: kernel.ChunkError, Error: message}
}

// streamResult is what Chat needs from a stream beyond its chunks: the typed error
// of a failed stream (the chunk carries only its text) and the stop reason of the
// final event. Stream passes no result. Since A-88 the stop reason rides on the done
// chunk, where Chat reads it, so only the typed error is left here.
type streamResult struct {
	err *kernel.ModelError
}

// fail yields the terminal error chunk of me and records me for Chat.
func (r *streamResult) fail(me *kernel.ModelError, yield func(kernel.StreamChunk, error) bool) {
	if r != nil {
		r.err = me
	}
	yield(errorChunk(me.Message), nil)
}

// Stream sends the request and turns the Responses events into chunks:
//   - response.output_text.delta gives a text chunk (an empty delta gives none);
//   - response.output_item.added of a function_call gives tool_call_start at once;
//   - response.function_call_arguments.delta gives tool_call_delta for the call found
//     by item_id, else by output_index (an unknown call gives nothing);
//   - response.output_item.done of a function_call gives the complete tool_call, built
//     with llmhub.ParseToolArguments from the item's final arguments, so a model that
//     writes broken JSON produces a call that carries an ArgumentsError instead of
//     failing the stream;
//   - response.completed (and response.incomplete) records the usage and the final
//     status, and done follows the end of the stream with the usage and the stop
//     reason (A-88): the one of an incomplete response (finalStopReason), else
//     tool_use when a tool_call was given and end_turn otherwise.
//
// response.failed and error end the stream with one terminal error chunk (its
// message, mapped by eventFailure), an event whose last line has no blank line (or
// no newline) after it is still read, and a stream that ends before its final event
// (a cut connection) ends with one terminal error chunk (errTruncated). Every other
// event gives nothing.
//
// A failure is one terminal error chunk with a nil error; only ctx.Err() goes in the
// error slot. The sequence has no goroutine of its own: it reads the response on the
// caller's goroutine, and breaking out of the loop closes the body and cancels the
// request's context, which releases the connection.
func (p *Provider) Stream(ctx context.Context, msgs []kernel.Message, opts llmhub.ModelOptions) iter.Seq2[kernel.StreamChunk, error] {
	return p.stream(ctx, msgs, opts, nil)
}

// stream is Stream, and also records the typed error and the final stop reason in
// res when it is not nil (Chat). The final stop reason is on the done chunk since
// A-88; res keeps the typed error only.
func (p *Provider) stream(ctx context.Context, msgs []kernel.Message, opts llmhub.ModelOptions, res *streamResult) iter.Seq2[kernel.StreamChunk, error] {
	return func(yield func(kernel.StreamChunk, error) bool) {
		if err := ctx.Err(); err != nil {
			yield(kernel.StreamChunk{}, err)
			return
		}
		body, err := p.buildBody(msgs, opts)
		if err != nil {
			res.fail(kernel.WithCause(kernel.NewModelError(err.Error(), kernel.ModelCodeProvider, false), err), yield)
			return
		}
		// One snapshot per request: the token and the account id of the same
		// UpdateTokens call.
		c := p.snapshot()
		rc, cancel := context.WithCancel(llmtransport.StreamContext(ctx))
		defer cancel()
		req, err := http.NewRequestWithContext(rc, http.MethodPost, p.responsesURL, bytes.NewReader(body))
		if err != nil {
			res.fail(mapNetworkError(err), yield)
			return
		}
		req.Header = buildHeaders(c)
		resp, err := p.client.Do(req)
		if err != nil {
			// The caller's context decides, not the derived one this function cancels
			// itself: an abort is not a model failure and is never mapped to one.
			if cerr := ctx.Err(); cerr != nil {
				yield(kernel.StreamChunk{}, cerr)
				return
			}
			res.fail(mapNetworkError(err), yield)
			return
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode < 200 || resp.StatusCode > 299 {
			res.fail(httpFailure(resp), yield)
			return
		}
		readEvents(ctx, resp.Body, maxLineBytes, yield, res)
	}
}

// readEvents reads the events of body and yields the chunks, then done. limit is the
// line limit of the parser. res, when not nil, receives the typed error and the
// final stop reason. Since A-88 the final stop reason is put on done instead, for
// Stream's consumers too; res receives the typed error only.
func readEvents(ctx context.Context, body io.Reader, limit int, yield func(kernel.StreamChunk, error) bool, res *streamResult) {
	emit := func(c kernel.StreamChunk) bool { return yield(c, nil) }
	parser := newSSEParser(body, limit)
	state := newStreamState()
	for {
		payload, err := parser.next()
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			if cerr := ctx.Err(); cerr != nil {
				yield(kernel.StreamChunk{}, cerr)
				return
			}
			res.fail(mapNetworkError(err), yield)
			return
		}
		if cerr := ctx.Err(); cerr != nil {
			yield(kernel.StreamChunk{}, cerr)
			return
		}
		var ev streamEvent
		if json.Unmarshal([]byte(payload), &ev) != nil {
			continue
		}
		ev.raw = payload
		if !state.handle(ev, emit) {
			return
		}
		if state.failure != nil {
			res.fail(state.failure, yield)
			return
		}
	}
	if !state.ended {
		res.fail(errTruncated(), yield)
		return
	}
	stop := state.stop
	if stop == "" {
		stop = kernel.StopEndTurn
		if state.calls {
			stop = kernel.StopToolUse
		}
	}
	yield(kernel.StreamChunk{Type: kernel.ChunkDone, Usage: state.usage, StopReason: stop}, nil)
}
