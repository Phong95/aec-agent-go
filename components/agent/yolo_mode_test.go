package agent_test

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/Phong95/aec-agent-go/aecmock"
	"github.com/Phong95/aec-agent-go/components/agent"
	"github.com/Phong95/aec-agent-go/components/agent/profile"
	"github.com/Phong95/aec-agent-go/components/events"
	"github.com/Phong95/aec-agent-go/components/llmhub"
	"github.com/Phong95/aec-agent-go/components/toolhub"
	"github.com/Phong95/aec-agent-go/components/trace"
	"github.com/Phong95/aec-agent-go/kernel"
)

// A-135: YOLO mode - a session whose calls that need approval run without asking.

// sendRFITool is a consequential tool that counts its runs.
type sendRFITool struct{ runs atomic.Int32 }

func (*sendRFITool) Name() string    { return "tool:send-rfi" }
func (*sendRFITool) Version() string { return "1.0.0" }
func (*sendRFITool) Definition() toolhub.Definition {
	return toolhub.Definition{
		Name:        "send_rfi",
		Description: "Send an RFI email to an external consultant",
		Parameters:  toolhub.JSONSchema{Type: "object"},
		Risk:        toolhub.RiskConsequential,
	}
}

func (t *sendRFITool) Execute(context.Context, kernel.Arguments, toolhub.Context) (toolhub.Output, error) {
	t.runs.Add(1)
	return toolhub.Output{Content: "sent"}, nil
}

// yoloHandler approves every call and runs onApproval first (nil = nothing).
type yoloHandler struct {
	requests   atomic.Int32
	onApproval func(ctx context.Context)
}

func (*yoloHandler) AskUser(context.Context, toolhub.ClarifyRequest) (toolhub.ClarifyResponse, error) {
	return toolhub.ClarifyResponse{Answer: "?"}, nil
}

func (h *yoloHandler) RequestApproval(ctx context.Context, _ toolhub.ApprovalRequest) (toolhub.ApprovalDecision, error) {
	h.requests.Add(1)
	if h.onApproval != nil {
		h.onApproval(ctx)
	}
	return toolhub.ApprovalDecision{Approved: true}, nil
}

// blockAll is a host policy that refuses every call.
type blockAll struct{}

func (blockAll) Name() string    { return "policy:block-all" }
func (blockAll) Version() string { return "1.0.0" }
func (blockAll) Check(context.Context, string, kernel.Arguments, toolhub.Context, toolhub.Definition, []string) toolhub.Decision {
	reason := "blocked by the host"
	return toolhub.Decision{Allowed: false, Reason: &reason}
}

type yoloRig struct {
	e       *env
	a       *agent.Agent
	tool    *sendRFITool
	session kernel.Session

	mu     sync.Mutex
	events []kernel.Event
}

// newYoloRig builds an agent whose model calls send_rfi `calls` times, one call per model
// call (ids tc-1, tc-2, ...), then ends the turn. A nil handler means no interactive user.
func newYoloRig(t *testing.T, calls int, handler toolhub.InteractionHandler) *yoloRig {
	t.Helper()
	r := &yoloRig{e: newEnv(t, "unused"), tool: &sendRFITool{}}
	var step atomic.Int32
	model := aecmock.NewMockProvider("scripted", nil, aecmock.WithResponse(func([]kernel.Message) kernel.ModelResponse {
		n := int(step.Add(1))
		usage := kernel.TokenUsage{InputTokens: 10, OutputTokens: 5}
		if n <= calls {
			return kernel.ModelResponse{
				Message:    kernel.Message{Role: kernel.RoleAssistant, ToolCalls: []kernel.ToolCall{{ID: "tc-" + strconv.Itoa(n), Name: "send_rfi", Arguments: kernel.Arguments(`{"to":"a@b.c"}`)}}},
				Usage:      usage,
				StopReason: kernel.StopToolUse,
			}
		}
		return kernel.ModelResponse{Message: kernel.Message{Role: kernel.RoleAssistant, Content: "done"}, Usage: usage, StopReason: kernel.StopEndTurn}
	}))
	r.e.llm = llmhub.NewLLMHub()
	mustOK(t, r.e.llm.Register(model))
	mustOK(t, r.e.tools.Register(r.tool))
	if handler != nil {
		r.e.tools.SetInteractionHandler(handler)
	}
	r.e.bus.OnAny(func(ev kernel.Event) {
		r.mu.Lock()
		r.events = append(r.events, ev)
		r.mu.Unlock()
	})
	p := testProfile("yolo-test")
	p.Tools = []string{"send_rfi"}
	zero := 0
	p.LoopConfig = &profile.LoopConfig{MaxToolRetries: &zero, RetryBackoffMs: &zero}
	r.a = r.e.spawn(t, p)
	s, err := r.a.StartSession(t.Context())
	mustOK(t, err)
	r.session = s
	return r
}

