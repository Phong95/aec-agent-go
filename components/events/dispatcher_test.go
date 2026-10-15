package events_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"iter"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"github.com/Phong95/aec-agent-go/components/events"
	"github.com/Phong95/aec-agent-go/internal/wiretest"
	"github.com/Phong95/aec-agent-go/kernel"
)

// collect drains a Replay sequence and fails the test on any yielded error.
func collect(t *testing.T, seq iter.Seq2[kernel.Event, error]) []kernel.Event {
	t.Helper()
	var out []kernel.Event
	for e, err := range seq {
		if err != nil {
			t.Fatalf("replay yielded error: %v", err)
		}
		out = append(out, e)
	}
	return out
}

func TestEmitOrderAndOverwrite(t *testing.T) {
	d := events.New(events.WithClock(wiretest.Clock(1000, 5)), events.WithIDFunc(wiretest.IDs()))

	var order []string
	d.On("a", func(kernel.Event) { order = append(order, "h1") })
	d.On("a", func(kernel.Event) { order = append(order, "h2") })
	d.On("a", func(kernel.Event) { order = append(order, "h3") })
	d.On("b", func(kernel.Event) { order = append(order, "other") })

	// ID and Timestamp supplied by the caller must be overwritten.
	got := d.Emit(kernel.Event{Type: "a", SessionID: "s", Payload: "p", ID: "caller-id", Timestamp: 7})
	if got.ID != "00000000-0000-4000-8000-000000000001" || got.Timestamp != 1000 {
		t.Fatalf("id/timestamp not overwritten: %+v", got)
	}
	if strings.Join(order, ",") != "h1,h2,h3" {
		t.Fatalf("handlers called out of registration order: %v", order)
	}
	second := d.Emit(kernel.Event{Type: "c", SessionID: "s"})
	if second.ID != "00000000-0000-4000-8000-000000000002" || second.Timestamp != 1005 {
		t.Fatalf("second event: %+v", second)
	}

	stored := collect(t, d.Replay(t.Context(), "s", 0))
	if len(stored) != 2 || stored[0] != got || stored[1] != second {
		t.Fatalf("stored events differ from returned events: %+v", stored)
	}
	if d.Name() != "event-dispatcher" || d.Version() != "1.0.0" {
		t.Fatalf("name/version: %q %q", d.Name(), d.Version())
	}
}

func TestPanickingHandlerIsolated(t *testing.T) {
	var buf bytes.Buffer
	d := events.New(events.WithLogger(slog.New(slog.NewTextHandler(&buf, nil))))

	const sentinel = "PAYLOAD-ONLY-SENTINEL"
	var secondCalled, thirdCalled bool
	d.On("boom", func(kernel.Event) { panic("kaboom-value") })
	d.On("boom", func(kernel.Event) { secondCalled = true })
	d.On("boom", func(kernel.Event) { thirdCalled = true })

	e := d.Emit(kernel.Event{Type: "boom", SessionID: "s", Payload: map[string]string{"secret": sentinel}})
	if e.Type != "boom" {
		t.Fatalf("Emit returned %+v", e)
	}
	if !secondCalled || !thirdCalled {
		t.Fatalf("delivery stopped after the panic: second=%v third=%v", secondCalled, thirdCalled)
	}
	out := buf.String()
	for _, want := range []string{"event handler panicked", "type=boom", "panic=string"} {
		if !strings.Contains(out, want) {
			t.Errorf("log %q does not contain %q", out, want)
		}
	}
	// A recovered panic value can carry user text, so the log has its type only
	// (10-SECURITY UF-1, Phase 12 12-02).
	if strings.Contains(out, "kaboom-value") {
		t.Errorf("log leaked the panic value: %q", out)
	}
	if strings.Contains(out, sentinel) {
		t.Errorf("log leaked payload content: %q", out)
	}
	if got := collect(t, d.Replay(t.Context(), "s", 0)); len(got) != 1 {
		t.Errorf("event not stored after panic: %d", len(got))
	}
}

