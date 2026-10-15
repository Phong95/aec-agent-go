// Package telegram is the Telegram channel (A-100): the Bot API over plain HTTP, with no
// dependency, the telegramPlatform of core/ts.
//
// It uses long polling (getUpdates), so the host needs no public URL: create a bot with
// @BotFather, paste its token, and the bot starts answering. Only one process may poll a
// bot at a time; a second one (or a webhook set on the bot) makes Telegram answer 409
// Conflict, reported as "retrying".
//
// Phase 2: Markdown is sent as Telegram HTML (plain text when Telegram rejects it),
// buttons are inline keyboards whose presses arrive as callback queries, and Edit updates
// a sent message (streamed previews, settled prompts).
//
// It is an opt-in package, like the LLM providers under components/llmhub/providers:
// register it with aec.AgentCore.UseChannelPlatform(telegram.Platform()). It lives
// outside components/agent, whose packages may not reach the network (tests/arch); the
// contract it implements is components/channelplatformhub/channel.
package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Phong95/aec-agent-go/components/channelplatformhub/channel"
	"github.com/Phong95/aec-agent-go/internal/jsonx"
)

const (
	defaultAPIBaseURL         = "https://api.telegram.org"
	defaultPollTimeoutSeconds = 30
	// maxMessageLength is under Telegram's 4096-character limit, with headroom for how it counts.
	maxMessageLength = 4000
	requestTimeout   = 15 * time.Second
	maxBackoff       = 30 * time.Second
	// maxSendRetryAfter: a 429 on send is retried once when Telegram asks for at most this long a wait.
	maxSendRetryAfter = 30
)

// Options configures the adapters of the platform.
type Options struct {
	// APIBaseURL defaults to https://api.telegram.org; set it for a local Bot API server or tests.
	APIBaseURL string
	// PollTimeoutSeconds is how long one getUpdates call waits for messages (default 30).
	PollTimeoutSeconds int
	// Client is the HTTP client (default: a client without a timeout; every call has its own).
	Client *http.Client
}

// Platform returns the Telegram platform, for aec.AgentCore.UseChannelPlatform. A channel
// needs the "botToken" credential.
func Platform(opts ...Options) channel.Platform {
	var o Options
	if len(opts) > 0 {
		o = opts[0]
	}
	return platform{o: o}
}

type platform struct{ o Options }

func (platform) Name() string             { return "telegram" }
func (platform) DisplayName() string      { return "Telegram" }
func (platform) CredentialKeys() []string { return []string{"botToken"} }

func (p platform) New(ch channel.Channel) (channel.Adapter, error) {
	return NewAdapter(ch.Credentials["botToken"], p.o), nil
}

// Adapter is one bot connection.
type Adapter struct {
	token       string
	baseURL     string
	pollTimeout int
	client      *http.Client

	mu      sync.Mutex
	started bool
	cancel  context.CancelFunc
	done    chan struct{}
	offset  int64
	botID   int64
	botUser string
}

var (
	_ channel.Adapter       = (*Adapter)(nil)
	_ channel.TypingSender  = (*Adapter)(nil)
	_ channel.Editor        = (*Adapter)(nil)
	_ channel.CommandSetter = (*Adapter)(nil)
)

// NewAdapter returns an adapter for the bot token.
func NewAdapter(botToken string, o Options) *Adapter {
	base := strings.TrimRight(o.APIBaseURL, "/")
	if base == "" {
		base = defaultAPIBaseURL
	}
	poll := o.PollTimeoutSeconds
	if poll <= 0 {
		poll = defaultPollTimeoutSeconds
	}
	client := o.Client
	if client == nil {
		client = &http.Client{}
	}
	return &Adapter{token: botToken, baseURL: base, pollTimeout: poll, client: client}
}

// Capabilities reports the message length limit, and that Markdown and buttons are
// rendered.
func (a *Adapter) Capabilities() channel.Capabilities {
	return channel.Capabilities{MaxMessageLength: maxMessageLength, Markdown: true, Buttons: true}
}

