package agentloop

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"slices"
	"time"

	"github.com/Phong95/aec-agent-go/components/agent/profile"
	"github.com/Phong95/aec-agent-go/components/events"
	"github.com/Phong95/aec-agent-go/components/toolhub"
	"github.com/Phong95/aec-agent-go/components/trace"
	"github.com/Phong95/aec-agent-go/internal/nilcheck"
	"github.com/Phong95/aec-agent-go/internal/redact"
	"github.com/Phong95/aec-agent-go/kernel"
)

// preauth is the outcome of the policy step, which attempt 0 reuses so the policy
// runs exactly once, before the retries, when nothing fails. Later attempts
// authorize again.
type preauth struct {
	auth toolhub.Authorized
	// err is an error of that Authorize call: ErrNotFound means the tool is not
	// available (the "not available" result), anything else is a failure of
	// attempt 0.
	err error

	// gated is true once the approval gate ran for this call and let it through (the
	// user approved, or no gate applies to the tool). It is per call, not per
	// attempt: a re-authorization that happens after it (a retry, or an
	// ErrPolicyChanged) keeps the approval the user gave. When it is still false at a
	// successful re-authorization, the first Authorize failed and the gate never ran,
	// so authorize runs it then: a tool never executes without the gate (WR-01).
	gated bool
	// span is the call's dispatch span, which a late gate annotates like the early one.
	span trace.SpanHandle

	// ran is true once the tool's handler was executed for this call, and
	// consequential is true when the definition that ran is consequential (an unset
	// Risk counts as consequential, fail closed). A consequential tool that ran is
	// never attempted again: a second attempt could repeat its external effect
	// (send the RFI twice), so its first outcome is final.
	ran           bool
	consequential bool
}

// markRan records that the tool of def is about to run for this call.
func (p *preauth) markRan(def toolhub.Definition) {
	if p == nil {
		return
	}
	p.ran = true
	if effectiveRisk(def) == toolhub.RiskConsequential {
		p.consequential = true
	}
}

// mayRetry reports whether another attempt of this call is allowed at all: not
// once a consequential tool ran.
func (p *preauth) mayRetry() bool {
	return p == nil || !p.ran || !p.consequential
}

// toolPanic is a panic recovered from one attempt. It is final: a panic is a bug,
// not a transient failure, so it is never retried, and its value (which can carry
// user, model or memory text) never reaches the model; it is logged, redacted,
// with the stack.
type toolPanic struct {
	value any
	stack []byte
}

func (p *toolPanic) Error() string { return "panic: " + redact.Panic(p.value) }

// maxPanicStack bounds the stack a panic log carries.
const maxPanicStack = 64 << 10

// panicStack is the stack of the calling goroutine, for the log of a recovered
// panic. It uses runtime.Stack (runtime/debug is a host-access import the layering
// rules keep out of the loop).
func panicStack() []byte {
	buf := make([]byte, maxPanicStack)
	return buf[:runtime.Stack(buf, false)]
}

// internalErrorMessage is what the model and the failed event are told about a call
// that panicked: a generic text, never the panic value.
func internalErrorMessage(name string) string {
	return "tool " + name + " failed: internal error"
}

// attemptOutcome is what one attempt produced.
type attemptOutcome struct {
	content   string
	isError   *bool
	retryable *bool
	// final marks a result that ends the retries and is returned as it is, without a
	// completion event: a denial of the policy on a re-authorization.
	final bool
	// empty is toolhub.Output.Empty (A-103): the tool succeeded and printed nothing.
	empty bool
	// link is toolhub.Output.Link (A-132): what the call produced for the user to open.
	link *kernel.ToolResultLink
	// compactArguments is toolhub.Output.CompactArguments (A-129): the call's arguments
	// as later model requests carry them.
	compactArguments kernel.Arguments
	// images is toolhub.Output.Images (A-180): images for the model to look at.
	images []kernel.MessageAttachment
}

