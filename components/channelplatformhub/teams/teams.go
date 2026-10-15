// Package teams is the Microsoft Teams channel (A-100 phase 3) over the Bot Framework,
// the teamsPlatform of core/ts. No dependency.
//
// Teams cannot be polled: the Bot Framework POSTs every activity to the bot's messaging
// endpoint, so the host must be reachable from the internet. In the Azure Bot resource
// set the messaging endpoint to
//
//	https://<host>/webhooks/channels/<channel id>
//
// and add the Teams channel. Credentials: the bot's Microsoft App ID and a client secret,
// plus the tenant id for a single-tenant bot.
//
// Inbound: every request carries a JWT signed by the Bot Framework; it is checked
// (signature against the published keys, issuer, audience = app id, lifetime,
// serviceUrl) before anything is read. Outbound: replies go to the activity's serviceUrl
// with a token from Microsoft Entra (client credentials). A conversation's serviceUrl is
// only known once it has sent a message, so the bot cannot write first after a restart
// until then.
//
// It is an opt-in package, like channelplatformhub/telegram: register it with
// aec.AgentCore.UseChannelPlatform(teams.Platform()).
package teams

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/Phong95/aec-agent-go/components/channelplatformhub/channel"
	"github.com/Phong95/aec-agent-go/components/channelplatformhub/internal/adapterkit"
	"github.com/Phong95/aec-agent-go/internal/jsonx"
)

const (
	defaultOpenIDMetadataURL = "https://login.botframework.com/v1/.well-known/openidconfiguration"
	botFrameworkIssuer       = "https://api.botframework.com"
	tokenScope               = "https://api.botframework.com/.default" //nolint:gosec // an OAuth scope, not a credential
	maxMessageLength         = 4000
	// clockSkew is allowed on token lifetimes.
	clockSkew = 300 * time.Second
	// keysTTL: signing keys are read again after this long, or sooner for an unknown key id.
	keysTTL           = 24 * time.Hour
	keysMinRefresh    = 5 * time.Minute
	maxConversations  = 10000
	maxBodyBytes      = 8 << 20
	tokenRefreshEarly = 5 * time.Minute
)

// Options configures the adapters of the platform.
type Options struct {
	// OpenIDMetadataURL defaults to the Bot Framework's OpenID metadata; set it for tests.
	OpenIDMetadataURL string
	// TokenURL defaults to https://login.microsoftonline.com/<tenant>/oauth2/v2.0/token;
	// set it for tests.
	TokenURL string
	// Client is the HTTP client (default: a client without a timeout; every call has its own).
	Client *http.Client
	// Now is the clock, for tests (default time.Now).
	Now func() time.Time
}

// Platform returns the Microsoft Teams platform, for aec.AgentCore.UseChannelPlatform.
// A channel needs "appId" and "appPassword"; "tenantId" is optional.
func Platform(opts ...Options) channel.Platform {
	var o Options
	if len(opts) > 0 {
		o = opts[0]
	}
	return platform{o: o}
}

type platform struct{ o Options }

func (platform) Name() string                     { return "teams" }
func (platform) DisplayName() string              { return "Microsoft Teams" }
func (platform) CredentialKeys() []string         { return []string{"appId", "appPassword"} }
func (platform) OptionalCredentialKeys() []string { return []string{"tenantId"} }
func (platform) Ingress() channel.Ingress         { return channel.IngressWebhook }

func (p platform) New(ch channel.Channel) (channel.Adapter, error) {
	return NewAdapter(ch.Credentials["appId"], ch.Credentials["appPassword"], ch.Credentials["tenantId"], p.o), nil
}

var (
	_ channel.OptionalCredentialPlatform = platform{}
	_ channel.IngressPlatform            = platform{}
)

// Adapter is one bot registration.
type Adapter struct {
	appID       string
	appPassword string
	tenantID    string
	o           Options
	client      *http.Client
	now         func() time.Time

	mu    sync.Mutex
	sink  channel.Sink
	token string
	// tokenExpires is when token expires.
	tokenExpires time.Time
	keys         map[string]*rsa.PublicKey
	keysLoadedAt time.Time
	// serviceURLs maps a conversation id to the serviceUrl its activities came from
	// (verified); order is oldest first, for eviction.
	serviceURLs map[string]string
	order       []string
}