// Start checks the token with getMe and starts long polling. The polling runs until Stop,
// not until ctx ends.
func (a *Adapter) Start(ctx context.Context, sink channel.Sink) (channel.AccountInfo, error) {
	if a.token == "" {
		return channel.AccountInfo{}, errors.New("a Telegram bot token is required")
	}
	a.mu.Lock()
	if a.started {
		a.mu.Unlock()
		return channel.AccountInfo{}, errors.New("the Telegram adapter is already started")
	}
	a.started = true
	a.mu.Unlock()

	var me tgUser
	if err := a.call(ctx, "getMe", map[string]any{}, requestTimeout, &me); err != nil {
		var apiErr *apiError
		if errors.As(err, &apiErr) && (apiErr.code == 401 || apiErr.code == 404) {
			return channel.AccountInfo{}, errors.New("Telegram rejected the bot token " + emDash + " check it with @BotFather")
		}
		return channel.AccountInfo{}, err
	}
	// The polling outlives the ctx of the call that started it; Stop ends it.
	loopCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	done := make(chan struct{})
	a.mu.Lock()
	a.botID, a.botUser = me.ID, me.Username
	a.cancel, a.done = cancel, done
	a.mu.Unlock()
	go func() {
		defer close(done)
		a.poll(loopCtx, sink)
	}()
	return channel.AccountInfo{ID: strconv.FormatInt(me.ID, 10), Username: me.Username, DisplayName: fullName(me)}, nil
}

// Stop ends the long poll and waits for it.
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

// Send posts plain text to the chat (in the thread, as a reply, when the target says so).
// -> Phase 2: Markdown as HTML (plain text again when Telegram cannot parse it), and
// buttons as an inline keyboard.
func (a *Adapter) Send(ctx context.Context, target channel.Target, m channel.OutboundMessage) (channel.MessageRef, error) {
	body := map[string]any{"chat_id": target.ChatID}
	if target.ThreadID != "" {
		body["message_thread_id"] = atoi(target.ThreadID)
	}
	if target.ReplyToMessageID != "" {
		body["reply_parameters"] = map[string]any{"message_id": atoi(target.ReplyToMessageID), "allow_sending_without_reply": true}
	}
	if len(m.Buttons) > 0 {
		body["reply_markup"] = inlineKeyboard(m.Buttons)
	}
	var sent tgMessage
	if err := a.withFormat(ctx, "sendMessage", body, m, &sent); err != nil {
		return channel.MessageRef{}, err
	}
	return channel.MessageRef{ChatID: strconv.FormatInt(sent.Chat.ID, 10), MessageID: strconv.FormatInt(sent.MessageID, 10)}, nil
}

// Edit replaces a sent message's text and buttons (none given = the keyboard is
// removed). Editing to the same text is not an error.
func (a *Adapter) Edit(ctx context.Context, ref channel.MessageRef, m channel.OutboundMessage) error {
	body := map[string]any{"chat_id": ref.ChatID, "message_id": atoi(ref.MessageID), "reply_markup": inlineKeyboard(m.Buttons)}
	err := a.withFormat(ctx, "editMessageText", body, m, nil)
	var apiErr *apiError
	if errors.As(err, &apiErr) && notModified.MatchString(apiErr.msg) {
		return nil
	}
	return err
}

var (
	notModified = regexp.MustCompile(`(?i)message is not modified`)
	parseFailed = regexp.MustCompile(`(?i)parse entities|can't find end|unsupported start tag`)
)