// failing reports whether the attempt is an error the harness may retry: an error
// result the tool did not mark non-retryable.
func (o attemptOutcome) failing() bool {
	return o.isError != nil && *o.isError && (o.retryable == nil || *o.retryable)
}

// executeWithRetries runs a call up to maxToolRetries+1 times and returns the
// result for the model. The limits are read from the live
// profile on every dispatch, so a profile updated between two calls applies to the
// next one.
//
//   - A result with isError is retried unless the tool marked it non-retryable. An
//     unknown tool is a non-retryable result: it is answered at once, without
//     retries or backoff.
//   - A returned error is retried only when it may succeed again: an SDK error
//     only when it is marked retryable (kernel.IsRetryable; a PolicyError or a
//     path_escape SandboxError is final), any other error yes. After the last
//     attempt it is answered with `Error after N attempts: <message>`.
//   - A consequential tool (or one without a Risk) is never attempted again once
//     it ran, whatever the outcome: a retry could repeat its external effect.
//   - A recovered panic is final: never retried, logged with its stack (the value
//     redacted), and answered with a generic `tool <name> failed: internal error`.
//   - Between attempts the call waits base x 2^attempt milliseconds, cut short when
//     the run is stopped.
//   - ToolCompleted carries the zero-based attempt, ToolRetry the one-based
//     number of the next attempt.
//
// The completion or failure event of the call is not emitted here but returned as
// done, which the caller runs only when it keeps the result: work abandoned by a
// stopped run stays silent, and a result that was ready when the stop arrived is
// kept and reported. Retry events are emitted as they
// happen.
func (e *executor) executeWithRetries(ctx context.Context, run *toolRun, call kernel.ToolCall, tc toolhub.Context, isSkill bool, kind kindEvents, pre *preauth) (res kernel.ToolResult, done func()) {
	r := run.r
	cfg := profile.ResolveLoopConfig(e.deps.Profile().LoopConfig)
	var lastErr error
	attempts := 0
	nothing := func() {}

	for attempt := 0; attempt <= cfg.MaxToolRetries; attempt++ {
		// Do not start another attempt once the run has been stopped.
		if ctx.Err() != nil {
			break
		}
		out, err := e.runAttempt(ctx, run, call, tc, isSkill, attempt, pre)
		// A-101: a secret value a tool echoed never reaches the model, the conversation, a
		// trace, an event or the UI.
		out.content = e.redact(out.content)
		if err == nil {
			if out.final {
				return resultOf(call, out), nothing
			}
			// The tool itself reported an error and there are retries left. A stopped run
			// does not retry: the result the tool returned is kept, as it ran.
			if out.failing() && attempt < cfg.MaxToolRetries && ctx.Err() == nil && pre.mayRetry() {
				emit(r, kind.retry, events.DispatchRetryPayload{AgentID: r.agentID, Name: call.Name, Attempt: attempt + 1, Reason: out.content})
				abortableDelay(ctx, backoff(cfg.RetryBackoffMs, attempt))
				continue
			}
			end := events.DispatchEndPayload{AgentID: r.agentID, Name: call.Name, IsError: out.isError != nil && *out.isError, Attempt: attempt}
			// A-103: a tool job's call keeps the tool's "printed nothing" flag.
			if run.empty != nil && out.empty && !end.IsError {
				run.empty.Store(true)
			}
			return resultOf(call, out), func() { emit(r, kind.end, end) }
		}

		var p *toolPanic
		if errors.As(err, &p) {
			e.logPanic(run, call, p)
			msg := internalErrorMessage(call.Name)
			return errorResult(call, msg), func() { e.emitFailed(run, kind, call, msg) }
		}

		lastErr = err
		attempts = attempt + 1
		// A returned error of a stopped run is most likely the stop itself: it is
		// answered as cancelled below.
		if ctx.Err() != nil {
			break
		}
		if attempt >= cfg.MaxToolRetries || !pre.mayRetry() || !retryableError(err) {
			break
		}
		emit(r, kind.retry, events.DispatchRetryPayload{AgentID: r.agentID, Name: call.Name, Attempt: attempt + 1, Reason: e.redact(err.Error())})
		abortableDelay(ctx, backoff(cfg.RetryBackoffMs, attempt))
	}

	// The run was stopped: the caller already answered, and nobody is waiting for an
	// event from work that was abandoned. The dispatch may still keep this result,
	// when it was ready at the instant the stop was seen (raceAbortKeepFinished picks
	// either branch then), so done emits the cancelled failure the abort branch would
	// have emitted: the event is the same whichever branch wins.
	if ctx.Err() != nil {
		return cancelledResult(call), func() { e.emitFailed(run, kind, call, "cancelled") }
	}

	// All retries exhausted, or the error is final: return the error to the model so
	// it can adapt.
	msg := "undefined"
	if lastErr != nil {
		msg = e.redact(lastErr.Error())
	}
	return errorResult(call, fmt.Sprintf("Error after %s: %s", attemptsText(attempts), msg)), func() {
		emit(r, kind.fail, events.DispatchFailedPayload{AgentID: r.agentID, Name: call.Name, Error: msg})
	}
}

