package memorysystem

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/Phong95/aec-agent-go/components/agent/memorysystem/memory"
	"github.com/Phong95/aec-agent-go/components/events"
	"github.com/Phong95/aec-agent-go/components/llmhub"
	"github.com/Phong95/aec-agent-go/components/persistence"
	"github.com/Phong95/aec-agent-go/internal/wiretest"
	"github.com/Phong95/aec-agent-go/kernel"
	"github.com/Phong95/aec-agent-go/kernel/ordered"
)

// reply is one scripted answer of the fake model: the content with an optional stop
// reason and usage, or an error to return. It is also the shape of a "reply" in the
// recorded fixtures.
type reply struct {
	Content    string             `json:"content"`
	StopReason kernel.StopReason  `json:"stopReason"`
	Usage      *kernel.TokenUsage `json:"usage"`
	Throws     *string            `json:"throws"`
}

// fakeModel is a scripted Model. Each call returns the next reply (the last one
// repeats) and is recorded. A call is an extraction call when its first message is a
// system message that names the memory extraction engine; every call is kept, and
// the accessors are locked because the scheduler tests of later plans read them from
// the test goroutine while the actor writes them.
type fakeModel struct {
	mu      sync.Mutex
	replies []reply
	n       int
	calls   [][]kernel.Message
	opts    []llmhub.ModelOptions
	// hold, when set, is called with the extraction messages before the reply is
	// returned; a test uses it to keep a pass open.
	hold func(ctx context.Context, msgs []kernel.Message) error
}

func newFakeModel(replies ...reply) *fakeModel {
	if len(replies) == 0 {
		replies = []reply{{Content: "[]"}}
	}
	return &fakeModel{replies: replies}
}

var _ Model = (*fakeModel)(nil)

// Chat implements Model.
func (f *fakeModel) Chat(ctx context.Context, msgs []kernel.Message, opts llmhub.ModelOptions) (kernel.ModelResponse, error) {
	f.mu.Lock()
	f.calls = append(f.calls, append([]kernel.Message(nil), msgs...))
	f.opts = append(f.opts, opts)
	r := f.replies[min(f.n, len(f.replies)-1)]
	f.n++
	hold := f.hold
	f.mu.Unlock()

	if hold != nil {
		if err := hold(ctx, msgs); err != nil {
			return kernel.ModelResponse{}, err
		}
	}
	if r.Throws != nil {
		return kernel.ModelResponse{}, errors.New(*r.Throws)
	}
	stop := r.StopReason
	if stop == "" {
		stop = kernel.StopEndTurn
	}
	var usage kernel.TokenUsage
	if r.Usage != nil {
		usage = *r.Usage
	}
	return kernel.ModelResponse{
		Message:    kernel.Message{Role: kernel.RoleAssistant, Content: r.Content},
		StopReason: stop,
		Usage:      usage,
	}, nil
}

// extractionCalls returns the recorded calls that are extraction calls.
func (f *fakeModel) extractionCalls() [][]kernel.Message {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out [][]kernel.Message
	for _, c := range f.calls {
		if isExtractionCall(c) {
			out = append(out, c)
		}
	}
	return out
}

// zeroOptions reports whether every call passed the zero ModelOptions (the pipeline
// calls chat with the messages only).
func (f *fakeModel) zeroOptions() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, o := range f.opts {
		if !reflect.DeepEqual(o, llmhub.ModelOptions{}) {
			return false
		}
	}
	return true
}

// isExtractionCall recognises an extraction call by the system prompt substring,
// the way the scripted responders of the consolidation tests do.
func isExtractionCall(msgs []kernel.Message) bool {
	return len(msgs) > 0 && msgs[0].Role == kernel.RoleSystem && strings.Contains(msgs[0].Content, "memory extraction engine")
}

// fixedClock returns a clock pinned to ms (unix milliseconds).
func fixedClock(ms int64) func() time.Time {
	return func() time.Time { return time.UnixMilli(ms) }
}

// tsIDs mimics the id function the fixtures were recorded with: mem_<ms>_<n> with the
// counter restarting at 0, the clock pinned to ms.
func tsIDs(ms int64) func() string {
	n := 0
	return func() string {
		n++
		return fmt.Sprintf("mem_%d_%d", ms, n)
	}
}

// loadFixture decodes tests/testdata/golden/<name>.json into v.
func loadFixture(t *testing.T, name string, v any) {
	t.Helper()
	if err := json.Unmarshal(wiretest.Load(t, name+".json"), v); err != nil {
		t.Fatalf("decode fixture %s: %v", name, err)
	}
}

// encode is the wire encoding of v (kernel.EncodeJSON), as a string for messages.
func encode(t *testing.T, v any) string {
	t.Helper()
	b, err := kernel.EncodeJSON(v)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	return string(b)
}

