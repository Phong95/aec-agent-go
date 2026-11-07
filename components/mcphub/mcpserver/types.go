// Package mcpserver holds the contract of MCP servers (A-208): the Model Context Protocol
// servers agents connect to - a definition in a global library (mcphub.McpHub), enabled
// per agent (AgentProfile.Mcp.Servers), connected by each agent's MCP manager. It also
// holds what a connected server offers (tools, resources, prompts) and the results of
// calling them.
//
// It is the contract the hub shares with its store (persistence.McpServerStore) and with
// the event payloads, so it lives in its own leaf package, like subagenthub/subagenttype.
// The JSON shapes are the wire shapes of core/ts (McpServerDefinition, McpServer, McpTool,
// ...).
package mcpserver

import (
	"maps"
	"slices"
	"strings"
)

// TransportType is how the host reaches a server. The string values are stable wire
// values.
type TransportType string

// The transport types.
const (
	// TransportStdio: the host starts the server as a child process and talks to it over
	// stdin / stdout (the opt-in mcphub/stdio package).
	TransportStdio TransportType = "stdio"
	// TransportHTTP: a remote server reached over MCP streamable HTTP.
	TransportHTTP TransportType = "http"
)

// Transport is the transport of a server - the McpStdioTransport | McpHttpTransport union
// of core/ts, one struct with a Type tag. Command, Args, Env and Cwd are for stdio; URL and
// Headers for http.
type Transport struct {
	Type TransportType `json:"type"`
	// Command is the program to run (stdio), e.g. "node". Not run through a shell.
	Command string `json:"command,omitempty"`
	// Args are its arguments (stdio).
	Args []string `json:"args,omitzero"`
	// Env are extra environment variables (stdio). Values may use {{secret:NAME}} -
	// resolved through the agent's secrets when it connects (the secret must allow the
	// "mcp" tool). The child gets only a small safe set of the host's variables (PATH,
	// HOME, ...) plus these.
	Env map[string]string `json:"env,omitzero"`
	// Cwd is the working directory of the child process (stdio); "" = the host's.
	Cwd string `json:"cwd,omitempty"`
	// URL is the MCP endpoint, http:// or https:// (http).
	URL string `json:"url,omitempty"`
	// Headers are extra request headers (http), e.g. Authorization: Bearer
	// {{secret:NAME}} - resolved through the agent's secrets when it connects (the secret
	// must allow the "mcp" tool and the url's host).
	Headers map[string]string `json:"headers,omitzero"`
	// OAuth is how to sign in when the server needs OAuth (it answers 401) - A-214.
	// Optional: every value is discovered; set a ClientID only for a server without
	// dynamic registration. Ignored when Headers has its own Authorization (http).
	OAuth *OAuthSettings `json:"oauth,omitempty"`
}

// OAuthSettings is Transport.OAuth (the McpOAuthSettings of core/ts, A-214).
type OAuthSettings struct {
	// ClientID is a client id registered with the authorization server by hand (a public
	// client, PKCE). "" = dynamic registration.
	ClientID string `json:"clientId,omitempty"`
	// Scopes are the scopes to ask for, space separated. "" = what the server advertises;
	// offline_access is added when the server supports it.
	Scopes string `json:"scopes,omitempty"`
	// AuthServerMetadataURL is the authorization server's metadata URL (https), when
	// discovery from the MCP server doesn't find it.
	AuthServerMetadataURL string `json:"authServerMetadataUrl,omitempty"`
}

// HasOwnAuthorization reports whether the http transport sends its own Authorization
// header - then the OAuth sign-in is not used (A-214).
func (t Transport) HasOwnAuthorization() bool {
	for k := range t.Headers {
		if strings.EqualFold(k, "authorization") {
			return true
		}
	}
	return false
}

// Clone returns a deep copy.
func (t Transport) Clone() Transport {
	if t.Args != nil {
		t.Args = slices.Clone(t.Args)
	}
	if t.Env != nil {
		t.Env = maps.Clone(t.Env)
	}
	if t.Headers != nil {
		t.Headers = maps.Clone(t.Headers)
	}
	if t.OAuth != nil {
		o := *t.OAuth
		t.OAuth = &o
	}
	return t
}

// Timeouts are the time limits of a server, in milliseconds; nil = the default.
type Timeouts struct {
	// ConnectMs is the most time to start the transport and finish the initialize
	// handshake. Default 30 000 ms.
	ConnectMs *int `json:"connectMs,omitempty"`
	// RequestMs is the most time one request (tools/list, tools/call, ...) may take.
	// Default 60 000 ms.
	RequestMs *int `json:"requestMs,omitempty"`
}

// Clone returns a deep copy (nil stays nil).
func (t *Timeouts) Clone() *Timeouts {
	if t == nil {
		return nil
	}
	c := Timeouts{}
	if t.ConnectMs != nil {
		v := *t.ConnectMs
		c.ConnectMs = &v
	}
	if t.RequestMs != nil {
		v := *t.RequestMs
		c.RequestMs = &v
	}
	return &c
}