var (
	_ channel.Adapter        = (*Adapter)(nil)
	_ channel.Editor         = (*Adapter)(nil)
	_ channel.TypingSender   = (*Adapter)(nil)
	_ channel.WebhookHandler = (*Adapter)(nil)
)

// NewAdapter returns an adapter for the app registration.
func NewAdapter(appID, appPassword, tenantID string, o Options) *Adapter {
	client := o.Client
	if client == nil {
		client = &http.Client{}
	}
	now := o.Now
	if now == nil {
		now = time.Now
	}
	return &Adapter{
		appID: appID, appPassword: appPassword, tenantID: strings.TrimSpace(tenantID),
		o: o, client: client, now: now, serviceURLs: map[string]string{},
	}
}

// Capabilities reports the message length limit, and that Markdown and buttons are
// rendered.
func (a *Adapter) Capabilities() channel.Capabilities {
	return channel.Capabilities{MaxMessageLength: maxMessageLength, Markdown: true, Buttons: true}
}

// Start checks the credentials: a token is only issued for a valid app id and secret.
func (a *Adapter) Start(ctx context.Context, sink channel.Sink) (channel.AccountInfo, error) {
	if a.appID == "" || a.appPassword == "" {
		return channel.AccountInfo{}, errors.New("Microsoft Teams needs the bot's app id and a client secret") //nolint:staticcheck // a user-facing message, capitalised (same text as core/ts)
	}
	if _, err := a.accessToken(ctx); err != nil {
		var apiErr *adapterkit.APIError
		if errors.As(err, &apiErr) && (apiErr.Status == 400 || apiErr.Status == 401) {
			code := ""
			if apiErr.Code != "" {
				code = " (" + apiErr.Code + ")"
			}
			return channel.AccountInfo{}, errors.New("Microsoft rejected the app id or client secret" + code)
		}
		return channel.AccountInfo{}, err
	}
	a.mu.Lock()
	a.sink = sink
	a.mu.Unlock()
	return channel.AccountInfo{ID: a.appID, DisplayName: "Teams bot"}, nil
}

// Stop drops the sink: no message reaches it afterwards.
func (a *Adapter) Stop(context.Context) error {
	a.mu.Lock()
	a.sink = nil
	a.mu.Unlock()
	return nil
}

// HandleWebhook checks the Bot Framework's token, then delivers a message activity.
func (a *Adapter) HandleWebhook(ctx context.Context, req channel.WebhookRequest) channel.WebhookResponse {
	if !strings.EqualFold(req.Method, http.MethodPost) {
		return channel.WebhookResponse{Status: 405}
	}
	var act activity
	if err := json.Unmarshal([]byte(req.Body), &act); err != nil {
		return channel.WebhookResponse{Status: 400, Body: map[string]string{"error": "The body is not JSON"}}
	}
	claims, err := a.verifyToken(ctx, req.Headers["authorization"])
	if err != nil {
		return channel.WebhookResponse{Status: 401, Body: map[string]string{"error": "Unauthorized: " + err.Error()}}
	}
	if claims.ServiceURL != "" && act.ServiceURL != "" && trimSlash(claims.ServiceURL) != trimSlash(act.ServiceURL) {
		return channel.WebhookResponse{Status: 401, Body: map[string]string{"error": "Unauthorized: the serviceUrl does not match the token"}}
	}
	if act.Conversation != nil && act.Conversation.ID != "" && act.ServiceURL != "" {
		a.rememberServiceURL(act.Conversation.ID, act.ServiceURL)
	}
	if act.Type == "message" {
		a.onMessage(act)
	}
	return channel.WebhookResponse{Status: 200, Body: map[string]any{}}
}

// Send posts a message activity (an Adaptive Card when it has buttons) to the conversation.
func (a *Adapter) Send(ctx context.Context, target channel.Target, m channel.OutboundMessage) (channel.MessageRef, error) {
	serviceURL, err := a.serviceURLFor(target.ChatID)
	if err != nil {
		return channel.MessageRef{}, err
	}
	body := messageActivity(m)
	if target.ReplyToMessageID != "" {
		body["replyToId"] = target.ReplyToMessageID
	}
	var sent struct {
		ID string `json:"id"`
	}
	err = adapterkit.WithRetry(ctx, func() error {
		return a.connector(ctx, http.MethodPost, serviceURL, "/v3/conversations/"+encodeURIComponent(target.ChatID)+"/activities", body, &sent)
	})
	if err != nil {
		return channel.MessageRef{}, err
	}
	return channel.MessageRef{ChatID: target.ChatID, MessageID: sent.ID}, nil
}

