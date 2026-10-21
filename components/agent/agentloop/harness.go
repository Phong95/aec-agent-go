package agentloop

import (
	"bytes"
	"context"
	"strconv"
	"sync"
	"sync/atomic"

	"github.com/Phong95/aec-agent-go/components/events"
	"github.com/Phong95/aec-agent-go/components/toolhub"
	"github.com/Phong95/aec-agent-go/components/trace"
	"github.com/Phong95/aec-agent-go/internal/jsstr"
	"github.com/Phong95/aec-agent-go/kernel"
)

// defaultMaxParallelTools is how many tool calls of one model response run at the
// same time unless WithMaxParallelTools says otherwise. The bound protects the
// process from an unbounded fan-out.
const defaultMaxParallelTools = 8

// cancelledMessage is the result of a call the executor stopped waiting on
// because the run was stopped.
const cancelledMessage = "Cancelled: the user stopped the run before this tool call finished."

// resultPreviewUnits is how much of a result the dispatch span keeps, in UTF-16
// code units.
const resultPreviewUnits = 300

// kindEvents is the set of events one dispatch kind emits. Skills
// are not dispatched by name: a skill is instructions, loaded with skill_view, and
// the skill_view calls are the "skill" kind.
type kindEvents struct {
	start events.Kind[events.DispatchStartPayload]
	end   events.Kind[events.DispatchEndPayload]
	retry events.Kind[events.DispatchRetryPayload]
	fail  events.Kind[events.DispatchFailedPayload]
}

var (
	toolEvents = kindEvents{
		start: events.ToolDispatched,
		end:   events.ToolCompleted,
		retry: events.ToolRetry,
		fail:  events.ToolFailed,
	}
	skillEvents = kindEvents{
		start: events.SkillDispatched,
		end:   events.SkillCompleted,
		retry: events.SkillRetry,
		fail:  events.SkillFailed,
	}
)

// executor is the dispatch engine of the loop: it owns
// policy, approval, retries, the parallel fan-out and the routing between tools
// and skills.
//
// It never holds a tool handler. A tool runs only as a toolhub.Authorized value that
// Tools.Authorize returned for this exact call, so the policy cannot be skipped
// (T-09-05). The one sanctioned exception is skill_view, which runs without policy
// or approval because it only reads.
type executor struct {
	deps        Deps
	maxParallel int
}

// toolRun is what the calls of one model response share: the run, the tool
// context handed to every call and the span the dispatch spans hang under.
// trunc is set when that response stopped at the output token limit (A-88).
type toolRun struct {
	r          *runState
	tc         toolhub.Context
	iterSpanID string
	trunc      *outputTruncation
	// empty is set by ExecuteToolCall (A-103): it records whether the call's kept result
	// came from a tool that succeeded with nothing to report (toolhub.Output.Empty). Nil
	// in a turn - the conversation stores plain results.
	empty *atomic.Bool
}

// outputTruncation is set on a toolRun when the model response that made the calls
// stopped at the output token limit (A-88). A cut-off call is never run: it gets an
// "output token limit" tool error instead of the generic invalid-arguments one,
// even when its arguments parsed.
type outputTruncation struct {
	// maxTokens is the configured output token limit, nil when none is set (the
	// provider's default applied).
	maxTokens *int
	// cutOff marks, by call index, the calls the response was cut off in: every call
	// with an ArgumentsError, and the last call of the response, the one the model
	// was writing when the limit hit. Its arguments can look complete: a
	// non-streaming provider hands over pre-parsed inputs, so no ArgumentsError.
	cutOff []bool
}

// newOutputTruncation marks the cut-off calls of a max_tokens response.
func newOutputTruncation(calls []kernel.ToolCall, maxTokens *int) *outputTruncation {
	cut := make([]bool, len(calls))
	for i, c := range calls {
		cut[i] = c.ArgumentsError != "" || i == len(calls)-1
	}
	return &outputTruncation{maxTokens: maxTokens, cutOff: cut}
}

// isCutOff reports whether call i was cut off; false when t is nil.
func (t *outputTruncation) isCutOff(i int) bool {
	return t != nil && i < len(t.cutOff) && t.cutOff[i]
}

