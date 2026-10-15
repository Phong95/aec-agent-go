// Package conformance holds the public conformance suites that every
// implementation of a kernel contract must pass. Implementations outside this
// module, such as the stores of server/go, run the suites against their own
// types.
//
// The package consists of ordinary (non-_test) files that import "testing",
// the same technique as testing/fstest and testing/iotest. It is not aliased in
// the root package. Assertions come from the stdlib-only internal/check helper,
// so importing conformance adds no third-party dependency.
//
// Each suite is one function taking a *testing.T and a factory. A factory
// returns a fresh instance on every call, but the store suites never assume it
// is empty (the EventBus suite still expects a fresh bus). A store suite
// isolates its data with a unique prefix from kernel.NewID(), asserts that
// results contain the expected items, checks an exact count only when a filter
// is unique to the run, and never asserts in-memory-only behaviour (list order
// other than where the contract defines one, upsert of absent rows, updatedAt
// after an update, JSON key order), so server/go can run the suites against a
// shared Postgres database. The durability suites take a factory that returns
// a new instance over the same persistent data.
//
// Each suite is a single entry function covering one contract; there is no
// function that runs several suites. Every store and contract factory has the
// form func(t *testing.T) <contract>; AgentLoop takes a harness builder
// instead, and Provider, ToolHandler and ToolPolicy take extra arguments next
// to their factory (see the table). The entry functions and the factory each
// one takes:
//
//	AgentLoop(t, newHarness)             func(t, AgentLoopScenario) AgentLoopHarness
//	ArtifactStore(t, ArtifactStoreFactory)       persistence.ArtifactStore
//	EventBus(t, EventBusFactory)                 events.Bus
//	EventBusDurability(t, EventBusFactory)       events.Bus, a new one over one log
//	Provider(t, ProviderFactory, ProviderOptions{NewFailing, NewBlocking})
//	                                             llmhub.Provider
//	SandboxView(t, SandboxViewFactory)           SandboxViewFixture{View, Symlink,
//	                                             PlantFile, ...}
//	SessionStore(t, SessionStoreFactory)         persistence.SessionStore
//	SessionStoreDurability(t, SessionStoreFactory)
//	                                             a new store over one shared backing
//	SkillStore(t, SkillStoreFactory)             persistence.SkillStore
//	ToolHandler(t, ToolHandlerFactory, validArgs kernel.Arguments)
//	                                             toolhub.Handler
//	ToolPolicy(t, newPolicy, newPolicyWithDenyHook ToolPolicyFactory)
//	                                             toolhub.Policy
//	MessageStore(t, MessageStoreFactory)         persistence.MessageStore
//	MemoryStore(t, MemoryStoreFactory)           persistence.MemoryStore
//	AgentStore(t, AgentStoreFactory)             persistence.AgentStore
//	ProviderStore(t, ProviderStoreFactory)       persistence.ProviderStore
//
// That is 15 entry functions; the last 4 store suites were written for the
// Postgres stores of server/go. core/go/examples/externalconsumer is a module
// outside core/go that runs all 15 of them against stores of its own; it is the
// example of running the suites from another module.
package conformance

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/Phong95/aec-agent-go/components/events"
	"github.com/Phong95/aec-agent-go/conformance/internal/check"
	"github.com/Phong95/aec-agent-go/kernel"
)

// EventBusFactory returns an events.Bus. See the package comment for the
// freshness rules of EventBus and EventBusDurability.
type EventBusFactory func(t *testing.T) events.Bus

// replayAll drains a Replay sequence and fails the test on a yielded error.
func replayAll(t *testing.T, bus events.Bus, sessionID string, from int64) []kernel.Event {
	t.Helper()
	var out []kernel.Event
	for e, err := range bus.Replay(t.Context(), sessionID, from) {
		check.NoErr(t, err, "replay of session %s", sessionID)
		out = append(out, e)
	}
	return out
}

