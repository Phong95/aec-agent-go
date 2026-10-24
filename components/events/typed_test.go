package events_test

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/Phong95/aec-agent-go/components/events"
	"github.com/Phong95/aec-agent-go/components/grouphub/group"
	"github.com/Phong95/aec-agent-go/internal/wiretest"
	"github.com/Phong95/aec-agent-go/kernel"
)

func intPtr(n int) *int       { return &n }
func strPtr(s string) *string { return &s }
func boolPtr(b bool) *bool    { return &b }

// sample is one of the 22 events with the exact payload wiregen emits for it.
// The generic constructor keeps the payload type and its Kind together.
type sample struct {
	name string // Go identifier of the kind, for failure messages
	kind string // event type string
	// emit emits the sample payload through b.
	emit func(b events.Bus) kernel.Event
	// reencode decodes e's payload into the kind's struct with PayloadAs and
	// encodes the struct back to JSON.
	reencode func(e kernel.Event) ([]byte, error)
	// deliver subscribes with the typed Subscribe, emits the sample and
	// returns whether the typed handler saw an equal struct.
	deliver func(t *testing.T) bool
}

func mk[P any](name string, k events.Kind[P], p P) sample {
	return sample{
		name: name,
		kind: k.Name,
		emit: func(b events.Bus) kernel.Event { return events.Emit(b, k, "s1", p) },
		reencode: func(e kernel.Event) ([]byte, error) {
			got, err := events.PayloadAs[P](e)
			if err != nil {
				return nil, err
			}
			return kernel.EncodeJSON(got)
		},
		deliver: func(t *testing.T) bool {
			t.Helper()
			d := events.New()
			var received []P
			events.Subscribe(d, k, func(_ kernel.Event, got P) { received = append(received, got) })
			events.Emit(d, k, "s1", p)
			return len(received) == 1 && reflect.DeepEqual(received[0], p)
		},
	}
}

const agentID = "agent-1"

var (
	sampleArgs = kernel.Arguments(`{"zeta":1,"alpha":"<tag>","mid":{"b":2,"a":1}}`)
	skillArgs  = kernel.Arguments(`{"path":"a.ts","deep":true}`)
)

