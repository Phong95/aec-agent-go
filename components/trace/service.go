package trace

import (
	"context"
	"reflect"
	"sync"

	"github.com/Phong95/aec-agent-go/kernel"
	"github.com/Phong95/aec-agent-go/kernel/ordered"
)

// ---------------------------------------------------------------------------
// Service: the "control room" that records everything the AI agent does
//
// ANALOGY: Think of this like a security camera system in a building.
// - The Service is the central recording system
// - "Spans" are individual camera clips (e.g. "door opened at 2:00, closed at 2:05")
// - "Sinks" are where the footage goes (TV monitor, hard drive, cloud backup)
// - "Context" tracks which room the camera is currently watching
//
// WHY IT EXISTS: When an AI agent processes a user message, many things happen
// internally: it reads context, calls the LLM, executes tools, loops back.
// Without tracing, this is a black box. Service makes it transparent
// by recording each step (a "span") with timing, and sending that data
// to any number of outputs ("sinks"): a console log, an SSE stream to
// a web dashboard, a file, etc.
//
// THE 80/20: The two things that matter most are:
//  1. StartSpan() - call this when a step BEGINS, get back a handle
//  2. handle.End() - call this when the step FINISHES, it records duration
//
// Everything else supports these two operations.
//
// The Service is safe for concurrent use; sinks receive a copy of the Span
// instead of the live object; Flush calls every sink even when an earlier one
// fails and returns the first error.
// ---------------------------------------------------------------------------

// Service records spans and notifies the registered sinks.
type Service struct {
	// mu guards sinks and contexts.
	mu sync.Mutex

	// List of outputs that want to receive trace data.
	// Example: a ConsoleSink that prints to terminal,
	// or an SSE sink that streams to a web browser.
	// When this list is EMPTY, tracing is essentially free (no-op).
	sinks []Sink

	// Active "recording sessions": one per agent run.
	// Each context tracks which span is currently "open" so that
	// new spans automatically become children of the current one.
	// Key = traceId (a unique ID for this entire agent run).
	contexts map[string]*MutableContext

	// tickMu serialises the clock and ID functions, so the deterministic test
	// helpers (which are not thread safe) can be used with a concurrent Service.
	tickMu sync.Mutex
	now    func() int64
	newID  func() string
}

// Option configures a Service.
type Option func(*Service)

// WithClock sets the timestamp source in epoch ms (default kernel.NowMillis).
func WithClock(now func() int64) Option {
	return func(s *Service) {
		if now != nil {
			s.now = now
		}
	}
}

// WithIDFunc sets the span and trace ID source (default kernel.NewID).
func WithIDFunc(f func() string) Option {
	return func(s *Service) {
		if f != nil {
			s.newID = f
		}
	}
}

// NewService returns a Service with no sinks and no contexts.
func NewService(opts ...Option) *Service {
	s := &Service{
		contexts: make(map[string]*MutableContext),
		now:      kernel.NowMillis,
		newID:    kernel.NewID,
	}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

func (s *Service) tick() int64 {
	s.tickMu.Lock()
	defer s.tickMu.Unlock()
	return s.now()
}

func (s *Service) nextID() string {
	s.tickMu.Lock()
	defer s.tickMu.Unlock()
	return s.newID()
}

// --- Sink management: add/remove outputs that receive trace data ---

// AddSink registers a new output destination for trace data.
// Call this BEFORE the agent starts processing to capture everything.
// A nil sink is ignored.
func (s *Service) AddSink(sink Sink) {
	if sink == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sinks = append(s.sinks, sink)
}

// RemoveSink unregisters an output destination.
// Call this when the output is no longer needed (e.g. browser disconnected).
// Sinks are matched with ==, so the dynamic type must be comparable; a sink
// whose value cannot be compared is never matched (and never panics).
func (s *Service) RemoveSink(sink Sink) {
	if sink == nil || !reflect.ValueOf(sink).Comparable() {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, have := range s.sinks {
		if reflect.TypeOf(have) == reflect.TypeOf(sink) && have == sink {
			s.sinks = append(s.sinks[:i:i], s.sinks[i+1:]...)
			return
		}
	}
}

// Sinks returns a copy of the registered sinks (for inspection/testing).
func (s *Service) Sinks() []Sink {
	return s.snapshotSinks()
}

func (s *Service) snapshotSinks() []Sink {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Sink, len(s.sinks))
	copy(out, s.sinks)
	return out
}

// ── Trace context management ──
// A "context" represents one complete agent run (user sends message → agent replies).
// It carries a unique traceId and knows which span is currently "open"
// so child spans can automatically nest under their parent.

// CreateContext starts a new recording session for an agent run and returns
// the context to pass to StartSpan. The traceID lets you provide your own ID
// (useful for correlating with external systems); an empty traceID generates
// a random one.
func (s *Service) CreateContext(traceID string) *MutableContext {
	id := traceID
	if id == "" {
		id = s.nextID()
	}
	ctx := NewMutableContext(id)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.contexts[id] = ctx
	return ctx
}

// Context looks up an existing recording session by its trace ID.
func (s *Service) Context(traceID string) (*MutableContext, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ctx, ok := s.contexts[traceID]
	return ctx, ok
}

// RemoveContext cleans up a recording session when the agent run is complete.
// Prevents memory from growing indefinitely as more runs happen.
func (s *Service) RemoveContext(traceID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.contexts, traceID)
}

