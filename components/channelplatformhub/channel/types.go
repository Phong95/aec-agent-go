// Package channel holds the contracts of channels (A-100): chat apps (Telegram, Slack,
// Discord, your own app...) users talk to an agent through, like the OpenClaw and
// Hermes Agent gateways.
//
// A Channel is data: one connected bot account, owned by one agent, kept in
// persistence.ChannelStore so it survives a restart. A Platform builds a live Adapter
// for it; the adapter normalises the platform's messages into InboundMessage and sends
// text back. Everything between the two (who may talk to the agent, which session a
// chat maps to, chat commands, splitting long replies) belongs to the agent's channel
// manager, so a new platform only has to implement the small Adapter contract.
//
// The agent decides WHAT to run; the host decides HOW through a TurnRunner (the same
// split as prompt jobs), so a channel turn takes the host's per-session lock, stop
// registry, traces and usage like a web chat.
//
// The JSON shapes are the wire shapes of core/ts (Channel, ChannelPairing,
// ChannelState), so the Go and TypeScript servers share the same Postgres rows and
// HTTP bodies.
package channel

import (
	"context"

	"github.com/Phong95/aec-agent-go/kernel"
)

// ---------------------------------------------------------------------------
// Stored channel
// ---------------------------------------------------------------------------

// DmPolicy says who may talk to the agent in direct messages.
type DmPolicy string

// The DM policies. The string values are stable wire values.
const (
	// DmPairing gives an unknown sender a one-time code the owner approves (the default).
	DmPairing DmPolicy = "pairing"
	// DmAllowlist lets only the senders in Access.AllowFrom talk; others are told their id.
	DmAllowlist DmPolicy = "allowlist"
	// DmOpen lets anyone who finds the bot talk to the agent.
	DmOpen DmPolicy = "open"
	// DmDisabled ignores direct messages.
	DmDisabled DmPolicy = "disabled"
)

// GroupPolicy says which group chats the bot answers in (phase 2).
type GroupPolicy string

// The group policies. The string values are stable wire values.
const (
	// GroupDisabled ignores group chats (the default).
	GroupDisabled GroupPolicy = "disabled"
	// GroupAllowlist answers only in the group chats of Access.Groups.
	GroupAllowlist GroupPolicy = "allowlist"
	// GroupOpen answers in any group the bot is added to.
	GroupOpen GroupPolicy = "open"
)

// Access is who may talk to the agent.
type Access struct {
	DmPolicy DmPolicy `json:"dmPolicy"`
	// AllowFrom are sender ids (as the platform reports them) allowed to talk to the
	// agent. Approving a pairing adds one. Never nil on a stored channel.
	// In group chats they are the only members who may approve tools.
	AllowFrom []string `json:"allowFrom"`
	// GroupPolicy defaults to GroupDisabled.
	GroupPolicy GroupPolicy `json:"groupPolicy"`
	// Groups are the group chat ids allowed under GroupAllowlist (/whoami in the group
	// shows it). Never nil on a stored channel.
	Groups []string `json:"groups"`
	// RequireMention: in groups, answer only messages that @-mention the bot or reply to
	// it (default true).
	RequireMention bool `json:"requireMention"`
}

// Settings say how replies are shown in the chat (phase 2).
type Settings struct {
	// Streaming shows the reply while it is written by editing one message in place
	// (default true). It needs an adapter that implements Editor; otherwise the reply is
	// sent when it is done.
	Streaming bool `json:"streaming"`
	// ToolProgress keeps one message listing the tools the agent runs, edited as they
	// finish (default true).
	ToolProgress bool `json:"toolProgress"`
}

// AccountInfo is the bot account a channel is connected as, read from the platform
// when it connects.
type AccountInfo struct {
	ID string `json:"id"`
	// Username is the handle without "@", e.g. "my_aec_bot".
	Username    string `json:"username,omitempty"`
	DisplayName string `json:"displayName,omitempty"`
}

// Home is the chat a channel sends scheduled-job results to when a job's delivery
// Update (A-126): job results (jobs of every kind).
// target is "home" (phase 3). It is set by /sethome in a chat, or by the owner through
// the host.
type Home struct {
	ChatID string `json:"chatId"`
	// ThreadID is the forum topic / thread inside the chat, when there is one.
	ThreadID string `json:"threadId,omitempty"`
	// Title is the chat's title, or the sender's name in a direct message: shown to users.
	Title string `json:"title,omitempty"`
	// SetBy is the sender id of who ran /sethome; empty when set through the host.
	SetBy string `json:"setBy,omitempty"`
	// SetAt is the ISO time it was set.
	SetAt string `json:"setAt"`
}