// newTestDeps returns the shared wiring of a pipeline over a pinned clock.
func newTestDeps(agentID string, extra ...Option) deps {
	return deps{agentID: agentID, opts: newOptions(extra...), writeMu: &sync.Mutex{}}
}

// seedStore returns an in-memory memory store holding the records in order.
func seedStore(t *testing.T, recs ...memory.Record) *persistence.MemoryMemoryStore {
	t.Helper()
	s := persistence.NewMemoryMemoryStore()
	for _, r := range recs {
		if err := s.Save(t.Context(), r); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	return s
}

// storeRecords returns the whole store in store order.
func storeRecords(t *testing.T, s persistence.MemoryStore) []memory.Record {
	t.Helper()
	recs, err := s.Query(t.Context(), persistence.MemoryFilter{})
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	return recs
}

// ---------------------------------------------------------------------------
// System-level helpers (plan 10-03): counting store wrappers, a gated model and a
// replaceable pass, over a real dispatcher and real in-memory stores.
// ---------------------------------------------------------------------------

// countingMessages wraps a MessageStore and records the arguments of GetRange.
type countingMessages struct {
	persistence.MessageStore
	mu    sync.Mutex
	calls [][3]any
}

func (c *countingMessages) GetRange(ctx context.Context, id string, before, limit int) (persistence.MessageSlice, error) {
	c.mu.Lock()
	c.calls = append(c.calls, [3]any{id, before, limit})
	c.mu.Unlock()
	return c.MessageStore.GetRange(ctx, id, before, limit)
}

// take returns the recorded calls and forgets them.
func (c *countingMessages) take() [][3]any {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := c.calls
	c.calls = nil
	if out == nil {
		return [][3]any{}
	}
	return out
}

// countingMemory wraps a MemoryStore and records every record saved.
type countingMemory struct {
	persistence.MemoryStore
	mu    sync.Mutex
	saved []memory.Record
}

func (c *countingMemory) Save(ctx context.Context, r memory.Record) error {
	c.mu.Lock()
	c.saved = append(c.saved, r)
	c.mu.Unlock()
	return c.MemoryStore.Save(ctx, r)
}

// saves returns a copy of the records saved so far.
func (c *countingMemory) saves() []memory.Record {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]memory.Record(nil), c.saved...)
}

// gate holds the extraction calls of a fakeModel open until it is released: set
// fakeModel.hold to gate.hold.
type gate struct {
	open chan struct{}
	once sync.Once
}

func newGate() *gate { return &gate{open: make(chan struct{})} }

func (g *gate) hold(ctx context.Context, msgs []kernel.Message) error {
	if !isExtractionCall(msgs) {
		return nil
	}
	select {
	case <-g.open:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// release lets every held call (and every later one) through.
func (g *gate) release() { g.once.Do(func() { close(g.open) }) }

// logCapture is a slog destination that tests read back.
type logCapture struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *logCapture) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *logCapture) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

func (l *logCapture) logger() *slog.Logger {
	return slog.New(slog.NewTextHandler(l, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

// passFn is the signature of a consolidation pass as the scheduler calls it.
type passFn = func(ctx context.Context, sessionID string) ([]memory.ConsolidationResult, error)

// passSpy records the passes the scheduler starts: the session of each, how many run
// at once and the peak.
type passSpy struct {
	mu       sync.Mutex
	sessions []string
	active   int
	peak     int
}

// spyPasses replaces the scheduler's pass of sys with a wrapper (call it before Start),
// so a test can observe and control every consolidate call. around, when set, decides
// what the pass does and gets the real one (inner) to call; nil runs the real pass.
func spyPasses(sys *System, around func(ctx context.Context, sessionID string, inner passFn) ([]memory.ConsolidationResult, error)) *passSpy {
	spy := &passSpy{}
	inner := sys.sched.consolidate
	sys.sched.consolidate = func(ctx context.Context, sessionID string) ([]memory.ConsolidationResult, error) {
		spy.mu.Lock()
		spy.sessions = append(spy.sessions, sessionID)
		spy.active++
		spy.peak = max(spy.peak, spy.active)
		spy.mu.Unlock()
		defer func() {
			spy.mu.Lock()
			spy.active--
			spy.mu.Unlock()
		}()
		if around != nil {
			return around(ctx, sessionID, inner)
		}
		return inner(ctx, sessionID)
	}
	return spy
}

func (p *passSpy) calls() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.sessions...)
}

func (p *passSpy) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.sessions)
}

func (p *passSpy) peakActive() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.peak
}

// world is the shared surroundings of one or more agents: one persistence hub with
// counting wrappers, and one real dispatcher, as agents of one AgentCore share them.
type world struct {
	t    *testing.T
	hub  *persistence.PersistenceHub
	bus  *events.Dispatcher
	msgs *countingMessages
	mem  *countingMemory
}

