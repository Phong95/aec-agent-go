package secrethub

import (
	"log/slog"

	"github.com/Phong95/aec-agent-go/components/events"
	"github.com/Phong95/aec-agent-go/internal/nilcheck"
)

// Option configures a SecretHub.
type Option func(*SecretHub)

// WithEvents makes the hub publish events.SecretCreated / SecretUpdated / SecretDeleted
// (SessionID "", awaited, with the call's ctx) and emit events.SecretUsed on b. A nil bus
// is ignored (no events).
func WithEvents(b events.Bus) Option {
	return func(h *SecretHub) {
		if !nilcheck.IsNil(b) {
			h.events = b
		}
	}
}

// WithSecretHubProvider sets the SecretHub provider new values are sealed with (default none: values
// are stored unencrypted, with one warning). UseSecretHubProvider changes it later.
func WithSecretHubProvider(p SecretHubProvider) Option {
	return func(h *SecretHub) {
		if !nilcheck.IsNil(p) {
			h.provider = p
		}
	}
}

// WithSealerOptions configures the hub's Sealer (legacy keys, data-key cache). Its Logger
// defaults to the hub's logger.
func WithSealerOptions(o SealerOptions) Option {
	return func(h *SecretHub) { h.sealerOpts = o }
}

// WithLogger sets the logger that receives the hub's warnings (default slog.Default()). A
// nil logger is ignored.
func WithLogger(l *slog.Logger) Option {
	return func(h *SecretHub) {
		if l != nil {
			h.logger = l
		}
	}
}

// WithClock sets the timestamp source in epoch milliseconds (default kernel.NowMillis),
// for CreatedAt, UpdatedAt and LastUsedAt. A nil clock is ignored.
func WithClock(now func() int64) Option {
	return func(h *SecretHub) {
		if now != nil {
			h.now = now
		}
	}
}
