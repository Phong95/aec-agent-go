package agentloop

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/Phong95/aec-agent-go/components/agent/memorysystem/memory"
	"github.com/Phong95/aec-agent-go/components/events"
	"github.com/Phong95/aec-agent-go/components/toolhub"
	"github.com/Phong95/aec-agent-go/components/trace"
	"github.com/Phong95/aec-agent-go/kernel"
	"github.com/Phong95/aec-agent-go/kernel/ordered"
)

// runState is everything one Run call carries from step to step.
// It owns the data the end of a run needs: the session snapshot, the usage, the
// trace and its spans. A ctx is never a field; every method that touches a store
// takes it as a parameter (a stored ctx is a contextcheck finding and a contract
// hazard).
type runState struct {
	bus       events.Bus
	conv      Conversation
	counter   *Counter
	sessionID string
	agentID   string
	// runID (A-105) is this run's id - handed to tools as toolhub.Context.RunID (children
	// of a turn share its budget).
	runID string

	// session is the run's snapshot of the active session. recordUsage updates
	// its usage fields and the loop hands it back through Conversation.UpdateSession.
	session kernel.Session

	// recalled is what the memory recall of this run found, nil when there is no
	// memory system or the recall failed.
	recalled *memory.RecalledMemories

	// totalUsage accumulates every model call of the run, and only those: it is not
	// seeded from the session's current usage.
	totalUsage kernel.TokenUsage
	// stopReason is the stop reason of the latest model response, empty before the
	// first one.
	stopReason    kernel.StopReason
	maxIterations int
	// compactor is the compactor of this run: the loop's, or one with the session's
	// context window override (Session.ContextWindowTokens).
	compactor *Compactor

	trace   *runTrace
	runSpan trace.SpanHandle

	persistTimeout time.Duration
	logger         *slog.Logger

	// iteration is the number of iterations started so far. An aborted run reports
	// it, so it lives here and not in a loop variable the cleanup cannot see.
	iteration int

	// iterSpan and modelSpan are the iteration span and the model-call span while
	// they are open, nil otherwise. The abort cleanup ends whichever is open.
	iterSpan  trace.SpanHandle
	modelSpan trace.SpanHandle

	// partial is the text of the streamed response the consumer has received so far.
	// It is appended to BEFORE a chunk is yielded, so what the consumer saw is what an
	// early exit persists. It lives in the run state, never in a value a helper
	// returns, because the helper's result is not available when the consumer stops
	// iterating (RESEARCH Pitfall 2).
	partial strings.Builder

	// usageDirty is true while the session snapshot holds usage that was not handed
	// back through Conversation.UpdateSession yet.
	usageDirty bool

	// done is set by every terminal path (a finished turn, the iteration cap, a
	// failure, an abort), which makes the abort cleanup idempotent: the deferred
	// call that guards a consumer that stops early finds it set and does nothing.
	done bool

	// truncatedResponses is the number of consecutive model responses cut off at
	// the output token limit while writing tool calls (A-88): a max_tokens response
	// with tool calls adds one, any other response that runs tools resets it. At
	// maxConsecutiveTruncations the run stops (actOnToolCalls).
	truncatedResponses int
}

// maxConsecutiveTruncations is how many model responses in a row may be cut off at
// the output token limit while writing tool call arguments before the run stops
// (A-88): the model keeps retrying the same oversized call otherwise.
const maxConsecutiveTruncations = 2

// persist runs one conversation or session write under a context that the
// caller's cancellation cannot reach: context.WithoutCancel keeps the values of
// ctx and drops its cancellation, and the timeout bounds a store that hangs. A
// cancel can therefore never lose the user message or the partial reply, and a
// stuck store cannot stall a run forever.
//
// Only the writes use it. Model calls, tool execution and approval waits use the
// caller's ctx, which is what makes them stoppable.
func (r *runState) persist(ctx context.Context, write func(context.Context) error) error {
	pctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), r.persistTimeout)
	defer cancel()
	return write(pctx)
}

// addMessage persists one message.
func (r *runState) addMessage(ctx context.Context, m kernel.Message) error {
	return r.persist(ctx, func(pctx context.Context) error { return r.conv.AddMessage(pctx, m) })
}

// updateSession hands the run's session snapshot back to the conversation. It does
// nothing when no usage was recorded since the last call, so an iteration without
// usage (a streamed response whose done chunk carried none) writes no session.
func (r *runState) updateSession(ctx context.Context) error {
	if !r.usageDirty {
		return nil
	}
	snapshot := r.session.Clone()
	err := r.persist(ctx, func(pctx context.Context) error { return r.conv.UpdateSession(pctx, snapshot) })
	if err == nil {
		r.usageDirty = false
	}
	return err
}

// emit sends one event of the run. The payload carries the agent id first; each
// payload type here starts with it.
func emit[P any](r *runState, k events.Kind[P], p P) {
	events.Emit(r.bus, k, r.sessionID, p)
}

