// Package discord is the Discord channel (A-100 phase 3) over the Gateway WebSocket and
// the REST API, the discordPlatform of core/ts. It needs no public URL.
//
// The bot (Developer Portal -> Bot) needs the privileged "Message Content" intent, or
// Discord closes the gateway with 4014. The adapter identifies, keeps the heartbeat, and
// resumes the session after a dropped connection. Button presses arrive as
// INTERACTION_CREATE and are acknowledged at once (deferred update). Discord renders
// Markdown itself, so text is sent as it is.
//
// It is an opt-in package, like channelplatformhub/telegram: register it with
// aec.AgentCore.UseChannelPlatform(discord.Platform()). The WebSocket client is
// github.com/coder/websocket (tests/arch vendorAllowed).
package discord

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"

	"github.com/Phong95/aec-agent-go/components/channelplatformhub/channel"
	"github.com/Phong95/aec-agent-go/components/channelplatformhub/internal/adapterkit"
	"github.com/Phong95/aec-agent-go/internal/jsonx"
)

const (
	defaultAPIBaseURL = "https://discord.com/api/v10"
	defaultGatewayURL = "wss://gateway.discord.gg"
	maxMessageLength  = 2000
	// intents is GUILD_MESSAGES | DIRECT_MESSAGES | MESSAGE_CONTENT.
	intents          = (1 << 9) | (1 << 12) | (1 << 15)
	maxButtonsPerRow = 5
	maxRows          = 5
	// readLimit bounds one gateway frame (READY can list many guilds).
	readLimit = 8 << 20
	// handshakeTimeout bounds how long Start waits for READY after connecting.
	handshakeTimeout = 30 * time.Second
	// closeReconnect is the code a connection the adapter drops itself ends with.
	closeReconnect = 4000
	closeAbnormal  = 1006
)

// fatalClose are the close codes after which reconnecting cannot help.
var fatalClose = map[int]string{
	4004: "Discord rejected the bot token " + adapterkit.EmDash + " it may have been reset",
	4010: "Discord rejected the connection (invalid shard)",
	4011: "Discord requires sharding for this bot",
	4012: "Discord rejected the gateway version",
	4013: "Discord rejected the gateway intents",
	4014: `Discord refused the "Message Content" intent ` + adapterkit.EmDash + " enable it under Bot -> Privileged Gateway Intents",
}

// Options configures the adapters of the platform.
type Options struct {
	// APIBaseURL defaults to https://discord.com/api/v10; set it for tests.
	APIBaseURL string
	// GatewayURL is used when /gateway/bot does not answer one.
	GatewayURL string
	// Client is the HTTP client for REST and the socket handshake (default: a client
	// without a timeout; every call has its own).
	Client *http.Client
}

// Platform returns the Discord platform, for aec.AgentCore.UseChannelPlatform. A channel
// needs the "botToken" credential.
func Platform(opts ...Options) channel.Platform {
	var o Options
	if len(opts) > 0 {
		o = opts[0]
	}
	return platform{o: o}
}

type platform struct{ o Options }

func (platform) Name() string             { return "discord" }
func (platform) DisplayName() string      { return "Discord" }
func (platform) CredentialKeys() []string { return []string{"botToken"} }

func (p platform) New(ch channel.Channel) (channel.Adapter, error) {
	return NewAdapter(ch.Credentials["botToken"], p.o), nil
}

// Adapter is one bot connection.
type Adapter struct {
	token   string
	baseURL string
	client  *http.Client

	mu         sync.Mutex
	started    bool
	cancel     context.CancelFunc
	done       chan struct{}
	gatewayURL string
	botID      string
	botUser    string
	// Gateway session, kept to resume after a dropped connection.
	seq       *int64
	sessionID string
	resumeURL string
}

var (
	_ channel.Adapter      = (*Adapter)(nil)
	_ channel.Editor       = (*Adapter)(nil)
	_ channel.TypingSender = (*Adapter)(nil)
)

