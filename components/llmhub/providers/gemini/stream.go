package gemini

import (
	"bytes"
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

// errTruncated is the error of a stream that ended before any finish reason.
// It is built per call, so a caller that changes the error it got changes no other.
func errTruncated() *kernel.ModelError {
	return kernel.NewModelError("the Gemini stream ended before a finish reason: the answer is incomplete", kernel.ModelCodeProvider, true)
}

// Stream sends a streamGenerateContent request (alt=sse) and turns its events into
// chunks:
//   - a text part that is not a thought gives a text chunk (an empty text none);
//   - a function call part gives tool_call_start, one tool_call_delta with the whole
//     arguments JSON and the complete tool_call: Gemini sends a call whole in one
//     event, and the progress chunks make it look like every other provider's;
//   - the usage and the finish reason of the last event that has them are kept, and
//     done follows the end of the stream with the usage and the stop reason
//     (mapStopReason).
//
// An event that reports an error ends the stream with one terminal error chunk (its
// message), and a stream that ends before any finish reason ends with one terminal
// error chunk (errTruncated). An event that is not JSON is skipped.
//
// A failure is one terminal error chunk with a nil error; only ctx.Err() goes in the
// error slot. The sequence has no goroutine of its own: it reads the response on the
// caller's goroutine, and breaking out of the loop closes the body and cancels the
// request's context, which releases the connection.
func (p *Provider) Stream(ctx context.Context, msgs []kernel.Message, opts llmhub.ModelOptions) iter.Seq2[kernel.StreamChunk, error] {
	return func(yield func(kernel.StreamChunk, error) bool) {
		if err := ctx.Err(); err != nil {
			yield(kernel.StreamChunk{}, err)
			return
		}
		body, err := buildBody(msgs, opts)
		if err != nil {
			yield(errorChunk(err.Error()), nil)
			return
		}
		rc, cancel := context.WithCancel(llmtransport.StreamContext(ctx))
		defer cancel()
		req, err := http.NewRequestWithContext(rc, http.MethodPost, p.modelURL(opts.Model, "streamGenerateContent")+"?alt=sse", bytes.NewReader(body))
		if err != nil {
			yield(errorChunk(mapNetworkError(err).Message), nil)
			return
		}
		req.Header = p.headers(true)
		resp, err := p.client.Do(req)
		if err != nil {
			// The caller's context decides, not the derived one this function cancels
			// itself: an abort is not a model failure and is never mapped to one.
			if cerr := ctx.Err(); cerr != nil {
				yield(kernel.StreamChunk{}, cerr)
				return
			}
			yield(errorChunk(mapNetworkError(err).Message), nil)
			return
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode < 200 || resp.StatusCode > 299 {
			yield(errorChunk(httpFailure(resp).Message), nil)
			return
		}
		readEvents(ctx, resp.Body, maxLineBytes, yield)
	}
}

// readEvents reads the events of body and yields the chunks, then done. limit is the
// line limit of the parser.
func readEvents(ctx context.Context, body io.Reader, limit int, yield func(kernel.StreamChunk, error) bool) {
	parser := newSSEParser(body, limit)
	var (
		usage  *kernel.TokenUsage
		reason string
		ended  bool
		calls  bool
	)
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
			yield(errorChunk(mapNetworkError(err).Message), nil)
			return
		}
		if cerr := ctx.Err(); cerr != nil {
			yield(kernel.StreamChunk{}, cerr)
			return
		}
		var ev responseBody
		if json.Unmarshal([]byte(payload), &ev) != nil {
			continue
		}
		if ev.Error != nil {
			message := ev.Error.Message
			if message == "" {
				message = "Gemini API stream error"
			}
			yield(errorChunk(llmtransport.TruncateMessage(message)), nil)
			return
		}
		for _, part := range ev.parts() {
			switch {
			case part.Thought:
			case part.FunctionCall != nil:
				call := toolCall(part)
				progress := kernel.ToolCallProgress{ID: call.ID, Name: call.Name}
				if !yield(kernel.StreamChunk{Type: kernel.ChunkToolCallStart, ToolCallProgress: &progress}, nil) {
					return
				}
				delta := progress
				delta.ArgumentsDelta = argumentsText(call.Arguments)
				if !yield(kernel.StreamChunk{Type: kernel.ChunkToolCallDelta, ToolCallProgress: &delta}, nil) {
					return
				}
				calls = true
				if !yield(kernel.StreamChunk{Type: kernel.ChunkToolCall, ToolCall: &call}, nil) {
					return
				}
			case part.Text != nil && *part.Text != "":
				text := *part.Text
				if !yield(kernel.StreamChunk{Type: kernel.ChunkText, Content: &text}, nil) {
					return
				}
			}
		}
		if u := ev.usage(); u != nil {
			usage = u
		}
		if r := ev.finishReason(); r != "" {
			reason, ended = r, true
		}
	}
	if !ended {
		yield(errorChunk(errTruncated().Message), nil)
		return
	}
	yield(kernel.StreamChunk{Type: kernel.ChunkDone, Usage: usage, StopReason: mapStopReason(reason, calls)}, nil)
}

// argumentsText is the JSON text of the arguments, {} when there are none.
func argumentsText(a kernel.Arguments) string {
	if len(a) == 0 {
		return "{}"
	}
	return string(a)
}

// errorChunk is the terminal chunk of a failed stream.
func errorChunk(message string) kernel.StreamChunk {
	return kernel.StreamChunk{Type: kernel.ChunkError, Error: message}
}
