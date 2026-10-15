package agentloop_test

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/Phong95/aec-agent-go/aecmock"
	"github.com/Phong95/aec-agent-go/components/agent/agentloop"
	"github.com/Phong95/aec-agent-go/components/agent/memorysystem/memory"
	"github.com/Phong95/aec-agent-go/components/llmhub"
	"github.com/Phong95/aec-agent-go/components/toolhub"
	"github.com/Phong95/aec-agent-go/kernel"
)

// ctxSpan is the end attributes of a context-assembly span, decoded for assertions.
type ctxSpan struct {
	Iteration            int      `json:"iteration"`
	MessageCount         int      `json:"messageCount"`
	PreparedMessageCount int      `json:"preparedMessageCount"`
	Compacted            bool     `json:"compacted"`
	SummaryIndex         *int     `json:"summaryIndex"`
	RecalledMemories     int      `json:"recalledMemories"`
	MemoryRecordIDs      []string `json:"memoryRecordIds"`
	MemoryBlock          *string  `json:"memoryBlock"`
	MemoryInjection      string   `json:"memoryInjection"`
	MemoryTokens         int      `json:"memoryTokens"`
	SystemMessageIndex   *int     `json:"systemMessageIndex"`
	BaseSystemPrompt     *string  `json:"baseSystemPrompt"`
	Messages             []struct {
		Role      string `json:"role"`
		Content   string `json:"content"`
		ToolCalls []struct {
			ID        string          `json:"id"`
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		} `json:"toolCalls"`
		ToolResults []struct {
			ToolCallID string `json:"toolCallId"`
			Content    string `json:"content"`
			IsError    *bool  `json:"isError"`
		} `json:"toolResults"`
		EstimatedTokens int `json:"estimatedTokens"`
	} `json:"messages"`
	Tools []struct {
		Name string `json:"name"`
	} `json:"tools"`
	ToolNames       []string       `json:"toolNames"`
	Provider        *string        `json:"provider"`
	Options         map[string]any `json:"options"`
	EstimatedTokens struct {
		Messages int `json:"messages"`
		Memory   int `json:"memory"`
		Tools    int `json:"tools"`
		Total    int `json:"total"`
	} `json:"estimatedTokens"`
}

// contextSpans decodes the end attributes of every context-assembly span, in order.
func contextSpans(t *testing.T, s *recSink) []ctxSpan {
	t.Helper()
	var out []ctxSpan
	for _, rec := range s.all() {
		if rec.phase != "end" || rec.span.Name != "context-assembly" {
			continue
		}
		b, err := kernel.EncodeJSON(rec.span.Attributes)
		if err != nil {
			t.Fatal(err)
		}
		var cs ctxSpan
		if err := json.Unmarshal(b, &cs); err != nil {
			t.Fatalf("decode %s: %v", b, err)
		}
		out = append(out, cs)
	}
	return out
}

// endKeys is the attribute keys of the first end of a span, in order.
func endKeys(t *testing.T, s *recSink, name string) []string {
	t.Helper()
	attrs, ok := s.endAttrs(name)
	if !ok {
		t.Fatalf("no %s span ended", name)
	}
	return attrs.Keys()
}

const recalledText = "## Recalled memories\n- [fact] Project - deadline: Friday"

func recalledMemories() memory.RecalledMemories {
	content := recalledText
	return memory.RecalledMemories{
		Content:    &content,
		RecordIDs:  []string{"m1"},
		Memories:   []memory.RecalledMemoryTrace{{ID: "m1", Kind: "fact", Key: "deadline", CombinedScore: 0.9, RelevanceScore: 0.8, RecencyScore: 0.95, ImportanceScore: 0.7}},
		MaxResults: 8, IndexRebuilt: false,
	}
}