// Edit replaces a sent activity (PUT); no buttons given = the card is replaced by text.
func (a *Adapter) Edit(ctx context.Context, ref channel.MessageRef, m channel.OutboundMessage) error {
	if ref.MessageID == "" {
		return nil
	}
	serviceURL, err := a.serviceURLFor(ref.ChatID)
	if err != nil {
		return err
	}
	body := messageActivity(m)
	body["id"] = ref.MessageID
	return adapterkit.WithRetry(ctx, func() error {
		return a.connector(ctx, http.MethodPut, serviceURL,
			"/v3/conversations/"+encodeURIComponent(ref.ChatID)+"/activities/"+encodeURIComponent(ref.MessageID), body, nil)
	})
}

// SendTyping sends a typing activity.
func (a *Adapter) SendTyping(ctx context.Context, target channel.Target) error {
	serviceURL, err := a.serviceURLFor(target.ChatID)
	if err != nil {
		return err
	}
	return a.connector(ctx, http.MethodPost, serviceURL, "/v3/conversations/"+encodeURIComponent(target.ChatID)+"/activities", map[string]any{"type": "typing"}, nil)
}

// ---------------------------------------------------------------------------

var (
	atTags     = regexp.MustCompile(`</?at>`)
	anyTag     = regexp.MustCompile(`<[^>]+>`)
	spaceRuns  = regexp.MustCompile(`[ \t]{2,}`)
	entityRepl = strings.NewReplacer("&nbsp;", " ", "&lt;", "<", "&gt;", ">", "&quot;", `"`, "&amp;", "&")
)

func (a *Adapter) onMessage(act activity) {
	a.mu.Lock()
	sink := a.sink
	a.mu.Unlock()
	if sink == nil || act.Conversation == nil || act.Conversation.ID == "" || act.From == nil || act.From.ID == "" || act.ID == "" {
		return
	}
	chat := channel.Chat{ID: act.Conversation.ID, Type: channel.ChatGroup, Title: act.Conversation.Name}
	if act.Conversation.ConversationType == "personal" {
		chat.Type = channel.ChatDM
	}
	// The Entra object id is stable across chats and tenants' bots; the Teams id is the fallback.
	sender := channel.Sender{ID: act.From.ID, Name: act.From.Name, IsBot: act.From.Role == "bot"}
	if act.From.AADObjectID != "" {
		sender.ID = act.From.AADObjectID
	}
	// An Adaptive Card button (Action.Submit) arrives as a message with a value.
	if len(act.Value) > 0 {
		var value map[string]any
		if json.Unmarshal(act.Value, &value) == nil {
			if data, ok := value["aec"].(string); ok {
				messageID := act.ReplyToID
				if messageID == "" {
					messageID = act.ID
				}
				sink.Button(channel.ButtonPress{Chat: chat, Sender: sender, MessageID: messageID, Data: data})
				return
			}
		}
	}
	botID := ""
	if act.Recipient != nil {
		botID = act.Recipient.ID
	}
	text := act.Text
	mentions := false
	for _, e := range act.Entities {
		if e.Type == "mention" && botID != "" && e.Mentioned != nil && e.Mentioned.ID == botID {
			mentions = true
			if e.Text != "" {
				text = strings.ReplaceAll(text, e.Text, " ")
			}
		}
	}
	text = anyTag.ReplaceAllString(atTags.ReplaceAllString(text, ""), "")
	text = strings.TrimSpace(spaceRuns.ReplaceAllString(entityRepl.Replace(text), " "))
	sink.Message(channel.InboundMessage{MessageID: act.ID, Chat: chat, Sender: sender, Text: text, MentionsBot: mentions})
}