// withFormat calls sendMessage or editMessageText with the message's text: Markdown as
// HTML, and plain text again when Telegram cannot parse the HTML. A 429 asking for a
// short wait is retried once.
func (a *Adapter) withFormat(ctx context.Context, method string, body map[string]any, m channel.OutboundMessage, out any) error {
	formatted := maps.Clone(body)
	if m.Format == channel.FormatMarkdown {
		formatted["text"] = MarkdownToHTML(m.Text)
		formatted["parse_mode"] = "HTML"
	} else {
		formatted["text"] = m.Text
	}
	err := a.callWithRetry(ctx, method, formatted, out)
	var apiErr *apiError
	if m.Format != channel.FormatMarkdown || !errors.As(err, &apiErr) || apiErr.code != 400 || !parseFailed.MatchString(apiErr.msg) {
		return err
	}
	plain := maps.Clone(body)
	plain["text"] = m.Text
	return a.callWithRetry(ctx, method, plain, out)
}

func (a *Adapter) callWithRetry(ctx context.Context, method string, body any, out any) error {
	err := a.call(ctx, method, body, requestTimeout, out)
	var apiErr *apiError
	if errors.As(err, &apiErr) && apiErr.code == 429 && apiErr.retryAfter > 0 && apiErr.retryAfter <= maxSendRetryAfter {
		if !sleep(ctx, time.Duration(apiErr.retryAfter*float64(time.Second))) {
			return ctx.Err()
		}
		err = a.call(ctx, method, body, requestTimeout, out)
	}
	return err
}

// SetCommands sets the bot's command list (setMyCommands), which Telegram shows when
// the user types "/".
func (a *Adapter) SetCommands(ctx context.Context, commands []channel.BotCommand) error {
	list := make([]map[string]string, len(commands))
	for i, c := range commands {
		list[i] = map[string]string{"command": c.Name, "description": c.Description}
	}
	return a.call(ctx, "setMyCommands", map[string]any{"commands": list}, requestTimeout, nil)
}

// SendTyping sends the "typing" chat action.
func (a *Adapter) SendTyping(ctx context.Context, target channel.Target) error {
	body := map[string]any{"chat_id": target.ChatID, "action": "typing"}
	if target.ThreadID != "" {
		body["message_thread_id"] = atoi(target.ThreadID)
	}
	return a.call(ctx, "sendChatAction", body, requestTimeout, nil)
}

// ---------------------------------------------------------------------------

func (a *Adapter) poll(ctx context.Context, sink channel.Sink) {
	backoff := time.Second
	retrying := false
	for ctx.Err() == nil {
		var updates []tgUpdate
		err := a.call(ctx, "getUpdates", map[string]any{
			"offset": a.offset, "timeout": a.pollTimeout, "allowed_updates": []string{"message", "callback_query"},
		}, time.Duration(a.pollTimeout)*time.Second+requestTimeout, &updates)
		if err == nil {
			if retrying {
				retrying = false
				sink.Status(channel.AdapterRunning, "")
			}
			backoff = time.Second
			for _, u := range updates {
				// Confirms the update on the next call, so it is not delivered again.
				a.offset = max(a.offset, u.UpdateID+1)
				if u.CallbackQuery != nil {
					a.onCallback(ctx, *u.CallbackQuery, sink)
					continue
				}
				if m, ok := a.toInbound(u); ok {
					sink.Message(m)
				}
			}
			continue
		}
		if ctx.Err() != nil {
			return
		}
		var apiErr *apiError
		isAPI := errors.As(err, &apiErr)
		if isAPI && (apiErr.code == 401 || apiErr.code == 404) {
			sink.Status(channel.AdapterFailed, "Telegram rejected the bot token "+emDash+" it may have been revoked")
			return
		}
		retrying = true
		msg := err.Error()
		if isAPI && apiErr.code == 409 {
			msg = "Another process is reading this bot's messages, or a webhook is set on it " + emDash + " only one may be active"
		}
		sink.Status(channel.AdapterRetrying, msg)
		wait := backoff
		if isAPI && apiErr.retryAfter > 0 {
			wait = time.Duration(apiErr.retryAfter * float64(time.Second))
		}
		backoff = min(backoff*2, maxBackoff)
		if !sleep(ctx, wait) {
			return
		}
	}
}

