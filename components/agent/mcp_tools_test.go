package agent_test

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/Phong95/aec-agent-go/aecmock"
	"github.com/Phong95/aec-agent-go/components/agent"
	"github.com/Phong95/aec-agent-go/components/events"
	"github.com/Phong95/aec-agent-go/components/mcphub/mcpserver"
	"github.com/Phong95/aec-agent-go/components/toolhub"
	"github.com/Phong95/aec-agent-go/kernel"
)

// ---------------------------------------------------------------------------
// A-213: MCP tools as agent tools - naming, schemas, results, permission rules, tool
// search, and an agent calling a tool of the fake MCP server through its loop. The Go
// form of core/ts tests/unit/mcp-tools.test.ts.
// ---------------------------------------------------------------------------

func TestMcpToolName(t *testing.T) {
	if got := agent.McpToolName("github", "create_issue"); got != "mcp__github__create_issue" {
		t.Fatalf("name = %q", got)
	}
	if got := agent.McpToolName("github", "repo.list files"); got != "mcp__github__repo_list_files" {
		t.Fatalf("name = %q", got)
	}
	a := agent.McpToolName("server", strings.Repeat("x", 80)+"a")
	b := agent.McpToolName("server", strings.Repeat("x", 80)+"b")
	if len(a) > agent.McpToolNameMax || a == b {
		t.Fatalf("long names %q %q", a, b)
	}
	if n := agent.McpToolName(strings.Repeat("s", 48), "tool"); len(n) > agent.McpToolNameMax {
		t.Fatalf("long server name %q", n)
	}
	if s, ok := agent.McpServerOf("mcp__github__x"); !ok || s != "github" {
		t.Fatalf("server of = %q %v", s, ok)
	}
	if _, ok := agent.McpServerOf("read_file"); ok {
		t.Fatal("read_file has no server")
	}
}

