package variablehub

import (
	"log/slog"

	"github.com/Phong95/aec-agent-go/components/events"
	"github.com/Phong95/aec-agent-go/internal/nilcheck"
)

// Option configures a VariableHub.
type Option func(*VariableHub)

// WithEvents makes the hub publish events.VariableCreated / VariableUpdated /
// VariableDeleted (SessionID "", awaited, with the call's ctx) on b. A nil bus is ignored.
func WithEvents(b events.Bus) Option {
	return func(h *VariableHub) {
		if !nilcheck.IsNil(b) {
			h.events = b
		}
	}
}

// WithSecretValueCheck sets the check that refuses a secret's value (AgentCore wires the
// SecretHub). Absent = no check. A nil check is ignored.
func WithSecretValueCheck(c SecretValueCheck) Option {
	return func(h *VariableHub) {
		if c != nil {
			h.secretCheck = c
		}
	}
}

// WithLogger sets the logger that receives the hub's warnings (default slog.Default()). A
// nil logger is ignored.
func WithLogger(l *slog.Logger) Option {
	return func(h *VariableHub) {
		if l != nil {
			h.logger = l
		}
	}
}

// WithClock sets the timestamp source in epoch milliseconds (default kernel.NowMillis), for
// CreatedAt and UpdatedAt. A nil clock is ignored.
func WithClock(now func() int64) Option {
	return func(h *VariableHub) {
		if now != nil {
			h.now = now
		}
	}
}