// HomeInput sets a channel's home chat through UpdateInput.
type HomeInput struct {
	ChatID   string `json:"chatId"`
	ThreadID string `json:"threadId,omitempty"`
	Title    string `json:"title,omitempty"`
}

// Channel is a stored channel.
type Channel struct {
	ID string `json:"id"`
	// AgentID is the owning agent. Every list path is filtered by it.
	AgentID string `json:"agentId"`
	// Platform is the registered Platform name, e.g. "telegram".
	Platform string `json:"platform"`
	// Name is the label shown to users.
	Name string `json:"name"`
	// Credentials are the platform secrets, e.g. {"botToken": ...} for Telegram. Hosts
	// must never return them from their APIs and should encrypt them at rest.
	Credentials map[string]string `json:"credentials"`
	Access      Access            `json:"access"`
	Settings    Settings          `json:"settings"`
	Enabled     bool              `json:"enabled"`
	// Account is set on the first successful connect.
	Account *AccountInfo `json:"account,omitempty"`
	// Home is where scheduled-job results go when a job delivers to "home" (phase 3).
	// Update (A-126): job results (jobs of every kind).
	Home *Home `json:"home,omitempty"`
	// CreatedAt and UpdatedAt are ISO timestamps (the shape of JS toISOString).
	CreatedAt string `json:"createdAt"`
	UpdatedAt string `json:"updatedAt"`
}

// AccessInput is a partial Access: a nil field is left as it is.
type AccessInput struct {
	DmPolicy       *DmPolicy    `json:"dmPolicy,omitempty"`
	AllowFrom      []string     `json:"allowFrom,omitempty"`
	GroupPolicy    *GroupPolicy `json:"groupPolicy,omitempty"`
	Groups         []string     `json:"groups,omitempty"`
	RequireMention *bool        `json:"requireMention,omitempty"`
}

// SettingsInput is a partial Settings: a nil field is left as it is.
type SettingsInput struct {
	Streaming    *bool `json:"streaming,omitempty"`
	ToolProgress *bool `json:"toolProgress,omitempty"`
}

// CreateInput is what a caller sets when connecting a channel.
type CreateInput struct {
	Platform string `json:"platform"`
	// Name defaults to the bot's display name, else the platform name.
	Name        string            `json:"name,omitempty"`
	Credentials map[string]string `json:"credentials"`
	// Access defaults to {DmPairing, [], GroupDisabled, [], RequireMention true}.
	Access *AccessInput `json:"access,omitempty"`
	// Settings defaults to {Streaming true, ToolProgress true}.
	Settings *SettingsInput `json:"settings,omitempty"`
	// Enabled defaults to true when nil.
	Enabled *bool `json:"enabled,omitempty"`
}

// UpdateInput is a partial update: a nil field is left as it is. Credentials are
// merged into the stored ones; a change reconnects.
type UpdateInput struct {
	Name        *string           `json:"name,omitempty"`
	Credentials map[string]string `json:"credentials,omitempty"`
	Access      *AccessInput      `json:"access,omitempty"`
	Settings    *SettingsInput    `json:"settings,omitempty"`
	Enabled     *bool             `json:"enabled,omitempty"`
	// Home sets the home chat (phase 3).
	Home *HomeInput `json:"home,omitempty"`
	// ClearHome removes the home chat (the wire's "home": null); it wins over Home.
	ClearHome bool `json:"-"`
}

// Status is a channel's live connection status.
type Status string

// The statuses.
const (
	StatusStopped    Status = "stopped"
	StatusConnecting Status = "connecting"
	StatusRunning    Status = "running"
	StatusRetrying   Status = "retrying"
	StatusFailed     Status = "failed"
)

// State is a channel's live connection state, kept in memory, not stored.
type State struct {
	Status Status `json:"status"`
	// Error says why it is retrying or failed.
	Error string `json:"error,omitempty"`
	// Since is the ISO time the status last changed.
	Since string `json:"since"`
}

// Pairing is a sender waiting for the owner to approve them (DmPairing). Pairings are
// kept in memory and their codes expire.
type Pairing struct {
	Code       string `json:"code"`
	ChannelID  string `json:"channelId"`
	SenderID   string `json:"senderId"`
	SenderName string `json:"senderName,omitempty"`
	// ChatID is the chat the sender is told in once approved.
	ChatID string `json:"chatId"`
	// RequestedAt and ExpiresAt are ISO timestamps.
	RequestedAt string `json:"requestedAt"`
	ExpiresAt   string `json:"expiresAt"`
}