// NewAdapter returns an adapter for the bot token.
func NewAdapter(botToken string, o Options) *Adapter {
	base := strings.TrimRight(o.APIBaseURL, "/")
	if base == "" {
		base = defaultAPIBaseURL
	}
	gateway := strings.TrimRight(o.GatewayURL, "/")
	if gateway == "" {
		gateway = defaultGatewayURL
	}
	client := o.Client
	if client == nil {
		client = &http.Client{}
	}
	return &Adapter{token: botToken, baseURL: base, gatewayURL: gateway, client: client}
}

// Capabilities reports the message length limit, and that Markdown and buttons are
// rendered.
func (a *Adapter) Capabilities() channel.Capabilities {
	return channel.Capabilities{MaxMessageLength: maxMessageLength, Markdown: true, Buttons: true}
}

// Start checks the token, opens the first gateway session and waits for READY, so a
// refused intent fails here. The gateway connection is kept until Stop, not until ctx
// ends.
func (a *Adapter) Start(ctx context.Context, sink channel.Sink) (channel.AccountInfo, error) {
	if a.token == "" {
		return channel.AccountInfo{}, errors.New("a Discord bot token is required")
	}
	a.mu.Lock()
	if a.started {
		a.mu.Unlock()
		return channel.AccountInfo{}, errors.New("the Discord adapter is already started")
	}
	a.started = true
	a.mu.Unlock()

	var me discordUser
	if err := a.rest(ctx, http.MethodGet, "/users/@me", nil, &me); err != nil {
		var apiErr *adapterkit.APIError
		if errors.As(err, &apiErr) && apiErr.Status == 401 {
			return channel.AccountInfo{}, errors.New("Discord rejected the bot token " + adapterkit.EmDash + " check it under Bot -> Token")
		}
		return channel.AccountInfo{}, err
	}
	var gateway struct {
		URL string `json:"url"`
	}
	a.mu.Lock()
	a.botID, a.botUser = me.ID, me.Username
	a.mu.Unlock()
	if err := a.rest(ctx, http.MethodGet, "/gateway/bot", nil, &gateway); err == nil && gateway.URL != "" {
		a.mu.Lock()
		a.gatewayURL = strings.TrimRight(gateway.URL, "/")
		a.mu.Unlock()
	}

	// The gateway outlives the ctx of the call that started it; Stop ends it.
	loopCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	ready := make(chan struct{})
	var readyOnce sync.Once
	first := make(chan session, 1)
	go func() {
		first <- a.session(loopCtx, sink, func() { readyOnce.Do(func() { close(ready) }) })
	}()
	timer := time.NewTimer(handshakeTimeout)
	defer timer.Stop()
	select {
	case <-ready:
	case r := <-first:
		cancel()
		if msg, ok := fatalClose[r.code]; ok {
			return channel.AccountInfo{}, errors.New(msg)
		}
		return channel.AccountInfo{}, fmt.Errorf("Discord closed the connection (%s)", closeText(r)) //nolint:staticcheck // a user-facing message, capitalised (same text as core/ts)
	case <-timer.C:
		cancel()
		<-first
		return channel.AccountInfo{}, errors.New("Discord did not finish the gateway handshake") //nolint:staticcheck // a user-facing message, capitalised
	case <-ctx.Done():
		cancel()
		<-first
		return channel.AccountInfo{}, ctx.Err()
	}
	done := make(chan struct{})
	a.mu.Lock()
	a.cancel, a.done = cancel, done
	a.mu.Unlock()
	go func() {
		defer close(done)
		a.run(loopCtx, <-first, sink)
	}()
	display := me.Username
	if me.GlobalName != "" {
		display = me.GlobalName
	}
	return channel.AccountInfo{ID: me.ID, Username: me.Username, DisplayName: display}, nil
}

