// Package events is the typed event system: the Bus contract, the in-memory
// Dispatcher, the 22 SDK event kinds with their payload structs, and the
// generic Emit, Subscribe and PayloadAs helpers.
//
// Event bus - append-only event stream. The Bus interface itself is
// non-generic (Go interface methods cannot have type parameters). Type safety
// comes from Kind[P]: a Kind ties an event type string to its payload struct,
// and the free functions Emit, Subscribe and PayloadAs convert between the
// untyped kernel.Event and the typed payload.
package events

import (
	"context"
	"iter"
	"sync"

	"github.com/Phong95/aec-agent-go/kernel"
)

// Handler receives an event of the type it was registered for. Handlers run
// synchronously inside Emit, on the goroutine of the emitter: a handler that
// blocks (I/O, a lock, a channel send nobody reads) blocks that Emit, and with it
// the agent loop, the scheduled job or the tool that emitted. A handler must
// therefore return quickly; one that has slow work to do hands the event to a
// goroutine or a buffered channel of its own.
type Handler func(kernel.Event)

// CtxHandler is a Handler that also receives the context of the delivery: the ctx
// passed to Publish, or context.Background() for an Emit. It is for subscribers
// that do I/O on the emitter's behalf (releasing an agent's data, say) and must
// honour its cancellation and deadline. Like a Handler it runs synchronously on the
// emitter's goroutine, and the emitter waits for it to return.
type CtxHandler func(ctx context.Context, e kernel.Event)

// Unsubscribe removes a subscription. It is idempotent.
type Unsubscribe func()

// Bus is the append-only event stream.
type Bus interface {
	kernel.Plugin

	// Emit assigns the event a fresh ID and Timestamp (any values in e are
	// overwritten), appends it to the log, delivers it to the handlers of its
	// type and returns the stored event.
	Emit(e kernel.Event) kernel.Event

	// Publish is Emit with a context: it stores and delivers e the same way, and
	// hands ctx to the CtxHandlers (OnCtx). It returns once every handler has
	// returned. A ctx that is already done does not stop the delivery; the
	// handlers see ctx.Err() and decide.
	Publish(ctx context.Context, e kernel.Event) kernel.Event

	// On registers h for events of eventType.
	On(eventType string, h Handler) Unsubscribe

	// OnCtx registers h for events of eventType; h receives the delivery's context
	// (see CtxHandler). On and OnCtx handlers of one type run in registration order.
	OnCtx(eventType string, h CtxHandler) Unsubscribe

	// OnAny registers h for every event, whatever its type (hosts: streaming,
	// audit, metrics).
	OnAny(h Handler) Unsubscribe

	// Replay yields the events of one session in emission order. A bus may
	// keep a bounded log (the Dispatcher does, see Log); then only the events
	// that are still in it are yielded. A
	// fromTimestamp of 0 means no filter (mirroring the JS falsy check); an
	// event whose timestamp equals fromTimestamp is kept. It is an iter.Seq2 so
	// a durable bus can report read failures.
	Replay(ctx context.Context, sessionID string, fromTimestamp int64) iter.Seq2[kernel.Event, error]
}

// Log is a mutex-guarded append-only event log. Sharing one Log between
// several Dispatchers (WithBacking) models durable storage for the durability
// conformance suite.
//
// The log is a bounded ring buffer. Once it holds its capacity of events, each new
// event overwrites the oldest one, so Replay yields only the events that are still
// in the buffer. A capacity of 0 keeps no event (Replay yields nothing, the handlers
// still receive every event), and a negative capacity keeps every event.
type Log struct {
	mu       sync.Mutex
	capacity int
	events   []kernel.Event
	// head is the index of the oldest event once the ring is full (len(events) ==
	// capacity); it stays 0 until then.
	head int
}

// DefaultLogCapacity is the number of events a Log keeps unless told otherwise
// (NewLog, and a Dispatcher built without WithBacking or WithLogCapacity).
const DefaultLogCapacity = 10_000

// NewLog returns an empty Log that keeps the last DefaultLogCapacity events.
func NewLog() *Log {
	return NewLogWithCapacity(DefaultLogCapacity)
}

// NewLogWithCapacity returns an empty Log that keeps the last n events. n == 0
// disables the log and a negative n keeps every event (unbounded).
func NewLogWithCapacity(n int) *Log {
	return &Log{capacity: n}
}

func (l *Log) append(e kernel.Event) {
	l.mu.Lock()
	defer l.mu.Unlock()
	switch {
	case l.capacity == 0:
		// Disabled: nothing is kept.
	case l.capacity < 0 || len(l.events) < l.capacity:
		l.events = append(l.events, e)
	default:
		l.events[l.head] = e
		l.head = (l.head + 1) % l.capacity
	}
}

// snapshot returns a copy of the log, oldest event first, so callers can iterate
// without the lock.
func (l *Log) snapshot() []kernel.Event {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]kernel.Event, 0, len(l.events))
	out = append(out, l.events[l.head:]...)
	return append(out, l.events[:l.head]...)
}

// Kind ties an event type string to the payload struct that event carries.
// Go has no generic methods, so the typed API is the free functions Emit,
// Subscribe and PayloadAs taking a Kind[P].
type Kind[P any] struct{ Name string }
