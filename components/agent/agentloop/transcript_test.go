package agentloop_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/synctest"

	"github.com/Phong95/aec-agent-go/aecmock"
	"github.com/Phong95/aec-agent-go/components/agent/agentloop"
	"github.com/Phong95/aec-agent-go/components/agent/memorysystem/memory"
	"github.com/Phong95/aec-agent-go/components/agent/profile"
	"github.com/Phong95/aec-agent-go/components/events"
	"github.com/Phong95/aec-agent-go/components/llmhub"
	"github.com/Phong95/aec-agent-go/components/persistence"
	"github.com/Phong95/aec-agent-go/components/sandboxhub"
	"github.com/Phong95/aec-agent-go/components/toolhub"
	"github.com/Phong95/aec-agent-go/components/trace"
	"github.com/Phong95/aec-agent-go/internal/jsonx"
	"github.com/Phong95/aec-agent-go/internal/wiretest"
	"github.com/Phong95/aec-agent-go/kernel"
	"github.com/Phong95/aec-agent-go/kernel/ordered"
)

// The transcript goldens are recorded by tools/wiregen/agentloop (see harness.mjs).
// Each fixture holds the scenario and the normalised recorded transcript.
// TestTranscripts runs the scenario on the loop over the aecmock fakes and requires the
// same transcript, byte for byte: events with payload bytes, spans with name, kind,
// parent name, status and attributes in key order, persisted messages without
// timestamps, and results.

// agentloopTranscripts are the fixtures replayed by TestTranscripts, by file name
// without the extension. Later plans add their scenarios here.
var agentloopTranscripts = []string{
	"agentloop-run-end-turn",
	"agentloop-run-max-tokens-cap",
	"agentloop-run-error-stop-cap",
	"agentloop-run-two-turns",
	"agentloop-run-tool-call",
	"agentloop-run-retry-is-error",
	"agentloop-run-unknown-tool",
	"agentloop-run-throwing-tool",
	"agentloop-run-invalid-arguments",
	"agentloop-run-skill-view",
	"agentloop-run-approval-approved",
	"agentloop-run-approval-denied",
	"agentloop-run-approval-no-handler",
	"agentloop-run-approval-sandbox",
	"agentloop-run-policy-denied",
	"agentloop-stream-end-turn",
	"agentloop-stream-tool-call",
	"agentloop-stream-error-chunk",
	"agentloop-stream-max-iterations",
	"agentloop-stream-tool-call-progress",
	"agentloop-stream-invalid-arguments",
	"agentloop-stream-unknown-tool",
	"agentloop-stream-abort-mid-stream",
	"agentloop-stream-already-aborted",
	"agentloop-stream-abort-during-tool",
	"agentloop-run-abort-during-model",
	"agentloop-run-compaction",
	"agentloop-stream-compaction",
	"agentloop-run-second-compaction",
	"agentloop-run-memory-recall",
	"agentloop-stream-memory-recall",
	"agentloop-run-effective-options",
}

// agentloopOtherFixtures are agentloop- fixtures consumed by a test other than
// TestTranscripts (a corpus replayed by its own test). TestAgentloopFixturesCovered
// requires every fixture to be named in one list or the other.
var agentloopOtherFixtures = []string{
	"agentloop-counter",    // TestCounterMatchesGolden
	"agentloop-compactor",  // TestCompactorMatchesGolden
	"agentloop-tool-pairs", // TestRepairToolPairsMatchesGolden
}

// tScenario is the scenario of a fixture (the shape harness.mjs documents).
type tScenario struct {
	AgentID      string              `json:"agentId"`
	SystemPrompt string              `json:"systemPrompt"`
	SessionID    string              `json:"sessionId"`
	LoopConfig   *profile.LoopConfig `json:"loopConfig"`
	Compactor    json.RawMessage     `json:"compactor"`
	// Summary is what the summary calls of the compactor are answered with (09-04).
	Summary *string `json:"summary"`
	// Memory scripts the memory system: each recall returns the next result (09-04).
	Memory *tMemory `json:"memory"`
	// ModelOptions scripts what the tracing hooks of the model resolve to (09-04).
	ModelOptions *tModelOptions `json:"modelOptions"`
	Tools        []tTool        `json:"tools"`
	SkillTools   []tTool        `json:"skillTools"`
	Interaction  *tInteraction  `json:"interaction"`
	Sandbox      *tSandbox      `json:"sandbox"`
	Policy       *tPolicy       `json:"policy"`
	Runs         []tRun         `json:"runs"`
}