func decodeMap(t *testing.T, s string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestNormalizeMcpInputSchema(t *testing.T) {
	refd := agent.NormalizeMcpInputSchema(decodeMap(t, `{
		"$schema": "https://json-schema.org/draft/2020-12/schema", "type": "object",
		"$defs": {"Repo": {"type": "object", "properties": {"owner": {"type": "string"}}}},
		"properties": {"repo": {"$ref": "#/$defs/Repo"}, "self": {"$ref": "#/$defs/Missing"}},
		"required": ["repo", "ghost"]}`))
	if !refd.OK {
		t.Fatalf("not ok: %s", refd.Reason)
	}
	raw, _ := json.Marshal(refd.Schema)
	if want := `{"properties":{"repo":{"properties":{"owner":{"type":"string"}},"type":"object"},"self":{}},"required":["repo"],"type":"object"}`; string(raw) != want {
		t.Fatalf("schema = %s", raw)
	}
	if !slices.Equal(refd.Schema.Required, []string{"repo"}) || refd.Schema.Type != "object" {
		t.Fatalf("policy view = %+v", refd.Schema)
	}
	if p, _ := refd.Schema.Properties.Get("repo"); p.Type != "object" {
		t.Fatalf("repo type = %q", p.Type)
	}

	union := agent.NormalizeMcpInputSchema(decodeMap(t, `{"anyOf": [{"properties": {"id": {"type": "string"}}, "required": ["id"]}, {"properties": {"url": {"type": "string"}}, "required": ["url"]}]}`))
	if !union.OK || union.Note != "Arguments: give (id) or (url)." || union.Schema.Properties.Len() != 2 || union.Schema.Required != nil {
		t.Fatalf("union = %+v", union)
	}
	all := agent.NormalizeMcpInputSchema(decodeMap(t, `{"allOf": [{"properties": {"a": {}}, "required": ["a"]}, {"properties": {"b": {}}, "required": ["b"]}]}`))
	if !slices.Equal(all.Schema.Required, []string{"a", "b"}) {
		t.Fatalf("allOf required = %v", all.Schema.Required)
	}
	if bad := agent.NormalizeMcpInputSchema(decodeMap(t, `{"type": "object", "properties": {"bad name": {}}}`)); bad.OK {
		t.Fatal("a bad argument name must be refused")
	}
	empty := agent.NormalizeMcpInputSchema(nil)
	if raw, _ := json.Marshal(empty.Schema); !empty.OK || string(raw) != `{"properties":{},"type":"object"}` {
		t.Fatalf("empty = %s", raw)
	}
}

func TestMcpToolOutput(t *testing.T) {
	out := agent.McpToolOutput(mcpserver.CallToolResult{Content: []mcpserver.Content{
		{"type": "text", "text": "hello"},
		{"type": "image", "mimeType": "image/png", "data": "iVBORw0KGgo="},
		{"type": "audio", "mimeType": "audio/wav", "data": "x"},
		{"type": "resource_link", "uri": "file:///a.txt", "name": "a.txt"},
		{"type": "resource", "resource": map[string]any{"uri": "file:///b.txt", "text": "inside"}},
	}})
	for _, want := range []string{"hello", "[image 1: image/png]", "[audio (audio/wav), not shown]", "[resource: a.txt file:///a.txt]", "[resource file:///b.txt]\ninside"} {
		if !strings.Contains(out.Content, want) {
			t.Fatalf("content %q lacks %q", out.Content, want)
		}
	}
	if len(out.Images) != 1 || out.Images[0].MIMEType != "image/png" || out.Images[0].Data != "iVBORw0KGgo=" {
		t.Fatalf("images = %+v", out.Images)
	}
	if got := agent.McpToolOutput(mcpserver.CallToolResult{StructuredContent: map[string]any{"n": 1}}).Content; got != "{\n  \"n\": 1\n}" {
		t.Fatalf("structured = %q", got)
	}
	failed := agent.McpToolOutput(mcpserver.CallToolResult{Content: []mcpserver.Content{{"type": "text", "text": "nope"}}, IsError: true})
	if failed.Content != "nope" || failed.IsError == nil || !*failed.IsError || failed.Retryable == nil || *failed.Retryable {
		t.Fatalf("error = %+v", failed)
	}
}

func TestMcpPermissionRules(t *testing.T) {
	cases := []struct {
		rule, tool, server string
		want               bool
	}{
		{"mcp__github", "mcp__github__create_issue", "github", true},
		{"mcp__github__*", "mcp__github__create_issue", "github", true},
		{"mcp__github__get_*", "mcp__github__get_issue", "github", true},
		{"mcp__github__get_*", "mcp__github__create_issue", "github", false},
		{"mcp__github__create_issue", "mcp__github__create_issue", "github", true},
		{"mcp__git", "mcp__github__x", "github", false},
		{"mcp__github", "mcp__gitlab__x", "gitlab", false},
	}
	for _, c := range cases {
		if got := mcpserver.MatchesRule(c.rule, c.tool, c.server); got != c.want {
			t.Errorf("MatchesRule(%q, %q) = %v", c.rule, c.tool, got)
		}
	}
	if msg := mcpserver.CheckRule("mcp__github__*"); msg != "" {
		t.Fatal(msg)
	}
	for _, bad := range []string{"*", "mcp__*", "github", ""} {
		if mcpserver.CheckRule(bad) == "" {
			t.Errorf("CheckRule(%q) accepted", bad)
		}
	}
}

// fakeConns stands in for the MCP manager: servers with tools, and the calls made.
type fakeConns struct {
	mu       sync.Mutex
	servers  []mcpserver.State
	calls    []string
	failNext error
}

func (f *fakeConns) List() []mcpserver.State {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.servers)
}

func (f *fakeConns) Get(id string) (mcpserver.State, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, s := range f.servers {
		if s.ServerID == id {
			return s, true
		}
	}
	return mcpserver.State{}, false
}

func (f *fakeConns) Reconnect(context.Context, string) (mcpserver.State, error) {
	return f.servers[0], nil
}
func (f *fakeConns) Ping(context.Context, string, mcpserver.CallOptions) error { return nil }

func (f *fakeConns) CallTool(_ context.Context, serverID, name string, args map[string]any, _ mcpserver.CallOptions) (mcpserver.CallToolResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	raw, _ := json.Marshal(args)
	f.calls = append(f.calls, serverID+"/"+name+" "+string(raw))
	if err := f.failNext; err != nil {
		f.failNext = nil
		return mcpserver.CallToolResult{}, err
	}
	return mcpserver.CallToolResult{Content: []mcpserver.Content{{"type": "text", "text": name + " ok"}}}, nil
}

func (f *fakeConns) ReadResource(context.Context, string, string, mcpserver.CallOptions) (mcpserver.ReadResourceResult, error) {
	return mcpserver.ReadResourceResult{}, nil
}

