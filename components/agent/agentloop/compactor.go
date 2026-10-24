package agentloop

import (
	"context"
	"log/slog"
	"strconv"
	"strings"

	"github.com/Phong95/aec-agent-go/components/agent/profile"
	"github.com/Phong95/aec-agent-go/kernel"
)

// clearedToolOutput replaces the content of an old tool result that is summarised.
// The summarizer then sees a short marker and not the whole output.
const clearedToolOutput = "[Old tool output cleared to save context space]"

// Compactor shortens a conversation that grows toward the context window. It is
// built once per Loop from the profile's CompactorConfig, in New.
//
// When the run's token usage reaches the threshold it keeps a protected head (the
// first ProtectFirstN messages, normally the system prompt) and a tail of the most
// recent messages, and replaces everything between them by one summary message.
// The tail never starts with tool results, so a tool call is never separated from
// its result.
type Compactor struct {
	config profile.CompactorConfig

	// logger and agentID are used for the one warning the compactor logs. A nil
	// logger means slog.Default(); a nil agentID means an empty id.
	logger  *slog.Logger
	agentID func() string
}

// NewCompactor returns a Compactor for config.
func NewCompactor(config profile.CompactorConfig) *Compactor {
	return &Compactor{config: config}
}

// sessionCompactorConfig returns the compactor config a session runs with: config,
// with the session's context window override (Session.ContextWindowTokens) when it
// has one. ok is false when the session does not change config.
func sessionCompactorConfig(config profile.CompactorConfig, session kernel.Session) (profile.CompactorConfig, bool) {
	window := session.ContextWindowTokens
	if window <= 0 || window == config.ContextWindowTokens {
		return config, false
	}
	config.ContextWindowTokens = window
	return config, true
}

// forSession returns the compactor of a run in session: c itself, or a copy with the
// session's context window override (sharing c's logger and agent id).
func (c *Compactor) forSession(session kernel.Session) *Compactor {
	config, ok := sessionCompactorConfig(c.config, session)
	if !ok {
		return c
	}
	return &Compactor{config: config, logger: c.logger, agentID: c.agentID}
}

// CompactIfNeeded compacts msgs when the run's token usage reached the threshold
// of the context window. Below the threshold, that is when
// usage.InputTokens + usage.OutputTokens < ContextWindowTokens * Threshold, it
// returns the messages unchanged with an empty summary source and zero tail
// tokens. usage is the context window size the latest model response reported
// (the agent loop passes the session's current token usage).
//
// Past the threshold the messages are split into the protected head, a middle
// and the tail (see splitMiddleAndTail). With an empty middle nothing is
// compacted and msgs is returned as it is. Otherwise the middle is pruned (the
// content of large old tool results is cleared, on copies), summarised through
// summarize, and the result is the head, one assistant message holding the
// summary, and the tail, with SummaryIndex the length of the head.
//
// A config whose ContextWindowTokens is not positive or whose Threshold is not
// positive (the zero CompactorConfig in particular) disables compaction: nothing is
// ever compacted, although any usage would reach a zero threshold;
// profile.DefaultCompactorConfig is the config an agent normally wants.
//
// A failed summary skips the compaction so the agent loop continues: the error is
// logged as a warning with the agent id and the error text only, never with
// message content, and msgs is returned unchanged.
//
// count counts the tokens of one message; nil means a fresh Counter. A nil
// summarize skips the compaction like a failed one.
func (c *Compactor) CompactIfNeeded(ctx context.Context, msgs []kernel.Message, usage kernel.TokenUsage, count CountFunc, summarize SummarizeFunc) CompactorResult {
	notCompacted := CompactorResult{Compacted: false, Messages: msgs}
	if !c.enabled() {
		return notCompacted
	}
	if float64(usage.InputTokens+usage.OutputTokens) < float64(c.config.ContextWindowTokens)*c.config.Threshold {
		return notCompacted
	}
	return c.compact(ctx, msgs, count, summarize, "")
}

// CompactNow (A-136) compacts msgs now, whatever the context size (/compact). The same
// split as CompactIfNeeded - protected head, summary, recent tail - with the user's focus
// (instructions, "" for none) added to the summary request. Not compacted when
// everything already fits in the head and tail, or when summarization fails (logged as
// CompactIfNeeded does). Unlike CompactIfNeeded it does not check that the config enables
// compaction: the user asked for it.
func (c *Compactor) CompactNow(ctx context.Context, msgs []kernel.Message, count CountFunc, summarize SummarizeFunc, instructions string) CompactorResult {
	return c.compact(ctx, msgs, count, summarize, instructions)
}

