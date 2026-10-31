package bedrock

import (
	"context"
	"iter"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime/types"

	"github.com/Phong95/aec-agent-go/components/llmhub"
	"github.com/Phong95/aec-agent-go/kernel"
)

// toolBuf is one tool-call block the stream is writing: its identity and the
// arguments text collected so far.
type toolBuf struct {
	id      string
	name    string
	jsonBuf []byte
}

// Stream sends the request and turns the event stream into chunks:
//   - a contentBlockStart for a tool use opens a buffer for its block and gives a
//     tool_call_start, so the call is announced while its arguments are still
//     being generated;
//   - a contentBlockDelta with text gives a text chunk when the text is not empty;
//     one with a tool-use input appends it to the buffer of ITS block and gives a
//     tool_call_delta when the input is not empty;
//   - a contentBlockStop gives the complete tool_call of that block, built with
//     llmhub.ParseToolArguments (a model that writes broken JSON produces a call that
//     carries an ArgumentsError instead of failing the stream), and frees the
//     buffer;
//   - the usage of the metadata event is sent with done (a missing count is 0);
//   - messageStart, messageStop (so no stop reason), reasoning deltas and every
//     other event are ignored; since A-88 messageStop is read for its stop reason
//     only, which rides on done mapped with mapStopReason (done has none when
//     messageStop carried none);
//   - a response with no stream gives a bare done (with no stop reason).
//
// Bedrock ends every answer with messageStop and then metadata. A stream whose
// channel closes cleanly before either arrived was cut short and ends with one
// terminal error chunk (errTruncated) in place of done. A cut inside an event is an es.Err() and maps as a
// failure, as before.
//
// There is one buffer per contentBlockIndex (a missing index is 0). Bedrock tags
// every event with its block index and a model may stream several tool calls whose
// blocks interleave, so a single "current" call would mix their arguments into one
// invalid JSON string.
//
// A failure is one terminal error chunk with a nil error and no done; only
// ctx.Err() goes in the error slot. The sequence has no goroutine of its own: it
// reads the event channel on the caller's goroutine, selecting on the caller's
// context as well, because a fake stream has no HTTP body for a cancel to reach.
// Breaking out of the loop, or any other exit, closes the stream and cancels its
// context, which releases the connection.
func (p *Provider) Stream(ctx context.Context, msgs []kernel.Message, opts llmhub.ModelOptions) iter.Seq2[kernel.StreamChunk, error] {
	return func(yield func(kernel.StreamChunk, error) bool) {
		if err := ctx.Err(); err != nil {
			yield(kernel.StreamChunk{}, err)
			return
		}
		req, err := p.buildRequest(msgs, opts)
		if err != nil {
			yield(kernel.StreamChunk{Type: kernel.ChunkError, Error: err.Error()}, nil)
			return
		}
		rc, cancel := context.WithCancel(ctx)
		defer cancel()
		es, err := p.converse.ConverseStream(rc, req.streamInput())
		if err != nil {
			// The caller's context decides, not the derived one this function cancels
			// itself: an abort is not a model failure and is never mapped to one.
			if cerr := ctx.Err(); cerr != nil {
				yield(kernel.StreamChunk{}, cerr)
				return
			}
			yield(kernel.StreamChunk{Type: kernel.ChunkError, Error: mapError(err, req.modelID).Message}, nil)
			return
		}
		if es == nil {
			yield(kernel.StreamChunk{Type: kernel.ChunkDone}, nil)
			return
		}
		defer func() { _ = es.Close() }()

		toolCalls := map[int32]*toolBuf{}
		var usage *kernel.TokenUsage
		// stop is the stop reason messageStop reported, put on done (A-88).
		var stop kernel.StopReason
		// ended is set by messageStop or metadata, the two terminal events.
		ended := false
		events := es.Events()
	loop:
		for {
			select {
			case <-ctx.Done():
				yield(kernel.StreamChunk{}, ctx.Err())
				return
			case ev, ok := <-events:
				if !ok {
					break loop
				}
				switch e := ev.(type) {
				case *types.ConverseStreamOutputMemberContentBlockStart:
					start, isTool := e.Value.Start.(*types.ContentBlockStartMemberToolUse)
					if !isTool {
						continue
					}
					buf := &toolBuf{id: aws.ToString(start.Value.ToolUseId), name: aws.ToString(start.Value.Name)}
					toolCalls[blockIndex(e.Value.ContentBlockIndex)] = buf
					// Announce the call now: the arguments can take a long time to
					// generate and the complete tool_call only follows at block stop.
					if !yield(kernel.StreamChunk{
						Type:             kernel.ChunkToolCallStart,
						ToolCallProgress: &kernel.ToolCallProgress{ID: buf.id, Name: buf.name},
					}, nil) {
						return
					}
				case *types.ConverseStreamOutputMemberContentBlockDelta:
					buf := toolCalls[blockIndex(e.Value.ContentBlockIndex)]
					switch d := e.Value.Delta.(type) {
					case *types.ContentBlockDeltaMemberText:
						if d.Value != "" {
							text := d.Value
							if !yield(kernel.StreamChunk{Type: kernel.ChunkText, Content: &text}, nil) {
								return
							}
						}
					case *types.ContentBlockDeltaMemberReasoningContent:
						// A-178: the model's reasoning text (a signature or redacted delta
						// carries none).
						if t, ok := d.Value.(*types.ReasoningContentBlockDeltaMemberText); ok && t.Value != "" {
							reasoning := t.Value
							if !yield(kernel.StreamChunk{Type: kernel.ChunkReasoning, Content: &reasoning}, nil) {
								return
							}
						}
					case *types.ContentBlockDeltaMemberToolUse:
						if buf == nil {
							continue
						}
						input := aws.ToString(d.Value.Input)
						buf.jsonBuf = append(buf.jsonBuf, input...)
						if input != "" {
							if !yield(kernel.StreamChunk{
								Type:             kernel.ChunkToolCallDelta,
								ToolCallProgress: &kernel.ToolCallProgress{ID: buf.id, Name: buf.name, ArgumentsDelta: input},
							}, nil) {
								return
							}
						}
					default:
						// Reasoning, citation and other deltas are ignored.
					}
				case *types.ConverseStreamOutputMemberContentBlockStop:
					index := blockIndex(e.Value.ContentBlockIndex)
					buf := toolCalls[index]
					if buf == nil {
						continue
					}
					args, argsErr := llmhub.ParseToolArguments(string(buf.jsonBuf))
					call := kernel.ToolCall{ID: buf.id, Name: buf.name, Arguments: args, ArgumentsError: argsErr}
					if !yield(kernel.StreamChunk{Type: kernel.ChunkToolCall, ToolCall: &call}, nil) {
						return
					}
					delete(toolCalls, index)
				case *types.ConverseStreamOutputMemberMessageStop:
					ended = true
					if e.Value.StopReason != "" {
						stop = mapStopReason(string(e.Value.StopReason))
					}
				case *types.ConverseStreamOutputMemberMetadata:
					ended = true
					if e.Value.Usage != nil {
						usage = &kernel.TokenUsage{
							InputTokens:  int(aws.ToInt32(e.Value.Usage.InputTokens)),
							OutputTokens: int(aws.ToInt32(e.Value.Usage.OutputTokens)),
						}
					}
				default:
					// messageStart and unknown members are ignored (messageStop only
					// marks the end, and gives the stop reason since A-88, above).
				}
			}
		}
		// The event channel is closed: a failure in the middle of the stream arrives
		// here, not on the channel.
		if err := es.Err(); err != nil {
			if cerr := ctx.Err(); cerr != nil {
				yield(kernel.StreamChunk{}, cerr)
				return
			}
			yield(kernel.StreamChunk{Type: kernel.ChunkError, Error: mapError(err, req.modelID).Message}, nil)
			return
		}
		if !ended {
			yield(kernel.StreamChunk{Type: kernel.ChunkError, Error: errTruncated}, nil)
			return
		}
		yield(kernel.StreamChunk{Type: kernel.ChunkDone, Usage: usage, StopReason: stop}, nil)
	}
}

// errTruncated is the error chunk text of a stream that ended before messageStop
// and metadata.
const errTruncated = "the stream ended before messageStop: the answer is incomplete"

// blockIndex is the content block index of an event, 0 when the event has none.
func blockIndex(i *int32) int32 {
	return aws.ToInt32(i)
}
