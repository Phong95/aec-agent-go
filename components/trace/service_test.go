package trace

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"reflect"
	"sync"
	"testing"

	"github.com/Phong95/aec-agent-go/internal/wiretest"
	"github.com/Phong95/aec-agent-go/kernel/ordered"
)

// attrs builds an attribute object from alternating keys and values, in order.
func attrs(kv ...any) ordered.Object {
	o := ordered.NewObject()
	for i := 0; i+1 < len(kv); i += 2 {
		o.Set(kv[i].(string), kv[i+1])
	}
	return o
}

// attr reads one attribute (nil when absent).
func attr(o ordered.Object, k string) any {
	v, _ := o.Get(k)
	return v
}

// recSink is the custom Sink used by the tests. It is a pointer type, so it is
// comparable and RemoveSink can find it.
type recSink struct {
	mu       sync.Mutex
	name     string
	log      *[]string // shared flush-order log (optional)
	flushErr error
	starts   []Span
	ends     []Span
	flushes  int
}

func (r *recSink) OnSpanStart(s Span) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.starts = append(r.starts, s)
}

func (r *recSink) OnSpanEnd(s Span) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ends = append(r.ends, s)
}

func (r *recSink) Flush(context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.flushes++
	if r.log != nil {
		*r.log = append(*r.log, r.name)
	}
	return r.flushErr
}

func newDeterministic(start, step int64) *Service {
	return NewService(WithClock(wiretest.Clock(start, step)), WithIDFunc(wiretest.IDs()))
}

func TestSpanLifecycleCustomSink(t *testing.T) {
	svc := newDeterministic(100, 5)
	sink := &recSink{}
	svc.AddSink(sink)
	tc := svc.CreateContext("t1")

	h := svc.StartSpan(tc, "work", KindToolDispatch, attrs("a", 1))
	if len(sink.starts) != 1 || len(sink.ends) != 0 {
		t.Fatalf("after start: %d starts %d ends, want 1 and 0", len(sink.starts), len(sink.ends))
	}
	st := sink.starts[0]
	if st.Status != StatusInProgress || st.StartTime != 100 || st.TraceID != "t1" || st.Name != "work" || st.Kind != KindToolDispatch {
		t.Fatalf("start span = %+v", st)
	}
	if st.SpanID != "00000000-0000-4000-8000-000000000001" || st.EndTime != nil || st.DurationMs != nil {
		t.Fatalf("start span id/end = %+v", st)
	}
	if attr(st.Attributes, "a") != 1 || st.Events == nil || len(st.Events) != 0 {
		t.Fatalf("start span attributes/events = %+v", st)
	}

	h.End(attrs("b", "x"))
	if len(sink.ends) != 1 {
		t.Fatalf("after end: %d ends, want 1", len(sink.ends))
	}
	en := sink.ends[0]
	if en.Status != StatusOK || en.EndTime == nil || *en.EndTime != 105 || en.DurationMs == nil || *en.DurationMs != 5 {
		t.Fatalf("end span = %+v", en)
	}
	if attr(en.Attributes, "a") != 1 || attr(en.Attributes, "b") != "x" {
		t.Fatalf("end attributes = %v", en.Attributes)
	}
	if got := h.Span(); got.Status != StatusOK || got.EndTime == nil {
		t.Fatalf("handle span = %+v", got)
	}
}

func TestSpanEventsAndSetAttributes(t *testing.T) {
	svc := newDeterministic(0, 1)
	sink := &recSink{}
	svc.AddSink(sink)
	tc := svc.CreateContext("t")
	h := svc.StartSpan(tc, "s", KindRun, ordered.Object{})
	h.AddEvent("one", ordered.Object{})
	h.AddEvent("two", attrs("n", 1))
	h.SetAttributes(attrs("x", true))
	h.End(ordered.Object{})

	en := sink.ends[0]
	if len(en.Events) != 2 || en.Events[0].Name != "one" || !en.Events[0].Attributes.IsZero() || attr(en.Events[1].Attributes, "n") != 1 {
		t.Fatalf("events = %+v", en.Events)
	}
	if en.Events[0].Timestamp != 1 || en.Events[1].Timestamp != 2 {
		t.Fatalf("event timestamps = %d, %d", en.Events[0].Timestamp, en.Events[1].Timestamp)
	}
	if attr(en.Attributes, "x") != true {
		t.Fatalf("attributes = %v", en.Attributes)
	}
}

