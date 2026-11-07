package agent

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Phong95/aec-agent-go/components/events"
	"github.com/Phong95/aec-agent-go/components/mcphub/mcpserver"
	"github.com/Phong95/aec-agent-go/components/toolhub"
	"github.com/Phong95/aec-agent-go/internal/jsonx"
	"github.com/Phong95/aec-agent-go/internal/jsstr"
	"github.com/Phong95/aec-agent-go/kernel"
)

// McpToolSearch is the tool that loads deferred MCP tools (A-213).
const McpToolSearch = "tool_search"

const (
	// mcpInstructionsMax cuts server instructions longer than this in the system prompt.
	mcpInstructionsMax = 4_096
	// mcpMaxSessions is how many sessions' loaded tools are remembered (least recently
	// used are forgotten).
	mcpMaxSessions = 200
	// mcpReconnectWait is how long a safe-to-repeat call waits for its server to
	// reconnect before trying again.
	mcpReconnectWait = 15 * time.Second
	// mcpReconnectPoll is how often that wait looks at the server's status.
	mcpReconnectPoll = 200 * time.Millisecond
)

// McpToolsDeps is what McpTools needs.
type McpToolsDeps struct {
	AgentID string
	Bus     events.Bus
	Mcp     mcpserver.Connections
	// Config is read on every use, so UpdateMcp applies (permissions, tool search).
	Config func() *mcpserver.Config
	// ContextWindowTokens is the model's context window (tool search's auto threshold).
	ContextWindowTokens func() int
}

type mcpToolEntry struct {
	name       string
	serverID   string
	serverName string
	tool       mcpserver.Tool
	parameters toolhub.JSONSchema
	// short and long are the description cut for upfront and for tool_search.
	short    string
	long     string
	readOnly bool
	tokens   int
	handler  *mcpToolHandler
}

// McpTools is the agent's MCP tools (A-213, McpTools of core/ts): every tool of every
// connected MCP server becomes an agent tool named mcp__<server>__<tool>. It is built
// from the MCP manager's mcp:* events (pub-sub: the MCP manager doesn't know about tools)
// and handed to the ToolManager as its DynamicToolSource.
//   - Tool search: when the definitions would take more than a share of the context
//     window (Config.ToolSearch "auto", default 10%) or always ("on"), the model gets only
//     their names (in <mcp_servers>) and the tool_search tool, which loads the ones it
//     needs into the session. "off" sends every tool upfront.
//   - Approval: every MCP tool asks by default. Config.Permissions allow / ask / deny rules
//     change that; a denied tool is not offered at all, and a tool marked
//     _meta["anthropic/requiresUserInteraction"] asks on every call even in YOLO mode.
//   - readOnlyHint tools are read-only (plan mode offers them); a readOnlyHint or
//     idempotentHint call that was cut off by a lost connection is repeated once after the
//     server reconnects.
//   - A tool with a schema the APIs refuse, or a name that collides, is left out and the
//     model is told why.
//
// It is safe for concurrent use.
type McpTools struct {
	d      McpToolsDeps
	search *mcpSearchHandler

	mu       sync.RWMutex
	order    []string
	entries  map[string]*mcpToolEntry
	excluded map[string][]string
	// loaded is the tools tool_search loaded per session; sessions is its use order
	// (least recently used first).
	loaded   map[string]map[string]bool
	sessions []string
	unsub    []events.Unsubscribe
}

var _ DynamicToolSource = (*McpTools)(nil)

// NewMcpTools builds the agent's MCP tools from the servers' current state and subscribes
// to their events. Call Dispose to unsubscribe.
func NewMcpTools(d McpToolsDeps) *McpTools {
	t := &McpTools{d: d, entries: map[string]*mcpToolEntry{}, excluded: map[string][]string{}, loaded: map[string]map[string]bool{}}
	t.search = &mcpSearchHandler{tools: t}
	refresh := func(agentID, serverID string) {
		if agentID == d.AgentID {
			t.refresh(serverID)
		}
	}
	if d.Bus != nil {
		t.unsub = []events.Unsubscribe{
			events.Subscribe(d.Bus, events.McpConnected, func(_ kernel.Event, p events.McpConnectedPayload) { refresh(p.AgentID, p.ServerID) }),
			events.Subscribe(d.Bus, events.McpToolsChanged, func(_ kernel.Event, p events.McpToolsChangedPayload) { refresh(p.AgentID, p.ServerID) }),
			events.Subscribe(d.Bus, events.McpDisconnected, func(_ kernel.Event, p events.McpDisconnectedPayload) { refresh(p.AgentID, p.ServerID) }),
			events.Subscribe(d.Bus, events.McpFailed, func(_ kernel.Event, p events.McpFailedPayload) { refresh(p.AgentID, p.ServerID) }),
		}
	}
	if d.Mcp != nil {
		for _, s := range d.Mcp.List() {
			t.refresh(s.ServerID)
		}
	}
	return t
}