// onCallback handles a button press: acknowledged at once (stops the button's spinner),
// then handed to the sink.
func (a *Adapter) onCallback(ctx context.Context, q tgCallbackQuery, sink channel.Sink) {
	_ = a.call(ctx, "answerCallbackQuery", map[string]any{"callback_query_id": q.ID}, requestTimeout, nil)
	if q.Message == nil || q.Data == "" {
		return
	}
	msg := q.Message
	chat := channel.Chat{ID: strconv.FormatInt(msg.Chat.ID, 10), Type: chatType(msg.Chat.Type), Title: msg.Chat.Title}
	if msg.IsTopicMessage && msg.MessageThreadID != 0 {
		chat.ThreadID = strconv.FormatInt(msg.MessageThreadID, 10)
	}
	sink.Button(channel.ButtonPress{
		Chat:      chat,
		Sender:    channel.Sender{ID: strconv.FormatInt(q.From.ID, 10), Name: fullName(q.From), Username: q.From.Username, IsBot: q.From.IsBot},
		MessageID: strconv.FormatInt(msg.MessageID, 10),
		Data:      q.Data,
	})
}

func (a *Adapter) toInbound(u tgUpdate) (channel.InboundMessage, bool) {
	msg := u.Message
	if msg == nil || msg.From == nil {
		return channel.InboundMessage{}, false
	}
	text := msg.Text
	entities := msg.Entities
	if text == "" {
		text, entities = msg.Caption, msg.CaptionEntities
	}
	a.mu.Lock()
	botID, botUser := a.botID, strings.ToLower(a.botUser)
	a.mu.Unlock()
	units := utf16Units(text)
	mentions := false
	for _, e := range entities {
		switch e.Type {
		case "mention":
			if botUser != "" && e.Offset+1 <= len(units) && e.Offset+e.Length <= len(units) &&
				strings.ToLower(utf16String(units[e.Offset+1:e.Offset+e.Length])) == botUser {
				mentions = true
			}
		case "text_mention":
			if e.User != nil && e.User.ID == botID {
				mentions = true
			}
		}
	}
	chat := channel.Chat{ID: strconv.FormatInt(msg.Chat.ID, 10), Type: chatType(msg.Chat.Type), Title: msg.Chat.Title}
	if msg.IsTopicMessage && msg.MessageThreadID != 0 {
		chat.ThreadID = strconv.FormatInt(msg.MessageThreadID, 10)
	}
	return channel.InboundMessage{
		MessageID: strconv.FormatInt(msg.MessageID, 10),
		Chat:      chat,
		Sender: channel.Sender{
			ID: strconv.FormatInt(msg.From.ID, 10), Name: fullName(*msg.From), Username: msg.From.Username, IsBot: msg.From.IsBot,
		},
		Text:        text,
		MentionsBot: mentions,
		ReplyToBot:  msg.ReplyTo != nil && msg.ReplyTo.From != nil && msg.ReplyTo.From.ID == botID,
	}, true
}

// call is one Bot API call. The token is only in the URL, never in an error message.
func (a *Adapter) call(ctx context.Context, method string, body any, timeout time.Duration, out any) error {
	raw, err := jsonx.Marshal(body)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.baseURL+"/bot"+a.token+"/"+method, bytes.NewReader(raw))
	if err != nil {
		return errors.New("Telegram " + method + ": could not build the request")
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := a.client.Do(req)
	if err != nil {
		// A *url.Error carries the URL, and with it the token: keep only the cause.
		var urlErr interface{ Unwrap() error }
		if errors.As(err, &urlErr) && urlErr.Unwrap() != nil {
			err = urlErr.Unwrap()
		}
		return fmt.Errorf("telegram %s failed: %w", method, err)
	}
	defer res.Body.Close()
	var data tgResponse
	if err := json.NewDecoder(res.Body).Decode(&data); err != nil {
		return &apiError{code: res.StatusCode, msg: fmt.Sprintf("Telegram %s returned HTTP %d", method, res.StatusCode)}
	}
	if !data.OK {
		code := data.ErrorCode
		if code == 0 {
			code = res.StatusCode
		}
		desc := data.Description
		if desc == "" {
			desc = fmt.Sprintf("HTTP %d", res.StatusCode)
		}
		e := &apiError{code: code, msg: "Telegram " + method + " failed: " + desc}
		if data.Parameters != nil {
			e.retryAfter = data.Parameters.RetryAfter
		}
		return e
	}
	if out != nil && len(data.Result) > 0 {
		return json.Unmarshal(data.Result, out)
	}
	return nil
}

