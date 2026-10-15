package subagenthub

import (
	"log/slog"

	"github.com/Phong95/aec-agent-go/components/events"
)

// Option configures a SubagentHub.
type Option func(*SubagentHub)

// WithClock sets the timestamp source in epoch milliseconds (default kernel.NowMillis),
// for the CreatedAt and UpdatedAt of custom types. A nil clock is ignored.
func WithClock(now func() int64) Option {
	return func(h *SubagentHub) {
		if now != nil {
			h.now = now
		}
	}
}

// WithLogger sets the logger that receives the warnings about stored types that are
// ignored (default slog.Default()). A nil logger is ignored.
func WithLogger(l *slog.Logger) Option {
	return func(h *SubagentHub) {
		if l != nil {
			h.logger = l
		}
	}
}

// WithEvents makes Delete publish events.SubagentTypeDeleted (SessionID "") on b once
// the type is removed. A nil bus is ignored (no event).
func WithEvents(b events.Bus) Option {
	return func(h *SubagentHub) {
		if b != nil {
			h.events = b
		}
	}
}