// promptRig is a rig whose context port builds the system message from the profile,
// so the model input has a system message to inject memories into.
func promptRig(t *testing.T, o rigOptions, turns ...aecmock.ScriptedTurn) *rig {
	t.Helper()
	prof := testProfile("test-agent")
	if o.prof != nil {
		prof = *o.prof
	}
	o.prof = &prof
	o.context = aecmock.NewPromptContext(prof)
	return newRig(t, o, turns...)
}

// sameInput fails unless the traced messages are exactly the messages of the model call.
func sameInput(t *testing.T, cs ctxSpan, sent []kernel.Message) {
	t.Helper()
	if cs.MessageCount != len(sent) || len(cs.Messages) != len(sent) {
		t.Fatalf("span lists %d messages (count %d), the model got %d", len(cs.Messages), cs.MessageCount, len(sent))
	}
	for i, m := range sent {
		if cs.Messages[i].Role != string(m.Role) || cs.Messages[i].Content != m.Content {
			t.Fatalf("span message %d = %s %q, model got %s %q", i, cs.Messages[i].Role, cs.Messages[i].Content, m.Role, m.Content)
		}
	}
}

// X1 and X2: the span is the exact model input, with memory injected into the last
// system message and the base prompt recorded before the injection.
func TestContextSpan_EqualsModelInput_Run(t *testing.T) { contextSpanEqualsModelInput(t, "run") }

func TestContextSpan_EqualsModelInput_Stream(t *testing.T) { contextSpanEqualsModelInput(t, "stream") }

func contextSpanEqualsModelInput(t *testing.T, mode string) {
	t.Helper()
	mem := aecmock.NewScriptedMemory(recalledMemories())
	r := promptRig(t, rigOptions{memory: mem}, aecmock.ScriptedTurn{Content: "Friday."})
	if mode == "run" {
		if _, err := r.run(t, "When is the deadline?"); err != nil {
			t.Fatal(err)
		}
	} else if _, err := r.stream(t, "When is the deadline?"); err != nil {
		t.Fatal(err)
	}

	spans := contextSpans(t, r.sink)
	if len(spans) != 1 {
		t.Fatalf("%d context-assembly spans, want 1", len(spans))
	}
	cs := spans[0]
	sent := r.models.Calls()[0].Messages
	sameInput(t, cs, sent)
	if cs.MemoryInjection != "appended-to-system" || cs.MemoryBlock == nil || *cs.MemoryBlock != recalledText {
		t.Fatalf("memory injection = %q, block %v", cs.MemoryInjection, cs.MemoryBlock)
	}
	if cs.BaseSystemPrompt == nil || *cs.BaseSystemPrompt != "You are a test agent." {
		t.Fatalf("baseSystemPrompt = %v, want the prompt before the injection", cs.BaseSystemPrompt)
	}
	if want := "You are a test agent.\n\n" + recalledText; sent[0].Content != want {
		t.Fatalf("system message = %q, want the memory appended after a blank line", sent[0].Content)
	}
	if cs.SystemMessageIndex == nil || *cs.SystemMessageIndex != 0 {
		t.Fatalf("systemMessageIndex = %v", cs.SystemMessageIndex)
	}
	if cs.RecalledMemories != 1 || !slices.Equal(cs.MemoryRecordIDs, []string{"m1"}) || cs.MemoryTokens <= 0 {
		t.Fatalf("recalled %d %v, memory tokens %d", cs.RecalledMemories, cs.MemoryRecordIDs, cs.MemoryTokens)
	}
	if calls := mem.Calls(); len(calls) != 1 || calls[0].UserMessage != "When is the deadline?" || calls[0].SessionID != "s-1" {
		t.Fatalf("recall calls = %+v, want exactly one for the user message", calls)
	}

	// The memory-recall span: a child of the run span, with the recall statistics.
	if got := r.sink.parentName("memory-recall"); got != "agent-run" {
		t.Fatalf("memory-recall parent = %q, want agent-run", got)
	}
	keys := endKeys(t, r.sink, "memory-recall")
	want := []string{"queryLength", "queryPreview", "recalledCount", "maxResults", "hit", "indexRebuilt", "injectedChars", "injectedTokens", "topScore", "memories"}
	if !slices.Equal(keys, want) {
		t.Fatalf("memory-recall end keys = %v, want %v", keys, want)
	}
}

