package agentloop

import (
	"context"
	"errors"
	"log/slog"
	"maps"
	"slices"
	"sync/atomic"
	"time"

	"github.com/Phong95/aec-agent-go/components/agent/profile"
	"github.com/Phong95/aec-agent-go/components/events"
	"github.com/Phong95/aec-agent-go/components/llmhub"
	"github.com/Phong95/aec-agent-go/components/trace"
	"github.com/Phong95/aec-agent-go/internal/nilcheck"
	"github.com/Phong95/aec-agent-go/kernel"
	"github.com/Phong95/aec-agent-go/kernel/ordered"
)

// defaultPersistTimeout bounds every conversation or session write of a run.
const defaultPersistTimeout = 10 * time.Second

// maxIterationsMessage is the content of the message Run returns when the loop
// runs out of iterations.
const maxIterationsMessage = "Reached maximum reasoning steps. Please refine your request."

// Loop is the agent loop: the OTAR cycle (Observe, Think, Act, Reflect) for one
// agent. It is built once per agent with New and is safe to use
// for several runs, but the runs of one conversation must not overlap: they share
// the conversation, and per-agent chat serialization stays the caller's job.
// -> Amended (A-97): runs of different conversations (RunOptions.Conversation) may
// overlap; the loop keeps no per-run state of its own (the token counter is per run),
// and keeping the runs of one session apart is the caller's job (the agent's
// per-session turn gate).
type Loop struct {
	deps           Deps
	compactor      *Compactor
	summarizer     summarizer
	persistTimeout time.Duration
	maxParallel    int
	logger         *slog.Logger
}

// Option changes how a Loop behaves. Options are applied in order.
type Option func(*Loop)

// WithPersistTimeout sets the timeout of every conversation and session write
// (default 10 s). A value of zero or less keeps the default.
func WithPersistTimeout(d time.Duration) Option {
	return func(l *Loop) {
		if d > 0 {
			l.persistTimeout = d
		}
	}
}

// WithMaxParallelTools bounds how many tool calls of one model response run at the
// same time (default 8). The bound protects against a model that asks for hundreds
// of calls. The results keep the
// order of the calls. A value below 1 keeps the default.
func WithMaxParallelTools(n int) Option {
	return func(l *Loop) {
		if n >= 1 {
			l.maxParallel = n
		}
	}
}

// WithLogger sets the logger for the loop's warnings (default slog.Default()). A
// nil logger keeps the default.
func WithLogger(lg *slog.Logger) Option {
	return func(l *Loop) {
		if lg != nil {
			l.logger = lg
		}
	}
}

// New builds a Loop. It returns an error naming the first missing required
// dependency: Models, Tools, Context, Conversation, Events and Profile (a typed
// nil counts as missing). Skills and Trace are optional.
// -> Amended (A-97): Conversation is optional too; see Deps.Conversation.
//
// The Compactor is built once, from the profile's compactor config at this
// moment; the loop limits and the agent id are read again from the profile at the start of
// every run.
func New(deps Deps, opts ...Option) (*Loop, error) {
	required := []struct {
		name string
		nil  bool
	}{
		{"Models", nilcheck.IsNil(deps.Models)},
		{"Tools", nilcheck.IsNil(deps.Tools)},
		{"Context", nilcheck.IsNil(deps.Context)},
		{"Events", nilcheck.IsNil(deps.Events)},
		{"Profile", deps.Profile == nil},
	}
	for _, d := range required {
		if d.nil {
			return nil, errors.New("agentloop: Deps." + d.name + " is required")
		}
	}
	if nilcheck.IsNil(deps.Skills) {
		deps.Skills = nil // a typed nil would panic at the first call
	}
	if nilcheck.IsNil(deps.Memory) {
		deps.Memory = nil // likewise: a typed nil Memory would panic when recall runs
	}
	if nilcheck.IsNil(deps.Conversation) {
		deps.Conversation = nil // A-97: optional; a typed nil would panic at the first run
	}
	l := &Loop{
		deps:           deps,
		compactor:      NewCompactor(deps.Profile().Compactor),
		summarizer:     summarizer{models: deps.Models},
		persistTimeout: defaultPersistTimeout,
		maxParallel:    defaultMaxParallelTools,
		logger:         slog.Default(),
	}
	for _, opt := range opts {
		if opt != nil {
			opt(l)
		}
	}
	// The compactor warns through the loop logger, with the agent id of the live
	// profile (read when the warning is logged, like every other profile read).
	l.compactor.logger = l.logger
	l.compactor.agentID = func() string { return l.deps.Profile().ID }
	return l, nil
}

