// Package channelplatformhub is the channel platform hub (A-100): the chat platforms (Telegram, Slack,
// Discord, Microsoft Teams, your own app...) a host supports, shared by every agent of an
// AgentCore. Each agent's channel manager (components/agent) builds its adapters from it.
//
//	ChannelPlatformHub (AgentCore.ChannelPlatformHub)
//	  ├─ channel             the contracts: Channel, Adapter, Platform, TurnRunner, ...
//	  ├─ telegram, slack,    the opt-in platform adapters, registered with
//	  │  discord, teams      aec.AgentCore.UseChannelPlatform
//	  └─ internal/adapterkit helpers the adapters share
//
// The contracts live apart from the hub in channel, because persistence imports them.
package channelplatformhub

import (
	"errors"
	"strings"
	"sync"

	"github.com/Phong95/aec-agent-go/components/channelplatformhub/channel"
	"github.com/Phong95/aec-agent-go/kernel"
)

// Compile-time proof that ChannelPlatformHub is a plugin.
var _ kernel.Plugin = (*ChannelPlatformHub)(nil)

// ChannelPlatformHub is the chat platforms a host supports (A-100), shared by every agent of an
// AgentCore: platforms are registered once (aec.AgentCore.UseChannelPlatform) and each
// agent builds its adapters from them.
//
// It also records which bot account is connected where, so the same bot is never
// connected twice in one process (most platforms allow one listener per bot; Telegram
// answers a second poller with 409 Conflict). It is safe for concurrent use.
type ChannelPlatformHub struct {
	mu        sync.RWMutex
	platforms map[string]channel.Platform
	order     []string
	// claims maps "<platform>:<account id>" to the channel id holding it.
	claims map[string]string
}

// New returns an empty hub.
func New() *ChannelPlatformHub {
	return &ChannelPlatformHub{platforms: map[string]channel.Platform{}, claims: map[string]string{}}
}

// Name is the plugin name.
func (h *ChannelPlatformHub) Name() string { return "channel-platform-hub" }

// Version is the plugin version.
func (h *ChannelPlatformHub) Version() string { return "1.0.0" }

// Register adds a platform, replacing one of the same name. A nil platform or an empty
// name is an error.
func (h *ChannelPlatformHub) Register(p channel.Platform) error {
	if p == nil || strings.TrimSpace(p.Name()) == "" {
		return errors.New("channel: a platform needs a name")
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, ok := h.platforms[p.Name()]; !ok {
		h.order = append(h.order, p.Name())
	}
	h.platforms[p.Name()] = p
	return nil
}

// Get returns the platform registered under name.
func (h *ChannelPlatformHub) Get(name string) (channel.Platform, bool) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	p, ok := h.platforms[name]
	return p, ok
}

// List returns the platforms in registration order.
func (h *ChannelPlatformHub) List() []channel.Platform {
	h.mu.RLock()
	defer h.mu.RUnlock()
	out := make([]channel.Platform, 0, len(h.order))
	for _, name := range h.order {
		out = append(out, h.platforms[name])
	}
	return out
}

// Claim records that channelID is connected as the bot accountID. It returns the id of
// the channel already holding that bot, or "" when the claim succeeded.
func (h *ChannelPlatformHub) Claim(platform, accountID, channelID string) string {
	key := platform + ":" + accountID
	h.mu.Lock()
	defer h.mu.Unlock()
	if holder, ok := h.claims[key]; ok && holder != channelID {
		return holder
	}
	h.claims[key] = channelID
	return ""
}

// Release drops every claim held by channelID.
func (h *ChannelPlatformHub) Release(channelID string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for key, holder := range h.claims {
		if holder == channelID {
			delete(h.claims, key)
		}
	}
}