// compact is the compaction of CompactIfNeeded and CompactNow, past the threshold check.
func (c *Compactor) compact(ctx context.Context, msgs []kernel.Message, count CountFunc, summarize SummarizeFunc, instructions string) CompactorResult {
	notCompacted := CompactorResult{Compacted: false, Messages: msgs}
	if count == nil {
		count = NewCounter().Count
	}

	head := msgs[:min(max(c.config.ProtectFirstN, 0), len(msgs))]
	remaining := msgs[len(head):]
	middle, tail := c.splitMiddleAndTail(remaining, count)
	// Nothing left to summarise once the tail was widened to keep tool pairs.
	if len(middle) == 0 {
		return notCompacted
	}
	pruned := c.pruneOldToolResults(middle, count)
	contents := make([]string, len(pruned))
	for i, m := range pruned {
		contents[i] = m.Content
	}
	summarySource := strings.Join(contents, "\n")

	if summarize == nil {
		return notCompacted
	}
	summary, err := summarize(ctx, pruned, c.config.SummarizeMaxOutputTokens, c.config.SummarizationModel, instructions)
	if err != nil {
		// Summarization failed (model unavailable, timeout, an empty summary): fall back
		// to the uncompacted messages so the agent loop continues. A stop of the run is
		// not a failure to warn about: the loop is about to end the run.
		if ctx.Err() == nil {
			c.warn(err)
		}
		return notCompacted
	}

	out := make([]kernel.Message, 0, len(head)+1+len(tail))
	out = append(out, head...)
	// A-104: Summary marks it as the compaction summary (context breakdown).
	out = append(out, kernel.Message{Role: kernel.RoleAssistant, Content: summary, Summary: true})
	out = append(out, tail...)
	tailTokens := 0
	for _, m := range tail {
		tailTokens += count(m)
	}
	return CompactorResult{
		Compacted:     true,
		Messages:      out,
		SummarySource: summarySource,
		TailTokens:    tailTokens,
		// The summary message is inserted right after the protected head.
		SummaryIndex: len(head),
	}
}

// enabled reports whether the config asks for compaction at all: a context window
// and a threshold that are both positive. A zero config is "compaction disabled",
// never "compact on every call".
func (c *Compactor) enabled() bool {
	return c.config.ContextWindowTokens > 0 && c.config.Threshold > 0
}

// warn logs a failed summarization. The log carries the agent id and the error
// text only: the messages being compacted are the conversation and are never logged.
func (c *Compactor) warn(err error) {
	logger := c.logger
	if logger == nil {
		logger = slog.Default()
	}
	id := ""
	if c.agentID != nil {
		id = c.agentID()
	}
	logger.Warn("agentloop: summarization failed, skipping compaction", "agentId", id, "error", err.Error())
}

// splitMiddleAndTail divides the messages after the protected head into the
// middle, which is summarised, and the tail, which is kept. The tail is built
// backwards from the newest message: a message joins it while the tail holds fewer
// than ProtectLastN messages, or while the message still fits the tail token
// budget.
//
// Then the tail is widened: a tail that opens with tool results would reach the
// model without the calls they answer (the calls are summarised away), which
// providers reject, so earlier messages are pulled in until the tail starts clean,
// even when that goes over budget. The returned slices alias msgs.
func (c *Compactor) splitMiddleAndTail(msgs []kernel.Message, count CountFunc) (middle, tail []kernel.Message) {
	budget := c.tailBudget()
	start := len(msgs) // msgs[start:] is the tail
	tailTokens := 0

	for index := len(msgs) - 1; index >= 0; index-- {
		m := msgs[index]
		n := count(m)
		mustKeep := len(msgs)-start < c.config.ProtectLastN
		fitsBudget := float64(tailTokens+n) <= budget
		if !mustKeep && !fitsBudget {
			break
		}
		start = index
		tailTokens += n
	}

	for start > 0 && start < len(msgs) && carriesToolResults(msgs[start]) {
		start--
	}
	return msgs[:start], msgs[start:]
}

// tailBudget is the token budget of the tail: LeanTailTokenBudget when it is set
// and not zero (so a negative budget is used as it is), else
// the context window times the threshold times the target ratio.
func (c *Compactor) tailBudget() float64 {
	if b := c.config.LeanTailTokenBudget; b != nil && *b != 0 {
		return float64(*b)
	}
	return float64(c.config.ContextWindowTokens) * c.config.Threshold * c.config.TailTargetRatio
}

// proactivePruneTargetRatio (A-129): the proactive clearing clears down to this share
// of ProactivePruneTokens (the low mark).
const proactivePruneTargetRatio = 0.5

// defaultKeepToolResults (A-129) is the default of CompactorConfig.KeepToolResults.
const defaultKeepToolResults = 3