// lifecycleSamples are the four lifecycle kinds added after the events-all-22 fixture
// was recorded, in canonical order (A-100: followed by the five channel kinds). They
// are not in that fixture. (Update (A-70): followed by the four trigger kinds.)
// (Update (A-126): the four job kinds.)
// (Update (A-105): followed by the four subagent kinds.)
// (Update (A-101): followed by the four secret kinds.)
// (Update (A-135): followed by session:approval-mode.)
// (Update (A-136): followed by session:updated and command:executed.)
func lifecycleSamples() []sample {
	return []sample{
		mk("AgentSpawned", events.AgentSpawned, events.AgentSpawnedPayload{AgentID: agentID}),
		mk("AgentRemoved", events.AgentRemoved, events.AgentRemovedPayload{AgentID: agentID}),
		mk("AgentProfileChanged", events.AgentProfileChanged, events.AgentProfileChangedPayload{
			AgentID: agentID,
			Profile: json.RawMessage(`{"id":"agent-1","name":"agent","tools":[],"skills":["plan"]}`),
		}),
		mk("SkillDeleted", events.SkillDeleted, events.SkillDeletedPayload{Name: "my-skill"}),
		// A-100: the channel kinds.
		mk("ChannelStatusChanged", events.ChannelStatusChanged, events.ChannelStatusChangedPayload{
			AgentID: agentID, ChannelID: "c1", Platform: "telegram", Status: "retrying", Error: "network down",
		}),
		mk("ChannelMessageReceived", events.ChannelMessageReceived, events.ChannelMessageReceivedPayload{
			AgentID: agentID, ChannelID: "c1", Platform: "telegram", ChatID: "5", SenderID: "5", MessageID: "1",
		}),
		mk("ChannelMessageSent", events.ChannelMessageSent, events.ChannelMessageSentPayload{
			AgentID: agentID, ChannelID: "c1", Platform: "telegram", ChatID: "5", Parts: 2,
		}),
		mk("ChannelPairingRequested", events.ChannelPairingRequested, events.ChannelPairingRequestedPayload{
			AgentID: agentID, ChannelID: "c1", Platform: "telegram", Code: "ABCD2345", SenderID: "5", SenderName: "Ann", ExpiresAt: "2026-10-07T01:00:00.000Z",
		}),
		mk("ChannelFailed", events.ChannelFailed, events.ChannelFailedPayload{
			AgentID: agentID, ChannelID: "c1", Platform: "telegram", ChatID: "5", Error: "boom",
		}),
		// A-70: the trigger kinds. Update (A-126): the job kinds.
		mk("JobEventReceived", events.JobEventReceived, events.JobEventReceivedPayload{
			AgentID: agentID, JobID: "t1", EventID: "e1", Type: "build.failed", Status: "accepted", RunID: "r1",
		}),
		mk("JobRunStarted", events.JobRunStarted, events.JobRunStartedPayload{
			AgentID: agentID, JobID: "t1", JobName: "ci", RunID: "r1", Cause: "webhook", Attempt: 1,
		}),
		mk("JobRunRetrying", events.JobRunRetrying, events.JobRunRetryingPayload{
			AgentID: agentID, JobID: "t1", JobName: "ci", RunID: "r1", Attempt: 1, Error: "timeout", RetryAt: "2026-10-10T00:00:30.000Z",
		}),
		mk("JobRunFinished", events.JobRunFinished, events.JobRunFinishedPayload{
			AgentID: agentID, JobID: "t1", RunID: "r1", Cause: "schedule", Status: "succeeded", JobName: "watch", Output: "[[SILENT]]", Silent: true,
		}),
		// A-105: the subagent kinds.
		mk("SubagentSpawned", events.SubagentSpawned, events.SubagentSpawnedPayload{
			AgentID: agentID, RunID: "sr1", ParentRunID: "run-1", ParentToolCallID: "tc-1", ChildAgentID: "researcher", ChildSessionID: "s-child", Access: "read-only",
		}),
		mk("SubagentCompleted", events.SubagentCompleted, events.SubagentFinishedPayload{
			AgentID: agentID, RunID: "sr1", ParentRunID: "run-1", ChildAgentID: "researcher", ChildSessionID: "s-child", Status: "partial",
			Usage: kernel.TokenUsage{InputTokens: 10, OutputTokens: 5}, ToolCalls: 2, DurationMs: 1500, Error: "turn limit",
		}),
		mk("SubagentFailed", events.SubagentFailed, events.SubagentFinishedPayload{
			AgentID: agentID, RunID: "sr2", ParentRunID: "run-1", ChildAgentID: "researcher", Status: "budget_exhausted", Error: "budget",
		}),
		mk("SubagentCancelled", events.SubagentCancelled, events.SubagentFinishedPayload{
			AgentID: agentID, RunID: "sr3", ParentRunID: "run-1", ChildAgentID: "researcher", ChildSessionID: "s-child", Status: "cancelled",
		}),
		mk("SubagentTypeDeleted", events.SubagentTypeDeleted, events.SubagentTypeDeletedPayload{Name: "drafter"}),
		// A-101: the secret kinds.
		mk("SecretCreated", events.SecretCreated, events.SecretChangedPayload{Name: "WEATHER_KEY", OwnerAgentID: agentID}),
		mk("SecretUpdated", events.SecretUpdated, events.SecretChangedPayload{Name: "WEATHER_KEY", ValueChanged: boolPtr(true)}),
		mk("SecretDeleted", events.SecretDeleted, events.SecretChangedPayload{Name: "WEATHER_KEY"}),
		mk("SecretUsed", events.SecretUsed, events.SecretUsedPayload{Name: "WEATHER_KEY", OwnerAgentID: agentID, AgentID: agentID, Tool: "http_request", Host: "api.example.com"}),
		// A-135: a session's approval mode.
		mk("SessionApprovalModeChanged", events.SessionApprovalModeChanged, events.SessionApprovalModeChangedPayload{AgentID: agentID, SessionID: "s-1", Mode: kernel.ApprovalModeYolo}),
		// A-136: a session's settings changed, and a slash command ran.
		mk("SessionUpdated", events.SessionUpdated, events.SessionUpdatedPayload{AgentID: agentID, SessionID: "s-1", Fields: []string{"title"}}),
		// A-153: a session's runner.
		mk("SandboxRunnerChanged", events.SandboxRunnerChanged, events.SandboxRunnerChangedPayload{AgentID: agentID, SessionID: "s-1", Runner: &events.RunnerInfo{
			AgentID: agentID, SessionID: "s-1", SandboxID: "remote:runner:s-1", FolderName: "my-app", OS: "windows", AllowExec: true, Connected: true, ConnectedAt: 1,
		}}),
		mk("CommandExecuted", events.CommandExecuted, events.CommandExecutedPayload{AgentID: agentID, SessionID: "s-1", Name: "rename", Client: "web", OK: false, Error: "invalid_arguments"}),
		// A-106: the group kinds.
		mk("GroupChanged", events.GroupChanged, events.GroupChangedPayload{GroupID: "g1", Change: group.ChangeUpdated}),
		mk("GroupMessageAdded", events.GroupMessageAdded, events.GroupMessagePayload{GroupID: "g1", Message: group.Message{
			ID: "m1", SessionID: "gs1", GroupID: "g1", Seq: 1, Kind: group.KindUser, Text: "hi", To: []string{},
			CreatedAt: "2026-10-21T00:00:00.000Z", UpdatedAt: "2026-10-21T00:00:00.000Z",
		}}),
		mk("GroupMessageUpdated", events.GroupMessageUpdated, events.GroupMessagePayload{GroupID: "g1", Message: group.Message{
			ID: "m2", SessionID: "gs1", GroupID: "g1", Seq: 2, Kind: group.KindAgent, Text: "done", AgentID: "spec", RoundID: "r1",
			Status: group.StatusPartial, Tools: []group.ToolStep{{ID: "t1", Name: "read_spec", Status: group.ToolDone, ResultPreview: "ok"}},
			Usage: &kernel.TokenUsage{InputTokens: 10, OutputTokens: 5}, Error: "the reply hit the output token limit",
			CreatedAt: "2026-10-21T00:00:00.000Z", UpdatedAt: "2026-10-21T00:00:01.000Z",
		}}),
		mk("GroupMessageDelta", events.GroupMessageDelta, events.GroupMessageDeltaPayload{GroupID: "g1", MessageID: "m2", AgentID: "spec", Text: "do"}),
		mk("GroupPresence", events.GroupPresence, events.GroupPresencePayload{GroupID: "g1", AgentID: "spec", State: group.PresenceTool, Tool: "read_spec", RoundID: "r1"}),
		mk("GroupTurnStarted", events.GroupTurnStarted, events.GroupTurnStartedPayload{GroupID: "g1", TurnID: "t1", Plan: group.PlanRound}),
		mk("GroupTurnFinished", events.GroupTurnFinished, events.GroupTurnFinishedPayload{GroupID: "g1", TurnID: "t1", Status: group.TurnCompleted, Usage: kernel.TokenUsage{InputTokens: 20, OutputTokens: 10}}),
	}
}

