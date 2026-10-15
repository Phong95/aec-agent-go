// Package slack is the Slack channel (A-100 phase 3) over Socket Mode, the slackPlatform
// of core/ts. It needs no public URL.
//
// A Slack app with Socket Mode on gives two tokens: the bot token (xoxb-...) for the Web
// API (chat.postMessage, chat.update, users.info) and an app-level token (xapp-..., scope
// connections:write) that opens the event WebSocket (apps.connections.open). Events
// arrive in envelopes that must be acknowledged; button presses arrive as "interactive"
// envelopes. Slack closes the socket from time to time (a "disconnect" envelope); the
// adapter opens a new one.
//
// The app needs the bot scopes chat:write, im:history, users:read (and channels:history /
// groups:history / app_mentions:read for channels) and the events message.im (+
// message.channels / message.groups / app_mention).
//
// It is an opt-in package, like channelplatformhub/telegram: register it with
// aec.AgentCore.UseChannelPlatform(slack.Platform()). The WebSocket client is
// github.com/coder/websocket (tests/arch vendorAllowed).
package slack

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/Phong95/aec-agent-go/components/channelplatformhub/channel"
	"github.com/Phong95/aec-agent-go/components/channelplatformhub/internal/adapterkit"
	"github.com/Phong95/aec-agent-go/internal/jsonx"
)

const (
	defaultAPIBaseURL = "https://slack.com/api"
	// maxMessageLength: a section block holds at most 3000 characters; replies are split to fit.
	maxMessageLength = 3000
	maxSenderCache   = 1000
	// readLimit bounds one socket frame (Slack's envelopes are small).
	readLimit = 1 << 20
)

// Options configures the adapters of the platform.
type Options struct {
	// APIBaseURL defaults to https://slack.com/api; set it for tests.
	APIBaseURL string
	// Client is the HTTP client for the Web API and the socket handshake (default: a
	// client without a timeout; every call has its own).
	Client *http.Client
}

// Platform returns the Slack platform, for aec.AgentCore.UseChannelPlatform. A channel
// needs the "botToken" and "appToken" credentials.
func Platform(opts ...Options) channel.Platform {
	var o Options
	if len(opts) > 0 {
		o = opts[0]
	}
	return platform{o: o}
}

type platform struct{ o Options }

func (platform) Name() string             { return "slack" }
func (platform) DisplayName() string      { return "Slack" }
func (platform) CredentialKeys() []string { return []string{"botToken", "appToken"} }

func (p platform) New(ch channel.Channel) (channel.Adapter, error) {
	return NewAdapter(ch.Credentials["botToken"], ch.Credentials["appToken"], p.o), nil
}

// Adapter is one Slack app connection.
type Adapter struct {
	botToken string
	appToken string
	baseURL  string
	client   *http.Client

	mu        sync.Mutex
	started   bool
	cancel    context.CancelFunc
	done      chan struct{}
	conn      *websocket.Conn
	botUserID string
	botName   string
	senders   map[string]channel.Sender
}

var (
	_ channel.Adapter = (*Adapter)(nil)
	_ channel.Editor  = (*Adapter)(nil)
)

// NewAdapter returns an adapter for the two tokens.
func NewAdapter(botToken, appToken string, o Options) *Adapter {
	base := strings.TrimRight(o.APIBaseURL, "/")
	if base == "" {
		base = defaultAPIBaseURL
	}
	client := o.Client
	if client == nil {
		client = &http.Client{}
	}
	return &Adapter{botToken: botToken, appToken: appToken, baseURL: base, client: client, senders: map[string]channel.Sender{}}
}

// Capabilities reports the message length limit, and that Markdown and buttons are
// rendered.
func (a *Adapter) Capabilities() channel.Capabilities {
	return channel.Capabilities{MaxMessageLength: maxMessageLength, Markdown: true, Buttons: true}
}