// Owns reports whether the name is an MCP tool (or tool_search) of the agent.
func (t *McpTools) Owns(name string) bool {
	if name == McpToolSearch {
		return true
	}
	t.mu.RLock()
	defer t.mu.RUnlock()
	_, ok := t.entries[name]
	return ok
}

// Get returns an MCP tool by name - loaded or not, so a call the model makes from an
// earlier search still runs. A denied tool: none.
func (t *McpTools) Get(name string) (toolhub.Handler, bool) {
	if name == McpToolSearch {
		if t.SearchActive() {
			return t.search, true
		}
		return nil, false
	}
	t.mu.RLock()
	e, ok := t.entries[name]
	t.mu.RUnlock()
	if !ok || t.approvalOf(e) == mcpserver.ApprovalDenied {
		return nil, false
	}
	return e.handler, true
}

// List returns the tools to offer the model in the session: all of them, or (tool
// search) the loaded ones plus tool_search.
func (t *McpTools) List(sessionID string) []toolhub.Handler {
	allowed := t.allowed()
	if !t.searchActive(allowed) {
		out := make([]toolhub.Handler, 0, len(allowed))
		for _, e := range allowed {
			out = append(out, e.handler)
		}
		return out
	}
	t.mu.RLock()
	loaded := t.loaded[sessionID]
	var out []toolhub.Handler
	for _, e := range allowed {
		if sessionID != "" && loaded[e.name] {
			out = append(out, e.handler)
		}
	}
	t.mu.RUnlock()
	return append(out, t.search)
}

// Describe returns the agent's MCP tools as hosts show them: which server, approval, and
// whether tool search defers them.
func (t *McpTools) Describe() []mcpserver.AgentTool {
	deferred := t.SearchActive()
	out := []mcpserver.AgentTool{}
	for _, e := range t.all() {
		out = append(out, mcpserver.AgentTool{
			Name: e.name, ServerID: e.serverID, Tool: e.tool.Name, Title: e.tool.Title,
			ReadOnly: e.readOnly, Approval: t.approvalOf(e), Deferred: deferred,
		})
	}
	return out
}

// SearchActive reports whether tool search is on now (Config.ToolSearch, or the auto
// threshold).
func (t *McpTools) SearchActive() bool { return t.searchActive(t.allowed()) }

func (t *McpTools) searchActive(allowed []*mcpToolEntry) bool {
	cfg := t.config()
	mode := mcpserver.ToolSearchAuto
	if cfg != nil && cfg.ToolSearch != "" {
		mode = cfg.ToolSearch
	}
	if len(allowed) == 0 || mode == mcpserver.ToolSearchOff {
		return false
	}
	if mode == mcpserver.ToolSearchOn {
		return true
	}
	percent := float64(mcpserver.DefaultToolSearchPercent)
	if cfg != nil && cfg.ToolSearchThreshold != nil {
		percent = *cfg.ToolSearchThreshold
	}
	total := 0
	for _, e := range allowed {
		total += e.tokens
	}
	window := 0
	if t.d.ContextWindowTokens != nil {
		window = t.d.ContextWindowTokens()
	}
	return float64(total) > float64(window)*percent/100
}