// allSamples is every SDK kind: the recorded 22 followed by the lifecycle kinds.
// JobStarted (A-69) is newer than the events-all-22 recording, so it is not one of the
// recorded 22; it is inserted before JobCompleted, its place in the TS kind order.
// Update (A-126): JobStarted is gone (prompt jobs emit job:*), and the last two recorded
// kinds (code jobs) were removed with scheduled tasks: 20 recorded kinds remain.
// Update (A-104): agent-loop:context-usage is not in the recorded fixture either; it is
// inserted after ModelResponded, its place in the TS kind order.
// Update (A-135): agent-loop:tool-auto-approved is inserted after ToolApprovalResolved,
// its place in the TS kind order.
func allSamples() []sample {
	recorded := samples()
	i := slices.IndexFunc(recorded, func(s sample) bool { return s.kind == events.ModelResponded.Name })
	recorded = slices.Insert(recorded, i+1, contextUsageSample())
	i = slices.IndexFunc(recorded, func(s sample) bool { return s.kind == events.ToolApprovalResolved.Name })
	recorded = slices.Insert(recorded, i+1, toolAutoApprovedSample())
	return append(recorded, lifecycleSamples()...)
}

// toolAutoApprovedSample is a sample of the ToolAutoApproved kind (A-135).
func toolAutoApprovedSample() sample {
	return mk("ToolAutoApproved", events.ToolAutoApproved, events.ToolAutoApprovedPayload{
		AgentID: agentID, Name: "write_file", ToolCallID: "call_2", Risk: "consequential", Mode: kernel.ApprovalModeYolo,
		Reason: "writes a host folder", Sandbox: &events.ApprovalSandboxPayload{ID: "sb1", Kind: "host", Isolated: false},
	})
}