// emitStrictlyLater emits an event of type earlyType and then events of type
// lateType (waiting 1 ms between attempts, for at most two seconds) until one
// of them carries a timestamp strictly greater than the early one. Events are
// stamped in epoch milliseconds, so two quick Emit calls usually share a
// timestamp, and a fromTimestamp filter test on such a pair passes for a bus
// that ignores the filter. The returned late event is the first strictly later
// one; lateType events emitted before it share early's timestamp.
func emitStrictlyLater(t *testing.T, bus events.Bus, sessionID, earlyType, lateType string) (early, late kernel.Event) {
	t.Helper()
	early = bus.Emit(kernel.Event{Type: earlyType, SessionID: sessionID})
	deadline := time.Now().Add(2 * time.Second)
	for {
		time.Sleep(time.Millisecond)
		late = bus.Emit(kernel.Event{Type: lateType, SessionID: sessionID})
		if late.Timestamp > early.Timestamp {
			return early, late
		}
		if time.Now().After(deadline) {
			t.Fatalf("the bus never stamped an event later than %d within 2s; cannot test fromTimestamp filtering", early.Timestamp)
		}
	}
}

func eventTypes(es []kernel.Event) []string {
	out := make([]string, 0, len(es))
	for _, e := range es {
		out = append(out, e.Type)
	}
	return out
}