func (r *yoloRig) ofKind(name string) []kernel.Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []kernel.Event
	for _, ev := range r.events {
		if ev.Type == name {
			out = append(out, ev)
		}
	}
	return out
}

func (r *yoloRig) mode(t *testing.T, sessionID string) kernel.ApprovalMode {
	t.Helper()
	mode, err := r.a.ApprovalMode(t.Context(), sessionID)
	mustOK(t, err)
	return mode
}

func (r *yoloRig) setMode(t *testing.T, mode kernel.ApprovalMode) kernel.Session {
	t.Helper()
	s, ok, err := r.a.SetApprovalMode(t.Context(), r.session.ID, mode)
	if err != nil || !ok {
		t.Fatalf("SetApprovalMode(%q): ok=%v err=%v", mode, ok, err)
	}
	return s
}

func (r *yoloRig) stored(t *testing.T) kernel.Session {
	t.Helper()
	s, err := r.e.hub.Sessions.Get(t.Context(), r.session.ID)
	mustOK(t, err)
	if s == nil {
		t.Fatal("the session is gone")
	}
	return *s
}

func TestYoloModeIsOffByDefault(t *testing.T) {
	h := &yoloHandler{}
	r := newYoloRig(t, 1, h)
	if m := r.mode(t, r.session.ID); m != kernel.ApprovalModeAsk {
		t.Fatalf("mode = %q", m)
	}
	chat(t, r.a, "go")
	if h.requests.Load() != 1 || r.tool.runs.Load() != 1 {
		t.Fatalf("requests = %d, runs = %d", h.requests.Load(), r.tool.runs.Load())
	}
	if n := len(r.ofKind(events.ToolAutoApproved.Name)); n != 0 {
		t.Fatalf("auto-approved = %d", n)
	}
}

func TestYoloModeRunsAConsequentialCallWithoutAskingEvenWithNoHandler(t *testing.T) {
	r := newYoloRig(t, 1, nil)
	sink := r.e.sinkOf()
	r.setMode(t, kernel.ApprovalModeYolo)
	chat(t, r.a, "go")
	if n := r.tool.runs.Load(); n != 1 {
		t.Fatalf("runs = %d", n)
	}
	if n := len(r.ofKind(events.ToolApprovalRequested.Name)); n != 0 {
		t.Fatalf("approval requests = %d", n)
	}
	auto := r.ofKind(events.ToolAutoApproved.Name)
	if len(auto) != 1 {
		t.Fatalf("auto-approved = %d", len(auto))
	}
	want := events.ToolAutoApprovedPayload{AgentID: "yolo-test", Name: "send_rfi", ToolCallID: "tc-1", Risk: "consequential", Mode: kernel.ApprovalModeYolo}
	if got, _ := auto[0].Payload.(events.ToolAutoApprovedPayload); got.AgentID != want.AgentID || got.Name != want.Name ||
		got.ToolCallID != want.ToolCallID || got.Risk != want.Risk || got.Mode != want.Mode {
		t.Fatalf("payload = %+v", auto[0].Payload)
	}
	if auto[0].SessionID != r.session.ID {
		t.Fatalf("session = %q", auto[0].SessionID)
	}
	// The dispatch span records the auto-approval.
	spans := sink.ofKind(trace.KindToolDispatch)
	if len(spans) != 1 {
		t.Fatalf("dispatch spans = %d", len(spans))
	}
	approval, _ := spans[0].Attributes.Get("approval")
	if got, err := json.Marshal(approval); err != nil || string(got) != `{"approved":true,"auto":"yolo"}` {
		t.Fatalf("approval = %s (%v)", got, err)
	}
}

