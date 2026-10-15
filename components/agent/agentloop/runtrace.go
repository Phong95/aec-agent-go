package agentloop

import (
	"sync"

	"github.com/Phong95/aec-agent-go/components/trace"
	"github.com/Phong95/aec-agent-go/kernel/ordered"
)

// runTrace is one run's trace: the trace context plus a span helper
// that does nothing when the agent has no trace service. Call sites never check
// for nil: with Deps.Trace nil, span returns a shared no-op handle.
//
// It also remembers every span it started, so a run that fails can end the ones
// still open (spans are ended at most once, so ending a finished one again is
// harmless). It is safe for concurrent use: the tool executor (09-02) starts
// dispatch spans from several goroutines.
type runTrace struct {
	svc *trace.Service
	ctx *trace.MutableContext

	mu      sync.Mutex
	started []trace.SpanHandle
}

// newRunTrace opens a trace context on svc. A nil svc means tracing is off.
func newRunTrace(svc *trace.Service) *runTrace {
	t := &runTrace{svc: svc}
	if svc != nil {
		t.ctx = svc.CreateContext("")
	}
	return t
}

// span starts a span as a child of the current one, or returns the shared no-op
// handle when tracing is off.
func (t *runTrace) span(name string, kind trace.SpanKind, attrs ordered.Object) trace.SpanHandle {
	if t.ctx == nil {
		return noSpan{}
	}
	h := t.svc.StartSpan(t.ctx, name, kind, attrs)
	t.mu.Lock()
	t.started = append(t.started, h)
	t.mu.Unlock()
	return h
}

// spanUnder starts a span as a child of the span parentID instead of the current
// one, or returns the shared no-op handle when tracing is off. Calls that run in
// parallel share one trace context, whose current span may belong to a sibling;
// naming the parent keeps every dispatch span under the iteration span and every
// sandbox-op span under its own dispatch span (withTracedSandbox relies on this for
// sandbox operations). An empty parentID means the current span.
func (t *runTrace) spanUnder(parentID, name string, kind trace.SpanKind, attrs ordered.Object) trace.SpanHandle {
	if t.ctx == nil {
		return noSpan{}
	}
	if parentID == "" {
		return t.span(name, kind, attrs)
	}
	h := t.svc.StartSpan(explicitParent{traceID: t.ctx.TraceID(), spanID: parentID}, name, kind, attrs)
	t.mu.Lock()
	t.started = append(t.started, h)
	t.mu.Unlock()
	return h
}

// explicitParent is a trace.Context that names its current span. StartSpan reads
// the parent from it and still registers the new span on the run's own context
// (the non-mutable context path of trace.Service).
type explicitParent struct{ traceID, spanID string }

func (p explicitParent) TraceID() string       { return p.traceID }
func (p explicitParent) CurrentSpanID() string { return p.spanID }

var _ trace.Context = explicitParent{}

// enabled reports whether spans are recorded (the run trace has a context).
// Work that only feeds a span, such as describing the model input, is skipped
// when it is false.
func (t *runTrace) enabled() bool { return t.ctx != nil }

// endOpen ends every span this run started and has not ended, newest first, with
// attrs. A span that already ended ignores the call.
func (t *runTrace) endOpen(attrs ordered.Object) {
	t.mu.Lock()
	started := t.started
	t.started = nil
	t.mu.Unlock()
	for i := len(started) - 1; i >= 0; i-- {
		started[i].End(attrs.Clone())
	}
}

// close removes the trace context. Call once, when the run ends.
func (t *runTrace) close() {
	if t.ctx != nil {
		t.svc.RemoveContext(t.ctx.TraceID())
	}
}

// noSpan is the shared handle of a run without tracing: every call does nothing.
type noSpan struct{}

func (noSpan) Span() trace.Span                { return trace.Span{} }
func (noSpan) AddEvent(string, ordered.Object) {}
func (noSpan) SetAttributes(ordered.Object)    {}
func (noSpan) End(ordered.Object)              {}

var _ trace.SpanHandle = noSpan{}

// obj builds an attribute set from alternating keys and values, in the order
// given. The call sites write their attribute sets in a fixed order, so a span
// keeps a stable key order. An odd trailing element is ignored.
func obj(kv ...any) ordered.Object {
	o := ordered.NewObject()
	for i := 0; i+1 < len(kv); i += 2 {
		if k, ok := kv[i].(string); ok {
			o.Set(k, kv[i+1])
		}
	}
	return o
}
