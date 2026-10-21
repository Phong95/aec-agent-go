package grouphub

import (
	"context"
	"slices"
	"sync"
	"time"

	"github.com/Phong95/aec-agent-go/components/grouphub/group"
	"github.com/Phong95/aec-agent-go/internal/jsstr"
	"github.com/Phong95/aec-agent-go/kernel"
)

// resultPreviewChars is the most characters of a tool step's result preview.
const resultPreviewChars = 300

// presenceFunc reports what the member writing an answer is doing.
type presenceFunc func(state group.PresenceState, tool string)

// answerWriter writes one member answer into the thread: it opens it ("running"), adds
// streamed text (as deltas) and tool steps, then stores how it ended. It is safe for
// concurrent use (a hand-off's answer is finished by the end of its child run).
type answerWriter struct {
	th *thread
	// base is the answer's session, group, member, round, summary flag and child run.
	base group.Message

	mu         sync.Mutex
	message    *group.Message
	toolStarts map[string]time.Time
	// gap: a tool step came after the last text, so the next text starts a new paragraph.
	gap         bool
	writing     bool
	finished    bool
	streamError string
}

func newAnswerWriter(th *thread, base group.Message) *answerWriter {
	return &answerWriter{th: th, base: base, toolStarts: map[string]time.Time{}}
}

func (w *answerWriter) text() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.message == nil {
		return ""
	}
	return w.message.Text
}

func (w *answerWriter) hasText() bool { return jsstr.Trim(w.text()) != "" }

func (w *answerWriter) hasTool(toolCallID string) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return toolCallID != "" && w.message != nil && slices.ContainsFunc(w.message.Tools, func(t group.ToolStep) bool { return t.ID == toolCallID })
}

func (w *answerWriter) isFinished() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.finished
}

func (w *answerWriter) streamErr() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.streamError
}

// open appends the answer, or resets existing (a retry) in place.
func (w *answerWriter) open(ctx context.Context, existing *group.Message) (group.Message, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	var m group.Message
	var err error
	if existing != nil {
		reset := existing.Clone()
		reset.Error, reset.Usage, reset.Tools = "", nil, nil
		reset.Text, reset.Status = "", group.StatusRunning
		m, err = w.th.update(ctx, reset)
	} else {
		fresh := w.base.Clone()
		fresh.Kind, fresh.Text, fresh.Status = group.KindAgent, "", group.StatusRunning
		m, err = w.th.append(ctx, fresh)
	}
	if err != nil {
		return group.Message{}, err
	}
	w.message = &m
	return m.Clone(), nil
}

// chunk adds one chunk of the member's stream to the answer.
func (w *answerWriter) chunk(ctx context.Context, c kernel.StreamChunk, presence presenceFunc) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	m := w.message
	if m == nil || w.finished {
		return nil
	}
	switch c.Type {
	case kernel.ChunkText:
		if c.Content == nil || *c.Content == "" {
			return nil
		}
		piece := *c.Content
		if w.gap && m.Text != "" {
			piece = "\n\n" + piece
		}
		w.gap = false
		m.Text += piece
		w.th.delta(*m, piece)
		if !w.writing {
			w.writing = true
			presence(group.PresenceWriting, "")
		}
	case kernel.ChunkToolCall:
		if c.ToolCall != nil {
			if err := w.toolCallLocked(ctx, *c.ToolCall); err != nil {
				return err
			}
		}
		w.writing = false
		if c.ToolCall != nil {
			presence(group.PresenceTool, c.ToolCall.Name)
		}
	case kernel.ChunkToolResult:
		if c.ToolResult != nil {
			if err := w.toolResultLocked(ctx, *c.ToolResult); err != nil {
				return err
			}
		}
		presence(group.PresenceThinking, "")
	case kernel.ChunkError:
		w.streamError = c.Error
	default:
	}
	return nil
}

// finish stores how the answer ended (once): running tool steps become errors, usage is
// kept when it counts something, errMsg replaces the old error.
func (w *answerWriter) finish(ctx context.Context, status group.MessageStatus, usage *kernel.TokenUsage, errMsg string) (*group.Message, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.message == nil || w.finished {
		if w.message == nil {
			return nil, nil
		}
		out := w.message.Clone()
		return &out, nil
	}
	w.finished = true
	next := w.message.Clone()
	for i := range next.Tools {
		if next.Tools[i].Status == group.ToolRunning {
			next.Tools[i].Status = group.ToolError
		}
	}
	next.Status = status
	if usage != nil && usage.InputTokens+usage.OutputTokens > 0 {
		u := usage.Clone()
		next.Usage = &u
	}
	next.Error = errMsg
	stored, err := w.th.update(ctx, next)
	if err != nil {
		return nil, err
	}
	w.message = &stored
	out := stored.Clone()
	return &out, nil
}

func (w *answerWriter) toolCallLocked(ctx context.Context, call kernel.ToolCall) error {
	w.gap = true
	w.toolStarts[call.ID] = time.Now()
	next := w.message.Clone()
	next.Tools = append(next.Tools, group.ToolStep{ID: call.ID, Name: call.Name, Status: group.ToolRunning, Arguments: argumentsOrEmpty(call.Arguments)})
	stored, err := w.th.update(ctx, next)
	if err != nil {
		return err
	}
	w.message = &stored
	return nil
}

func (w *answerWriter) toolResultLocked(ctx context.Context, result kernel.ToolResult) error {
	next := w.message.Clone()
	for i, t := range next.Tools {
		if t.ID != result.ToolCallID {
			continue
		}
		t.Status = group.ToolDone
		if result.IsError != nil && *result.IsError {
			t.Status = group.ToolError
		}
		t.ResultPreview = jsstr.TruncateUTF16(result.Content, resultPreviewChars)
		if started, ok := w.toolStarts[result.ToolCallID]; ok {
			d := time.Since(started).Milliseconds()
			t.DurationMs = &d
		}
		next.Tools[i] = t
	}
	stored, err := w.th.update(ctx, next)
	if err != nil {
		return err
	}
	w.message = &stored
	return nil
}

// argumentsOrEmpty is the call's arguments, {} when the model sent none.
func argumentsOrEmpty(a kernel.Arguments) kernel.Arguments {
	if len(a) == 0 {
		return kernel.Arguments("{}")
	}
	return slices.Clone(a)
}