// Stop closes the gateway connection and waits for it.
func (a *Adapter) Stop(ctx context.Context) error {
	a.mu.Lock()
	cancel, done := a.cancel, a.done
	a.mu.Unlock()
	if cancel == nil {
		return nil
	}
	cancel()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Send posts the message (Markdown as it is, buttons as components, never pinging
// anyone) to the channel.
func (a *Adapter) Send(ctx context.Context, target channel.Target, m channel.OutboundMessage) (channel.MessageRef, error) {
	body := map[string]any{
		"content": m.Text,
		// Never ping @everyone, roles or users from the agent's text.
		"allowed_mentions": map[string]any{"parse": []string{}},
	}
	if len(m.Buttons) > 0 {
		body["components"] = components(m.Buttons)
	}
	if target.ReplyToMessageID != "" {
		body["message_reference"] = map[string]any{"message_id": target.ReplyToMessageID, "fail_if_not_exists": false}
	}
	var sent struct {
		ID        string `json:"id"`
		ChannelID string `json:"channel_id"`
	}
	err := adapterkit.WithRetry(ctx, func() error {
		return a.rest(ctx, http.MethodPost, "/channels/"+target.ChatID+"/messages", body, &sent)
	})
	if err != nil {
		return channel.MessageRef{}, err
	}
	return channel.MessageRef{ChatID: sent.ChannelID, MessageID: sent.ID}, nil
}

// Edit replaces a sent message's text; no buttons given removes the old ones.
func (a *Adapter) Edit(ctx context.Context, ref channel.MessageRef, m channel.OutboundMessage) error {
	body := map[string]any{
		"content":          m.Text,
		"allowed_mentions": map[string]any{"parse": []string{}},
		"components":       []any{},
	}
	if len(m.Buttons) > 0 {
		body["components"] = components(m.Buttons)
	}
	return adapterkit.WithRetry(ctx, func() error {
		return a.rest(ctx, http.MethodPatch, "/channels/"+ref.ChatID+"/messages/"+ref.MessageID, body, nil)
	})
}

// SendTyping triggers the typing indicator.
func (a *Adapter) SendTyping(ctx context.Context, target channel.Target) error {
	return a.rest(ctx, http.MethodPost, "/channels/"+target.ChatID+"/typing", nil, nil)
}

// ---------------------------------------------------------------------------
// Gateway
// ---------------------------------------------------------------------------

// session is how one gateway connection ended.
type session struct {
	code   int
	reason string
	// ready: it reached READY (or RESUMED) before it closed.
	ready bool
}

func closeText(r session) string {
	if r.reason != "" {
		return fmt.Sprintf("%d: %s", r.code, r.reason)
	}
	return fmt.Sprintf("%d", r.code)
}

// run reconnects (resuming when possible) whenever the connection closes, until Stop.
func (a *Adapter) run(ctx context.Context, last session, sink channel.Sink) {
	var backoff adapterkit.Backoff
	r := last
	for {
		if ctx.Err() != nil {
			return
		}
		if msg, ok := fatalClose[r.code]; ok {
			sink.Status(channel.AdapterFailed, msg)
			return
		}
		if !r.ready {
			sink.Status(channel.AdapterRetrying, "Discord connection closed ("+closeText(r)+")")
			if !adapterkit.Sleep(ctx, backoff.Next()) {
				return
			}
		}
		r = a.session(ctx, sink, func() {
			backoff.Reset()
			sink.Status(channel.AdapterRunning, "")
		})
	}
}

// session opens one gateway connection and serves it until it closes. onReady is called
// at READY (or RESUMED).
func (a *Adapter) session(ctx context.Context, sink channel.Sink, onReady func()) session {
	a.mu.Lock()
	url := a.gatewayURL
	if a.sessionID != "" && a.resumeURL != "" {
		url = a.resumeURL
	}
	a.mu.Unlock()
	dialCtx, cancelDial := context.WithTimeout(ctx, adapterkit.RequestTimeout)
	conn, _, err := websocket.Dial(dialCtx, url+"/?v=10&encoding=json", &websocket.DialOptions{HTTPClient: a.client}) //nolint:bodyclose // coder/websocket owns the handshake response; its body needs no close
	cancelDial()
	if err != nil {
		return session{code: closeAbnormal, reason: adapterkit.Cause(err).Error()}
	}
	conn.SetReadLimit(readLimit)

	connCtx, stopConn := context.WithCancel(ctx)
	var wg sync.WaitGroup
	defer wg.Wait()
	defer stopConn()
	defer func() { _ = conn.CloseNow() }()

	var acked atomic.Bool
	ready := false
	send := func(v any) {
		if raw, err := jsonx.Marshal(v); err == nil {
			_ = conn.Write(connCtx, websocket.MessageText, raw)
		}
	}
	for {
		_, data, err := conn.Read(connCtx)
		if err != nil {
			code := int(websocket.CloseStatus(err))
			reason := ""
			var ce websocket.CloseError
			if errors.As(err, &ce) {
				reason = ce.Reason
			}
			if code < 0 {
				code = closeAbnormal
			}
			return session{code: code, reason: reason, ready: ready}
		}
		var f frame
		if json.Unmarshal(data, &f) != nil {
			continue
		}
		if f.S != nil {
			a.mu.Lock()
			s := *f.S
			a.seq = &s
			a.mu.Unlock()
		}
		switch f.Op {
		case 10:
			var hello struct {
				HeartbeatInterval int64 `json:"heartbeat_interval"`
			}
			_ = json.Unmarshal(f.D, &hello)
			acked.Store(true)
			if hello.HeartbeatInterval > 0 {
				wg.Add(1)
				go func() {
					defer wg.Done()
					a.heartbeat(connCtx, conn, time.Duration(hello.HeartbeatInterval)*time.Millisecond, &acked, send)
				}()
			}
			a.mu.Lock()
			sessionID, seq := a.sessionID, a.seq
			a.mu.Unlock()
			if sessionID != "" {
				send(map[string]any{"op": 6, "d": map[string]any{"token": a.token, "session_id": sessionID, "seq": seq}})
			} else {
				send(map[string]any{"op": 2, "d": map[string]any{
					"token": a.token, "intents": intents,
					"properties": map[string]any{"os": "linux", "browser": "aec-agent", "device": "aec-agent"},
				}})
			}
		case 11:
			acked.Store(true)
		case 1:
			a.mu.Lock()
			seq := a.seq
			a.mu.Unlock()
			send(map[string]any{"op": 1, "d": seq})
		case 7:
			// Discord asks for a reconnect: resume on a new connection.
			return session{code: closeReconnect, reason: "reconnect", ready: ready}
		case 9:
			// Invalid session: start a new one unless Discord says it can be resumed.
			if string(f.D) != "true" {
				a.mu.Lock()
				a.sessionID, a.resumeURL, a.seq = "", "", nil
				a.mu.Unlock()
			}
			return session{code: closeReconnect, reason: "invalid session", ready: ready}
		case 0:
			switch f.T {
			case "READY":
				var d struct {
					SessionID        string `json:"session_id"`
					ResumeGatewayURL string `json:"resume_gateway_url"`
				}
				_ = json.Unmarshal(f.D, &d)
				a.mu.Lock()
				a.sessionID, a.resumeURL = d.SessionID, strings.TrimRight(d.ResumeGatewayURL, "/")
				a.mu.Unlock()
				ready = true
				onReady()
			case "RESUMED":
				ready = true
				onReady()
			case "MESSAGE_CREATE":
				var m discordMessage
				if json.Unmarshal(f.D, &m) == nil {
					a.onMessage(m, sink)
				}
			case "INTERACTION_CREATE":
				var in interaction
				if json.Unmarshal(f.D, &in) == nil {
					a.onInteraction(connCtx, in, sink)
				}
			}
		}
	}
}

// heartbeat beats every interval; a beat without an ack since the last one means the
// connection is dead, and it is dropped (to be resumed on a new one).
func (a *Adapter) heartbeat(ctx context.Context, conn *websocket.Conn, interval time.Duration, acked *atomic.Bool, send func(any)) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if !acked.Load() {
				_ = conn.CloseNow()
				return
			}
			acked.Store(false)
			a.mu.Lock()
			seq := a.seq
			a.mu.Unlock()
			send(map[string]any{"op": 1, "d": seq})
		}
	}
}

