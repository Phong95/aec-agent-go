package agentloop_test

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/Phong95/aec-agent-go/aecmock"
	"github.com/Phong95/aec-agent-go/components/agent/agentloop"
	"github.com/Phong95/aec-agent-go/components/agent/profile"
	"github.com/Phong95/aec-agent-go/components/events"
	"github.com/Phong95/aec-agent-go/components/llmhub"
	"github.com/Phong95/aec-agent-go/components/persistence"
	"github.com/Phong95/aec-agent-go/components/toolhub"
	"github.com/Phong95/aec-agent-go/components/trace"
	"github.com/Phong95/aec-agent-go/kernel"
	"github.com/Phong95/aec-agent-go/kernel/ordered"
)

// defaultCompactor is the conformance default compactor, which is
// profile.DefaultCompactorConfig. A zero CompactorConfig has threshold 0, which
// means compaction disabled, and no test here means that.
func defaultCompactor() profile.CompactorConfig {
	return profile.DefaultCompactorConfig()
}

// testProfile is a profile with the given id and the default compactor.
func testProfile(id string) profile.AgentProfile {
	return profile.AgentProfile{
		ID:           id,
		Name:         id,
		SystemPrompt: "You are a test agent.",
		Compactor:    defaultCompactor(),
	}
}

func intPtr(v int) *int { return &v }

// spanRec is one span notification a recording sink saw.
type spanRec struct {
	phase string // "start" or "end"
	span  trace.Span
}

// recSink records every span notification in order. It is a pointer type, so it
// is comparable and the service can remove it.
type recSink struct {
	mu   sync.Mutex
	recs []spanRec
}

func (s *recSink) OnSpanStart(sp trace.Span) { s.add("start", sp) }
func (s *recSink) OnSpanEnd(sp trace.Span)   { s.add("end", sp) }
func (*recSink) Flush(context.Context) error { return nil }

func (s *recSink) add(phase string, sp trace.Span) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.recs = append(s.recs, spanRec{phase: phase, span: sp})
}

func (s *recSink) all() []spanRec {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]spanRec(nil), s.recs...)
}

// endNames returns the span names in the order they ended.
func (s *recSink) endNames() []string {
	var out []string
	for _, r := range s.all() {
		if r.phase == "end" {
			out = append(out, r.span.Name)
		}
	}
	return out
}

// endAttrsAt returns the end attributes of the n-th (0-based) span with the name.
func (s *recSink) endAttrsAt(name string, n int) (ordered.Object, bool) {
	for _, r := range s.all() {
		if r.phase == "end" && r.span.Name == name {
			if n == 0 {
				return r.span.Attributes, true
			}
			n--
		}
	}
	return ordered.Object{}, false
}

// parentName returns the name of the parent of the named span ("" for the root).
func (s *recSink) parentName(name string) string {
	recs := s.all()
	byID := map[string]string{}
	for _, r := range recs {
		if r.phase == "start" {
			byID[r.span.SpanID] = r.span.Name
		}
	}
	for _, r := range recs {
		if r.phase == "start" && r.span.Name == name {
			return byID[r.span.ParentSpanID]
		}
	}
	return ""
}

// endAttrs returns the end attributes of the first span with the name.
func (s *recSink) endAttrs(name string) (ordered.Object, bool) {
	for _, r := range s.all() {
		if r.phase == "end" && r.span.Name == name {
			return r.span.Attributes, true
		}
	}
	return ordered.Object{}, false
}

// startedAndEnded reports how many spans started and how many ended.
func (s *recSink) startedAndEnded() (started, ended int) {
	for _, r := range s.all() {
		if r.phase == "start" {
			started++
		} else {
			ended++
		}
	}
	return started, ended
}

// eventLog records every SDK event of a bus, in order.
type eventLog struct {
	mu     sync.Mutex
	events []kernel.Event
}

func recordEvents(bus events.Bus) *eventLog {
	log := &eventLog{}
	for _, name := range events.KindNames() {
		bus.On(name, func(e kernel.Event) {
			log.mu.Lock()
			defer log.mu.Unlock()
			log.events = append(log.events, e)
		})
	}
	return log
}

func (l *eventLog) all() []kernel.Event {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]kernel.Event(nil), l.events...)
}

func (l *eventLog) types() []string {
	var out []string
	for _, e := range l.all() {
		out = append(out, e.Type)
	}
	return out
}

// payload returns the wire bytes of the payload of the first event of the type.
func (l *eventLog) payload(t *testing.T, eventType string) string {
	t.Helper()
	for _, e := range l.all() {
		if e.Type == eventType {
			b, err := kernel.EncodeJSON(e.Payload)
			if err != nil {
				t.Fatalf("encode %s payload: %v", eventType, err)
			}
			return string(b)
		}
	}
	t.Fatalf("no %s event among %v", eventType, l.types())
	return ""
}

// rig is a loop wired over the aecmock fakes, with everything a test inspects.
type rig struct {
	loop   *agentloop.Loop
	models *aecmock.ScriptedModels
	conv   *aecmock.Conversation
	hub    *persistence.PersistenceHub
	tools  *toolhub.ToolHub
	bus    *events.Dispatcher
	log    *eventLog
	sink   *recSink
	deps   agentloop.Deps
}

// rigOptions tunes newRig.
type rigOptions struct {
	prof     *profile.AgentProfile
	profFn   func() profile.AgentProfile // a live profile; wins over prof
	noTrace  bool
	noStart  bool // do not start a session
	loopOpts []agentloop.Option

	// Tool wiring (09-02). handlers are registered with the tool hub and are the
	// agent's whole tool list.
	handlers []toolhub.Handler
	policy   toolhub.Policy
	// context replaces the passthrough context (the one that builds the system
	// message, for example).
	context agentloop.Context
	// memory is the agent's memory system; nil means none.
	memory      agentloop.Memory
	skills      agentloop.Skills
	interaction toolhub.InteractionHandler
	sandboxes   agentloop.Sandboxes
	// wrapTools decorates the tools port the rig built over the hub.
	wrapTools func(agentloop.Tools) agentloop.Tools
}