// Run runs one agent turn and returns when the model ends its turn.
// It persists the user message, then loops: assemble the model input, ask the
// model, record the reply, run the tools it asked for, and ask again, until the
// model ends its turn or the iteration cap is reached.
//
// Stop reasons follow one rule shared with Stream (runsTools): a tool_use response with
// tool calls runs the tools and the loop asks again; every other response ends the
// turn, so end_turn, max_tokens and error all end it. RunResult.StopReason is the stop
// reason of the last response. At the cap Run emits MaxIterationsReached (no
// RunCompleted) and returns a fixed fallback message with Iterations equal to the cap.
//
// Output token limit (A-88): a max_tokens response WITH tool calls is the exception to
// "max_tokens ends the turn". It was cut off while writing a call, so its tools run
// (actOnToolCalls): the cut-off calls are never executed and get an output-token-limit
// tool error, the others run normally, and the loop asks again. After
// maxConsecutiveTruncations such responses in a row the run completes (RunCompleted,
// stopReason max_tokens on the run span) and Run returns a fallback message that is not
// saved, with StopReason max_tokens; every call still has its saved result. A turn that
// ends with max_tokens or error also puts that stopReason on the run span.
//
// A stopped run is a result: when ctx is cancelled or its deadline passes (both are
// a stop; context.Cause(ctx) tells which), Run returns an empty assistant message,
// the usage so far, the iterations started and Aborted true, with a nil error. The
// user message and the work already done stay persisted, and every span is closed.
//
// What a stop does not wait for: the model call, the compaction summary call, the
// memory recall, the context middlewares, and the tool and approval waits are raced
// against ctx, so a port that ignores ctx does not delay the stop (it keeps running
// in its own goroutine and its late result is dropped). The calls that are not
// raced are ctx-cooperative by contract: the provider's Stream sequence is
// iterated on the caller's goroutine and ends at its next chunk, and the
// conversation and session writes deliberately ignore the cancel (they run under
// context.WithoutCancel, bounded by the persist timeout, so a stop never loses the
// user message or the partial reply).
//
// Errors are preconditions and failures, never an abort: ErrNoActiveSession when
// the conversation has no session, ErrSessionNotActive when sessionID is not the id
// of the active session, and the error of a failed port call, which for
// the model is the *kernel.ModelError unchanged. A failed run ends every open
// span with the error and removes its trace context. A panic of a port that is not
// raced (the conversation, the tools list, the skills, the option resolver) does
// the same cleanup and then panics again, so it is never swallowed. overrides are
// merged over the tool definitions the loop picks; ro is reserved for future
// per-run options.
// -> Amended (A-97): ro.Conversation picks the conversation of the run (nil uses
// Deps.Conversation).
func (l *Loop) Run(ctx context.Context, userMessage, sessionID string, overrides llmhub.ModelOptions, ro RunOptions) (RunResult, error) {
	// No other per-run options yet; cancellation comes through the ctx.
	r, err := l.beginRun(ctx, userMessage, sessionID, ro)
	if err != nil {
		return RunResult{}, err
	}
	// Mirrors the deferred cleanup of Stream: a panic must not leak the trace context
	// or leave spans open. It does nothing once a terminal path ran.
	defer r.cleanupOnPanic()
	res, err := l.run(ctx, r, overrides)
	if err != nil {
		r.fail(err)
		return RunResult{}, err
	}
	return res, nil
}

