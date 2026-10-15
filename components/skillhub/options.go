package skillhub

import (
	"log/slog"

	"github.com/Phong95/aec-agent-go/components/events"
)

// Option configures a SkillHub.
type Option func(*SkillHub)

// WithClock sets the timestamp source in epoch milliseconds (default
// kernel.NowMillis). The hub formats it with kernel.MillisToISO (an ISO 8601 UTC
// timestamp with milliseconds). A nil clock is ignored.
func WithClock(now func() int64) Option {
	return func(h *SkillHub) {
		if now != nil {
			h.now = now
		}
	}
}

// WithLogger sets the logger that receives the warnings about stored skills
// that are ignored (default slog.Default()). A nil logger is ignored.
func WithLogger(l *slog.Logger) Option {
	return func(h *SkillHub) {
		if l != nil {
			h.logger = l
		}
	}
}

// WithEvents makes Delete emit events.SkillDeleted (SessionID "") on b once the skill
// is removed and the OnDeleted listeners have returned, so a subscriber sees the skill
// already disabled for every agent. A nil bus is ignored (no event).
func WithEvents(b events.Bus) Option {
	return func(h *SkillHub) {
		if b != nil {
			h.events = b
		}
	}
}
