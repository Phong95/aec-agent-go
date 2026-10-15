package e2e_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"

	aec "github.com/Phong95/aec-agent-go"
	"github.com/Phong95/aec-agent-go/aecmock"
	"github.com/Phong95/aec-agent-go/components/agent"
	"github.com/Phong95/aec-agent-go/components/agent/profile"
	"github.com/Phong95/aec-agent-go/components/events"
	"github.com/Phong95/aec-agent-go/components/sandboxhub"
	"github.com/Phong95/aec-agent-go/components/toolhub"
	"github.com/Phong95/aec-agent-go/components/toolhub/builtin/workspace"
	"github.com/Phong95/aec-agent-go/components/trace"
	"github.com/Phong95/aec-agent-go/kernel"
)

// TestWorkspace_WriteFileEndToEnd is the tracer of plan 11-02: an agent built with
// CreateAgent and the write_file tool writes a file into its folder of the in-memory
// sandbox of a real AgentCore, and the tool message the model receives is the expected text
// ("Created <path> (<size>, <n> lines) - verified by read-back (sha256 <12 hex>)").
func TestWorkspace_WriteFileEndToEnd(t *testing.T) {
	ctx := t.Context()
	wc := newWorkspaceCore(t, workspaceCoreOptions{})
	core, provider := wc.core, wc.docker

	const content = "hello\n"
	model := aecmock.NewScriptedProvider("scripted", []aecmock.ScriptedTurn{
		toolTurn(toolCallOf("tc-1", "write_file", `{"path":"notes.md","content":"hello\n"}`)),
		{Content: "Written"},
	})

	a, err := aec.CreateAgent(ctx, aec.AgentConfig{
		Provider: "scripted",
		APIKey:   "k",
		Model:    "m",
		Tools:    []toolhub.Handler{workspace.NewWriteFileTool(workspace.Options{})},
	}, aec.WithCore(core), aec.WithProviderRegistry(registryFor(model)))
	if err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}
	disposed := false
	dispose := func() {
		t.Helper()
		if disposed {
			return
		}
		disposed = true
		if err := a.Dispose(ctx); err != nil {
			t.Errorf("Dispose: %v", err)
		}
	}
	defer dispose()

	if _, err := a.StartSession(ctx); err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	res, err := a.Chat(ctx, "Write the notes")
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if res.Message.Content != "Written" {
		t.Errorf("reply = %q, want Written", res.Message.Content)
	}

	// The file is in the agent's own folder of the in-memory sandbox, with that content.
	if n := provider.CreateCount(); n != 1 {
		t.Errorf("CreateCount = %d, want one shared sandbox", n)
	}
	sb, ok := provider.Sandbox("docker:shared")
	if !ok {
		t.Fatal("the provider holds no shared sandbox")
	}
	path := "/workspace/agents/" + a.ID() + "/notes.md"
	if !sb.Has(path) {
		t.Fatalf("the sandbox has no file %s", path)
	}
	view, err := a.Sandbox(ctx)
	if err != nil {
		t.Fatalf("Sandbox: %v", err)
	}
	got, err := view.ReadFile(ctx, "notes.md", sandboxhub.ReadOptions{})
	if err != nil || got.Content != content {
		t.Fatalf("notes.md = %+v, %v; want content %q", got, err, content)
	}

	// The model received the expected text as the tool result of the same turn.
	sum := sha256.Sum256([]byte(content))
	short := hex.EncodeToString(sum[:])[:12]
	want := "Created " + path + " (6 B, 1 lines) \u2014 verified by read-back (sha256 " + short + ")"
	var toolContent string
	var isError bool
	for _, call := range model.Calls() {
		for _, m := range call.Messages {
			for _, r := range m.ToolResults {
				toolContent = r.Content
				isError = r.IsError != nil && *r.IsError
			}
		}
	}
	if !strings.HasPrefix(toolContent, "Created ") || !strings.Contains(toolContent, "verified by read-back (sha256 "+short) {
		t.Errorf("tool message = %q, want it to start with Created and carry sha256 %s", toolContent, short)
	}
	if toolContent != want {
		t.Errorf("tool message = %q\n want %q", toolContent, want)
	}
	if isError {
		t.Error("the tool result is an error")
	}
	dispose()
}