// ── Span lifecycle ──
// A "span" is a single timed step: "model call started at X, ended at Y, took 2000ms".
// Spans form a TREE: the root span is the entire run, iteration spans are children
// of the run, model-call spans are children of iterations, tool calls are children
// of iterations too. This tree structure lets a UI render a collapsible timeline.

// StartSpan begins recording a new step and returns a "handle", a remote
// control for this span. When the step finishes, call handle.End() to record
// the end time and duration.
//
// Parameters:
//
//	tc    - the recording session (from CreateContext)
//	name  - human-readable label like "model-call" or "schedule-lookup"
//	kind  - category: "run", "iteration", "model-call", "tool-dispatch", etc.
//	attrs - extra data to attach (e.g. {"model": "claude-sonnet", "iteration": 3});
//	        an ordered.Object, so the keys keep the order the caller set them in
//
// THE KEY OPTIMIZATION: when no sinks are registered, this returns a no-op
// handle that does absolutely nothing: no ID, no timing, no context stack
// change. This means tracing is free to leave wired up in production with no
// sinks.
func (s *Service) StartSpan(tc Context, name string, kind SpanKind, attrs ordered.Object) SpanHandle {
	sinks := s.snapshotSinks()

	// Fast path: if nobody is listening, don't do any work
	if len(sinks) == 0 {
		return newNoOpHandle(tc, name, kind)
	}

	// Build the span data object: this is the "recording" for this step.
	// The ID is taken before the clock (spanId then startTime).
	spanID := s.nextID()

	// PARENT-CHILD NESTING: push this span onto the context stack.
	// Any span started AFTER this one (but before it ends) will
	// automatically have this span as its parent.
	// Think of it like nested folders: Run > Iteration > Model Call > ...
	//
	// Reading the parent and pushing the child is ONE atomic step on the
	// registered context, so two spans started concurrently on one context
	// cannot both read the same parent and then push in either order.
	s.mu.Lock()
	mutableCtx := s.contexts[tc.TraceID()]
	s.mu.Unlock()
	var parentSpanID string
	if mc, ok := tc.(*MutableContext); ok && mc != nil && mc == mutableCtx {
		parentSpanID = mutableCtx.pushSpanReturningParent(spanID)
	} else {
		parentSpanID = tc.CurrentSpanID()
		if mutableCtx != nil {
			mutableCtx.PushSpan(spanID)
		}
	}

	span := Span{
		TraceID:      tc.TraceID(),       // which agent run this belongs to
		SpanID:       spanID,             // unique ID for THIS specific step
		ParentSpanID: parentSpanID,       // which step "contains" this one (the parent)
		Name:         name,               // human label: "model-call", "context-assembly"
		Kind:         kind,               // category: "run" | "iteration" | "model-call" | ...
		StartTime:    s.tick(),           // when this step began (milliseconds since epoch)
		Status:       StatusInProgress,   // will change to "ok" or "error" when End is called
		Attributes:   copyOrEmpty(attrs), // extra key-value data
		Events:       []SpanEvent{},      // notable moments during the span (rarely used)
	}

	// Tell all sinks "a new step just started" (they can show a spinner, log it, etc.)
	notifyStart(sinks, span)

	// Return a handle so the caller can later call handle.End() when this step is done
	return &liveHandle{span: span, svc: s, ctx: mutableCtx}
}

func copyOrEmpty(o ordered.Object) ordered.Object {
	if o.IsZero() {
		return ordered.NewObject()
	}
	return cloneAttrs(o)
}

// ── Sink notifications (internal) ──
// These functions broadcast span events to ALL registered sinks.
// Called by StartSpan and by liveHandle.End, outside every lock, each sink
// with its own copy of the span.

// Tell all sinks: "a step just started".
func notifyStart(sinks []Sink, span Span) {
	for _, sink := range sinks {
		sink.OnSpanStart(cloneSpan(span))
	}
}