// X3: after a compaction the span says so, its summaryIndex points at the summary,
// and more messages were prepared than are sent.
func TestContextSpan_Compaction_Run(t *testing.T)    { contextSpanCompaction(t, "run") }
func TestContextSpan_Compaction_Stream(t *testing.T) { contextSpanCompaction(t, "stream") }

func contextSpanCompaction(t *testing.T, mode string) {
	t.Helper()
	usage := &kernel.TokenUsage{InputTokens: 100, OutputTokens: 50}
	r := promptRig(t, rigOptions{prof: x3Profile()},
		aecmock.ScriptedTurn{Content: "first", Usage: usage},
		aecmock.ScriptedTurn{Content: "second", Usage: usage},
	)
	r.models.With(aecmock.WithSummaryReply("SUMMARY-TEXT"))
	drive := func(msg string) {
		if mode == "run" {
			if _, err := r.run(t, msg); err != nil {
				t.Fatal(err)
			}
		} else if _, err := r.stream(t, msg); err != nil {
			t.Fatal(err)
		}
	}
	drive("q1")
	drive("q2")

	spans := contextSpans(t, r.sink)
	if len(spans) != 2 {
		t.Fatalf("%d context spans, want 2", len(spans))
	}
	if spans[0].Compacted {
		t.Fatal("the first turn compacted")
	}
	last := spans[1]
	if !last.Compacted || last.SummaryIndex == nil {
		t.Fatalf("the second span is not compacted: %+v", last)
	}
	if got := last.Messages[*last.SummaryIndex].Content; got != "SUMMARY-TEXT" {
		t.Fatalf("message at summaryIndex = %q, want the summary", got)
	}
	if last.PreparedMessageCount <= last.MessageCount {
		t.Fatalf("prepared %d messages, sent %d: compaction must have dropped some", last.PreparedMessageCount, last.MessageCount)
	}
	sameInput(t, last, mainCalls(r.models.Calls())[1].Messages)
}

// X4: in a tool loop the second span holds the tool calls and results and the tool
// definitions.
func TestContextSpan_ToolLoop_Run(t *testing.T)    { contextSpanToolLoop(t, "run") }
func TestContextSpan_ToolLoop_Stream(t *testing.T) { contextSpanToolLoop(t, "stream") }

func contextSpanToolLoop(t *testing.T, mode string) {
	t.Helper()
	h := constTool("test_tool", toolhub.Output{Content: "ok"})
	r := promptRig(t, rigOptions{handlers: []toolhub.Handler{h}},
		toolTurn(callOf("tc-1", "test_tool", `{"x":1}`)),
		aecmock.ScriptedTurn{Content: "done"},
	)
	if mode == "run" {
		if _, err := r.run(t, "go"); err != nil {
			t.Fatal(err)
		}
	} else if _, err := r.stream(t, "go"); err != nil {
		t.Fatal(err)
	}

	spans := contextSpans(t, r.sink)
	if len(spans) != 2 {
		t.Fatalf("%d context spans, want 2", len(spans))
	}
	first, second := spans[0], spans[1]
	if len(first.Tools) != 1 || first.Tools[0].Name != "test_tool" || !slices.Equal(first.ToolNames, []string{"test_tool"}) {
		t.Fatalf("the first span's tools = %+v", first.Tools)
	}
	if second.Iteration != 2 || len(second.Messages) != 4 {
		t.Fatalf("second span: iteration %d, %d messages, want 2 and 4", second.Iteration, len(second.Messages))
	}
	call, result := second.Messages[2], second.Messages[3]
	if len(call.ToolCalls) != 1 || call.ToolCalls[0].Name != "test_tool" || string(call.ToolCalls[0].Arguments) != `{"x":1}` {
		t.Fatalf("the call message = %+v", call)
	}
	if len(result.ToolResults) != 1 || result.ToolResults[0].ToolCallID != "tc-1" || result.ToolResults[0].Content != "ok" || result.ToolResults[0].IsError != nil {
		t.Fatalf("the result message = %+v", result)
	}
	if len(second.Tools) != 1 || second.Tools[0].Name != "test_tool" {
		t.Fatalf("the second span's tools = %+v", second.Tools)
	}
	sameInput(t, second, r.models.Calls()[1].Messages)
	if second.EstimatedTokens.Tools <= 0 || second.EstimatedTokens.Total != second.EstimatedTokens.Messages+second.EstimatedTokens.Tools {
		t.Fatalf("token estimate = %+v", second.EstimatedTokens)
	}
}