func TestYoloModeEmitsTheChangeAndKeepsTheModeOnTheStoredSession(t *testing.T) {
	r := newYoloRig(t, 0, nil)
	r.setMode(t, kernel.ApprovalModeYolo)
	if m := r.stored(t).ApprovalMode; m != kernel.ApprovalModeYolo {
		t.Fatalf("stored = %q", m)
	}
	r.setMode(t, kernel.ApprovalModeAsk)
	if m := r.stored(t).ApprovalMode; m != "" {
		t.Fatalf("stored = %q, want unset", m)
	}
	var changes []events.SessionApprovalModeChangedPayload
	for _, ev := range r.ofKind(events.SessionApprovalModeChanged.Name) {
		changes = append(changes, ev.Payload.(events.SessionApprovalModeChangedPayload))
	}
	want := []events.SessionApprovalModeChangedPayload{
		{AgentID: "yolo-test", SessionID: r.session.ID, Mode: kernel.ApprovalModeYolo},
		{AgentID: "yolo-test", SessionID: r.session.ID, Mode: kernel.ApprovalModeAsk},
	}
	if !slices.Equal(changes, want) {
		t.Fatalf("changes = %+v", changes)
	}
}

func TestYoloModeSwitchedDuringATurnAppliesToItsNextCallAndSurvivesItsWrites(t *testing.T) {
	h := &yoloHandler{}
	r := newYoloRig(t, 2, h)
	// The user approves the first call and switches YOLO on while answering.
	h.onApproval = func(ctx context.Context) {
		if _, _, err := r.a.SetApprovalMode(ctx, r.session.ID, kernel.ApprovalModeYolo); err != nil {
			t.Errorf("SetApprovalMode: %v", err)
		}
	}
	chat(t, r.a, "go")
	if h.requests.Load() != 1 || r.tool.runs.Load() != 2 {
		t.Fatalf("requests = %d, runs = %d", h.requests.Load(), r.tool.runs.Load())
	}
	var ids []string
	for _, ev := range r.ofKind(events.ToolAutoApproved.Name) {
		ids = append(ids, ev.Payload.(events.ToolAutoApprovedPayload).ToolCallID)
	}
	if !slices.Equal(ids, []string{"tc-2"}) {
		t.Fatalf("auto-approved = %v", ids)
	}
	// The turn saved its session after the switch (messages, token usage) without undoing it.
	stored := r.stored(t)
	if stored.ApprovalMode != kernel.ApprovalModeYolo || stored.CurrentTokenUsage == nil {
		t.Fatalf("stored = %+v", stored)
	}
	if m := r.mode(t, r.session.ID); m != kernel.ApprovalModeYolo {
		t.Fatalf("mode = %q", m)
	}
}

func TestYoloModeDoesNotSkipTheToolPolicy(t *testing.T) {
	r := newYoloRig(t, 1, nil)
	r.e.tools.SetPolicy(blockAll{})
	r.setMode(t, kernel.ApprovalModeYolo)
	chat(t, r.a, "go")
	if n := r.tool.runs.Load(); n != 0 {
		t.Fatalf("runs = %d", n)
	}
	if n := len(r.ofKind(events.ToolAutoApproved.Name)); n != 0 {
		t.Fatalf("auto-approved = %d", n)
	}
	if n := len(r.ofKind(events.ToolDenied.Name)); n != 1 {
		t.Fatalf("denied = %d", n)
	}
}

func TestYoloModeIsNeverOnInAJobsSession(t *testing.T) {
	r := newYoloRig(t, 0, nil)
	jobSession := r.stored(t)
	jobSession.JobID = "job-1"
	mustOK(t, r.e.hub.Sessions.Update(t.Context(), jobSession))

	_, ok, err := r.a.SetApprovalMode(t.Context(), r.session.ID, kernel.ApprovalModeYolo)
	var se *kernel.SessionError
	if !ok || !errors.As(err, &se) || se.Code != kernel.SessionCodeNotAllowed {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	if !kernel.HasCode(err, kernel.SessionCodeNotAllowed) {
		t.Fatalf("HasCode(not_allowed) = false for %v", err)
	}
	// A mode stored anyway (an older row, a host write) is ignored.
	jobSession.ApprovalMode = kernel.ApprovalModeYolo
	mustOK(t, r.e.hub.Sessions.Update(t.Context(), jobSession))
	if m := r.mode(t, r.session.ID); m != kernel.ApprovalModeAsk {
		t.Fatalf("mode = %q", m)
	}
	// Switching it off is always allowed.
	if s := r.setMode(t, kernel.ApprovalModeAsk); s.ID != r.session.ID {
		t.Fatalf("session = %+v", s)
	}
}

func TestYoloModeForAnUnknownSession(t *testing.T) {
	r := newYoloRig(t, 0, nil)
	if _, ok, err := r.a.SetApprovalMode(t.Context(), "nope", kernel.ApprovalModeYolo); ok || err != nil {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	if m := r.mode(t, "nope"); m != kernel.ApprovalModeAsk {
		t.Fatalf("mode = %q", m)
	}
}