// ---------------------------------------------------------------------------
// TestWorkspaceToolsInTheAgentLoop: the 8 end-to-end cases of the workspace tools
// ---------------------------------------------------------------------------

// e2eAgentID is the id of the test agent.
const e2eAgentID = "e2e-agent"

// loopCall is one scripted tool call: the model issues one per turn.
type loopCall struct{ name, args string }

// loopRunOptions are the options of runWorkspaceLoop.
type loopRunOptions struct {
	calls []loopCall
	// tools is the allowlist of the agent profile; nil means every workspace tool.
	tools []string
	// sandbox is the sandbox selection of the profile; the zero value is the shared one.
	sandbox sandboxhub.Selection
	// maxRetries is LoopConfig.MaxToolRetries; nil means the default of the test, 2.
	maxRetries *int
}

// loopRun is a finished run: the agent after one chat in which the scripted model issued
// the calls, with the tool result the model saw after each call.
type loopRun struct {
	wc       *workspaceCore
	provider *aecmock.ScriptedProvider
	agent    *agent.Agent
	results  []kernel.ToolResult
}

// runWorkspaceLoop runs one case: every workspace tool is registered on the core, an
// agent with the given allowlist and sandbox selection is spawned, and one chat plays the
// scripted calls, one per model turn.
func runWorkspaceLoop(t *testing.T, wc *workspaceCore, o loopRunOptions) *loopRun {
	t.Helper()
	ctx := t.Context()
	turns := make([]aecmock.ScriptedTurn, 0, len(o.calls)+1)
	for i, c := range o.calls {
		turns = append(turns, toolTurn(toolCallOf(fmt.Sprintf("tc-%d", i+1), c.name, c.args)))
	}
	turns = append(turns, aecmock.ScriptedTurn{Content: "done"})
	provider := aecmock.NewScriptedProvider("scripted", turns)
	if err := wc.core.UseLLMProvider(provider); err != nil {
		t.Fatalf("UseLLMProvider: %v", err)
	}
	for _, h := range workspace.NewTools(workspace.Options{}) {
		if err := wc.core.UseTool(h); err != nil {
			t.Fatalf("UseTool: %v", err)
		}
	}

	allowed := o.tools
	if allowed == nil {
		allowed = workspace.ToolNames()
	}
	retries, backoff := 2, 0
	if o.maxRetries != nil {
		retries = *o.maxRetries
	}
	a, err := wc.core.SpawnAgent(ctx, profile.AgentProfile{
		ID: e2eAgentID, Name: "e2e", SystemPrompt: "test", Tools: allowed, Skills: []string{},
		Compactor: testCompactor(), Memory: loopMemoryOff(),
		LoopConfig: &profile.LoopConfig{MaxToolRetries: &retries, RetryBackoffMs: &backoff},
		Sandbox:    o.sandbox,
	})
	if err != nil {
		t.Fatalf("SpawnAgent: %v", err)
	}
	if _, err := a.StartSession(ctx); err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	if _, err := a.Chat(ctx, "go"); err != nil {
		t.Fatalf("Chat: %v", err)
	}

	// Record, at every model turn after the first, the first result of the last tool
	// message the model was given.
	var results []kernel.ToolResult
	calls := provider.Calls()
	for i := 1; i < len(calls); i++ {
		msgs := calls[i].Messages
		for j := len(msgs) - 1; j >= 0; j-- {
			if len(msgs[j].ToolResults) > 0 {
				results = append(results, msgs[j].ToolResults[0])
				break
			}
		}
	}
	return &loopRun{wc: wc, provider: provider, agent: a, results: results}
}

func (r *loopRun) dispose(t *testing.T) {
	t.Helper()
	if err := r.wc.core.Dispose(context.WithoutCancel(t.Context())); err != nil {
		t.Errorf("Dispose: %v", err)
	}
}