// X5: the options of the span are the effective options (the manager's defaults
// merged under the overrides) without the tools, and the provider is named. The same
// facts are on the model-call span, which takes them from the same description.
func TestContextSpan_EffectiveOptions(t *testing.T) {
	r := promptRig(t, rigOptions{handlers: []toolhub.Handler{constTool("lookup", toolhub.Output{Content: "x"})}}, aecmock.ScriptedTurn{Content: "hi"})
	r.models.With(
		aecmock.WithOptionResolver(func(o llmhub.ModelOptions) llmhub.ModelOptions {
			if o.Model == "" {
				o.Model = "scripted-model"
			}
			if o.Temperature == nil {
				temp := 0.2
				o.Temperature = &temp
			}
			return o
		}),
		aecmock.WithProviderName("scripted"),
	)
	maxTokens := 256
	if _, err := r.loop.Run(t.Context(), "hello", "s-1", llmhub.ModelOptions{MaxTokens: &maxTokens}, agentloop.RunOptions{}); err != nil {
		t.Fatal(err)
	}

	cs := contextSpans(t, r.sink)[0]
	if cs.Provider == nil || *cs.Provider != "scripted" {
		t.Fatalf("provider = %v", cs.Provider)
	}
	want := map[string]any{"model": "scripted-model", "maxTokens": 256.0, "temperature": 0.2}
	if len(cs.Options) != len(want) {
		t.Fatalf("options = %v, want %v (no tools key)", cs.Options, want)
	}
	for k, v := range want {
		if cs.Options[k] != v {
			t.Fatalf("options[%s] = %v, want %v", k, cs.Options[k], v)
		}
	}
	keys := endKeys(t, r.sink, "context-assembly")
	if got := keys[len(keys)-4:]; !slices.Equal(got, []string{"toolNames", "provider", "options", "estimatedTokens"}) {
		t.Fatalf("the last attribute keys are %v", keys)
	}

	start := r.sink.all()
	for _, rec := range start {
		if rec.phase != "start" || rec.span.Name != "model-call" {
			continue
		}
		if v, _ := rec.span.Attributes.Get("provider"); v != "scripted" {
			t.Fatalf("model-call provider = %v", v)
		}
		if v, _ := rec.span.Attributes.Get("model"); v != "scripted-model" {
			t.Fatalf("model-call model = %v", v)
		}
		if _, ok := rec.span.Attributes.Get("options"); !ok {
			t.Fatal("model-call has no options")
		}
	}
	// What the model actually received still carries the tools (they are not an option
	// of the span, they are the request).
	if sent := r.models.Calls()[0].Options; len(sent.Tools) != 1 || sent.Tools[0].Name != "lookup" || sent.MaxTokens == nil || *sent.MaxTokens != 256 {
		t.Fatalf("model options = %+v", sent)
	}
}