func (a *Adapter) onMessage(m discordMessage, sink channel.Sink) {
	// Default messages and replies only (not joins, pins, ...).
	if m.Type != nil && *m.Type != 0 && *m.Type != 19 {
		return
	}
	a.mu.Lock()
	botID, botUser := a.botID, a.botUser
	a.mu.Unlock()
	if botID == "" || m.Author == nil {
		return
	}
	mentions := false
	for _, u := range m.Mentions {
		if u.ID == botID {
			mentions = true
		}
	}
	chat := channel.Chat{ID: m.ChannelID, Type: channel.ChatDM}
	if m.GuildID != "" {
		chat.Type = channel.ChatGroup
	}
	// "<@id>" mentions of the bot as "@name", so its own mention can be removed by name.
	text := regexp.MustCompile(`<@!?`+regexp.QuoteMeta(botID)+`>`).ReplaceAllString(m.Content, "@"+botUser)
	sink.Message(channel.InboundMessage{
		MessageID:   m.ID,
		Chat:        chat,
		Sender:      senderOf(*m.Author),
		Text:        text,
		MentionsBot: mentions,
		ReplyToBot:  m.ReferencedMessage != nil && m.ReferencedMessage.Author != nil && m.ReferencedMessage.Author.ID == botID,
	})
}