// executeAll runs every call and returns the results in call order. At most maxParallel
// calls run at a time. Every call gets a result and a closed span, also when ctx is
// done: a call that cannot get a slot because the run was stopped still reaches
// executeOne, which answers it with the cancelled result.
func (e *executor) executeAll(ctx context.Context, run *toolRun, calls []kernel.ToolCall) []kernel.ToolResult {
	results := make([]kernel.ToolResult, len(calls))
	sem := make(chan struct{}, e.maxParallel)
	var wg sync.WaitGroup
	for i, call := range calls {
		wg.Go(func() {
			// A panic outside the tool's own code (the ports, an event handler) must not
			// take the process down: the call is answered with an error result. Once the
			// dispatch span is open executeOne recovers (and closes the span) itself; this
			// is the net for what happens before that. The model gets the generic text,
			// never the panic value, which is logged redacted with the stack.
			defer func() {
				if rec := recover(); rec != nil {
					e.logPanic(run, call, &toolPanic{value: rec, stack: panicStack()})
					results[i] = errorResult(call, internalErrorMessage(call.Name))
				}
			}()
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
			}
			results[i] = e.executeOne(ctx, run, call, run.trunc.isCutOff(i))
		})
	}
	wg.Wait()
	return results
}

// executeOne runs one call in this order: route it as a skill or a tool, start
// its span, answer a stopped run or unusable arguments without running anything,
// check the policy, ask for approval, then run it with retries.
// cutOff is true for a call the response was cut off in at the output token limit
// (A-88): it is answered with the output-token-limit error, like unusable
// arguments, without running anything.
func (e *executor) executeOne(ctx context.Context, run *toolRun, call kernel.ToolCall, cutOff bool) (result kernel.ToolResult) {
	r := run.r
	kind, spanKind := toolEvents, trace.KindToolDispatch
	isSkill := e.isSkill(call.Name)
	if isSkill {
		kind, spanKind = skillEvents, trace.KindSkillDispatch
	}

	// skillName is the skill skill_view loads; it is used only when the argument is a
	// string, and names the span after it.
	skillName, hasSkillName := "", false
	if isSkill {
		skillName, hasSkillName = skillNameOf(call.Arguments)
	}
	spanName := call.Name
	if hasSkillName {
		spanName = skillName
	}
	attrs := obj("toolName", call.Name, "arguments", kernel.Arguments(bytes.Clone(call.Arguments)))
	if skillName != "" {
		attrs.Set("skillName", skillName)
	}
	span := r.trace.spanUnder(run.iterSpanID, spanName, spanKind, attrs)

	// A panic after the span opened (a port, an event handler, the sandbox resolver)
	// ends the span and reports the failure here, so the dispatch span is never left
	// open; executeAll's own recover then has nothing left to do for this call. Like a
	// panic of the tool it is final and reported with the generic text; the value is
	// logged redacted, with the stack.
	defer func() {
		if rec := recover(); rec != nil {
			e.logPanic(run, call, &toolPanic{value: rec, stack: panicStack()})
			msg := internalErrorMessage(call.Name)
			span.End(obj("error", msg))
			result = errorResult(call, msg)
			e.emitFailedSafely(run, kind, call, msg)
		}
	}()

	emit(r, kind.start, events.DispatchStartPayload{AgentID: r.agentID, Name: call.Name, Arguments: call.Arguments})

	// The sandbox is resolved once per dispatch and shared by the approval gate and
	// the tool, then wrapped so its operations are traced as sandbox-op spans under
	// this call's dispatch span.
	base := run.tc
	base.GetSandbox = resolveOnce(base.GetSandbox)
	// A-105: the call's id, so a tool can tie what it starts to this call
	// (spawn_subagent's run record).
	base.ToolCallID = call.ID
	tc := e.withTracedSandbox(run, base, span)

	// The run was stopped before this call could even start.
	if ctx.Err() != nil {
		e.emitFailed(run, kind, call, "cancelled")
		span.End(obj("error", "cancelled"))
		return cancelledResult(call)
	}

	// A-88: the response stopped at max_tokens and this call is one it was cut off
	// in. It never runs (nor asks for approval), even if its arguments parsed: it is
	// answered with an "output token limit" error that tells the model to write
	// large content in smaller parts.
	if cutOff {
		e.emitFailed(run, kind, call, "output-truncated")
		end := obj("error", "output-truncated")
		if call.ArgumentsError != "" {
			end.Set("argumentsError", call.ArgumentsError)
		}
		if run.trunc.maxTokens != nil {
			end.Set("maxTokens", *run.trunc.maxTokens)
		}
		span.End(end)
		return errorResult(call, outputTruncatedMessage(call, run.trunc.maxTokens))
	}

	// The model's arguments could not be parsed: never run the tool (nor ask for
	// approval); answer with a tool error so the model can call it again with valid
	// JSON. Nothing below this point sees such a call (T-08-01).
	if call.ArgumentsError != "" {
		e.emitFailed(run, kind, call, "invalid-arguments")
		span.End(obj("error", "invalid-arguments", "argumentsError", call.ArgumentsError))
		return errorResult(call, invalidArgumentsMessage(call))
	}

	// A-105: a subagent's session may use only the tools its scope allows (read-only by
	// default, never spawn_subagent). Checked before the policy, so nothing is asked.
	if !isSkill {
		if denied := e.checkScope(run, call); denied != nil {
			span.End(obj("error", "scope-denied"))
			return *denied
		}
	}

	// Policy check before dispatch (tools only: skill_view only reads). The returned
	// Authorized is what attempt 0 runs, so the policy runs exactly once when nothing
	// fails.
	var pre *preauth
	if !isSkill {
		auth, decision, err := e.deps.Tools.Authorize(ctx, call.Name, call.Arguments, tc)
		switch {
		case err != nil:
			// ErrNotFound is the "not available" result; any other error is a failure
			// of attempt 0. Neither has a tool to gate yet: a later re-authorization that
			// succeeds runs the approval gate first (authorize).
			pre = &preauth{err: err, span: span}
		case !decision.Allowed:
			emit(r, events.ToolDenied, events.ToolDeniedPayload{AgentID: r.agentID, Name: call.Name, Reason: decision.Reason})
			span.End(obj("error", "policy-denied"))
			return errorResult(call, policyDeniedMessage(decision.Reason))
		default:
			pre = &preauth{auth: auth, span: span}
		}
	}

	// A-136: plan mode - only read-only calls run; refused before the approval gate, so
	// nothing is asked. (core/ts checks it before the policy; here the call's own risk -
	// http_request GET is read-only - is known once the call is authorized, so a call
	// the policy denies is answered with the policy's denial instead.)
	if pre != nil && pre.err == nil {
		if denied := e.checkPlanMode(ctx, run, call, callDefinition(pre.auth)); denied != nil {
			span.End(obj("error", "plan-mode"))
			return *denied
		}
	}

	// Approval gate: consequential tools, and sandbox operations the sandbox requires
	// approval for (for example writes on a host folder), pause for the user. Once it
	// let the call through (gated), later attempts of this call do not ask again.
	if pre != nil && pre.err == nil {
		// A-101: a tool whose risk depends on the call (http_request GET vs POST) says so per call.
		if denied := e.gateCall(ctx, run, call, tc, callDefinition(pre.auth), span); denied != nil {
			span.End(obj("error", "approval-denied"))
			return *denied
		}
		pre.gated = true
	}

	// Stopped while waiting for approval (approved, then stopped right after) or
	// between the approval gate and dispatch.
	if ctx.Err() != nil {
		span.End(obj("error", "cancelled"))
		return cancelledResult(call)
	}

	// Race the whole retry loop against the run: a tool that ignores ctx keeps
	// running in the background, but its result is dropped once the run was stopped.
	// A result that was already there when the stop was seen is kept: the tool ran,
	// so the history must say so, and the loop ends the run as aborted at its next
	// safe point.
	type dispatched struct {
		result kernel.ToolResult
		done   func()
	}
	d, aborted, err := raceAbortKeepFinished(ctx, func() (dispatched, error) {
		res, done := e.executeWithRetries(ctx, run, call, tc, isSkill, kind, pre)
		return dispatched{result: res, done: done}, nil
	})
	if aborted {
		e.emitFailed(run, kind, call, "cancelled")
		span.End(obj("error", "cancelled"))
		return cancelledResult(call)
	}
	result = d.result
	if err != nil {
		// A panic that escaped the per-attempt recover (raceAbort's own net). The
		// model gets the generic text; raceAbort kept only the value's text, which is
		// not logged.
		msg := internalErrorMessage(call.Name)
		e.emitFailed(run, kind, call, msg)
		result = errorResult(call, msg)
	} else if d.done != nil {
		// The completion or failure event of the result the call keeps.
		d.done()
	}
	span.End(obj("isError", isErrorResult(result), "resultPreview", jsstr.TruncateUTF16(result.Content, resultPreviewUnits)))
	return result
}