// Tell all sinks: "a step just finished" (with duration, status, final attributes).
func notifyEnd(sinks []Sink, span Span) {
	for _, sink := range sinks {
		sink.OnSpanEnd(cloneSpan(span))
	}
}

// Flush forces all sinks to push out any buffered data, in registration
// order. Useful at the end of a request to make sure everything is sent. Every
// sink is flushed even when an earlier one fails; the first error is returned.
func (s *Service) Flush(ctx context.Context) error {
	var first error
	for _, sink := range s.snapshotSinks() {
		if err := sink.Flush(ctx); err != nil && first == nil {
			first = err
		}
	}
	return first
}

// ---------------------------------------------------------------------------
// MutableContext: the "breadcrumb trail" that tracks nesting
//
// ANALOGY: Imagine you're exploring nested folders on your computer.
// The span stack is like your address bar showing the path:
//
//	Run > Iteration-1 > Model-Call
//
// When you open a deeper folder (start a child span), it gets pushed on.
// When you close it (end the span), it gets popped off.
// This way, the next span you open automatically knows its parent.
// ---------------------------------------------------------------------------

// MutableContext is the Context implementation the Service nests spans with.
type MutableContext struct {
	// The unique ID for this entire agent run.
	// All spans in this run share the same traceId.
	traceID string

	mu sync.Mutex
	// Stack of currently-open span IDs, from outermost to innermost.
	// Example mid-run: ["run-span-id", "iteration-1-span-id", "model-call-span-id"]
	// The LAST item is always the "current" span: new spans become its children.
	spanStack []string
}

// NewMutableContext returns an empty context for traceID. Use
// Service.CreateContext to also register it on a Service, which is what makes
// spans nest.
func NewMutableContext(traceID string) *MutableContext {
	return &MutableContext{traceID: traceID}
}

// TraceID returns the trace ID shared by every span of the run.
func (c *MutableContext) TraceID() string { return c.traceID }

// CurrentSpanID returns the span ID at the top of the stack (the innermost
// open span), or "" when no spans are open (the very start/end).
// New spans will have this value as their parentSpanId.
func (c *MutableContext) CurrentSpanID() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.spanStack) == 0 {
		return ""
	}
	return c.spanStack[len(c.spanStack)-1]
}

// PushSpan is called when a new span starts: it pushes the span onto the stack
// so it becomes the new "current" parent for any deeper spans.
func (c *MutableContext) PushSpan(spanID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.spanStack = append(c.spanStack, spanID)
}

// pushSpanReturningParent reads the current top of the stack (the new span's
// parent, "" when empty) and pushes spanID under one lock acquisition.
func (c *MutableContext) pushSpanReturningParent(spanID string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	parent := ""
	if n := len(c.spanStack); n > 0 {
		parent = c.spanStack[n-1]
	}
	c.spanStack = append(c.spanStack, spanID)
	return parent
}

// PopSpan is called when a span ends: it removes the top of the stack so its
// parent becomes "current" again. It is a no-op on an empty stack.
func (c *MutableContext) PopSpan() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if n := len(c.spanStack); n > 0 {
		c.spanStack = c.spanStack[:n-1]
	}
}

// PopSpanID removes the given span from the stack, wherever it is (the
// innermost match). When spans end out of LIFO order this keeps the spans that
// are still open on the stack, where PopSpan would drop the wrong one. It is a
// no-op when the id is not on the stack. This is what handle.End uses.
func (c *MutableContext) PopSpanID(spanID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for i := len(c.spanStack) - 1; i >= 0; i-- {
		if c.spanStack[i] == spanID {
			c.spanStack = append(c.spanStack[:i], c.spanStack[i+1:]...)
			return
		}
	}
}

// ---------------------------------------------------------------------------
// liveHandle: the "remote control" for an active span
//
// When you call StartSpan, you get back one of these (when sinks exist).
// It holds the span data and lets you:
//  1. Add events (notable moments during the span)
//  2. Set extra attributes (data you learn after the span started)
//  3. End the span (records end time, calculates duration, notifies sinks)
//
// IMPORTANT: End can only take effect ONCE. Calling it again is a no-op.
// This prevents double-counting or corrupted timing.
// ---------------------------------------------------------------------------

type liveHandle struct {
	mu sync.Mutex
	// The actual span data that gets copied to sinks
	span Span
	// Reference back to the Service so we can notify sinks when ending
	svc *Service
	// Reference to the context so we can pop this span off the stack when ending
	ctx *MutableContext
}