// run is the iteration loop of Run, after beginRun.
func (l *Loop) run(ctx context.Context, r *runState, overrides llmhub.ModelOptions) (RunResult, error) {
	iteration := 0
	for iteration < r.maxIterations {
		// Safe point A: before the next iteration starts. It covers a ctx that was
		// already done when Run was called and a stop during the tools of the previous
		// iteration (the loop comes back here before it counts the next iteration).
		if ctx.Err() != nil {
			return l.abortedRun(ctx, r)
		}

		iteration++
		r.startIteration(iteration)

		// OBSERVE: prepare the context through the middleware pipeline.
		in, err := l.assembleModelInput(ctx, r, overrides)
		if errors.Is(err, errRunStopped) {
			return l.abortedRun(ctx, r)
		}
		if err != nil {
			return RunResult{}, err
		}

		// Safe point B: after context assembly and compaction, before the model is
		// asked for anything.
		if ctx.Err() != nil {
			return l.abortedRun(ctx, r)
		}

		// THINK: send it to the model. The call is raced against ctx, so a model that
		// ignores it cannot delay a stop. A failure that arrives while ctx is done is
		// the stop too, whatever its text (a provider reports a cancel as its own error),
		// so the decision is ctx.Err(), not the error value; a failure while ctx is alive,
		// a provider timeout included, is a model failure.
		l.startModelCall(r, iteration, in)
		resp, aborted, err := raceAbort(ctx, func() (kernel.ModelResponse, error) {
			return l.deps.Models.Chat(ctx, in.messages, in.options)
		})
		if aborted || (err != nil && ctx.Err() != nil) {
			r.endModelSpan(obj("aborted", true))
			return l.abortedRun(ctx, r)
		}
		if err != nil {
			return RunResult{}, err
		}

		// Run the afterModel middlewares, raced against ctx like the model call. The
		// response and the middleware context are handed over by value, so the
		// abandoned call of a stopped run shares nothing with the cleanup.
		raw, mc := resp, l.middlewareContext(r)
		processed, stopped, err := raceAbort(ctx, func() (kernel.ModelResponse, error) {
			return l.deps.Context.ProcessResponse(ctx, raw, mc)
		})
		if stopped || (err != nil && ctx.Err() != nil) {
			return l.abortedRun(ctx, r)
		}
		if err != nil {
			return RunResult{}, err
		}
		resp = processed

		hasToolCalls := len(resp.Message.ToolCalls) > 0
		end := obj("stopReason", string(resp.StopReason))
		// A refusal or filter stop ends the span with status error, not ok.
		if resp.StopReason == kernel.StopError {
			end.Set("error", modelErrorStop)
		}
		end.Set("inputTokens", resp.Usage.InputTokens)
		end.Set("outputTokens", resp.Usage.OutputTokens)
		end.Set("hasToolCalls", hasToolCalls)
		end.Set("responsePreview", resp.Message.Content)
		if resp.Message.ToolCalls != nil {
			end.Set("toolCalls", toolCallsAttr(resp.Message.ToolCalls))
		}
		r.endModelSpan(end)

		r.recordUsage(in.messages, in.toolDefs, resp.Usage)
		r.stopReason = resp.StopReason
		usage := cloneUsage(resp.Usage)
		emit(r, events.ModelResponded, events.ModelRespondedPayload{
			AgentID:      r.agentID,
			StopReason:   string(resp.StopReason),
			Usage:        &usage,
			HasToolCalls: hasToolCalls,
		})
		l.emitContextUsage(r, iteration, in, resp.Message, &usage)

		// The response's usage is saved with the message it produced (none reported: no usage).
		if resp.Usage.InputTokens > 0 || resp.Usage.OutputTokens > 0 {
			resp.Message.Usage = usagePtr(resp.Usage)
		}
		if err := r.addMessage(ctx, resp.Message); err != nil {
			return RunResult{}, err
		}
		// The usage recorded above reaches the stored session right after the
		// message write (Conversation.UpdateSession contract).
		if err := r.updateSession(ctx); err != nil {
			return RunResult{}, err
		}

		// End turn: return the final answer. Every response that does not run tools
		// ends the turn (end_turn, max_tokens, error, tool_use without calls).
		// (A-88: max_tokens without calls only; a max_tokens response with calls runs them.)
		if !runsTools(resp.StopReason, resp.Message.ToolCalls) {
			r.completeRun(iteration, resp.StopReason)
			return RunResult{Message: resp.Message, Usage: cloneUsage(r.totalUsage), Iterations: iteration, StopReason: resp.StopReason}, nil
		}

		// ACT: execute the tool and skill calls and append the results as one message.
		_, limitReached, err := l.actOnToolCalls(ctx, r, in, resp.StopReason, resp.Message.ToolCalls)
		if err != nil {
			return RunResult{}, err
		}
		// A-88: cut off mid tool call too many times in a row: stop. Like the
		// max-iterations fallback, this message is returned but not saved. A stop
		// during the tools is a stop, not the guard.
		if limitReached {
			if ctx.Err() != nil {
				return l.abortedRun(ctx, r)
			}
			r.completeRun(iteration, kernel.StopMaxTokens)
			return RunResult{
				Message:    kernel.Message{Role: kernel.RoleAssistant, Content: truncationLimitMessage(l.resolvedMaxTokens(in))},
				Usage:      cloneUsage(r.totalUsage),
				Iterations: iteration,
				StopReason: kernel.StopMaxTokens,
			}, nil
		}

		// REFLECT: loop back; the model sees the tool results and decides.
		r.endIteration()
	}

	// A stop during the last iteration's tools is a stop, not the cap: the loop left
	// before safe point A could see it.
	if ctx.Err() != nil {
		return l.abortedRun(ctx, r)
	}

	// Safety valve: the iteration cap.
	r.closeMaxIterations()
	return RunResult{
		Message:    kernel.Message{Role: kernel.RoleAssistant, Content: maxIterationsMessage},
		Usage:      cloneUsage(r.totalUsage),
		Iterations: r.maxIterations,
		StopReason: r.stopReason,
	}, nil
}