// messageActivity is a message activity: Markdown text, or an Adaptive Card when it has
// buttons.
func messageActivity(m channel.OutboundMessage) map[string]any {
	format := "plain"
	if m.Format == channel.FormatMarkdown {
		format = "markdown"
	}
	if len(m.Buttons) == 0 {
		return map[string]any{"type": "message", "text": m.Text, "textFormat": format, "attachments": []any{}}
	}
	var actions []any
	for _, row := range m.Buttons {
		for _, b := range row {
			actions = append(actions, map[string]any{"type": "Action.Submit", "title": b.Label, "data": map[string]any{"aec": b.Data}})
		}
	}
	return map[string]any{
		"type": "message",
		"attachments": []any{map[string]any{
			"contentType": "application/vnd.microsoft.card.adaptive",
			"content": map[string]any{
				"type":    "AdaptiveCard",
				"$schema": "http://adaptivecards.io/schemas/adaptive-card.json",
				"version": "1.4",
				"body":    []any{map[string]any{"type": "TextBlock", "text": m.Text, "wrap": true}},
				"actions": actions,
			},
		}},
	}
}

func (a *Adapter) serviceURLFor(conversationID string) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	u, ok := a.serviceURLs[conversationID]
	if !ok {
		return "", errors.New("this Teams conversation has not sent the bot a message since the server started")
	}
	return u, nil
}

func (a *Adapter) rememberServiceURL(conversationID, serviceURL string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if _, ok := a.serviceURLs[conversationID]; !ok {
		if len(a.order) >= maxConversations {
			delete(a.serviceURLs, a.order[0])
			a.order = a.order[1:]
		}
		a.order = append(a.order, conversationID)
	}
	a.serviceURLs[conversationID] = serviceURL
}

// -- Outbound auth -----------------------------------------------------------

func (a *Adapter) accessToken(ctx context.Context) (string, error) {
	a.mu.Lock()
	if a.token != "" && a.tokenExpires.Add(-tokenRefreshEarly).After(a.now()) {
		token := a.token
		a.mu.Unlock()
		return token, nil
	}
	a.mu.Unlock()
	tenant := a.tenantID
	if tenant == "" {
		tenant = "botframework.com"
	}
	tokenURL := a.o.TokenURL
	if tokenURL == "" {
		tokenURL = "https://login.microsoftonline.com/" + url.PathEscape(tenant) + "/oauth2/v2.0/token"
	}
	form := url.Values{
		"grant_type": {"client_credentials"}, "client_id": {a.appID}, "client_secret": {a.appPassword}, "scope": {tokenScope},
	}
	ctx, cancel := context.WithTimeout(ctx, adapterkit.RequestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", errors.New("microsoft token request: could not build the request")
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	res, err := a.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("microsoft token request failed: %w", adapterkit.Cause(err))
	}
	defer res.Body.Close()
	var data struct {
		AccessToken string  `json:"access_token"`
		ExpiresIn   float64 `json:"expires_in"`
		Error       string  `json:"error"`
	}
	_ = json.NewDecoder(io.LimitReader(res.Body, maxBodyBytes)).Decode(&data)
	if res.StatusCode < 200 || res.StatusCode >= 300 || data.AccessToken == "" {
		desc := data.Error
		if desc == "" {
			desc = fmt.Sprintf("HTTP %d", res.StatusCode)
		}
		return "", &adapterkit.APIError{Status: res.StatusCode, Msg: "Microsoft token request failed: " + desc, Code: data.Error}
	}
	expires := data.ExpiresIn
	if expires <= 0 {
		expires = 3600
	}
	a.mu.Lock()
	a.token, a.tokenExpires = data.AccessToken, a.now().Add(time.Duration(expires*float64(time.Second)))
	a.mu.Unlock()
	return data.AccessToken, nil
}

