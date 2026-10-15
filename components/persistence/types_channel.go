package persistence

import (
	"context"

	"github.com/Phong95/aec-agent-go/components/channelplatformhub/channel"
)

// ---------------------------------------------------------------------------
// Channel persistence (A-100) - same pluggable-store pattern as
// ScheduledJobStore. Connected chat-app channels survive a restart here; the live
// connections live in each agent's channel manager.
//
// Channels are agent-owned: Channel.AgentID is stamped at creation and every list
// path is filtered by agent. Reads by id return any agent's channel - the agent
// checks ownership, so stores stay simple.
//
// Channel.Credentials holds platform secrets (bot tokens): a durable store should
// encrypt them at rest.
// ---------------------------------------------------------------------------

// ChannelStore persists channels.
type ChannelStore interface {
	// Save inserts or replaces the channel by id.
	Save(ctx context.Context, c channel.Channel) error
	// Get returns the channel, or a nil pointer and a nil error when the id is unknown.
	Get(ctx context.Context, id string) (*channel.Channel, error)
	// ListByAgent returns one agent's channels, oldest created first.
	ListByAgent(ctx context.Context, agentID string) ([]channel.Channel, error)
	// Delete removes the channel.
	Delete(ctx context.Context, id string) error
	// DeleteByAgent removes every channel of one agent (the agent was deleted).
	DeleteByAgent(ctx context.Context, agentID string) error
}
