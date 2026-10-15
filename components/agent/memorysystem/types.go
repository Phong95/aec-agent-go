package memorysystem

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/Phong95/aec-agent-go/components/llmhub"
	"github.com/Phong95/aec-agent-go/kernel"
)

// Model is the one thing the memory pipeline needs from the agent's model access: a
// non-streaming chat call. The agent's ModelManager satisfies it. The pipeline passes
// the zero llmhub.ModelOptions (the call carries the messages only), so the manager's
// defaults apply.
type Model interface {
	Chat(ctx context.Context, msgs []kernel.Message, opts llmhub.ModelOptions) (kernel.ModelResponse, error)
}

// options is the resolved configuration of a pipeline.
type options struct {
	now    func() time.Time
	nextID func() string
	logger *slog.Logger
	loc    *time.Location
	// passTimeout bounds one background consolidation pass (see WithPassTimeout).
	passTimeout time.Duration
}

// Option configures a pipeline component.
type Option func(*options)

// WithClock sets the clock the pipeline reads for every timestamp (default time.Now).
// Tests pin it; the fixture recorder pins its clock the same way.
func WithClock(now func() time.Time) Option {
	return func(o *options) {
		if now != nil {
			o.now = now
		}
	}
}

// WithIDs sets the function that makes record ids (default mem_<unix ms>_<n>_<tag>: the
// format mem_<unix ms>_<n> with a package-level counter, and a tag of 16 lowercase hex
// digits minted once per process, so processes that share a store never mint the same
// id).
func WithIDs(next func() string) Option {
	return func(o *options) {
		if next != nil {
			o.nextID = next
		}
	}
}

// WithLogger sets the logger (default slog.Default()). Log lines carry ids only,
// never a memory value or message text.
func WithLogger(l *slog.Logger) Option {
	return func(o *options) {
		if l != nil {
			o.logger = l
		}
	}
}

// WithLocation sets the location in which a date string without an offset is read
// (JS local time, default time.Local). It only matters for the legacy forms that a
// stored expiresAt could hold, and tests pin UTC.
func WithLocation(loc *time.Location) Option {
	return func(o *options) {
		if loc != nil {
			o.loc = loc
		}
	}
}

// DefaultPassTimeout is how long one background consolidation pass may run before its
// context is cancelled (see WithPassTimeout).
const DefaultPassTimeout = 5 * time.Minute

// WithPassTimeout bounds every consolidation pass the scheduler runs in the background
// (default DefaultPassTimeout): the pass gets a context that ends after d, so a model
// or a store that hangs cannot keep the scheduler busy for ever. A pass that runs out
// of time fails like any failed pass: it is logged and its session stays dirty, so the
// next tick retries it. d <= 0 removes the bound (the pass then runs under the lifetime
// context only). A direct Consolidate call is bounded by its own ctx, not by this
// option.
func WithPassTimeout(d time.Duration) Option {
	return func(o *options) {
		o.passTimeout = d
	}
}

// newOptions applies opts over the defaults. The default id function reads the
// configured clock, but ids are reproducible only with WithIDs: WithClock fixes the
// millisecond part only, while the package-level counter and the per-process tag
// (see processTag) still vary between runs.
func newOptions(opts ...Option) options {
	o := options{now: time.Now, logger: slog.Default(), loc: time.Local, passTimeout: DefaultPassTimeout}
	for _, opt := range opts {
		opt(&o)
	}
	if o.nextID == nil {
		o.nextID = defaultIDs(o.now)
	}
	return o
}

// deps is what every pipeline component of one agent shares.
type deps struct {
	// agentID is the owning agent; every record the pipeline writes carries it and
	// every read is filtered by it.
	agentID string
	opts    options
	// writeMu serializes the store writes of this agent's pipeline: the extraction
	// write section, the expiry pass and the recall stamps. It is never held across
	// a model call.
	writeMu *sync.Mutex
}