// connector is one Bot Connector call. It is only made to a serviceUrl taken from a
// verified activity.
func (a *Adapter) connector(ctx context.Context, method, serviceURL, path string, body any, out any) error {
	token, err := a.accessToken(ctx)
	if err != nil {
		return err
	}
	raw, err := jsonx.Marshal(body)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, adapterkit.RequestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, trimSlash(serviceURL)+path, bytes.NewReader(raw))
	if err != nil {
		return errors.New("Teams " + method + " activity: could not build the request")
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	res, err := a.client.Do(req)
	if err != nil {
		return fmt.Errorf("teams %s activity failed: %w", method, adapterkit.Cause(err))
	}
	defer res.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(res.Body, maxBodyBytes))
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		var e struct {
			Error struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		_ = json.Unmarshal(data, &e)
		desc := e.Error.Message
		if desc == "" {
			desc = e.Error.Code
		}
		if desc == "" {
			desc = fmt.Sprintf("HTTP %d", res.StatusCode)
		}
		apiErr := &adapterkit.APIError{Status: res.StatusCode, Msg: "Teams " + method + " activity failed: " + desc, Code: e.Error.Code}
		if res.StatusCode == http.StatusTooManyRequests {
			apiErr.RetryAfter = adapterkit.RetryAfterHeader(res)
			if apiErr.RetryAfter == 0 {
				apiErr.RetryAfter = 1
			}
		}
		return apiErr
	}
	if out != nil && len(data) > 0 {
		_ = json.Unmarshal(data, out)
	}
	return nil
}

// -- Inbound auth (Bot Framework JWT) ----------------------------------------

type jwtClaims struct {
	Iss        string          `json:"iss"`
	Aud        json.RawMessage `json:"aud"`
	Exp        *float64        `json:"exp"`
	Nbf        *float64        `json:"nbf"`
	ServiceURL string          `json:"serviceurl"`
}

var bearer = regexp.MustCompile(`(?i)^Bearer\s+(\S+)$`)

