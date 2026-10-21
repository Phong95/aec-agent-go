package agent

import (
	"cmp"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/Phong95/aec-agent-go/components/agent/command"
	"github.com/Phong95/aec-agent-go/components/agent/job"
	"github.com/Phong95/aec-agent-go/components/channelplatformhub"
	"github.com/Phong95/aec-agent-go/components/channelplatformhub/channel"
	"github.com/Phong95/aec-agent-go/components/events"
	"github.com/Phong95/aec-agent-go/components/persistence"
	"github.com/Phong95/aec-agent-go/components/toolhub"
	"github.com/Phong95/aec-agent-go/internal/jsstr"
	"github.com/Phong95/aec-agent-go/internal/redact"
	"github.com/Phong95/aec-agent-go/kernel"
	"github.com/Phong95/aec-agent-go/kernel/ordered"
)

// typingInterval is how often "typing..." is renewed during a turn: it lasts about five
// seconds on most platforms.
const typingInterval = 4 * time.Second

// maxSeenMessages is how many inbound message ids are remembered to drop duplicates.
const maxSeenMessages = 5000

// pairingAlphabet has no 0/O or 1/I, so a code read aloud or retyped is not misread.
const pairingAlphabet = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789"

const pairingCodeLength = 8

// maxListedModels is the most models /model lists.
const maxListedModels = 30

// immediateCommands are answered at once, even while a turn of the chat is running.
// A-135: /yolo too, so YOLO mode can be switched off while a reply is running.
// A-136: /permissions likewise, and the read-only /help, /context and /usage.
var immediateCommands = []string{"stop", "status", "whoami", "approve", "deny", "yolo", "permissions", "help", "context", "usage"}

// channelCommands are shown in /help and published to the platform
// (channel.CommandSetter), so the chat can suggest them. /start and /reset are
// answered too, as aliases.
var channelCommands = []channel.BotCommand{
	{Name: "new", Description: "Start a new conversation"},
	{Name: "stop", Description: "Stop the reply in progress"},
	{Name: "status", Description: "Show this conversation"},
	{Name: "model", Description: "Show or switch this conversation's model"},
	{Name: "approve", Description: "Approve a pending tool call"},
	{Name: "deny", Description: "Deny a pending tool call"},
	{Name: "yolo", Description: "Run tools without asking (on/off)"},
	{Name: "whoami", Description: "Show your id"},
	{Name: "sethome", Description: "Send job results to this chat"},
	{Name: "help", Description: "Show the commands"},
}

// chatCommands (A-136) are the commands the chat answers itself; every other one the agent
// has (its command manager: /compact, /resume, /permissions...) runs through
// Agent.Commands. /clear is the chat's /new (a chat's session is named after the chat).
var chatCommands = map[string]bool{
	"new": true, "reset": true, "clear": true, "start": true, "help": true, "stop": true,
	"approve": true, "deny": true, "whoami": true, "sethome": true,
}

// publishableName (A-136): platforms accept command names of lower-case letters, digits
// and "_" (Telegram: 1-32).
var publishableName = regexp.MustCompile(`^[a-z0-9_]{1,32}$`)

// maxPublishedCommands (A-136) is the most commands a platform takes (Telegram: 100).
const maxPublishedCommands = 100

// channelPrompt (A-136) is a command's prompt (/review, /init) a turn sends instead of
// the message's text.
type channelPrompt struct {
	text     string
	readOnly bool
}

// channelTurnKey is the context key under which the ctx of a channel turn carries the
// manager that runs it, so Agent.Dispose called from inside a turn does not wait for
// that turn (it would wait for itself).
type channelTurnKey struct{}

// channelManagerDeps is what a channelManager needs.
type channelManagerDeps struct {
	agentID  string
	bus      events.Bus
	store    persistence.ChannelStore
	sessions persistence.SessionStore
	// platformHub is the host's channel platform hub: its platforms and bot claims (nil when none is registered).
	platformHub *channelplatformhub.ChannelPlatformHub
	// runner is resolved on every turn: the host's runner, or the agent's default one.
	runner func() channel.TurnRunner
	// agentName is used in replies such as /status.
	agentName func() string
	// listModels returns the models /model offers; ok=false when the provider has no
	// catalog (any name is accepted then). Nil means no catalog.
	listModels func(ctx context.Context) (models []string, ok bool)
	// defaultModel is the agent's own model, used by sessions that set none.
	defaultModel func() string
	// appendMessage (phase 3) adds a message to a session between its turns (the
	// conversation manager's). A scheduled job's result posted in a chat is added to that
	// chat's session with it, so a reply in the chat ("tell me more") has it. Nil means
	// results are only sent.
	appendMessage func(ctx context.Context, sessionID string, m kernel.Message) error
	// approvalMode and setApprovalMode (A-135) are a session's approval mode and its
	// setter (the conversation manager's), for /yolo and /status. Nil means /yolo answers
	// that it is not available.
	approvalMode    func(ctx context.Context, sessionID string) (kernel.ApprovalMode, error)
	setApprovalMode func(ctx context.Context, sessionID string, mode kernel.ApprovalMode) error
	// commands (A-136) returns the agent's slash commands, for every command the chat does
	// not answer itself. Nil (or a nil result) means only the chat's own commands (and the
	// /model, /yolo and /status below).
	commands func() command.Commands
	// promptTimeout is how long an approval or question waits in the chat (default 5 min).
	promptTimeout time.Duration
	// lifetime is the agent's lifetime context; turns and inbound handling derive from it.
	lifetime context.Context
	logger   *slog.Logger
	// now is the clock; tests replace it.
	now func() time.Time
}

// liveChannel is one connected channel.
type liveChannel struct {
	// id is the channel's id (fixed, so it is read without the lock).
	id string
	// ch is the latest stored copy, so access changes apply to the next message.
	// Guarded by the manager's mu.
	ch       channel.Channel
	platform channel.Platform
	adapter  channel.Adapter
	// gen tells this connection from a later one: sink calls of a replaced adapter are
	// ignored.
	gen uint64
}

// chatRun is the running turn of a chat, for /stop.
type chatRun struct{ cancel context.CancelFunc }

// chatQueue runs the turns of one chat one after another.
type chatQueue struct {
	items   []func()
	running bool
}

// channelManager is one agent's chat-app channels (A-100), the ChannelManager of
// core/ts, like the OpenClaw / Hermes Agent gateways: it connects each enabled channel
// through its platform's adapter and turns inbound messages into agent turns.
//
//   - Access: direct messages only (groups come later). DmPairing gives an unknown
//     sender a one-time code for the owner to approve.
//     -> Phase 2: group chats too, by GroupPolicy (GroupDisabled by default), and only
//     when the bot is mentioned or replied to (RequireMention).
//   - Sessions: one per chat, found again through the session's metadata (source
//     "channel", channelId, chatKey) after a restart. /new starts a fresh one; the old
//     one stays in the history.
//   - Turns of one chat run one after another; different chats run in parallel (the
//     agent's turn gate and maxConcurrentTurns still apply).
//   - Commands: /start, /help, /new, /reset, /stop, /status, /whoami. Any other "/..."
//     text goes to the agent.
//   - Replies are plain text, split to the platform's message length.
//     -> Phase 2: Markdown rendered by the adapter when it can, streamed by editing one
//     message, with a tool-progress message (turnView).
//   - Phase 2: approvals and clarify questions of a channel turn are asked in its chat
//     (channelPrompts); interactionFor is the channel side of the agent's interaction
//     router. /approve, /deny and /model are added.
//     -> A-135: /yolo on|off switches the conversation's approval mode (owners only).
//
// The inbound handlers, chat queues and typing loops are counted in wg; close cancels
// the turns and waits for them, so nothing outlives the agent. It is safe for
// concurrent use.
type channelManager struct {
	d channelManagerDeps

	mu           sync.Mutex
	live         map[string]*liveChannel
	states       map[string]channel.State
	pairings     map[string][]channel.Pairing
	chatSessions map[string]string
	chatRuns     map[string]*chatRun
	chatQueues   map[string]*chatQueue
	// ops serializes the connect and disconnect calls of one channel.
	ops  map[string]*sync.Mutex
	seen map[string]struct{}
	// activeTurns maps a session id to the chat of the channel turn running in it
	// (interaction routing).
	activeTurns map[string]promptChat
	prompts     *channelPrompts
	// unsubscribe drops the scheduled-job subscriptions (phase 3 delivery) on close.
	unsubscribe []events.Unsubscribe
	seenLog     []string
	gen         uint64
	started     bool
	closed      bool
	wg          sync.WaitGroup
}

var _ channel.Channels = (*channelManager)(nil)

func newChannelManager(d channelManagerDeps) *channelManager {
	if d.now == nil {
		d.now = time.Now
	}
	if d.logger == nil {
		d.logger = slog.Default()
	}
	m := &channelManager{
		d: d, live: map[string]*liveChannel{}, states: map[string]channel.State{}, pairings: map[string][]channel.Pairing{},
		chatSessions: map[string]string{}, chatRuns: map[string]*chatRun{}, chatQueues: map[string]*chatQueue{},
		ops: map[string]*sync.Mutex{}, seen: map[string]struct{}{}, activeTurns: map[string]promptChat{},
	}
	m.prompts = newChannelPrompts(channelPromptsDeps{
		send: func(ctx context.Context, chat promptChat, msg channel.OutboundMessage) (channel.MessageRef, bool) {
			m.mu.Lock()
			lc := m.live[chat.channelID]
			m.mu.Unlock()
			if lc == nil {
				return channel.MessageRef{}, false
			}
			ref, err := lc.adapter.Send(ctx, chat.target, msg)
			if err != nil {
				m.d.logger.Warn("[channels] could not ask in the chat", "agent", m.d.agentID, "channel", chat.channelID, "error", err)
				return channel.MessageRef{}, false
			}
			return ref, true
		},
		edit: func(chat promptChat, ref channel.MessageRef, msg channel.OutboundMessage) {
			m.mu.Lock()
			lc := m.live[chat.channelID]
			m.mu.Unlock()
			if lc == nil {
				return
			}
			if ed, ok := lc.adapter.(channel.Editor); ok {
				_ = ed.Edit(m.d.lifetime, ref, msg)
			}
		},
		async: m.goTracked,
		buttons: func(channelID string) bool {
			m.mu.Lock()
			defer m.mu.Unlock()
			lc := m.live[channelID]
			return lc != nil && lc.adapter.Capabilities().Buttons
		},
		approvers: func(channelID string) []string {
			m.mu.Lock()
			defer m.mu.Unlock()
			if lc := m.live[channelID]; lc != nil {
				return slices.Clone(lc.ch.Access.AllowFrom)
			}
			return nil
		},
		agentName: d.agentName,
		timeout:   d.promptTimeout,
	})
	// Phase 3: a prompt job that delivers to one of this agent's chats. The scheduler only
	// publishes how the run ended; sending it is the channel side's job.
	if d.bus != nil {
		m.unsubscribe = []events.Unsubscribe{
			// A-70: a trigger run with a delivery - the trigger manager only publishes how
			// it ended.
			// Update (A-126): one event for both - job:run-finished, from the job manager.
			events.Subscribe(d.bus, events.JobRunFinished, func(e kernel.Event, p events.JobRunFinishedPayload) {
				m.onJobRunFinished(e.SessionID, p)
			}),
		}
	}
	return m
}