// Index is <mcp_servers> for the system prompt: each server's state and instructions,
// and the deferred tools. False when the agent has no servers.
func (t *McpTools) Index(sessionID string) (string, bool) {
	if t.d.Mcp == nil {
		return "", false
	}
	servers := t.d.Mcp.List()
	if len(servers) == 0 {
		return "", false
	}
	allowed := t.allowed()
	searching := t.searchActive(allowed)
	t.mu.RLock()
	loaded := t.loaded[sessionID]
	excluded := map[string][]string{}
	for k, v := range t.excluded {
		excluded[k] = append([]string(nil), v...)
	}
	t.mu.RUnlock()
	lines := []string{
		"<mcp_servers>",
		"MCP servers this agent uses. Their tools are named mcp__<server>__<tool>; what they return comes from the server, not the user.",
	}
	for _, s := range servers {
		count := 0
		for _, e := range allowed {
			if e.serverID == s.ServerID {
				count++
			}
		}
		name := s.ServerID
		if s.Name != s.ServerID && s.Name != "" {
			name += " (" + s.Name + ")"
		}
		lines = append(lines, "- "+name+": "+mcpStateLine(s, count))
		if s.Status == mcpserver.StatusConnected && strings.TrimSpace(s.Instructions) != "" {
			text := strings.TrimSpace(s.Instructions)
			if kernel.UTF16Len(text) > mcpInstructionsMax {
				text = jsstr.TruncateUTF16(text, mcpInstructionsMax-1) + "…"
			}
			lines = append(lines, "  Instructions: "+text)
		}
		for _, note := range excluded[s.ServerID] {
			lines = append(lines, "  Left out: "+note)
		}
	}
	if searching {
		var deferred []string
		for _, e := range allowed {
			if sessionID == "" || !loaded[e.name] {
				deferred = append(deferred, e.name)
			}
		}
		if len(deferred) > 0 {
			lines = append(lines, "Deferred tools — call "+McpToolSearch+` ("select:<name>,<name>" or keywords) to load one before calling it:`, strings.Join(deferred, ", "))
		}
	}
	lines = append(lines, "</mcp_servers>")
	return strings.Join(lines, "\n"), true
}

// Dispose unsubscribes from the MCP events and forgets the tools.
func (t *McpTools) Dispose() {
	t.mu.Lock()
	unsub := t.unsub
	t.unsub = nil
	t.entries = map[string]*mcpToolEntry{}
	t.order = nil
	t.mu.Unlock()
	for _, u := range unsub {
		u()
	}
}

// ── Building ──