// contextUsageSample is a sample of the ContextUsage kind (A-104).
func contextUsageSample() sample {
	return mk("ContextUsage", events.ContextUsage, events.ContextUsagePayload{
		AgentID: agentID, SessionID: "s-1", Model: "m", ContextWindowTokens: 1000, CompactAtTokens: 800,
		UsedTokens: 46, UsedSource: "reported", FreeTokens: 954, Iteration: 1,
		Categories: []events.ContextCategory{{Key: "system_prompt", Tokens: 10}, {Key: "messages", Tokens: 36}},
		Tools:      []events.ContextToolTokens{{Name: "read_file", Tokens: 7}},
	})
}

// samples mirrors the events-all-22 case of tools/wiregen, in canonical order.
func samples() []sample {
	return []sample{
		mk("UserMessage", events.UserMessage, events.UserMessagePayload{AgentID: agentID, Role: "user", Content: "Hello <world> & more"}),
		mk("Iteration", events.Iteration, events.IterationPayload{AgentID: agentID, Iteration: 1, MaxIterations: 25}),
		mk("ModelRequested", events.ModelRequested, events.ModelRequestedPayload{AgentID: agentID, Iteration: 1, ToolCount: 3}),
		mk("ModelResponded", events.ModelResponded, events.ModelRespondedPayload{
			AgentID: agentID, StopReason: "tool_use", HasToolCalls: true,
			Usage: &kernel.TokenUsage{InputTokens: 12, OutputTokens: 34, CacheReadTokens: intPtr(5)},
		}),
		mk("ToolDispatched", events.ToolDispatched, events.DispatchStartPayload{AgentID: agentID, Name: "read_file", Arguments: sampleArgs}),
		mk("ToolCompleted", events.ToolCompleted, events.DispatchEndPayload{AgentID: agentID, Name: "read_file", IsError: false, Attempt: 1}),
		mk("ToolRetry", events.ToolRetry, events.DispatchRetryPayload{AgentID: agentID, Name: "read_file", Attempt: 2, Reason: "timeout"}),
		mk("ToolFailed", events.ToolFailed, events.DispatchFailedPayload{AgentID: agentID, Name: "read_file", Error: "boom"}),
		mk("ToolDenied", events.ToolDenied, events.ToolDeniedPayload{AgentID: agentID, Name: "write_file", Reason: strPtr("denied by policy")}),
		mk("ToolApprovalRequested", events.ToolApprovalRequested, events.ToolApprovalRequestedPayload{
			AgentID: agentID, Name: "write_file", ToolCallID: "call_1", Risk: "high", Arguments: sampleArgs,
		}),
		mk("ToolApprovalResolved", events.ToolApprovalResolved, events.ToolApprovalResolvedPayload{
			AgentID: agentID, Name: "write_file", ToolCallID: "call_1", Approved: true, WaitedMs: 1500,
		}),
		mk("SkillDispatched", events.SkillDispatched, events.DispatchStartPayload{AgentID: agentID, Name: "review", Arguments: skillArgs}),
		mk("SkillCompleted", events.SkillCompleted, events.DispatchEndPayload{AgentID: agentID, Name: "review", IsError: true, Attempt: 2}),
		mk("SkillRetry", events.SkillRetry, events.DispatchRetryPayload{AgentID: agentID, Name: "review", Attempt: 2, Reason: "rate limited"}),
		mk("SkillFailed", events.SkillFailed, events.DispatchFailedPayload{AgentID: agentID, Name: "review", Error: "failed"}),
		mk("RunCompleted", events.RunCompleted, events.RunCompletedPayload{
			AgentID: agentID, Iterations: 3, Aborted: true,
			TotalUsage: kernel.TokenUsage{InputTokens: 100, OutputTokens: 20, CacheReadTokens: intPtr(0), CacheWriteTokens: intPtr(0)},
		}),
		mk("MaxIterationsReached", events.MaxIterationsReached, events.MaxIterationsPayload{
			AgentID: agentID, Iterations: 25, TotalUsage: kernel.TokenUsage{InputTokens: 5000, OutputTokens: 900},
		}),
		mk("ArtifactSaved", events.ArtifactSaved, events.ArtifactWrittenPayload{
			AgentID: agentID, ArtifactID: "art_1", Name: "report.md", Type: "text/markdown",
			Version: 1, Bytes: 2048, SandboxPath: "/work/report.md", SandboxID: "sb1",
		}),
		mk("ArtifactUpdated", events.ArtifactUpdated, events.ArtifactWrittenPayload{
			AgentID: agentID, ArtifactID: "art_1", Name: "report.md", Type: "text/markdown",
			Version: 2, Bytes: 4096, SandboxPath: "/work/report.md", SandboxID: "sb1",
		}),
		mk("ArtifactDeleted", events.ArtifactDeleted, events.ArtifactDeletedPayload{AgentID: agentID, ArtifactID: "art_1", Name: "report.md"}),
	}
}