func newWorld(t *testing.T) *world {
	t.Helper()
	msgs := &countingMessages{MessageStore: persistence.NewMemoryMessageStore()}
	mem := &countingMemory{MemoryStore: persistence.NewMemoryMemoryStore()}
	return &world{
		t:    t,
		hub:  persistence.NewPersistenceHub(persistence.WithMessages(msgs), persistence.WithMemory(mem)),
		bus:  events.New(),
		msgs: msgs,
		mem:  mem,
	}
}

// system returns the memory system of an agent over the shared hub and bus.
func (w *world) system(agentID string, cfg memory.Config, model Model, opts ...Option) *System {
	w.t.Helper()
	sys, err := New(Deps{AgentID: agentID, Persistence: w.hub, Events: w.bus, Model: model}, cfg, opts...)
	if err != nil {
		w.t.Fatalf("New: %v", err)
	}
	return sys
}

// session creates a session in the shared store, like Agent.StartSession.
func (w *world) session() string {
	w.t.Helper()
	s, err := w.hub.Sessions.Create(w.t.Context(), ordered.Object{})
	if err != nil {
		w.t.Fatalf("create session: %v", err)
	}
	return s.ID
}

// say appends the two messages of one chat turn (the user text and a reply) to the
// session without announcing the turn: "messages during a turn".
func (w *world) say(sessionID, text string) {
	w.t.Helper()
	for _, m := range []kernel.Message{
		{Role: kernel.RoleUser, Content: text},
		{Role: kernel.RoleAssistant, Content: "ok"},
	} {
		if err := w.hub.Messages.Append(w.t.Context(), sessionID, m); err != nil {
			w.t.Fatalf("append: %v", err)
		}
	}
}

// finish announces the end of a turn of the agent in the session (RunCompleted).
func (w *world) finish(agentID, sessionID string) {
	events.Emit(w.bus, events.RunCompleted, sessionID, events.RunCompletedPayload{AgentID: agentID})
}

// chat is a whole turn: the messages are stored, then the turn is announced, as
// Agent.Chat does.
func (w *world) chat(agentID, sessionID, text string) {
	w.t.Helper()
	w.say(sessionID, text)
	w.finish(agentID, sessionID)
}

// checkpoint is the stored consolidation checkpoint of the session (0 when unset).
func (w *world) checkpoint(sessionID string) int {
	w.t.Helper()
	s, err := w.hub.Sessions.Get(w.t.Context(), sessionID)
	if err != nil || s == nil {
		w.t.Fatalf("session %s: %v %v", sessionID, s, err)
	}
	if s.ConsolidatedMessageCount == nil {
		return 0
	}
	return *s.ConsolidatedMessageCount
}

// records are the agent's records in the store, any status.
func (w *world) records(agentID string) []memory.Record {
	w.t.Helper()
	recs, err := w.hub.Memory.Query(w.t.Context(), persistence.MemoryFilter{AgentID: agentID})
	if err != nil {
		w.t.Fatalf("query: %v", err)
	}
	return recs
}

// sharedKey is the key every ownership scenario extracts.
const sharedKey = "response_language"

// extractionOf is the model reply that extracts one response_language preference.
func extractionOf(t *testing.T, value string) reply {
	t.Helper()
	return reply{Content: encode(t, []map[string]any{{
		"kind": "user_preference", "scope": "user", "subject": "user",
		"key": sharedKey, "value": value, "confidence": "explicit",
	}})}
}

// memCfg is a memory config that is enabled, with a consolidation interval.
func memCfg(intervalMinutes float64) memory.Config {
	return memory.Config{Enabled: true, UpdateUser: true, Consolidation: &memory.ConsolidationTrigger{IntervalMinutes: &intervalMinutes}}
}

// memCfgOff is a memory config with a consolidation interval that is disabled.
func memCfgOff(intervalMinutes float64) memory.Config {
	c := memCfg(intervalMinutes)
	c.Enabled = false
	return c
}

// memCfgNoInterval is a memory config that is enabled, with an empty job.
func memCfgNoInterval() memory.Config {
	return memory.Config{Enabled: true, UpdateUser: true, Consolidation: &memory.ConsolidationTrigger{}}
}

// advance sleeps on the bubble's fake clock and lets every goroutine settle.
func advance(d time.Duration) {
	time.Sleep(d)
	synctest.Wait()
}

const minute = time.Minute

// dispose is the end of every timed test: Dispose with a generous bound, failing the
// test on an error. A bubble that returns with the actor still alive panics.
func dispose(t *testing.T, sys *System) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Hour)
	defer cancel()
	if err := sys.Dispose(ctx); err != nil {
		t.Errorf("Dispose: %v", err)
	}
}

