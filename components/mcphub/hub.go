// Package mcphub is the global library of MCP server definitions (A-208) over
// persistence.McpServerStore - the McpHub of core/ts - and the registry of the transports
// agents reach them through (streamable HTTP is built in; stdio is the opt-in
// mcphub/stdio package, RegisterTransport).
//
// App servers are registered in memory; custom servers live in the
// persistence.McpServerStore returned by the getStore function on every load and write,
// so AgentCore.UsePersistence can swap the store (and calls Reload). The hub only holds
// definitions: connections belong to each agent's MCP manager, which hears
// mcp-server:changed and reconnects or closes.
//
// A-214: the hub also owns the OAuth sign-ins of its HTTP servers (OAuth, the
// mcphub/mcpoauth package), one per server, kept in the persistence.McpAuthStore of
// WithAuthStore (in memory by default) and sealed with WithSealer.
package mcphub

import (
	"cmp"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"

	"github.com/Phong95/aec-agent-go/components/events"
	"github.com/Phong95/aec-agent-go/components/mcphub/mcpclient"
	"github.com/Phong95/aec-agent-go/components/mcphub/mcpoauth"
	"github.com/Phong95/aec-agent-go/components/mcphub/mcpserver"
	"github.com/Phong95/aec-agent-go/components/persistence"
	"github.com/Phong95/aec-agent-go/internal/nilcheck"
	"github.com/Phong95/aec-agent-go/kernel"
)

// errNoStore is returned when getStore yields no store.
var errNoStore = errors.New("mcphub: the persistence MCP server store is not configured")

// McpHub is the global library of MCP server definitions - like the SubagentHub. Two
// kinds of entries:
//   - registered (app servers, AgentCore.UseMcp): in memory, read-only at runtime;
//   - custom (created at runtime): persisted in the McpServerStore, loaded on first use
//     and cached - write custom servers through this hub so the cache stays current.
//
// Registered ids win: a stored custom server with the same id is ignored (logged).
// Agents enable servers by id (AgentProfile.Mcp.Servers). Create, Update and Delete
// publish mcp-server:changed (WithEvents); each agent's MCP manager reconnects or closes,
// and AgentCore drops a deleted server from every agent's profile.
//
// Concurrency: Create, Update, Delete and the lazy load are serialised by one mutex;
// Reload only invalidates the cache. Every Server the hub returns is a deep copy. It is
// safe for concurrent use.
type McpHub struct {
	getStore func() persistence.McpServerStore
	now      func() int64
	logger   *slog.Logger
	events   events.Bus

	opMu sync.Mutex

	mu         sync.RWMutex
	registered map[string]mcpserver.Server
	custom     map[string]mcpserver.Server
	loaded     bool
	generation uint64
	transports map[mcpserver.TransportType]mcpclient.Factory

	// A-214: the OAuth sign-ins and what they are built from (options).
	oauth      *mcpoauth.OAuth
	authStore  func() persistence.McpAuthStore
	sealer     mcpoauth.Sealer
	httpClient *http.Client
}

// NewMcpHub returns an empty hub with the http transport registered. getStore is
// resolved on every load and write, so AgentCore.UsePersistence can swap the store.
func NewMcpHub(getStore func() persistence.McpServerStore, opts ...Option) *McpHub {
	h := &McpHub{
		getStore:   getStore,
		now:        kernel.NowMillis,
		logger:     slog.Default(),
		registered: map[string]mcpserver.Server{},
		custom:     map[string]mcpserver.Server{},
		transports: map[mcpserver.TransportType]mcpclient.Factory{mcpserver.TransportHTTP: mcpclient.HTTPFactory},
	}
	for _, o := range opts {
		if o != nil {
			o(h)
		}
	}
	authStore := h.authStore
	if authStore == nil {
		memory := persistence.NewMemoryMcpAuthStore()
		authStore = func() persistence.McpAuthStore { return memory }
	}
	h.oauth = mcpoauth.New(mcpoauth.Options{
		GetServer: h.Get,
		GetStore:  authStore,
		Sealer:    h.sealer,
		Events:    h.events,
		Client:    h.httpClient,
		Logger:    h.logger,
	})
	return h
}

// OAuth returns the OAuth sign-ins of the HTTP servers (A-214): one per server, shared by
// every agent that uses it.
func (h *McpHub) OAuth() *mcpoauth.OAuth { return h.oauth }