// tMemory scripts the memory system of a scenario.
type tMemory struct {
	Results []memory.RecalledMemories `json:"results"`
}

// tModelOptions is what the stub model resolves for tracing: Defaults are merged
// under the options of a call (resolveOptions), Provider names the provider.
type tModelOptions struct {
	Defaults llmhub.ModelOptions `json:"defaults"`
	Provider string              `json:"provider"`
}

// merge returns o with every option it leaves unset taken from the defaults, the
// way the real ModelManager merges its defaults under a call's options.
func (m tModelOptions) merge(o llmhub.ModelOptions) llmhub.ModelOptions {
	d := m.Defaults
	if o.Provider == "" {
		o.Provider = d.Provider
	}
	if o.Model == "" {
		o.Model = d.Model
	}
	if o.MaxTokens == nil {
		o.MaxTokens = d.MaxTokens
	}
	if o.Temperature == nil {
		o.Temperature = d.Temperature
	}
	if o.TopP == nil {
		o.TopP = d.TopP
	}
	if o.StopSequences == nil {
		o.StopSequences = d.StopSequences
	}
	if o.SystemPrompt == "" {
		o.SystemPrompt = d.SystemPrompt
	}
	return o
}

// tTool is a scripted tool: each call returns the next output, the last repeats.
type tTool struct {
	Definition toolhub.Definition `json:"definition"`
	Outputs    []tOutput          `json:"outputs"`
}

// tOutput is one scripted result of a tool: content with optional flags, or a throw.
type tOutput struct {
	Content   string  `json:"content"`
	IsError   *bool   `json:"isError"`
	Retryable *bool   `json:"retryable"`
	Throws    *string `json:"throws"`
}

// tInteraction scripts the host's interaction handler: one decision per approval
// request, the last one repeats.
type tInteraction struct {
	Decisions []tDecision `json:"decisions"`
}

type tDecision struct {
	Approved bool    `json:"approved"`
	Reason   *string `json:"reason"`
	Throws   *string `json:"throws"`
}

// tSandbox is the sandbox the agent works in.
type tSandbox struct {
	ID       string          `json:"id"`
	Kind     sandboxhub.Kind `json:"kind"`
	Isolated bool            `json:"isolated"`
}

// tPolicy is a tool policy that denies every call with the reason.
type tPolicy struct {
	Deny string `json:"deny"`
}

type tRun struct {
	Mode        string  `json:"mode"`
	UserMessage string  `json:"userMessage"`
	Turns       []tTurn `json:"turns"`
	// Abort stops the run at a named point (09-03); see tAbort.
	Abort *tAbort `json:"abort"`
	// Overrides are the model options passed to Run or Stream (09-04).
	Overrides *llmhub.ModelOptions `json:"overrides"`
}

// tAbort says where a scenario stops its run, through the ctx:
//
//	"before"      cancelled before the call
//	"chunk"       cancelled by the consumer after it received chunk number Chunk (1-based)
//	"model-call"  cancelled inside the model call, which then does not answer until released
//	"tool"        cancelled inside the tool named Tool, which then does not return until released
type tAbort struct {
	At    string `json:"at"`
	Chunk int    `json:"chunk"`
	Tool  string `json:"tool"`
}

type tTurn struct {
	Content    string               `json:"content"`
	ToolCalls  []kernel.ToolCall    `json:"toolCalls"`
	StopReason kernel.StopReason    `json:"stopReason"`
	Usage      *kernel.TokenUsage   `json:"usage"`
	Chunks     []kernel.StreamChunk `json:"chunks"`
	Error      string               `json:"error"`
}

// tTranscript is the normalised transcript, in the key order harness.mjs writes.
type tTranscript struct {
	Results  []json.RawMessage `json:"results"`
	Events   []tEvent          `json:"events"`
	Spans    []tSpan           `json:"spans"`
	Messages []tMessage        `json:"messages"`
	// ApprovalRequests are the requests the interaction handler received; present
	// only when the scenario has a handler.
	ApprovalRequests []json.RawMessage `json:"approvalRequests,omitempty"`
	// SummaryRequests are the summary calls the compactor made; present only when
	// the scenario sets a summary (09-04).
	SummaryRequests []tSummaryRequest `json:"summaryRequests,omitempty"`
}