// ---------------------------------------------------------------------------
// Adapter contract (implemented per platform)
// ---------------------------------------------------------------------------

// ChatType is the kind of chat a message came from.
type ChatType string

// The chat types.
const (
	ChatDM      ChatType = "dm"
	ChatGroup   ChatType = "group"
	ChatChannel ChatType = "channel"
)

// Chat is where a message came from.
type Chat struct {
	ID   string   `json:"id"`
	Type ChatType `json:"type"`
	// ThreadID is the forum topic or thread inside the chat, when the platform has them.
	ThreadID string `json:"threadId,omitempty"`
	Title    string `json:"title,omitempty"`
}

// Sender is who sent a message.
type Sender struct {
	ID       string `json:"id"`
	Name     string `json:"name,omitempty"`
	Username string `json:"username,omitempty"`
	IsBot    bool   `json:"isBot,omitempty"`
}

// InboundMessage is one message from the platform, normalised by the adapter.
type InboundMessage struct {
	// MessageID is unique within the chat; it is used to drop duplicates.
	MessageID string `json:"messageId"`
	Chat      Chat   `json:"chat"`
	Sender    Sender `json:"sender"`
	// Text is the message text (or a media caption); empty when it carried no text.
	Text string `json:"text"`
	// MentionsBot is true when the message @-mentions the bot.
	MentionsBot bool `json:"mentionsBot,omitempty"`
	// ReplyToBot is true when the message replies to one of the bot's messages.
	ReplyToBot bool `json:"replyToBot,omitempty"`
}

// Target is where to send: a chat, optionally a thread in it and a message to reply to.
type Target struct {
	ChatID           string `json:"chatId"`
	ThreadID         string `json:"threadId,omitempty"`
	ReplyToMessageID string `json:"replyToMessageId,omitempty"`
}

// Button is a button under a message (approve / deny, clarify choices).
type Button struct {
	Label string `json:"label"`
	// Data is handed back in ButtonPress.Data. At most 64 bytes (Telegram's limit).
	Data string `json:"data"`
}

// The message formats of OutboundMessage.Format.
const (
	FormatPlain    = "plain"
	FormatMarkdown = "markdown"
)

// OutboundMessage is plain text, already within Capabilities.MaxMessageLength.
// -> Phase 2: Format "markdown" marks the agent's Markdown, which the adapter renders in
// its platform's format (and sends as plain text when that fails); "" means plain.
// Buttons are rows of buttons, sent only when Capabilities.Buttons is true.
type OutboundMessage struct {
	Text    string     `json:"text"`
	Format  string     `json:"format,omitempty"`
	Buttons [][]Button `json:"buttons,omitempty"`
}

// ButtonPress is someone pressing one of the bot's buttons.
type ButtonPress struct {
	Chat   Chat   `json:"chat"`
	Sender Sender `json:"sender"`
	// MessageID is the message the button is on.
	MessageID string `json:"messageId"`
	// Data is Button.Data.
	Data string `json:"data"`
}

// MessageRef names a sent message.
type MessageRef struct {
	ChatID    string `json:"chatId"`
	MessageID string `json:"messageId"`
}

// Capabilities are what an adapter can do.
type Capabilities struct {
	// MaxMessageLength is the longest text (in UTF-16 units, as JS counts) one message
	// may carry; longer replies are split.
	MaxMessageLength int
	// Markdown: Send renders OutboundMessage.Format "markdown". Otherwise Markdown is sent
	// as it is.
	Markdown bool
	// Buttons: Send and Edit show OutboundMessage.Buttons, and presses reach Sink.Button.
	Buttons bool
}

// AdapterStatus is what an adapter reports through its Sink.
type AdapterStatus string

// The adapter statuses.
const (
	// AdapterRunning is reported once the adapter is (re)connected.
	AdapterRunning AdapterStatus = "running"
	// AdapterRetrying is reported while a transient error is being retried.
	AdapterRetrying AdapterStatus = "retrying"
	// AdapterFailed is reported when the adapter gave up; it has stopped itself then.
	AdapterFailed AdapterStatus = "failed"
)

// Sink is how an adapter reports to the agent. Its methods may be called from any
// goroutine and return quickly.
type Sink interface {
	Message(m InboundMessage)
	// Button is a button press (Capabilities.Buttons). The adapter acknowledges it on the
	// platform itself.
	Button(p ButtonPress)
	Status(s AdapterStatus, errText string)
}

