package agentloop

import (
	"bytes"
	"context"
	"errors"
	"slices"

	"github.com/Phong95/aec-agent-go/components/events"
	"github.com/Phong95/aec-agent-go/components/llmhub"
	"github.com/Phong95/aec-agent-go/components/toolhub"
	"github.com/Phong95/aec-agent-go/components/trace"
	"github.com/Phong95/aec-agent-go/kernel"
	"github.com/Phong95/aec-agent-go/kernel/ordered"
)

// modelInput is the exact model input of one iteration: the same messages and
// options are sent to the model and described in the trace.
type modelInput struct {
	messages []kernel.Message
	toolDefs []toolhub.Definition
	options  llmhub.ModelOptions
	call     callTrace
	// sections (A-104) are the labelled parts of the system message; nil when the
	// Context gives none (no SectionedContext).
	sections []ContextSection
}

// callTrace is what the model-call span records about the call beyond the counts:
// the tool names, the provider that would serve it and the effective options (the
// slice of describeLlmInput the model-call span reads). It is filled only when
// the run is traced, from the same description the context-assembly span ends with,
// so the two spans always agree.
type callTrace struct {
	toolNames []string
	provider  string
	hasProv   bool
	model     string
	options   ordered.Object
}

// assembleModelInput is OBSERVE: it prepares the context through the middleware
// pipeline, compacts it if needed and picks the tools, which gives the exact
// input of this iteration's model call.
//
// The context-assembly span ends with the full description of the model input
// (describeLlmInput) when the run is traced, and with no attributes otherwise.
//
// The middleware pipeline is raced against ctx like the model call, so a middleware
// that ignores ctx cannot delay a stop; errRunStopped is returned then, which the
// caller turns into the aborted result. A panic of a middleware reaches the caller as
// an error, as a panic of the model call does (the work runs in its own goroutine).
func (l *Loop) assembleModelInput(ctx context.Context, r *runState, overrides llmhub.ModelOptions) (modelInput, error) {
	ctxSpan := r.trace.span("context-assembly", trace.KindContextAssembly, ordered.Object{})

	// The LLM working set (compacted if available) instead of the full history. Its
	// inputs are read before the race starts, so the abandoned call of a stopped run
	// shares nothing with the cleanup. A working set stored with its system message
	// in front (by an older version) is read without it, since the
	// context port adds its own.
	history := withoutLeadingSystem(r.conv.LLMMessages())
	// A-129: clear old large tool results once they pass ProactivePruneTokens, below the
	// compaction threshold too. Stored, so later requests read the same messages.
	clearing := r.compactor.ClearOldToolResults(history, r.counter.Count)
	if clearing.Cleared > 0 {
		if err := r.persist(ctx, func(pctx context.Context) error {
			return r.conv.SetCompactedMessages(pctx, clearing.Messages)
		}); err != nil {
			return modelInput{}, err
		}
		history = clearing.Messages
	}
	// A-129: calls whose content was written to a file carry a reference to it instead.
	history = withCompactArguments(history)
	mc := l.middlewareContext(r)
	// A-104: with the system message's labelled sections when the Context gives them.
	var sections []ContextSection
	prepared, aborted, err := raceAbort(ctx, func() ([]kernel.Message, error) {
		p, s, err := l.prepareContext(ctx, history, mc)
		sections = s
		return p, err
	})
	if aborted || (err != nil && ctx.Err() != nil) {
		return modelInput{}, errRunStopped
	}
	if err != nil {
		return modelInput{}, err
	}

	// Compaction: check whether the context needs shrinking.
	// The compactor is fed the context window size the latest response reported
	// (contextUsage), not the run total.
	usage := r.contextUsage()
	compactSpan := r.trace.span("compaction", trace.KindCompaction, obj(
		"messageCountBefore", len(prepared),
		"inputTokens", usage.InputTokens,
		"outputTokens", usage.OutputTokens,
	))
	result := r.compactor.CompactIfNeeded(ctx, prepared, usage, r.counter.Count, l.summarizer.traced(r.trace))

	// Persist the compacted messages so later turns do not compact again. The system
	// message the context port put in front is not stored: the port prepends a fresh
	// one on every call, so a stored copy would be a second system message from the
	// next turn on.
	if result.Compacted {
		stored := withoutLeadingSystem(result.Messages)
		if err := r.persist(ctx, func(pctx context.Context) error {
			return r.conv.SetCompactedMessages(pctx, stored)
		}); err != nil {
			return modelInput{}, err
		}
	}
	compactAttrs := obj(
		"triggered", result.Compacted,
		"messageCountAfter", len(result.Messages),
		"summarySourceLength", kernel.UTF16Len(result.SummarySource),
		"tailTokens", result.TailTokens,
	)
	// A-129: only when old tool results were cleared this iteration
	if clearing.Cleared > 0 {
		compactAttrs.Set("toolResultsCleared", clearing.Cleared)
		compactAttrs.Set("toolResultTokensCleared", clearing.ClearedTokens)
	}
	compactSpan.End(compactAttrs)

	// The model input is built once: the same slice is sent to the model and
	// described in the trace. The recalled memories join the last system message,
	// and repairToolPairs drops tool calls and results that lost their partner (a
	// history compacted by an older version, a run that stopped between a call and
	// its result), so a provider does not reject the request. It returns the same
	// slice when there is nothing to repair.
	messages := repairToolPairs(withRecalledMemories(result.Messages, r.recalled))

	// The agent's tools, plus skill_view when it has skills.
	// A-105: a subagent's session sees only the tools its scope allows.
	toolDefs, err := l.toolDefinitions(ctx, r.session)
	if err != nil {
		return modelInput{}, err
	}

	// The overrides are laid over the tool list: an explicit Tools slice in the
	// overrides wins, even an empty one.
	options := overrides
	if options.Tools == nil {
		options.Tools = toolDefs
	}

	in := modelInput{messages: messages, toolDefs: toolDefs, options: options, sections: sections}
	// Computed once per iteration, only when the run is traced: tracing must never
	// run (or fail) when there is no trace context to attach it to.
	if r.trace.enabled() {
		d := l.describeLlmInput(llmInputParams{
			iteration:   r.iteration,
			prepared:    prepared,
			compact:     result,
			llmMessages: messages,
			recalled:    r.recalled,
			options:     options,
			counter:     r.counter,
		})
		in.call = d.call
		ctxSpan.End(d.attrs)
	} else {
		ctxSpan.End(ordered.Object{})
	}
	return in, nil
}