// Span returns a copy of the current span state.
func (h *liveHandle) Span() Span {
	h.mu.Lock()
	defer h.mu.Unlock()
	return cloneSpan(h.span)
}

// AddEvent records a notable moment DURING the span (rarely needed).
// Example: AddEvent("retry", {"attempt": 2, "reason": "timeout"}).
func (h *liveHandle) AddEvent(name string, attrs ordered.Object) {
	ts := h.svc.tick()
	h.mu.Lock()
	defer h.mu.Unlock()
	h.span.Events = append(h.span.Events, SpanEvent{
		Name:       name,
		Timestamp:  ts,
		Attributes: cloneAttrs(attrs),
	})
}

// SetAttributes attaches additional data to the span after it was started.
// Example: SetAttributes({"model": "claude-opus", "temperature": 0.7})
// Merges with any attributes already set; doesn't replace them.
func (h *liveHandle) SetAttributes(attrs ordered.Object) {
	h.mu.Lock()
	defer h.mu.Unlock()
	mergeAttrs(&h.span.Attributes, attrs)
}

// mergeAttrs applies attrs onto dst in attrs order with Set: an existing key keeps its
// position and takes the new value, a new key is appended. The values are copied so the
// caller keeps no alias.
func mergeAttrs(dst *ordered.Object, attrs ordered.Object) {
	for k, v := range attrs.Clone().All() {
		dst.Set(k, v)
	}
}

// End FINISHES the span: this is the most important method.
// Records the end time, calculates how long the step took,
// determines success/error status, pops the context stack,
// and tells all sinks "this step is done, here's the final data."
//
// The optional attrs parameter lets you add final data
// that's only known when the step completes:
//
//	handle.End({"inputTokens": 1500, "outputTokens": 300, "stopReason": "end_turn"})
func (h *liveHandle) End(attrs ordered.Object) {
	h.mu.Lock()
	// Guard: if already ended, do nothing (prevents double-end bugs)
	if h.span.EndTime != nil {
		h.mu.Unlock()
		return
	}

	// Merge any final attributes
	mergeAttrs(&h.span.Attributes, attrs)

	// Record timing: when it ended and how long it took
	end := h.svc.tick()
	dur := end - h.span.StartTime
	h.span.EndTime = &end
	h.span.DurationMs = &dur

	// Determine status: if there's a truthy "error" attribute, mark as error
	// (JS truthiness, see jsTruthy)
	if errVal, _ := h.span.Attributes.Get("error"); jsTruthy(errVal) {
		h.span.Status = StatusError
	} else {
		h.span.Status = StatusOK
	}
	final := cloneSpan(h.span)
	h.mu.Unlock()

	// Pop this span off the nesting stack so the parent becomes "current" again.
	// It pops THIS span's id, not the top of the stack, so an ended span never
	// stays the parent when spans end out of order.
	if h.ctx != nil {
		h.ctx.PopSpanID(final.SpanID)
	}

	// Broadcast to all sinks: "this step finished, here's the complete data"
	notifyEnd(h.svc.snapshotSinks(), final)
}

// ---------------------------------------------------------------------------
// noOpHandle: the "do nothing" handle for zero overhead
//
// WHY THIS EXISTS: When no sinks are registered (nobody is listening),
// there's no point recording timing data, allocating UUIDs, or managing
// the context stack. This type has empty method bodies that do nothing,
// so the agent loop runs at full speed with no tracing overhead.
//
// The calling code doesn't need to check "is tracing enabled?" before
// every span: it always gets back a SpanHandle. When sinks are present,
// it's a liveHandle that records everything. When no sinks exist,
// it's this noOpHandle that silently ignores all calls.
// ---------------------------------------------------------------------------

type noOpHandle struct {
	span Span
}

func newNoOpHandle(tc Context, name string, kind SpanKind) *noOpHandle {
	// Minimal span with dummy values: only exists to satisfy the type
	return &noOpHandle{span: Span{
		TraceID:      tc.TraceID(),
		SpanID:       "",
		ParentSpanID: tc.CurrentSpanID(),
		Name:         name,
		Kind:         kind,
		StartTime:    0,
		Status:       StatusOK,
		Attributes:   ordered.NewObject(),
		Events:       []SpanEvent{},
	}}
}

// Span returns a copy of the dummy span.
func (h *noOpHandle) Span() Span { return cloneSpan(h.span) }

// All methods are intentionally empty: no work to do when nobody is listening.
func (*noOpHandle) AddEvent(string, ordered.Object) {}
func (*noOpHandle) SetAttributes(ordered.Object)    {}
func (*noOpHandle) End(ordered.Object)              {}