// sdkEventTypeValues returns the values of sdk-event-types.json in file order,
// walking the tokens so the order is the order the file was written in.
func sdkEventTypeValues(t *testing.T) []string {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(wiretest.Load(t, "sdk-event-types.json")))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		t.Fatalf("sdk-event-types.json must be an object: %v %v", tok, err)
	}
	var values []string
	for dec.More() {
		if _, err := dec.Token(); err != nil { // key
			t.Fatal(err)
		}
		tok, err := dec.Token() // value
		if err != nil {
			t.Fatal(err)
		}
		s, ok := tok.(string)
		if !ok {
			t.Fatalf("sdk-event-types.json value %v is not a string", tok)
		}
		values = append(values, s)
	}
	if _, err := dec.Token(); err != nil { // closing brace
		t.Fatal(err)
	}
	if _, err := dec.Token(); err != io.EOF {
		t.Fatalf("trailing data in sdk-event-types.json: %v", err)
	}
	return values
}

func TestKindNames(t *testing.T) {
	got := events.KindNames()
	if len(got) != 55 {
		t.Fatalf("KindNames has %d entries, want 55", len(got))
	}
	seen := map[string]bool{}
	for _, n := range got {
		if seen[n] {
			t.Errorf("duplicate kind name %q", n)
		}
		seen[n] = true
	}
	want := sdkEventTypeValues(t)
	if !slices.Equal(got, want) {
		t.Fatalf("KindNames differs from sdk-event-types.json\n got: %v\nwant: %v", got, want)
	}
	// The returned slice is fresh: mutating it must not affect later calls.
	got[0] = "mutated"
	if events.KindNames()[0] == "mutated" {
		t.Fatal("KindNames returned a shared slice")
	}
	// The samples cover every kind in the same order.
	var sampleKinds []string
	for _, s := range allSamples() {
		sampleKinds = append(sampleKinds, s.kind)
	}
	if !slices.Equal(sampleKinds, want) {
		t.Fatalf("samples are not in TS kind order")
	}
}

func TestSubscribeAll22Kinds(t *testing.T) {
	ss := allSamples()
	if len(ss) != len(events.KindNames()) {
		t.Fatalf("%d samples, want %d", len(ss), len(events.KindNames()))
	}
	for _, s := range ss {
		t.Run(s.name, func(t *testing.T) {
			if !s.deliver(t) {
				t.Fatalf("typed handler for %s did not receive an equal payload struct", s.kind)
			}
		})
	}
}

func TestSubscribeUnsubscribeAndIsolation(t *testing.T) {
	d := events.New()
	var iter, other int
	unsub := events.Subscribe(d, events.Iteration, func(_ kernel.Event, p events.IterationPayload) { iter += p.Iteration })
	events.Subscribe(d, events.ModelRequested, func(kernel.Event, events.ModelRequestedPayload) { other++ })
	events.Emit(d, events.Iteration, "s", events.IterationPayload{AgentID: "a", Iteration: 4})
	unsub()
	events.Emit(d, events.Iteration, "s", events.IterationPayload{AgentID: "a", Iteration: 4})
	if iter != 4 || other != 0 {
		t.Fatalf("iter=%d other=%d, want 4 and 0", iter, other)
	}
}

func TestSubscribeSkipsUndecodablePayload(t *testing.T) {
	d := events.New()
	var calls int
	events.Subscribe(d, events.Iteration, func(kernel.Event, events.IterationPayload) { calls++ })
	// A raw emit with the wrong payload type must not panic or call the handler.
	d.Emit(kernel.Event{Type: events.Iteration.Name, SessionID: "s", Payload: 42})
	d.Emit(kernel.Event{Type: events.Iteration.Name, SessionID: "s", Payload: nil})
	if calls != 0 {
		t.Fatalf("handler called %d times for undecodable payloads", calls)
	}
}

