package e2e_test

import (
	"context"
	"encoding/json"
	"iter"
	"sync"
	"sync/atomic"
	"testing"

	aec "github.com/Phong95/aec-agent-go"
	"github.com/Phong95/aec-agent-go/aecmock"
	"github.com/Phong95/aec-agent-go/components/agent"
	"github.com/Phong95/aec-agent-go/components/agent/agentloop"
	"github.com/Phong95/aec-agent-go/components/agent/memorysystem/memory"
	"github.com/Phong95/aec-agent-go/components/agent/profile"
	"github.com/Phong95/aec-agent-go/components/events"
	"github.com/Phong95/aec-agent-go/components/toolhub"
	"github.com/Phong95/aec-agent-go/components/trace"
	"github.com/Phong95/aec-agent-go/kernel"
)

// This file is the shared setup of the five test files that drive the loop through the
// AgentCore facade (run-abort, agent-loop-config, agent-context-trace and
// the chatStream halves of tool-call-invalid-arguments and tool-call-streaming): a real
// aec.AgentCore with an aecmock.ScriptedProvider registered, one spawned agent with an
// active session, a recording trace sink and an event recorder on the core's bus.
//
// Every test disposes the core at its end. The test calls dispose itself, before the body of a synctest bubble returns: a t.Cleanup would run
// after the bubble has ended.

// loopAgentID is the id of the agent every facade test spawns (a fixed id, not a random
// UUID).
const loopAgentID = "loop-agent"

// loopMemoryOff is the memory config of run-abort and agent-loop-config: memory disabled,
// and the consolidation interval 0, so recall and consolidation stay inactive.
func loopMemoryOff() memory.Config {
	zero := 0.0
	return memory.Config{Consolidation: &memory.ConsolidationTrigger{IntervalMinutes: &zero}}
}

// loopMemoryOn is the memory config of agent-context-trace: memory enabled and the
// interval 0, so consolidation never runs and the memories are seeded directly.
func loopMemoryOn() memory.Config {
	zero := 0.0
	return memory.Config{Enabled: true, UpdateUser: true, Consolidation: &memory.ConsolidationTrigger{IntervalMinutes: &zero}}
}

// loopOptions tunes newLoopCore. The zero value is the setup of run-abort: a plain
// agent, memory disabled, no tools.
type loopOptions struct {
	// profile changes the test profile before the agent is spawned.
	profile func(*profile.AgentProfile)
	// tools are registered with UseTool and are the agent's whole tool list.
	tools []toolhub.Handler
	// interaction becomes the tool hub's interaction handler.
	interaction toolhub.InteractionHandler
	// seed is stored in the core's memory store before the agent is spawned.
	seed []memory.Record
}

// loopCore is an AgentCore with one agent, its active session and the recorders a facade
// test reads.
type loopCore struct {
	core     *aec.AgentCore
	agent    *agent.Agent
	provider *aecmock.ScriptedProvider
	session  kernel.Session
	spans    *spanLog
	log      *eventLog
}

// newLoopCore builds an AgentCore over a ScriptedProvider that serves turns, registers
// the tools and the interaction handler, seeds the memory store, spawns the agent with
// the test profile and starts its session. Call dispose before the test (or the
// synctest bubble) ends.
func newLoopCore(t *testing.T, o loopOptions, turns []aecmock.ScriptedTurn, providerOpts ...aecmock.ScriptedProviderOption) *loopCore {
	t.Helper()
	ctx := t.Context()
	core, err := aec.NewAgentCore()
	if err != nil {
		t.Fatalf("NewAgentCore: %v", err)
	}
	provider := aecmock.NewScriptedProvider("scripted", turns, providerOpts...)
	if err := core.UseLLMProvider(provider); err != nil {
		t.Fatalf("UseLLMProvider: %v", err)
	}
	var names []string
	for _, h := range o.tools {
		if err := core.UseTool(h); err != nil {
			t.Fatalf("UseTool: %v", err)
		}
		names = append(names, h.Definition().Name)
	}
	if o.interaction != nil {
		core.ToolHub().SetInteractionHandler(o.interaction)
	}
	spans := &spanLog{}
	core.Trace().AddSink(spans)
	log := recordEvents(core.Events())

	for _, rec := range o.seed {
		if rec.AgentID == "" {
			rec.AgentID = loopAgentID
		}
		if err := core.PersistenceHub().Memory.Save(ctx, rec); err != nil {
			t.Fatalf("seed memory %s: %v", rec.ID, err)
		}
	}

	p := profile.AgentProfile{
		ID:           loopAgentID,
		Name:         "loop-test",
		SystemPrompt: "You are a helpful assistant.",
		Tools:        names,
		Skills:       []string{},
		Compactor:    testCompactor(),
		Memory:       loopMemoryOff(),
	}
	if o.profile != nil {
		o.profile(&p)
	}
	a, err := core.SpawnAgent(ctx, p)
	if err != nil {
		t.Fatalf("SpawnAgent: %v", err)
	}
	session, err := a.StartSession(ctx)
	if err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	return &loopCore{core: core, agent: a, provider: provider, session: session, spans: spans, log: log}
}