// interactionFor is the InteractionHandler of a session while a channel turn runs in it:
// its approvals and questions are asked in the chat. Nil for any other session (a web
// chat of the same session included): the host's handler applies.
func (m *channelManager) interactionFor(sessionID string) toolhub.InteractionHandler {
	m.mu.Lock()
	chat, ok := m.activeTurns[sessionID]
	m.mu.Unlock()
	if !ok {
		return nil
	}
	return chatInteraction{p: m.prompts, chat: chat}
}

// deliveryFor (A-100 phase 3) is the delivery of a job scheduled from a session while a
// channel turn runs in it: back to that chat, as "home" when it is the channel's home
// chat (so a later /sethome moves it along). Nil for any other session: a web chat
// schedules jobs that keep their results in their conversation.
// Update (A-127): deliveryOptions - that chat (Here) and every running channel with a
// home chat (Homes), so a job made outside the chat app (a web chat) still reports to
// the home chats, and one made in a chat can go to them all.
func (m *channelManager) deliveryOptions(sessionID string) job.DeliveryOptions {
	m.mu.Lock()
	defer m.mu.Unlock()
	return job.DeliveryOptions{Here: m.deliveryHereLocked(sessionID), Homes: m.homesLocked()}
}

// deliveryHereLocked is the chat of the channel turn running in the session, nil when
// none does. The caller holds mu.
func (m *channelManager) deliveryHereLocked(sessionID string) *job.Delivery {
	chat, ok := m.activeTurns[sessionID]
	if !ok {
		return nil
	}
	var home *channel.Home
	if lc := m.live[chat.channelID]; lc != nil {
		home = lc.ch.Home
	}
	if home != nil && home.ChatID == chat.target.ChatID && home.ThreadID == chat.target.ThreadID {
		return &job.Delivery{ChannelID: chat.channelID, Target: job.DeliveryHome, When: job.DeliverAlways}
	}
	return &job.Delivery{
		ChannelID: chat.channelID, Target: chat.target.ChatID, ThreadID: chat.target.ThreadID, When: job.DeliverAlways,
	}
}

// homesLocked are the running channels that have a home chat, by name. The caller holds
// mu.
func (m *channelManager) homesLocked() []job.HomeChat {
	var homes []job.HomeChat
	for id, lc := range m.live {
		if lc.ch.Home == nil || m.states[id].Status == channel.StatusFailed {
			continue
		}
		homes = append(homes, job.HomeChat{ChannelID: id, Name: lc.ch.Name, Platform: lc.ch.Platform})
	}
	slices.SortFunc(homes, func(a, b job.HomeChat) int {
		return cmp.Or(strings.Compare(a.Name, b.Name), strings.Compare(a.ChannelID, b.ChannelID))
	})
	return homes
}

// chatInteraction is the InteractionHandler of one channel turn.
type chatInteraction struct {
	p    *channelPrompts
	chat promptChat
}

// AskUser asks the clarify question in the chat.
func (c chatInteraction) AskUser(ctx context.Context, req toolhub.ClarifyRequest) (toolhub.ClarifyResponse, error) {
	return c.p.askUser(ctx, c.chat, req)
}

// RequestApproval asks for approval in the chat.
func (c chatInteraction) RequestApproval(ctx context.Context, req toolhub.ApprovalRequest) (toolhub.ApprovalDecision, error) {
	return c.p.requestApproval(ctx, c.chat, req)
}

// ---------------------------------------------------------------------------
// Lifecycle
// ---------------------------------------------------------------------------

// start connects the agent's stored, enabled channels. A channel that cannot connect
// shows "failed" in State; it does not stop the others. Idempotent.
func (m *channelManager) start(ctx context.Context) error {
	m.mu.Lock()
	if m.started || m.closed {
		m.mu.Unlock()
		return nil
	}
	m.started = true
	m.mu.Unlock()
	channels, err := m.d.store.ListByAgent(ctx, m.d.agentID)
	if err != nil {
		return err
	}
	var wg sync.WaitGroup
	for _, ch := range channels {
		if !ch.Enabled {
			continue
		}
		ch = ch.WithDefaults()
		wg.Go(func() {
			unlock := m.lockOps(ch.ID)
			defer unlock()
			m.connect(ctx, ch)
		})
	}
	wg.Wait()
	return nil
}