func TestUnsubscribeSemantics(t *testing.T) {
	d := events.New()

	// Unsubscribe stops delivery and is idempotent.
	var n int
	unsub := d.On("t", func(kernel.Event) { n++ })
	d.Emit(kernel.Event{Type: "t", SessionID: "s"})
	unsub()
	unsub()
	d.Emit(kernel.Event{Type: "t", SessionID: "s"})
	if n != 1 {
		t.Fatalf("handler called %d times, want 1", n)
	}

	// A handler unsubscribed by an earlier handler during the same Emit is skipped.
	var unsubSecond events.Unsubscribe
	var secondCalls int
	d.On("u", func(kernel.Event) { unsubSecond() })
	unsubSecond = d.On("u", func(kernel.Event) { secondCalls++ })
	d.Emit(kernel.Event{Type: "u", SessionID: "s"})
	if secondCalls != 0 {
		t.Fatalf("handler unsubscribed in flight was still called %d times", secondCalls)
	}

	// A handler added during an Emit is first called on the next Emit.
	var added int
	var once sync.Once
	d.On("v", func(kernel.Event) {
		once.Do(func() { d.On("v", func(kernel.Event) { added++ }) })
	})
	d.Emit(kernel.Event{Type: "v", SessionID: "s"})
	if added != 0 {
		t.Fatalf("handler added during Emit was called in the same Emit")
	}
	d.Emit(kernel.Event{Type: "v", SessionID: "s"})
	if added != 1 {
		t.Fatalf("handler added during Emit called %d times on the next Emit, want 1", added)
	}

	// Handlers may re-enter Emit (the lock is not held while they run).
	var inner int
	d.On("inner", func(kernel.Event) { inner++ })
	d.On("outer", func(kernel.Event) { d.Emit(kernel.Event{Type: "inner", SessionID: "s"}) })
	d.Emit(kernel.Event{Type: "outer", SessionID: "s"})
	if inner != 1 {
		t.Fatalf("re-entrant emit delivered %d times, want 1", inner)
	}

	// The same function registered twice is called twice (Go cannot compare funcs).
	var twice int
	h := func(kernel.Event) { twice++ }
	d.On("w", h)
	d.On("w", h)
	d.Emit(kernel.Event{Type: "w", SessionID: "s"})
	if twice != 2 {
		t.Fatalf("duplicate registration called %d times, want 2", twice)
	}
}

func TestReplayFiltering(t *testing.T) {
	d := events.New(events.WithClock(wiretest.Clock(100, 10)))
	d.Emit(kernel.Event{Type: "a", SessionID: "s1"}) // ts 100
	d.Emit(kernel.Event{Type: "b", SessionID: "s2"}) // ts 110
	d.Emit(kernel.Event{Type: "c", SessionID: "s1"}) // ts 120
	d.Emit(kernel.Event{Type: "d", SessionID: "s1"}) // ts 130

	types := func(es []kernel.Event) string {
		var s []string
		for _, e := range es {
			s = append(s, e.Type)
		}
		return strings.Join(s, ",")
	}
	ctx := t.Context()
	if got := types(collect(t, d.Replay(ctx, "s1", 0))); got != "a,c,d" {
		t.Errorf("session filter / order: %s", got)
	}
	if got := types(collect(t, d.Replay(ctx, "s1", 120))); got != "c,d" {
		t.Errorf("event equal to fromTimestamp must be kept: %s", got)
	}
	if got := types(collect(t, d.Replay(ctx, "s1", 121))); got != "d" {
		t.Errorf("fromTimestamp 121: %s", got)
	}
	if got := collect(t, d.Replay(ctx, "nope", 0)); len(got) != 0 {
		t.Errorf("unknown session yielded %d events", len(got))
	}
}

func TestReplayBreakAndCancel(t *testing.T) {
	d := events.New()
	for i := range 5 {
		d.Emit(kernel.Event{Type: fmt.Sprintf("e%d", i), SessionID: "s"})
	}

	// Breaking out of the range loop stops iteration.
	var seen int
	for _, err := range d.Replay(t.Context(), "s", 0) {
		if err != nil {
			t.Fatal(err)
		}
		seen++
		if seen == 2 {
			break
		}
	}
	if seen != 2 {
		t.Fatalf("saw %d events after break, want 2", seen)
	}

	// A cancelled context yields its error and stops (the loop does not
	// break on the error: the sequence itself must end).
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var got int
	var errs []error
	for e, err := range d.Replay(ctx, "s", 0) {
		if err != nil {
			errs = append(errs, err)
			continue
		}
		got++
		if e.Type == "e1" {
			cancel()
		}
	}
	if got != 2 || len(errs) != 1 || !errors.Is(errs[0], context.Canceled) {
		t.Fatalf("got %d events and errors %v, want 2 events and one context.Canceled", got, errs)
	}

	// Cancelled before the first element: the very first yield is the error.
	ctx2, cancel2 := context.WithCancel(t.Context())
	cancel2()
	var first []error
	for e, err := range d.Replay(ctx2, "s", 0) {
		first = append(first, err)
		if err == nil || e.Type != "" {
			t.Fatalf("expected zero event with error, got %+v %v", e, err)
		}
	}
	if len(first) != 1 {
		t.Fatalf("cancelled replay yielded %d items, want 1", len(first))
	}
}