// dispose disposes the core at the end of a test and fails the test when
// that fails. Nothing may be left running afterwards, or a synctest bubble deadlocks and
// goleak reports it.
func (l *loopCore) dispose(t *testing.T) {
	t.Helper()
	if err := l.core.Dispose(context.WithoutCancel(t.Context())); err != nil {
		t.Errorf("Dispose: %v", err)
	}
}

// chat is Agent.Chat with no model options and no run options.
func (l *loopCore) chat(ctx context.Context, message string) (agentloop.RunResult, error) {
	return l.agent.Chat(ctx, message)
}

// stream is Agent.ChatStream with no model options and no run options.
func (l *loopCore) stream(ctx context.Context, message string) iter.Seq2[kernel.StreamChunk, error] {
	return l.agent.ChatStream(ctx, message)
}

// stored is the session's stored messages, read under a live ctx.
func (l *loopCore) stored(t *testing.T) []kernel.Message {
	t.Helper()
	msgs, err := l.core.PersistenceHub().Messages.Get(t.Context(), l.session.ID)
	if err != nil {
		t.Fatalf("Messages.Get: %v", err)
	}
	return msgs
}

// drainStream collects every chunk of a stream and returns the first error of its
// error slot.
func drainStream(seq iter.Seq2[kernel.StreamChunk, error]) ([]kernel.StreamChunk, error) {
	return drainStreamEach(seq, func(kernel.StreamChunk) {})
}

// drainStreamEach collects a stream like drainStream and calls each after every chunk the
// consumer received, which is where a test stops the run from the consumer side.
func drainStreamEach(seq iter.Seq2[kernel.StreamChunk, error], each func(kernel.StreamChunk)) ([]kernel.StreamChunk, error) {
	var chunks []kernel.StreamChunk
	for c, err := range seq {
		if err != nil {
			// Agent.ChatStream yields a model failure as its error chunk with the error.
			if c.Type == kernel.ChunkError {
				chunks = append(chunks, c)
			}
			return chunks, err
		}
		chunks = append(chunks, c)
		each(c)
	}
	return chunks, nil
}

// textChunk is a text chunk.
func textChunk(s string) kernel.StreamChunk {
	return kernel.StreamChunk{Type: kernel.ChunkText, Content: &s}
}

// toolCallOf is a tool call with the given raw JSON arguments.
func toolCallOf(id, name, args string) kernel.ToolCall {
	return kernel.ToolCall{ID: id, Name: name, Arguments: kernel.Arguments(args)}
}

// toolTurn is a scripted model turn that calls tools.
func toolTurn(calls ...kernel.ToolCall) aecmock.ScriptedTurn {
	return aecmock.ScriptedTurn{ToolCalls: calls}
}

// chunkTypesOf lists the kinds of the chunks, in order.
func chunkTypesOf(chunks []kernel.StreamChunk) []kernel.ChunkType {
	out := make([]kernel.ChunkType, len(chunks))
	for i, c := range chunks {
		out[i] = c.Type
	}
	return out
}

// requireAbortedDoneChunk requires the stream to end with exactly one done chunk marked
// aborted, and returns it.
func requireAbortedDoneChunk(t *testing.T, chunks []kernel.StreamChunk) kernel.StreamChunk {
	t.Helper()
	if len(chunks) == 0 {
		t.Fatal("the stream yielded nothing, want a done chunk marked aborted")
	}
	last := chunks[len(chunks)-1]
	if last.Type != kernel.ChunkDone || !last.Aborted {
		t.Fatalf("last chunk = %+v, want a done chunk marked aborted", last)
	}
	dones := 0
	for _, c := range chunks {
		if c.Type == kernel.ChunkDone {
			dones++
		}
	}
	if dones != 1 {
		t.Fatalf("%d done chunks, want exactly one", dones)
	}
	return last
}