// X5, second half: the stream model-call span ends with the token counts when the done
// chunk carried usage, and without them when it did not.
func TestModelCallSpan_StreamUsage(t *testing.T) {
	with := newRig(t, rigOptions{}, aecmock.ScriptedTurn{Content: "hi", Usage: &kernel.TokenUsage{InputTokens: 12, OutputTokens: 7}})
	if _, err := with.stream(t, "q"); err != nil {
		t.Fatal(err)
	}
	attrs, _ := with.sink.endAttrs("model-call")
	if in, _ := attrs.Get("inputTokens"); in != 12 {
		t.Fatalf("inputTokens = %v, want 12", in)
	}
	if out, _ := attrs.Get("outputTokens"); out != 7 {
		t.Fatalf("outputTokens = %v, want 7", out)
	}
	keys := attrs.Keys()
	if got := keys[len(keys)-2:]; !slices.Equal(got, []string{"inputTokens", "outputTokens"}) {
		t.Fatalf("the stream usage keys come last, got %v", keys)
	}

	without := newRig(t, rigOptions{}, aecmock.ScriptedTurn{Content: "hi"})
	if _, err := without.stream(t, "q"); err != nil {
		t.Fatal(err)
	}
	attrs, _ = without.sink.endAttrs("model-call")
	if attrs.Has("inputTokens") || attrs.Has("outputTokens") {
		t.Fatalf("a done chunk without usage recorded tokens: %v", attrs.Keys())
	}
}

// X6: with no memory system the span says "none", has no memory block and an empty id
// list, and no memory-recall span exists.
func TestContextSpan_MemoryDisabled(t *testing.T) {
	r := promptRig(t, rigOptions{}, aecmock.ScriptedTurn{Content: "ok"})
	if _, err := r.run(t, "hello"); err != nil {
		t.Fatal(err)
	}
	cs := contextSpans(t, r.sink)[0]
	if cs.MemoryInjection != "none" || cs.MemoryBlock != nil || cs.MemoryRecordIDs == nil || len(cs.MemoryRecordIDs) != 0 || cs.RecalledMemories != 0 || cs.MemoryTokens != 0 {
		t.Fatalf("memory fields = %+v", cs)
	}
	for _, rec := range r.sink.all() {
		if rec.span.Name == "memory-recall" {
			t.Fatal("a memory-recall span was started without a memory system")
		}
	}
	// The attributes without a value are left out: no memoryBlock, no summaryIndex, no
	// provider.
	keys := endKeys(t, r.sink, "context-assembly")
	for _, absent := range []string{"memoryBlock", "summaryIndex", "provider"} {
		if slices.Contains(keys, absent) {
			t.Fatalf("attribute %s is present: %v", absent, keys)
		}
	}
	wantOrder := []string{"iteration", "messageCount", "preparedMessageCount", "compacted", "recalledMemories", "memoryRecordIds", "memoryInjection", "memoryTokens", "systemMessageIndex", "baseSystemPrompt", "messages", "tools", "toolNames", "options", "estimatedTokens"}
	if !slices.Equal(keys, wantOrder) {
		t.Fatalf("attribute keys = %v\nwant %v", keys, wantOrder)
	}
}

// A failing recall never fails the turn: the span ends with the error, a warning is
// logged with ids and the error text only, and the run goes on without memories.
func TestMemoryRecallFailureDoesNotFailTheTurn(t *testing.T) {
	lg, buf := failureLog()
	mem := aecmock.NewScriptedMemory().With(aecmock.WithRecallError(errBoom("index unavailable")))
	r := promptRig(t, rigOptions{memory: mem, loopOpts: []agentloop.Option{agentloop.WithLogger(lg)}}, aecmock.ScriptedTurn{Content: "carried on"})
	res, err := r.run(t, "SECRET-QUESTION")
	if err != nil || res.Message.Content != "carried on" {
		t.Fatalf("Run = %+v, %v", res, err)
	}

	attrs, ok := r.sink.endAttrs("memory-recall")
	if !ok {
		t.Fatal("the memory-recall span never ended")
	}
	if v, _ := attrs.Get("error"); v != "index unavailable" {
		t.Fatalf("memory-recall error = %v", v)
	}
	if cs := contextSpans(t, r.sink)[0]; cs.MemoryInjection != "none" || cs.RecalledMemories != 0 {
		t.Fatalf("a failed recall injected memories: %+v", cs)
	}
	logged := buf.String()
	for _, want := range []string{"agentId=test-agent", "sessionId=s-1", "index unavailable"} {
		if !strings.Contains(logged, want) {
			t.Fatalf("the warning lacks %q:\n%s", want, logged)
		}
	}
	if strings.Contains(logged, "SECRET-QUESTION") {
		t.Fatalf("the warning leaks the user message:\n%s", logged)
	}
}