func TestJSTruthy(t *testing.T) {
	var nilInt *int
	one := 1
	var nilMap map[string]any
	var nilSlice []any
	falsy := map[string]any{
		"nil":            nil,
		"empty string":   "",
		"int 0":          0,
		"int64 0":        int64(0),
		"uint8 0":        uint8(0),
		"float64 0":      0.0,
		"float64 -0":     math.Copysign(0, -1),
		"float32 0":      float32(0),
		"false":          false,
		"NaN":            math.NaN(),
		"float32 NaN":    float32(math.NaN()),
		"json.Number 0":  json.Number("0"),
		"json.Number -0": json.Number("-0.0"),
		"nil pointer":    nilInt,
		"nil map":        nilMap,
		"nil slice":      nilSlice,
	}
	for name, v := range falsy {
		if jsTruthy(v) {
			t.Errorf("jsTruthy(%s) = true, want false", name)
		}
	}
	truthy := map[string]any{
		"non-empty string": "x",
		"string 0":         "0",
		"string false":     "false",
		"int 1":            1,
		"int -1":           -1,
		"uint 1":           uint(1),
		"float 0.5":        0.5,
		"float32 1":        float32(1),
		"+Inf":             math.Inf(1),
		"true":             true,
		"json.Number 1":    json.Number("1"),
		"empty map":        map[string]any{},
		"empty slice":      []any{},
		"empty typed map":  map[string]string{},
		"struct":           struct{}{},
		"non-nil pointer":  &one,
	}
	for name, v := range truthy {
		if !jsTruthy(v) {
			t.Errorf("jsTruthy(%s) = false, want true", name)
		}
	}
}

func TestStatusFollowsErrorTruthiness(t *testing.T) {
	cases := []struct {
		name string
		err  any
		want SpanStatus
	}{
		{"message", "x", StatusError},
		{"empty string", "", StatusOK},
		{"zero", 0, StatusOK},
		{"nonzero", 2, StatusError},
		{"false", false, StatusOK},
		{"nil", nil, StatusOK},
		{"empty map", map[string]any{}, StatusError},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svc := newDeterministic(0, 1)
			sink := &recSink{}
			svc.AddSink(sink)
			h := svc.StartSpan(svc.CreateContext("t"), "s", KindRun, ordered.Object{})
			h.End(attrs("error", tc.err))
			if got := sink.ends[0].Status; got != tc.want {
				t.Fatalf("status = %q, want %q", got, tc.want)
			}
		})
	}
	t.Run("set before end", func(t *testing.T) {
		svc := newDeterministic(0, 1)
		sink := &recSink{}
		svc.AddSink(sink)
		h := svc.StartSpan(svc.CreateContext("t"), "s", KindRun, attrs("error", "early"))
		h.End(ordered.Object{})
		if got := sink.ends[0].Status; got != StatusError {
			t.Fatalf("status = %q, want error", got)
		}
	})
}

func TestEndIdempotent(t *testing.T) {
	svc := newDeterministic(10, 1)
	sink := &recSink{}
	svc.AddSink(sink)
	tc := svc.CreateContext("t")
	h := svc.StartSpan(tc, "s", KindRun, ordered.Object{})
	h.End(attrs("a", 1))
	h.End(attrs("error", "late", "b", 2))
	if len(sink.ends) != 1 {
		t.Fatalf("OnSpanEnd called %d times, want 1", len(sink.ends))
	}
	got := h.Span()
	if got.Status != StatusOK || attr(got.Attributes, "error") != nil || attr(got.Attributes, "b") != nil || attr(got.Attributes, "a") != 1 {
		t.Fatalf("span after second End = %+v", got)
	}
	if got.EndTime == nil || *got.EndTime != 11 {
		t.Fatalf("EndTime = %v, want 11 (the second End must not read the clock)", got.EndTime)
	}
}