// startIteration emits the Iteration event and opens the iteration span.
func (r *runState) startIteration(iteration int) {
	r.iteration = iteration
	emit(r, events.Iteration, events.IterationPayload{
		AgentID:       r.agentID,
		Iteration:     iteration,
		MaxIterations: r.maxIterations,
	})
	r.iterSpan = r.trace.span(iterationSpanName(iteration), trace.KindIteration, obj(
		"iteration", iteration,
		"maxIterations", r.maxIterations,
	))
}

// endIteration ends the iteration span with no attributes: the iteration finished
// and the loop goes round again.
func (r *runState) endIteration() {
	if r.iterSpan != nil {
		r.iterSpan.End(ordered.Object{})
		r.iterSpan = nil
	}
}

// endModelSpan ends the model-call span with attrs, when it is open.
func (r *runState) endModelSpan(attrs ordered.Object) {
	if r.modelSpan != nil {
		r.modelSpan.End(attrs)
		r.modelSpan = nil
	}
}

// recordUsage adds a model response's usage to the run total, calibrates the token
// counter against it and saves both on the session snapshot. sent
// is the exact input of the model call (the messages after compaction, memory
// injection and pair repair), not the pre-compaction history: calibrating against
// content the model never saw would skew the chars-per-token ratio and every later
// estimate.
//
// The snapshot's CurrentTokenUsage is the raw usage of the latest response: it IS
// the current context window size (inputTokens is the whole conversation sent,
// outputTokens what the model just added). Its CounterCalibration is the counter's
// state after this response, so the next run starts with the learned ratio. Both
// reach the stored session through Conversation.UpdateSession.
func (r *runState) recordUsage(sent []kernel.Message, tools []toolhub.Definition, usage kernel.TokenUsage) {
	accumulateUsage(&r.totalUsage, usage)
	// A-104 audit: the tool definitions were sent too and are in the input tokens.
	r.counter.Calibrate(sent, usage, tools...)
	u := cloneUsage(usage)
	r.session.CurrentTokenUsage = &u
	cal := r.counter.Calibration()
	r.session.CounterCalibration = &cal
	r.usageDirty = true
}

// completeRun closes the spans and emits RunCompleted: the model ended its turn.
// stop is the stop reason the turn ended with (A-88): max_tokens (the output token
// limit was hit) and error (the provider reported a stop that is not a normal
// finish, a refusal or a filter) are added to the run span as stopReason; any
// other reason adds nothing. RunCompleted is emitted unchanged either way.
func (r *runState) completeRun(iterations int, stop kernel.StopReason) {
	r.done = true
	r.endIteration()
	end := obj("iterations", iterations, "totalUsage", cloneUsage(r.totalUsage))
	if stop == kernel.StopMaxTokens || stop == kernel.StopError {
		end.Set("stopReason", string(stop))
	}
	r.runSpan.End(end)
	r.trace.close()
	emit(r, events.RunCompleted, events.RunCompletedPayload{
		AgentID:    r.agentID,
		Iterations: iterations,
		TotalUsage: cloneUsage(r.totalUsage),
	})
}

// abortRun ends a run that was stopped: the caller cancelled ctx, or the consumer of
// Stream stopped iterating. It returns the error of a failed write, after finishing the
// cleanup anyway.
//
// It persists the partial reply when there is one (never the tool calls collected
// from a response that did not finish: they were never run and would dangle without
// a result), then writes the usage the run recorded and had not handed back yet
// (with or without a partial reply: a stop after a usage record and before any
// message write must not lose the session's token usage and counter calibration,
// Phase 9 review IN-03), ends the model-call span, the iteration span and the run
// span marked aborted, removes the trace context and emits RunCompleted with
// Aborted. It never yields, so it is safe to call from a deferred function. It is
// idempotent: every terminal path sets done, and a second call does nothing.
func (r *runState) abortRun(ctx context.Context) error {
	if r.done {
		return nil
	}
	r.done = true

	r.endModelSpan(obj("aborted", true, "responsePreview", r.partial.String()))
	var err error
	if r.partial.Len() > 0 {
		err = r.addMessage(ctx, kernel.Message{Role: kernel.RoleAssistant, Content: r.partial.String()})
		r.partial.Reset()
	}
	// The recorded usage is written whether or not there was a partial reply to save
	// (updateSession does nothing when no usage is waiting). It follows the message
	// write, as everywhere else, so the session it saves carries the UpdatedAt that
	// write bumped. It is tried also when the partial reply could not be written: the
	// two writes are independent, and the usage must not be lost with the reply; the
	// first error is the one returned.
	if uerr := r.updateSession(ctx); err == nil {
		err = uerr
	}
	if r.iterSpan != nil {
		r.iterSpan.End(obj("aborted", true))
		r.iterSpan = nil
	}
	r.runSpan.End(obj("iterations", r.iteration, "totalUsage", cloneUsage(r.totalUsage), "aborted", true))
	// Whatever else is still open ends marked aborted too, so no span outlives the run.
	r.trace.endOpen(obj("aborted", true))
	r.trace.close()
	emit(r, events.RunCompleted, events.RunCompletedPayload{
		AgentID:    r.agentID,
		Iterations: r.iteration,
		TotalUsage: cloneUsage(r.totalUsage),
		Aborted:    true,
	})
	return err
}