func TestConcurrentEmitOrder(t *testing.T) {
	const goroutines, perG = 8, 100
	d := events.New()
	var delivered sync.Map
	d.On("c", func(e kernel.Event) { delivered.Store(e.ID, true) })

	var wg sync.WaitGroup
	for g := range goroutines {
		wg.Go(func() {
			sid := fmt.Sprintf("g%d", g)
			for i := range perG {
				d.Emit(kernel.Event{Type: "c", SessionID: sid, Payload: i})
			}
		})
	}
	wg.Wait()

	all := 0
	for g := range goroutines {
		got := collect(t, d.Replay(t.Context(), fmt.Sprintf("g%d", g), 0))
		if len(got) != perG {
			t.Fatalf("goroutine %d: %d events, want %d", g, len(got), perG)
		}
		for i, e := range got {
			if e.Payload != i {
				t.Fatalf("goroutine %d: event %d has payload %v: per-goroutine order broken", g, i, e.Payload)
			}
		}
		all += len(got)
	}
	n := 0
	delivered.Range(func(_, _ any) bool { n++; return true })
	if all != goroutines*perG || n != goroutines*perG {
		t.Fatalf("stored %d, delivered %d, want %d", all, n, goroutines*perG)
	}
}

func TestSharedBackingLog(t *testing.T) {
	log := events.NewLog()
	a := events.New(events.WithBacking(log))
	a.Emit(kernel.Event{Type: "x", SessionID: "s"})
	b := events.New(events.WithBacking(log))
	b.Emit(kernel.Event{Type: "y", SessionID: "s"})
	if got := collect(t, b.Replay(t.Context(), "s", 0)); len(got) != 2 {
		t.Fatalf("shared log: %d events, want 2", len(got))
	}
	if err := kernel.Initialize(t.Context(), a); err != nil {
		t.Fatal(err)
	}
	if err := kernel.Dispose(t.Context(), a); err != nil {
		t.Fatal(err)
	}
}

// TestLogCapacity: the log is a ring buffer. Once it is full the oldest event is
// dropped and Replay yields the newest ones, oldest first; a capacity of 0 keeps
// nothing while the handlers still receive every event; a negative capacity keeps
// everything; and the default keeps DefaultLogCapacity.
func TestLogCapacity(t *testing.T) {
	types := func(es []kernel.Event) string {
		out := make([]string, 0, len(es))
		for _, e := range es {
			out = append(out, e.Type)
		}
		return strings.Join(out, ",")
	}
	emit := func(d *events.Dispatcher, n int) {
		for i := range n {
			d.Emit(kernel.Event{Type: fmt.Sprintf("e%d", i), SessionID: "s"})
		}
	}

	t.Run("ring keeps the newest", func(t *testing.T) {
		d := events.New(events.WithLogCapacity(3))
		emit(d, 2)
		if got := types(collect(t, d.Replay(t.Context(), "s", 0))); got != "e0,e1" {
			t.Fatalf("before the ring is full: %q", got)
		}
		emit(d, 7) // e0..e6 again: the last three are e4, e5, e6
		if got := types(collect(t, d.Replay(t.Context(), "s", 0))); got != "e4,e5,e6" {
			t.Fatalf("after wrapping: %q, want e4,e5,e6", got)
		}
	})

	t.Run("zero disables the log, not delivery", func(t *testing.T) {
		d := events.New(events.WithLogCapacity(0))
		delivered := 0
		d.On("e0", func(kernel.Event) { delivered++ })
		emit(d, 1)
		if delivered != 1 {
			t.Fatalf("delivered %d, want 1", delivered)
		}
		if got := collect(t, d.Replay(t.Context(), "s", 0)); len(got) != 0 {
			t.Fatalf("replay of a disabled log = %d events, want none", len(got))
		}
	})

	t.Run("negative is unbounded", func(t *testing.T) {
		d := events.New(events.WithLogCapacity(-1))
		emit(d, events.DefaultLogCapacity+5)
		if got := collect(t, d.Replay(t.Context(), "s", 0)); len(got) != events.DefaultLogCapacity+5 {
			t.Fatalf("unbounded log kept %d events", len(got))
		}
	})

	t.Run("default capacity", func(t *testing.T) {
		d := events.New()
		emit(d, events.DefaultLogCapacity+5)
		got := collect(t, d.Replay(t.Context(), "s", 0))
		if len(got) != events.DefaultLogCapacity || got[0].Type != "e5" {
			t.Fatalf("default log kept %d events starting at %q, want %d from e5", len(got), got[0].Type, events.DefaultLogCapacity)
		}
	})

	t.Run("backing log wins over WithLogCapacity", func(t *testing.T) {
		log := events.NewLogWithCapacity(2)
		d := events.New(events.WithBacking(log), events.WithLogCapacity(100))
		emit(d, 4)
		if got := types(collect(t, d.Replay(t.Context(), "s", 0))); got != "e2,e3" {
			t.Fatalf("backing log: %q, want e2,e3", got)
		}
	})
}