type boomError string

func (e boomError) Error() string { return string(e) }

func errBoom(s string) error { return boomError(s) }

// withRecalledMemories: appended to the LAST system message with a blank line, or a new
// system message in front; the same slice back when there is nothing to inject; the
// input never changed.
func TestWithRecalledMemories(t *testing.T) {
	text := "MEM"
	recalled := &memory.RecalledMemories{Content: &text}
	sys := func(c string) kernel.Message { return kernel.Message{Role: kernel.RoleSystem, Content: c} }
	usr := func(c string) kernel.Message { return kernel.Message{Role: kernel.RoleUser, Content: c} }

	msgs := []kernel.Message{sys("A"), usr("q"), sys("B"), usr("q2")}
	out := agentloop.WithRecalledMemories(msgs, recalled)
	if got := contents(out); !slices.Equal(got, []string{"A", "q", "B\n\nMEM", "q2"}) {
		t.Fatalf("appended to the wrong message: %v", got)
	}
	if msgs[2].Content != "B" {
		t.Fatal("the input message was changed")
	}

	noSystem := []kernel.Message{usr("q")}
	out = agentloop.WithRecalledMemories(noSystem, recalled)
	if len(out) != 2 || out[0].Role != kernel.RoleSystem || out[0].Content != "MEM" || out[1].Content != "q" {
		t.Fatalf("no system message: %+v, want a new one in front", out)
	}

	empty := ""
	for name, r := range map[string]*memory.RecalledMemories{
		"nil recall":    nil,
		"nil content":   {},
		"empty content": {Content: &empty},
	} {
		if got := agentloop.WithRecalledMemories(msgs, r); !sameSlice(got, msgs) {
			t.Errorf("%s: a copy came back, want the same slice", name)
		}
	}
}

// When the compacted messages hold no system message, memory injection puts a new one in
// front and the summary index shifts by one.
func TestMemoryInjectionShiftsTheSummaryIndex(t *testing.T) {
	usage := &kernel.TokenUsage{InputTokens: 100, OutputTokens: 50}
	mem := aecmock.NewScriptedMemory(recalledMemories())
	r := newRig(t, rigOptions{prof: x3Profile(), memory: mem},
		aecmock.ScriptedTurn{Content: "first", Usage: usage},
		aecmock.ScriptedTurn{Content: "second", Usage: usage},
	)
	r.models.With(aecmock.WithSummaryReply("SUMMARY-TEXT"))
	for _, q := range []string{"q1", "q2"} {
		if _, err := r.run(t, q); err != nil {
			t.Fatal(err)
		}
	}
	last := contextSpans(t, r.sink)[1]
	if last.MemoryInjection != "new-system-message" || !last.Compacted || last.SummaryIndex == nil {
		t.Fatalf("span = %+v", last)
	}
	if last.Messages[0].Role != "system" || last.Messages[0].Content != recalledText {
		t.Fatalf("message 0 = %+v, want the memory as a new system message", last.Messages[0])
	}
	if got := last.Messages[*last.SummaryIndex].Content; got != "SUMMARY-TEXT" {
		t.Fatalf("summaryIndex %d points at %q, want the summary (shifted past the new system message)", *last.SummaryIndex, got)
	}
	if last.BaseSystemPrompt != nil {
		t.Fatalf("baseSystemPrompt = %q, want none (there was no system message before the injection)", *last.BaseSystemPrompt)
	}
}