func TestParentChildNesting(t *testing.T) {
	svc := newDeterministic(0, 1)
	sink := &recSink{}
	svc.AddSink(sink)
	tc := svc.CreateContext("t")

	outer := svc.StartSpan(tc, "outer", KindRun, ordered.Object{})
	outerID := outer.Span().SpanID
	if outer.Span().ParentSpanID != "" {
		t.Fatalf("outer parent = %q, want none", outer.Span().ParentSpanID)
	}
	child := svc.StartSpan(tc, "child", KindIteration, ordered.Object{})
	if got := child.Span().ParentSpanID; got != outerID {
		t.Fatalf("child parent = %q, want %q", got, outerID)
	}
	if tc.CurrentSpanID() != child.Span().SpanID {
		t.Fatalf("current = %q, want child", tc.CurrentSpanID())
	}
	child.End(ordered.Object{})
	if tc.CurrentSpanID() != outerID {
		t.Fatalf("current after child end = %q, want outer", tc.CurrentSpanID())
	}
	second := svc.StartSpan(tc, "second", KindModelCall, ordered.Object{})
	if got := second.Span().ParentSpanID; got != outerID {
		t.Fatalf("second parent = %q, want outer %q", got, outerID)
	}
	second.End(ordered.Object{})
	outer.End(ordered.Object{})
	if tc.CurrentSpanID() != "" {
		t.Fatalf("current after all ended = %q, want none", tc.CurrentSpanID())
	}
	third := svc.StartSpan(tc, "third", KindRun, ordered.Object{})
	if third.Span().ParentSpanID != "" {
		t.Fatalf("third parent = %q, want none", third.Span().ParentSpanID)
	}
	third.End(ordered.Object{})
}

func TestUnregisteredContextDoesNotNest(t *testing.T) {
	svc := newDeterministic(0, 1)
	svc.AddSink(&recSink{})
	foreign := NewMutableContext("loose")
	a := svc.StartSpan(foreign, "a", KindRun, ordered.Object{})
	b := svc.StartSpan(foreign, "b", KindRun, ordered.Object{})
	if b.Span().ParentSpanID != "" {
		t.Fatalf("a context that is not registered on the service must not be pushed: parent = %q", b.Span().ParentSpanID)
	}
	a.End(ordered.Object{})
	b.End(ordered.Object{})
}

func TestNoSinkFastPath(t *testing.T) {
	ids := 0
	svc := NewService(WithClock(wiretest.Clock(500, 1)), WithIDFunc(func() string { ids++; return fmt.Sprintf("id-%d", ids) }))
	tc := svc.CreateContext("t")
	if ids != 0 {
		t.Fatalf("CreateContext with an explicit trace ID consumed %d IDs", ids)
	}

	h := svc.StartSpan(tc, "idle", KindRun, attrs("ignored", true))
	sp := h.Span()
	if sp.SpanID != "" || sp.StartTime != 0 || sp.Status != StatusOK || sp.TraceID != "t" || sp.Name != "idle" || sp.Kind != KindRun {
		t.Fatalf("no-op span = %+v", sp)
	}
	if sp.Attributes.IsZero() || sp.Attributes.Len() != 0 || sp.Events == nil || len(sp.Events) != 0 {
		t.Fatalf("no-op span must carry empty attributes and events: %+v", sp)
	}
	h.AddEvent("e", ordered.Object{})
	h.SetAttributes(attrs("x", 1))
	h.End(attrs("error", "ignored"))
	if got := h.Span(); got.Status != StatusOK || got.Attributes.Len() != 0 || len(got.Events) != 0 {
		t.Fatalf("no-op handle changed state: %+v", got)
	}
	if ids != 0 {
		t.Fatalf("no-op path consumed %d IDs, want 0", ids)
	}
	if tc.CurrentSpanID() != "" {
		t.Fatalf("no-op path pushed on the context stack: %q", tc.CurrentSpanID())
	}

	// A sink added under the same context afterwards: the span has no parent
	// because the no-op handle never pushed anything.
	sink := &recSink{}
	svc.AddSink(sink)
	live := svc.StartSpan(tc, "live", KindRun, ordered.Object{})
	if got := live.Span().ParentSpanID; got != "" {
		t.Fatalf("live span parent = %q, want none", got)
	}
	live.End(ordered.Object{})

	// A no-op span copies the parent from the context it was given.
	svc.RemoveSink(sink)
	outer := svc.StartSpan(tc, "outer", KindRun, ordered.Object{}) // no sinks again: no-op
	_ = outer
	tc.PushSpan("manual-parent")
	child := svc.StartSpan(tc, "child", KindRun, ordered.Object{})
	if got := child.Span().ParentSpanID; got != "manual-parent" {
		t.Fatalf("no-op span parent = %q, want manual-parent", got)
	}
}