// loopTool is a toolhub.Handler whose behaviour a test supplies. It counts its executions
// (a spy on Execute).
type loopTool struct {
	def   toolhub.Definition
	run   func(ctx context.Context, args kernel.Arguments, tc toolhub.Context) (toolhub.Output, error)
	calls atomic.Int32
}

// newLoopTool builds a tool named name; its schema accepts any object (type "object"
// with no properties).
func newLoopTool(name string, run func(ctx context.Context, args kernel.Arguments, tc toolhub.Context) (toolhub.Output, error)) *loopTool {
	return &loopTool{
		def: toolhub.Definition{Name: name, Description: "Test tool: " + name, Parameters: toolhub.JSONSchema{Type: "object"}, Risk: toolhub.RiskReadOnly},
		run: run,
	}
}

func (l *loopTool) Name() string                   { return "tool:" + l.def.Name }
func (*loopTool) Version() string                  { return "1.0.0" }
func (l *loopTool) Definition() toolhub.Definition { return l.def.Clone() }

func (l *loopTool) Execute(ctx context.Context, args kernel.Arguments, tc toolhub.Context) (toolhub.Output, error) {
	l.calls.Add(1)
	return l.run(ctx, args, tc)
}

// executions is how often the tool ran.
func (l *loopTool) executions() int { return int(l.calls.Load()) }

// loopApproval is an interaction handler whose approval answer a test supplies. AskUser
// answers with an empty string (a stub).
type loopApproval func(ctx context.Context, req toolhub.ApprovalRequest) (toolhub.ApprovalDecision, error)

func (loopApproval) AskUser(context.Context, toolhub.ClarifyRequest) (toolhub.ClarifyResponse, error) {
	return toolhub.ClarifyResponse{}, nil
}

func (f loopApproval) RequestApproval(ctx context.Context, req toolhub.ApprovalRequest) (toolhub.ApprovalDecision, error) {
	return f(ctx, req)
}

// spanLog is a trace sink that records every span notification, in order.
type spanLog struct {
	mu   sync.Mutex
	recs []spanRecord
}

// spanRecord is one span notification: phase is "start" or "end".
type spanRecord struct {
	phase string
	span  trace.Span
}

func (s *spanLog) OnSpanStart(sp trace.Span) { s.add("start", sp) }
func (s *spanLog) OnSpanEnd(sp trace.Span)   { s.add("end", sp) }
func (*spanLog) Flush(context.Context) error { return nil }

func (s *spanLog) add(phase string, sp trace.Span) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.recs = append(s.recs, spanRecord{phase: phase, span: sp})
}

func (s *spanLog) all() []spanRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]spanRecord(nil), s.recs...)
}

// startedAndEnded reports how many spans started and how many ended.
func (s *spanLog) startedAndEnded() (started, ended int) {
	for _, r := range s.all() {
		if r.phase == "start" {
			started++
		} else {
			ended++
		}
	}
	return started, ended
}

// ended lists the spans of the given name, as they were when they ended, in order.
func (s *spanLog) ended(name string) []trace.Span {
	var out []trace.Span
	for _, r := range s.all() {
		if r.phase == "end" && r.span.Name == name {
			out = append(out, r.span)
		}
	}
	return out
}

// attrsJSON decodes the attributes of a span to plain data, so numbers compare as
// float64 whatever Go type the loop stored them in.
func attrsJSON(t *testing.T, sp trace.Span) map[string]any {
	t.Helper()
	raw, err := kernel.EncodeJSON(sp.Attributes)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode %s: %v", raw, err)
	}
	return out
}

// eventLog records every SDK event of a bus, in order.
type eventLog struct {
	mu     sync.Mutex
	events []kernel.Event
}

// recordEvents subscribes to every SDK event kind of the bus.
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

// count is how many events of the type were emitted.
func (l *eventLog) count(eventType string) int {
	n := 0
	for _, e := range l.all() {
		if e.Type == eventType {
			n++
		}
	}
	return n
}

// types lists the event types, in order.
func (l *eventLog) types() []string {
	var out []string
	for _, e := range l.all() {
		out = append(out, e.Type)
	}
	return out
}