// Start checks the bot token (auth.test) and opens the first socket, so a wrong app token
// fails here. The socket is kept open until Stop, not until ctx ends.
func (a *Adapter) Start(ctx context.Context, sink channel.Sink) (channel.AccountInfo, error) {
	if a.botToken == "" || a.appToken == "" {
		return channel.AccountInfo{}, errors.New("Slack needs a bot token and an app-level token") //nolint:staticcheck // a user-facing message, capitalised (same text as core/ts)
	}
	a.mu.Lock()
	if a.started {
		a.mu.Unlock()
		return channel.AccountInfo{}, errors.New("the Slack adapter is already started")
	}
	a.started = true
	a.mu.Unlock()

	var auth struct {
		UserID string `json:"user_id"`
		User   string `json:"user"`
		Team   string `json:"team"`
	}
	if err := a.api(ctx, "auth.test", map[string]any{}, a.botToken, &auth); err != nil {
		var apiErr *adapterkit.APIError
		if errors.As(err, &apiErr) && isAuthError(apiErr.Code) {
			return channel.AccountInfo{}, errors.New("Slack rejected the bot token " + adapterkit.EmDash + " check it under OAuth & Permissions")
		}
		return channel.AccountInfo{}, err
	}
	a.mu.Lock()
	a.botUserID, a.botName = auth.UserID, auth.User
	a.mu.Unlock()

	// The socket outlives the ctx of the call that started it; Stop ends it.
	loopCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	// The first connection is opened here, so a wrong app token fails start().
	conn, err := a.connect(loopCtx)
	if err != nil {
		cancel()
		var apiErr *adapterkit.APIError
		if errors.As(err, &apiErr) && isAuthError(apiErr.Code) {
			return channel.AccountInfo{}, errors.New("Slack rejected the app-level token " + adapterkit.EmDash + " create one with connections:write under Basic Information")
		}
		return channel.AccountInfo{}, err
	}
	done := make(chan struct{})
	a.mu.Lock()
	a.cancel, a.done = cancel, done
	a.mu.Unlock()
	go func() {
		defer close(done)
		a.run(loopCtx, conn, sink)
	}()

	display := auth.UserID
	switch {
	case auth.Team != "":
		user := auth.User
		if user == "" {
			user = "bot"
		}
		display = user + " (" + auth.Team + ")"
	case auth.User != "":
		display = auth.User
	}
	return channel.AccountInfo{ID: auth.UserID, Username: auth.User, DisplayName: display}, nil
}

