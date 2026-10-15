package sandboxhub

// SpanEnder ends a span with its result attributes. trace.SpanHandle satisfies
// it, so the Phase 9 harness binds trace.Service.StartSpan in a SpanStarter
// closure; the narrow interface keeps package sandbox free of a trace import
// (the layer rule of ARCHITECTURE).
type SpanEnder interface {
	End(attrs map[string]any)
}

// SpanStarter starts a "sandbox-op" span - bound by the caller to a trace
// context. The attrs map is the caller's to keep: a fresh
// one is made for every span.
type SpanStarter func(operation string, attrs map[string]any) SpanEnder

// TraceScope says whose operations the spans record. An empty field means
// absent: the attribute is then left out of the span. It is not wire data.
type TraceScope struct {
	AgentID   string
	SessionID string
}