func TestPayloadAsForms(t *testing.T) {
	want := events.IterationPayload{AgentID: "a", Iteration: 2, MaxIterations: 9}
	raw := `{"agentId":"a","iteration":2,"maxIterations":9}`

	forms := map[string]any{
		"value":       want,
		"pointer":     &want,
		"RawMessage":  json.RawMessage(raw),
		"bytes":       []byte(raw),
		"decoded map": map[string]any{"agentId": "a", "iteration": float64(2), "maxIterations": float64(9)},
	}
	for name, payload := range forms {
		t.Run(name, func(t *testing.T) {
			got, err := events.PayloadAs[events.IterationPayload](kernel.Event{Payload: payload})
			if err != nil {
				t.Fatalf("PayloadAs: %v", err)
			}
			if got != want {
				t.Fatalf("got %+v, want %+v", got, want)
			}
		})
	}

	var nilPtr *events.IterationPayload
	bad := map[string]any{
		"nil":                 nil,
		"nil pointer":         nilPtr,
		"mismatched struct":   events.ModelRequestedPayload{AgentID: "a"},
		"number":              42,
		"string":              "text",
		"malformed JSON":      json.RawMessage(`{"agentId":`),
		"malformed bytes":     []byte(`not json`),
		"JSON null":           json.RawMessage(`null`),
		"unknown field":       json.RawMessage(`{"agentId":"a","extra":1}`),
		"wrong field type":    json.RawMessage(`{"agentId":"a","iteration":"two"}`),
		"trailing data":       json.RawMessage(`{"agentId":"a"} {}`),
		"map with wrong type": map[string]any{"iteration": "two"},
		"array":               json.RawMessage(`[1,2]`),
		"channel":             make(chan int),
	}
	for name, payload := range bad {
		t.Run("error/"+name, func(t *testing.T) {
			if _, err := events.PayloadAs[events.IterationPayload](kernel.Event{Payload: payload}); err == nil {
				t.Fatalf("expected an error for payload %T", payload)
			}
		})
	}
}

// WR-06: a payload decoded through a map[string]any loses the key order of a
// kernel.Arguments field; DecodeEvent keeps the payload raw and so keeps it.
func TestDecodeEventKeepsArgumentsBytes(t *testing.T) {
	const rawArgs = `{"z":1,"a":1.50,"nested":{"b":2,"a":1}}`
	stored := `{"type":"` + events.ToolApprovalRequested.Name + `","sessionId":"s","payload":` +
		`{"agentId":"a","name":"n","toolCallId":"c","risk":"high","arguments":` + rawArgs + `},"id":"i","timestamp":5}`

	e, err := events.DecodeEvent([]byte(stored))
	if err != nil {
		t.Fatalf("DecodeEvent: %v", err)
	}
	if e.Type != events.ToolApprovalRequested.Name || e.SessionID != "s" || e.ID != "i" || e.Timestamp != 5 {
		t.Fatalf("envelope decoded wrongly: %+v", e)
	}
	p, err := events.PayloadAs[events.ToolApprovalRequestedPayload](e)
	if err != nil {
		t.Fatalf("PayloadAs: %v", err)
	}
	if string(p.Arguments) != rawArgs {
		t.Fatalf("raw path changed the arguments bytes: got %s want %s", p.Arguments, rawArgs)
	}

	// The map path is lossy by design: keys are sorted and 1.50 becomes 1.5.
	var viaMap kernel.Event
	if err := json.Unmarshal([]byte(stored), &viaMap); err != nil {
		t.Fatal(err)
	}
	pm, err := events.PayloadAs[events.ToolApprovalRequestedPayload](viaMap)
	if err != nil {
		t.Fatalf("PayloadAs(map): %v", err)
	}
	if string(pm.Arguments) == rawArgs {
		t.Fatalf("expected the map path to alter the bytes, got %s", pm.Arguments)
	}

	if _, err := events.DecodeEvent([]byte(`{"type":"x","bogus":1}`)); err == nil {
		t.Error("unknown field must be rejected")
	}
	if _, err := events.DecodeEvent([]byte(`{"type":"x"} trailing`)); err == nil {
		t.Error("trailing data must be rejected")
	}
}

// IN-02: Subscribe reports an undecodable payload through the bus's own
// logger, not slog.Default().
func TestSubscribeLogsThroughBusLogger(t *testing.T) {
	var buf bytes.Buffer
	d := events.New(events.WithLogger(slog.New(slog.NewTextHandler(&buf, nil))))
	called := false
	events.Subscribe(d, events.ToolDenied, func(kernel.Event, events.ToolDeniedPayload) { called = true })
	d.Emit(kernel.Event{Type: events.ToolDenied.Name, SessionID: "s", Payload: 42})
	if called {
		t.Fatal("handler must not run for an undecodable payload")
	}
	if !strings.Contains(buf.String(), "event payload not decodable") {
		t.Fatalf("bus logger did not receive the error: %q", buf.String())
	}
}