func (a *Adapter) onInteraction(ctx context.Context, in interaction, sink channel.Sink) {
	if in.Type != 3 || in.Data == nil || in.Data.CustomID == "" {
		return
	}
	// Acknowledge within 3 s, without changing the message (it is edited when the prompt settles).
	_ = a.rest(ctx, http.MethodPost, "/interactions/"+in.ID+"/"+in.Token+"/callback", map[string]any{"type": 6}, nil)
	u := in.User
	if in.Member != nil && in.Member.User != nil {
		u = in.Member.User
	}
	if u == nil || in.ChannelID == "" || in.Message == nil || in.Message.ID == "" {
		return
	}
	chat := channel.Chat{ID: in.ChannelID, Type: channel.ChatDM}
	if in.GuildID != "" {
		chat.Type = channel.ChatGroup
	}
	sink.Button(channel.ButtonPress{Chat: chat, Sender: senderOf(*u), MessageID: in.Message.ID, Data: in.Data.CustomID})
}

// ---------------------------------------------------------------------------
// REST
// ---------------------------------------------------------------------------

var (
	routeInteraction = regexp.MustCompile(`/interactions/[^/]+/[^/]+`)
	routeID          = regexp.MustCompile(`/\d{5,}`)
	ellipsis         = string(rune(0x2026))
)

// routeOf is the route without its ids, for error messages (interaction tokens are secrets).
func routeOf(path string) string {
	return routeID.ReplaceAllString(routeInteraction.ReplaceAllString(path, "/interactions/"+ellipsis), "/:id")
}