// tSummaryRequest is one summary call: the single message and the options.
type tSummaryRequest struct {
	Messages []tMessage         `json:"messages"`
	Options  tSummaryRequestOpt `json:"options"`
}

// tSummaryRequestOpt is the options of a summary call, in the recorded key order.
type tSummaryRequestOpt struct {
	SystemPrompt string `json:"systemPrompt"`
	MaxTokens    *int   `json:"maxTokens,omitempty"`
	Model        string `json:"model,omitempty"`
}

type tEvent struct {
	Type      string          `json:"type"`
	SessionID string          `json:"sessionId"`
	Payload   json.RawMessage `json:"payload"`
}

type tSpan struct {
	Phase      string         `json:"phase"`
	Name       string         `json:"name"`
	Kind       string         `json:"kind"`
	Parent     string         `json:"parent"`
	Status     string         `json:"status,omitempty"`
	Attributes ordered.Object `json:"attributes"`
}

// tMessage is a message without its timestamp.
type tMessage struct {
	Role        kernel.Role         `json:"role"`
	Content     string              `json:"content"`
	ToolCalls   []kernel.ToolCall   `json:"toolCalls,omitzero"`
	ToolResults []kernel.ToolResult `json:"toolResults,omitzero"`
}

func normMessage(m kernel.Message) tMessage {
	return tMessage{Role: m.Role, Content: m.Content, ToolCalls: m.ToolCalls, ToolResults: m.ToolResults}
}

// tResult is what one run returned.
type tResult struct {
	Message    tMessage          `json:"message"`
	Usage      kernel.TokenUsage `json:"usage"`
	Iterations int               `json:"iterations"`
	Aborted    bool              `json:"aborted,omitempty"`
}

// tStreamResult is what one stream run yielded: every chunk, in order.
type tStreamResult struct {
	Chunks []kernel.StreamChunk `json:"chunks"`
}

// tThrows is the result of a run that returned an error.
type tThrows struct {
	Throws string `json:"throws"`
}

// fixtureFile is one fixture: the scenario and the recorded transcript (raw bytes).
type fixtureFile struct {
	Scenario   json.RawMessage `json:"scenario"`
	Transcript json.RawMessage `json:"transcript"`
}

// spanRecorder is a trace sink that keeps a copy of every span, with the name of
// the parent resolved at start time.
type spanRecorder struct {
	mu     sync.Mutex
	nameBy map[string]string
	spans  []tSpan
}

func newSpanRecorder() *spanRecorder { return &spanRecorder{nameBy: map[string]string{}} }

func (s *spanRecorder) record(phase string, sp trace.Span) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if phase == "start" {
		s.nameBy[sp.SpanID] = sp.Name
	}
	rec := tSpan{
		Phase:      phase,
		Name:       sp.Name,
		Kind:       string(sp.Kind),
		Parent:     s.nameBy[sp.ParentSpanID],
		Attributes: sp.Attributes.Clone(),
	}
	if phase == "end" {
		rec.Status = string(sp.Status)
	}
	s.spans = append(s.spans, rec)
}

func (s *spanRecorder) OnSpanStart(sp trace.Span) { s.record("start", sp) }
func (s *spanRecorder) OnSpanEnd(sp trace.Span)   { s.record("end", sp) }
func (*spanRecorder) Flush(context.Context) error { return nil }
func (s *spanRecorder) all() []tSpan              { s.mu.Lock(); defer s.mu.Unlock(); return slices.Clone(s.spans) }