// abortedRun ends a run that was stopped and returns what Run returns for it: an
// empty assistant message, the usage so far, the iterations started, Aborted true
// and no error. A stop is a result, never an error; the only error here is a write
// the cleanup could not make.
func (l *Loop) abortedRun(ctx context.Context, r *runState) (RunResult, error) {
	if err := r.abortRun(ctx); err != nil {
		return RunResult{}, err
	}
	return RunResult{
		Message:    kernel.Message{Role: kernel.RoleAssistant, Content: ""},
		Usage:      cloneUsage(r.totalUsage),
		Iterations: r.iteration,
		Aborted:    true,
		StopReason: r.stopReason,
	}, nil
}

// beginRun starts a run: it opens the trace, records the user's message and
// restores the token usage and the counter calibration from the session, and
// recalls memories.
//
// A-97: the run works in ro.Conversation (Deps.Conversation when nil), and its token
// counter is its own, seeded from that session's calibration, so runs of other
// sessions running at the same time never skew its estimates.
func (l *Loop) beginRun(ctx context.Context, userMessage, sessionID string, ro RunOptions) (*runState, error) {
	conv := ro.Conversation
	if nilcheck.IsNil(conv) {
		conv = l.deps.Conversation
	}
	if conv == nil {
		return nil, ErrNoActiveSession
	}
	session, ok := conv.Session()
	if !ok {
		return nil, ErrNoActiveSession
	}
	// The run belongs to the session it names: a mismatch fails with an error that
	// matches ErrSessionNotActive here, before the run trace opens and before any
	// message is written or event emitted (IN-05).
	if sessionID != session.ID {
		return nil, &sessionNotActiveError{given: sessionID, active: session.ID}
	}
	// A-165: images attached to the user's message, checked before anything is written.
	if len(ro.Attachments) > 0 {
		if err := kernel.CheckAttachments(ro.Attachments); err != nil {
			return nil, kernel.NewModelError(err.Error(), kernel.ModelCodeInvalidRequest, false)
		}
	}

	// The profile is read once per run: a profile updated between two runs
	// applies to the next one.
	prof := l.deps.Profile()
	r := &runState{
		bus:            l.deps.Events,
		conv:           conv,
		counter:        NewCounter(),
		sessionID:      sessionID,
		agentID:        prof.ID,
		runID:          kernel.NewID(),
		session:        session.Clone(),
		persistTimeout: l.persistTimeout,
		logger:         l.logger,
		trace:          newRunTrace(l.deps.Trace),
		// Resolved once per run: a runtime profile change takes effect on the
		// next turn.
		maxIterations: profile.ResolveLoopConfig(prof.LoopConfig).MaxIterations,
		// A session may plan around a larger (or smaller) context window than the agent's.
		compactor: l.compactor.forSession(session),
	}
	// A-105: a child run adds its parent turn (RunOptions.TraceAttributes); agentId and
	// sessionId always name this run's own agent and session.
	r.runSpan = r.trace.span("agent-run", trace.KindRun, runSpanAttrs(ro.TraceAttributes, prof.ID, sessionID))
	// A panic of a port while the run starts (the conversation, the counter seed) must
	// not leak the trace context either; Run and Stream guard the rest of the run.
	defer r.cleanupOnPanic()

	if err := r.addMessage(ctx, kernel.Message{Role: kernel.RoleUser, Content: userMessage, Attachments: slices.Clone(ro.Attachments)}); err != nil {
		r.fail(err)
		return nil, err
	}
	emit(r, events.UserMessage, events.UserMessagePayload{
		AgentID: prof.ID,
		Role:    string(kernel.RoleUser),
		Content: userMessage,
	})

	// The total counts this run's model calls only. It is not seeded from the
	// session's usage, so the run's usage, its done chunk and RunCompleted do not
	// include the context window the previous turn ended with. The compactor reads
	// the context window size from the session itself (contextUsage).

	// Restore the learned chars-per-token ratio of the previous turns.
	if cal := r.session.CounterCalibration; cal != nil {
		r.counter.Seed(cal.CharsPerToken, cal.Samples)
	}

	// Memory recall: once per run, keyed on the user's message. It never fails the turn.
	r.recalled = l.recallMemories(ctx, r, userMessage)
	return r, nil
}

