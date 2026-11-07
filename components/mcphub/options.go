package mcphub

import (
	"log/slog"
	"net/http"

	"github.com/Phong95/aec-agent-go/components/events"
	"github.com/Phong95/aec-agent-go/components/mcphub/mcpoauth"
	"github.com/Phong95/aec-agent-go/components/persistence"
	"github.com/Phong95/aec-agent-go/internal/nilcheck"
)

// Option configures an McpHub.
type Option func(*McpHub)

// WithClock sets the timestamp source in epoch milliseconds (default kernel.NowMillis),
// for the CreatedAt and UpdatedAt of custom servers. A nil clock is ignored.
func WithClock(now func() int64) Option {
	return func(h *McpHub) {
		if now != nil {
			h.now = now
		}
	}
}

// WithLogger sets the logger that receives the warnings about stored servers that are
// ignored (default slog.Default()). A nil logger is ignored.
func WithLogger(l *slog.Logger) Option {
	return func(h *McpHub) {
		if l != nil {
			h.logger = l
		}
	}
}

// WithEvents makes Create, Update and Delete publish events.McpServerChanged (SessionID
// "") on b. A nil bus is ignored (no event).
func WithEvents(b events.Bus) Option {
	return func(h *McpHub) {
		if b != nil {
			h.events = b
		}
	}
}

// WithAuthStore sets where the OAuth sign-ins are kept (A-214); getStore is resolved on
// every load, so AgentCore.UsePersistence can swap the store. A nil function is ignored
// (in memory).
func WithAuthStore(getStore func() persistence.McpAuthStore) Option {
	return func(h *McpHub) {
		if getStore != nil {
			h.authStore = getStore
		}
	}
}

// WithSealer seals the stored sign-ins (A-214; the SecretHub). nil = plain JSON.
func WithSealer(s mcpoauth.Sealer) Option {
	return func(h *McpHub) {
		if !nilcheck.IsNil(s) {
			h.sealer = s
		}
	}
}

// WithHTTPClient sets the HTTP client of the OAuth requests (A-214; default
// http.DefaultClient). A nil client is ignored.
func WithHTTPClient(c *http.Client) Option {
	return func(h *McpHub) {
		if c != nil {
			h.httpClient = c
		}
	}
}