func equalStrings(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

// EventBus runs the conformance suite for events.Bus implementations. It
// verifies emit/on/replay ordering, id/timestamp generation and session
// filtering, plus the Go-only guarantees: a panicking handler does not stop
// delivery, all 22 typed events reach typed subscribers, and concurrent
// emitters keep their own order.
// Update (A-126): 20 of the recorded 22 now - the two code-job kinds were removed with
// scheduled tasks; the subtest keeps its name.
func EventBus(t *testing.T, newBus EventBusFactory) {
	t.Helper()

	// -- Lifecycle --

	t.Run("has a non-empty name and version", func(t *testing.T) {
		bus := newBus(t)
		check.True(t, bus.Name() != "", "name is empty")
		check.True(t, bus.Version() != "", "version is empty")
	})

	t.Run("initialize() resolves without error", func(t *testing.T) {
		bus := newBus(t)
		check.NoErr(t, kernel.Initialize(t.Context(), bus))
	})

	t.Run("dispose() resolves without error", func(t *testing.T) {
		bus := newBus(t)
		check.NoErr(t, kernel.Initialize(t.Context(), bus))
		check.NoErr(t, kernel.Dispose(t.Context(), bus))
	})

	// -- emit() --

	t.Run("emit() returns an Event with generated id and timestamp", func(t *testing.T) {
		bus := newBus(t)
		e := bus.Emit(kernel.Event{
			Type:      "test-event",
			SessionID: "session-1",
			Payload:   map[string]any{"data": "hello"},
		})

		check.True(t, e.ID != "", "id is empty")
		check.True(t, e.Timestamp > 0, "timestamp %d is not positive", e.Timestamp)
	})

	t.Run("emit() preserves type, sessionId, and payload", func(t *testing.T) {
		bus := newBus(t)
		e := bus.Emit(kernel.Event{
			Type:      "my-event",
			SessionID: "session-42",
			Payload:   map[string]any{"key": "value"},
		})

		check.Equal(t, e.Type, "my-event")
		check.Equal(t, e.SessionID, "session-42")
		payload, ok := e.Payload.(map[string]any)
		check.True(t, ok, "payload is %T, want map[string]any", e.Payload)
		check.Equal(t, payload["key"], any("value"))
	})

	t.Run("emit() generates unique ids for different events", func(t *testing.T) {
		bus := newBus(t)
		e1 := bus.Emit(kernel.Event{Type: "a", SessionID: "s"})
		e2 := bus.Emit(kernel.Event{Type: "b", SessionID: "s"})
		check.NotEqual(t, e1.ID, e2.ID)
	})

	// -- on() --

	t.Run("on() fires handler when matching event is emitted", func(t *testing.T) {
		bus := newBus(t)
		var received []kernel.Event
		bus.On("test-event", func(e kernel.Event) { received = append(received, e) })

		bus.Emit(kernel.Event{Type: "test-event", SessionID: "s", Payload: "data"})

		check.Len(t, received, 1)
		if len(received) == 1 {
			check.Equal(t, received[0].Type, "test-event")
		}
	})

	t.Run("on() does not fire handler for non-matching event type", func(t *testing.T) {
		bus := newBus(t)
		var received []kernel.Event
		bus.On("type-A", func(e kernel.Event) { received = append(received, e) })

		bus.Emit(kernel.Event{Type: "type-B", SessionID: "s"})

		check.Len(t, received, 0)
	})

	t.Run("on() returns an unsubscribe function that stops delivery", func(t *testing.T) {
		bus := newBus(t)
		var received []kernel.Event
		unsub := bus.On("test", func(e kernel.Event) { received = append(received, e) })

		bus.Emit(kernel.Event{Type: "test", SessionID: "s"})
		check.Len(t, received, 1)

		unsub()
		bus.Emit(kernel.Event{Type: "test", SessionID: "s"})
		check.Len(t, received, 1)
	})

	t.Run("on() supports multiple handlers for the same type", func(t *testing.T) {
		bus := newBus(t)
		var received1, received2 []kernel.Event
		bus.On("test", func(e kernel.Event) { received1 = append(received1, e) })
		bus.On("test", func(e kernel.Event) { received2 = append(received2, e) })

		bus.Emit(kernel.Event{Type: "test", SessionID: "s"})

		check.Len(t, received1, 1)
		check.Len(t, received2, 1)
	})

	// -- publish() / onCtx() --

	t.Run("publish() hands its ctx to onCtx handlers; emit() hands context.Background()", func(t *testing.T) {
		type key struct{}
		bus := newBus(t)
		var seen []any
		bus.OnCtx("x", func(ctx context.Context, _ kernel.Event) { seen = append(seen, ctx.Value(key{})) })

		bus.Publish(context.WithValue(t.Context(), key{}, "pub"), kernel.Event{Type: "x", SessionID: "s"})
		bus.Emit(kernel.Event{Type: "x", SessionID: "s"})

		check.DeepEqual(t, seen, []any{"pub", nil})
		check.Len(t, replayAll(t, bus, "s", 0), 2)
	})

	t.Run("on() and onCtx() handlers of one type run in registration order", func(t *testing.T) {
		bus := newBus(t)
		var order []string
		bus.On("x", func(kernel.Event) { order = append(order, "on-1") })
		bus.OnCtx("x", func(context.Context, kernel.Event) { order = append(order, "ctx-2") })
		bus.On("x", func(kernel.Event) { order = append(order, "on-3") })

		bus.Publish(t.Context(), kernel.Event{Type: "x", SessionID: "s"})

		check.DeepEqual(t, order, []string{"on-1", "ctx-2", "on-3"})
	})

	t.Run("publish() delivers with a done ctx; the handler sees ctx.Err()", func(t *testing.T) {
		bus := newBus(t)
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		var errs []error
		bus.OnCtx("x", func(ctx context.Context, _ kernel.Event) { errs = append(errs, ctx.Err()) })

		bus.Publish(ctx, kernel.Event{Type: "x", SessionID: "s"})

		check.Len(t, errs, 1)
		check.True(t, len(errs) == 1 && errs[0] != nil, "handler saw no ctx error")
	})

	t.Run("a panicking onCtx handler does not stop delivery", func(t *testing.T) {
		bus := newBus(t)
		var after int
		bus.OnCtx("x", func(context.Context, kernel.Event) { panic("boom") })
		bus.OnCtx("x", func(context.Context, kernel.Event) { after++ })

		bus.Publish(t.Context(), kernel.Event{Type: "x", SessionID: "s"})

		check.Equal(t, after, 1)
	})

	t.Run("onCtx() returns an unsubscribe function that stops delivery", func(t *testing.T) {
		bus := newBus(t)
		var got int
		unsub := bus.OnCtx("x", func(context.Context, kernel.Event) { got++ })

		bus.Publish(t.Context(), kernel.Event{Type: "x", SessionID: "s"})
		unsub()
		bus.Publish(t.Context(), kernel.Event{Type: "x", SessionID: "s"})

		check.Equal(t, got, 1)
	})

	// -- onAny() --

	t.Run("onAny() receives every event, after the typed handlers", func(t *testing.T) {
		bus := newBus(t)
		var order []string
		bus.On("a", func(kernel.Event) { order = append(order, "typed:a") })
		bus.OnAny(func(e kernel.Event) { order = append(order, "any:"+e.Type) })

		bus.Emit(kernel.Event{Type: "a", SessionID: "s"})
		bus.Emit(kernel.Event{Type: "b", SessionID: "s"})

		check.DeepEqual(t, order, []string{"typed:a", "any:a", "any:b"})
	})

	t.Run("onAny() returns an unsubscribe function that stops delivery", func(t *testing.T) {
		bus := newBus(t)
		var received []kernel.Event
		unsub := bus.OnAny(func(e kernel.Event) { received = append(received, e) })

		bus.Emit(kernel.Event{Type: "x", SessionID: "s"})
		unsub()
		bus.Emit(kernel.Event{Type: "y", SessionID: "s"})

		check.Len(t, received, 1)
	})

	t.Run("dispose() stops delivery and keeps the log", func(t *testing.T) {
		bus := newBus(t)
		var received []kernel.Event
		bus.On("x", func(e kernel.Event) { received = append(received, e) })
		bus.OnAny(func(e kernel.Event) { received = append(received, e) })
		bus.Emit(kernel.Event{Type: "x", SessionID: "s"})

		check.NoErr(t, kernel.Dispose(t.Context(), bus))
		bus.Emit(kernel.Event{Type: "x", SessionID: "s"})

		check.Len(t, received, 2, "a subscriber was called after dispose")
		check.Len(t, replayAll(t, bus, "s", 0), 2)
	})

	// -- replay() --

	t.Run("replay() yields events for the specified session", func(t *testing.T) {
		bus := newBus(t)
		bus.Emit(kernel.Event{Type: "a", SessionID: "session-1"})
		bus.Emit(kernel.Event{Type: "b", SessionID: "session-2"})
		bus.Emit(kernel.Event{Type: "c", SessionID: "session-1"})

		events := replayAll(t, bus, "session-1", 0)

		check.Len(t, events, 2)
		for _, e := range events {
			check.Equal(t, e.SessionID, "session-1")
		}
	})

	t.Run("replay() preserves emission order", func(t *testing.T) {
		bus := newBus(t)
		bus.Emit(kernel.Event{Type: "first", SessionID: "s1"})
		bus.Emit(kernel.Event{Type: "second", SessionID: "s1"})
		bus.Emit(kernel.Event{Type: "third", SessionID: "s1"})

		got := eventTypes(replayAll(t, bus, "s1", 0))

		check.DeepEqual(t, got, []string{"first", "second", "third"})
	})

	t.Run("replay() filters by fromTimestamp", func(t *testing.T) {
		bus := newBus(t)
		early, e2 := emitStrictlyLater(t, bus, "s1", "early", "late")
		check.True(t, early.Timestamp < e2.Timestamp, "test setup: late event must be strictly later")

		events := replayAll(t, bus, "s1", e2.Timestamp)

		for _, e := range events {
			check.True(t, e.Timestamp >= e2.Timestamp, "event %s at %d is older than %d", e.Type, e.Timestamp, e2.Timestamp)
		}
		// The event before fromTimestamp is dropped (a bus that ignores the filter fails here).
		check.True(t, !containsType(events, "early"), "the event before fromTimestamp was not filtered out")
		// An event whose timestamp equals fromTimestamp is kept.
		check.True(t, containsType(events, "late"), "the event at fromTimestamp was dropped")
	})

	t.Run("replay() returns empty for unknown session", func(t *testing.T) {
		bus := newBus(t)
		bus.Emit(kernel.Event{Type: "test", SessionID: "s1"})

		check.Len(t, replayAll(t, bus, "nonexistent", 0), 0)
	})

	// -- Cross-session isolation --

	t.Run("events from session A do not appear in session B replay", func(t *testing.T) {
		bus := newBus(t)
		bus.Emit(kernel.Event{Type: "secret", SessionID: "session-A", Payload: map[string]any{"data": "A-only"}})
		bus.Emit(kernel.Event{Type: "public", SessionID: "session-B", Payload: map[string]any{"data": "B-only"}})

		eventsB := replayAll(t, bus, "session-B", 0)

		check.Len(t, eventsB, 1)
		if len(eventsB) == 1 {
			check.Equal(t, eventsB[0].SessionID, "session-B")
			check.Equal(t, eventsB[0].Type, "public")
		}
	})

	// -- Go-only guarantees --

	t.Run("panicking handler does not stop delivery to other handlers", func(t *testing.T) {
		bus := newBus(t)
		var received []kernel.Event
		// This panic is intentional (non-test code, hence the forbidigo exclusion
		// for conformance/): it is the input that the suite feeds the bus to prove
		// that a panicking subscriber is isolated (KRN-03): the bus recovers per
		// handler, so a panicking handler does not stop delivery.
		bus.On("boom", func(kernel.Event) { panic("intentional panic from a conformance handler") })
		bus.On("boom", func(e kernel.Event) { received = append(received, e) })

		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("Emit propagated a handler panic: %v", r)
				}
			}()
			bus.Emit(kernel.Event{Type: "boom", SessionID: "s"})
		}()

		check.Len(t, received, 1, "the handler after the panicking one was not called")
		// The event is stored even though a handler panicked.
		check.Len(t, replayAll(t, bus, "s", 0), 1)
	})

	t.Run("subscriber receives all 22 typed events with type-safe payloads", func(t *testing.T) {
		bus := newBus(t)
		all22Kinds(t, bus)
	})

	t.Run("concurrent emit preserves per-goroutine order", func(t *testing.T) {
		const goroutines, perGoroutine = 8, 100
		bus := newBus(t)

		sessions := make([]string, goroutines)
		for g := range sessions {
			sessions[g] = fmt.Sprintf("conc-%d-%s", g, kernel.NewID())
		}
		var wantTypes []string
		for i := range perGoroutine {
			wantTypes = append(wantTypes, fmt.Sprintf("e%d", i))
		}

		var wg sync.WaitGroup
		for g := range goroutines {
			wg.Go(func() {
				for i := range perGoroutine {
					bus.Emit(kernel.Event{Type: fmt.Sprintf("e%d", i), SessionID: sessions[g]})
				}
			})
		}
		wg.Wait()

		for g, sid := range sessions {
			got := eventTypes(replayAll(t, bus, sid, 0))
			check.True(t, equalStrings(got, wantTypes), "goroutine %d: replay is not in its emission order (%d events)", g, len(got))
		}
	})

	t.Run("replay stops on break and on a cancelled context", func(t *testing.T) {
		bus := newBus(t)
		sid := "stop-" + kernel.NewID()
		for i := range 5 {
			bus.Emit(kernel.Event{Type: fmt.Sprintf("e%d", i), SessionID: sid})
		}

		// Breaking out of the range loop stops iteration.
		seen := 0
		for _, err := range bus.Replay(t.Context(), sid, 0) {
			check.NoErr(t, err)
			seen++
			if seen == 2 {
				break
			}
		}
		check.Equal(t, seen, 2)

		// A cancelled context yields its error and stops: nothing follows the error.
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		cancel()
		var errs []error
		afterErr := 0
		for _, err := range bus.Replay(ctx, sid, 0) {
			if err != nil {
				errs = append(errs, err)
				continue
			}
			if len(errs) > 0 {
				afterErr++
			}
		}
		check.Equal(t, len(errs), 1, "a cancelled context must yield exactly one error")
		if len(errs) == 1 {
			check.True(t, errors.Is(errs[0], context.Canceled), "error %v is not context.Canceled", errs[0])
		}
		check.Equal(t, afterErr, 0, "events were yielded after the context error")
	})
}