// replay runs a scenario on the loop and returns its normalised transcript.
func replay(ctx context.Context, t *testing.T, sc tScenario) tTranscript {
	t.Helper()

	prof := testProfile(sc.AgentID)
	prof.SystemPrompt = sc.SystemPrompt
	prof.LoopConfig = sc.LoopConfig
	if len(sc.Compactor) > 0 {
		if err := json.Unmarshal(sc.Compactor, &prof.Compactor); err != nil {
			t.Fatalf("decode the compactor override: %v", err)
		}
	}

	var turns []aecmock.ScriptedTurn
	for _, run := range sc.Runs {
		for _, turn := range run.Turns {
			st := aecmock.ScriptedTurn{
				Content:    turn.Content,
				ToolCalls:  turn.ToolCalls,
				StopReason: turn.StopReason,
				Usage:      turn.Usage,
				Chunks:     turn.Chunks,
			}
			if turn.Error != "" {
				st.Err = &scriptedError{turn.Error}
			}
			turns = append(turns, st)
		}
	}

	scripted := aecmock.NewScriptedModels(turns...)
	if sc.Summary != nil {
		scripted.With(aecmock.WithSummaryReply(*sc.Summary))
	}
	if mo := sc.ModelOptions; mo != nil {
		scripted.With(aecmock.WithOptionResolver(mo.merge))
		if mo.Provider != "" {
			scripted.With(aecmock.WithProviderName(mo.Provider))
		}
	}
	ctl := newAbortCtl()
	toolHub := toolhub.NewToolHub()
	var toolNames []string
	// RISK-UNSET (divergence): the recording runs a tool that declares no risk
	// without approval; the loop gates it like a consequential one (fail closed). A
	// scenario tool without a risk and without a sandbox operation is meant as a "no
	// approval needed" tool, so it is registered here as read-only, which says the
	// same thing. The model still sees the recorded definition (riskless), so the
	// tool definitions and their token estimates in the transcript are the recorded
	// ones. A sandbox tool keeps the unset risk: its sandbox gates it either way, with
	// risk "consequential" in the approval request.
	declared := map[string]bool{}
	for _, st := range sc.Tools {
		if st.Definition.Risk == "" && st.Definition.SandboxOperation == "" {
			st.Definition = st.Definition.Clone()
			st.Definition.Risk = toolhub.RiskReadOnly
			declared[st.Definition.Name] = true
		}
		if err := toolHub.Register(newScriptedTool(st, ctl)); err != nil {
			t.Fatalf("register tool %s: %v", st.Definition.Name, err)
		}
		toolNames = append(toolNames, st.Definition.Name)
	}
	if sc.Policy != nil {
		toolHub.SetPolicy(denyPolicy{reason: sc.Policy.Deny})
	}
	var skills agentloop.Skills
	if len(sc.SkillTools) > 0 {
		var handlers []toolhub.Handler
		for _, st := range sc.SkillTools {
			handlers = append(handlers, newScriptedTool(st, ctl))
		}
		skills = aecmock.NewSkillTools(handlers...)
	}

	// The session starts under the id the scenario's runs use (IN-05: the loop refuses
	// a sessionID that is not the active session's id).
	hub := persistence.NewPersistenceHub(persistence.WithSessions(persistence.NewMemorySessionStore(
		persistence.WithSessionIDs(func() string { return sc.SessionID }))))
	conv := aecmock.NewConversation(hub, aecmock.WithConversationClock(func() string { return "2026-01-01T00:00:00.000Z" }))
	if _, err := conv.StartSession(ctx, ordered.Object{}); err != nil {
		t.Fatalf("start session: %v", err)
	}

	bus := events.New()
	var evs []tEvent
	var evMu sync.Mutex
	for _, name := range events.KindNames() {
		bus.On(name, func(e kernel.Event) {
			raw, err := kernel.EncodeJSON(e.Payload)
			if err != nil {
				t.Errorf("encode %s payload: %v", e.Type, err)
				return
			}
			evMu.Lock()
			defer evMu.Unlock()
			evs = append(evs, tEvent{Type: e.Type, SessionID: e.SessionID, Payload: raw})
		})
	}

	rec := newSpanRecorder()
	svc := trace.NewService()
	svc.AddSink(rec)

	deps := agentloop.Deps{
		Models:       &abortingModels{ScriptedModels: scripted, ctl: ctl},
		Tools:        tsRiskTools{Tools: aecmock.NewHubTools(toolHub, toolNames), declared: declared},
		Skills:       skills,
		Context:      aecmock.NewPromptContext(prof),
		Conversation: conv,
		Events:       bus,
		Profile:      func() profile.AgentProfile { return prof },
		Trace:        svc,
	}
	if sc.Memory != nil {
		deps.Memory = aecmock.NewScriptedMemory(sc.Memory.Results...)
	}
	var interaction *scriptedInteraction
	if sc.Interaction != nil {
		interaction = &scriptedInteraction{decisions: sc.Interaction.Decisions}
		deps.Interaction = func(string) toolhub.InteractionHandler { return interaction }
	}
	if sc.Sandbox != nil {
		deps.Sandbox = newTestSandboxes(ctx, t, sc)
	}
	loop, err := agentloop.New(deps)
	if err != nil {
		t.Fatalf("agentloop.New: %v", err)
	}

	out := tTranscript{Results: []json.RawMessage{}}
	for _, run := range sc.Runs {
		var overrides llmhub.ModelOptions
		if run.Overrides != nil {
			overrides = *run.Overrides
		}
		runCtx, cancel := context.WithCancel(ctx)
		ctl.begin(run.Abort, cancel)
		if run.Abort != nil && run.Abort.At == "before" {
			cancel()
		}
		var raw []byte
		var err error
		switch run.Mode {
		case "run":
			var res agentloop.RunResult
			res, err = loop.Run(runCtx, run.UserMessage, sc.SessionID, overrides, agentloop.RunOptions{})
			if err == nil {
				raw, err = kernel.EncodeJSON(tResult{Message: normMessage(res.Message), Usage: res.Usage, Iterations: res.Iterations, Aborted: res.Aborted})
			}
		case "stream":
			chunks := []kernel.StreamChunk{}
			var serr error
			for chunk, cerr := range loop.Stream(runCtx, run.UserMessage, sc.SessionID, overrides, agentloop.RunOptions{}) {
				if cerr != nil {
					serr = cerr
					break
				}
				chunks = append(chunks, chunk)
				if run.Abort != nil && run.Abort.At == "chunk" && len(chunks) == run.Abort.Chunk {
					cancel()
				}
			}
			err = serr
			if err == nil {
				raw, err = kernel.EncodeJSON(tStreamResult{Chunks: chunks})
			}
		default:
			t.Fatalf("unsupported run mode %q", run.Mode)
		}
		cancel()
		if err != nil && raw == nil {
			raw, err = kernel.EncodeJSON(tThrows{Throws: err.Error()})
		}
		if err != nil {
			t.Fatalf("encode result: %v", err)
		}
		out.Results = append(out.Results, raw)
	}
	// Whatever a scenario left blocked (a model call or a tool that does not answer)
	// is released, and the abandoned work given time to finish: it must end silently,
	// and a bubble may not end with a live goroutine.
	ctl.releaseAll()
	synctest.Wait()

	evMu.Lock()
	out.Events = append([]tEvent{}, evs...)
	evMu.Unlock()
	out.Spans = append([]tSpan{}, rec.all()...)
	out.Messages = []tMessage{}
	for _, m := range conv.Messages() {
		out.Messages = append(out.Messages, normMessage(m))
	}
	if interaction != nil {
		out.ApprovalRequests = interaction.encoded(t)
	}
	if sc.Summary != nil {
		out.SummaryRequests = []tSummaryRequest{}
		for _, c := range scripted.Calls() {
			if c.Options.SystemPrompt == "" {
				continue // a main model call
			}
			req := tSummaryRequest{Options: tSummaryRequestOpt{SystemPrompt: c.Options.SystemPrompt, MaxTokens: c.Options.MaxTokens, Model: c.Options.Model}}
			for _, m := range c.Messages {
				req.Messages = append(req.Messages, normMessage(m))
			}
			out.SummaryRequests = append(out.SummaryRequests, req)
		}
	}
	return out
}