// rest is one REST call. The token is only in the Authorization header, never in an
// error message.
func (a *Adapter) rest(ctx context.Context, method, path string, body any, out any) error {
	var reader io.Reader
	if body != nil {
		raw, err := jsonx.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(raw)
	}
	ctx, cancel := context.WithTimeout(ctx, adapterkit.RequestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, a.baseURL+path, reader)
	if err != nil {
		return errors.New("Discord " + method + " " + routeOf(path) + ": could not build the request")
	}
	req.Header.Set("Authorization", "Bot "+a.token)
	req.Header.Set("User-Agent", "DiscordBot (https://github.com/Phong95/aec-agent, 1.0)")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := a.client.Do(req)
	if err != nil {
		return fmt.Errorf("discord %s %s failed: %w", method, routeOf(path), adapterkit.Cause(err))
	}
	defer res.Body.Close()
	if res.StatusCode == http.StatusNoContent {
		return nil
	}
	raw, err := io.ReadAll(io.LimitReader(res.Body, 8<<20))
	if err != nil || (len(raw) > 0 && !json.Valid(raw)) || len(raw) == 0 {
		if res.StatusCode >= 200 && res.StatusCode < 300 {
			return nil
		}
		return &adapterkit.APIError{Status: res.StatusCode, Msg: fmt.Sprintf("Discord %s %s returned HTTP %d", method, routeOf(path), res.StatusCode)}
	}
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		var e struct {
			Message    string   `json:"message"`
			Code       *int     `json:"code"`
			RetryAfter *float64 `json:"retry_after"`
		}
		_ = json.Unmarshal(raw, &e)
		msg := e.Message
		if msg == "" {
			msg = fmt.Sprintf("HTTP %d", res.StatusCode)
		}
		apiErr := &adapterkit.APIError{Status: res.StatusCode, Msg: "Discord " + method + " " + routeOf(path) + " failed: " + msg}
		if e.Code != nil {
			apiErr.Code = fmt.Sprintf("%d", *e.Code)
		}
		if res.StatusCode == http.StatusTooManyRequests {
			switch {
			case e.RetryAfter != nil:
				apiErr.RetryAfter = *e.RetryAfter
			case adapterkit.RetryAfterHeader(res) > 0:
				apiErr.RetryAfter = adapterkit.RetryAfterHeader(res)
			default:
				apiErr.RetryAfter = 1
			}
		}
		return apiErr
	}
	if out != nil {
		return json.Unmarshal(raw, out)
	}
	return nil
}

func components(rows [][]channel.Button) []any {
	out := make([]any, 0, min(len(rows), maxRows))
	for _, row := range rows[:min(len(rows), maxRows)] {
		buttons := make([]any, 0, min(len(row), maxButtonsPerRow))
		for _, b := range row[:min(len(row), maxButtonsPerRow)] {
			style := 2
			switch {
			case strings.Contains(b.Label, "Approve"):
				style = 3
			case strings.Contains(b.Label, "Deny"):
				style = 4
			}
			buttons = append(buttons, map[string]any{"type": 2, "style": style, "label": truncateRunes(b.Label, 80), "custom_id": truncateRunes(b.Data, 100)})
		}
		out = append(out, map[string]any{"type": 1, "components": buttons})
	}
	return out
}

func senderOf(u discordUser) channel.Sender {
	name := u.Username
	if u.GlobalName != "" {
		name = u.GlobalName
	}
	return channel.Sender{ID: u.ID, Name: name, Username: u.Username, IsBot: u.Bot}
}

func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

// -- Discord shapes (the fields used here) -------------------------------------

type frame struct {
	Op int             `json:"op"`
	D  json.RawMessage `json:"d"`
	S  *int64          `json:"s"`
	T  string          `json:"t"`
}

type discordUser struct {
	ID         string `json:"id"`
	Username   string `json:"username"`
	GlobalName string `json:"global_name"`
	Bot        bool   `json:"bot"`
}

type discordMessage struct {
	ID                string        `json:"id"`
	Type              *int          `json:"type"`
	ChannelID         string        `json:"channel_id"`
	GuildID           string        `json:"guild_id"`
	Author            *discordUser  `json:"author"`
	Content           string        `json:"content"`
	Mentions          []discordUser `json:"mentions"`
	ReferencedMessage *struct {
		Author *discordUser `json:"author"`
	} `json:"referenced_message"`
}

type interaction struct {
	ID    string `json:"id"`
	Token string `json:"token"`
	Type  int    `json:"type"`
	Data  *struct {
		CustomID string `json:"custom_id"`
	} `json:"data"`
	ChannelID string `json:"channel_id"`
	GuildID   string `json:"guild_id"`
	Member    *struct {
		User *discordUser `json:"user"`
	} `json:"member"`
	User    *discordUser `json:"user"`
	Message *struct {
		ID string `json:"id"`
	} `json:"message"`
}
