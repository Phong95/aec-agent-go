package anthropic

import (
	"context"
	"iter"
	"strings"

	"github.com/anthropics/anthropic-sdk-go/option"

	"github.com/Phong95/aec-agent-go/components/llmhub"
	"github.com/Phong95/aec-agent-go/internal/llmtransport"
	"github.com/Phong95/aec-agent-go/kernel"
)

// toolBlock is the tool_use block the stream is inside of: its identity and the
// arguments text collected so far. The stream keeps one such value, not a map:
// Anthropic writes its content blocks one after another.
type toolBlock struct {
	id   string
	name string
	buf  strings.Builder
}

// Stream sends the request and turns the server-sent events into chunks:
//   - text_delta gives a text chunk, including an empty one: the delta is yielded
//     unguarded, so an empty delta is a chunk with content "";
//   - a tool_use block start gives tool_call_start at once, because the arguments
//     can take a long time to generate;
//   - input_json_delta is added to the block's buffer and gives a tool_call_delta
//     when it is not empty;
//   - content_block_stop of a tool_use block gives the complete tool_call, built
//     with llmhub.ParseToolArguments, so a model that writes broken JSON produces a
//     call that carries an ArgumentsError instead of failing the stream;
//   - the input tokens come from message_start, the output tokens from
//     message_delta, and message_stop gives done with those two numbers only (a
//     stream carries no cache usage, CONTEXT C1);
//   - the stop reason of message_delta, mapped with mapStopReason, rides on done
//     (A-88); a stream whose message_delta carried none gives done without one;
//   - a tool_use block still open at message_stop (a response cut off at
//     max_tokens may end without closing the block it was writing) gives its
//     tool_call right before done, with the ArgumentsError of the partial JSON,
//     instead of being dropped (A-88);
//   - thinking blocks and pings give no chunk.
//
// A stream that ends without message_stop must not end quietly: the chunks that
// were read stand, and the stream ends with
// one terminal error chunk (errTruncated), so a consumer never takes a cut answer
// for a complete one.
//
// A failure is one terminal error chunk with a nil error; only ctx.Err() goes in
// the error slot. The sequence has no goroutine of its own: it reads the SDK
// stream on the caller's goroutine, and breaking out of the loop closes the stream
// and cancels its context, which releases the connection.
func (p *Provider) Stream(ctx context.Context, msgs []kernel.Message, opts llmhub.ModelOptions) iter.Seq2[kernel.StreamChunk, error] {
	return func(yield func(kernel.StreamChunk, error) bool) {
		if err := ctx.Err(); err != nil {
			yield(kernel.StreamChunk{}, err)
			return
		}
		body, params, err := p.buildRequest(msgs, opts)
		if err != nil {
			yield(kernel.StreamChunk{Type: kernel.ChunkError, Error: err.Error()}, nil)
			return
		}
		rc, cancel := context.WithCancel(llmtransport.StreamContext(ctx))
		defer cancel()
		st := p.client.Messages.NewStreaming(rc, params, option.WithRequestBody("application/json", body))
		defer func() { _ = st.Close() }()

		var (
			cur       *toolBlock
			inTokens  int
			outTokens int
			// stop is the stop reason message_delta reported, put on done (A-88).
			stop kernel.StopReason
		)
		for st.Next() {
			if err := ctx.Err(); err != nil {
				yield(kernel.StreamChunk{}, err)
				return
			}
			ev := st.Current()
			switch ev.Type {
			case "content_block_start":
				if ev.ContentBlock.Type == "tool_use" {
					cur = &toolBlock{id: ev.ContentBlock.ID, name: ev.ContentBlock.Name}
					// Announce the call now - the arguments can take a long time to
					// generate and the complete "tool_call" only follows at block stop.
					if !yield(kernel.StreamChunk{
						Type:             kernel.ChunkToolCallStart,
						ToolCallProgress: &kernel.ToolCallProgress{ID: cur.id, Name: cur.name},
					}, nil) {
						return
					}
				}
			case "content_block_delta":
				switch ev.Delta.Type {
				case "text_delta":
					chunk := kernel.StreamChunk{Type: kernel.ChunkText}
					// An empty delta is an empty string, not an absent one; a delta
					// without a text field has no content.
					if ev.Delta.JSON.Text.Valid() {
						text := ev.Delta.Text
						chunk.Content = &text
					}
					if !yield(chunk, nil) {
						return
					}
				case "input_json_delta":
					if cur == nil {
						continue
					}
					cur.buf.WriteString(ev.Delta.PartialJSON)
					if ev.Delta.PartialJSON != "" {
						if !yield(kernel.StreamChunk{
							Type: kernel.ChunkToolCallDelta,
							ToolCallProgress: &kernel.ToolCallProgress{
								ID: cur.id, Name: cur.name, ArgumentsDelta: ev.Delta.PartialJSON,
							},
						}, nil) {
							return
						}
					}
				}
			case "content_block_stop":
				if cur != nil {
					args, argsErr := llmhub.ParseToolArguments(cur.buf.String())
					call := kernel.ToolCall{ID: cur.id, Name: cur.name, Arguments: args, ArgumentsError: argsErr}
					cur = nil
					if !yield(kernel.StreamChunk{Type: kernel.ChunkToolCall, ToolCall: &call}, nil) {
						return
					}
				}
			case "message_start":
				inTokens = int(ev.Message.Usage.InputTokens)
			case "message_delta":
				outTokens = int(ev.Usage.OutputTokens)
				if ev.Delta.StopReason != "" {
					stop = mapStopReason(string(ev.Delta.StopReason))
				}
			case "message_stop":
				// A-88: a tool_use block the response was cut off in is handed over,
				// not dropped; its partial arguments give it an ArgumentsError.
				if cur != nil {
					args, argsErr := llmhub.ParseToolArguments(cur.buf.String())
					call := kernel.ToolCall{ID: cur.id, Name: cur.name, Arguments: args, ArgumentsError: argsErr}
					cur = nil
					if !yield(kernel.StreamChunk{Type: kernel.ChunkToolCall, ToolCall: &call}, nil) {
						return
					}
				}
				// Nothing is read after done: the stream contract allows no chunk
				// after it, and the real API closes the stream here.
				yield(kernel.StreamChunk{
					Type:       kernel.ChunkDone,
					Usage:      &kernel.TokenUsage{InputTokens: inTokens, OutputTokens: outTokens},
					StopReason: stop,
				}, nil)
				return
			}
		}
		if err := st.Err(); err != nil {
			// The caller's context decides, not the derived one this function cancels
			// itself: an abort is not a model failure and is never mapped to one.
			if cerr := ctx.Err(); cerr != nil {
				yield(kernel.StreamChunk{}, cerr)
				return
			}
			yield(kernel.StreamChunk{Type: kernel.ChunkError, Error: mapError(err).Message}, nil)
			return
		}
		// The body ended cleanly but before message_stop: the connection was cut
		// between two events.
		if err := ctx.Err(); err != nil {
			yield(kernel.StreamChunk{}, err)
			return
		}
		yield(kernel.StreamChunk{Type: kernel.ChunkError, Error: errTruncated}, nil)
	}
}

// errTruncated is the error chunk text of a stream that ended before message_stop.
const errTruncated = "the stream ended before message_stop: the answer is incomplete"