// Stop closes the socket and waits for the reader.
func (a *Adapter) Stop(ctx context.Context) error {
	a.mu.Lock()
	cancel, done, conn := a.cancel, a.done, a.conn
	a.mu.Unlock()
	if cancel == nil {
		return nil
	}
	cancel()
	if conn != nil {
		_ = conn.CloseNow()
	}
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Send posts the message (mrkdwn, with buttons as blocks) to the chat, in the thread when
// the target has one.
func (a *Adapter) Send(ctx context.Context, target channel.Target, m channel.OutboundMessage) (channel.MessageRef, error) {
	body := map[string]any{"channel": target.ChatID}
	if target.ThreadID != "" {
		body["thread_ts"] = target.ThreadID
	}
	for k, v := range messageBody(m) {
		body[k] = v
	}
	var sent struct {
		Channel string `json:"channel"`
		TS      string `json:"ts"`
	}
	err := adapterkit.WithRetry(ctx, func() error { return a.api(ctx, "chat.postMessage", body, a.botToken, &sent) })
	if err != nil {
		return channel.MessageRef{}, err
	}
	return channel.MessageRef{ChatID: sent.Channel, MessageID: sent.TS}, nil
}

// Edit replaces a sent message's text; no buttons given removes the old ones.
func (a *Adapter) Edit(ctx context.Context, ref channel.MessageRef, m channel.OutboundMessage) error {
	body := map[string]any{"channel": ref.ChatID, "ts": ref.MessageID, "blocks": []any{}}
	for k, v := range messageBody(m) {
		body[k] = v
	}
	err := adapterkit.WithRetry(ctx, func() error { return a.api(ctx, "chat.update", body, a.botToken, nil) })
	var apiErr *adapterkit.APIError
	if errors.As(err, &apiErr) && apiErr.Code == "cant_update_message" {
		return nil
	}
	return err
}

// ---------------------------------------------------------------------------

// messageBody is the text (and blocks with buttons) of a message, in mrkdwn.
func messageBody(m channel.OutboundMessage) map[string]any {
	text := EscapeMrkdwn(m.Text)
	if m.Format == channel.FormatMarkdown {
		text = MarkdownToMrkdwn(m.Text)
	}
	if len(m.Buttons) == 0 {
		return map[string]any{"text": text}
	}
	blocks := []any{map[string]any{"type": "section", "text": map[string]any{"type": "mrkdwn", "text": text}}}
	for _, row := range m.Buttons {
		elements := make([]any, 0, len(row))
		for i, b := range row {
			elements = append(elements, buttonElement(b, i))
		}
		blocks = append(blocks, map[string]any{"type": "actions", "elements": elements})
	}
	return map[string]any{"text": text, "blocks": blocks}
}

func buttonElement(b channel.Button, index int) map[string]any {
	el := map[string]any{
		"type":      "button",
		"text":      map[string]any{"type": "plain_text", "text": truncateRunes(b.Label, 75), "emoji": true},
		"action_id": truncateRunes("aec_"+strconv.Itoa(index)+"_"+b.Data, 255),
		"value":     b.Data,
	}
	switch {
	case strings.Contains(b.Label, "Approve"):
		el["style"] = "primary"
	case strings.Contains(b.Label, "Deny"):
		el["style"] = "danger"
	}
	return el
}

// run reads the socket and opens a new one, with backoff, whenever it closes, until Stop.
func (a *Adapter) run(ctx context.Context, conn *websocket.Conn, sink channel.Sink) {
	var backoff adapterkit.Backoff
	for {
		a.read(ctx, conn, sink)
		if ctx.Err() != nil {
			return
		}
		for {
			next, err := a.connect(ctx)
			if err == nil {
				backoff.Reset()
				sink.Status(channel.AdapterRunning, "")
				conn = next
				break
			}
			if ctx.Err() != nil {
				return
			}
			var apiErr *adapterkit.APIError
			isAPI := errors.As(err, &apiErr)
			if isAPI && isAuthError(apiErr.Code) {
				sink.Status(channel.AdapterFailed, "Slack rejected the app-level token "+adapterkit.EmDash+" it may have been revoked")
				return
			}
			sink.Status(channel.AdapterRetrying, err.Error())
			wait := backoff.Next()
			if isAPI && apiErr.RetryAfter > 0 {
				wait = time.Duration(apiErr.RetryAfter * float64(time.Second))
			}
			if !adapterkit.Sleep(ctx, wait) {
				return
			}
		}
	}
}

// connect asks for a socket URL with the app token and opens it.
func (a *Adapter) connect(ctx context.Context) (*websocket.Conn, error) {
	var open struct {
		URL string `json:"url"`
	}
	if err := a.api(ctx, "apps.connections.open", map[string]any{}, a.appToken, &open); err != nil {
		return nil, err
	}
	dialCtx, cancel := context.WithTimeout(ctx, adapterkit.RequestTimeout)
	defer cancel()
	conn, _, err := websocket.Dial(dialCtx, open.URL, &websocket.DialOptions{HTTPClient: a.client}) //nolint:bodyclose // coder/websocket owns the handshake response; its body needs no close
	if err != nil {
		return nil, fmt.Errorf("could not connect to Slack: %w", adapterkit.Cause(err))
	}
	conn.SetReadLimit(readLimit)
	a.mu.Lock()
	a.conn = conn
	a.mu.Unlock()
	return conn, nil
}

func (a *Adapter) read(ctx context.Context, conn *websocket.Conn, sink channel.Sink) {
	defer func() { _ = conn.CloseNow() }()
	for {
		_, data, err := conn.Read(ctx)
		if err != nil {
			return
		}
		if a.onFrame(ctx, conn, data, sink) {
			return
		}
	}
}

// onFrame handles one envelope. It returns true when the socket must be replaced.
func (a *Adapter) onFrame(ctx context.Context, conn *websocket.Conn, data []byte, sink channel.Sink) bool {
	var env envelope
	if err := json.Unmarshal(data, &env); err != nil {
		return false
	}
	// Acknowledge first: Slack retries an envelope that is not acknowledged within 3 s.
	if env.EnvelopeID != "" {
		if raw, err := jsonx.Marshal(map[string]string{"envelope_id": env.EnvelopeID}); err == nil {
			_ = conn.Write(ctx, websocket.MessageText, raw)
		}
	}
	switch env.Type {
	case "events_api":
		if env.Payload != nil {
			a.onEvent(ctx, env.Payload.Event, sink)
		}
	case "interactive":
		a.onInteractive(ctx, env.Payload, sink)
	case "disconnect":
		// Slack is about to close this socket: open the next one now.
		return true
	}
	return false
}

func (a *Adapter) onEvent(ctx context.Context, ev *event, sink channel.Sink) {
	if ev == nil || (ev.Type != "message" && ev.Type != "app_mention") {
		return
	}
	// Edits, joins, bot posts and the like.
	if ev.Subtype != "" || ev.BotID != "" || ev.User == "" || ev.Channel == "" || ev.TS == "" {
		return
	}
	a.mu.Lock()
	botID := a.botUserID
	a.mu.Unlock()
	chat := channel.Chat{ID: ev.Channel, Type: channel.ChatGroup}
	if ev.ChannelType == "im" {
		chat.Type = channel.ChatDM
	}
	if ev.ThreadTS != "" && ev.ThreadTS != ev.TS {
		chat.ThreadID = ev.ThreadTS
	}
	sender := a.sender(ctx, ev.User, nil)
	if ctx.Err() != nil {
		return
	}
	sink.Message(channel.InboundMessage{
		MessageID:   ev.TS,
		Chat:        chat,
		Sender:      sender,
		Text:        a.readableText(ev.Text),
		MentionsBot: botID != "" && strings.Contains(ev.Text, "<@"+botID+">"),
		ReplyToBot:  botID != "" && ev.ParentUserID == botID,
	})
}

func (a *Adapter) onInteractive(ctx context.Context, p *payload, sink channel.Sink) {
	if p == nil || p.Type != "block_actions" || p.User == nil || p.User.ID == "" {
		return
	}
	channelID, messageTS, threadTS := "", "", ""
	if p.Channel != nil {
		channelID = p.Channel.ID
	}
	if p.Message != nil {
		messageTS, threadTS = p.Message.TS, p.Message.ThreadTS
	}
	if p.Container != nil {
		channelID = firstNonEmpty(channelID, p.Container.ChannelID)
		messageTS = firstNonEmpty(messageTS, p.Container.MessageTS)
		threadTS = firstNonEmpty(threadTS, p.Container.ThreadTS)
	}
	if channelID == "" || messageTS == "" {
		return
	}
	for _, action := range p.Actions {
		data := firstNonEmpty(action.Value, action.ActionID)
		if data == "" {
			continue
		}
		chat := channel.Chat{ID: channelID, Type: channel.ChatGroup}
		if strings.HasPrefix(channelID, "D") {
			chat.Type = channel.ChatDM
		}
		if threadTS != "" && threadTS != messageTS {
			chat.ThreadID = threadTS
		}
		sink.Button(channel.ButtonPress{Chat: chat, Sender: a.sender(ctx, p.User.ID, p.User), MessageID: messageTS, Data: data})
	}
}

var (
	slackLinkLabeled = regexp.MustCompile(`<(https?:[^|>]+)\|([^>]+)>`)
	slackLink        = regexp.MustCompile(`<(https?:[^>]+)>`)
)

// readableText turns Slack's "<@U123>" mentions of the bot into "@name", so its own
// mention can be removed by name, and unwraps links and escapes.
func (a *Adapter) readableText(text string) string {
	a.mu.Lock()
	botID, botName := a.botUserID, a.botName
	a.mu.Unlock()
	out := text
	if botName != "" && botID != "" {
		out = regexp.MustCompile(`<@`+regexp.QuoteMeta(botID)+`(\|[^>]*)?>`).ReplaceAllString(out, "@"+botName)
	}
	out = slackLinkLabeled.ReplaceAllString(out, "$2 ($1)")
	out = slackLink.ReplaceAllString(out, "$1")
	return strings.NewReplacer("&lt;", "<", "&gt;", ">", "&amp;", "&").Replace(out)
}

// sender is the sender's name from users.info (cached); the id alone when that fails.
func (a *Adapter) sender(ctx context.Context, userID string, hint *user) channel.Sender {
	a.mu.Lock()
	cached, ok := a.senders[userID]
	a.mu.Unlock()
	if ok {
		return cached
	}
	s := channel.Sender{ID: userID}
	if hint != nil {
		s.Username = hint.Username
	}
	var info struct {
		User struct {
			Name     string `json:"name"`
			RealName string `json:"real_name"`
			IsBot    bool   `json:"is_bot"`
			Profile  struct {
				DisplayName string `json:"display_name"`
			} `json:"profile"`
		} `json:"user"`
	}
	if err := a.apiForm(ctx, "users.info", url.Values{"user": {userID}}, a.botToken, &info); err == nil {
		u := info.User
		s = channel.Sender{ID: userID, Name: firstNonEmpty(u.Profile.DisplayName, u.RealName, u.Name), Username: u.Name, IsBot: u.IsBot}
	} else if hint != nil && hint.Name != "" {
		s.Name = hint.Name
	}
	a.mu.Lock()
	if len(a.senders) >= maxSenderCache {
		clear(a.senders)
	}
	a.senders[userID] = s
	a.mu.Unlock()
	return s
}

// api is one Web API call. Tokens are only in the Authorization header, never in an
// error message.
func (a *Adapter) api(ctx context.Context, method string, body any, token string, out any) error {
	raw, err := jsonx.Marshal(body)
	if err != nil {
		return err
	}
	return a.call(ctx, method, raw, "application/json; charset=utf-8", token, out)
}

// apiForm is api for read methods (users.info): Slack takes their arguments
// form-encoded, not as JSON.
func (a *Adapter) apiForm(ctx context.Context, method string, args url.Values, token string, out any) error {
	return a.call(ctx, method, []byte(args.Encode()), "application/x-www-form-urlencoded", token, out)
}

func (a *Adapter) call(ctx context.Context, method string, raw []byte, contentType string, token string, out any) error {
	ctx, cancel := context.WithTimeout(ctx, adapterkit.RequestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.baseURL+"/"+method, bytes.NewReader(raw))
	if err != nil {
		return errors.New("Slack " + method + ": could not build the request")
	}
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("Authorization", "Bearer "+token)
	res, err := a.client.Do(req)
	if err != nil {
		return fmt.Errorf("slack %s failed: %w", method, adapterkit.Cause(err))
	}
	defer res.Body.Close()
	if res.StatusCode == http.StatusTooManyRequests {
		retry := adapterkit.RetryAfterHeader(res)
		if retry == 0 {
			retry = 1
		}
		return &adapterkit.APIError{Status: 429, Msg: "Slack " + method + " was rate limited", RetryAfter: retry, Code: "ratelimited"}
	}
	var data json.RawMessage
	if err := json.NewDecoder(res.Body).Decode(&data); err != nil {
		return &adapterkit.APIError{Status: res.StatusCode, Msg: fmt.Sprintf("Slack %s returned HTTP %d", method, res.StatusCode)}
	}
	var head struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}
	if err := json.Unmarshal(data, &head); err != nil {
		return &adapterkit.APIError{Status: res.StatusCode, Msg: fmt.Sprintf("Slack %s returned HTTP %d", method, res.StatusCode)}
	}
	if !head.OK {
		desc := head.Error
		if desc == "" {
			desc = fmt.Sprintf("HTTP %d", res.StatusCode)
		}
		return &adapterkit.APIError{Status: res.StatusCode, Msg: "Slack " + method + " failed: " + desc, Code: head.Error}
	}
	if out != nil {
		return json.Unmarshal(data, out)
	}
	return nil
}