// closeAborted is abortRun for a deferred call: no consumer is left to receive an
// error, so a failed write is logged. The log carries the agent id, the session id
// and the error text only, never message content.
func (r *runState) closeAborted(ctx context.Context) {
	if err := r.abortRun(ctx); err != nil {
		r.logger.Error("agentloop: persisting the partial reply of a stopped run failed",
			"agentId", r.agentID, "sessionId", r.sessionID, "error", err.Error())
	}
}

// closeMaxIterations closes the run span and emits MaxIterationsReached: the loop
// ran out of iterations. No RunCompleted follows.
func (r *runState) closeMaxIterations() {
	r.done = true
	r.runSpan.End(obj(
		"iterations", r.maxIterations,
		"totalUsage", cloneUsage(r.totalUsage),
		"maxIterationsReached", true,
	))
	r.trace.close()
	emit(r, events.MaxIterationsReached, events.MaxIterationsPayload{
		AgentID:    r.agentID,
		Iterations: r.maxIterations,
		TotalUsage: cloneUsage(r.totalUsage),
	})
}

// fail ends a run that stops with an error: every span still open ends with the
// error text and the trace context is removed. The caller returns the error unchanged.
func (r *runState) fail(err error) {
	if r.done {
		return // the run already ended: its spans are closed and the context removed
	}
	r.done = true
	r.trace.endOpen(obj("error", err.Error()))
	r.trace.close()
}

// cleanupOnPanic is deferred by Run (and beginRun): when the function panics it
// ends every span still open with the panic text, removes the trace context and
// panics again with the same value, so the panic is never swallowed and nothing is
// leaked. It does nothing when nothing panicked, and (through fail) when a terminal
// path already ended the run. No RunCompleted is emitted: like every failed run, a
// panicked run has no completion.
func (r *runState) cleanupOnPanic() {
	if p := recover(); p != nil {
		r.fail(fmt.Errorf("panic: %v", p))
		panic(p) //nolint:forbidigo // re-panics with the original value: a panic is never swallowed, only cleaned up after
	}
}

// contextUsage is the size of the context window as the latest model response
// reported it (input and output only): the session's CurrentTokenUsage, which a
// run starts from and recordUsage replaces after every call. It is the compactor's
// input, rather than the run total, which grows past the window size with every
// call of a run.
func (r *runState) contextUsage() kernel.TokenUsage {
	if u := r.session.CurrentTokenUsage; u != nil {
		return kernel.TokenUsage{InputTokens: u.InputTokens, OutputTokens: u.OutputTokens}
	}
	return kernel.TokenUsage{}
}

// runsTools is the one stop-reason rule Run and Stream share: a response runs its tools
// only when it stopped with tool_use and carries tool calls; every other response
// (end_turn, max_tokens, error, tool_use without a call) ends the turn.
//
// Since A-88 a max_tokens response that carries tool calls runs its tools too: the
// response was cut off while writing a call, and every call must get a result.
// actOnToolCalls answers the cut-off calls with an output-token-limit tool error
// instead of running them. A max_tokens response without a call (a text reply cut
// short) still ends the turn, as do end_turn, error and tool_use without a call.
func runsTools(stop kernel.StopReason, calls []kernel.ToolCall) bool {
	return (stop == kernel.StopToolUse || stop == kernel.StopMaxTokens) && len(calls) > 0
}

// accumulateUsage adds step to total. The cache counters are
// written as explicit sums with zero defaults, so a run total always carries them
// even when the provider never reports cache usage.
func accumulateUsage(total *kernel.TokenUsage, step kernel.TokenUsage) {
	total.InputTokens += step.InputTokens
	total.OutputTokens += step.OutputTokens
	read := intValue(total.CacheReadTokens) + intValue(step.CacheReadTokens)
	write := intValue(total.CacheWriteTokens) + intValue(step.CacheWriteTokens)
	total.CacheReadTokens = &read
	total.CacheWriteTokens = &write
}

// cloneUsage copies u, including its optional counters.
func cloneUsage(u kernel.TokenUsage) kernel.TokenUsage {
	if u.CacheReadTokens != nil {
		v := *u.CacheReadTokens
		u.CacheReadTokens = &v
	}
	if u.CacheWriteTokens != nil {
		v := *u.CacheWriteTokens
		u.CacheWriteTokens = &v
	}
	return u
}

// intValue reads an optional counter, zero when absent.
func intValue(p *int) int {
	if p == nil {
		return 0
	}
	return *p
}

func iterationSpanName(iteration int) string {
	return "iteration-" + strconv.Itoa(iteration)
}