// Adapter is one live connection to a platform account, built by a Platform for a
// stored Channel. Implement it to connect your own app's chat.
type Adapter interface {
	Capabilities() Capabilities
	// Start connects and starts delivering messages to sink. It returns once connected
	// with the bot account, or an error when the credentials are wrong or the platform
	// cannot be reached; nothing keeps running then. The adapter's background work must
	// not end with ctx: it runs until Stop.
	Start(ctx context.Context, sink Sink) (AccountInfo, error)
	// Stop disconnects. No message reaches the sink after it returns. Safe to call twice.
	Stop(ctx context.Context) error
	Send(ctx context.Context, target Target, m OutboundMessage) (MessageRef, error)
}

// Editor is an optional Adapter method: replace the text (and buttons; none given =
// removed) of a message the bot sent. Streamed previews, the tool-progress message and
// settled approval prompts need it. Editing to the same text must not fail.
type Editor interface {
	Edit(ctx context.Context, ref MessageRef, m OutboundMessage) error
}

// TypingSender is an optional Adapter method: show "typing…" in the chat for a few
// seconds.
type TypingSender interface {
	SendTyping(ctx context.Context, target Target) error
}

// BotCommand is a chat command the bot answers, e.g. {Name: "new", Description: "Start a
// new conversation"}.
type BotCommand struct {
	// Name is without the "/": lower case letters, digits and "_".
	Name        string `json:"name"`
	Description string `json:"description"`
}

// CommandSetter is an optional Adapter method: publish the chat commands the bot
// answers, so the platform can suggest them (Telegram's "/" menu). The channel manager
// calls it once connected; a failure is logged and the channel keeps running.
type CommandSetter interface {
	SetCommands(ctx context.Context, commands []BotCommand) error
}

// WebhookHandler is an optional Adapter method (phase 3): an inbound HTTP request for this
// channel, for platforms that push messages to the host (Microsoft Teams' Bot Framework)
// instead of being polled or connected to. The host routes POST /webhooks/channels/{id}
// here (aec.AgentCore.HandleChannelWebhook). The adapter checks the request is genuine
// (signature / token) and hands messages to the sink it got in Start.
type WebhookHandler interface {
	HandleWebhook(ctx context.Context, req WebhookRequest) WebhookResponse
}

// WebhookRequest is an inbound HTTP request for a channel's webhook (phase 3).
type WebhookRequest struct {
	Method string `json:"method"`
	// Headers has lower-case names.
	Headers map[string]string `json:"headers"`
	// Body is the raw body.
	Body string `json:"body"`
}

// WebhookResponse is what the host answers a webhook request with.
type WebhookResponse struct {
	Status int `json:"status"`
	// Body is JSON-encoded by the host when it is not a string; nil = no body.
	Body any `json:"body,omitempty"`
}

// Ingress is how a platform reaches the host (phase 3).
type Ingress string

const (
	// IngressConnect: the adapter polls or holds a connection; no public URL is needed.
	IngressConnect Ingress = "connect"
	// IngressWebhook: the platform pushes to the host's POST /webhooks/channels/{id},
	// which must be reachable from the internet.
	IngressWebhook Ingress = "webhook"
)

// OptionalCredentialPlatform is an optional Platform method (phase 3): credential keys a
// channel may have, e.g. ["tenantId"], shown as optional fields.
type OptionalCredentialPlatform interface {
	OptionalCredentialKeys() []string
}

// IngressPlatform is an optional Platform method (phase 3): how the platform reaches the
// host. A platform without it is IngressConnect.
type IngressPlatform interface {
	Ingress() Ingress
}

// OptionalCredentialKeysOf returns p's optional credential keys (never nil).
func OptionalCredentialKeysOf(p Platform) []string {
	if o, ok := p.(OptionalCredentialPlatform); ok {
		if keys := o.OptionalCredentialKeys(); keys != nil {
			return keys
		}
	}
	return []string{}
}

// IngressOf returns how p reaches the host (IngressConnect unless it says otherwise).
func IngressOf(p Platform) Ingress {
	if i, ok := p.(IngressPlatform); ok && i.Ingress() != "" {
		return i.Ingress()
	}
	return IngressConnect
}

// Platform builds adapters for one chat platform. Register it with
// aec.AgentCore.UseChannelPlatform.
type Platform interface {
	// Name is stored on Channel.Platform, e.g. "telegram".
	Name() string
	// DisplayName is e.g. "Telegram".
	DisplayName() string
	// CredentialKeys are the credential keys a channel must have, e.g. ["botToken"];
	// they are checked on create.
	CredentialKeys() []string
	New(ch Channel) (Adapter, error)
}