// emitMaxIterations announces a run that stopped at its iteration limit.
func emitMaxIterations(w *world, agentID, sessionID string) {
	events.Emit(w.bus, events.MaxIterationsReached, sessionID, events.MaxIterationsPayload{AgentID: agentID, Iterations: 3})
}

// emitAborted announces a run that was aborted (RunCompleted with Aborted set).
func emitAborted(w *world, agentID, sessionID string) {
	events.Emit(w.bus, events.RunCompleted, sessionID, events.RunCompletedPayload{AgentID: agentID, Aborted: true})
}

// The scheduler introspection below exists for tests only: it reads state under the
// scheduler's own lock.

// isRunning reports whether an actor goroutine is alive.
func (s *scheduler) isRunning() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.running
}

// doneClosed reports whether the last actor has exited (its done channel is closed).
func (s *scheduler) doneClosed() bool {
	s.mu.Lock()
	done := s.done
	s.mu.Unlock()
	if done == nil {
		return false
	}
	select {
	case <-done:
		return true
	default:
		return false
	}
}

// isDirty reports whether the session is in the dirty set.
func (s *scheduler) isDirty(sessionID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.dirtySet[sessionID]
	return ok
}

// ---------------------------------------------------------------------------
// Expectations rewritten from the recorded records
//
// The goldens (tests/testdata/golden/memorysystem-*.json) are never edited. For two
// fields the pipeline writes something other than the recorded records hold, so the
// expectation is the golden rewritten by these helpers:
//
//   - sourceTrust: the goldens hold user_approved on every extracted record. The
//     pipeline derives the trust from the extracted confidence (trustFromConfidence).
//     A record that is not in the seed was written by the extraction, so its trust is
//     rewritten; a seed record keeps its trust, also when it is reinforced.
//   - supersededBy: the goldens leave it unset on a superseded record. The pipeline
//     sets it to the id of the record that replaces it: the next record of the same
//     agent, scope and key that the extraction created (the store lists records in
//     creation order).
// ---------------------------------------------------------------------------

// decodeRecords decodes a recorded record array; null and an absent field are nil.
func decodeRecords(t *testing.T, raw json.RawMessage) []memory.Record {
	t.Helper()
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var recs []memory.Record
	if err := json.Unmarshal(raw, &recs); err != nil {
		t.Fatalf("decode the TS records: %v", err)
	}
	return recs
}

// goReturnedRecords is the Go expectation of records returned by an extraction: Q24
// only (a returned record is a snapshot taken when it was written, before any later
// supersede).
func goReturnedRecords(t *testing.T, raw json.RawMessage, seed []memory.Record) string {
	t.Helper()
	if len(raw) == 0 || string(raw) == "null" {
		return string(raw)
	}
	recs := decodeRecords(t, raw)
	if recs == nil {
		recs = []memory.Record{}
	}
	seeded := seedIDs(seed)
	for i := range recs {
		if !seeded[recs[i].ID] {
			recs[i].SourceTrust = trustFromConfidence(recs[i].Confidence)
		}
	}
	return encode(t, recs)
}

// goStoreRecords is the Go expectation of a store after a pass: Q24 and Q23.
func goStoreRecords(t *testing.T, recs []memory.Record, seed []memory.Record) []memory.Record {
	t.Helper()
	recs = append([]memory.Record{}, recs...)
	seeded := map[string]memory.Record{}
	for _, r := range seed {
		seeded[r.ID] = r
	}
	created := func(r memory.Record) bool { _, ok := seeded[r.ID]; return !ok }
	for i := range recs {
		if created(recs[i]) {
			recs[i].SourceTrust = trustFromConfidence(recs[i].Confidence)
		}
	}
	for i, r := range recs {
		if r.Status != memory.StatusSuperseded || r.SupersededBy != "" {
			continue
		}
		if before, ok := seeded[r.ID]; ok && before.Status == memory.StatusSuperseded {
			continue // superseded before the pass: not touched by it
		}
		from := 0
		if created(r) {
			from = i + 1
		}
		for _, next := range recs[from:] {
			if created(next) && next.ID != r.ID && next.AgentID == r.AgentID && next.Scope == r.Scope && next.Key == r.Key {
				recs[i].SupersededBy = next.ID
				break
			}
		}
	}
	return recs
}

// goStoreJSON is goStoreRecords over a recorded JSON array, encoded for comparison.
func goStoreJSON(t *testing.T, raw json.RawMessage, seed []memory.Record) string {
	t.Helper()
	if len(raw) == 0 || string(raw) == "null" {
		return string(raw)
	}
	recs := goStoreRecords(t, decodeRecords(t, raw), seed)
	if recs == nil {
		recs = []memory.Record{}
	}
	return encode(t, recs)
}

func seedIDs(seed []memory.Record) map[string]bool {
	out := map[string]bool{}
	for _, r := range seed {
		out[r.ID] = true
	}
	return out
}