// endedSpans are the spans of a kind, as they were when they ended, in order.
func (r *loopRun) endedSpans(kind trace.SpanKind) []trace.Span {
	var out []trace.Span
	for _, rec := range r.wc.spans.all() {
		if rec.phase == "end" && rec.span.Kind == kind {
			out = append(out, rec.span)
		}
	}
	return out
}

// requireResults fails unless the model saw exactly n tool results.
func (r *loopRun) requireResults(t *testing.T, n int) {
	t.Helper()
	if len(r.results) != n {
		t.Fatalf("the model saw %d tool results, want %d: %+v", len(r.results), n, r.results)
	}
}

// fileIn reads a file through the agent's sandbox view, or reports that it is absent.
func fileIn(t *testing.T, a *agent.Agent, path string) (string, bool) {
	t.Helper()
	view, err := a.Sandbox(t.Context())
	if err != nil {
		t.Fatalf("Sandbox: %v", err)
	}
	fc, err := view.ReadFile(t.Context(), path, sandboxhub.ReadOptions{})
	if err != nil {
		return "", false
	}
	return fc.Content, true
}

// workspaceApprovals is an approval handler: it approves everything except the
// terminal tool, which it declines with the reason "not now".
func workspaceApprovals() *aecmock.ScriptedInteraction {
	return aecmock.NewScriptedInteraction(aecmock.WithApprovalFunc(func(_ context.Context, req toolhub.ApprovalRequest) (toolhub.ApprovalDecision, error) {
		reason := "not now"
		return toolhub.ApprovalDecision{Approved: req.ToolName != "terminal", Reason: &reason}, nil
	}))
}