func (a *Adapter) verifyToken(ctx context.Context, authorization string) (jwtClaims, error) {
	m := bearer.FindStringSubmatch(strings.TrimSpace(authorization))
	if m == nil {
		return jwtClaims{}, errors.New("no bearer token")
	}
	parts := strings.Split(m[1], ".")
	if len(parts) != 3 {
		return jwtClaims{}, errors.New("malformed token")
	}
	var header struct {
		Alg string `json:"alg"`
		Kid string `json:"kid"`
	}
	var claims jwtClaims
	rawHeader, err1 := b64url(parts[0])
	rawClaims, err2 := b64url(parts[1])
	sig, err3 := b64url(parts[2])
	if err1 != nil || err2 != nil || err3 != nil || json.Unmarshal(rawHeader, &header) != nil || json.Unmarshal(rawClaims, &claims) != nil {
		return jwtClaims{}, errors.New("malformed token")
	}
	if header.Alg != "RS256" || header.Kid == "" {
		return jwtClaims{}, errors.New("unsupported token algorithm")
	}
	key, err := a.signingKey(ctx, header.Kid)
	if err != nil {
		return jwtClaims{}, err
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if rsa.VerifyPKCS1v15(key, crypto.SHA256, digest[:], sig) != nil {
		return jwtClaims{}, errors.New("bad signature")
	}
	if claims.Iss != botFrameworkIssuer {
		return jwtClaims{}, errors.New("wrong issuer")
	}
	var audiences []string
	var single string
	if json.Unmarshal(claims.Aud, &single) == nil {
		audiences = []string{single}
	} else {
		_ = json.Unmarshal(claims.Aud, &audiences)
	}
	if !slices.Contains(audiences, a.appID) {
		return jwtClaims{}, errors.New("wrong audience")
	}
	nowS := float64(a.now().UnixMilli()) / 1000
	skew := clockSkew.Seconds()
	if claims.Exp == nil || *claims.Exp+skew < nowS {
		return jwtClaims{}, errors.New("token expired")
	}
	if claims.Nbf != nil && *claims.Nbf-skew > nowS {
		return jwtClaims{}, errors.New("token not valid yet")
	}
	return claims, nil
}

// signingKey is the Bot Framework's signing key with this id, from its OpenID metadata
// (cached).
func (a *Adapter) signingKey(ctx context.Context, kid string) (*rsa.PublicKey, error) {
	a.mu.Lock()
	loaded := a.keys != nil
	age := a.now().Sub(a.keysLoadedAt)
	fresh := loaded && age < keysTTL
	known := a.keys[kid]
	a.mu.Unlock()
	if known != nil && fresh {
		return known, nil
	}
	// An unknown key id reloads the keys, at most every few minutes.
	if !loaded || !fresh || age > keysMinRefresh {
		if err := a.loadKeys(ctx); err != nil {
			return nil, err
		}
	}
	a.mu.Lock()
	key := a.keys[kid]
	a.mu.Unlock()
	if key == nil {
		return nil, errors.New("unknown signing key")
	}
	return key, nil
}

func (a *Adapter) loadKeys(ctx context.Context) error {
	metaURL := a.o.OpenIDMetadataURL
	if metaURL == "" {
		metaURL = defaultOpenIDMetadataURL
	}
	var meta struct {
		JWKSURI string `json:"jwks_uri"`
	}
	if err := a.getJSON(ctx, metaURL, &meta); err != nil {
		return err
	}
	if meta.JWKSURI == "" {
		return errors.New("the Bot Framework's OpenID metadata has no jwks_uri")
	}
	var jwks struct {
		Keys []struct {
			Kid          string   `json:"kid"`
			Kty          string   `json:"kty"`
			N            string   `json:"n"`
			E            string   `json:"e"`
			Endorsements []string `json:"endorsements"`
		} `json:"keys"`
	}
	if err := a.getJSON(ctx, meta.JWKSURI, &jwks); err != nil {
		return err
	}
	keys := map[string]*rsa.PublicKey{}
	for _, k := range jwks.Keys {
		// Keys endorsed for other channels only are not used for Teams.
		if k.Kid == "" || k.Kty != "RSA" {
			continue
		}
		if len(k.Endorsements) > 0 && !slices.Contains(k.Endorsements, "msteams") {
			continue
		}
		n, errN := b64url(k.N)
		e, errE := b64url(k.E)
		if errN != nil || errE != nil || len(n) == 0 || len(e) == 0 {
			continue
		}
		exp := new(big.Int).SetBytes(e)
		if !exp.IsInt64() || exp.Int64() > 1<<31 {
			continue
		}
		keys[k.Kid] = &rsa.PublicKey{N: new(big.Int).SetBytes(n), E: int(exp.Int64())}
	}
	a.mu.Lock()
	a.keys, a.keysLoadedAt = keys, a.now()
	a.mu.Unlock()
	return nil
}

func (a *Adapter) getJSON(ctx context.Context, u string, out any) error {
	ctx, cancel := context.WithTimeout(ctx, adapterkit.RequestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return errors.New("could not read the Bot Framework's signing keys")
	}
	res, err := a.client.Do(req)
	if err != nil {
		return fmt.Errorf("could not read the Bot Framework's signing keys: %w", adapterkit.Cause(err))
	}
	defer res.Body.Close()
	if err := json.NewDecoder(io.LimitReader(res.Body, maxBodyBytes)).Decode(out); err != nil {
		return errors.New("could not read the Bot Framework's signing keys")
	}
	return nil
}

func b64url(s string) ([]byte, error) {
	return base64.RawURLEncoding.DecodeString(strings.TrimRight(s, "="))
}

func trimSlash(u string) string { return strings.TrimRight(u, "/") }

// encodeURIComponent escapes like JavaScript's encodeURIComponent (":" ";" "=" "@" too).
func encodeURIComponent(s string) string {
	return strings.ReplaceAll(url.QueryEscape(s), "+", "%20")
}

// -- Bot Framework shapes (the fields used here) -------------------------------

type activity struct {
	Type       string          `json:"type"`
	ID         string          `json:"id"`
	ServiceURL string          `json:"serviceUrl"`
	Text       string          `json:"text"`
	Value      json.RawMessage `json:"value"`
	ReplyToID  string          `json:"replyToId"`
	From       *struct {
		ID          string `json:"id"`
		Name        string `json:"name"`
		AADObjectID string `json:"aadObjectId"`
		Role        string `json:"role"`
	} `json:"from"`
	Recipient *struct {
		ID string `json:"id"`
	} `json:"recipient"`
	Conversation *struct {
		ID               string `json:"id"`
		ConversationType string `json:"conversationType"`
		Name             string `json:"name"`
	} `json:"conversation"`
	Entities []struct {
		Type      string `json:"type"`
		Text      string `json:"text"`
		Mentioned *struct {
			ID string `json:"id"`
		} `json:"mentioned"`
	} `json:"entities"`
}