// checkScope answers a call outside the session's tool scope (A-105: a subagent's
// read-only scope) with a denial, and returns nil for a call in scope, a session without
// a scope, or a tool the agent does not have (the policy reports that one).
func (e *executor) checkScope(run *toolRun, call kernel.ToolCall) *kernel.ToolResult {
	if e.deps.ToolScope == nil {
		return nil
	}
	inScope := e.deps.ToolScope(run.r.sessionID)
	if inScope == nil {
		return nil
	}
	for _, def := range listTools(e.deps.Tools, run.r.sessionID) {
		if def.Name != call.Name {
			continue
		}
		if inScope(def) {
			return nil
		}
		reason := `"` + call.Name + `" is not available to you as a subagent in this run`
		emit(run.r, events.ToolDenied, events.ToolDeniedPayload{AgentID: run.r.agentID, Name: call.Name, Reason: &reason})
		res := errorResult(call, "Policy denied: "+reason+".")
		return &res
	}
	return nil
}

// checkPlanMode (A-136) answers a call that is not read-only with a denial while the
// session is in plan mode, and returns nil otherwise.
func (e *executor) checkPlanMode(ctx context.Context, run *toolRun, call kernel.ToolCall, def toolhub.Definition) *kernel.ToolResult {
	if effectiveRisk(def) == toolhub.RiskReadOnly {
		return nil
	}
	if e.approvalMode(ctx, run.r.sessionID) != kernel.ApprovalModePlan {
		return nil
	}
	reason := `"` + call.Name + `" can change things, and this conversation is in plan mode (read-only)`
	emit(run.r, events.ToolDenied, events.ToolDeniedPayload{AgentID: run.r.agentID, Name: call.Name, Reason: &reason})
	res := errorResult(call, "Plan mode: "+reason+", so it was not run. Look things up with read-only tools, then "+
		"present your plan; the user switches plan mode off to carry it out.")
	return &res
}