// refresh rebuilds one server's tools from its state: connected = its tools; anything
// else = none.
func (t *McpTools) refresh(serverID string) {
	var state mcpserver.State
	ok := false
	if t.d.Mcp != nil {
		state, ok = t.d.Mcp.Get(serverID)
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	kept := t.order[:0:0]
	for _, name := range t.order {
		if t.entries[name].serverID == serverID {
			delete(t.entries, name)
			continue
		}
		kept = append(kept, name)
	}
	t.order = kept
	delete(t.excluded, serverID)
	if !ok || state.Status != mcpserver.StatusConnected {
		return
	}
	var notes []string
	for _, tool := range state.Tools {
		if tool.Name == "" {
			continue
		}
		name := McpToolName(serverID, tool.Name)
		if _, taken := t.entries[name]; taken {
			notes = append(notes, tool.Name+" (its name "+name+" is taken by another tool)")
			continue
		}
		schema := NormalizeMcpInputSchema(tool.InputSchema)
		if !schema.OK {
			notes = append(notes, tool.Name+" ("+schema.Reason+")")
			continue
		}
		readOnly := tool.Annotations != nil && tool.Annotations.ReadOnlyHint != nil && *tool.Annotations.ReadOnlyHint
		e := &mcpToolEntry{
			name: name, serverID: serverID, serverName: state.Name, tool: tool, parameters: schema.Schema,
			short:    McpToolDescription(tool, state.Name, schema.Note, McpDescriptionMax),
			long:     McpToolDescription(tool, state.Name, schema.Note, McpSearchDescriptionMax),
			readOnly: readOnly,
		}
		e.handler = &mcpToolHandler{tools: t, e: e}
		if raw, err := jsonx.Marshal(map[string]any{"name": name, "description": e.short, "parameters": e.parameters}); err == nil {
			e.tokens = int(math.Ceil(float64(kernel.UTF16Len(string(raw))) / 4))
		}
		t.entries[name] = e
		t.order = append(t.order, name)
	}
	if len(notes) > 0 {
		t.excluded[serverID] = notes
	}
}

func (t *McpTools) config() *mcpserver.Config {
	if t.d.Config == nil {
		return nil
	}
	return t.d.Config()
}

// approvalOf is a tool's approval: denied, always (the server insists), ask, or never
// (allowed). Default ask.
func (t *McpTools) approvalOf(e *mcpToolEntry) string {
	var rules mcpserver.Permissions
	if cfg := t.config(); cfg != nil && cfg.Permissions != nil {
		rules = *cfg.Permissions
	}
	matches := func(list []string) bool {
		for _, r := range list {
			if mcpserver.MatchesRule(r, e.name, e.serverID) {
				return true
			}
		}
		return false
	}
	switch {
	case matches(rules.Deny):
		return mcpserver.ApprovalDenied
	case McpRequiresUser(e.tool):
		return mcpserver.ApprovalAlways
	case matches(rules.Ask):
		return mcpserver.ApprovalAsk
	case matches(rules.Allow):
		return mcpserver.ApprovalNever
	default:
		return mcpserver.ApprovalAsk
	}
}

func (t *McpTools) all() []*mcpToolEntry {
	t.mu.RLock()
	defer t.mu.RUnlock()
	out := make([]*mcpToolEntry, 0, len(t.order))
	for _, name := range t.order {
		out = append(out, t.entries[name])
	}
	return out
}

func (t *McpTools) allowed() []*mcpToolEntry {
	var out []*mcpToolEntry
	for _, e := range t.all() {
		if t.approvalOf(e) != mcpserver.ApprovalDenied {
			out = append(out, e)
		}
	}
	return out
}

func (t *McpTools) markLoaded(sessionID string, names []string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	set := t.loaded[sessionID]
	if set == nil {
		set = map[string]bool{}
		t.loaded[sessionID] = set
	}
	for _, n := range names {
		set[n] = true
	}
	for i, s := range t.sessions {
		if s == sessionID {
			t.sessions = append(t.sessions[:i], t.sessions[i+1:]...)
			break
		}
	}
	t.sessions = append(t.sessions, sessionID)
	for len(t.sessions) > mcpMaxSessions {
		delete(t.loaded, t.sessions[0])
		t.sessions = t.sessions[1:]
	}
}

// call runs the tool on its server.
func (t *McpTools) call(ctx context.Context, e *mcpToolEntry, args map[string]any, repeatable bool) toolhub.Output {
	result, err := t.d.Mcp.CallTool(ctx, e.serverID, e.tool.Name, args, mcpserver.CallOptions{})
	if err == nil {
		return McpToolOutput(result)
	}
	// Cut off by a lost connection: a call that is safe to repeat goes again once, on the new one.
	var me *mcpserver.Error
	if repeatable && errors.As(err, &me) && (me.Code == mcpserver.CodeClosed || me.Code == mcpserver.CodeNotConnected) && ctx.Err() == nil {
		if t.waitConnected(ctx, e.serverID) {
			again, err2 := t.d.Mcp.CallTool(ctx, e.serverID, e.tool.Name, args, mcpserver.CallOptions{})
			if err2 == nil {
				return McpToolOutput(again)
			}
			return mcpFailure(e, err2)
		}
	}
	return mcpFailure(e, err)
}

func (t *McpTools) waitConnected(ctx context.Context, serverID string) bool {
	deadline := time.Now().Add(mcpReconnectWait)
	timer := time.NewTimer(0)
	defer timer.Stop()
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return false
		case <-timer.C:
		}
		state, ok := t.d.Mcp.Get(serverID)
		if !ok {
			return false
		}
		switch state.Status {
		case mcpserver.StatusConnected:
			return true
		case mcpserver.StatusReconnecting, mcpserver.StatusConnecting:
		default:
			return false
		}
		timer.Reset(mcpReconnectPoll)
	}
	return false
}

func mcpFailure(e *mcpToolEntry, err error) toolhub.Output {
	return toolhub.Error(`The MCP server "` + e.serverName + `" could not run ` + e.tool.Name + ": " + err.Error()).NotRetryable()
}