// redact is the A-101 redaction of every tool result and error: each secret value
// revealed in this process (and its URL / base64 forms) becomes [secret:NAME].
func (e *executor) redact(text string) string {
	if nilcheck.IsNil(e.deps.Secrets) {
		return text
	}
	return e.deps.Secrets.Redact(text)
}

// attemptsText is "1 attempt" or "N attempts".
func attemptsText(n int) string {
	if n == 1 {
		return "1 attempt"
	}
	return fmt.Sprintf("%d attempts", n)
}

// retryableError reports whether a returned error may succeed on another attempt.
// An SDK error says so itself (kernel.IsRetryable: a PolicyError, a path_escape
// SandboxError or any SDK error built non-retryable is final). Any other error
// carries no such claim and is retried, as a transient failure of a tool that is
// safe to repeat (a consequential tool never gets here once it ran).
func retryableError(err error) bool {
	var sdk *kernel.SDKError
	if errors.As(err, &sdk) && sdk != nil {
		return kernel.IsRetryable(err)
	}
	return true
}

// logPanic logs a panic of a call with its stack. The value is redacted (it can
// carry user, model or memory text); the stack holds code locations only.
func (e *executor) logPanic(run *toolRun, call kernel.ToolCall, p *toolPanic) {
	defer func() { _ = recover() }() // a logger that panics must not escape the recover path
	lg := run.r.logger
	if lg == nil {
		return
	}
	lg.Error("agentloop: tool call panicked",
		"agentId", run.r.agentID, "sessionId", run.r.sessionID, "tool", call.Name,
		"panic", redact.Panic(p.value), "stack", string(p.stack))
}

// resultOf is the result handed to the model: the retryable flag is dropped. The
// link (A-132) is kept on a successful result only, for the UI.
func resultOf(call kernel.ToolCall, o attemptOutcome) kernel.ToolResult {
	res := kernel.ToolResult{ToolCallID: call.ID, Content: o.content}
	if o.isError != nil {
		v := *o.isError
		res.IsError = &v
	}
	if o.link != nil && (o.isError == nil || !*o.isError) {
		l := *o.link
		res.Link = &l
	}
	// A-129: the call's arguments as later model requests carry them, on a successful result only
	if len(o.compactArguments) > 0 && (o.isError == nil || !*o.isError) {
		res.CompactArguments = slices.Clone(o.compactArguments)
	}
	// A-180: images the call produced for the model to look at, on a successful result only
	if len(o.images) > 0 && (o.isError == nil || !*o.isError) {
		res.Images = slices.Clone(o.images)
	}
	return res
}