func TestSinkReceivesCopy(t *testing.T) {
	svc := newDeterministic(0, 1)
	mutator := &mutatingSink{}
	observer := &recSink{}
	svc.AddSink(mutator)
	svc.AddSink(observer)
	tc := svc.CreateContext("t")

	h := svc.StartSpan(tc, "s", KindRun, attrs("keep", "me"))
	h.AddEvent("e", attrs("k", 1))
	h.End(ordered.Object{})

	for i, sp := range append(append([]Span{}, observer.starts...), observer.ends...) {
		if attr(sp.Attributes, "poison") != nil || sp.Name != "s" {
			t.Fatalf("observer span %d saw the first sink's mutation: %+v", i, sp)
		}
		for _, ev := range sp.Events {
			if ev.Name != "e" || attr(ev.Attributes, "poison") != nil {
				t.Fatalf("observer span %d event was mutated: %+v", i, ev)
			}
		}
	}
	got := h.Span()
	if attr(got.Attributes, "poison") != nil || attr(got.Attributes, "keep") != "me" || len(got.Events) != 1 || got.Events[0].Name != "e" || attr(got.Events[0].Attributes, "poison") != nil {
		t.Fatalf("service span was changed through a sink copy: %+v", got)
	}
	// Mutating the value returned by Span() must not change the handle either.
	got.Attributes.Set("poison", true)
	got.Events[0].Name = "changed"
	if again := h.Span(); attr(again.Attributes, "poison") != nil || again.Events[0].Name != "e" {
		t.Fatalf("Span() leaked internal state: %+v", again)
	}
}

// mutatingSink scribbles over every span it receives.
type mutatingSink struct{}

func (*mutatingSink) OnSpanStart(s Span) { scribble(s) }
func (*mutatingSink) OnSpanEnd(s Span)   { scribble(s) }
func (*mutatingSink) Flush(context.Context) error {
	return nil
}

func scribble(s Span) {
	s.Attributes.Set("poison", true)
	s.Name = "poisoned"
	for i := range s.Events {
		s.Events[i].Name = "poisoned"
		if !s.Events[i].Attributes.IsZero() {
			s.Events[i].Attributes.Set("poison", true)
		}
	}
}

func TestFlushOrderAndError(t *testing.T) {
	svc := newDeterministic(0, 1)
	var order []string
	boom := errors.New("boom")
	a := &recSink{name: "a", log: &order}
	b := &recSink{name: "b", log: &order, flushErr: boom}
	c := &recSink{name: "c", log: &order, flushErr: errors.New("later")}
	svc.AddSink(a)
	svc.AddSink(b)
	svc.AddSink(c)

	err := svc.Flush(context.Background())
	if !errors.Is(err, boom) {
		t.Fatalf("Flush error = %v, want the first sink error", err)
	}
	if !reflect.DeepEqual(order, []string{"a", "b", "c"}) {
		t.Fatalf("flush order = %v, want [a b c]", order)
	}

	order = nil
	svc.RemoveSink(b)
	svc.RemoveSink(c)
	if err := svc.Flush(context.Background()); err != nil {
		t.Fatalf("Flush = %v, want nil", err)
	}
	if !reflect.DeepEqual(order, []string{"a"}) {
		t.Fatalf("flush order after remove = %v, want [a]", order)
	}
}

func TestSinkManagement(t *testing.T) {
	svc := newDeterministic(0, 1)
	a, b := &recSink{}, &recSink{}
	svc.AddSink(a)
	svc.AddSink(nil) // ignored
	svc.AddSink(b)
	got := svc.Sinks()
	if len(got) != 2 || got[0] != Sink(a) || got[1] != Sink(b) {
		t.Fatalf("Sinks() = %v", got)
	}
	got[0] = nil // the returned slice is a copy
	if again := svc.Sinks(); again[0] != Sink(a) {
		t.Fatal("Sinks() exposed the internal slice")
	}
	svc.RemoveSink(a)
	svc.RemoveSink(a) // second removal is a no-op
	if got := svc.Sinks(); len(got) != 1 || got[0] != Sink(b) {
		t.Fatalf("Sinks() after remove = %v", got)
	}
	svc.RemoveSink(uncomparableSink{}) // must not panic
}

// uncomparableSink has a slice field, so == on it would panic at run time.
type uncomparableSink struct{ _ []int }

func (uncomparableSink) OnSpanStart(Span)            {}
func (uncomparableSink) OnSpanEnd(Span)              {}
func (uncomparableSink) Flush(context.Context) error { return nil }

func TestContextManagement(t *testing.T) {
	ids := wiretest.IDs()
	svc := NewService(WithClock(wiretest.Clock(0, 1)), WithIDFunc(ids))
	generated := svc.CreateContext("")
	if generated.TraceID() != "00000000-0000-4000-8000-000000000001" {
		t.Fatalf("generated trace ID = %q", generated.TraceID())
	}
	got, ok := svc.Context(generated.TraceID())
	if !ok || got != generated {
		t.Fatal("Context did not return the stored context")
	}
	svc.RemoveContext(generated.TraceID())
	if _, ok := svc.Context(generated.TraceID()); ok {
		t.Fatal("context still present after RemoveContext")
	}
	svc.RemoveContext("never-existed") // no-op
}