// isSkill reports whether name is a skill tool (skill_view), that is a tool the
// skill manager knows.
func (e *executor) isSkill(name string) bool {
	if e.deps.Skills == nil {
		return false
	}
	_, ok := e.deps.Skills.Tool(name)
	return ok
}

// emitFailed emits the kind's failed event.
func (e *executor) emitFailed(run *toolRun, kind kindEvents, call kernel.ToolCall, msg string) {
	emit(run.r, kind.fail, events.DispatchFailedPayload{AgentID: run.r.agentID, Name: call.Name, Error: msg})
}

// emitFailedSafely is emitFailed for a recover path: a bus that panics again must
// not escape the recover that is reporting the first panic.
func (e *executor) emitFailedSafely(run *toolRun, kind kindEvents, call kernel.ToolCall, msg string) {
	defer func() { _ = recover() }()
	e.emitFailed(run, kind, call, msg)
}

// skillNameOf reads the "name" argument of a skill call. ok is true only when it
// is a string.
func skillNameOf(args kernel.Arguments) (name string, ok bool) {
	var v struct {
		Name any `json:"name"`
	}
	if err := args.Decode(&v); err != nil {
		return "", false
	}
	name, ok = v.Name.(string)
	return name, ok
}

// invalidArgumentsMessage is the result of a call whose arguments could not be
// parsed.
func invalidArgumentsMessage(call kernel.ToolCall) string {
	return `Invalid arguments for "` + call.Name + `": ` + call.ArgumentsError + ". " +
		"Call the tool again with its arguments as one valid JSON object " +
		"(escape quotes, backslashes and newlines inside strings)."
}

// outputTruncatedMessage is the result of a call the response was cut off in at the
// output token limit (A-88). It names maxTokens when one is configured.
func outputTruncatedMessage(call kernel.ToolCall, maxTokens *int) string {
	return "Your response hit the output token limit" + maxTokensSuffix(maxTokens) +
		` while writing the arguments for "` + call.Name + `", so this call was not run. ` +
		"Write large content in smaller parts — " +
		"e.g. create the file with a first section, then append the rest in further calls."
}

// truncationLimitMessage is the text that ends a run stopped by the
// consecutive-truncation guard (A-88): the Run fallback message and the Stream
// error chunk.
func truncationLimitMessage(maxTokens *int) string {
	return "The model's output was cut off at the output token limit" + maxTokensSuffix(maxTokens) +
		" on " + strconv.Itoa(maxConsecutiveTruncations) + " consecutive responses while writing tool call arguments. " +
		"Raise maxTokens or ask for the work in smaller steps."
}

// maxTokensSuffix is " (maxTokens=N)" when maxTokens is set, empty otherwise.
func maxTokensSuffix(maxTokens *int) string {
	if maxTokens == nil {
		return ""
	}
	return " (maxTokens=" + strconv.Itoa(*maxTokens) + ")"
}

// notAvailableMessage is the result of a tool the agent cannot use: it is not
// registered, or not in the agent's tool list.
func notAvailableMessage(name string) string {
	return `Tool "` + name + `" not available for this agent`
}

// policyDeniedMessage is the result of a call the policy denied. An absent or empty
// reason reads just "Policy denied", never "Policy denied: undefined".
func policyDeniedMessage(reason *string) string {
	if reason == nil || *reason == "" {
		return "Policy denied"
	}
	return "Policy denied: " + *reason
}

// cancelledResult is the result of a call the run's stop cut short.
func cancelledResult(call kernel.ToolCall) kernel.ToolResult {
	return errorResult(call, cancelledMessage)
}

// errorResult is an isError result for call.
func errorResult(call kernel.ToolCall, content string) kernel.ToolResult {
	t := true
	return kernel.ToolResult{ToolCallID: call.ID, Content: content, IsError: &t}
}

// isErrorResult reports whether r is an error result; an unset flag means false.
func isErrorResult(r kernel.ToolResult) bool { return r.IsError != nil && *r.IsError }
