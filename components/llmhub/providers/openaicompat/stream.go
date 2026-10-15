package openaicompat

import (
	"bytes"
	"context"
	"io"
	"iter"
	"net/http"
	"strings"

	sdk "github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"

	"github.com/Phong95/aec-agent-go/components/llmhub"
	"github.com/Phong95/aec-agent-go/internal/llmtransport"
	"github.com/Phong95/aec-agent-go/kernel"
)

// toolBuf is one tool call the stream is writing: its identity, the arguments text
// collected so far, whether the call was announced, and how much of the arguments
// was already sent as progress. There is one per delta index.
type toolBuf struct {
	id         string
	name       string
	args       strings.Builder
	started    bool
	emittedLen int
}

// toolBufs holds the buffers by delta index and remembers the order the indexes
// were first seen in: the complete tool_call chunks come out in that order, as the
// iteration order of a JS Map does.
type toolBufs struct {
	byIndex map[int64]*toolBuf
	order   []*toolBuf
}

// flush makes the complete tool_call chunk of every buffered call, in first-seen
// order, and then clears the buffers, so a call is emitted once however many
// chunks carry a finish_reason. It returns false
// when the consumer stopped.
func (b *toolBufs) flush(yield func(kernel.StreamChunk, error) bool) bool {
	order := b.order
	b.byIndex, b.order = nil, nil
	for _, buf := range order {
		args, argsErr := llmhub.ParseToolArguments(buf.args.String())
		call := kernel.ToolCall{ID: buf.id, Name: buf.name, Arguments: args, ArgumentsError: argsErr}
		if !yield(kernel.StreamChunk{Type: kernel.ChunkToolCall, ToolCall: &call}, nil) {
			return false
		}
	}
	return true
}

// get returns the buffer of index, creating it from the delta on first sight
// (a missing id or function name is "").
func (b *toolBufs) get(index int64, id, name string) *toolBuf {
	if buf, ok := b.byIndex[index]; ok {
		return buf
	}
	if b.byIndex == nil {
		b.byIndex = map[int64]*toolBuf{}
	}
	buf := &toolBuf{id: id, name: name}
	b.byIndex[index] = buf
	b.order = append(b.order, buf)
	return buf
}

// Stream sends the request and turns the server-sent chunks into chunks:
//   - delta.content gives a text chunk when it is not empty;
//   - every tool-call delta goes to the buffer of its index. The call is announced
//     with tool_call_start once its id and name are both known, and any argument
//     text that arrived before that is sent right after the start as one
//     tool_call_delta; later argument text gives a tool_call_delta with the part
//     not sent yet;
//   - at a chunk that has a finish_reason, a tool_call chunk is made for every
//     buffered call, in the order the indexes were first seen, with
//     llmhub.ParseToolArguments (a model that writes broken JSON produces a call that
//     carries an ArgumentsError instead of failing the stream). The buffers are
//     cleared after the flush, so each call is emitted exactly once even when a
//     second chunk has a finish_reason. A finish_reason counts even on a choice
//     without a delta;
//   - the usage of the chunk that carries one (the last chunk, with an empty
//     choices list, when stream_options asks for it) is sent with done;
//   - the finish_reason, mapped with mapFinishReason, is sent with done as its stop
//     reason (A-88; the last one wins when several chunks carry one, and done has
//     none when no chunk did);
//   - done comes when the stream ends after a terminal event: a [DONE] line or a
//     chunk with a finish_reason. A stream that ends with neither was cut short
//     (the connection closed in the middle of the answer) and ends with one
//     terminal error chunk (errTruncated), so a cut answer is never taken for a complete
//     one. The SDK reads [DONE] and hides it, so doneWatch looks for it in the
//     body.
//
// A failure is one terminal error chunk with a nil error and no done; only
// ctx.Err() goes in the error slot. The sequence has no goroutine of its own: it
// reads the SDK stream on the caller's goroutine, and breaking out of the loop
// closes the stream and cancels its context, which releases the connection.
//
// The SDK's chat completion accumulator is not used: it would hide the per-delta
// progress chunks.
func (p *Provider) Stream(ctx context.Context, msgs []kernel.Message, opts llmhub.ModelOptions) iter.Seq2[kernel.StreamChunk, error] {
	return func(yield func(kernel.StreamChunk, error) bool) {
		if err := ctx.Err(); err != nil {
			yield(kernel.StreamChunk{}, err)
			return
		}
		body, err := p.buildRequest(msgs, opts, true)
		if err != nil {
			yield(kernel.StreamChunk{Type: kernel.ChunkError, Error: err.Error()}, nil)
			return
		}
		rc, cancel := context.WithCancel(llmtransport.StreamContext(ctx))
		defer cancel()
		var (
			fail failure
			end  doneWatch
		)
		st := p.chat.NewStreaming(rc, sdk.ChatCompletionNewParams{},
			option.WithRequestBody("application/json", body), option.WithMiddleware(fail.middleware, end.middleware))
		defer func() { _ = st.Close() }()

		var (
			bufs  toolBufs
			usage *kernel.TokenUsage
			// finished is set by the first chunk with a finish_reason.
			finished bool
			// stop is the mapped finish_reason, put on done (A-88).
			stop kernel.StopReason
		)
		for st.Next() {
			if err := ctx.Err(); err != nil {
				yield(kernel.StreamChunk{}, err)
				return
			}
			chunk := st.Current()
			if chunk.JSON.Usage.Valid() {
				usage = &kernel.TokenUsage{
					InputTokens:  int(chunk.Usage.PromptTokens),
					OutputTokens: int(chunk.Usage.CompletionTokens),
				}
			}

			// A chunk with no choice is skipped. A choice with no delta still has
			// its finish_reason read, so the buffered calls of such a server still
			// come out.
			if len(chunk.Choices) == 0 {
				continue
			}
			choice := chunk.Choices[0]
			if !choice.JSON.Delta.Valid() {
				if choice.FinishReason != "" {
					finished = true
					stop = mapFinishReason(choice.FinishReason)
					if !bufs.flush(yield) {
						return
					}
				}
				continue
			}

			if choice.Delta.Content != "" {
				text := choice.Delta.Content
				if !yield(kernel.StreamChunk{Type: kernel.ChunkText, Content: &text}, nil) {
					return
				}
			}

			for _, tc := range choice.Delta.ToolCalls {
				buf := bufs.get(tc.Index, tc.ID, tc.Function.Name)
				if tc.ID != "" {
					buf.id = tc.ID
				}
				if tc.Function.Name != "" {
					buf.name = tc.Function.Name
				}
				if tc.Function.Arguments != "" {
					buf.args.WriteString(tc.Function.Arguments)
				}

				// Stream progress while the arguments are still being generated -
				// the complete "tool_call" only follows at finish_reason. The call
				// is announced once its id and name are known; any argument text
				// that arrived before that is sent right after the start.
				if !buf.started && buf.id != "" && buf.name != "" {
					buf.started = true
					if !yield(kernel.StreamChunk{
						Type:             kernel.ChunkToolCallStart,
						ToolCallProgress: &kernel.ToolCallProgress{ID: buf.id, Name: buf.name},
					}, nil) {
						return
					}
				}
				if buf.started && buf.args.Len() > buf.emittedLen {
					delta := buf.args.String()[buf.emittedLen:]
					buf.emittedLen = buf.args.Len()
					if !yield(kernel.StreamChunk{
						Type:             kernel.ChunkToolCallDelta,
						ToolCallProgress: &kernel.ToolCallProgress{ID: buf.id, Name: buf.name, ArgumentsDelta: delta},
					}, nil) {
						return
					}
				}
			}

			if choice.FinishReason != "" {
				finished = true
				stop = mapFinishReason(choice.FinishReason)
				if !bufs.flush(yield) {
					return
				}
			}
		}
		if err := st.Err(); err != nil {
			// The caller's context decides, not the derived one this function cancels
			// itself: an abort is not a model failure and is never mapped to one.
			if cerr := ctx.Err(); cerr != nil {
				yield(kernel.StreamChunk{}, cerr)
				return
			}
			yield(kernel.StreamChunk{Type: kernel.ChunkError, Error: mapError(err, &fail).Message}, nil)
			return
		}
		if err := ctx.Err(); err != nil {
			yield(kernel.StreamChunk{}, err)
			return
		}
		if !finished && !end.seen() {
			yield(kernel.StreamChunk{Type: kernel.ChunkError, Error: errTruncated}, nil)
			return
		}
		// A call that a server streamed after its finish_reason still comes out once.
		if !bufs.flush(yield) {
			return
		}
		yield(kernel.StreamChunk{Type: kernel.ChunkDone, Usage: usage, StopReason: stop}, nil)
	}
}

