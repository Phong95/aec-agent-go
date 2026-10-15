package agentloop

import (
	"context"
	"errors"
	"strings"

	"github.com/Phong95/aec-agent-go/components/llmhub"
	"github.com/Phong95/aec-agent-go/components/trace"
	"github.com/Phong95/aec-agent-go/internal/jsstr"
	"github.com/Phong95/aec-agent-go/kernel"
)

// summarySystemPrompt is the system prompt of the summary call, byte for byte. It
// has no trailing newline.
const summarySystemPrompt = "You are a context-compaction worker.\n" +
	"Create a factual, implementation-ready handoff from the supplied conversation.\n" +
	"Do not invent facts. Preserve exact IDs, paths, commands, URLs, configuration\n" +
	"values, error messages, and unresolved constraints verbatim when present.\n" +
	"\n" +
	"Use exactly these Markdown sections:\n" +
	"## Goal\n" +
	"## Constraints & Preferences\n" +
	"## Progress\n" +
	"### Done\n" +
	"### In Progress\n" +
	"### Blocked\n" +
	"## Key Decisions\n" +
	"## Relevant Files\n" +
	"## Next Steps\n" +
	"## Critical Context"

// Errors of the summarizer. Their text is fixed; the compactor logs it and
// skips the compaction.
var (
	errSummarizeEmptyList    = errors.New("Cannot summarize an empty message list")  //nolint:staticcheck // a user-facing message, capitalised
	errSummarizeEmptySummary = errors.New("Summary model returned an empty summary") //nolint:staticcheck // a user-facing message, capitalised
)

// summarizer is the LLM-facing part of context compaction: it turns
// old conversation messages into one structured handoff the next turn can use.
type summarizer struct {
	models Models
}

// summarize sends the messages to the model as one transcript and returns the
// trimmed summary. It errors on an empty list, on a failed call and on an empty
// summary; the compactor treats every error as "skip the compaction".
//
// The call uses the caller's ctx and is raced against it, so a stopped run stops
// waiting at once even when the model ignores ctx; the error is then ctx.Err().
// maxOutputTokens bounds the summary; a non-empty model overrides the agent's
// default model.
func (s summarizer) summarize(ctx context.Context, msgs []kernel.Message, maxOutputTokens int, model string) (string, error) {
	return s.summarizeTraced(ctx, msgs, maxOutputTokens, model, nil)
}

// traced is summarize with the summary call traced as a "summarize" model-call span
// under the run's current span (the compaction span), so compaction's model latency
// and token cost show in the trace.
func (s summarizer) traced(t *runTrace) SummarizeFunc {
	return func(ctx context.Context, msgs []kernel.Message, maxOutputTokens int, model string) (string, error) {
		return s.summarizeTraced(ctx, msgs, maxOutputTokens, model, t)
	}
}

// summarizeTraced is summarize, with the span when t is not nil.
func (s summarizer) summarizeTraced(ctx context.Context, msgs []kernel.Message, maxOutputTokens int, model string, t *runTrace) (string, error) {
	if len(msgs) == 0 {
		return "", errSummarizeEmptyList
	}
	opts := llmhub.ModelOptions{SystemPrompt: summarySystemPrompt, MaxTokens: &maxOutputTokens}
	if model != "" {
		opts.Model = model
	}
	input := []kernel.Message{{Role: kernel.RoleUser, Content: renderTranscript(msgs)}}
	var span trace.SpanHandle = noSpan{}
	if t != nil {
		attrs := obj("inputMessageCount", len(msgs), "maxOutputTokens", maxOutputTokens)
		if model != "" {
			attrs.Set("model", model)
		}
		span = t.span("summarize", trace.KindModelCall, attrs)
	}
	resp, aborted, err := raceAbort(ctx, func() (kernel.ModelResponse, error) {
		return s.models.Chat(ctx, input, opts)
	})
	if aborted {
		span.End(obj("aborted", true))
		return "", ctx.Err()
	}
	if err != nil {
		span.End(obj("error", err.Error()))
		return "", err
	}
	normalized := jsstr.Trim(resp.Message.Content)
	end := obj(
		"stopReason", string(resp.StopReason),
		"inputTokens", resp.Usage.InputTokens,
		"outputTokens", resp.Usage.OutputTokens,
		"summaryLength", kernel.UTF16Len(normalized),
	)
	if normalized == "" {
		end.Set("error", errSummarizeEmptySummary.Error())
	}
	span.End(end)
	if normalized == "" {
		return "", errSummarizeEmptySummary
	}
	return normalized, nil
}

// renderTranscript renders each message as `[ROLE] content`, joined by a blank
// line. Tool results are invisible on purpose: their message content is empty and
// only the content is rendered.
func renderTranscript(msgs []kernel.Message) string {
	parts := make([]string, len(msgs))
	for i, m := range msgs {
		parts[i] = "[" + strings.ToUpper(string(m.Role)) + "] " + m.Content
	}
	return strings.Join(parts, "\n\n")
}