// close disconnects every channel, cancels the turns in progress and waits for the
// inbound work to end, or until ctx ends. No new turn starts afterwards. wait=false
// skips the wait (Dispose called from inside a channel turn of this agent).
func (m *channelManager) close(ctx context.Context, wait bool) error {
	m.mu.Lock()
	m.closed = true
	for _, run := range m.chatRuns {
		run.cancel()
	}
	ids := slices.Collect(maps.Keys(m.live))
	unsubscribe := m.unsubscribe
	m.unsubscribe = nil
	m.mu.Unlock()
	for _, off := range unsubscribe {
		off()
	}
	for _, id := range ids {
		unlock := m.lockOps(id)
		m.disconnect(ctx, id)
		unlock()
	}
	if !wait {
		return nil
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		m.wg.Wait()
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// ---------------------------------------------------------------------------
// channel.Channels
// ---------------------------------------------------------------------------

// List returns the agent's channels, oldest created first.
func (m *channelManager) List(ctx context.Context) ([]channel.Channel, error) {
	channels, err := m.d.store.ListByAgent(ctx, m.d.agentID)
	for i := range channels {
		channels[i] = channels[i].WithDefaults()
	}
	return channels, err
}

// Get returns the agent's channel, or nil and a nil error.
func (m *channelManager) Get(ctx context.Context, id string) (*channel.Channel, error) {
	ch, err := m.d.store.Get(ctx, id)
	if err != nil || ch == nil || ch.AgentID != m.d.agentID {
		return nil, err
	}
	normalised := ch.WithDefaults()
	return &normalised, nil
}

// Create saves a channel and, when enabled, connects it first: a channel whose
// credentials the platform rejects is never saved.
func (m *channelManager) Create(ctx context.Context, in channel.CreateInput) (channel.Channel, error) {
	if m.isClosed() {
		return channel.Channel{}, channel.NewError("the agent is stopped", channel.CodeInvalidArgument, "")
	}
	platform, err := m.requirePlatform(in.Platform)
	if err != nil {
		return channel.Channel{}, err
	}
	existing, err := m.List(ctx)
	if err != nil {
		return channel.Channel{}, err
	}
	if len(existing) >= channel.MaxChannelsPerAgent {
		return channel.Channel{}, channel.NewError(fmt.Sprintf("An agent can have at most %d channels", channel.MaxChannelsPerAgent), channel.CodeLimitReached, "")
	}
	name := ""
	if in.Name != "" {
		if name, err = validateChannelName(in.Name); err != nil {
			return channel.Channel{}, err
		}
	}
	creds, err := validateCredentials(platform, in.Credentials)
	if err != nil {
		return channel.Channel{}, err
	}
	access, err := mergeAccess(channel.DefaultAccess(), in.Access)
	if err != nil {
		return channel.Channel{}, err
	}
	nowISO := isoTime(m.d.now())
	ch := channel.Channel{
		ID: kernel.NewID(), AgentID: m.d.agentID, Platform: platform.Name(), Name: name, Credentials: creds,
		Access: access, Settings: mergeSettings(channel.DefaultSettings(), in.Settings),
		Enabled: in.Enabled == nil || *in.Enabled, CreatedAt: nowISO, UpdatedAt: nowISO,
	}

	if ch.Enabled {
		unlock := m.lockOps(ch.ID)
		account, ok := m.connect(ctx, ch)
		unlock()
		if !ok {
			m.mu.Lock()
			reason := m.states[ch.ID].Error
			delete(m.states, ch.ID)
			m.mu.Unlock()
			if reason == "" {
				reason = "unknown error"
			}
			return channel.Channel{}, channel.NewError(fmt.Sprintf("Could not connect to %s: %s", platformLabel(platform), reason), channel.CodeConnectFailed, ch.ID)
		}
		ch.Account = &account
	}
	if ch.Name == "" {
		ch.Name = platformLabel(platform)
		if ch.Account != nil && ch.Account.DisplayName != "" {
			ch.Name = ch.Account.DisplayName
		}
	}
	if err := m.d.store.Save(ctx, ch); err != nil {
		return channel.Channel{}, err
	}
	m.mu.Lock()
	if lc := m.live[ch.ID]; lc != nil {
		lc.ch = ch.Clone()
	}
	m.mu.Unlock()
	return ch, nil
}

// Update applies a partial update and reconnects or stops the channel when needed. A
// failed reconnect is reported through State, not returned: the change is saved.
func (m *channelManager) Update(ctx context.Context, id string, in channel.UpdateInput) (channel.Channel, error) {
	current, err := m.require(ctx, id)
	if err != nil {
		return channel.Channel{}, err
	}
	platform, err := m.requirePlatform(current.Platform)
	if err != nil {
		return channel.Channel{}, err
	}
	updated := current.Clone()
	credentialsChanged := false
	if in.Credentials != nil {
		merged := maps.Clone(current.Credentials)
		if merged == nil {
			merged = map[string]string{}
		}
		maps.Copy(merged, in.Credentials)
		if updated.Credentials, err = validateCredentials(platform, merged); err != nil {
			return channel.Channel{}, err
		}
		for k, v := range updated.Credentials {
			if current.Credentials[k] != v {
				credentialsChanged = true
			}
		}
	}
	if in.Name != nil {
		if updated.Name, err = validateChannelName(*in.Name); err != nil {
			return channel.Channel{}, err
		}
	}
	if in.Access != nil {
		if updated.Access, err = mergeAccess(current.Access, in.Access); err != nil {
			return channel.Channel{}, err
		}
	}
	if in.Settings != nil {
		updated.Settings = mergeSettings(current.Settings, in.Settings)
	}
	if in.Enabled != nil {
		updated.Enabled = *in.Enabled
	}
	switch {
	case in.ClearHome:
		updated.Home = nil
	case in.Home != nil:
		home, err := validateHome(*in.Home, isoTime(m.d.now()))
		if err != nil {
			return channel.Channel{}, err
		}
		updated.Home = &home
	}
	// A new bot token may be another bot: its account is read again on connect.
	if credentialsChanged {
		updated.Account = nil
	}
	updated.UpdatedAt = isoTime(m.d.now())
	if err := m.d.store.Save(ctx, updated); err != nil {
		return channel.Channel{}, err
	}

	unlock := m.lockOps(id)
	m.mu.Lock()
	lc := m.live[id]
	closed := m.closed
	m.mu.Unlock()
	switch {
	case !updated.Enabled || closed:
		if lc != nil {
			m.disconnect(ctx, id)
		}
	case lc != nil && !credentialsChanged:
		m.mu.Lock()
		lc.ch = updated.Clone()
		m.mu.Unlock()
	default:
		if lc != nil {
			m.disconnect(ctx, id)
		}
		m.connect(ctx, updated)
	}
	unlock()

	if latest, err := m.Get(ctx, id); err == nil && latest != nil {
		return *latest, nil
	}
	return updated, nil
}

// Delete disconnects and deletes the channel. Its chats' sessions stay.
func (m *channelManager) Delete(ctx context.Context, id string) error {
	if _, err := m.require(ctx, id); err != nil {
		return err
	}
	unlock := m.lockOps(id)
	m.disconnect(ctx, id)
	unlock()
	m.mu.Lock()
	delete(m.pairings, id)
	delete(m.states, id)
	for key := range m.chatSessions {
		if strings.HasPrefix(key, id+"\x00") {
			delete(m.chatSessions, key)
		}
	}
	m.mu.Unlock()
	return m.d.store.Delete(ctx, id)
}

// State returns the live connection state; StatusStopped for an unknown channel.
func (m *channelManager) State(id string) channel.State {
	m.mu.Lock()
	defer m.mu.Unlock()
	if s, ok := m.states[id]; ok {
		return s
	}
	return channel.State{Status: channel.StatusStopped, Since: isoTime(time.UnixMilli(0))}
}

// ListPairings returns the senders waiting for approval, oldest first.
func (m *channelManager) ListPairings(ctx context.Context, id string) ([]channel.Pairing, error) {
	if _, err := m.require(ctx, id); err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Clone(m.pendingPairingsLocked(id)), nil
}

// ApprovePairing adds the sender to Access.AllowFrom and tells them in their chat.
func (m *channelManager) ApprovePairing(ctx context.Context, id, code string) (channel.Channel, error) {
	ch, err := m.require(ctx, id)
	if err != nil {
		return channel.Channel{}, err
	}
	pairing, err := m.takePairing(id, code)
	if err != nil {
		return channel.Channel{}, err
	}
	if !slices.Contains(ch.Access.AllowFrom, pairing.SenderID) {
		ch.Access.AllowFrom = append(slices.Clone(ch.Access.AllowFrom), pairing.SenderID)
		ch.UpdatedAt = isoTime(m.d.now())
		if err := m.d.store.Save(ctx, ch); err != nil {
			return channel.Channel{}, err
		}
	}
	m.mu.Lock()
	lc := m.live[id]
	if lc != nil {
		lc.ch = ch.Clone()
	}
	m.mu.Unlock()
	if lc != nil {
		m.reply(ctx, lc, channel.Target{ChatID: pairing.ChatID}, fmt.Sprintf("You're approved! Send a message to start talking to %s.", m.d.agentName()))
	}
	return ch, nil
}

// RejectPairing drops the request.
func (m *channelManager) RejectPairing(ctx context.Context, id, code string) error {
	if _, err := m.require(ctx, id); err != nil {
		return err
	}
	_, err := m.takePairing(id, code)
	return err
}

// Send sends text to a chat outside a turn. The channel must be running.
func (m *channelManager) Send(ctx context.Context, id string, target channel.Target, text string) ([]channel.MessageRef, error) {
	if _, err := m.require(ctx, id); err != nil {
		return nil, err
	}
	m.mu.Lock()
	lc := m.live[id]
	failed := m.states[id].Status == channel.StatusFailed
	m.mu.Unlock()
	if lc == nil || failed {
		return nil, channel.NewError("The channel is not connected", channel.CodeNotRunning, id)
	}
	return m.sendText(ctx, lc, target, text)
}

// HandleWebhook implements channel.Channels (phase 3).
func (m *channelManager) HandleWebhook(ctx context.Context, id string, req channel.WebhookRequest) channel.WebhookResponse {
	ch, err := m.Get(ctx, id)
	if err != nil || ch == nil {
		return channel.WebhookResponse{Status: 404, Body: map[string]string{"error": "Channel not found"}}
	}
	m.mu.Lock()
	lc := m.live[id]
	failed := m.states[id].Status == channel.StatusFailed
	m.mu.Unlock()
	if lc == nil || failed {
		return channel.WebhookResponse{Status: 503, Body: map[string]string{"error": "The channel is not connected"}}
	}
	handler, ok := lc.adapter.(channel.WebhookHandler)
	if !ok {
		return channel.WebhookResponse{Status: 405, Body: map[string]string{"error": "This channel's platform takes no webhooks"}}
	}
	return handler.HandleWebhook(ctx, req)
}

// ---------------------------------------------------------------------------
// Connections
// ---------------------------------------------------------------------------

// connect builds the channel's adapter and connects it. It returns the bot account, or
// false when it failed (the reason is in State). The caller holds the channel's op lock.
func (m *channelManager) connect(ctx context.Context, ch channel.Channel) (channel.AccountInfo, bool) {
	var platform channel.Platform
	if m.d.platformHub != nil {
		platform, _ = m.d.platformHub.Get(ch.Platform)
	}
	if platform == nil {
		m.setState(ch, channel.StatusFailed, fmt.Sprintf(`No "%s" platform is registered on this host`, ch.Platform))
		return channel.AccountInfo{}, false
	}
	adapter, err := platform.New(ch.Clone())
	if err != nil {
		m.setState(ch, channel.StatusFailed, err.Error())
		return channel.AccountInfo{}, false
	}
	m.mu.Lock()
	m.gen++
	lc := &liveChannel{id: ch.ID, ch: ch.Clone(), platform: platform, adapter: adapter, gen: m.gen}
	m.live[ch.ID] = lc
	m.mu.Unlock()
	m.setState(ch, channel.StatusConnecting, "")

	account, err := adapter.Start(ctx, &channelSink{m: m, lc: lc})
	if err != nil {
		m.dropIfCurrent(lc)
		m.setState(ch, channel.StatusFailed, err.Error())
		return channel.AccountInfo{}, false
	}
	if !m.isCurrent(lc) {
		return channel.AccountInfo{}, false
	}
	if holder := m.d.platformHub.Claim(platform.Name(), account.ID, ch.ID); holder != "" {
		m.dropIfCurrent(lc)
		if err := adapter.Stop(ctx); err != nil {
			m.d.logger.Warn("[channels] could not stop a duplicate connection", "agent", m.d.agentID, "channel", ch.ID, "error", err)
		}
		m.setState(ch, channel.StatusFailed, fmt.Sprintf("This bot is already connected through another channel (%s)", holder))
		return channel.AccountInfo{}, false
	}

	// Record the account on the stored channel (a new one is not saved yet: Create saves it).
	current := ch
	if stored, err := m.d.store.Get(ctx, ch.ID); err == nil && stored != nil {
		current = stored.WithDefaults()
		if current.Account == nil || *current.Account != account {
			current.Account = &account
			if err := m.d.store.Save(ctx, current); err != nil {
				m.d.logger.Warn("[channels] could not save the bot account", "agent", m.d.agentID, "channel", ch.ID, "error", err)
			}
		}
	}
	m.mu.Lock()
	lc.ch = current.Clone()
	m.mu.Unlock()
	if m.isCurrent(lc) {
		m.setState(current, channel.StatusRunning, "")
	}
	if m.isCurrent(lc) {
		m.publishCommands(ctx, lc)
	}
	return account, true
}

// publishCommands publishes the chat commands to the platform, when the adapter can.
// A failure is logged, not returned.
func (m *channelManager) publishCommands(ctx context.Context, lc *liveChannel) {
	setter, ok := lc.adapter.(channel.CommandSetter)
	if !ok {
		return
	}
	if err := setter.SetCommands(ctx, m.botCommands()); err != nil {
		m.d.logger.Warn("[channels] could not publish the commands", "agent", m.d.agentID, "channel", lc.id, "error", err)
	}
}

// disconnect stops the channel's adapter and its chats' turns. The caller holds the
// channel's op lock.
func (m *channelManager) disconnect(ctx context.Context, id string) {
	m.mu.Lock()
	lc := m.live[id]
	delete(m.live, id)
	m.abortChatsLocked(id)
	m.mu.Unlock()
	m.prompts.cancelPrefix(id+"\x00", "the channel stopped")
	if m.d.platformHub != nil {
		m.d.platformHub.Release(id)
	}
	if lc == nil {
		return
	}
	if err := lc.adapter.Stop(ctx); err != nil {
		m.d.logger.Warn("[channels] a channel did not stop cleanly", "agent", m.d.agentID, "channel", id, "error", err)
	}
	m.mu.Lock()
	ch := lc.ch
	m.mu.Unlock()
	m.setState(ch, channel.StatusStopped, "")
}

// lockOps takes the op lock of one channel and returns its unlock.
func (m *channelManager) lockOps(id string) func() {
	m.mu.Lock()
	l := m.ops[id]
	if l == nil {
		l = &sync.Mutex{}
		m.ops[id] = l
	}
	m.mu.Unlock()
	l.Lock()
	return l.Unlock
}

func (m *channelManager) isCurrent(lc *liveChannel) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.live[lc.id] == lc
}

func (m *channelManager) dropIfCurrent(lc *liveChannel) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.live[lc.id] == lc {
		delete(m.live, lc.id)
	}
}

func (m *channelManager) setState(ch channel.Channel, status channel.Status, errText string) {
	m.mu.Lock()
	prev, ok := m.states[ch.ID]
	if ok && prev.Status == status && prev.Error == errText {
		m.mu.Unlock()
		return
	}
	m.states[ch.ID] = channel.State{Status: status, Error: errText, Since: isoTime(m.d.now())}
	m.mu.Unlock()
	if status == channel.StatusFailed || status == channel.StatusRetrying {
		m.d.logger.Warn(fmt.Sprintf("[channels] agent=%s channel=%s (%s) %s: %s", m.d.agentID, ch.ID, ch.Platform, status, errText))
	}
	events.Emit(m.d.bus, events.ChannelStatusChanged, "", events.ChannelStatusChangedPayload{
		AgentID: m.d.agentID, ChannelID: ch.ID, Platform: ch.Platform, Status: string(status), Error: errText,
	})
}

// channelSink is the Sink of one connection.
type channelSink struct {
	m  *channelManager
	lc *liveChannel
}

// Message hands an inbound message to the manager.
func (s *channelSink) Message(msg channel.InboundMessage) {
	if s.m.isCurrent(s.lc) {
		s.m.onInbound(s.lc, msg)
	}
}