func mcpStateLine(s mcpserver.State, tools int) string {
	switch s.Status {
	case mcpserver.StatusConnected:
		plural := "s"
		if tools == 1 {
			plural = ""
		}
		return fmt.Sprintf("connected, %d tool%s", tools, plural)
	case mcpserver.StatusNeedsAuth:
		return "not connected — it needs a sign-in (the user signs in under Settings → MCP servers)"
	case mcpserver.StatusDisabled:
		return "disabled"
	case mcpserver.StatusConnecting, mcpserver.StatusReconnecting:
		last := ""
		if s.Error != "" {
			last = " (last error: " + s.Error + ")"
		}
		return string(s.Status) + last + " — its tools show up once it is connected"
	default:
		if s.Error != "" {
			return "failed: " + s.Error
		}
		return "failed"
	}
}

// ── The handlers ──

// mcpToolHandler is one MCP tool as an agent tool.
type mcpToolHandler struct {
	tools *McpTools
	e     *mcpToolEntry
}

var (
	_ toolhub.Handler      = (*mcpToolHandler)(nil)
	_ toolhub.CallApprover = (*mcpToolHandler)(nil)
)

func (h *mcpToolHandler) Name() string    { return "mcp-tool:" + h.e.name }
func (h *mcpToolHandler) Version() string { return "1.0.0" }

func (h *mcpToolHandler) Definition() toolhub.Definition {
	desc := h.e.short
	if h.tools.SearchActive() {
		desc = h.e.long
	}
	risk := toolhub.RiskConsequential
	if h.e.readOnly {
		risk = toolhub.RiskReadOnly
	}
	return toolhub.Definition{
		Name: h.e.name, Description: desc, Parameters: h.e.parameters.Clone(), Risk: risk,
		// MCP tools stay with the agent that connected to the server.
		Subagent:       toolhub.SubagentNever,
		MaxResultChars: McpResultLimit(h.e.tool),
	}
}

// ApprovalFor is the tool's approval by the agent's permission rules (a denied tool, which
// is never offered, counts as always).
func (h *mcpToolHandler) ApprovalFor(kernel.Arguments) toolhub.ToolApproval {
	switch h.tools.approvalOf(h.e) {
	case mcpserver.ApprovalNever:
		return toolhub.ToolApprovalNever
	case mcpserver.ApprovalAsk:
		return toolhub.ToolApprovalAsk
	default:
		return toolhub.ToolApprovalAlways
	}
}

func (h *mcpToolHandler) Execute(ctx context.Context, args kernel.Arguments, _ toolhub.Context) (toolhub.Output, error) {
	var parsed map[string]any
	if len(args) > 0 {
		if err := args.Decode(&parsed); err != nil {
			// A tool-level error the model can fix, not a failure of the call.
			return toolhub.Error("the arguments must be one JSON object: " + err.Error()).NotRetryable(), nil //nolint:nilerr // reported to the model as the tool's result
		}
	}
	if parsed == nil {
		parsed = map[string]any{}
	}
	ann := h.e.tool.Annotations
	repeatable := h.e.readOnly || (ann != nil && ann.IdempotentHint != nil && *ann.IdempotentHint)
	return h.tools.call(ctx, h.e, parsed, repeatable), nil
}

// mcpSearchHandler is tool_search.
type mcpSearchHandler struct{ tools *McpTools }

var _ toolhub.Handler = (*mcpSearchHandler)(nil)

func (h *mcpSearchHandler) Name() string    { return "mcp-tool:" + McpToolSearch }
func (h *mcpSearchHandler) Version() string { return "1.0.0" }

func (h *mcpSearchHandler) Definition() toolhub.Definition {
	return toolhub.Definition{
		Name: McpToolSearch,
		Description: "Load MCP tools before you call them. Only their names are listed in <mcp_servers> in the system prompt; " +
			`this returns their full definitions and makes them callable from your next step. query: "select:<name>,<name>" ` +
			"loads those tools by name; other text searches the tools' names and descriptions by keyword.",
		Parameters: func() toolhub.JSONSchema {
			s := toolhub.Object(
				toolhub.Prop("query", toolhub.String(`e.g. "select:mcp__github__create_issue" or "create issue"`)),
				toolhub.Prop("max_results", toolhub.Number("Most tools to load for a keyword search (1-20, default 5).")),
			)
			s.Required = []string{"query"}
			return s
		}(),
		Risk:     toolhub.RiskReadOnly,
		Subagent: toolhub.SubagentNever,
	}
}