func isAuthError(code string) bool {
	switch code {
	case "invalid_auth", "not_authed", "account_inactive", "token_revoked", "not_allowed_token_type":
		return true
	}
	return false
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

// -- Slack shapes (the fields used here) ---------------------------------------

type envelope struct {
	Type       string   `json:"type"`
	EnvelopeID string   `json:"envelope_id"`
	Payload    *payload `json:"payload"`
}

type event struct {
	Type         string `json:"type"`
	Subtype      string `json:"subtype"`
	BotID        string `json:"bot_id"`
	User         string `json:"user"`
	Text         string `json:"text"`
	Channel      string `json:"channel"`
	ChannelType  string `json:"channel_type"`
	TS           string `json:"ts"`
	ThreadTS     string `json:"thread_ts"`
	ParentUserID string `json:"parent_user_id"`
}

type user struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Username string `json:"username"`
}

type payload struct {
	Event   *event `json:"event"`
	Type    string `json:"type"`
	User    *user  `json:"user"`
	Channel *struct {
		ID string `json:"id"`
	} `json:"channel"`
	Container *struct {
		ChannelID string `json:"channel_id"`
		MessageTS string `json:"message_ts"`
		ThreadTS  string `json:"thread_ts"`
	} `json:"container"`
	Message *struct {
		TS       string `json:"ts"`
		ThreadTS string `json:"thread_ts"`
	} `json:"message"`
	Actions []struct {
		ActionID string `json:"action_id"`
		Value    string `json:"value"`
	} `json:"actions"`
}