// withoutLeadingSystem returns msgs without the system messages at its front. It
// returns msgs itself when there is none, and a subslice otherwise (never a
// modified copy of a message).
func withoutLeadingSystem(msgs []kernel.Message) []kernel.Message {
	i := 0
	for i < len(msgs) && msgs[i].Role == kernel.RoleSystem {
		i++
	}
	return msgs[i:]
}

// errRunStopped is what a step that was cut short by a stop of the run returns to
// its caller, which answers it with the aborted result (never an error to the
// consumer): Run returns RunResult{Aborted: true}, Stream yields the aborted done
// chunk.
var errRunStopped = errors.New("agentloop: the run was stopped")

// middlewareContext is the context a middleware sees: a snapshot of the session
// and of the usage, so a middleware cannot change the run's own copies.
func (l *Loop) middlewareContext(r *runState) MiddlewareContext {
	return MiddlewareContext{
		Session:     r.session.Clone(),
		TokenBudget: TokenBudget,
		Usage:       cloneUsage(r.totalUsage),
	}
}

// optionsObject renders model options as an attribute object without the tools
// (the effective options after the manager's defaults merge). Keys follow
// the llmhub.ModelOptions field order and an unset option is left out, as an
// undefined value is omitted in JSON. An empty, non-nil stop sequence list is
// kept.
func optionsObject(o llmhub.ModelOptions) ordered.Object {
	out := ordered.NewObject()
	if o.Provider != "" {
		out.Set("provider", o.Provider)
	}
	if o.Model != "" {
		out.Set("model", o.Model)
	}
	if o.MaxTokens != nil {
		out.Set("maxTokens", *o.MaxTokens)
	}
	if o.Temperature != nil {
		out.Set("temperature", *o.Temperature)
	}
	if o.TopP != nil {
		out.Set("topP", *o.TopP)
	}
	if o.StopSequences != nil {
		out.Set("stopSequences", slices.Clone(o.StopSequences))
	}
	if o.SystemPrompt != "" {
		out.Set("systemPrompt", o.SystemPrompt)
	}
	return out
}

// startModelCall emits ModelRequested and opens the model-call span.
func (l *Loop) startModelCall(r *runState, iteration int, in modelInput) {
	emit(r, events.ModelRequested, events.ModelRequestedPayload{
		AgentID:   r.agentID,
		Iteration: iteration,
		ToolCount: len(in.toolDefs),
	})

	toolNames := in.call.toolNames
	if toolNames == nil {
		toolNames = make([]string, len(in.toolDefs))
		for i, d := range in.toolDefs {
			toolNames[i] = d.Name
		}
	}
	attrs := obj(
		"iteration", iteration,
		"toolCount", len(in.toolDefs),
		"toolNames", toolNames,
		"inputMessageCount", len(in.messages),
	)
	if in.call.hasProv {
		attrs.Set("provider", in.call.provider)
	}
	if in.call.model != "" {
		attrs.Set("model", in.call.model)
	}
	if r.trace.enabled() {
		attrs.Set("options", in.call.options)
	}
	r.modelSpan = r.trace.span("model-call", trace.KindModelCall, attrs)
}

// toolCallsAttr renders a response's tool calls for the span: name and arguments
// per call.
func toolCallsAttr(calls []kernel.ToolCall) []any {
	out := make([]any, len(calls))
	for i, c := range calls {
		out[i] = obj("name", c.Name, "arguments", kernel.Arguments(bytes.Clone(c.Arguments)))
	}
	return out
}