func containsType(es []kernel.Event, typ string) bool {
	for _, e := range es {
		if e.Type == typ {
			return true
		}
	}
	return false
}

// EventBusDurability runs the durability suite for events.Bus implementations.
//
// reopen must return a NEW instance backed by the SAME persistent storage
// each time it is called. This proves event data survives instance destruction.
//
// In-memory buses participate by sharing a backing Log across instances
// (events.WithBacking). Database-backed buses share the same connection or
// database. Each test uses its own unique session ids, so one shared backing
// can serve the whole suite.
func EventBusDurability(t *testing.T, reopen EventBusFactory) {
	t.Helper()

	t.Run("events survive dispose and recreate", func(t *testing.T) {
		sid := "dur-survive-" + kernel.NewID()
		bus1 := reopen(t)
		check.NoErr(t, kernel.Initialize(t.Context(), bus1))

		bus1.Emit(kernel.Event{Type: "event-1", SessionID: sid, Payload: map[string]any{"n": 1}})
		bus1.Emit(kernel.Event{Type: "event-2", SessionID: sid, Payload: map[string]any{"n": 2}})
		check.NoErr(t, kernel.Dispose(t.Context(), bus1))

		bus2 := reopen(t)
		check.NoErr(t, kernel.Initialize(t.Context(), bus2))

		events := replayAll(t, bus2, sid, 0)

		check.Len(t, events, 2)
		if len(events) == 2 {
			check.Equal(t, events[0].Type, "event-1")
			check.Equal(t, events[1].Type, "event-2")
		}
	})

	t.Run("replay ordering preserved across dispose and recreate", func(t *testing.T) {
		sid := "dur-order-" + kernel.NewID()
		bus1 := reopen(t)
		check.NoErr(t, kernel.Initialize(t.Context(), bus1))

		bus1.Emit(kernel.Event{Type: "first", SessionID: sid})
		bus1.Emit(kernel.Event{Type: "second", SessionID: sid})
		bus1.Emit(kernel.Event{Type: "third", SessionID: sid})
		check.NoErr(t, kernel.Dispose(t.Context(), bus1))

		bus2 := reopen(t)
		check.NoErr(t, kernel.Initialize(t.Context(), bus2))

		check.DeepEqual(t, eventTypes(replayAll(t, bus2, sid, 0)), []string{"first", "second", "third"})
	})

	t.Run("new events append after recreate", func(t *testing.T) {
		sid := "dur-append-" + kernel.NewID()
		bus1 := reopen(t)
		check.NoErr(t, kernel.Initialize(t.Context(), bus1))

		bus1.Emit(kernel.Event{Type: "before", SessionID: sid})
		check.NoErr(t, kernel.Dispose(t.Context(), bus1))

		bus2 := reopen(t)
		check.NoErr(t, kernel.Initialize(t.Context(), bus2))

		bus2.Emit(kernel.Event{Type: "after", SessionID: sid})

		events := replayAll(t, bus2, sid, 0)

		check.Len(t, events, 2)
		check.DeepEqual(t, eventTypes(events), []string{"before", "after"})
	})

	t.Run("cross-session isolation holds across dispose and recreate", func(t *testing.T) {
		sidA := "dur-iso-A-" + kernel.NewID()
		sidB := "dur-iso-B-" + kernel.NewID()
		bus1 := reopen(t)
		check.NoErr(t, kernel.Initialize(t.Context(), bus1))

		bus1.Emit(kernel.Event{Type: "a-event", SessionID: sidA})
		bus1.Emit(kernel.Event{Type: "b-event", SessionID: sidB})
		check.NoErr(t, kernel.Dispose(t.Context(), bus1))

		bus2 := reopen(t)
		check.NoErr(t, kernel.Initialize(t.Context(), bus2))

		eventsA := replayAll(t, bus2, sidA, 0)
		eventsB := replayAll(t, bus2, sidB, 0)

		check.Len(t, eventsA, 1)
		if len(eventsA) == 1 {
			check.Equal(t, eventsA[0].Type, "a-event")
		}
		check.Len(t, eventsB, 1)
		if len(eventsB) == 1 {
			check.Equal(t, eventsB[0].Type, "b-event")
		}
	})

	t.Run("fromTimestamp filtering works after recreate", func(t *testing.T) {
		sid := "dur-ts-" + kernel.NewID()
		bus1 := reopen(t)
		check.NoErr(t, kernel.Initialize(t.Context(), bus1))

		early, late := emitStrictlyLater(t, bus1, sid, "early", "late")
		check.True(t, early.Timestamp < late.Timestamp, "test setup: late event must be strictly later")
		check.NoErr(t, kernel.Dispose(t.Context(), bus1))

		bus2 := reopen(t)
		check.NoErr(t, kernel.Initialize(t.Context(), bus2))

		got := replayAll(t, bus2, sid, late.Timestamp)
		for _, e := range got {
			check.True(t, e.Timestamp >= late.Timestamp, "event %s at %d is older than %d", e.Type, e.Timestamp, late.Timestamp)
		}
		check.True(t, !containsType(got, "early"), "the event before fromTimestamp was not filtered out")
		check.True(t, containsType(got, "late"), "the event at fromTimestamp was dropped")
	})
}