// RegisterTransport sets the factory agents build a transport type's connections with,
// e.g. RegisterTransport(mcpserver.TransportStdio, stdio.Factory). It replaces one of
// the same type; a nil factory removes it.
func (h *McpHub) RegisterTransport(kind mcpserver.TransportType, f mcpclient.Factory) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if f == nil {
		delete(h.transports, kind)
		return
	}
	h.transports[kind] = f
}

// NewTransport builds the transport of cfg with the factory of its type. It returns a
// *mcpserver.Error ("invalid_argument") when no factory is registered for the type.
func (h *McpHub) NewTransport(cfg mcpserver.Transport, tc mcpclient.TransportContext) (mcpclient.Transport, error) {
	h.mu.RLock()
	f := h.transports[cfg.Type]
	h.mu.RUnlock()
	if f == nil {
		return nil, mcpserver.NewError("no MCP transport for "+string(cfg.Type)+" is registered on this host", mcpserver.CodeInvalidArgument)
	}
	return f(cfg, tc)
}

// Register adds an app server. It returns a *mcpserver.Error ("invalid_argument",
// "already_exists").
func (h *McpHub) Register(def mcpserver.Definition) (mcpserver.Server, error) {
	s, err := ToServer(def, mcpserver.SourceApp)
	if err != nil {
		return mcpserver.Server{}, err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, taken := h.registered[s.ID]; taken {
		return mcpserver.Server{}, mcpserver.NewError(`MCP server "`+s.ID+`" already registered`, mcpserver.CodeAlreadyExists)
	}
	h.registered[s.ID] = s
	return s.Clone(), nil
}

// Get returns a registered or custom server by id; the bool is false when there is none.
func (h *McpHub) Get(ctx context.Context, id string) (mcpserver.Server, bool, error) {
	h.mu.RLock()
	s, ok := h.registered[id]
	h.mu.RUnlock()
	if ok {
		return s.Clone(), true, nil
	}
	var found mcpserver.Server
	var hit bool
	err := h.readLoaded(ctx, func() {
		if c, has := h.custom[id]; has {
			found, hit = c.Clone(), true
		}
	})
	if err != nil {
		return mcpserver.Server{}, false, err
	}
	return found, hit, nil
}

// List returns every server, registered first, each group sorted by id.
func (h *McpHub) List(ctx context.Context) ([]mcpserver.Server, error) {
	var out []mcpserver.Server
	err := h.readLoaded(ctx, func() {
		out = make([]mcpserver.Server, 0, len(h.registered)+len(h.custom))
		out = appendSorted(out, h.registered)
		out = appendSorted(out, h.custom)
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// Create adds a custom server and publishes mcp-server:changed ("created"). It returns
// a *mcpserver.Error ("invalid_argument", "already_exists").
func (h *McpHub) Create(ctx context.Context, def mcpserver.Definition) (mcpserver.Server, error) {
	s, err := h.createLocked(ctx, def)
	if err != nil {
		return mcpserver.Server{}, err
	}
	h.changed(ctx, s.ID, "created")
	return s, nil
}

func (h *McpHub) createLocked(ctx context.Context, def mcpserver.Definition) (mcpserver.Server, error) {
	s, err := ToServer(def, mcpserver.SourceCustom)
	if err != nil {
		return mcpserver.Server{}, err
	}
	now := kernel.MillisToISO(h.now())
	s.CreatedAt, s.UpdatedAt = now, now
	h.opMu.Lock()
	defer h.opMu.Unlock()
	if err := h.ensureLoadedLocked(ctx); err != nil {
		return mcpserver.Server{}, err
	}
	if h.exists(s.ID) {
		return mcpserver.Server{}, mcpserver.NewError(`An MCP server with id "`+s.ID+`" already exists`, mcpserver.CodeAlreadyExists)
	}
	if err := h.save(ctx, s); err != nil {
		return mcpserver.Server{}, err
	}
	h.cache(s)
	return s.Clone(), nil
}

// Update replaces a custom server's definition; its id stays (def.ID is ignored). App
// servers can't be changed ("already_exists"); an unknown id is "not_found". It
// publishes mcp-server:changed ("updated").
func (h *McpHub) Update(ctx context.Context, id string, def mcpserver.Definition) (mcpserver.Server, error) {
	s, err := h.updateLocked(ctx, id, def)
	if err != nil {
		return mcpserver.Server{}, err
	}
	h.changed(ctx, id, "updated")
	return s, nil
}

func (h *McpHub) updateLocked(ctx context.Context, id string, def mcpserver.Definition) (mcpserver.Server, error) {
	h.opMu.Lock()
	defer h.opMu.Unlock()
	existing, err := h.requireCustom(ctx, id)
	if err != nil {
		return mcpserver.Server{}, err
	}
	def.ID = id
	s, err := ToServer(def, mcpserver.SourceCustom)
	if err != nil {
		return mcpserver.Server{}, err
	}
	s.CreatedAt = existing.CreatedAt
	s.UpdatedAt = kernel.MillisToISO(h.now())
	if err := h.save(ctx, s); err != nil {
		return mcpserver.Server{}, err
	}
	h.cache(s)
	return s.Clone(), nil
}

// Delete removes a custom server, then publishes mcp-server:changed ("deleted") with ctx
// and returns once its subscribers returned - so the agents that used it have closed it
// and dropped it from their profiles.
func (h *McpHub) Delete(ctx context.Context, id string) error {
	if err := h.deleteLocked(ctx, id); err != nil {
		return err
	}
	// A-214: and its sign-in.
	if err := h.oauth.Forget(ctx, id); err != nil {
		h.logger.Warn(`[mcp] could not delete the sign-in of "`+id+`"`, "error", err)
	}
	h.changed(ctx, id, "deleted")
	return nil
}

func (h *McpHub) deleteLocked(ctx context.Context, id string) error {
	h.opMu.Lock()
	defer h.opMu.Unlock()
	if _, err := h.requireCustom(ctx, id); err != nil {
		return err
	}
	store, err := h.store()
	if err != nil {
		return err
	}
	if err := store.Delete(ctx, id); err != nil {
		return err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.custom, id)
	return nil
}

// Reload forgets the cached custom servers and sign-ins (the store was swapped).
func (h *McpHub) Reload() {
	h.mu.Lock()
	h.generation++
	h.loaded = false
	h.custom = map[string]mcpserver.Server{}
	h.mu.Unlock()
	h.oauth.Reload()
}

func (h *McpHub) changed(ctx context.Context, id, change string) {
	if h.events != nil {
		events.Publish(ctx, h.events, events.McpServerChanged, "", events.McpServerChangedPayload{ServerID: id, Change: change})
	}
}

func (h *McpHub) exists(id string) bool {
	h.mu.RLock()
	defer h.mu.RUnlock()
	_, reg := h.registered[id]
	_, cus := h.custom[id]
	return reg || cus
}

func (h *McpHub) cache(s mcpserver.Server) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.loaded {
		h.custom[s.ID] = s
	}
}

func (h *McpHub) save(ctx context.Context, s mcpserver.Server) error {
	store, err := h.store()
	if err != nil {
		return err
	}
	return store.Save(ctx, s.Clone())
}

func (h *McpHub) store() (persistence.McpServerStore, error) {
	if h.getStore == nil {
		return nil, errNoStore
	}
	store := h.getStore()
	if nilcheck.IsNil(store) {
		return nil, errNoStore
	}
	return store, nil
}

func (h *McpHub) requireCustom(ctx context.Context, id string) (mcpserver.Server, error) {
	h.mu.RLock()
	_, isRegistered := h.registered[id]
	h.mu.RUnlock()
	if isRegistered {
		return mcpserver.Server{}, mcpserver.NewError(`"`+id+`" is an app MCP server and can't be changed`, mcpserver.CodeAlreadyExists)
	}
	if err := h.ensureLoadedLocked(ctx); err != nil {
		return mcpserver.Server{}, err
	}
	h.mu.RLock()
	defer h.mu.RUnlock()
	s, ok := h.custom[id]
	if !ok {
		return mcpserver.Server{}, mcpserver.NewError(`No MCP server with id "`+id+`"`, mcpserver.CodeNotFound)
	}
	return s, nil
}

func (h *McpHub) readLoaded(ctx context.Context, read func()) error {
	for {
		if err := h.ensureLoaded(ctx); err != nil {
			return err
		}
		h.mu.RLock()
		if !h.loaded {
			h.mu.RUnlock()
			if err := ctx.Err(); err != nil {
				return err
			}
			continue
		}
		read()
		h.mu.RUnlock()
		return nil
	}
}

func (h *McpHub) ensureLoaded(ctx context.Context) error {
	h.mu.RLock()
	loaded := h.loaded
	h.mu.RUnlock()
	if loaded {
		return nil
	}
	h.opMu.Lock()
	defer h.opMu.Unlock()
	return h.ensureLoadedLocked(ctx)
}

func (h *McpHub) ensureLoadedLocked(ctx context.Context) error {
	for {
		h.mu.RLock()
		loaded, generation := h.loaded, h.generation
		h.mu.RUnlock()
		if loaded {
			return nil
		}
		custom, err := h.readStore(ctx)
		if err != nil {
			return err
		}
		h.mu.Lock()
		if h.generation == generation {
			h.custom = custom
			h.loaded = true
			h.mu.Unlock()
			return nil
		}
		h.mu.Unlock()
		if err := ctx.Err(); err != nil {
			return err
		}
	}
}

func (h *McpHub) readStore(ctx context.Context) (map[string]mcpserver.Server, error) {
	store, err := h.store()
	if err != nil {
		return nil, err
	}
	records, err := store.List(ctx)
	if err != nil {
		return nil, err
	}
	out := make(map[string]mcpserver.Server, len(records))
	for _, s := range records {
		h.mu.RLock()
		_, shadowed := h.registered[s.ID]
		h.mu.RUnlock()
		if shadowed {
			h.logger.Warn(`[mcp] stored server "` + s.ID + `" is shadowed by an app server and ignored`)
			continue
		}
		s = s.Clone()
		s.Source = mcpserver.SourceCustom
		out[s.ID] = s
	}
	return out, nil
}

func appendSorted(out []mcpserver.Server, m map[string]mcpserver.Server) []mcpserver.Server {
	start := len(out)
	for _, s := range m {
		out = append(out, s.Clone())
	}
	slices.SortFunc(out[start:], func(a, b mcpserver.Server) int { return cmp.Compare(a.ID, b.ID) })
	return out
}

var (
	idPattern         = regexp.MustCompile(`^[a-z][a-z0-9_-]*$`)
	envNamePattern    = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	headerNamePattern = regexp.MustCompile("^[!#$%&'*+.^_`|~0-9A-Za-z-]+$")
)

// ToServer returns a definition checked and normalised (toServer of core/ts), or a
// *mcpserver.Error ("invalid_argument").
func ToServer(def mcpserver.Definition, source mcpserver.Source) (mcpserver.Server, error) {
	fail := func(msg string) (mcpserver.Server, error) {
		return mcpserver.Server{}, mcpserver.NewError(msg, mcpserver.CodeInvalidArgument)
	}
	id := strings.TrimSpace(def.ID)
	if !idPattern.MatchString(id) || len(id) > mcpserver.MaxIDLength {
		return fail(`MCP server id "` + id + `" must be lower-case letters, digits, "-" and "_", starting with a letter (max ` + strconv.Itoa(mcpserver.MaxIDLength) + ")")
	}
	name := strings.TrimSpace(def.Name)
	if name == "" {
		name = id
	}
	if len([]rune(name)) > mcpserver.MaxNameLength {
		return fail("name is longer than " + strconv.Itoa(mcpserver.MaxNameLength) + " characters")
	}
	description := strings.TrimSpace(def.Description)
	if len([]rune(description)) > mcpserver.MaxDescriptionLength {
		return fail("description is longer than " + strconv.Itoa(mcpserver.MaxDescriptionLength) + " characters")
	}
	transport, msg := toTransport(def.Transport)
	if msg != "" {
		return fail(msg)
	}
	timeouts, msg := toTimeouts(def.Timeouts)
	if msg != "" {
		return fail(msg)
	}
	return mcpserver.Server{
		ID:          id,
		Name:        name,
		Description: description,
		Transport:   transport,
		Enabled:     def.Enabled == nil || *def.Enabled,
		Timeouts:    timeouts,
		Source:      source,
	}, nil
}

func toTransport(t mcpserver.Transport) (mcpserver.Transport, string) {
	switch t.Type {
	case mcpserver.TransportStdio:
		command := strings.TrimSpace(t.Command)
		if command == "" {
			return mcpserver.Transport{}, "a stdio MCP server needs a command"
		}
		if len(t.Args) > mcpserver.MaxArgs {
			return mcpserver.Transport{}, "at most " + strconv.Itoa(mcpserver.MaxArgs) + " args"
		}
		env, msg := toStringMap(t.Env, "env", envNamePattern, mcpserver.MaxEnv)
		if msg != "" {
			return mcpserver.Transport{}, msg
		}
		out := mcpserver.Transport{Type: mcpserver.TransportStdio, Command: command, Env: env, Cwd: strings.TrimSpace(t.Cwd)}
		if len(t.Args) > 0 {
			out.Args = slices.Clone(t.Args)
		}
		return out, ""
	case mcpserver.TransportHTTP:
		raw := strings.TrimSpace(t.URL)
		u, err := url.Parse(raw)
		if err != nil || u.Host == "" {
			return mcpserver.Transport{}, `MCP server url "` + raw + `" is not a valid URL`
		}
		if u.Scheme != "http" && u.Scheme != "https" {
			return mcpserver.Transport{}, "MCP server url must start with http:// or https://"
		}
		headers, msg := toStringMap(t.Headers, "headers", headerNamePattern, mcpserver.MaxHeaders)
		if msg != "" {
			return mcpserver.Transport{}, msg
		}
		oauth, msg := toOAuth(t.OAuth)
		if msg != "" {
			return mcpserver.Transport{}, msg
		}
		return mcpserver.Transport{Type: mcpserver.TransportHTTP, URL: raw, Headers: headers, OAuth: oauth}, ""
	default:
		return mcpserver.Transport{}, `unknown MCP transport type "` + string(t.Type) + `" (stdio or http)`
	}
}

func toStringMap(m map[string]string, what string, key *regexp.Regexp, maxN int) (map[string]string, string) {
	if len(m) == 0 {
		return nil, ""
	}
	if len(m) > maxN {
		return nil, "at most " + strconv.Itoa(maxN) + " " + what
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		if !key.MatchString(k) {
			return nil, what + ` name "` + k + `" is not valid`
		}
		out[k] = v
	}
	return out, ""
}

// toOAuth checks Transport.OAuth (A-214); nil when nothing is set.
func toOAuth(o *mcpserver.OAuthSettings) (*mcpserver.OAuthSettings, string) {
	if o == nil {
		return nil, ""
	}
	clientID := strings.TrimSpace(o.ClientID)
	if len([]rune(clientID)) > mcpserver.MaxOAuthClientID {
		return nil, "oauth.clientId is longer than " + strconv.Itoa(mcpserver.MaxOAuthClientID) + " characters"
	}
	scopes := strings.TrimSpace(o.Scopes)
	if len([]rune(scopes)) > mcpserver.MaxOAuthScopes {
		return nil, "oauth.scopes is longer than " + strconv.Itoa(mcpserver.MaxOAuthScopes) + " characters"
	}
	scopes = strings.Join(strings.Fields(scopes), " ")
	metadata := strings.TrimSpace(o.AuthServerMetadataURL)
	if len(metadata) > 2_000 {
		return nil, "oauth.authServerMetadataUrl is longer than 2000 characters"
	}
	if metadata != "" {
		u, err := url.Parse(metadata)
		if err != nil || u.Host == "" {
			return nil, `oauth.authServerMetadataUrl "` + metadata + `" is not a valid URL`
		}
		host := u.Hostname()
		loopback := host == "localhost" || host == "127.0.0.1" || host == "::1"
		if u.Scheme != "https" && (u.Scheme != "http" || !loopback) {
			return nil, "oauth.authServerMetadataUrl must use https"
		}
	}
	if clientID == "" && scopes == "" && metadata == "" {
		return nil, ""
	}
	return &mcpserver.OAuthSettings{ClientID: clientID, Scopes: scopes, AuthServerMetadataURL: metadata}, ""
}

func toTimeouts(t *mcpserver.Timeouts) (*mcpserver.Timeouts, string) {
	if t == nil {
		return nil, ""
	}
	out := mcpserver.Timeouts{}
	set := false
	for _, f := range []struct {
		name string
		in   *int
		out  **int
	}{{"connectMs", t.ConnectMs, &out.ConnectMs}, {"requestMs", t.RequestMs, &out.RequestMs}} {
		if f.in == nil {
			continue
		}
		if *f.in <= 0 {
			return nil, "timeouts." + f.name + " must be a positive number"
		}
		v := min(*f.in, mcpserver.MaxTimeoutMs)
		*f.out = &v
		set = true
	}
	if !set {
		return nil, ""
	}
	return &out, ""
}