// tsRiskTools lists the tools the way the recorded scenario declared them: the risk
// the replay declared for a riskless tool (see RISK-UNSET in replay) is cleared again
// in List, so the model and the trace see the recorded definitions. Authorize is the hub's,
// with the declared risk, which is what the approval gate reads.
type tsRiskTools struct {
	agentloop.Tools
	declared map[string]bool
}

func (t tsRiskTools) List() []toolhub.Definition {
	defs := t.Tools.List()
	for i := range defs {
		if t.declared[defs[i].Name] {
			defs[i] = defs[i].Clone()
			defs[i].Risk = ""
		}
	}
	return defs
}

// scriptedTool is a toolhub.Handler that answers from a script: each call returns the
// next output and the last one repeats; a throws output returns an error with that
// text (a hard failure, not an error result).
type scriptedTool struct {
	def  toolhub.Definition
	mu   sync.Mutex
	outs []tOutput
	next int
	ctl  *abortCtl
}

func newScriptedTool(st tTool, ctl *abortCtl) *scriptedTool {
	return &scriptedTool{def: st.Definition, outs: st.Outputs, ctl: ctl}
}

func (s *scriptedTool) Name() string                   { return "tool:" + s.def.Name }
func (*scriptedTool) Version() string                  { return "1.0.0" }
func (s *scriptedTool) Definition() toolhub.Definition { return s.def.Clone() }