// kindCase emits p as kind k through bus and requires exactly one typed delivery
// carrying an equal payload struct.
func kindCase[P any](t *testing.T, bus events.Bus, k events.Kind[P], p P) {
	t.Helper()
	var received []P
	unsub := events.Subscribe(bus, k, func(e kernel.Event, got P) {
		check.Equal(t, e.Type, k.Name)
		received = append(received, got)
	})
	defer unsub()
	events.Emit(bus, k, "kinds-"+kernel.NewID(), p)
	check.Len(t, received, 1, "kind %s: want exactly one typed delivery", k.Name)
	if len(received) == 1 {
		check.DeepEqual(t, received[0], p, "kind %s: payload differs", k.Name)
	}
}

// all22Kinds delivers one payload of every SDK event kind to a typed subscriber.
func all22Kinds(t *testing.T, bus events.Bus) {
	t.Helper()
	const agent = "agent-1"
	usage := kernel.TokenUsage{InputTokens: 100, OutputTokens: 20}
	args := kernel.Arguments(`{"path":"a.ts"}`)
	reason := "policy"

	kindCase(t, bus, events.UserMessage, events.UserMessagePayload{AgentID: agent, Role: "user", Content: "hi"})
	kindCase(t, bus, events.Iteration, events.IterationPayload{AgentID: agent, Iteration: 1, MaxIterations: 25})
	kindCase(t, bus, events.ModelRequested, events.ModelRequestedPayload{AgentID: agent, Iteration: 1, ToolCount: 3})
	kindCase(t, bus, events.ModelResponded, events.ModelRespondedPayload{AgentID: agent, StopReason: "end_turn", Usage: &usage, HasToolCalls: true})
	kindCase(t, bus, events.ToolDispatched, events.DispatchStartPayload{AgentID: agent, Name: "read_file", Arguments: args})
	kindCase(t, bus, events.ToolCompleted, events.DispatchEndPayload{AgentID: agent, Name: "read_file", IsError: true, Attempt: 2})
	kindCase(t, bus, events.ToolRetry, events.DispatchRetryPayload{AgentID: agent, Name: "read_file", Attempt: 2, Reason: "timeout"})
	kindCase(t, bus, events.ToolFailed, events.DispatchFailedPayload{AgentID: agent, Name: "read_file", Error: "boom"})
	kindCase(t, bus, events.ToolDenied, events.ToolDeniedPayload{AgentID: agent, Name: "write_file", Reason: &reason})
	kindCase(t, bus, events.ToolApprovalRequested, events.ToolApprovalRequestedPayload{AgentID: agent, Name: "write_file", ToolCallID: "call_1", Risk: "high", Arguments: args})
	kindCase(t, bus, events.ToolApprovalResolved, events.ToolApprovalResolvedPayload{AgentID: agent, Name: "write_file", ToolCallID: "call_1", Approved: true, WaitedMs: 1500})
	kindCase(t, bus, events.SkillDispatched, events.DispatchStartPayload{AgentID: agent, Name: "review", Arguments: args})
	kindCase(t, bus, events.SkillCompleted, events.DispatchEndPayload{AgentID: agent, Name: "review", Attempt: 1})
	kindCase(t, bus, events.SkillRetry, events.DispatchRetryPayload{AgentID: agent, Name: "review", Attempt: 2, Reason: "rate limited"})
	kindCase(t, bus, events.SkillFailed, events.DispatchFailedPayload{AgentID: agent, Name: "review", Error: "failed"})
	kindCase(t, bus, events.RunCompleted, events.RunCompletedPayload{AgentID: agent, Iterations: 3, TotalUsage: usage, Aborted: true})
	kindCase(t, bus, events.MaxIterationsReached, events.MaxIterationsPayload{AgentID: agent, Iterations: 25, TotalUsage: usage})
	kindCase(t, bus, events.ArtifactSaved, events.ArtifactWrittenPayload{AgentID: agent, ArtifactID: "art_1", Name: "report.md", Type: "text/markdown", Version: 1, Bytes: 2048, SandboxPath: "/work/report.md", SandboxID: "sb1"})
	kindCase(t, bus, events.ArtifactUpdated, events.ArtifactWrittenPayload{AgentID: agent, ArtifactID: "art_1", Name: "report.md", Type: "text/markdown", Version: 2, Bytes: 4096, SandboxPath: "/work/report.md", SandboxID: "sb1"})
	kindCase(t, bus, events.ArtifactDeleted, events.ArtifactDeletedPayload{AgentID: agent, ArtifactID: "art_1", Name: "report.md"})
	// Update (A-126): the code-job kinds of the recording were removed with scheduled tasks.
}