// The counter calibration is part of the session: a new Loop over a conversation whose
// session carries a calibration starts the run with that ratio, not the cold 4. The
// span's token estimates show it (8 characters are 4 tokens at 2 characters per token
// and 2 at the default 4).
func TestRun_SeedsTheCounterFromTheSession(t *testing.T) {
	r := newRig(t, rigOptions{}, aecmock.ScriptedTurn{Content: "ok"})
	s, _ := r.conv.Session()
	s.CounterCalibration = &kernel.CounterCalibration{CharsPerToken: 2, Samples: 5}
	if err := r.conv.UpdateSession(t.Context(), s); err != nil {
		t.Fatal(err)
	}
	if _, err := r.run(t, "12345678"); err != nil {
		t.Fatal(err)
	}
	cs := contextSpans(t, r.sink)[0]
	if got := cs.Messages[0].EstimatedTokens; got != 4 {
		t.Fatalf("the 8-character message is %d tokens, want 4 at the seeded ratio of 2", got)
	}

	// Without a saved calibration the ratio is the default 4.
	cold := newRig(t, rigOptions{}, aecmock.ScriptedTurn{Content: "ok"})
	if _, err := cold.run(t, "12345678"); err != nil {
		t.Fatal(err)
	}
	if got := contextSpans(t, cold.sink)[0].Messages[0].EstimatedTokens; got != 2 {
		t.Fatalf("a cold run counts %d tokens, want 2 at the default ratio of 4", got)
	}
}

// A message the loop builds for one call only (the system message that memory injection
// rewrote, the memory block, the tool list) is counted at the ratio of the moment, as
// a new message. After the first model response has moved the ratio, the
// second iteration therefore counts them again, while the messages of the conversation
// keep the count they had.
func TestFreshMessagesAreCountedAtTheCurrentRatio(t *testing.T) {
	h := constTool("lookup", toolhub.Output{Content: "ok"})
	// 10 input tokens for the first call: the ratio moves to (chars sent) / 10.
	first := toolTurn(callOf("tc-1", "lookup", `{}`))
	first.Usage = &kernel.TokenUsage{InputTokens: 10, OutputTokens: 5}
	mem := aecmock.NewScriptedMemory(recalledMemories())
	r := promptRig(t, rigOptions{handlers: []toolhub.Handler{h}, memory: mem}, first, aecmock.ScriptedTurn{Content: "done"})
	if _, err := r.run(t, "go"); err != nil {
		t.Fatal(err)
	}
	s, _ := r.conv.Session()
	if s.CounterCalibration == nil {
		t.Fatal("no calibration was saved")
	}
	if s.CounterCalibration.CharsPerToken == 4 {
		t.Fatalf("setup: the ratio did not move from 4")
	}
	spans := contextSpans(t, r.sink)
	if len(spans) != 2 {
		t.Fatalf("%d context spans", len(spans))
	}
	it1, it2 := spans[0], spans[1]

	// The injected system message and the memory block are fresh at iteration 2: their
	// counts differ from iteration 1, which used the cold ratio, and equal the estimate
	// at the ratio that iteration 1's calibration produced.
	if it1.Messages[0].EstimatedTokens == it2.Messages[0].EstimatedTokens {
		t.Fatalf("the injected system message kept its count %d: it must be counted again at the new ratio", it1.Messages[0].EstimatedTokens)
	}
	if it1.MemoryTokens == it2.MemoryTokens {
		t.Fatalf("the memory block kept its count %d", it1.MemoryTokens)
	}

	// A message of the conversation keeps the count it had when it was first seen.
	if it1.Messages[1].EstimatedTokens != it2.Messages[1].EstimatedTokens {
		t.Fatalf("the user message was recounted: %d then %d", it1.Messages[1].EstimatedTokens, it2.Messages[1].EstimatedTokens)
	}
}
