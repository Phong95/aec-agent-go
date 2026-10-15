// Package trace records what an agent does as a tree of timed spans and fans
// the spans out to any number of developer-supplied sinks.
//
// The span JSON is byte-identical to the recorded span fixtures (runtime key
// order, see Span), and ConsoleSink prints one human-readable line per span
// start and end.
package trace

import (
	"context"

	"github.com/Phong95/aec-agent-go/internal/jsonx"
	"github.com/Phong95/aec-agent-go/kernel/ordered"
)

// ---------------------------------------------------------------------------
// Trace types: structured span-tree observability for the agent loop
// ---------------------------------------------------------------------------

// SpanKind is the category of a span.
type SpanKind string

// The nine span kinds.
const (
	KindRun             SpanKind = "run"
	KindIteration       SpanKind = "iteration"
	KindContextAssembly SpanKind = "context-assembly"
	KindCompaction      SpanKind = "compaction"
	KindMemoryRecall    SpanKind = "memory-recall"
	KindModelCall       SpanKind = "model-call"
	KindToolDispatch    SpanKind = "tool-dispatch"
	KindSkillDispatch   SpanKind = "skill-dispatch"
	KindSandboxOp       SpanKind = "sandbox-op"
)

// SpanStatus is the lifecycle state of a span.
type SpanStatus string

// The three span statuses.
const (
	StatusOK         SpanStatus = "ok"
	StatusError      SpanStatus = "error"
	StatusInProgress SpanStatus = "in-progress"
)

// Span is one timed step. Field order is the RUNTIME key order: the Service
// builds the span with the fields up to events and assigns endTime and
// durationMs later in End, so they come last in the JSON.
type Span struct {
	TraceID      string         `json:"traceId"`
	SpanID       string         `json:"spanId"`
	ParentSpanID string         `json:"parentSpanId,omitempty"`
	Name         string         `json:"name"`
	Kind         SpanKind       `json:"kind"`
	StartTime    int64          `json:"startTime"` // epoch ms
	Status       SpanStatus     `json:"status"`
	Attributes   ordered.Object `json:"attributes"` // never null: {}; keys keep the insertion order
	Events       []SpanEvent    `json:"events"`     // never null: []
	EndTime      *int64         `json:"endTime,omitempty"`
	DurationMs   *int64         `json:"durationMs,omitempty"` // a pointer: 0 is common and must survive
}

// MarshalJSON normalises an absent Attributes to {} and a nil Events to [] (they
// are never emitted as null) and encodes with the wire encoder so the output
// is byte-identical to JSON.stringify.
func (s Span) MarshalJSON() ([]byte, error) {
	type alias Span // drops the methods, avoids recursion
	if s.Attributes.IsZero() {
		s.Attributes = ordered.NewObject()
	}
	if s.Events == nil {
		s.Events = []SpanEvent{}
	}
	return jsonx.Marshal(alias(s))
}

// SpanEvent is a notable moment during a span.
type SpanEvent struct {
	Name      string `json:"name"`
	Timestamp int64  `json:"timestamp"`
	// Attributes is omitted when absent (the zero Object) and kept when empty
	// (ordered.NewObject), as JS drops an undefined value and keeps {}.
	Attributes ordered.Object `json:"attributes,omitzero"`
}

// MarshalJSON encodes with the wire encoder (see Span.MarshalJSON).
func (e SpanEvent) MarshalJSON() ([]byte, error) {
	type alias SpanEvent
	return jsonx.Marshal(alias(e))
}

// ---------------------------------------------------------------------------
// Sink: pluggable output interface for trace consumers
// ---------------------------------------------------------------------------

// Sink receives span notifications from a Service.
//
// A Sink receives a COPY of the Span (cloned Attributes object and Events slice), so it
// may keep or mutate it freely. Passing a copy keeps the service race-free.
//
// Implementations must be comparable (pointer types are), otherwise Service.RemoveSink
// cannot find them. The Service does not guard against a panicking sink: the panic
// propagates to the caller. Sinks are developer-supplied code and are called outside
// the service lock.
type Sink interface {
	OnSpanStart(Span)
	OnSpanEnd(Span)
	Flush(ctx context.Context) error
}

// ---------------------------------------------------------------------------
// SpanHandle: returned by StartSpan, the caller ends it when done
// ---------------------------------------------------------------------------

// SpanHandle is the remote control for one span.
type SpanHandle interface {
	// Span returns a copy of the current span state.
	Span() Span
	// AddEvent records a notable moment during the span.
	AddEvent(name string, attrs ordered.Object)
	// SetAttributes merges attrs into the span attributes: an existing key
	// keeps its position and gets the new value, a new key is appended.
	SetAttributes(attrs ordered.Object)
	// End finishes the span. Only the first call has an effect.
	End(attrs ordered.Object)
}

// ---------------------------------------------------------------------------
// Context: carries the current trace/span scope through the agent loop
// ---------------------------------------------------------------------------

// Context is the trace context: the trace ID and the innermost open span.
type Context interface {
	TraceID() string
	// CurrentSpanID is the innermost open span, or "" when none is open.
	CurrentSpanID() string
}

// cloneAttrs returns a deep copy of an attribute object (ordered.Object.Clone:
// nested objects, slices and maps are copied, the zero Object stays absent).
// Attribute sets are copied whenever they cross the Service boundary, so a
// caller that keeps building the object it passed in cannot change a span.
func cloneAttrs(o ordered.Object) ordered.Object {
	return o.Clone()
}

// cloneSpan copies the attribute object, the events slice and each event's
// attribute object, and the end pointers, so no memory is shared with the source.
func cloneSpan(s Span) Span {
	out := s
	out.Attributes = cloneAttrs(s.Attributes)
	if out.Attributes.IsZero() {
		out.Attributes = ordered.NewObject()
	}
	out.Events = make([]SpanEvent, len(s.Events))
	for i, ev := range s.Events {
		ev.Attributes = cloneAttrs(ev.Attributes)
		out.Events[i] = ev
	}
	if s.EndTime != nil {
		v := *s.EndTime
		out.EndTime = &v
	}
	if s.DurationMs != nil {
		v := *s.DurationMs
		out.DurationMs = &v
	}
	return out
}