func TestWorkspaceToolsInTheAgentLoop(t *testing.T) {
	t.Run("writes, reads, edits, searches and runs commands in the shared sandbox, traced per operation", func(t *testing.T) {
		run := runWorkspaceLoop(t, newWorkspaceCore(t, workspaceCoreOptions{}), loopRunOptions{calls: []loopCall{
			{"write_file", `{"path":"rfi/draft.md","content":"Beam B1: 300x600\nStatus: open\n"}`},
			{"read_file", `{"path":"rfi/draft.md"}`},
			{"edit_file", `{"path":"rfi/draft.md","old_text":"Status: open","new_text":"Status: answered"}`},
			{"search_files", `{"pattern":"answered"}`},
			{"terminal", `{"command":"cat rfi/draft.md"}`},
			{"list_files", `{"recursive":true}`},
		}})
		defer run.dispose(t)

		run.requireResults(t, 6)
		for i, r := range run.results {
			if isErrorResult(r) {
				t.Errorf("result %d is an error: %q", i, r.Content)
			}
		}
		for i, want := range map[int]string{
			0: "verified by read-back",
			1: "1\tBeam B1: 300x600",
			3: "rfi/draft.md:2: Status: answered",
			4: "Status: answered",
			5: "rfi/draft.md",
		} {
			if !strings.Contains(run.results[i].Content, want) {
				t.Errorf("result %d = %q, want it to contain %q", i, run.results[i].Content, want)
			}
		}
		if got, ok := fileIn(t, run.agent, "rfi/draft.md"); !ok || got != "Beam B1: 300x600\nStatus: answered\n" {
			t.Errorf("the file in the sandbox = %q (present %v)", got, ok)
		}
		if _, ok := run.wc.docker.Sandbox("docker:shared"); !ok {
			t.Error("the shared sandbox was never created")
		}

		// Every sandbox operation is a child of its tool's dispatch span and carries no file contents.
		dispatch := run.endedSpans(trace.KindToolDispatch)
		ops := run.endedSpans(trace.KindSandboxOp)
		var names []string
		for _, d := range dispatch {
			names = append(names, d.Name)
		}
		if want := []string{"write_file", "read_file", "edit_file", "search_files", "terminal", "list_files"}; !slices.Equal(names, want) {
			t.Fatalf("tool-dispatch spans = %v, want %v", names, want)
		}
		opsOf := func(toolName string) []string {
			var id string
			for _, d := range dispatch {
				if d.Name == toolName {
					id = d.SpanID
				}
			}
			var out []string
			for _, o := range ops {
				if o.ParentSpanID == id {
					out = append(out, o.Name)
				}
			}
			return out
		}
		if got, want := opsOf("write_file"), []string{"fs.write", "fs.read"}; !slices.Equal(got, want) { // write + read-back
			t.Errorf("write_file ops = %v, want %v", got, want)
		}
		if got, want := opsOf("edit_file"), []string{"fs.read", "fs.write", "fs.read"}; !slices.Equal(got, want) {
			t.Errorf("edit_file ops = %v, want %v", got, want)
		}
		if got, want := opsOf("terminal"), []string{"exec"}; !slices.Equal(got, want) {
			t.Errorf("terminal ops = %v, want %v", got, want)
		}
		if raw, err := kernel.EncodeJSON(ops); err != nil || strings.Contains(string(raw), "300x600") {
			t.Errorf("the sandbox-op spans must carry no file contents (err %v): %s", err, raw)
		}
		for _, op := range ops {
			attrs := attrsJSON(t, op)
			if attrs["sandboxId"] != "docker:shared" || attrs["agentId"] != e2eAgentID {
				t.Errorf("span %s attributes = %v, want sandboxId docker:shared and agentId %s", op.Name, attrs, e2eAgentID)
			}
		}
	})

	t.Run("enforces the profile allowlist and argument schema before any sandbox is started", func(t *testing.T) {
		run := runWorkspaceLoop(t, newWorkspaceCore(t, workspaceCoreOptions{}), loopRunOptions{
			tools: []string{"read_file"},
			calls: []loopCall{
				{"terminal", `{"command":"echo hi"}`},
				{"read_file", `{"path":42}`},
			},
		})
		defer run.dispose(t)

		run.requireResults(t, 2)
		if !strings.Contains(run.results[0].Content, `Tool "terminal" not available`) {
			t.Errorf("result 0 = %q", run.results[0].Content)
		}
		if !regexp.MustCompile(`Policy denied: Argument "path" must be string`).MatchString(run.results[1].Content) {
			t.Errorf("result 1 = %q", run.results[1].Content)
		}
		if n := run.wc.docker.CreateCount(); n != 0 {
			t.Errorf("CreateCount = %d, want 0: no sandbox may start before the policy allows the call", n)
		}
		if n := run.wc.log.count(events.ToolDenied.Name); n != 1 {
			t.Errorf("tool-denied events = %d, want 1", n)
		}
	})

	t.Run("lets the host's authorization hook veto a call", func(t *testing.T) {
		var mu sync.Mutex
		var hookCalls []toolhub.Context
		hook := func(_ context.Context, name string, args kernel.Arguments, tc toolhub.Context) (toolhub.Decision, error) {
			mu.Lock()
			hookCalls = append(hookCalls, tc)
			mu.Unlock()
			var a struct {
				Path string `json:"path"`
			}
			if err := json.Unmarshal(args, &a); err != nil {
				return toolhub.Decision{}, err
			}
			if name == "write_file" && strings.HasPrefix(a.Path, "contracts/") {
				return toolhub.Deny("contracts are read-only for agents"), nil
			}
			return toolhub.Allow(), nil
		}
		zero := 0
		run := runWorkspaceLoop(t, newWorkspaceCore(t, workspaceCoreOptions{hook: hook}), loopRunOptions{
			tools:      []string{"write_file"},
			maxRetries: &zero,
			calls: []loopCall{
				{"write_file", `{"path":"contracts/c1.md","content":"x"}`},
				{"write_file", `{"path":"notes/n1.md","content":"x"}`},
			},
		})
		defer run.dispose(t)

		run.requireResults(t, 2)
		if !strings.Contains(run.results[0].Content, "Policy denied: contracts are read-only for agents") {
			t.Errorf("result 0 = %q", run.results[0].Content)
		}
		if !strings.HasPrefix(run.results[1].Content, "Created") {
			t.Errorf("result 1 = %q, want it to start with Created", run.results[1].Content)
		}
		mu.Lock()
		defer mu.Unlock()
		if len(hookCalls) == 0 || hookCalls[0].AgentID != e2eAgentID {
			t.Errorf("the hook saw the tool contexts %+v, want the first with agent %s", hookCalls, e2eAgentID)
		}
	})

	t.Run("denies host paths and other agents' folders, without retrying", func(t *testing.T) {
		run := runWorkspaceLoop(t, newWorkspaceCore(t, workspaceCoreOptions{}), loopRunOptions{calls: []loopCall{
			{"read_file", `{"path":"/etc/passwd"}`},
			{"write_file", `{"path":"C:\\Windows\\System32\\evil.dll","content":"x"}`},
			{"list_files", `{"path":"../other-agent"}`},
			{"terminal", `{"command":"pwd","cwd":"/etc"}`},
			{"read_file", `{"path":"/workspace/agents/other-agent/secret.txt"}`},
		}})
		defer run.dispose(t)

		run.requireResults(t, 5)
		for i, r := range run.results {
			if !isErrorResult(r) || !strings.Contains(r.Content, "path_escape") {
				t.Errorf("result %d = %+v, want a path_escape error", i, r)
			}
		}
		if n := run.wc.log.count(events.ToolRetry.Name); n != 0 {
			t.Errorf("tool-retry events = %d, want 0", n)
		}
		view, err := run.agent.Sandbox(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		all, limit := true, 1000
		listed, err := view.List(t.Context(), "/workspace/agents/"+e2eAgentID, sandboxhub.ListOptions{Recursive: &all, MaxEntries: &limit, IncludeHidden: &all})
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range listed.Entries {
			if strings.Contains(e.Path, "evil") {
				t.Errorf("the sandbox holds %s: a host path must never create a file", e.Path)
			}
		}
	})

	t.Run("never asks for approval in an isolated sandbox", func(t *testing.T) {
		handler := workspaceApprovals()
		run := runWorkspaceLoop(t, newWorkspaceCore(t, workspaceCoreOptions{interaction: handler}), loopRunOptions{calls: []loopCall{
			{"write_file", `{"path":"a.txt","content":"x"}`},
			{"terminal", `{"command":"echo ok"}`},
		}})
		defer run.dispose(t)

		if n := len(handler.Approvals()); n != 0 {
			t.Errorf("requestApproval was called %d times, want never", n)
		}
		run.requireResults(t, 2)
		for i, r := range run.results {
			if isErrorResult(r) {
				t.Errorf("result %d is an error: %q", i, r.Content)
			}
		}
	})

	t.Run("asks for approval for writes and commands on a host folder, not for reads", func(t *testing.T) {
		handler := workspaceApprovals() // approves write_file, declines terminal
		own := sandboxhub.Selection{Mode: sandboxhub.SelectionOwn, Own: &sandboxhub.OwnConfig{}}
		run := runWorkspaceLoop(t, newWorkspaceCore(t, workspaceCoreOptions{withHost: true, interaction: handler}), loopRunOptions{
			sandbox: own,
			calls: []loopCall{
				{"write_file", `{"path":"model.ifc.txt","content":"IFC data"}`},
				{"read_file", `{"path":"model.ifc.txt"}`},
				{"terminal", `{"command":"echo should-not-run"}`},
			},
		})
		defer run.dispose(t)

		reqs := handler.Approvals()
		if len(reqs) != 2 {
			t.Fatalf("requestApproval was called %d times, want 2: %+v", len(reqs), reqs)
		}
		hostID := "host:own:" + e2eAgentID
		w := reqs[0]
		if w.ToolName != "write_file" || w.Risk != toolhub.RiskDraft || w.Sandbox == nil ||
			w.Sandbox.ID != hostID || w.Sandbox.Kind != sandboxhub.KindHost || w.Sandbox.Isolated {
			t.Errorf("write request = %+v (sandbox %+v), want write_file, draft, a host sandbox %s that is not isolated", w, w.Sandbox, hostID)
		}
		if !strings.Contains(w.Reason, "not isolated") {
			t.Errorf("write request reason = %q, want it to say the sandbox is not isolated", w.Reason)
		}
		if reqs[1].ToolName != "terminal" {
			t.Errorf("second request is for %q, want terminal", reqs[1].ToolName)
		}

		run.requireResults(t, 3)
		if !strings.HasPrefix(run.results[0].Content, "Created") {
			t.Errorf("result 0 = %q", run.results[0].Content)
		}
		if !strings.Contains(run.results[1].Content, "IFC data") {
			t.Errorf("result 1 = %q", run.results[1].Content)
		}
		if !isErrorResult(run.results[2]) || !strings.Contains(run.results[2].Content, "Approval denied") {
			t.Errorf("result 2 = %+v, want an Approval denied error", run.results[2])
		}
		if got, ok := fileIn(t, run.agent, "/workspace/model.ifc.txt"); !ok || got != "IFC data" {
			t.Errorf("the host file = %q (present %v), want IFC data", got, ok)
		}

		var terminalSpan trace.Span
		for _, d := range run.endedSpans(trace.KindToolDispatch) {
			if d.Name == "terminal" {
				terminalSpan = d
			}
		}
		attrs := attrsJSON(t, terminalSpan)
		if attrs["approvalSandbox"] != hostID || attrs["error"] != "approval-denied" {
			t.Errorf("terminal span attributes = %v, want approvalSandbox %s and error approval-denied", attrs, hostID)
		}
		// Declined: the command never reached the sandbox.
		for _, op := range run.endedSpans(trace.KindSandboxOp) {
			if op.ParentSpanID == terminalSpan.SpanID {
				t.Errorf("a declined command opened the sandbox operation %s", op.Name)
			}
		}
	})

	t.Run("fails closed on a host folder when no user can approve", func(t *testing.T) {
		own := sandboxhub.Selection{Mode: sandboxhub.SelectionOwn, Own: &sandboxhub.OwnConfig{}}
		run := runWorkspaceLoop(t, newWorkspaceCore(t, workspaceCoreOptions{withHost: true}), loopRunOptions{
			sandbox: own,
			calls:   []loopCall{{"write_file", `{"path":"x.txt","content":"x"}`}},
		})
		defer run.dispose(t)

		run.requireResults(t, 1)
		if !isErrorResult(run.results[0]) || !strings.Contains(run.results[0].Content, "no interactive user is available") {
			t.Errorf("result = %+v, want an error that says no interactive user is available", run.results[0])
		}
		if sb, ok := run.wc.host.Sandbox("host:own:" + e2eAgentID); ok && sb.Has("/workspace/x.txt") {
			t.Error("the file was written although nobody could approve it")
		}
	})

	// The approval gate fails closed for a sandbox tool whose sandbox cannot be resolved:
	// without the sandbox's traits nobody knows whether the call touches the host, so the
	// user decides. The case therefore runs with a user who approves, and the tool still
	// reports the disabled sandbox without a retry; with nobody to ask, the gate denies the
	// call before the tool runs.
	t.Run(`reports "no sandbox" for an agent with sandbox: "none", without retrying`, func(t *testing.T) {
		none := sandboxhub.Selection{Mode: sandboxhub.SelectionNone}
		call := []loopCall{{"terminal", `{"command":"echo hi"}`}}

		handler := aecmock.NewScriptedInteraction() // approves
		run := runWorkspaceLoop(t, newWorkspaceCore(t, workspaceCoreOptions{interaction: handler}), loopRunOptions{sandbox: none, calls: call})
		defer run.dispose(t)
		run.requireResults(t, 1)
		if !isErrorResult(run.results[0]) || !strings.Contains(run.results[0].Content, "(disabled)") {
			t.Errorf("result = %+v, want an error that says (disabled)", run.results[0])
		}
		if n := run.wc.log.count(events.ToolRetry.Name); n != 0 {
			t.Errorf("tool-retry events = %d, want 0", n)
		}

		closed := runWorkspaceLoop(t, newWorkspaceCore(t, workspaceCoreOptions{}), loopRunOptions{sandbox: none, calls: call})
		defer closed.dispose(t)
		closed.requireResults(t, 1)
		if !isErrorResult(closed.results[0]) || !strings.Contains(closed.results[0].Content, "Approval denied") ||
			!strings.Contains(closed.results[0].Content, "could not be resolved") {
			t.Errorf("without a user the result = %+v, want the fail-closed approval denial", closed.results[0])
		}
	})
}