// Status records the adapter's status; "failed" means the adapter stopped itself.
func (s *channelSink) Status(status channel.AdapterStatus, errText string) {
	m := s.m
	m.mu.Lock()
	if m.live[s.lc.id] != s.lc {
		m.mu.Unlock()
		return
	}
	if status == channel.AdapterFailed {
		delete(m.live, s.lc.id)
		m.abortChatsLocked(s.lc.id)
	}
	ch := s.lc.ch
	m.mu.Unlock()
	if status == channel.AdapterFailed {
		m.prompts.cancelPrefix(ch.ID+"\x00", "the channel stopped")
		if m.d.platformHub != nil {
			m.d.platformHub.Release(ch.ID)
		}
	}
	m.setState(ch, channel.Status(status), errText)
}

// Button hands a button press to the manager.
func (s *channelSink) Button(press channel.ButtonPress) {
	if s.m.isCurrent(s.lc) {
		s.m.onButton(s.lc, press)
	}
}

// ---------------------------------------------------------------------------
// Inbound messages
// ---------------------------------------------------------------------------

// onInbound filters a message and hands it to a goroutine; it returns quickly.
func (m *channelManager) onInbound(lc *liveChannel, msg channel.InboundMessage) {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return
	}
	ch := lc.ch.Clone()
	duplicate := m.isDuplicateLocked(ch.ID + "\x00" + msg.Chat.ID + "\x00" + msg.MessageID)
	m.mu.Unlock()
	// Only direct messages for now; group chats need mention rules first.
	// -> Phase 2: group chats by GroupPolicy and RequireMention; broadcast channels stay
	// ignored.
	if duplicate || msg.Sender.IsBot || msg.Chat.Type == channel.ChatChannel {
		return
	}
	target := channel.Target{ChatID: msg.Chat.ID, ThreadID: msg.Chat.ThreadID}
	cmd, isCommand := channel.ParseCommand(msg.Text)
	if isCommand && cmd.Target != "" && ch.Account != nil && ch.Account.Username != "" && !strings.EqualFold(cmd.Target, ch.Account.Username) {
		return
	}
	var command *channel.Command
	if isCommand {
		command = &cmd
	}
	if msg.Chat.Type == channel.ChatGroup {
		// /whoami answers anywhere, so the owner can learn a group's id to allow it.
		if (command == nil || command.Name != "whoami") && !groupAllowed(ch.Access, msg.Chat.ID) {
			return
		}
		// A command counts as addressed to the bot (platforms deliver commands to it).
		if ch.Access.RequireMention && command == nil && !msg.MentionsBot && !msg.ReplyToBot {
			return
		}
	}
	handle := func() { m.handleInbound(lc, ch, msg, target, command) }
	key := chatKey(ch.ID, msg)
	// The answer to a question the running turn asked: it cannot wait in the queue behind
	// that turn.
	if command == nil && mayTalk(ch.Access, msg) && m.prompts.hasQuestion(key) {
		if m.prompts.answerText(key, msg.Sender, agentText(ch, msg, false)) {
			return
		}
	}
	if command != nil && slices.Contains(immediateCommands, command.Name) {
		// Answered at once, even while a turn of the chat is running.
		m.goTracked(handle)
		return
	}
	// Everything else is handled in the chat's arrival order: queued here, before any
	// goroutine runs, so two messages of one chat cannot swap places.
	m.enqueue(key, handle)
}

// goTracked runs f in a goroutine counted in wg, unless the manager is closed.
func (m *channelManager) goTracked(f func()) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return
	}
	m.wg.Go(func() {
		defer func() {
			if r := recover(); r != nil {
				m.d.logger.Error("[channels] a message handler panicked", "agent", m.d.agentID, "panic", redact.Panic(r))
			}
		}()
		f()
	})
}

func (m *channelManager) handleInbound(lc *liveChannel, ch channel.Channel, msg channel.InboundMessage, target channel.Target, command *channel.Command) {
	ctx := m.d.lifetime
	label := platformLabel(lc.platform)

	// Answered for anyone, so a sender can tell the owner which id to allow.
	if command != nil && command.Name == "whoami" {
		group := ""
		if msg.Chat.Type == channel.ChatGroup {
			group = "\nThis group's id is " + msg.Chat.ID + "."
		}
		m.reply(ctx, lc, target, fmt.Sprintf("Your %s id is %s.%s", label, msg.Sender.ID, group))
		return
	}
	access := ch.Access
	// Group access was checked on arrival (groupAllowed); any member of an allowed group
	// may talk.
	if msg.Chat.Type == channel.ChatDM && access.DmPolicy == channel.DmDisabled {
		return
	}
	if !mayTalk(access, msg) {
		if access.DmPolicy == channel.DmAllowlist {
			m.reply(ctx, lc, target, fmt.Sprintf("This bot is private. Ask its owner to add your %s id: %s", label, msg.Sender.ID))
		} else {
			m.requestPairing(ctx, lc, ch, msg, target)
		}
		return
	}

	key := chatKey(ch.ID, msg)
	// A-136: the agent's commands (its command manager) - all but the chat's own.
	if command != nil && !chatCommands[command.Name] {
		if cmds := m.agentCommandSet(); cmds != nil {
			if info, ok := cmds.Get(command.Name, commandClient()); ok {
				m.runAgentCommand(lc, ch, msg, target, key, *command, cmds, info)
				return
			}
		}
	}
	if command != nil {
		switch command.Name {
		case "start", "help":
			m.reply(ctx, lc, target, m.helpText())
			return
		// A-136: "clear" - the command manager's /clear, done the chat's way (a session
		// named after the chat).
		case "new", "reset", "clear":
			id, err := m.createSession(ctx, ch, msg, lc.platform)
			if err != nil {
				m.d.logger.Error("[channels] could not start a new conversation", "agent", m.d.agentID, "channel", ch.ID, "error", err)
				m.reply(ctx, lc, target, "Sorry, something went wrong: "+err.Error())
				return
			}
			m.mu.Lock()
			m.chatSessions[key] = id
			m.mu.Unlock()
			m.reply(ctx, lc, target, "Started a new conversation.")
			return
		case "stop":
			m.mu.Lock()
			run := m.chatRuns[key]
			// Not running from now on, even before the turn has finished unwinding.
			if run != nil {
				delete(m.chatRuns, key)
			}
			m.mu.Unlock()
			if run != nil {
				run.cancel()
				m.reply(ctx, lc, target, "Stopped.")
			} else {
				m.reply(ctx, lc, target, "Nothing is running.")
			}
			return
		case "status":
			m.reply(ctx, lc, target, m.statusText(ctx, ch, msg, key))
			return
		case "approve", "deny":
			if notice := m.prompts.answerCommand(key, msg.Sender, command.Name == "approve", command.Args); notice != "" {
				m.reply(ctx, lc, target, notice)
			}
			return
		case "model":
			m.reply(ctx, lc, target, m.modelCommand(ctx, lc, ch, msg, key, command.Args))
			return
		case "sethome":
			m.reply(ctx, lc, target, m.setHome(ctx, ch, msg))
			return
		case "yolo":
			m.reply(ctx, lc, target, m.yoloCommand(ctx, lc, ch, msg, key, command.Args))
			return
		}
	}
	if jsstr.Trim(msg.Text) == "" {
		m.reply(ctx, lc, target, "I can only read text messages for now.")
		return
	}
	// handleInbound runs in the chat's queue here, so the turn runs in arrival order.
	m.runTurn(ch.ID, msg, target, key, nil)
}

// enqueue runs f after the earlier messages of the chat; other chats are not held up.
func (m *channelManager) enqueue(key string, f func()) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return
	}
	q := m.chatQueues[key]
	if q == nil {
		q = &chatQueue{}
		m.chatQueues[key] = q
	}
	q.items = append(q.items, f)
	if q.running {
		return
	}
	q.running = true
	m.wg.Go(func() {
		for {
			m.mu.Lock()
			if len(q.items) == 0 || m.closed {
				q.running = false
				q.items = nil
				delete(m.chatQueues, key)
				m.mu.Unlock()
				return
			}
			next := q.items[0]
			q.items = q.items[1:]
			m.mu.Unlock()
			func() {
				defer func() {
					if r := recover(); r != nil {
						m.d.logger.Error("[channels] a channel turn panicked", "agent", m.d.agentID, "panic", redact.Panic(r))
					}
				}()
				next()
			}()
		}
	})
}

// A-136: prompt is a command's prompt (/review, /init) to send instead of the message's
// text; nil sends the text.
func (m *channelManager) runTurn(channelID string, msg channel.InboundMessage, target channel.Target, key string, prompt *channelPrompt) {
	// Read again: the channel may have been stopped or edited while this message waited.
	m.mu.Lock()
	lc := m.live[channelID]
	closed := m.closed
	var ch channel.Channel
	if lc != nil {
		ch = lc.ch.Clone()
	}
	m.mu.Unlock()
	if lc == nil || closed || !mayTalk(ch.Access, msg) {
		return
	}

	sessionID, err := m.resolveSession(m.d.lifetime, ch, msg, lc.platform, key)
	if err != nil {
		m.turnFailed(lc, ch, "", msg, target, err)
		return
	}
	turnCtx, cancel := context.WithCancel(context.WithValue(m.d.lifetime, channelTurnKey{}, m))
	defer cancel()
	run := &chatRun{cancel: cancel}
	m.mu.Lock()
	m.chatRuns[key] = run
	m.mu.Unlock()
	events.Emit(m.d.bus, events.ChannelMessageReceived, sessionID, events.ChannelMessageReceivedPayload{
		AgentID: m.d.agentID, ChannelID: channelID, Platform: ch.Platform, ChatID: msg.Chat.ID, SenderID: msg.Sender.ID, MessageID: msg.MessageID,
	})

	stopTyping := m.keepTyping(turnCtx, lc, target)
	view := newTurnView(m.d.lifetime, lc.adapter, target, ch.Settings, func(err error) {
		m.d.logger.Warn("[channels] could not update the chat", "agent", m.d.agentID, "channel", channelID, "error", err)
	})
	// Approvals and questions of this turn are asked in the chat (interactionFor).
	m.mu.Lock()
	m.activeTurns[sessionID] = promptChat{channelID: channelID, key: key, target: target, chatType: msg.Chat.Type, initiator: msg.Sender}
	m.mu.Unlock()
	tc := channel.TurnContext{
		AgentID: m.d.agentID, Channel: ch, SessionID: sessionID, Message: msg, Text: agentText(ch, msg, true), OnChunk: view.chunk,
	}
	if prompt != nil {
		tc.Text, tc.ReadOnly = prompt.text, prompt.readOnly
	}
	result, err := m.callRunner(turnCtx, tc)
	stopTyping()
	m.mu.Lock()
	if m.chatRuns[key] == run {
		delete(m.chatRuns, key)
	}
	if m.activeTurns[sessionID].key == key {
		delete(m.activeTurns, sessionID)
	}
	m.mu.Unlock()
	m.prompts.cancelChat(key, "the turn ended before anyone answered")
	aborted := turnCtx.Err() != nil
	if err != nil {
		view.abandon()
		if !aborted {
			m.turnFailed(lc, ch, sessionID, msg, target, err)
		}
		return
	}
	if result.Aborted || aborted {
		view.abandon()
		return
	}
	output := jsstr.Trim(result.Output)
	if output == "" && result.StopReason != "" {
		output = "(The agent stopped without a reply: " + result.StopReason + ".)"
	}
	if output == "" {
		view.abandon()
		return
	}
	refs, err := view.finish(output)
	if err != nil {
		m.d.logger.Error(fmt.Sprintf("[channels] agent=%s channel=%s chat=%s reply failed: %v", m.d.agentID, channelID, msg.Chat.ID, err))
		events.Emit(m.d.bus, events.ChannelFailed, sessionID, events.ChannelFailedPayload{
			AgentID: m.d.agentID, ChannelID: channelID, Platform: ch.Platform, ChatID: msg.Chat.ID, Error: err.Error(),
		})
		return
	}
	events.Emit(m.d.bus, events.ChannelMessageSent, sessionID, events.ChannelMessageSentPayload{
		AgentID: m.d.agentID, ChannelID: channelID, Platform: ch.Platform, ChatID: msg.Chat.ID, Parts: len(refs),
	})
}