// ToolResultClearing (A-129) is what ClearOldToolResults did.
type ToolResultClearing struct {
	// Messages are the messages with the cleared results; the input itself when
	// nothing was cleared.
	Messages []kernel.Message
	// Cleared is how many tool results were cleared.
	Cleared int
	// ClearedTokens is their estimated size before clearing.
	ClearedTokens int
}

// clearedToolResultNote (A-129) is what a proactively cleared tool result reads.
func clearedToolResultNote(toolName string, tokens int) string {
	return "[Old " + toolName + " output cleared to save context space (~" + strconv.Itoa(tokens) +
		" tokens) — run it again if you need it]"
}

// ClearOldToolResults (A-129) clears old large tool results before compaction is
// needed. The current turn (from the last user message on) and the latest
// KeepToolResults results before it are never cleared; of the rest, the results of
// at least OldToolResultMinTokens are clearable. Once those add up to more than
// ProactivePruneTokens (the high mark), the oldest are replaced by a short note until
// they are down to half of it (the low mark). Clearing in batches like this changes
// the messages rarely, so the prompt cache mostly holds; the caller stores the
// result. Off when ProactivePruneTokens is not positive. msgs is never changed.
func (c *Compactor) ClearOldToolResults(msgs []kernel.Message, count CountFunc) ToolResultClearing {
	unchanged := ToolResultClearing{Messages: msgs}
	high := c.config.ProactivePruneTokens
	if high <= 0 {
		return unchanged
	}

	turnStart := len(msgs)
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == kernel.RoleUser && !carriesToolResults(msgs[i]) {
			turnStart = i
			break
		}
	}

	names := map[string]string{}
	for _, m := range msgs {
		for _, call := range m.ToolCalls {
			names[call.ID] = call.Name
		}
	}

	// Every result before the current turn, oldest first: the message index and the
	// result index, or -1 for a legacy tool message.
	type found struct{ at, index, tokens int }
	var results []found
	for at := range turnStart {
		m := msgs[at]
		switch {
		case len(m.ToolResults) > 0:
			for index, r := range m.ToolResults {
				one := kernel.Message{Role: m.Role, ToolResults: []kernel.ToolResult{r}}
				results = append(results, found{at, index, count(one)})
			}
		case m.Role == kernel.RoleTool:
			results = append(results, found{at, -1, count(m)})
		}
	}
	keep := defaultKeepToolResults
	// A negative value falls back to the default (it would otherwise slice past the end).
	if c.config.KeepToolResults != nil && *c.config.KeepToolResults >= 0 {
		keep = *c.config.KeepToolResults
	}
	minimum := c.config.OldToolResultMinTokens
	// A cleared result is a short note, far below the minimum, so it is never cleared again.
	var clearable []found
	for _, r := range results[:max(0, len(results)-keep)] {
		if r.tokens >= minimum {
			clearable = append(clearable, r)
		}
	}

	total := 0
	for _, r := range clearable {
		total += r.tokens
	}
	if total <= high {
		return unchanged
	}

	low := float64(high) * proactivePruneTargetRatio
	out := make([]kernel.Message, len(msgs))
	copy(out, msgs)
	cleared, clearedTokens := 0, 0
	for _, r := range clearable {
		if float64(total) <= low {
			break
		}
		m := out[r.at].Clone()
		if r.index == -1 {
			m.Content = clearedToolResultNote("tool", r.tokens)
		} else {
			name, ok := names[m.ToolResults[r.index].ToolCallID]
			if !ok {
				name = "tool"
			}
			m.ToolResults[r.index].Content = clearedToolResultNote(name, r.tokens)
		}
		out[r.at] = m
		total -= r.tokens
		cleared++
		clearedTokens += r.tokens
	}
	return ToolResultClearing{Messages: out, Cleared: cleared, ClearedTokens: clearedTokens}
}

// pruneOldToolResults clears the content of the messages that carry large tool
// results, on copies: the conversation history is never changed, only what the
// summarizer is shown. The agent loop records results on ToolResults, whose Content
// is cleared per result; a legacy tool message keeps its result in Content, which
// is cleared. A message below OldToolResultMinTokens, or without results, is kept
// as it is.
func (c *Compactor) pruneOldToolResults(msgs []kernel.Message, count CountFunc) []kernel.Message {
	minimum := c.config.OldToolResultMinTokens
	out := make([]kernel.Message, len(msgs))
	for i, m := range msgs {
		if !carriesToolResults(m) || count(m) < minimum {
			out[i] = m
			continue
		}
		cleared := m.Clone()
		if len(cleared.ToolResults) > 0 {
			for j := range cleared.ToolResults {
				cleared.ToolResults[j].Content = clearedToolOutput
			}
		} else {
			cleared.Content = clearedToolOutput
		}
		out[i] = cleared
	}
	return out
}