func (s *scriptedTool) Execute(context.Context, kernel.Arguments, toolhub.Context) (toolhub.Output, error) {
	// A scenario that stops the run from inside this tool: cancel, then do not return
	// until the replay releases it (the tool ignores ctx, like a real one may).
	s.ctl.fire("tool", s.def.Name)
	s.mu.Lock()
	o := s.outs[min(s.next, len(s.outs)-1)]
	s.next++
	s.mu.Unlock()
	if o.Throws != nil {
		return toolhub.Output{}, errors.New(*o.Throws)
	}
	return toolhub.Output{Content: o.Content, IsError: o.IsError, Retryable: o.Retryable}, nil
}

// abortCtl lets a scenario stop its own run from inside the model call or a tool, as
// the recording harness does from its scripted stubs. The run in flight registers its
// abort point and cancel func with begin; fire cancels it when the caller is the named
// point and then blocks until releaseAll, so the blocked work looks like a model or a
// tool that never answers.
type abortCtl struct {
	mu      sync.Mutex
	abort   *tAbort
	cancel  context.CancelFunc
	release chan struct{}
}

func newAbortCtl() *abortCtl { return &abortCtl{release: make(chan struct{})} }

func (c *abortCtl) begin(a *tAbort, cancel context.CancelFunc) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.abort, c.cancel = a, cancel
}

// fire cancels the run and blocks until released when the current run aborts at
// point at (and, for a tool, in the tool named name).
func (c *abortCtl) fire(at, name string) {
	c.mu.Lock()
	a, cancel := c.abort, c.cancel
	c.mu.Unlock()
	if a == nil || a.At != at || (at == "tool" && a.Tool != name) {
		return
	}
	cancel()
	<-c.release
}

func (c *abortCtl) releaseAll() { close(c.release) }

// abortingModels is the scripted model of a scenario, plus the abort-inside-the-
// model-call point. It embeds the concrete ScriptedModels so the tracing-only
// interfaces it implements stay visible to the loop.
type abortingModels struct {
	*aecmock.ScriptedModels
	ctl *abortCtl
}

func (m *abortingModels) Chat(ctx context.Context, msgs []kernel.Message, opts llmhub.ModelOptions) (kernel.ModelResponse, error) {
	m.ctl.fire("model-call", "")
	return m.ScriptedModels.Chat(ctx, msgs, opts)
}

// scriptedInteraction is the host's interaction handler of a scenario: it records
// every approval request it receives (with the session id replaced, as the
// recording harness does) and answers with the next scripted decision.
type scriptedInteraction struct {
	decisions []tDecision
	mu        sync.Mutex
	requests  []toolhub.ApprovalRequest
}

func (*scriptedInteraction) AskUser(context.Context, toolhub.ClarifyRequest) (toolhub.ClarifyResponse, error) {
	return toolhub.ClarifyResponse{}, nil
}

func (s *scriptedInteraction) RequestApproval(_ context.Context, req toolhub.ApprovalRequest) (toolhub.ApprovalDecision, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	req.SessionID = "<session>"
	s.requests = append(s.requests, req)
	d := s.decisions[min(len(s.requests)-1, len(s.decisions)-1)]
	if d.Throws != nil {
		return toolhub.ApprovalDecision{}, errors.New(*d.Throws)
	}
	return toolhub.ApprovalDecision{Approved: d.Approved, Reason: d.Reason}, nil
}

// encoded returns the recorded requests as wire bytes.
func (s *scriptedInteraction) encoded(t *testing.T) []json.RawMessage {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]json.RawMessage, 0, len(s.requests))
	for _, req := range s.requests {
		raw, err := kernel.EncodeJSON(req)
		if err != nil {
			t.Fatalf("encode an approval request: %v", err)
		}
		out = append(out, raw)
	}
	return out
}

// denyPolicy is a tool policy that denies every call.
type denyPolicy struct{ reason string }

func (denyPolicy) Name() string    { return "policy:test" }
func (denyPolicy) Version() string { return "1.0.0" }
func (p denyPolicy) Check(context.Context, string, kernel.Arguments, toolhub.Context, toolhub.Definition, []string) toolhub.Decision {
	return toolhub.Deny(p.reason)
}

// testSandboxes is the agent's sandbox access of a scenario: an in-memory sandbox
// of the scenario's kind, and the real default sandbox approval policy.
type testSandboxes struct {
	view sandboxhub.View
	hub  *sandboxhub.SandboxHub
}