// turnFailed reports a failed turn: logged, emitted and told in the chat.
func (m *channelManager) turnFailed(lc *liveChannel, ch channel.Channel, sessionID string, msg channel.InboundMessage, target channel.Target, err error) {
	m.d.logger.Error(fmt.Sprintf("[channels] agent=%s channel=%s chat=%s turn failed: %v", m.d.agentID, ch.ID, msg.Chat.ID, err))
	events.Emit(m.d.bus, events.ChannelFailed, sessionID, events.ChannelFailedPayload{
		AgentID: m.d.agentID, ChannelID: ch.ID, Platform: ch.Platform, ChatID: msg.Chat.ID, Error: err.Error(),
	})
	m.reply(m.d.lifetime, lc, target, "Sorry, something went wrong: "+err.Error())
}

// callRunner calls the runner under recover.
func (m *channelManager) callRunner(ctx context.Context, tc channel.TurnContext) (result channel.TurnResult, err error) {
	defer func() {
		if r := recover(); r != nil {
			result, err = channel.TurnResult{}, errors.New("panic: "+redact.Panic(r))
		}
	}()
	runner := m.d.runner()
	if runner == nil {
		return channel.TurnResult{}, errors.New("no channel turn runner is configured")
	}
	return runner(ctx, tc)
}