// errTruncated is the error chunk text of a stream that ended before [DONE] and
// before any finish_reason.
const errTruncated = "the stream ended before [DONE]: the answer is incomplete"

// doneWatch notes whether the body of a stream carried the [DONE] event: a data
// line whose value starts with [DONE], the test the SDK uses to end the stream. The
// middleware wraps the body of every successful attempt; the last attempt is the one
// the stream reads. One doneWatch belongs to one call.
type doneWatch struct {
	body *doneSniffer
}

// middleware wraps the body of a 2xx response.
func (w *doneWatch) middleware(req *http.Request, next option.MiddlewareNext) (*http.Response, error) {
	res, err := next(req)
	w.body = nil
	if err == nil && res != nil && res.StatusCode < 300 && res.Body != nil {
		w.body = &doneSniffer{ReadCloser: res.Body}
		res.Body = w.body
	}
	return res, err
}

// seen reports whether [DONE] was read.
func (w *doneWatch) seen() bool { return w.body != nil && w.body.done }

// doneSniffer passes the body through and looks at the start of every line. It
// keeps only the first bytes of the line being read, so it costs nothing per event.
type doneSniffer struct {
	io.ReadCloser
	head []byte // the first bytes of the current line, at most len(donePrefix)+1
	skip bool   // the current line is longer than head and cannot be [DONE]
	done bool
}

// Read implements io.Reader.
func (d *doneSniffer) Read(p []byte) (int, error) {
	n, err := d.ReadCloser.Read(p)
	for _, b := range p[:n] {
		if b == '\n' || b == '\r' {
			d.endLine()
			continue
		}
		if !d.skip {
			if len(d.head) < maxDoneLine {
				d.head = append(d.head, b)
			} else {
				d.endLine()
				d.skip = true
			}
		}
	}
	if err != nil {
		// An unterminated last line still counts, as the SDK scanner reads it.
		d.endLine()
	}
	return n, err
}

// maxDoneLine is the longest line start that is looked at: "data: [DONE]" with
// room for a little whitespace.
const maxDoneLine = 32

// endLine tests the line that just ended.
func (d *doneSniffer) endLine() {
	if !d.skip {
		line := bytes.TrimPrefix(d.head, []byte("data:"))
		if len(line) < len(d.head) && bytes.HasPrefix(bytes.TrimPrefix(line, []byte(" ")), []byte("[DONE]")) {
			d.done = true
		}
	}
	d.head, d.skip = d.head[:0], false
}
