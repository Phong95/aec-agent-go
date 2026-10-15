package aec

import (
	"log/slog"

	"github.com/Phong95/aec-agent-go/components/events"
	"github.com/Phong95/aec-agent-go/components/persistence"
	"github.com/Phong95/aec-agent-go/components/trace"
	"github.com/Phong95/aec-agent-go/internal/nilcheck"
)

// coreOptions are the settings NewAgentCore reads; a nil field keeps the default.
type coreOptions struct {
	persistence *persistence.PersistenceHub
	bus         events.Bus
	trace       *trace.Service
	logger      *slog.Logger
}

// AgentCoreOption configures NewAgentCore. A nil option, and an option holding a
// nil or typed-nil value, keeps the default.
type AgentCoreOption func(*coreOptions)

// WithPersistence sets the persistence hub the AgentCore starts with (default
// persistence.NewPersistenceHub(), every store in memory). A slot of h that is nil gets the
// in-memory default (persistence.PersistenceHub.FillDefaults), so &persistence.PersistenceHub{Agents: pg}
// keeps only the agents in pg. It can be swapped later with UsePersistence.
func WithPersistence(h *persistence.PersistenceHub) AgentCoreOption {
	return func(o *coreOptions) {
		if !nilcheck.IsNil(h) {
			o.persistence = h
		}
	}
}

// WithEventBus sets the event bus every agent of the core emits to (default
// events.New()).
func WithEventBus(b events.Bus) AgentCoreOption {
	return func(o *coreOptions) {
		if !nilcheck.IsNil(b) {
			o.bus = b
		}
	}
}

// WithTrace sets the trace service the agents record spans on (default
// trace.NewService()).
func WithTrace(s *trace.Service) AgentCoreOption {
	return func(o *coreOptions) {
		if !nilcheck.IsNil(s) {
			o.trace = s
		}
	}
}

// WithLogger sets the logger of the core, its default event bus, its skill hub and
// its agents (default slog.Default()).
func WithLogger(l *slog.Logger) AgentCoreOption {
	return func(o *coreOptions) {
		if l != nil {
			o.logger = l
		}
	}
}