// keepTyping shows "typing..." until the returned function is called.
func (m *channelManager) keepTyping(ctx context.Context, lc *liveChannel, target channel.Target) func() {
	typer, ok := lc.adapter.(channel.TypingSender)
	if !ok {
		return func() {}
	}
	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Go(func() {
		ticker := time.NewTicker(typingInterval)
		defer ticker.Stop()
		for {
			_ = typer.SendTyping(ctx, target)
			select {
			case <-done:
				return
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	})
	return func() {
		close(done)
		wg.Wait()
	}
}

// ---------------------------------------------------------------------------
// Sessions
// ---------------------------------------------------------------------------

// resolveSession returns the chat's current session: cached, else the newest one stored
// for it, else a new one.
func (m *channelManager) resolveSession(ctx context.Context, ch channel.Channel, msg channel.InboundMessage, platform channel.Platform, key string) (string, error) {
	m.mu.Lock()
	cached := m.chatSessions[key]
	m.mu.Unlock()
	if cached != "" {
		if s, err := m.d.sessions.Get(ctx, cached); err == nil && s != nil {
			return cached, nil
		}
	}
	id, err := m.findSession(ctx, ch, chatKeyOf(msg))
	if err != nil {
		return "", err
	}
	if id == "" {
		if id, err = m.createSession(ctx, ch, msg, platform); err != nil {
			return "", err
		}
	}
	m.mu.Lock()
	m.chatSessions[key] = id
	m.mu.Unlock()
	return id, nil
}

// existingSession (A-136) is the chat's current session when it has one (cached, else
// stored) - never a new one; "" when there is none.
func (m *channelManager) existingSession(ctx context.Context, ch channel.Channel, msg channel.InboundMessage, key string) (string, error) {
	m.mu.Lock()
	cached := m.chatSessions[key]
	m.mu.Unlock()
	if cached != "" {
		if s, err := m.d.sessions.Get(ctx, cached); err == nil && s != nil {
			return cached, nil
		}
	}
	return m.findSession(ctx, ch, chatKeyOf(msg))
}

// chatSession (phase 3) is the current session of a chat the agent posts to unprompted
// (a job's result): the one its messages use, else a new one, so the chat's next
// message continues it.
func (m *channelManager) chatSession(ctx context.Context, ch channel.Channel, platform channel.Platform, target channel.Target) (string, error) {
	ck := target.ChatID
	if target.ThreadID != "" {
		ck += ":" + target.ThreadID
	}
	key := ch.ID + "\x00" + ck
	m.mu.Lock()
	cached := m.chatSessions[key]
	m.mu.Unlock()
	if cached != "" {
		if s, err := m.d.sessions.Get(ctx, cached); err == nil && s != nil {
			return cached, nil
		}
	}
	id, err := m.findSession(ctx, ch, ck)
	if err != nil {
		return "", err
	}
	if id == "" {
		title := target.ChatID
		if isHomeTarget(ch.Home, target) && ch.Home.Title != "" {
			title = ch.Home.Title
		}
		meta := ordered.NewObject()
		meta.Set("agentId", m.d.agentID)
		meta.Set("source", "channel")
		meta.Set("channelId", ch.ID)
		meta.Set("platform", ch.Platform)
		meta.Set("chatKey", ck)
		meta.Set("chatId", target.ChatID)
		meta.Set("title", title+" "+midDot+" "+platformLabel(platform))
		s, err := m.d.sessions.Create(ctx, meta)
		if err != nil {
			return "", err
		}
		id = s.ID
	}
	m.mu.Lock()
	m.chatSessions[key] = id
	m.mu.Unlock()
	return id, nil
}

func (m *channelManager) findSession(ctx context.Context, ch channel.Channel, chatKey string) (string, error) {
	filter := ordered.NewObject()
	filter.Set("agentId", m.d.agentID)
	filter.Set("source", "channel")
	filter.Set("channelId", ch.ID)
	filter.Set("chatKey", chatKey)
	stored, err := m.d.sessions.List(ctx, kernel.SessionFilter{Metadata: filter})
	if err != nil {
		return "", err
	}
	// A-136: the most recently used one (was: the newest created), so a conversation
	// /resume went back to stays the chat's after a restart.
	var newest *kernel.Session
	for i := range stored {
		if newest == nil || stored[i].UpdatedAt > newest.UpdatedAt {
			newest = &stored[i]
		}
	}
	if newest == nil {
		return "", nil
	}
	return newest.ID, nil
}

func (m *channelManager) createSession(ctx context.Context, ch channel.Channel, msg channel.InboundMessage, platform channel.Platform) (string, error) {
	sender := msg.Sender.Name
	if sender == "" {
		sender = msg.Sender.ID
		if msg.Sender.Username != "" {
			sender = "@" + msg.Sender.Username
		}
	}
	meta := ordered.NewObject()
	// The same ownership key the host routes use, so the session lists and opens like
	// any other.
	meta.Set("agentId", m.d.agentID)
	meta.Set("source", "channel")
	meta.Set("channelId", ch.ID)
	meta.Set("platform", ch.Platform)
	meta.Set("chatKey", chatKeyOf(msg))
	meta.Set("chatId", msg.Chat.ID)
	meta.Set("chatType", string(msg.Chat.Type))
	meta.Set("senderId", msg.Sender.ID)
	if msg.Sender.Name != "" {
		meta.Set("senderName", msg.Sender.Name)
	}
	// A group's session is shared by its members, so it is named after the group.
	if msg.Chat.Type == channel.ChatGroup && msg.Chat.Title != "" {
		sender = msg.Chat.Title
	}
	meta.Set("title", sender+" "+midDot+" "+platformLabel(platform))
	s, err := m.d.sessions.Create(ctx, meta)
	if err != nil {
		return "", err
	}
	return s.ID, nil
}

// ---------------------------------------------------------------------------
// Pairing
// ---------------------------------------------------------------------------

func (m *channelManager) requestPairing(ctx context.Context, lc *liveChannel, ch channel.Channel, msg channel.InboundMessage, target channel.Target) {
	m.mu.Lock()
	pending := m.pendingPairingsLocked(ch.ID)
	var pairing *channel.Pairing
	for i := range pending {
		if pending[i].SenderID == msg.Sender.ID {
			pairing = &pending[i]
		}
	}
	full := pairing == nil && len(pending) >= channel.MaxPendingPairings
	created := false
	if pairing == nil && !full {
		now := m.d.now()
		p := channel.Pairing{
			Code: pairingCode(), ChannelID: ch.ID, SenderID: msg.Sender.ID, SenderName: msg.Sender.Name, ChatID: msg.Chat.ID,
			RequestedAt: isoTime(now), ExpiresAt: isoTime(now.Add(channel.PairingTTLMs * time.Millisecond)),
		}
		m.pairings[ch.ID] = append(pending, p)
		pairing = &p
		created = true
	}
	m.mu.Unlock()
	if full {
		m.reply(ctx, lc, target, "This bot is private, and too many people are waiting for approval right now. Please try again later.")
		return
	}
	if created {
		events.Emit(m.d.bus, events.ChannelPairingRequested, "", events.ChannelPairingRequestedPayload{
			AgentID: m.d.agentID, ChannelID: ch.ID, Platform: ch.Platform, Code: pairing.Code,
			SenderID: pairing.SenderID, SenderName: pairing.SenderName, ExpiresAt: pairing.ExpiresAt,
		})
	}
	m.reply(ctx, lc, target, "This bot is private. Your pairing code is "+pairing.Code+" "+emDash+" ask its owner to approve it. The code expires in an hour.")
}

// pendingPairingsLocked returns the channel's unexpired pairings and drops the expired
// ones. The caller holds mu.
func (m *channelManager) pendingPairingsLocked(channelID string) []channel.Pairing {
	nowISO := isoTime(m.d.now())
	var pending []channel.Pairing
	for _, p := range m.pairings[channelID] {
		if p.ExpiresAt > nowISO {
			pending = append(pending, p)
		}
	}
	if len(pending) > 0 {
		m.pairings[channelID] = pending
	} else {
		delete(m.pairings, channelID)
	}
	return pending
}

func (m *channelManager) takePairing(channelID, code string) (channel.Pairing, error) {
	wanted := strings.ToUpper(strings.TrimSpace(code))
	m.mu.Lock()
	defer m.mu.Unlock()
	pending := m.pendingPairingsLocked(channelID)
	for i, p := range pending {
		if p.Code == wanted {
			rest := slices.Delete(slices.Clone(pending), i, i+1)
			if len(rest) > 0 {
				m.pairings[channelID] = rest
			} else {
				delete(m.pairings, channelID)
			}
			return p, nil
		}
	}
	return channel.Pairing{}, channel.NewError(`No pending pairing with code "`+code+`"`, channel.CodeNotFound, channelID)
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// ---------------------------------------------------------------------------
// Home chat and scheduled-job delivery (phase 3)
// ---------------------------------------------------------------------------

// setHome is /sethome: it makes this chat the channel's home, where jobs delivering to
// "home" post. Only the bot's owners (AllowFrom) may: in an open DM or group anyone can
// talk, and the home chat receives what the agent produces unattended.
func (m *channelManager) setHome(ctx context.Context, ch channel.Channel, msg channel.InboundMessage) string {
	if !slices.Contains(ch.Access.AllowFrom, msg.Sender.ID) {
		return "Only the bot's owners (the senders on its allow list) can choose the home chat."
	}
	// Read again so a change saved since this channel connected is not lost.
	stored, err := m.require(ctx, ch.ID)
	if err == nil {
		sender := msg.Sender.Name
		if sender == "" {
			sender = msg.Sender.ID
			if msg.Sender.Username != "" {
				sender = "@" + msg.Sender.Username
			}
		}
		title := sender
		if msg.Chat.Type != channel.ChatDM && msg.Chat.Title != "" {
			title = msg.Chat.Title
		}
		now := isoTime(m.d.now())
		stored.Home = &channel.Home{ChatID: msg.Chat.ID, ThreadID: msg.Chat.ThreadID, Title: title, SetBy: msg.Sender.ID, SetAt: now}
		stored.UpdatedAt = now
		err = m.d.store.Save(ctx, stored)
	}
	if err != nil {
		m.d.logger.Error("[channels] could not set the home chat", "agent", m.d.agentID, "channel", ch.ID, "error", err)
		return "Sorry, something went wrong: " + err.Error()
	}
	m.mu.Lock()
	if cur := m.live[stored.ID]; cur != nil {
		cur.ch = stored.Clone()
	}
	m.mu.Unlock()
	return fmt.Sprintf(`This is now the home chat of %s: jobs that deliver to "home" post their results here.`, stored.Name)
}

// jobResultKind says what a finished job run sends to its delivery chat.
type jobResultKind int

const (
	jobResultReply jobResultKind = iota
	jobResultEmpty
	jobResultFailed
	// jobResultBlocked (A-70) is a trigger run that ended without doing its work (an
	// approval was needed, the token budget): reason, plus what the agent said (text).
	jobResultBlocked
)

// jobResult is what a finished job run sends: the reply, a "no reply" notice, or the
// error (text).
type jobResult struct {
	kind jobResultKind
	text string
	// reason is why a trigger run was blocked (jobResultBlocked only).
	reason string
	// paused (A-103): the job turned itself off after this run.
	paused bool
}

// resultOrigin is whose result it is: a scheduled job (A-100 phase 3) or a trigger
// (A-70). id is "" for a code job.
// Update (A-126): a job (of any kind).
type resultOrigin struct {
	id   string
	name string
}

// label is how logs name the origin: job "name".
func (o resultOrigin) label() string { return `job "` + o.name + `"` }

// jobResultOf is the result to deliver for a finished trigger run, or nil when there is
// nothing to send.
// Update (A-126): for any job run; a silent run ([[SILENT]]) has nothing to send.
func jobResultOf(p events.JobRunFinishedPayload, when string) *jobResult {
	if p.Silent && p.Status == "succeeded" {
		return nil
	}
	switch p.Status {
	case "suppressed", "stopped":
		// Suppressed (resolved, stale, disabled) and stopped runs did nothing worth
		// reporting.
		return nil
	case "failed", "blocked":
		// A-103: a run that paused its job is always reported, so the job never goes quiet
		// unseen.
		if when == "on-output" && !p.Paused {
			return nil
		}
		if p.Status == "failed" {
			return &jobResult{kind: jobResultFailed, text: cmpOr(p.Error, "the run failed"), paused: p.Paused}
		}
		// What the agent said about it (e.g. which approval it needed).
		return &jobResult{kind: jobResultBlocked, reason: cmpOr(p.Reason, "the run was blocked"), text: jsstr.Trim(p.Output), paused: p.Paused}
	}
	if when == "on-failure" {
		return nil
	}
	if text := jsstr.Trim(p.Output); text != "" {
		return &jobResult{kind: jobResultReply, text: text}
	}
	if when == "always" {
		return &jobResult{kind: jobResultEmpty}
	}
	return nil
}

// onJobFinished is called on the event bus when a prompt job of any agent ends; a job of
// this agent with a delivery has its result sent in the background (bus handlers must
// not block).
// onTriggerRunFinished (A-70) is called on the event bus when a trigger run of any agent
// ends; a run of this agent's trigger with a delivery has its result sent in the
// background when the delivery asks for it. Sent with the run's session, so a delivery
// failure (ChannelFailed) carries the run's depth and can't wake another trigger by
// default.
// Update (A-126): onJobRunFinished - one handler for every job run (schedule, event or
// webhook).
func (m *channelManager) onJobRunFinished(sessionID string, p events.JobRunFinishedPayload) {
	if p.AgentID != m.d.agentID || len(p.Deliveries) == 0 {
		return
	}
	origin := resultOrigin{id: p.JobID, name: cmpOr(p.JobName, "job")}
	// Update (A-127): each delivery on its own - its own "when", and one chat that fails
	// does not stop the others.
	for _, delivery := range p.Deliveries {
		res := jobResultOf(p, delivery.When)
		if res == nil {
			continue
		}
		m.goTracked(func() { m.deliver(sessionID, origin, delivery, *res) })
	}
}

// deliver sends one job result. Failures are logged and published as ChannelFailed,
// never returned.
// Update (A-70): or one trigger run's result; origin says whose it is.
func (m *channelManager) deliver(sessionID string, origin resultOrigin, d events.JobDelivery, res jobResult) {
	ctx := m.d.lifetime
	ch, err := m.Get(ctx, d.ChannelID)
	platform := ""
	if ch != nil {
		platform = ch.Platform
	}
	jobID := origin.id
	fail := func(errText, chatID string) {
		m.d.logger.Warn("[channels] could not deliver "+origin.label(), "agent", m.d.agentID, "channel", d.ChannelID, "error", errText)
		events.Emit(m.d.bus, events.ChannelFailed, sessionID, events.ChannelFailedPayload{
			AgentID: m.d.agentID, ChannelID: d.ChannelID, Platform: platform, ChatID: chatID, Error: errText, JobID: jobID,
		})
	}
	if err != nil {
		fail(err.Error(), "")
		return
	}
	if ch == nil {
		fail("this agent has no such channel", "")
		return
	}
	var target channel.Target
	if d.Target == "home" {
		if ch.Home == nil {
			fail(fmt.Sprintf(`the channel "%s" has no home chat %s send /sethome in the chat that should get the results`, ch.Name, emDash), "")
			return
		}
		target = channel.Target{ChatID: ch.Home.ChatID, ThreadID: ch.Home.ThreadID}
	} else {
		target = channel.Target{ChatID: d.Target, ThreadID: d.ThreadID}
	}
	// A channel still connecting (a catch-up run right after a restart) is waited for.
	m.lockOps(ch.ID)()
	m.mu.Lock()
	lc := m.live[ch.ID]
	m.mu.Unlock()
	if lc == nil || m.State(ch.ID).Status == channel.StatusFailed {
		fail("the channel is not connected", target.ChatID)
		return
	}
	refs, err := m.sendResult(ctx, lc, target, origin, res)
	if err != nil {
		fail(err.Error(), target.ChatID)
		return
	}
	events.Emit(m.d.bus, events.ChannelMessageSent, sessionID, events.ChannelMessageSentPayload{
		AgentID: m.d.agentID, ChannelID: ch.ID, Platform: ch.Platform, ChatID: target.ChatID, Parts: len(refs), JobID: jobID,
	})
	m.recordResult(ctx, *ch, lc.platform, target, origin, res)
}

// recordResult adds a delivered result to the chat's conversation as the agent's
// message, so the chat's next turn knows what was posted. The job's own session keeps
// the run as before. A failure is logged: the result was delivered either way.
func (m *channelManager) recordResult(ctx context.Context, ch channel.Channel, platform channel.Platform, target channel.Target, origin resultOrigin, res jobResult) {
	if m.d.appendMessage == nil {
		return
	}
	sessionID, err := m.chatSession(ctx, ch, platform, target)
	if err == nil {
		err = m.d.appendMessage(ctx, sessionID, kernel.Message{Role: kernel.RoleAssistant, Content: resultText(origin, res, true)})
	}
	if err != nil {
		m.d.logger.Warn("[channels] could not add "+origin.label()+" to the chat's conversation", "agent", m.d.agentID, "channel", ch.ID, "error", err.Error())
	}
}

// sendResult sends a job's result as chat messages: Markdown when the adapter renders
// it, split to its length.
func (m *channelManager) sendResult(ctx context.Context, lc *liveChannel, target channel.Target, origin resultOrigin, res jobResult) ([]channel.MessageRef, error) {
	caps := lc.adapter.Capabilities()
	text := resultText(origin, res, caps.Markdown)
	var parts []string
	if caps.Markdown {
		parts = channel.SplitMarkdown(text, caps.MaxMessageLength)
	} else {
		parts = channel.SplitMessage(text, caps.MaxMessageLength)
	}
	var refs []channel.MessageRef
	for _, part := range parts {
		out := channel.OutboundMessage{Text: part}
		if caps.Markdown {
			out.Format = channel.FormatMarkdown
		}
		ref, err := lc.adapter.Send(ctx, target, out)
		if err != nil {
			return refs, err
		}
		refs = append(refs, ref)
	}
	return refs, nil
}

// isHome reports whether the message's chat (and thread) is the channel's home chat.
func isHome(home *channel.Home, msg channel.InboundMessage) bool {
	return home != nil && home.ChatID == msg.Chat.ID && home.ThreadID == msg.Chat.ThreadID
}

// validateHome checks a home chat set through Update.
func validateHome(in channel.HomeInput, setAt string) (channel.Home, error) {
	chatID := jsstr.Trim(in.ChatID)
	if chatID == "" {
		return channel.Home{}, channel.NewError(`"home.chatId" is required`, channel.CodeInvalidArgument, "")
	}
	return channel.Home{ChatID: chatID, ThreadID: jsstr.Trim(in.ThreadID), Title: jsstr.Trim(in.Title), SetAt: setAt}, nil
}

func (m *channelManager) sendText(ctx context.Context, lc *liveChannel, target channel.Target, text string) ([]channel.MessageRef, error) {
	var refs []channel.MessageRef
	for _, part := range channel.SplitMessage(text, lc.adapter.Capabilities().MaxMessageLength) {
		ref, err := lc.adapter.Send(ctx, target, channel.OutboundMessage{Text: part})
		if err != nil {
			return refs, err
		}
		refs = append(refs, ref)
	}
	return refs, nil
}

// reply sends a short notice; a failure is logged, not returned.
func (m *channelManager) reply(ctx context.Context, lc *liveChannel, target channel.Target, text string) {
	if _, err := m.sendText(ctx, lc, target, text); err != nil {
		m.d.logger.Warn("[channels] could not reply", "agent", m.d.agentID, "channel", lc.id, "error", err)
	}
}

func (m *channelManager) helpText() string {
	lines := []string{"Hi! Send a message to talk to " + m.d.agentName() + ".", ""}
	for _, c := range channelCommands {
		if c.Name == "help" {
			continue
		}
		lines = append(lines, "/"+c.Name+" "+emDash+" "+strings.ToLower(c.Description[:1])+c.Description[1:])
	}
	// A-136: and the agent's commands that work in a chat.
	for _, c := range m.agentCommands() {
		lines = append(lines, commandHelpLine(c))
	}
	return strings.Join(lines, "\n")
}

// agentCommandSet (A-136) is the agent's command manager, or nil when there is none.
func (m *channelManager) agentCommandSet() command.Commands {
	if m.d.commands == nil {
		return nil
	}
	return m.d.commands()
}

// commandClient (A-136) is the client a chat runs commands in: "channel", no
// capabilities.
func commandClient() command.AvailabilityContext {
	return command.AvailabilityContext{Client: "channel"}
}

// agentCommands (A-136) is the agent's commands a chat can run (not the chat's own, none
// needing a client capability).
func (m *channelManager) agentCommands() []command.Info {
	cmds := m.agentCommandSet()
	if cmds == nil {
		return nil
	}
	var out []command.Info
	for _, c := range cmds.List(commandClient()) {
		if c.Available && !chatCommands[c.Name] {
			out = append(out, c)
		}
	}
	return out
}

// botCommands (A-136) is the commands published to the platform: the chat's own, then the
// agent's (the names a platform takes), at most maxPublishedCommands.
func (m *channelManager) botCommands() []channel.BotCommand {
	out := slices.Clone(channelCommands)
	names := map[string]bool{}
	for _, c := range channelCommands {
		names[c.Name] = true
	}
	for _, c := range m.agentCommands() {
		if publishableName.MatchString(c.Name) && !names[c.Name] {
			out = append(out, channel.BotCommand{Name: c.Name, Description: c.Description})
		}
	}
	return out[:min(len(out), maxPublishedCommands)]
}

// runAgentCommand (A-136) runs one of the agent's commands in the chat. Privileged ones
// that change something (/permissions yolo) are for the bot's owners only - they typed
// it, which counts as the confirmation the web app asks for. The result's action is
// carried out here: a session picked by /resume becomes the chat's, a prompt (/plan
// <prompt>, /init, /review) runs as a turn. It runs under the agent's lifetime, like
// handleInbound.
func (m *channelManager) runAgentCommand(lc *liveChannel, ch channel.Channel, msg channel.InboundMessage, target channel.Target, key string, cmd channel.Command, cmds command.Commands, info command.Info) {
	ctx := m.d.lifetime
	owner := slices.Contains(ch.Access.AllowFrom, msg.Sender.ID)
	if info.Safety == command.SafetyPrivileged && cmd.Args != "" && !owner {
		if info.Name == "yolo" {
			m.reply(ctx, lc, target, "Only the bot's owners (the senders on its allow list) can switch YOLO mode.")
		} else {
			m.reply(ctx, lc, target, "Only the bot's owners (the senders on its allow list) can use /"+info.Name+" to change things.")
		}
		return
	}
	// A read-only command does not start a conversation the chat does not have yet.
	var sessionID string
	if info.Safety == command.SafetyReadOnly {
		existing, err := m.existingSession(ctx, ch, msg, key)
		if err != nil {
			m.reply(ctx, lc, target, "Sorry, something went wrong: "+err.Error())
			return
		}
		if existing == "" {
			if info.Name == "status" {
				m.reply(ctx, lc, target, m.statusText(ctx, ch, msg, key))
			} else {
				m.reply(ctx, lc, target, "No conversation yet.")
			}
			return
		}
		sessionID = existing
	} else {
		id, err := m.resolveSession(ctx, ch, msg, lc.platform, key)
		if err != nil {
			m.reply(ctx, lc, target, "Sorry, something went wrong: "+err.Error())
			return
		}
		sessionID = id
	}
	text := "/" + info.Name
	if cmd.Args != "" {
		text += " " + cmd.Args
	}
	result := cmds.Run(ctx, text, command.Invocation{
		SessionID:    sessionID,
		Client:       "channel",
		Confirmed:    true,
		SessionScope: scopeOf("agentId", m.d.agentID, "source", "channel", "channelId", ch.ID, "chatKey", chatKeyOf(msg)),
	})
	reply := result.Text
	if info.Name == "status" && result.Error == "" {
		var extra []string
		if isHome(ch.Home, msg) {
			extra = append(extra, "This is the home chat: job results are sent here.")
		}
		m.mu.Lock()
		_, running := m.chatRuns[key]
		m.mu.Unlock()
		if running {
			extra = append(extra, "A reply is in progress (/stop to stop it).")
		}
		if len(extra) > 0 {
			reply += "\n" + strings.Join(extra, "\n")
		}
	}
	m.reply(ctx, lc, target, reply)
	if result.Action == nil {
		return
	}
	switch result.Action.Type {
	case command.ActionOpenSession:
		m.mu.Lock()
		m.chatSessions[key] = result.Action.SessionID
		m.mu.Unlock()
	case command.ActionSendPrompt:
		m.runTurn(ch.ID, msg, target, key, &channelPrompt{text: result.Action.Prompt, readOnly: result.Action.ReadOnly})
	case command.ActionCopy, command.ActionExit, command.ActionConfirm:
		// Nothing for a chat to do: /copy and /exit are unavailable there, and a chat's
		// command is confirmed already (an owner typed it).
	}
}

// onButton handles a button press: an approval or a question's choice.
func (m *channelManager) onButton(lc *liveChannel, press channel.ButtonPress) {
	m.mu.Lock()
	closed := m.closed
	ch := lc.ch.Clone()
	m.mu.Unlock()
	if closed || press.Sender.IsBot {
		return
	}
	msg := channel.InboundMessage{MessageID: press.MessageID, Chat: press.Chat, Sender: press.Sender}
	if !mayTalk(ch.Access, msg) {
		return
	}
	if handled, notice := m.prompts.answerButton(chatKey(ch.ID, msg), press.Sender, press.Data); handled && notice != "" {
		target := channel.Target{ChatID: press.Chat.ID, ThreadID: press.Chat.ThreadID}
		m.goTracked(func() { m.reply(m.d.lifetime, lc, target, notice) })
	}
}

// modelCommand is /model: show the conversation's model, or switch it ("/model default"
// goes back to the agent's).
func (m *channelManager) modelCommand(ctx context.Context, lc *liveChannel, ch channel.Channel, msg channel.InboundMessage, key, args string) string {
	sessionID, err := m.resolveSession(ctx, ch, msg, lc.platform, key)
	if err != nil {
		return "Sorry, something went wrong: " + err.Error()
	}
	session, err := m.d.sessions.Get(ctx, sessionID)
	if err != nil || session == nil {
		return "No conversation yet."
	}
	agentModel := ""
	if m.d.defaultModel != nil {
		agentModel = m.d.defaultModel()
	}
	var models []string
	if m.d.listModels != nil {
		if list, ok := m.d.listModels(ctx); ok {
			models = list
		}
	}

	wanted := jsstr.Trim(args)
	if wanted == "" {
		var lines []string
		if session.LLMModel != "" {
			lines = append(lines, "Model: "+session.LLMModel)
		} else {
			name := agentModel
			if name == "" {
				name = "the provider's default"
			}
			lines = append(lines, "Model: "+name+" (the agent's default)")
		}
		if len(models) > 0 {
			lines = append(lines, "", "Available:")
			for _, id := range models[:min(len(models), maxListedModels)] {
				lines = append(lines, bullet+" "+id)
			}
			if len(models) > maxListedModels {
				lines = append(lines, fmt.Sprintf("%s and %d more", symEllipsis, len(models)-maxListedModels))
			}
		}
		lines = append(lines, "", "Switch with /model <name>; /model default goes back to the agent's model.")
		return strings.Join(lines, "\n")
	}

	if strings.EqualFold(wanted, "default") {
		session.LLMModel = ""
	} else {
		match := ""
		for _, id := range models {
			if strings.EqualFold(id, wanted) {
				match = id
				break
			}
		}
		if len(models) > 0 && match == "" {
			return `There is no model "` + wanted + `". Send /model to see the list.`
		}
		if match == "" {
			match = wanted
		}
		session.LLMModel = match
	}
	session.UpdatedAt = m.d.now().UnixMilli()
	if err := m.d.sessions.Update(ctx, *session); err != nil {
		return "Sorry, something went wrong: " + err.Error()
	}
	if session.LLMModel != "" {
		return "This conversation now uses " + session.LLMModel + "."
	}
	if agentModel != "" {
		return "This conversation now uses the agent's model (" + agentModel + ")."
	}
	return "This conversation now uses the agent's model."
}

// yoloCommand (A-135) is /yolo [on|off]: show or switch the conversation's approval
// mode. In YOLO mode the agent runs tools that need approval without asking here. Only
// the bot's owners (AllowFrom) may switch it: in an open DM or group anyone can talk.
func (m *channelManager) yoloCommand(ctx context.Context, lc *liveChannel, ch channel.Channel, msg channel.InboundMessage, key, args string) string {
	if m.d.approvalMode == nil || m.d.setApprovalMode == nil {
		return "YOLO mode is not available for this agent."
	}
	sessionID, err := m.resolveSession(ctx, ch, msg, lc.platform, key)
	if err != nil {
		return "Sorry, something went wrong: " + err.Error()
	}
	wanted := strings.ToLower(jsstr.Trim(args))
	if wanted == "" {
		mode, err := m.d.approvalMode(ctx, sessionID)
		if err != nil {
			return "Sorry, something went wrong: " + err.Error()
		}
		if mode == kernel.ApprovalModeYolo {
			return "YOLO mode is on: tools run without asking. Send /yolo off to ask again."
		}
		return "YOLO mode is off: tools that need approval ask here first. Send /yolo on to run them without asking."
	}
	if wanted != "on" && wanted != "off" {
		return "Send /yolo on or /yolo off."
	}
	if !slices.Contains(ch.Access.AllowFrom, msg.Sender.ID) {
		return "Only the bot's owners (the senders on its allow list) can switch YOLO mode."
	}
	mode := kernel.ApprovalModeAsk
	if wanted == "on" {
		mode = kernel.ApprovalModeYolo
	}
	if err := m.d.setApprovalMode(ctx, sessionID, mode); err != nil {
		return "Sorry, something went wrong: " + err.Error()
	}
	if wanted == "on" {
		return "YOLO mode is on: tools in this conversation run without asking, until /yolo off."
	}
	return "YOLO mode is off: tools that need approval ask here first."
}

func (m *channelManager) statusText(ctx context.Context, ch channel.Channel, msg channel.InboundMessage, key string) string {
	m.mu.Lock()
	sessionID := m.chatSessions[key]
	_, running := m.chatRuns[key]
	m.mu.Unlock()
	var session *kernel.Session
	if sessionID != "" {
		session, _ = m.d.sessions.Get(ctx, sessionID)
	}
	if session == nil {
		if id, err := m.findSession(ctx, ch, chatKeyOf(msg)); err == nil && id != "" {
			session, _ = m.d.sessions.Get(ctx, id)
		}
	}
	lines := []string{"Agent: " + m.d.agentName()}
	if session != nil {
		lines = append(lines, "Conversation started "+time.UnixMilli(session.CreatedAt).UTC().Format("2006-01-02 15:04")+" UTC")
	} else {
		lines = append(lines, "No conversation yet.")
	}
	model := ""
	if session != nil {
		model = session.LLMModel
	}
	if model == "" && m.d.defaultModel != nil {
		model = m.d.defaultModel()
	}
	if model != "" {
		lines = append(lines, "Model: "+model)
	}
	if session != nil && m.d.approvalMode != nil {
		if mode, err := m.d.approvalMode(ctx, session.ID); err == nil && mode == kernel.ApprovalModeYolo {
			lines = append(lines, "YOLO mode: tools run without asking (/yolo off to stop).")
		}
	}
	if isHome(ch.Home, msg) {
		lines = append(lines, "This is the home chat: job results are sent here.")
	}
	if running {
		lines = append(lines, "A reply is in progress (/stop to stop it).")
	}
	return strings.Join(lines, "\n")
}

// abortChatsLocked cancels the running turns of one channel. The caller holds mu.
func (m *channelManager) abortChatsLocked(channelID string) {
	for key, run := range m.chatRuns {
		if strings.HasPrefix(key, channelID+"\x00") {
			run.cancel()
		}
	}
}

// isDuplicateLocked records key and reports whether it was seen before. The caller
// holds mu.
func (m *channelManager) isDuplicateLocked(key string) bool {
	if _, ok := m.seen[key]; ok {
		return true
	}
	m.seen[key] = struct{}{}
	m.seenLog = append(m.seenLog, key)
	if len(m.seenLog) > maxSeenMessages {
		delete(m.seen, m.seenLog[0])
		m.seenLog = m.seenLog[1:]
	}
	return false
}

func (m *channelManager) isClosed() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.closed
}

func (m *channelManager) requirePlatform(name string) (channel.Platform, error) {
	if m.d.platformHub != nil {
		if p, ok := m.d.platformHub.Get(name); ok {
			return p, nil
		}
	}
	return nil, channel.NewError(fmt.Sprintf(`No "%s" channel platform is registered on this host`, name), channel.CodeUnknownPlatform, "")
}

func (m *channelManager) require(ctx context.Context, id string) (channel.Channel, error) {
	ch, err := m.Get(ctx, id)
	if err != nil {
		return channel.Channel{}, err
	}
	if ch == nil {
		return channel.Channel{}, channel.NewError(`No channel "`+id+`" for this agent`, channel.CodeNotFound, id)
	}
	return *ch, nil
}

func isAllowed(access channel.Access, senderID string) bool {
	return access.DmPolicy == channel.DmOpen || slices.Contains(access.AllowFrom, senderID)
}

// mayTalk reports whether the sender may talk to the agent in this chat: in a direct
// message by DmPolicy and AllowFrom; in a group, any member once the group is allowed.
func mayTalk(access channel.Access, msg channel.InboundMessage) bool {
	if msg.Chat.Type == channel.ChatGroup {
		return groupAllowed(access, msg.Chat.ID)
	}
	if access.DmPolicy == channel.DmDisabled {
		return false
	}
	return isAllowed(access, msg.Sender.ID)
}

func groupAllowed(access channel.Access, chatID string) bool {
	return access.GroupPolicy == channel.GroupOpen || (access.GroupPolicy == channel.GroupAllowlist && slices.Contains(access.Groups, chatID))
}

// agentText is the message as the agent reads it: the bot's @-mention removed, and in a
// group (withSender) prefixed with who wrote it, since several people share the session.
func agentText(ch channel.Channel, msg channel.InboundMessage, withSender bool) string {
	text := msg.Text
	if ch.Account != nil && ch.Account.Username != "" {
		mention := regexp.MustCompile(`(?i)@` + regexp.QuoteMeta(ch.Account.Username) + `\b`)
		text = multiSpace.ReplaceAllString(mention.ReplaceAllString(text, " "), " ")
	}
	text = jsstr.Trim(text)
	if !withSender || msg.Chat.Type != channel.ChatGroup {
		return text
	}
	return "[" + senderLabel(msg.Sender) + "]: " + text
}

// multiSpace is a run of two or more spaces or tabs.
var multiSpace = regexp.MustCompile(`[ \t]{2,}`)

// bullet is U+2022, built from a rune value so this source stays ASCII-only.
var bullet = string(rune(0x2022))

// resultText is what the chat sees for a job's result; markdown bolds the job's name.
// Update (A-70): or a trigger's ("Trigger" instead of "Scheduled job"), with the blocked
// result of a trigger run.
func resultText(origin resultOrigin, res jobResult, markdown bool) string {
	name := origin.name
	// Update (A-126): "Job" for every job.
	what := "Job"
	switch res.kind {
	case jobResultReply:
		title := name
		if markdown {
			title = "**" + name + "**"
		}
		return title + "\n\n" + res.text
	case jobResultEmpty:
		return fmt.Sprintf(`%s "%s" finished without a reply.`, what, name)
	case jobResultBlocked:
		text := fmt.Sprintf(`%s "%s" stopped without finishing: %s`, what, name, res.reason)
		if res.text != "" {
			text += "\n\n" + res.text
		}
		return text + pausedNote(res)
	default:
		return fmt.Sprintf(`%s "%s" failed: %s`, what, name, res.text) + pausedNote(res)
	}
}

// pausedNote is the line a delivery adds when the run paused its job (A-103).
func pausedNote(res jobResult) string {
	if !res.paused {
		return ""
	}
	return "\n\nThe job is now paused after too many failed runs in a row. Turn it back on on the agent's Jobs page once it's fixed."
}

func isHomeTarget(home *channel.Home, target channel.Target) bool {
	return home != nil && home.ChatID == target.ChatID && home.ThreadID == target.ThreadID
}

// chatKeyOf is a chat inside its channel: the chat id, plus the thread when there is one.
func chatKeyOf(msg channel.InboundMessage) string {
	if msg.Chat.ThreadID != "" {
		return msg.Chat.ID + ":" + msg.Chat.ThreadID
	}
	return msg.Chat.ID
}

func chatKey(channelID string, msg channel.InboundMessage) string {
	return channelID + "\x00" + chatKeyOf(msg)
}

func platformLabel(p channel.Platform) string {
	if name := p.DisplayName(); name != "" {
		return name
	}
	return p.Name()
}

// pairingCode is a random code of pairingAlphabet. The alphabet has 32 letters, so a
// random byte modulo 32 picks each one without bias.
func pairingCode() string {
	buf := make([]byte, pairingCodeLength)
	_, _ = rand.Read(buf) // crypto/rand.Read never returns an error (Go 1.24+)
	for i, b := range buf {
		buf[i] = pairingAlphabet[int(b)%len(pairingAlphabet)]
	}
	return string(buf)
}

func validateChannelName(v string) (string, error) {
	name := jsstr.Trim(v)
	if name == "" {
		return "", channel.NewError(`A channel "name" cannot be empty`, channel.CodeInvalidArgument, "")
	}
	if utf8.RuneCountInString(name) > channel.MaxNameLength {
		return "", channel.NewError(fmt.Sprintf("Channel name is longer than %d characters", channel.MaxNameLength), channel.CodeInvalidArgument, "")
	}
	return name, nil
}

func validateCredentials(p channel.Platform, in map[string]string) (map[string]string, error) {
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = jsstr.Trim(v)
	}
	for _, key := range p.CredentialKeys() {
		if out[key] == "" {
			return nil, channel.NewError(fmt.Sprintf(`%s needs the "%s" credential`, platformLabel(p), key), channel.CodeInvalidArgument, "")
		}
	}
	return out, nil
}

// mergeAccess applies a partial access over base and validates the result.
func mergeAccess(base channel.Access, in *channel.AccessInput) (channel.Access, error) {
	out := channel.Access{
		DmPolicy: base.DmPolicy, AllowFrom: slices.Clone(base.AllowFrom),
		GroupPolicy: base.GroupPolicy, Groups: slices.Clone(base.Groups), RequireMention: base.RequireMention,
	}
	if in != nil {
		if in.DmPolicy != nil {
			out.DmPolicy = *in.DmPolicy
		}
		if in.AllowFrom != nil {
			out.AllowFrom = in.AllowFrom
		}
		if in.GroupPolicy != nil {
			out.GroupPolicy = *in.GroupPolicy
		}
		if in.Groups != nil {
			out.Groups = in.Groups
		}
		if in.RequireMention != nil {
			out.RequireMention = *in.RequireMention
		}
	}
	switch out.GroupPolicy {
	case channel.GroupDisabled, channel.GroupAllowlist, channel.GroupOpen:
	default:
		return channel.Access{}, channel.NewError(`"groupPolicy" must be one of disabled, allowlist, open`, channel.CodeInvalidArgument, "")
	}
	out.Groups = uniqueTrimmed(out.Groups)
	switch out.DmPolicy {
	case channel.DmPairing, channel.DmAllowlist, channel.DmOpen, channel.DmDisabled:
	default:
		return channel.Access{}, channel.NewError(`"dmPolicy" must be one of pairing, allowlist, open, disabled`, channel.CodeInvalidArgument, "")
	}
	allow := []string{}
	for _, id := range out.AllowFrom {
		if id = strings.TrimSpace(id); id != "" && !slices.Contains(allow, id) {
			allow = append(allow, id)
		}
	}
	out.AllowFrom = allow
	return out, nil
}

// uniqueTrimmed is ids trimmed, without blanks and duplicates, never nil.
func uniqueTrimmed(ids []string) []string {
	out := []string{}
	for _, id := range ids {
		if id = strings.TrimSpace(id); id != "" && !slices.Contains(out, id) {
			out = append(out, id)
		}
	}
	return out
}

// mergeSettings applies partial settings over base.
func mergeSettings(base channel.Settings, in *channel.SettingsInput) channel.Settings {
	if in != nil {
		if in.Streaming != nil {
			base.Streaming = *in.Streaming
		}
		if in.ToolProgress != nil {
			base.ToolProgress = *in.ToolProgress
		}
	}
	return base
}