type apiError struct {
	code       int
	msg        string
	retryAfter float64
}

func (e *apiError) Error() string { return e.msg }

// inlineKeyboard is the reply_markup of rows of buttons.
func inlineKeyboard(rows [][]channel.Button) map[string]any {
	keyboard := make([][]map[string]string, 0, len(rows))
	for _, row := range rows {
		r := make([]map[string]string, 0, len(row))
		for _, b := range row {
			r = append(r, map[string]string{"text": b.Label, "callback_data": b.Data})
		}
		keyboard = append(keyboard, r)
	}
	return map[string]any{"inline_keyboard": keyboard}
}

func chatType(t string) channel.ChatType {
	switch t {
	case "private":
		return channel.ChatDM
	case "channel":
		return channel.ChatChannel
	default:
		return channel.ChatGroup
	}
}

func fullName(u tgUser) string {
	name := strings.TrimSpace(strings.Join([]string{u.FirstName, u.LastName}, " "))
	if name != "" {
		return name
	}
	if u.Username != "" {
		return u.Username
	}
	return strconv.FormatInt(u.ID, 10)
}

func atoi(s string) int64 {
	n, _ := strconv.ParseInt(s, 10, 64)
	return n
}

// sleep waits d, or returns false when ctx ends first.
func sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}

// emDash is U+2014, built from a rune value so this source stays ASCII-only.
var emDash = string(rune(0x2014))

// -- Bot API shapes (the fields used here) ------------------------------------

type tgResponse struct {
	OK          bool            `json:"ok"`
	Result      json.RawMessage `json:"result"`
	ErrorCode   int             `json:"error_code"`
	Description string          `json:"description"`
	Parameters  *struct {
		RetryAfter float64 `json:"retry_after"`
	} `json:"parameters"`
}

type tgUser struct {
	ID        int64  `json:"id"`
	IsBot     bool   `json:"is_bot"`
	FirstName string `json:"first_name"`
	LastName  string `json:"last_name"`
	Username  string `json:"username"`
}

type tgEntity struct {
	Type   string  `json:"type"`
	Offset int     `json:"offset"`
	Length int     `json:"length"`
	User   *tgUser `json:"user"`
}

type tgMessage struct {
	MessageID       int64   `json:"message_id"`
	MessageThreadID int64   `json:"message_thread_id"`
	IsTopicMessage  bool    `json:"is_topic_message"`
	From            *tgUser `json:"from"`
	Chat            struct {
		ID    int64  `json:"id"`
		Type  string `json:"type"`
		Title string `json:"title"`
	} `json:"chat"`
	Text            string     `json:"text"`
	Caption         string     `json:"caption"`
	Entities        []tgEntity `json:"entities"`
	CaptionEntities []tgEntity `json:"caption_entities"`
	ReplyTo         *struct {
		From *tgUser `json:"from"`
	} `json:"reply_to_message"`
}

type tgCallbackQuery struct {
	ID      string     `json:"id"`
	From    tgUser     `json:"from"`
	Message *tgMessage `json:"message"`
	Data    string     `json:"data"`
}

type tgUpdate struct {
	UpdateID      int64            `json:"update_id"`
	Message       *tgMessage       `json:"message"`
	CallbackQuery *tgCallbackQuery `json:"callback_query"`
}