// maxBackoff caps one wait between two attempts. The cap only matters for a loop
// config far outside the defaults (500 ms base, 2 retries) and keeps the arithmetic
// below from overflowing.
const maxBackoff = 5 * time.Minute

// backoff is the delay before attempt+1: base x 2^attempt milliseconds, at most
// maxBackoff. The base is clamped before it becomes a Duration, because a base above
// about 9.2e12 ms would wrap negative in the conversion itself, and the doubling
// stops at the cap.
func backoff(baseMs, attempt int) time.Duration {
	if baseMs <= 0 {
		return 0
	}
	d := time.Duration(min(baseMs, int(maxBackoff/time.Millisecond))) * time.Millisecond
	for i := 0; i < attempt && d < maxBackoff; i++ {
		d *= 2
	}
	return min(d, maxBackoff)
}

// runAttempt makes one attempt. A returned error is a failure, and a panic of the
// tool is turned into a *toolPanic here, per attempt, so the turn never crashes;
// the caller treats it as final (never retried).
func (e *executor) runAttempt(ctx context.Context, run *toolRun, call kernel.ToolCall, tc toolhub.Context, isSkill bool, attempt int, pre *preauth) (out attemptOutcome, err error) {
	defer func() {
		if rec := recover(); rec != nil {
			out, err = attemptOutcome{}, &toolPanic{value: rec, stack: panicStack()}
		}
	}()
	if isSkill {
		return e.runSkill(ctx, call, tc)
	}
	return e.runTool(ctx, run, call, tc, attempt, pre)
}

// runSkill runs the skill tool directly: skill_view skips the policy and the
// approval gate. The handler may have disappeared since the lookup.
func (e *executor) runSkill(ctx context.Context, call kernel.ToolCall, tc toolhub.Context) (attemptOutcome, error) {
	var h toolhub.Handler
	if e.deps.Skills != nil {
		h, _ = e.deps.Skills.Tool(call.Name)
	}
	if h == nil {
		return attemptOutcome{content: `Skill tool "` + call.Name + `" not available for this agent`, isError: boolPtr(true)}, nil
	}
	o, err := h.Execute(ctx, call.Arguments, tc)
	if err != nil {
		return attemptOutcome{}, err
	}
	return outcomeOf(o), nil
}

// runTool runs the tool through its authorization. Attempt 0 uses the one from the
// policy step; every later attempt authorizes again, and so does an attempt whose
// Authorized was minted under a policy that has since been replaced
// (ErrPolicyChanged), once.
func (e *executor) runTool(ctx context.Context, run *toolRun, call kernel.ToolCall, tc toolhub.Context, attempt int, pre *preauth) (attemptOutcome, error) {
	var auth toolhub.Authorized
	if attempt == 0 && pre != nil {
		if pre.err != nil {
			return authError(call, pre.err)
		}
		auth = pre.auth
	} else {
		a, stop, err := e.authorize(ctx, run, call, tc, pre)
		if stop != nil || err != nil {
			return derefOutcome(stop), err
		}
		auth = a
	}

	// The call counts as run from here on (a panic inside Execute is the tool's). A
	// stale authorization (ErrPolicyChanged) never reached the handler.
	o, err := auth.Execute(ctx)
	if !errors.Is(err, toolhub.ErrPolicyChanged) {
		pre.markRan(callDefinition(auth))
	} else {
		a, stop, aerr := e.authorize(ctx, run, call, tc, pre)
		if stop != nil || aerr != nil {
			return derefOutcome(stop), aerr
		}
		o, err = a.Execute(ctx)
		if !errors.Is(err, toolhub.ErrPolicyChanged) {
			pre.markRan(callDefinition(a))
		}
	}
	if err != nil {
		return attemptOutcome{}, err
	}
	return outcomeOf(o), nil
}