// Definition is what an MCP server is registered or created from.
type Definition struct {
	// ID is lower-case letters, digits, "-" and "_", starting with a letter (max 48) - how
	// agents enable it.
	ID string `json:"id"`
	// Name is the display name; "" = the id.
	Name string `json:"name,omitempty"`
	// Description is what the server is for.
	Description string    `json:"description,omitempty"`
	Transport   Transport `json:"transport"`
	// Enabled false means no agent connects to it (a connected one is closed); nil = true.
	Enabled  *bool     `json:"enabled,omitempty"`
	Timeouts *Timeouts `json:"timeouts,omitempty"`
}

// Source is where a server comes from. The string values are stable wire values.
type Source string

// The sources.
const (
	// SourceApp: a server an app registered (AgentCore.UseMcp), read-only at runtime.
	SourceApp Source = "app"
	// SourceCustom: a server created at runtime, kept in the persistence.McpServerStore.
	SourceCustom Source = "custom"
)

// Server is a server definition as the McpHub keeps it: checked, normalised, with its
// source.
type Server struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	Description string    `json:"description,omitempty"`
	Transport   Transport `json:"transport"`
	Enabled     bool      `json:"enabled"`
	Timeouts    *Timeouts `json:"timeouts,omitempty"`
	Source      Source    `json:"source"`
	// CreatedAt and UpdatedAt are set on custom servers only.
	CreatedAt string `json:"createdAt,omitempty"`
	UpdatedAt string `json:"updatedAt,omitempty"`
}

// Clone returns a deep copy. Add a line here when a pointer, slice or map field is added.
func (s Server) Clone() Server {
	s.Transport = s.Transport.Clone()
	s.Timeouts = s.Timeouts.Clone()
	return s
}

// ConnectTimeoutMs is the server's connect limit, or the default.
func (s Server) ConnectTimeoutMs() int {
	if s.Timeouts != nil && s.Timeouts.ConnectMs != nil {
		return *s.Timeouts.ConnectMs
	}
	return DefaultConnectMs
}

// RequestTimeoutMs is the server's request limit, or the default.
func (s Server) RequestTimeoutMs() int {
	if s.Timeouts != nil && s.Timeouts.RequestMs != nil {
		return *s.Timeouts.RequestMs
	}
	return DefaultRequestMs
}

// The limits of a definition (MCP_LIMITS of core/ts).
const (
	MaxIDLength          = 48
	MaxNameLength        = 100
	MaxDescriptionLength = 1_000
	MaxArgs              = 100
	MaxEnv               = 100
	MaxHeaders           = 100
	// MaxTimeoutMs is the longest timeout a definition may set (10 minutes).
	MaxTimeoutMs = 600_000
	// MaxOAuthClientID and MaxOAuthScopes bound oauth.clientId and oauth.scopes (A-214).
	MaxOAuthClientID = 200
	MaxOAuthScopes   = 1_000
)

// The default timeouts (MCP_DEFAULT_TIMEOUTS of core/ts).
const (
	DefaultConnectMs = 30_000
	DefaultRequestMs = 60_000
)

// ---------------------------------------------------------------------------
// What a connected server offers (capability discovery)
// ---------------------------------------------------------------------------

// ToolAnnotations are MCP tool annotations - hints only (A-142 maps them to a tool's risk).
type ToolAnnotations struct {
	Title           string `json:"title,omitempty"`
	ReadOnlyHint    *bool  `json:"readOnlyHint,omitempty"`
	DestructiveHint *bool  `json:"destructiveHint,omitempty"`
	IdempotentHint  *bool  `json:"idempotentHint,omitempty"`
	OpenWorldHint   *bool  `json:"openWorldHint,omitempty"`
}

// Tool is one tool of a server.
type Tool struct {
	Name        string `json:"name"`
	Title       string `json:"title,omitempty"`
	Description string `json:"description,omitempty"`
	// InputSchema is the JSON Schema of the arguments (an object schema).
	InputSchema  map[string]any   `json:"inputSchema"`
	OutputSchema map[string]any   `json:"outputSchema,omitempty"`
	Annotations  *ToolAnnotations `json:"annotations,omitempty"`
	// Meta (A-213) is extra data the server attaches. Read: "anthropic/maxResultSizeChars"
	// (a larger result limit, up to 500 000) and "anthropic/requiresUserInteraction" (every
	// call asks).
	Meta map[string]any `json:"_meta,omitempty"`
}

// Resource is one resource of a server.
type Resource struct {
	URI         string `json:"uri"`
	Name        string `json:"name"`
	Title       string `json:"title,omitempty"`
	Description string `json:"description,omitempty"`
	MimeType    string `json:"mimeType,omitempty"`
	Size        *int64 `json:"size,omitempty"`
}

// PromptArgument is one argument of a prompt.
type PromptArgument struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Required    bool   `json:"required,omitempty"`
}

