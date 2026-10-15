package events

import (
	"context"
	"iter"
	"log/slog"
	"sync"
	"sync/atomic"

	"github.com/Phong95/aec-agent-go/internal/redact"
	"github.com/Phong95/aec-agent-go/kernel"
)

// subscription is one registered handler. active is cleared by Unsubscribe so
// an Emit that already took its snapshot skips a handler removed in flight.
type subscription struct {
	h      Handler
	hc     CtxHandler // set instead of h by OnCtx
	active atomic.Bool
}

// Dispatcher is the in-memory Bus.
//
// Pub-sub event to decouple between services (like MediatR in .Net).
//
// Two delivery rules:
//
//   - Panic isolation (KRN-03): each handler call is wrapped in a recover that
//     reports the event type and the type of the panic (through internal/redact)
//     through log/slog and then continues with the next handler, so a panicking
//     handler does not stop delivery to the others. The payload is never logged.
//   - Snapshot delivery: handlers added during an Emit are first called on the
//     next Emit, and the same function registered twice is called twice because
//     Go cannot compare funcs. A handler removed during an Emit before its turn
//     is skipped.
type Dispatcher struct {
	mu      sync.Mutex
	subs    map[string][]*subscription // ordered per type; a map is only used for lookup, never iterated
	anySubs []*subscription            // OnAny handlers, called after the handlers of the type
	log     *Log
	logCap  int // capacity of the own log; unused with WithBacking
	logger  *slog.Logger
	now     func() int64
	newID   func() string
}

// Option configures a Dispatcher.
type Option func(*Dispatcher)

// WithBacking makes the Dispatcher append to a shared Log (nil is ignored). The
// capacity is then the one of that Log, and WithLogCapacity has no effect.
func WithBacking(l *Log) Option {
	return func(d *Dispatcher) {
		if l != nil {
			d.log = l
		}
	}
}

// WithLogCapacity sets how many events the Dispatcher's own log keeps (default
// DefaultLogCapacity). Once the log is full the oldest event is dropped, so Replay
// yields only the last n events. n == 0 disables the log (Replay yields nothing; the
// handlers still receive every event) and a negative n keeps every event.
// It has no effect together with WithBacking (see Log).
func WithLogCapacity(n int) Option {
	return func(d *Dispatcher) {
		d.logCap = n
	}
}

// WithLogger sets the logger used to report handler panics (default slog.Default()).
func WithLogger(l *slog.Logger) Option {
	return func(d *Dispatcher) {
		if l != nil {
			d.logger = l
		}
	}
}

// WithClock sets the timestamp source in epoch ms (default kernel.NowMillis).
func WithClock(now func() int64) Option {
	return func(d *Dispatcher) {
		if now != nil {
			d.now = now
		}
	}
}

// WithIDFunc sets the event ID source (default kernel.NewID).
func WithIDFunc(f func() string) Option {
	return func(d *Dispatcher) {
		if f != nil {
			d.newID = f
		}
	}
}

// New returns an empty Dispatcher.
func New(opts ...Option) *Dispatcher {
	d := &Dispatcher{
		subs:   make(map[string][]*subscription),
		logCap: DefaultLogCapacity,
		logger: slog.Default(),
		now:    kernel.NowMillis,
		newID:  kernel.NewID,
	}
	for _, o := range opts {
		o(d)
	}
	if d.log == nil {
		d.log = NewLogWithCapacity(d.logCap)
	}
	return d
}

// Name returns the plugin name.
func (d *Dispatcher) Name() string { return "event-dispatcher" }

// Version returns the plugin version.
func (d *Dispatcher) Version() string { return "1.0.0" }

// Logger returns the logger the dispatcher reports problems through (see
// WithLogger). Subscribe uses it for payloads it cannot decode.
func (d *Dispatcher) Logger() *slog.Logger { return d.logger }

// Initialize does nothing. Dispose drops every subscriber (see Dispose).
func (d *Dispatcher) Initialize(context.Context) error { return nil }

// Dispose drops every subscriber, typed and OnAny, so later Emits reach nobody. A
// handler of an Emit already in flight that has not had its turn is skipped. The log
// is kept: a shared backing Log must stay replayable.
func (d *Dispatcher) Dispose(context.Context) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, list := range d.subs {
		for _, s := range list {
			s.active.Store(false)
		}
	}
	for _, s := range d.anySubs {
		s.active.Store(false)
	}
	d.subs = make(map[string][]*subscription)
	d.anySubs = nil
	return nil
}

// Emit stores e with a fresh ID and Timestamp and delivers it to the handlers
// registered for its type, in registration order, synchronously. Handlers run
// outside the lock, so they may call Emit and On. They run on the caller's
// goroutine and Emit returns only when the last one has returned, so a handler
// must not block (see Handler).
func (d *Dispatcher) Emit(e kernel.Event) kernel.Event {
	return d.deliver(context.Background(), e)
}