func newTestSandboxes(ctx context.Context, t *testing.T, sc tScenario) *testSandboxes {
	t.Helper()
	prov := aecmock.NewInMemoryProvider(aecmock.WithKind(sc.Sandbox.Kind))
	env, err := prov.Create(ctx, sandboxhub.Spec{Kind: sc.Sandbox.Kind}, sandboxhub.CreateContext{ID: sc.Sandbox.ID, Tenancy: sandboxhub.TenancyOwn})
	if err != nil {
		t.Fatalf("create the scenario sandbox: %v", err)
	}
	view, err := env.ForAgent(ctx, sc.AgentID)
	if err != nil {
		t.Fatalf("open the scenario sandbox: %v", err)
	}
	if view.Traits().Isolated != sc.Sandbox.Isolated {
		t.Fatalf("the in-memory %s sandbox is isolated=%v, the scenario says %v", sc.Sandbox.Kind, view.Traits().Isolated, sc.Sandbox.Isolated)
	}
	return &testSandboxes{view: view, hub: sandboxhub.NewSandboxHub()}
}

func (s *testSandboxes) GetSandbox(context.Context) (sandboxhub.View, error) { return s.view, nil }

func (s *testSandboxes) RequiresApproval(traits sandboxhub.Traits, op sandboxhub.Operation) bool {
	return s.hub.RequiresApproval(traits, op)
}

// scriptedError is a scripted model failure.
type scriptedError struct{ msg string }

func (e *scriptedError) Error() string { return e.msg }

func TestTranscripts(t *testing.T) {
	for _, name := range agentloopTranscripts {
		t.Run(name, func(t *testing.T) {
			raw := wiretest.Load(t, name+".json")
			var fx fixtureFile
			if err := json.Unmarshal(raw, &fx); err != nil {
				t.Fatalf("decode fixture: %v", err)
			}
			var sc tScenario
			if err := jsonx.UnmarshalStrict(fx.Scenario, &sc); err != nil {
				t.Fatalf("decode the scenario: %v", err)
			}
			var want tTranscript
			if err := jsonx.UnmarshalStrict(fx.Transcript, &want); err != nil {
				t.Fatalf("decode the TS transcript: %v", err)
			}
			// The decoder must be lossless, or the comparison below proves nothing.
			if again, err := kernel.EncodeJSON(want); err != nil || !bytes.Equal(again, fx.Transcript) {
				t.Fatalf("the TS transcript does not survive a decode and encode\n got: %s\nwant: %s (%v)", again, fx.Transcript, err)
			}

			// The recorded transcript becomes the expectation: where the loop differs
			// from the recording, the named divergences rewrite exactly the parts that
			// behaviour is about, and every other byte is still the recorded one
			// (transcript_divergences_test.go).
			applyGoDivergences(t, name, &want)

			// One bubble per scenario: persist timeouts and any backoff run on fake time.
			synctest.Test(t, func(t *testing.T) {
				got := replay(t.Context(), t, sc)
				gotBytes, err := kernel.EncodeJSON(got)
				if err != nil {
					t.Fatal(err)
				}
				wantBytes, err := kernel.EncodeJSON(want)
				if err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(gotBytes, wantBytes) {
					t.Fatalf("Go transcript differs from the expected transcript (%s)\n got: %s\nwant: %s", firstDifference(t, got, want), gotBytes, wantBytes)
				}
			})
		})
	}
}

// TestAgentloopFixturesCovered fails when a fixture with the agentloop- prefix is
// replayed by no test, so a case added to wiregen cannot be forgotten here.
func TestAgentloopFixturesCovered(t *testing.T) {
	files, err := filepath.Glob(filepath.Join(wiretest.Dir(t), "agentloop-*.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatal("no agentloop- fixture found: is the working directory inside the module?")
	}
	for _, f := range files {
		name := strings.TrimSuffix(filepath.Base(f), ".json")
		if !slices.Contains(agentloopTranscripts, name) && !slices.Contains(agentloopOtherFixtures, name) {
			t.Errorf("fixture %s is replayed by no test: add it to agentloopTranscripts or agentloopOtherFixtures", name)
		}
	}
	for _, name := range slices.Concat(agentloopTranscripts, agentloopOtherFixtures) {
		if _, err := os.Stat(filepath.Join(wiretest.Dir(t), name+".json")); err != nil {
			t.Errorf("listed fixture %s is missing: %v", name, err)
		}
	}
}