func (h *mcpSearchHandler) Execute(_ context.Context, args kernel.Arguments, tc toolhub.Context) (toolhub.Output, error) {
	var in struct {
		Query      any `json:"query"`
		MaxResults any `json:"max_results"`
	}
	_ = args.Decode(&in)
	query := ""
	if s, ok := in.Query.(string); ok {
		query = s
	} else if in.Query != nil {
		query = fmt.Sprint(in.Query)
	}
	limit := 5
	switch v := in.MaxResults.(type) {
	case float64:
		if v != 0 && !math.IsNaN(v) {
			limit = int(v)
		}
	case string:
		var n float64
		if _, err := fmt.Sscan(v, &n); err == nil && n != 0 {
			limit = int(n)
		}
	default:
	}
	return h.tools.searchTools(query, limit, tc.SessionID), nil
}

func (t *McpTools) searchTools(query string, limit int, sessionID string) toolhub.Output {
	allowed := t.allowed()
	q := strings.TrimSpace(query)
	if q == "" {
		return toolhub.Error(`Give a query: "select:<name>,<name>" or keywords.`).NotRetryable()
	}
	var found []*mcpToolEntry
	var missing []string
	if strings.HasPrefix(strings.ToLower(q), "select:") {
		for _, n := range strings.Split(q[len("select:"):], ",") {
			n = strings.TrimSpace(n)
			if n == "" {
				continue
			}
			var hit *mcpToolEntry
			for _, e := range allowed {
				if e.name == n {
					hit = e
					break
				}
			}
			if hit == nil {
				for _, e := range allowed {
					if e.tool.Name == n {
						hit = e
						break
					}
				}
			}
			switch {
			case hit == nil:
				missing = append(missing, n)
			case !containsEntry(found, hit):
				found = append(found, hit)
			default:
			}
		}
	} else {
		words := strings.FieldsFunc(strings.ToLower(q), func(r rune) bool { return r == ',' || jsstr.IsSpace(r) })
		limit = min(max(limit, 1), 20)
		type scored struct {
			e     *mcpToolEntry
			score int
		}
		var hits []scored
		for _, e := range allowed {
			name := strings.ToLower(e.name)
			text := strings.ToLower(e.tool.Title + " " + e.tool.Description)
			score := 0
			for _, w := range words {
				if strings.Contains(name, w) {
					score += 3
				}
				if strings.Contains(e.serverID, w) {
					score += 2
				}
				if strings.Contains(text, w) {
					score++
				}
			}
			if score > 0 {
				hits = append(hits, scored{e, score})
			}
		}
		sort.SliceStable(hits, func(i, j int) bool {
			if hits[i].score != hits[j].score {
				return hits[i].score > hits[j].score
			}
			return hits[i].e.name < hits[j].e.name
		})
		for i := 0; i < len(hits) && i < limit; i++ {
			found = append(found, hits[i].e)
		}
	}
	if len(found) == 0 {
		hint := ""
		if len(missing) == 0 {
			hint = " Try other words, or select: with a name from <mcp_servers>."
		}
		return toolhub.Error(`No MCP tool matches "` + q + `".` + hint).NotRetryable()
	}
	names := make([]string, 0, len(found))
	type def struct {
		Name        string             `json:"name"`
		Description string             `json:"description"`
		Parameters  toolhub.JSONSchema `json:"parameters"`
	}
	defs := make([]def, 0, len(found))
	for _, e := range found {
		names = append(names, e.name)
		defs = append(defs, def{Name: e.name, Description: e.long, Parameters: e.parameters})
	}
	t.markLoaded(sessionID, names)
	raw, err := jsonx.Marshal(defs)
	if err != nil {
		return toolhub.Error("could not encode the tool definitions: " + err.Error()).NotRetryable()
	}
	plural, pronoun := "s", "them"
	if len(found) == 1 {
		plural, pronoun = "", "it"
	}
	text := fmt.Sprintf("Loaded %d tool%s; you can call %s from your next step.", len(found), plural, pronoun)
	if len(missing) > 0 {
		text += " Not found: " + strings.Join(missing, ", ") + "."
	}
	return toolhub.Text(text + "\n\n" + indentJSON(raw))
}

func containsEntry(list []*mcpToolEntry, e *mcpToolEntry) bool {
	for _, x := range list {
		if x == e {
			return true
		}
	}
	return false
}
