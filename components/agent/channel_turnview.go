package agent

import (
	"context"
	"strings"
	"sync"
	"time"
	"unicode/utf16"

	"github.com/Phong95/aec-agent-go/components/channelplatformhub/channel"
	"github.com/Phong95/aec-agent-go/internal/jsstr"
	"github.com/Phong95/aec-agent-go/kernel"
)

// previewInterval is how often a streamed preview is edited at most (platforms
// rate-limit edits).
const previewInterval = time.Second

// minPreviewChars: the preview is first sent once this much text is written, so a short
// reply arrives in one piece.
const minPreviewChars = 40

// maxToolLine is the longest tool line of the tool-progress message.
const maxToolLine = 60

// previewCursor is shown at the end of a preview while the reply is still being written.
var previewCursor = " " + string(rune(0x258d))

type toolLine struct {
	id    string
	name  string
	state string // "running", "done" or "failed"
}

// turnView is how one channel turn shows in the chat (A-100 phase 2), the TurnView of
// core/ts, fed the turn's stream chunks:
//
//   - Preview streaming (Settings.Streaming, an Editor adapter): the reply is sent once
//     it has some text and then edited in place as more arrives. Text the agent writes
//     before calling tools stays in the chat as its own message.
//   - Tool progress (Settings.ToolProgress, an Editor adapter): one message lists the
//     tools the agent runs and is edited as they finish.
//   - finish: the final reply, rendered as Markdown when the adapter can, split to the
//     platform's length. The preview message becomes its first part.
//
// Adapter calls run one at a time, in order, on a worker goroutine. A failed send or edit
// is reported and skipped; it never fails the turn.
type turnView struct {
	ctx          context.Context
	adapter      channel.Adapter
	editor       channel.Editor
	caps         channel.Capabilities
	target       channel.Target
	streaming    bool
	toolProgress bool
	onError      func(error)

	mu sync.Mutex
	// buffer is the text of the model response being written now.
	buffer string
	// preview is the preview message; previewShown its text last sent, so an unchanged
	// preview is not edited again.
	preview       *channel.MessageRef
	previewShown  string
	previewTimer  *time.Timer
	lastPreviewAt time.Time
	tools         []toolLine
	toolMessage   *channel.MessageRef
	closed        bool

	ops opQueue
}

func newTurnView(ctx context.Context, adapter channel.Adapter, target channel.Target, settings channel.Settings, onError func(error)) *turnView {
	editor, canEdit := adapter.(channel.Editor)
	v := &turnView{
		ctx: ctx, adapter: adapter, editor: editor, caps: adapter.Capabilities(), target: target,
		streaming: settings.Streaming && canEdit, toolProgress: settings.ToolProgress && canEdit, onError: onError,
	}
	v.ops.cond = sync.NewCond(&v.ops.mu)
	return v
}

// chunk feeds one chunk of the turn (TurnContext.OnChunk).
func (v *turnView) chunk(c kernel.StreamChunk) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.closed {
		return
	}
	switch c.Type {
	case kernel.ChunkText:
		if c.Content == nil || *c.Content == "" {
			return
		}
		v.buffer += *c.Content
		if v.streaming {
			v.schedulePreviewLocked()
		}
	case kernel.ChunkToolCall:
		// Text written before the tools run is a message of its own (streaming only:
		// without it the chat gets the final reply only, as before).
		if v.streaming && jsstr.Trim(v.buffer) != "" {
			v.settleIntermediateLocked()
		}
		v.buffer = ""
		if c.ToolCall != nil && v.toolProgress {
			v.tools = append(v.tools, toolLine{id: c.ToolCall.ID, name: c.ToolCall.Name, state: "running"})
			v.queueToolMessageLocked()
		}
	case kernel.ChunkToolResult:
		if c.ToolResult == nil {
			return
		}
		for i := range v.tools {
			if v.tools[i].id == c.ToolResult.ToolCallID {
				v.tools[i].state = "done"
				if c.ToolResult.IsError != nil && *c.ToolResult.IsError {
					v.tools[i].state = "failed"
				}
				v.queueToolMessageLocked()
				break
			}
		}
	default:
	}
}

// finish sends the final reply. The preview (if any) is edited into the first part; the
// rest are sent as new messages. It returns the reply's messages.
func (v *turnView) finish(output string) ([]channel.MessageRef, error) {
	v.close()
	v.ops.wait()
	parts := v.split(output)
	var refs []channel.MessageRef
	first := 0
	v.mu.Lock()
	preview := v.preview
	v.preview = nil
	v.mu.Unlock()
	if preview != nil && len(parts) > 0 {
		if err := v.editor.Edit(v.ctx, *preview, v.outbound(parts[0])); err != nil {
			v.onError(err)
		} else {
			refs = append(refs, *preview)
			first = 1
		}
	}
	for _, part := range parts[first:] {
		ref, err := v.adapter.Send(v.ctx, v.target, v.outbound(part))
		if err != nil {
			return refs, err
		}
		refs = append(refs, ref)
	}
	return refs, nil
}

// abandon is for a turn that ended without a reply (stopped or failed): stop updating,
// and drop the cursor from a preview so it reads as what was written so far.
func (v *turnView) abandon() {
	v.close()
	v.mu.Lock()
	text := jsstr.Trim(v.buffer)
	v.mu.Unlock()
	v.ops.push(func() {
		v.mu.Lock()
		preview := v.preview
		v.preview = nil
		v.mu.Unlock()
		if preview != nil && text != "" {
			v.report(v.editor.Edit(v.ctx, *preview, channel.OutboundMessage{Text: v.fit(text)}))
		}
	})
	v.ops.wait()
}