func (f *fakeConns) GetPrompt(context.Context, string, string, map[string]string, mcpserver.CallOptions) (mcpserver.GetPromptResult, error) {
	return mcpserver.GetPromptResult{}, nil
}

func mcpState(id string, tools []mcpserver.Tool) mcpserver.State {
	return mcpserver.State{ServerID: id, Name: id, Status: mcpserver.StatusConnected, Tools: tools}
}

func mcpTool(name string, edit func(*mcpserver.Tool)) mcpserver.Tool {
	tl := mcpserver.Tool{Name: name, Description: name + " things", InputSchema: map[string]any{"type": "object", "properties": map[string]any{"q": map[string]any{"type": "string"}}}}
	if edit != nil {
		edit(&tl)
	}
	return tl
}

type toolsRig struct {
	tools *agent.McpTools
	bus   *events.Dispatcher
	conns *fakeConns
	mu    sync.Mutex
	cfg   *mcpserver.Config
}

func newToolsRig(t *testing.T, states []mcpserver.State, cfg mcpserver.Config, window int) *toolsRig {
	t.Helper()
	r := &toolsRig{bus: events.New(), conns: &fakeConns{servers: states}}
	for _, s := range states {
		cfg.Servers = append(cfg.Servers, s.ServerID)
	}
	r.cfg = &cfg
	r.tools = agent.NewMcpTools(agent.McpToolsDeps{
		AgentID: "a1", Bus: r.bus, Mcp: r.conns,
		Config:              func() *mcpserver.Config { r.mu.Lock(); defer r.mu.Unlock(); return r.cfg },
		ContextWindowTokens: func() int { return window },
	})
	t.Cleanup(r.tools.Dispose)
	return r
}

func (r *toolsRig) setConfig(edit func(*mcpserver.Config)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	c := *r.cfg.Clone()
	edit(&c)
	r.cfg = &c
}

func handlerNames(hs []toolhub.Handler) []string {
	out := make([]string, 0, len(hs))
	for _, h := range hs {
		out = append(out, h.Definition().Name)
	}
	return out
}

func mustGetMcp(t *testing.T, tools *agent.McpTools, name string) toolhub.Handler {
	t.Helper()
	h, ok := tools.Get(name)
	if !ok {
		t.Fatalf("no tool %s", name)
	}
	return h
}