// newRig builds a loop over a scripted model, an in-memory conversation (session
// started), an empty tool hub, the passthrough context, a dispatcher and a trace
// service with a recording sink.
func newRig(t *testing.T, o rigOptions, turns ...aecmock.ScriptedTurn) *rig {
	t.Helper()
	// The session starts under the id the tests pass to Run and Stream ("s-1"): the loop
	// refuses a sessionID that is not the active session's id (IN-05).
	hub := persistence.NewPersistenceHub(persistence.WithSessions(persistence.NewMemorySessionStore(
		persistence.WithSessionIDs(func() string { return "s-1" }))))
	conv := aecmock.NewConversation(hub, aecmock.WithConversationClock(func() string { return "2026-01-01T00:00:00.000Z" }))
	if !o.noStart {
		if _, err := conv.StartSession(t.Context(), ordered.Object{}); err != nil {
			t.Fatalf("start session: %v", err)
		}
	}
	models := aecmock.NewScriptedModels(turns...)
	bus := events.New()
	prof := testProfile("test-agent")
	if o.prof != nil {
		prof = *o.prof
	}
	sink := &recSink{}
	svc := trace.NewService()
	svc.AddSink(sink)

	toolHub := toolhub.NewToolHub()
	var names []string
	for _, h := range o.handlers {
		if err := toolHub.Register(h); err != nil {
			t.Fatalf("register a tool: %v", err)
		}
		names = append(names, h.Definition().Name)
	}
	if o.policy != nil {
		toolHub.SetPolicy(o.policy)
	}
	deps := agentloop.Deps{
		Models:       models,
		Tools:        aecmock.NewHubTools(toolHub, names),
		Skills:       o.skills,
		Context:      aecmock.PassthroughContext{},
		Conversation: conv,
		Events:       bus,
		Profile:      func() profile.AgentProfile { return prof },
		Sandbox:      o.sandboxes,
	}
	if o.wrapTools != nil {
		deps.Tools = o.wrapTools(deps.Tools)
	}
	if o.profFn != nil {
		deps.Profile = o.profFn
	}
	if o.context != nil {
		deps.Context = o.context
	}
	if o.memory != nil {
		deps.Memory = o.memory
	}
	if o.interaction != nil {
		deps.Interaction = func(string) toolhub.InteractionHandler { return o.interaction }
	}
	if !o.noTrace {
		deps.Trace = svc
	}
	loop, err := agentloop.New(deps, o.loopOpts...)
	if err != nil {
		t.Fatalf("agentloop.New: %v", err)
	}
	return &rig{loop: loop, models: models, conv: conv, hub: hub, tools: toolHub, bus: bus, log: recordEvents(bus), sink: sink, deps: deps}
}

func (r *rig) run(t *testing.T, userMessage string) (agentloop.RunResult, error) {
	t.Helper()
	return r.loop.Run(t.Context(), userMessage, "s-1", llmhub.ModelOptions{}, agentloop.RunOptions{})
}

// fakeTool is a toolhub.Handler whose behaviour a test supplies. It counts its
// executions.
type fakeTool struct {
	def   toolhub.Definition
	run   func(ctx context.Context, args kernel.Arguments, tc toolhub.Context) (toolhub.Output, error)
	calls atomic.Int32
}

// newFakeTool builds a tool with the given name and behaviour; the schema accepts
// any object. It is read-only: a tool without a Risk asks for approval like a
// consequential one (fail closed), so a test that means the unset Risk clears it.
func newFakeTool(name string, run func(ctx context.Context, args kernel.Arguments, tc toolhub.Context) (toolhub.Output, error)) *fakeTool {
	return &fakeTool{
		def: toolhub.Definition{Name: name, Description: "fake tool " + name, Parameters: toolhub.JSONSchema{Type: "object"}, Risk: toolhub.RiskReadOnly},
		run: run,
	}
}

// constTool is a fake tool that always returns the same output.
func constTool(name string, out toolhub.Output) *fakeTool {
	return newFakeTool(name, func(context.Context, kernel.Arguments, toolhub.Context) (toolhub.Output, error) { return out, nil })
}

func (f *fakeTool) Name() string                   { return "tool:" + f.def.Name }
func (*fakeTool) Version() string                  { return "1.0.0" }
func (f *fakeTool) Definition() toolhub.Definition { return f.def.Clone() }

func (f *fakeTool) Execute(ctx context.Context, args kernel.Arguments, tc toolhub.Context) (toolhub.Output, error) {
	f.calls.Add(1)
	return f.run(ctx, args, tc)
}

// executions is how often the tool ran.
func (f *fakeTool) executions() int { return int(f.calls.Load()) }

// toolTurn is a scripted model turn that calls tools.
func toolTurn(calls ...kernel.ToolCall) aecmock.ScriptedTurn {
	return aecmock.ScriptedTurn{ToolCalls: calls}
}

// callOf builds a tool call with the given JSON arguments.
func callOf(id, name, args string) kernel.ToolCall {
	return kernel.ToolCall{ID: id, Name: name, Arguments: kernel.Arguments(args)}
}

func boolRef(v bool) *bool { return &v }

func strRef(s string) *string { return &s }

func mustEncode(t *testing.T, v any) string {
	t.Helper()
	b, err := kernel.EncodeJSON(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