// Publish is Emit with a context for the CtxHandlers (see Bus.Publish). Like Emit
// it returns only when the last handler has returned.
func (d *Dispatcher) Publish(ctx context.Context, e kernel.Event) kernel.Event {
	return d.deliver(ctx, e)
}

// deliver stores e and calls its handlers, giving ctx to the CtxHandlers.
func (d *Dispatcher) deliver(ctx context.Context, e kernel.Event) kernel.Event {
	d.mu.Lock()
	// The ID is drawn before the timestamp. Both run under the lock so custom
	// (non-thread-safe) clock and ID functions are safe and log order equals
	// timestamp order.
	e.ID = d.newID()
	e.Timestamp = d.now()
	d.log.append(e)
	snapshot := append([]*subscription(nil), d.subs[e.Type]...)
	snapshot = append(snapshot, d.anySubs...)
	d.mu.Unlock()

	for _, s := range snapshot {
		if s.active.Load() {
			d.call(ctx, s, e)
		}
	}
	return e
}

// call runs one handler and converts a panic into a log record. Only the event
// type and the type of the panic (through internal/redact) are logged, never the
// panic value or the payload.
func (d *Dispatcher) call(ctx context.Context, s *subscription, e kernel.Event) {
	defer func() {
		if r := recover(); r != nil {
			d.logger.Error("event handler panicked", "type", e.Type, "panic", redact.Panic(r))
		}
	}()
	if s.hc != nil {
		s.hc(ctx, e)
		return
	}
	s.h(e)
}

// On registers h for eventType. The same function may be registered more than
// once and is then called once per registration. h runs synchronously on the
// goroutine of every Emit of that type, so it must not block (see Handler).
func (d *Dispatcher) On(eventType string, h Handler) Unsubscribe {
	return d.subscribe(eventType, &subscription{h: h})
}

// OnCtx registers h for eventType with the delivery's context (see CtxHandler). It
// shares the per-type order with On: handlers run in registration order.
func (d *Dispatcher) OnCtx(eventType string, h CtxHandler) Unsubscribe {
	return d.subscribe(eventType, &subscription{hc: h})
}

// subscribe adds s to the handlers of eventType and returns its Unsubscribe.
func (d *Dispatcher) subscribe(eventType string, s *subscription) Unsubscribe {
	s.active.Store(true)

	d.mu.Lock()
	d.subs[eventType] = append(d.subs[eventType], s)
	d.mu.Unlock()

	return func() {
		s.active.Store(false)
		d.mu.Lock()
		defer d.mu.Unlock()
		list := d.subs[eventType]
		for i, x := range list {
			if x == s {
				// Build a new slice: an in-flight Emit may hold the old backing array.
				d.subs[eventType] = append(append([]*subscription(nil), list[:i]...), list[i+1:]...)
				return
			}
		}
	}
}

// OnAny registers h for every event, whatever its type. It is meant for hosts
// (streaming, audit, metrics). h runs after the handlers registered for the event's
// type, synchronously on the goroutine of every Emit, so it must not block (see
// Handler).
func (d *Dispatcher) OnAny(h Handler) Unsubscribe {
	s := &subscription{h: h}
	s.active.Store(true)

	d.mu.Lock()
	d.anySubs = append(d.anySubs, s)
	d.mu.Unlock()

	return func() {
		s.active.Store(false)
		d.mu.Lock()
		defer d.mu.Unlock()
		for i, x := range d.anySubs {
			if x == s {
				// Build a new slice: an in-flight Emit may hold the old backing array.
				d.anySubs = append(append([]*subscription(nil), d.anySubs[:i]...), d.anySubs[i+1:]...)
				return
			}
		}
	}
}

// Replay yields the events of sessionID in emission order, among those that are
// still in the log (see WithLogCapacity). See Bus.Replay.
func (d *Dispatcher) Replay(ctx context.Context, sessionID string, fromTimestamp int64) iter.Seq2[kernel.Event, error] {
	return func(yield func(kernel.Event, error) bool) {
		for _, e := range d.log.snapshot() {
			if e.SessionID != sessionID {
				continue
			}
			if fromTimestamp != 0 && e.Timestamp < fromTimestamp {
				continue
			}
			if err := ctx.Err(); err != nil {
				yield(kernel.Event{}, err)
				return
			}
			if !yield(e, nil) {
				return
			}
		}
	}
}

var (
	_ Bus                = (*Dispatcher)(nil)
	_ kernel.Initializer = (*Dispatcher)(nil)
)