// runSpanAttrs is the start attributes of the "agent-run" span: the run's extra trace
// attributes in sorted key order, then agentId and sessionId (which replace an extra
// attribute of the same name, in place).
func runSpanAttrs(extra map[string]any, agentID, sessionID string) ordered.Object {
	if len(extra) == 0 {
		return obj("agentId", agentID, "sessionId", sessionID)
	}
	o := ordered.NewObject()
	for _, k := range slices.Sorted(maps.Keys(extra)) {
		o.Set(k, extra[k])
	}
	o.Set("agentId", agentID)
	o.Set("sessionId", sessionID)
	return o
}

// actOnToolCalls is ACT, the single entry point of Run and Stream (A-88): it runs the
// calls of a response that runsTools let through and keeps the consecutive-truncation
// count. When the response stopped at max_tokens, a call counts as cut off if it has an
// ArgumentsError or it is the LAST call of the response (the one the model was writing
// when the limit hit; a non-streaming provider hands over pre-parsed inputs, so it
// carries no ArgumentsError). Cut-off calls are never run: each gets an
// output-token-limit tool error that names maxTokens when one is configured. Earlier
// calls run normally. Every call gets a saved result, so the conversation never has a
// tool call without one, also when the guard trips.
//
// limitReached is true when this response made maxConsecutiveTruncations max_tokens
// responses with tool calls in a row; any other response that runs tools resets the
// count.
func (l *Loop) actOnToolCalls(ctx context.Context, r *runState, in modelInput, stop kernel.StopReason, calls []kernel.ToolCall) (results []kernel.ToolResult, limitReached bool, err error) {
	var trunc *outputTruncation
	if stop == kernel.StopMaxTokens {
		trunc = newOutputTruncation(calls, l.resolvedMaxTokens(in))
		r.truncatedResponses++
	} else {
		r.truncatedResponses = 0
	}
	results, err = l.executeToolCalls(ctx, r, calls, trunc)
	if err != nil {
		return nil, false, err
	}
	return results, r.truncatedResponses >= maxConsecutiveTruncations, nil
}