// ---------------------------------------------------------------------------

func (v *turnView) close() {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.closed = true
	if v.previewTimer != nil {
		v.previewTimer.Stop()
		v.previewTimer = nil
	}
}

func (v *turnView) schedulePreviewLocked() {
	if v.previewTimer != nil {
		return
	}
	if v.preview == nil && units16(jsstr.Trim(v.buffer)) < minPreviewChars {
		return
	}
	wait := max(0, time.Until(v.lastPreviewAt.Add(previewInterval)))
	v.previewTimer = time.AfterFunc(wait, func() {
		v.mu.Lock()
		v.previewTimer = nil
		closed := v.closed
		v.mu.Unlock()
		if !closed {
			v.ops.push(v.updatePreview)
		}
	})
}

func (v *turnView) updatePreview() {
	v.mu.Lock()
	text := jsstr.Trim(v.buffer)
	preview := v.preview
	shownBefore := v.previewShown
	closed := v.closed
	v.mu.Unlock()
	// A timer that fired as the turn ended: finish writes the reply instead.
	if closed {
		return
	}
	// Too long for one message: the final reply is split; the preview stays as it is.
	if text == "" || units16(text)+units16(previewCursor) > v.caps.MaxMessageLength {
		return
	}
	shown := text + previewCursor
	if shown == shownBefore {
		return
	}
	v.mu.Lock()
	v.lastPreviewAt = time.Now()
	v.mu.Unlock()
	if preview != nil {
		if err := v.editor.Edit(v.ctx, *preview, channel.OutboundMessage{Text: shown}); err != nil {
			v.onError(err)
			return
		}
	} else {
		ref, err := v.adapter.Send(v.ctx, v.target, channel.OutboundMessage{Text: shown})
		if err != nil {
			v.onError(err)
			return
		}
		v.mu.Lock()
		v.preview = &ref
		v.mu.Unlock()
	}
	v.mu.Lock()
	v.previewShown = shown
	v.mu.Unlock()
}

// settleIntermediateLocked finishes the preview of the text written before a tool call,
// or sends that text. The caller holds mu.
func (v *turnView) settleIntermediateLocked() {
	if v.previewTimer != nil {
		v.previewTimer.Stop()
		v.previewTimer = nil
	}
	parts := v.split(v.buffer)
	// Read the preview inside the queue: a preview send still in flight has finished by then.
	v.ops.push(func() {
		v.mu.Lock()
		preview := v.preview
		v.preview = nil
		v.previewShown = ""
		v.mu.Unlock()
		rest := parts
		if preview != nil && len(parts) > 0 {
			if err := v.editor.Edit(v.ctx, *preview, v.outbound(parts[0])); err != nil {
				v.onError(err)
				return
			}
			rest = parts[1:]
		}
		for _, part := range rest {
			if _, err := v.adapter.Send(v.ctx, v.target, v.outbound(part)); err != nil {
				v.onError(err)
				return
			}
		}
	})
}

func (v *turnView) queueToolMessageLocked() {
	v.ops.push(func() {
		v.mu.Lock()
		lines := make([]string, 0, len(v.tools))
		for _, t := range v.tools {
			icon := symRunning
			switch t.state {
			case "done":
				icon = symCheck
			case "failed":
				icon = symWarning
			}
			lines = append(lines, icon+" "+truncate16(t.name, maxToolLine))
		}
		ref := v.toolMessage
		v.mu.Unlock()
		text := v.fit(strings.Join(lines, "\n"))
		if ref != nil {
			v.report(v.editor.Edit(v.ctx, *ref, channel.OutboundMessage{Text: text}))
			return
		}
		sent, err := v.adapter.Send(v.ctx, v.target, channel.OutboundMessage{Text: text})
		if err != nil {
			v.onError(err)
			return
		}
		v.mu.Lock()
		v.toolMessage = &sent
		v.mu.Unlock()
	})
}

func (v *turnView) report(err error) {
	if err != nil {
		v.onError(err)
	}
}

func (v *turnView) split(text string) []string {
	if v.caps.Markdown {
		return channel.SplitMarkdown(text, v.caps.MaxMessageLength)
	}
	return channel.SplitMessage(text, v.caps.MaxMessageLength)
}

func (v *turnView) outbound(text string) channel.OutboundMessage {
	if v.caps.Markdown {
		return channel.OutboundMessage{Text: text, Format: channel.FormatMarkdown}
	}
	return channel.OutboundMessage{Text: text}
}

// fit keeps the last part of a text that is too long for one message.
func (v *turnView) fit(text string) string {
	u := utf16.Encode([]rune(text))
	if len(u) <= v.caps.MaxMessageLength {
		return text
	}
	return symEllipsis + string(utf16.Decode(u[len(u)-v.caps.MaxMessageLength+1:]))
}

// opQueue runs pushed functions one at a time, in push order, on a worker goroutine that
// ends when the queue is empty.
type opQueue struct {
	mu      sync.Mutex
	cond    *sync.Cond
	items   []func()
	running bool
}

func (q *opQueue) push(f func()) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.items = append(q.items, f)
	if q.running {
		return
	}
	q.running = true
	go q.drain()
}

func (q *opQueue) drain() {
	for {
		q.mu.Lock()
		if len(q.items) == 0 {
			q.running = false
			q.cond.Broadcast()
			q.mu.Unlock()
			return
		}
		f := q.items[0]
		q.items = q.items[1:]
		q.mu.Unlock()
		f()
	}
}

// wait returns once the queue is empty and no function runs.
func (q *opQueue) wait() {
	q.mu.Lock()
	defer q.mu.Unlock()
	for q.running {
		q.cond.Wait()
	}
}