func TestConcurrentSpans(t *testing.T) {
	const workers, perWorker = 8, 50
	svc := NewService() // real clock and IDs
	sink := &recSink{}
	svc.AddSink(sink)

	var wg sync.WaitGroup
	for w := range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			tc := svc.CreateContext(fmt.Sprintf("trace-%d", w))
			for i := range perWorker {
				outer := svc.StartSpan(tc, "outer", KindIteration, attrs("i", i))
				inner := svc.StartSpan(tc, "inner", KindModelCall, ordered.Object{})
				inner.AddEvent("tick", attrs("w", w))
				inner.SetAttributes(attrs("w", w))
				inner.End(ordered.Object{})
				outer.End(attrs("done", true))
				_ = outer.Span()
			}
			svc.RemoveContext(tc.TraceID())
		}()
	}
	wg.Wait()
	if err := svc.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	want := workers * perWorker * 2
	if len(sink.starts) != want || len(sink.ends) != want {
		t.Fatalf("recorded %d starts and %d ends, want %d each", len(sink.starts), len(sink.ends), want)
	}
	for _, sp := range sink.ends {
		if sp.Status != StatusOK || sp.EndTime == nil || sp.DurationMs == nil {
			t.Fatalf("ended span incomplete: %+v", sp)
		}
	}
}

// WR-03: spans on one context that end out of LIFO order must not leave an
// already-ended span as the current parent. End pops its own id.
func TestOverlappingSpansEndOutOfOrder(t *testing.T) {
	svc := newDeterministic(0, 1)
	svc.AddSink(&recSink{})
	tc := svc.CreateContext("t")

	a := svc.StartSpan(tc, "a", KindRun, ordered.Object{})
	b := svc.StartSpan(tc, "b", KindIteration, ordered.Object{})
	aID, bID := a.Span().SpanID, b.Span().SpanID
	if b.Span().ParentSpanID != aID {
		t.Fatalf("b parent = %q, want %q", b.Span().ParentSpanID, aID)
	}

	// a ends first although b is on top of the stack.
	a.End(ordered.Object{})
	if got := tc.CurrentSpanID(); got != bID {
		t.Fatalf("current after ending a = %q, want b (%q): an ended span must not stay on the stack", got, bID)
	}
	c := svc.StartSpan(tc, "c", KindModelCall, ordered.Object{})
	if got := c.Span().ParentSpanID; got != bID {
		t.Fatalf("c parent = %q, want b (%q), not the ended span a", got, bID)
	}
	c.End(ordered.Object{})
	b.End(ordered.Object{})
	if got := tc.CurrentSpanID(); got != "" {
		t.Fatalf("current after all ended = %q, want none", got)
	}

	// An unknown id and a repeated pop are no-ops.
	tc.PopSpanID("never-pushed")
	tc.PushSpan("x")
	tc.PopSpanID("x")
	tc.PopSpanID("x")
	if got := tc.CurrentSpanID(); got != "" {
		t.Fatalf("current = %q, want none", got)
	}
}

// WR-03: reading the parent and pushing the child is one atomic step, so
// concurrent starts on one context each see a distinct parent (the previous
// push), forming a chain, and every span is popped again by its own id.
func TestConcurrentStartsOnOneContextFormAChain(t *testing.T) {
	const n = 64
	svc := NewService()
	svc.AddSink(&recSink{})
	tc := svc.CreateContext("shared")

	handles := make([]SpanHandle, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			handles[i] = svc.StartSpan(tc, "s", KindToolDispatch, ordered.Object{})
		}()
	}
	wg.Wait()

	parentUse := map[string]int{}
	roots := 0
	for _, h := range handles {
		p := h.Span().ParentSpanID
		if p == "" {
			roots++
			continue
		}
		parentUse[p]++
	}
	if roots != 1 {
		t.Fatalf("%d spans have no parent, want exactly 1", roots)
	}
	for p, c := range parentUse {
		if c != 1 {
			t.Fatalf("parent %q was read by %d spans, want 1 (non-atomic read+push)", p, c)
		}
	}

	// End in an arbitrary (creation) order: the stack must empty out.
	for _, h := range handles {
		h.End(ordered.Object{})
	}
	if got := tc.CurrentSpanID(); got != "" {
		t.Fatalf("current after all ended = %q, want none", got)
	}
}