// authorize asks the tools port for a fresh authorization. It returns either the
// Authorized, or an outcome that ends this attempt (the tool is not available, the
// policy denies it now, or the approval gate that had not run yet denies it), or an
// error that is a throw.
//
// pre is the call's state: when the first Authorize failed, the approval gate has
// not run for this call, so the first successful authorization runs it here
// (fail-safe: a consequential tool, or one the sandbox gates, never executes
// without the gate on any attempt). A call whose gate already passed is not asked
// again: the user approved this exact call.
func (e *executor) authorize(ctx context.Context, run *toolRun, call kernel.ToolCall, tc toolhub.Context, pre *preauth) (toolhub.Authorized, *attemptOutcome, error) {
	auth, decision, err := e.deps.Tools.Authorize(ctx, call.Name, call.Arguments, tc)
	if err != nil {
		out, aerr := authError(call, err)
		if aerr != nil {
			return toolhub.Authorized{}, nil, aerr
		}
		return toolhub.Authorized{}, &out, nil
	}
	if !decision.Allowed {
		r := run.r
		emit(r, events.ToolDenied, events.ToolDeniedPayload{AgentID: r.agentID, Name: call.Name, Reason: decision.Reason})
		return toolhub.Authorized{}, &attemptOutcome{content: policyDeniedMessage(decision.Reason), isError: boolPtr(true), final: true}, nil
	}
	if pre != nil && !pre.gated {
		// A stopped run asks nobody: the abandoned dispatch stays silent.
		if ctx.Err() != nil {
			return toolhub.Authorized{}, &attemptOutcome{content: cancelledMessage, isError: boolPtr(true), final: true}, nil //nolint:nilerr // a stopped run is the cancelled outcome, not an error
		}
		// A-136: plan mode, which executeOne checks for a call authorized at once.
		if denied := e.checkPlanMode(ctx, run, call, callDefinition(auth)); denied != nil {
			return toolhub.Authorized{}, &attemptOutcome{content: denied.Content, isError: boolPtr(true), final: true}, nil
		}
		if denied := e.gateCall(ctx, run, call, tc, callDefinition(auth), pre.span); denied != nil {
			return toolhub.Authorized{}, &attemptOutcome{content: denied.Content, isError: boolPtr(true), final: true}, nil
		}
		pre.gated = true
	}
	return auth, nil, nil
}

// authError turns an error of Authorize into the outcome of an attempt: a tool that
// is not available is a result with isError and retryable false (it is not going to
// appear on a retry, so the model hears about it at once); any other error is a
// failure of the attempt.
func authError(call kernel.ToolCall, err error) (attemptOutcome, error) {
	if errors.Is(err, toolhub.ErrNotFound) {
		return attemptOutcome{content: notAvailableMessage(call.Name), isError: boolPtr(true), retryable: boolPtr(false)}, nil
	}
	return attemptOutcome{}, err
}

// outcomeOf copies a tool's output into an attempt outcome.
func outcomeOf(o toolhub.Output) attemptOutcome {
	out := attemptOutcome{content: o.Content, empty: o.Empty}
	if o.IsError != nil {
		out.isError = boolPtr(*o.IsError)
	}
	if o.Retryable != nil {
		out.retryable = boolPtr(*o.Retryable)
	}
	if o.Link != nil {
		l := *o.Link
		out.link = &l
	}
	out.compactArguments = slices.Clone(o.CompactArguments)
	// A-180: a tool's images must fit what every provider accepts; ones that don't are left
	// out with a note, so the model knows it did not get them.
	if len(o.Images) > 0 {
		if err := kernel.CheckAttachments(o.Images); err != nil {
			out.content += "\n\n(The images this tool returned were left out: " + err.Error() + ".)"
		} else {
			out.images = slices.Clone(o.Images)
		}
	}
	return out
}

func derefOutcome(p *attemptOutcome) attemptOutcome {
	if p == nil {
		return attemptOutcome{}
	}
	return *p
}

func boolPtr(v bool) *bool { return &v }