// resolvedMaxTokens is the output token limit of the model call in was made with:
// the options after the Models port's own defaults merge (OptionResolver) when it
// has one, else the options as they were sent. Nil means none is configured and the
// provider's default applied (A-88).
func (l *Loop) resolvedMaxTokens(in modelInput) *int {
	opts := in.options
	if res, ok := l.deps.Models.(OptionResolver); ok {
		opts = res.ResolveOptions(opts)
	}
	return opts.MaxTokens
}

// executeToolCalls is ACT: it runs the model's tool and skill calls and persists all
// the results as ONE message, then returns them in call order.
// The calls run under the caller's ctx, which is what makes them stoppable; the write
// does not (persist).
// trunc is set when the response stopped at the output token limit (A-88): its
// cut-off calls are answered with an output-token-limit tool error instead of run.
// ExecuteToolCall runs one tool call outside a turn - a tool job's stored call (A-103).
// It goes through the same steps as a call the model makes (tool policy, approval gate,
// retries, sandbox and dispatch tracing, tool events), under its own "tool-job" run
// span. Nothing is written to the session here: the caller (the job manager) records
// the call and its result. empty reports that the tool succeeded and printed nothing
// (toolhub.Output.Empty).
func (l *Loop) ExecuteToolCall(ctx context.Context, call kernel.ToolCall, sessionID string) (result kernel.ToolResult, empty bool) {
	prof := l.deps.Profile()
	r := &runState{
		bus:            l.deps.Events,
		sessionID:      sessionID,
		agentID:        prof.ID,
		runID:          kernel.NewID(),
		session:        kernel.Session{ID: sessionID},
		persistTimeout: l.persistTimeout,
		logger:         l.logger,
		trace:          newRunTrace(l.deps.Trace),
	}
	defer r.trace.close()
	runSpan := r.trace.span("tool-job", trace.KindRun, obj("agentId", prof.ID, "sessionId", sessionID, "toolName", call.Name))
	var flag atomic.Bool
	run := &toolRun{r: r, tc: l.buildToolContext(r), iterSpanID: runSpan.Span().SpanID, empty: &flag}
	ex := &executor{deps: l.deps, maxParallel: 1}
	result = ex.executeAll(ctx, run, []kernel.ToolCall{call})[0]
	runSpan.End(obj("isError", isErrorResult(result)))
	return result, flag.Load() && !isErrorResult(result)
}

func (l *Loop) executeToolCalls(ctx context.Context, r *runState, calls []kernel.ToolCall, trunc *outputTruncation) ([]kernel.ToolResult, error) {
	run := &toolRun{r: r, tc: l.buildToolContext(r), iterSpanID: r.iterSpan.Span().SpanID, trunc: trunc}
	ex := &executor{deps: l.deps, maxParallel: l.maxParallel}
	results := ex.executeAll(ctx, run, calls)
	// A-129 item 4: a result the model is given is capped (a tool job keeps its whole).
	for i := range results {
		results[i] = l.limitOutput(ctx, run, calls[i], results[i])
	}
	if err := r.addMessage(ctx, kernel.Message{Role: kernel.RoleAssistant, Content: "", ToolResults: results}); err != nil {
		return nil, err
	}
	return results, nil
}
