package agentloop

import (
	"context"
	"errors"
	"maps"
	"slices"
	"sync"

	"github.com/Phong95/aec-agent-go/components/artifacthub/artifact"
	"github.com/Phong95/aec-agent-go/components/sandboxhub"
	"github.com/Phong95/aec-agent-go/components/skillhub/skill"
	"github.com/Phong95/aec-agent-go/components/toolhub"
	"github.com/Phong95/aec-agent-go/components/trace"
	"github.com/Phong95/aec-agent-go/internal/nilcheck"
	"github.com/Phong95/aec-agent-go/kernel"
	"github.com/Phong95/aec-agent-go/kernel/ordered"
)

// buildToolContext is the toolhub.Context every call of one model response gets.
//
// The interaction handler is resolved here, once per batch. The skills are the
// agent's skill access when it also implements skill.AgentSkills (the skill manager
// itself). GetSandbox and GetArtifacts exist
// only when the host wired a sandbox and an artifact store. Emit forwards the event
// a tool publishes to the run's bus under the run's session id.
func (l *Loop) buildToolContext(r *runState) toolhub.Context {
	tc := toolhub.Context{
		SessionID: r.session.ID,
		RunID:     r.runID,
		AgentID:   r.agentID,
	}
	if l.deps.Interaction != nil {
		if h := l.deps.Interaction(r.session.ID); !nilcheck.IsNil(h) {
			tc.Interaction = h
		}
	}
	if sk, ok := l.deps.Skills.(skill.AgentSkills); ok {
		tc.Skills = sk
	}
	if !nilcheck.IsNil(l.deps.Sandbox) {
		tc.GetSandbox = l.deps.Sandbox.GetSandbox
	}
	if l.deps.Artifacts != nil {
		// A new view per call so each version carries the session that wrote it.
		sessionID := r.session.ID
		tc.GetArtifacts = func() artifact.View { return l.deps.Artifacts(sessionID) }
	}
	if !nilcheck.IsNil(l.deps.Jobs) {
		tc.Jobs = l.deps.Jobs
	}
	if l.deps.JobDeliveryOptions != nil {
		tc.JobDeliveryOptions = l.deps.JobDeliveryOptions(r.session.ID)
	}
	// A-105: for spawn_subagent.
	if !nilcheck.IsNil(l.deps.Subagents) {
		tc.Subagents = l.deps.Subagents
	}
	// A-101: for the tools that use secrets.
	if !nilcheck.IsNil(l.deps.Secrets) {
		tc.Secrets = l.deps.Secrets
	}
	tc.Emit = func(eventType string, payload any) {
		r.bus.Emit(kernel.Event{Type: eventType, SessionID: r.sessionID, Payload: payload})
	}
	return tc
}

// resolveOnce wraps a sandbox resolver so it runs at most once: every later call
// returns the first call's view and error. The executor applies it once per
// dispatch, so the sandbox the approval gate judges is the sandbox the tool acts in
// (no window between the check and the use, WR-02), and a failed resolution stays
// failed for the whole dispatch, retries included, for the gate and the tool alike.
// A resolver that panics leaves an error for the later callers, never a nil view
// with a nil error.
func resolveOnce(inner func(context.Context) (sandboxhub.View, error)) func(context.Context) (sandboxhub.View, error) {
	if inner == nil {
		return nil
	}
	var (
		once sync.Once
		view sandboxhub.View
		err  error
	)
	return func(ctx context.Context) (sandboxhub.View, error) {
		once.Do(func() {
			err = errSandboxPanicked // stays when inner panics
			view, err = inner(ctx)
		})
		return view, err
	}
}

// errSandboxPanicked is what a resolver that panicked leaves for its later callers.
var errSandboxPanicked = errors.New("agentloop: the sandbox resolver panicked")

// sessionWorkspace (A-153) is the sandbox resolver of one call: a tool marked
// SessionWorkspace gets the session's workspace - the runner folder the user attached
// to it (or to the parent session, for a subagent's run), else the agent's sandbox.
// Other tools, and a Sandboxes port without the session side, keep get.
func (e *executor) sessionWorkspace(run *toolRun, name string, get func(context.Context) (sandboxhub.View, error)) func(context.Context) (sandboxhub.View, error) {
	if get == nil || nilcheck.IsNil(e.deps.Sandbox) {
		return get
	}
	ss, ok := e.deps.Sandbox.(SessionSandboxes)
	if !ok {
		return get
	}
	sessionID := run.tc.SessionID
	for _, def := range listTools(e.deps.Tools, sessionID) {
		if def.Name == name {
			if !def.SessionWorkspace {
				return get
			}
			return func(ctx context.Context) (sandboxhub.View, error) { return ss.GetSessionSandbox(ctx, sessionID) }
		}
	}
	return get
}

// withTracedSandbox returns tc with GetSandbox wrapped, so the sandbox operations
// a call makes are recorded as sandbox-op spans under the call's dispatch span. The
// parent is explicit: calls that run in parallel share one
// trace context, whose current span may belong to a sibling.
func (e *executor) withTracedSandbox(run *toolRun, tc toolhub.Context, dispatch trace.SpanHandle) toolhub.Context {
	inner := tc.GetSandbox
	if inner == nil || !run.r.trace.enabled() {
		return tc
	}
	parentID := dispatch.Span().SpanID
	start := func(operation string, attrs map[string]any) sandboxhub.SpanEnder {
		h := run.r.trace.spanUnder(parentID, operation, trace.KindSandboxOp, sortedObject(attrs))
		return spanEnder{h}
	}
	scope := sandboxhub.TraceScope{AgentID: tc.AgentID, SessionID: tc.SessionID}
	tc.GetSandbox = func(ctx context.Context) (sandboxhub.View, error) {
		view, err := inner(ctx)
		if err != nil {
			return nil, err
		}
		return sandboxhub.WithTracing(view, start, scope), nil
	}
	return tc
}

// spanEnder adapts a span handle to sandboxhub.SpanEnder, which ends a span with a
// plain map (the Phase 7 contract stays map-based).
type spanEnder struct{ h trace.SpanHandle }

func (s spanEnder) End(attrs map[string]any) { s.h.End(sortedObject(attrs)) }

// sortedObject converts the attribute map sandboxhub.WithTracing hands over into an
// ordered object with sorted keys, which keeps the bytes the sandbox-traced
// fixture pins.
func sortedObject(m map[string]any) ordered.Object {
	o := ordered.NewObject()
	for _, k := range slices.Sorted(maps.Keys(m)) {
		o.Set(k, m[k])
	}
	return o
}

// toolScope is the session's tool scope (A-105), or nil when every tool of the agent may
// be used.
func (l *Loop) toolScope(sessionID string) func(toolhub.Definition) bool {
	if l.deps.ToolScope == nil {
		return nil
	}
	return l.deps.ToolScope(sessionID)
}