// ---------------------------------------------------------------------------
// Turn runner
// ---------------------------------------------------------------------------

// Usage is the token usage of one turn.
type Usage struct {
	InputTokens  int `json:"inputTokens"`
	OutputTokens int `json:"outputTokens"`
}

// TurnContext is what the TurnRunner receives for one inbound message. The turn's
// context.Context is cancelled by /stop, when the channel stops, or when the agent is
// disposed.
type TurnContext struct {
	AgentID string  `json:"agentId"`
	Channel Channel `json:"channel"`
	// SessionID is the chat's session: already created and owned by the agent.
	SessionID string         `json:"sessionId"`
	Message   InboundMessage `json:"message"`
	// Text is the user message for the agent (the inbound text).
	// -> Phase 2: in groups prefixed with the sender's name.
	Text string `json:"text"`
	// OnChunk hands each chunk of the turn to the channel (text, tool calls, tool
	// results) so it can show the reply while it is written and the tools as they run. A
	// runner that streams (Agent.ChatStream) calls it; one that does not leaves the chat
	// with the final reply only. Never nil when the agent calls the runner.
	OnChunk func(kernel.StreamChunk) `json:"-"`
}

// TurnResult is what a TurnRunner returns for a finished turn.
type TurnResult struct {
	// Output is the agent's reply.
	Output string `json:"output,omitempty"`
	Usage  *Usage `json:"usage,omitempty"`
	// Aborted is true when the turn was stopped before it finished.
	Aborted    bool   `json:"aborted,omitempty"`
	StopReason string `json:"stopReason,omitempty"`
}

// TurnRunner runs one agent turn for an inbound chat message. The host supplies it
// (aec.AgentCore.UseChannelTurnRunner) so the turn goes through the same session lock
// and bookkeeping as its chat routes; nil means the agent's own Chat. An error tells
// the chat that something went wrong.
type TurnRunner func(ctx context.Context, tc TurnContext) (TurnResult, error)

// ---------------------------------------------------------------------------
// Agent API
// ---------------------------------------------------------------------------

// Channels is one agent's channels: the API host routes use. Every call is scoped to
// the owning agent; a channel id of another agent is not found (an *Error with
// CodeNotFound).
type Channels interface {
	// List returns the agent's channels, oldest created first. They carry credentials:
	// mask them in host APIs.
	List(ctx context.Context) ([]Channel, error)
	// Get returns the channel, or nil and a nil error when the agent has no such channel.
	Get(ctx context.Context, id string) (*Channel, error)
	// Create saves a channel and, when enabled, connects it. It fails with CodeConnectFailed
	// (and saves nothing) when the platform rejects the credentials.
	Create(ctx context.Context, in CreateInput) (Channel, error)
	// Update applies a partial update. Changed credentials, a re-enable or a disable
	// reconnect or stop the channel; a failed reconnect shows in State.
	Update(ctx context.Context, id string, in UpdateInput) (Channel, error)
	// Delete disconnects and deletes the channel. Its chats' sessions stay.
	Delete(ctx context.Context, id string) error
	// State is the live connection state; StatusStopped for a disabled or unknown channel.
	State(id string) State
	// ListPairings returns the senders waiting for approval, oldest first. Expired codes
	// are dropped.
	ListPairings(ctx context.Context, id string) ([]Pairing, error)
	// ApprovePairing adds the sender to Access.AllowFrom and tells them in their chat.
	ApprovePairing(ctx context.Context, id, code string) (Channel, error)
	// RejectPairing drops the request; the sender may ask again later.
	RejectPairing(ctx context.Context, id, code string) error
	// Send sends text to a chat outside a turn, split when long. The channel must be
	// running.
	Send(ctx context.Context, id string, target Target, text string) ([]MessageRef, error)
	// HandleWebhook hands an inbound webhook request to the channel's adapter (phase 3):
	// 404 when the channel is not this agent's, 503 when it is not connected, 405 when its
	// platform takes no webhooks.
	HandleWebhook(ctx context.Context, id string, req WebhookRequest) WebhookResponse
}

// Limits of the channels of one agent.
const (
	// MaxChannelsPerAgent is the most channels one agent may have.
	MaxChannelsPerAgent = 10
	// MaxNameLength bounds a channel's name, in characters.
	MaxNameLength = 80
	// PairingTTLMs is how long a pairing code stays valid.
	PairingTTLMs = 60 * 60 * 1000
	// MaxPendingPairings is the most senders waiting for approval per channel; more are
	// asked to try later.
	MaxPendingPairings = 3
)