func executeMcp(t *testing.T, h toolhub.Handler, args string, session string) toolhub.Output {
	t.Helper()
	a, err := kernel.NewArguments([]byte(args))
	if err != nil {
		t.Fatal(err)
	}
	out, err := h.Execute(t.Context(), a, toolhub.Context{SessionID: session})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestMcpToolsUpfrontAndEvents(t *testing.T) {
	ro := true
	states := []mcpserver.State{mcpState("github", []mcpserver.Tool{mcpTool("create_issue", nil), mcpTool("get_issue", func(tl *mcpserver.Tool) {
		tl.Annotations = &mcpserver.ToolAnnotations{ReadOnlyHint: &ro}
	})})}
	r := newToolsRig(t, states, mcpserver.Config{}, 200_000)
	if got := handlerNames(r.tools.List("s1")); !slices.Equal(got, []string{"mcp__github__create_issue", "mcp__github__get_issue"}) {
		t.Fatalf("list = %v", got)
	}
	if d := mustGetMcp(t, r.tools, "mcp__github__get_issue").Definition(); d.Risk != toolhub.RiskReadOnly || d.Subagent != toolhub.SubagentNever {
		t.Fatalf("def = %+v", d)
	}
	if d := mustGetMcp(t, r.tools, "mcp__github__create_issue").Definition(); d.Risk != toolhub.RiskConsequential {
		t.Fatalf("risk = %s", d.Risk)
	}
	if _, ok := r.tools.Get(agent.McpToolSearch); ok {
		t.Fatal("tool_search must be off while the tools are small")
	}

	r.conns.mu.Lock()
	r.conns.servers[0].Tools = append(r.conns.servers[0].Tools, mcpTool("close_issue", nil))
	r.conns.mu.Unlock()
	events.Emit(r.bus, events.McpToolsChanged, "", events.McpToolsChangedPayload{AgentID: "a1", ServerID: "github"})
	if !r.tools.Owns("mcp__github__close_issue") {
		t.Fatal("tools-changed was not applied")
	}
	r.conns.mu.Lock()
	r.conns.servers[0].Status = mcpserver.StatusReconnecting
	r.conns.mu.Unlock()
	events.Emit(r.bus, events.McpDisconnected, "", events.McpDisconnectedPayload{AgentID: "other", ServerID: "github"})
	if n := len(r.tools.List("")); n != 3 {
		t.Fatalf("another agent's event changed the list: %d", n)
	}
	events.Emit(r.bus, events.McpDisconnected, "", events.McpDisconnectedPayload{AgentID: "a1", ServerID: "github", WillReconnect: true})
	if n := len(r.tools.List("")); n != 0 {
		t.Fatalf("tools of a disconnected server: %d", n)
	}
	if idx, _ := r.tools.Index(""); !strings.Contains(idx, "github: reconnecting") {
		t.Fatalf("index = %s", idx)
	}
}

func TestMcpToolsApproval(t *testing.T) {
	r := newToolsRig(t, []mcpserver.State{mcpState("github", []mcpserver.Tool{
		mcpTool("create_issue", nil), mcpTool("get_issue", nil), mcpTool("delete_repo", nil),
		mcpTool("pay", func(tl *mcpserver.Tool) { tl.Meta = map[string]any{"anthropic/requiresUserInteraction": true} }),
	})}, mcpserver.Config{}, 200_000)
	approval := func(name string) toolhub.ToolApproval {
		h, ok := r.tools.Get(name)
		if !ok {
			return ""
		}
		return h.(toolhub.CallApprover).ApprovalFor(nil)
	}
	if got := approval("mcp__github__create_issue"); got != toolhub.ToolApprovalAsk {
		t.Fatalf("default = %s", got)
	}
	r.setConfig(func(c *mcpserver.Config) {
		c.Permissions = &mcpserver.Permissions{Allow: []string{"mcp__github"}, Ask: []string{"mcp__github__create_*"}, Deny: []string{"mcp__github__delete_repo"}}
	})
	for name, want := range map[string]toolhub.ToolApproval{
		"mcp__github__get_issue": toolhub.ToolApprovalNever, "mcp__github__create_issue": toolhub.ToolApprovalAsk,
		"mcp__github__pay": toolhub.ToolApprovalAlways, "mcp__github__delete_repo": "",
	} {
		if got := approval(name); got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
	if slices.Contains(handlerNames(r.tools.List("")), "mcp__github__delete_repo") {
		t.Fatal("a denied tool is offered")
	}
	for _, d := range r.tools.Describe() {
		if d.Tool == "delete_repo" && d.Approval != mcpserver.ApprovalDenied {
			t.Fatalf("describe = %+v", d)
		}
	}
}

func TestMcpToolsSearch(t *testing.T) {
	var many []mcpserver.Tool
	for i := range 40 {
		many = append(many, mcpTool(fmt.Sprintf("tool_%d", i), func(tl *mcpserver.Tool) {
			tl.Description = strings.Repeat(fmt.Sprintf("does thing %d ", i), 20)
		}))
	}
	many = append(many, mcpTool("create_issue", func(tl *mcpserver.Tool) { tl.Description = "Create a GitHub issue in a repository" }))
	st := mcpState("github", many)
	st.Instructions = "Use create_issue for bugs."
	r := newToolsRig(t, []mcpserver.State{st}, mcpserver.Config{}, 20_000)
	if !r.tools.SearchActive() {
		t.Fatal("tool search must be on")
	}
	if got := handlerNames(r.tools.List("s1")); !slices.Equal(got, []string{agent.McpToolSearch}) {
		t.Fatalf("list = %v", got)
	}
	idx, _ := r.tools.Index("s1")
	if !strings.Contains(idx, "Instructions: Use create_issue for bugs.") || !strings.Contains(idx, "mcp__github__create_issue") {
		t.Fatalf("index = %s", idx)
	}
	search := mustGetMcp(t, r.tools, agent.McpToolSearch)
	found := executeMcp(t, search, `{"query":"create issue","max_results":0}`, "s1")
	if !strings.Contains(found.Content, "Loaded 1 tool") || !strings.Contains(found.Content, `"name": "mcp__github__create_issue"`) {
		t.Fatalf("found = %s", found.Content)
	}
	if got := handlerNames(r.tools.List("s1")); !slices.Equal(got, []string{"mcp__github__create_issue", agent.McpToolSearch}) {
		t.Fatalf("list after search = %v", got)
	}
	if got := handlerNames(r.tools.List("s2")); !slices.Equal(got, []string{agent.McpToolSearch}) {
		t.Fatalf("other session = %v", got)
	}
	if idx, _ := r.tools.Index("s1"); strings.Contains(idx[max(strings.Index(idx, "Deferred tools"), 0):], "mcp__github__create_issue") {
		t.Fatal("a loaded tool is still listed as deferred")
	}
	selected := executeMcp(t, search, `{"query":"select:mcp__github__tool_1, tool_2, nope"}`, "s2")
	if !strings.Contains(selected.Content, "Loaded 2 tools") || !strings.Contains(selected.Content, "Not found: nope") {
		t.Fatalf("selected = %s", selected.Content)
	}
	if out := executeMcp(t, search, `{"query":"zzz"}`, "s2"); out.IsError == nil || !*out.IsError {
		t.Fatal("no match must be an error")
	}
	if _, ok := r.tools.Get("mcp__github__tool_7"); !ok {
		t.Fatal("a deferred tool must still run by name")
	}
}

func TestMcpToolsSearchModes(t *testing.T) {
	r := newToolsRig(t, []mcpserver.State{mcpState("github", []mcpserver.Tool{mcpTool("a", nil)})}, mcpserver.Config{}, 200_000)
	r.setConfig(func(c *mcpserver.Config) { c.ToolSearch = mcpserver.ToolSearchOn })
	if got := handlerNames(r.tools.List("")); !slices.Equal(got, []string{agent.McpToolSearch}) {
		t.Fatalf("on = %v", got)
	}
	r.setConfig(func(c *mcpserver.Config) { c.ToolSearch = mcpserver.ToolSearchOff })
	if got := handlerNames(r.tools.List("")); !slices.Equal(got, []string{"mcp__github__a"}) {
		t.Fatalf("off = %v", got)
	}
}

func TestMcpToolsCallsAndRepeat(t *testing.T) {
	ro := true
	r := newToolsRig(t, []mcpserver.State{mcpState("github", []mcpserver.Tool{
		mcpTool("get_issue", func(tl *mcpserver.Tool) { tl.Annotations = &mcpserver.ToolAnnotations{ReadOnlyHint: &ro} }),
		mcpTool("create_issue", nil),
	})}, mcpserver.Config{}, 200_000)
	if out := executeMcp(t, mustGetMcp(t, r.tools, "mcp__github__get_issue"), `{"q":"1"}`, "s1"); out.Content != "get_issue ok" {
		t.Fatalf("out = %+v", out)
	}
	countCalls := func(arg string) int {
		r.conns.mu.Lock()
		defer r.conns.mu.Unlock()
		n := 0
		for _, c := range r.conns.calls {
			if strings.Contains(c, arg) {
				n++
			}
		}
		return n
	}
	r.conns.mu.Lock()
	r.conns.failNext = mcpserver.NewError("the MCP connection was closed", mcpserver.CodeClosed)
	r.conns.mu.Unlock()
	if out := executeMcp(t, mustGetMcp(t, r.tools, "mcp__github__get_issue"), `{"q":"2"}`, "s1"); out.Content != "get_issue ok" {
		t.Fatalf("repeat = %+v", out)
	}
	if n := countCalls(`"2"`); n != 2 {
		t.Fatalf("calls = %d", n)
	}
	r.conns.mu.Lock()
	r.conns.failNext = mcpserver.NewError("the MCP connection was closed", mcpserver.CodeClosed)
	r.conns.mu.Unlock()
	failed := executeMcp(t, mustGetMcp(t, r.tools, "mcp__github__create_issue"), `{"q":"3"}`, "s1")
	if failed.IsError == nil || !*failed.IsError || failed.Retryable == nil || *failed.Retryable || !strings.Contains(failed.Content, "could not run create_issue") {
		t.Fatalf("failed = %+v", failed)
	}
	if n := countCalls(`"3"`); n != 1 {
		t.Fatalf("a call that is not safe to repeat ran %d times", n)
	}
}

func TestMcpToolsLeftOut(t *testing.T) {
	r := newToolsRig(t, []mcpserver.State{mcpState("github", []mcpserver.Tool{
		mcpTool("ok", nil),
		mcpTool("bad", func(tl *mcpserver.Tool) {
			tl.InputSchema = map[string]any{"type": "object", "properties": map[string]any{"a b": map[string]any{}}}
		}),
		mcpTool("ok", nil),
	})}, mcpserver.Config{}, 200_000)
	if got := handlerNames(r.tools.List("")); !slices.Equal(got, []string{"mcp__github__ok"}) {
		t.Fatalf("list = %v", got)
	}
	idx, _ := r.tools.Index("")
	if !strings.Contains(idx, "Left out: bad") || !strings.Contains(idx, "is taken by another tool") {
		t.Fatalf("index = %s", idx)
	}
}

func TestMcpToolsResultLimit(t *testing.T) {
	r := newToolsRig(t, []mcpserver.State{mcpState("db", []mcpserver.Tool{
		mcpTool("schema", func(tl *mcpserver.Tool) { tl.Meta = map[string]any{"anthropic/maxResultSizeChars": float64(9_000_000)} }),
	})}, mcpserver.Config{}, 200_000)
	if got := mustGetMcp(t, r.tools, "mcp__db__schema").Definition().MaxResultChars; got != 500_000 {
		t.Fatalf("limit = %d", got)
	}
}

// ── Through an agent ──

// approvalRecorder records the approval requests and answers with approve.
type approvalRecorder struct {
	mu       sync.Mutex
	approve  bool
	requests []string
}

func (*approvalRecorder) AskUser(context.Context, toolhub.ClarifyRequest) (toolhub.ClarifyResponse, error) {
	return toolhub.ClarifyResponse{Answer: "?"}, nil
}

func (h *approvalRecorder) RequestApproval(_ context.Context, req toolhub.ApprovalRequest) (toolhub.ApprovalDecision, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.requests = append(h.requests, req.ToolName)
	return toolhub.ApprovalDecision{Approved: h.approve}, nil
}

func (h *approvalRecorder) names() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Clone(h.requests)
}

// scriptedCalls is a mock model that makes the given calls, one per model call, then
// ends the turn.
func scriptedCalls(t *testing.T, calls ...[2]string) *aecmock.MockProvider {
	t.Helper()
	return aecmock.NewMockProvider("scripted", nil, aecmock.WithResponse(func(msgs []kernel.Message) kernel.ModelResponse {
		usage := kernel.TokenUsage{InputTokens: 1, OutputTokens: 1}
		done := 0
		for _, m := range msgs {
			if len(m.ToolResults) > 0 {
				done++
			}
		}
		if done >= len(calls) {
			return kernel.ModelResponse{Message: kernel.Message{Role: kernel.RoleAssistant, Content: "done"}, Usage: usage, StopReason: kernel.StopEndTurn}
		}
		args, err := kernel.NewArguments([]byte(calls[done][1]))
		if err != nil {
			panic(err)
		}
		return kernel.ModelResponse{
			Message:    kernel.Message{Role: kernel.RoleAssistant, ToolCalls: []kernel.ToolCall{{ID: fmt.Sprintf("tc-%d", done+1), Name: calls[done][0], Arguments: args}}},
			Usage:      usage,
			StopReason: kernel.StopToolUse,
		}
	}))
}

// toolResults are the tool results the mock saw, in order.
func toolResults(m *aecmock.MockProvider) []string {
	calls := m.Calls()
	if len(calls) == 0 {
		return nil
	}
	var out []string
	for _, msg := range calls[len(calls)-1].Messages {
		for _, r := range msg.ToolResults {
			out = append(out, r.Content)
		}
	}
	return out
}

func offered(m *aecmock.MockProvider, i int) []string {
	calls := m.Calls()
	if i >= len(calls) {
		return nil
	}
	var out []string
	for _, d := range calls[i].Options.Tools {
		out = append(out, d.Name)
	}
	return out
}

func mcpSystemOf(m *aecmock.MockProvider, i int) string {
	calls := m.Calls()
	if i >= len(calls) {
		return ""
	}
	for _, msg := range calls[i].Messages {
		if msg.Role == kernel.RoleSystem {
			return msg.Content
		}
	}
	return ""
}

func mcpAgent(t *testing.T, cfg mcpserver.Config, approve bool, calls ...[2]string) (*agent.Agent, *aecmock.MockProvider, *approvalRecorder) {
	t.Helper()
	r := newMcpRig(t, stdioDef("fake", nil))
	model := scriptedCalls(t, calls...)
	if err := r.env.llm.Register(model); err != nil {
		t.Fatal(err)
	}
	rec := &approvalRecorder{approve: approve}
	r.env.tools.SetInteractionHandler(rec)
	a := r.spawn(t, "a1", []string{"fake"})
	cfg.Servers = []string{"fake"}
	a.UpdateProvider("scripted")
	if err := a.UpdateMcp(t.Context(), &cfg); err != nil {
		t.Fatal(err)
	}
	if err := a.StartMcp(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := a.StartSession(t.Context()); err != nil {
		t.Fatal(err)
	}
	return a, model, rec
}

func TestMcpToolsThroughAnAgent(t *testing.T) {
	a, model, rec := mcpAgent(t, mcpserver.Config{}, true, [2]string{"mcp__fake__echo", `{"text":"from the model"}`})
	chat(t, a, "go")
	if got := offered(model, 0); !slices.Contains(got, "mcp__fake__echo") || !slices.Contains(got, "mcp__fake__add") {
		t.Fatalf("offered = %v", got)
	}
	if sys := mcpSystemOf(model, 0); !strings.Contains(sys, "<mcp_servers>") || !strings.Contains(sys, "Use echo to test.") {
		t.Fatalf("system = %s", sys)
	}
	if got := rec.names(); !slices.Equal(got, []string{"mcp__fake__echo"}) {
		t.Fatalf("approvals = %v", got)
	}
	if res := toolResults(model); len(res) != 1 || !strings.Contains(res[0], "from the model") {
		t.Fatalf("results = %v", res)
	}
	var echo *mcpserver.AgentTool
	for _, d := range a.McpToolList() {
		if d.Name == "mcp__fake__echo" {
			echo = &d
		}
	}
	if echo == nil || echo.ServerID != "fake" || echo.Tool != "echo" || !echo.ReadOnly || echo.Approval != mcpserver.ApprovalAsk {
		t.Fatalf("describe = %+v", echo)
	}
}

func TestMcpToolsAllowedAndDenied(t *testing.T) {
	a, model, rec := mcpAgent(t, mcpserver.Config{Permissions: &mcpserver.Permissions{Allow: []string{"mcp__fake__*"}}}, true, [2]string{"mcp__fake__add", `{"a":2,"b":3}`})
	chat(t, a, "go")
	if got := rec.names(); len(got) != 0 {
		t.Fatalf("an allowed tool asked: %v", got)
	}
	if res := toolResults(model); len(res) != 1 || !strings.Contains(res[0], "5") {
		t.Fatalf("results = %v", res)
	}

	b, model2, rec2 := mcpAgent(t, mcpserver.Config{}, false, [2]string{"mcp__fake__add", `{"a":1,"b":1}`})
	chat(t, b, "go")
	if got := rec2.names(); !slices.Equal(got, []string{"mcp__fake__add"}) {
		t.Fatalf("approvals = %v", got)
	}
	if res := toolResults(model2); len(res) != 1 || strings.Contains(res[0], "2") {
		t.Fatalf("a denied call ran: %v", res)
	}
}

func TestMcpToolSearchThroughAnAgent(t *testing.T) {
	a, model, _ := mcpAgent(t, mcpserver.Config{ToolSearch: mcpserver.ToolSearchOn, Permissions: &mcpserver.Permissions{Allow: []string{"mcp__fake"}}}, true,
		[2]string{agent.McpToolSearch, `{"query":"select:mcp__fake__add"}`},
		[2]string{"mcp__fake__add", `{"a":20,"b":22}`},
	)
	chat(t, a, "go")
	if got := offered(model, 0); !slices.Equal(got, []string{agent.McpToolSearch}) {
		t.Fatalf("first offer = %v", got)
	}
	if sys := mcpSystemOf(model, 0); !strings.Contains(sys, "Deferred tools") {
		t.Fatalf("system = %s", sys)
	}
	if got := offered(model, 1); !slices.Equal(got, []string{"mcp__fake__add", agent.McpToolSearch}) {
		t.Fatalf("second offer = %v", got)
	}
	res := toolResults(model)
	if len(res) != 2 || !strings.Contains(res[0], "Loaded 1 tool") || !strings.Contains(res[1], "42") {
		t.Fatalf("results = %v", res)
	}
}