// Prompt is one prompt of a server.
type Prompt struct {
	Name        string           `json:"name"`
	Title       string           `json:"title,omitempty"`
	Description string           `json:"description,omitempty"`
	Arguments   []PromptArgument `json:"arguments,omitempty"`
}

// ListChanged is a capability that may announce list changes.
type ListChanged struct {
	ListChanged bool `json:"listChanged,omitempty"`
}

// ResourcesCapability is the resources capability of a server.
type ResourcesCapability struct {
	Subscribe   bool `json:"subscribe,omitempty"`
	ListChanged bool `json:"listChanged,omitempty"`
}

// Capabilities is what the server said it supports in its initialize result. A nil
// field = the server does not offer it.
type Capabilities struct {
	Tools     *ListChanged         `json:"tools,omitempty"`
	Resources *ResourcesCapability `json:"resources,omitempty"`
	Prompts   *ListChanged         `json:"prompts,omitempty"`
	Logging   map[string]any       `json:"logging,omitempty"`
}

// Implementation names a client or server (clientInfo / serverInfo).
type Implementation struct {
	Name    string `json:"name"`
	Version string `json:"version"`
	Title   string `json:"title,omitempty"`
}

// Content is one block of a tool result or prompt message (text, image, audio,
// resource...), kept as the server sent it.
type Content = map[string]any

// CallToolResult is a tools/call result.
type CallToolResult struct {
	Content           []Content      `json:"content"`
	StructuredContent map[string]any `json:"structuredContent,omitempty"`
	IsError           bool           `json:"isError,omitempty"`
}

// ResourceContents is one entry of a resources/read result (text or a base64 blob).
type ResourceContents struct {
	URI      string `json:"uri"`
	MimeType string `json:"mimeType,omitempty"`
	Text     string `json:"text,omitempty"`
	Blob     string `json:"blob,omitempty"`
}

// ReadResourceResult is a resources/read result.
type ReadResourceResult struct {
	Contents []ResourceContents `json:"contents"`
}

// PromptMessage is one message of a prompts/get result.
type PromptMessage struct {
	Role    string  `json:"role"`
	Content Content `json:"content"`
}

// GetPromptResult is a prompts/get result.
type GetPromptResult struct {
	Description string          `json:"description,omitempty"`
	Messages    []PromptMessage `json:"messages"`
}

// Config is AgentProfile.Mcp - the McpHub servers an agent connects to (the McpConfig of
// core/ts).
type Config struct {
	// Servers are server ids from the McpHub.
	Servers []string `json:"servers"`
	// Permissions (A-213) say which MCP tools run without asking (Allow), always ask (Ask)
	// or are not offered (Deny). Rules: mcp__<server>, mcp__<server>__*,
	// mcp__<server>__<prefix>*, mcp__<server>__<tool> (see MatchesRule). Deny wins over
	// Ask, Ask over Allow. A tool no rule covers asks. Nil means no rules.
	Permissions *Permissions `json:"permissions,omitempty"`
	// ToolSearch (A-213) is tool search: ToolSearchAuto ("" counts as auto) defers the
	// tools - the model sees their names and loads them with tool_search - once their
	// definitions take more than ToolSearchThreshold percent of the context window;
	// ToolSearchOn always defers them, ToolSearchOff always sends them upfront.
	ToolSearch string `json:"toolSearch,omitempty"`
	// ToolSearchThreshold (A-213) is the auto threshold in percent of the context window
	// (0-100). Nil means DefaultToolSearchPercent.
	ToolSearchThreshold *float64 `json:"toolSearchThreshold,omitempty"`
}

// The values of Config.ToolSearch (A-213).
const (
	ToolSearchAuto = "auto"
	ToolSearchOn   = "on"
	ToolSearchOff  = "off"
)

// DefaultToolSearchPercent (A-213) is the share of the context window MCP tool
// definitions may take before they are deferred.
const DefaultToolSearchPercent = 10

// Permissions is Config.Permissions (A-213, McpPermissions of core/ts).
type Permissions struct {
	Allow []string `json:"allow,omitempty"`
	Ask   []string `json:"ask,omitempty"`
	Deny  []string `json:"deny,omitempty"`
}

// Clone returns a deep copy (nil stays nil).
func (c *Config) Clone() *Config {
	if c == nil {
		return nil
	}
	out := Config{Servers: slices.Clone(c.Servers), ToolSearch: c.ToolSearch}
	if out.Servers == nil {
		out.Servers = []string{}
	}
	if c.Permissions != nil {
		out.Permissions = &Permissions{Allow: slices.Clone(c.Permissions.Allow), Ask: slices.Clone(c.Permissions.Ask), Deny: slices.Clone(c.Permissions.Deny)}
	}
	if c.ToolSearchThreshold != nil {
		v := *c.ToolSearchThreshold
		out.ToolSearchThreshold = &v
	}
	return &out
}
