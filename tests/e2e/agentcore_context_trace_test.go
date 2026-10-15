package e2e_test

import (
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/Phong95/aec-agent-go/aecmock"
	"github.com/Phong95/aec-agent-go/components/agent/memorysystem/memory"
	"github.com/Phong95/aec-agent-go/components/agent/profile"
	"github.com/Phong95/aec-agent-go/components/toolhub"
	"github.com/Phong95/aec-agent-go/kernel"
)

// ctxSpan is the end attributes of a context-assembly span, decoded for assertions.
type ctxSpan struct {
	Iteration            int              `json:"iteration"`
	MessageCount         int              `json:"messageCount"`
	PreparedMessageCount int              `json:"preparedMessageCount"`
	Compacted            bool             `json:"compacted"`
	SummaryIndex         *int             `json:"summaryIndex"`
	MemoryRecordIDs      []string         `json:"memoryRecordIds"`
	MemoryBlock          *string          `json:"memoryBlock"`
	MemoryInjection      string           `json:"memoryInjection"`
	SystemMessageIndex   *int             `json:"systemMessageIndex"`
	BaseSystemPrompt     *string          `json:"baseSystemPrompt"`
	Messages             []ctxSpanMessage `json:"messages"`
	Tools                []struct {
		Name        string          `json:"name"`
		Description string          `json:"description"`
		Parameters  json.RawMessage `json:"parameters"`
	} `json:"tools"`
	Provider *string        `json:"provider"`
	Options  map[string]any `json:"options"`
}

// ctxSpanMessage is one message of a context-assembly span.
type ctxSpanMessage struct {
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
}

// contextSpans decodes the end attributes of every context-assembly span, in order.
func contextSpans(t *testing.T, l *loopCore) []ctxSpan {
	t.Helper()
	var out []ctxSpan
	for _, sp := range l.spans.ended("context-assembly") {
		raw, err := kernel.EncodeJSON(sp.Attributes)
		if err != nil {
			t.Fatal(err)
		}
		var cs ctxSpan
		if err := json.Unmarshal(raw, &cs); err != nil {
			t.Fatalf("decode %s: %v", raw, err)
		}
		out = append(out, cs)
	}
	return out
}

// normMessage keeps the fields of a message that must match exactly, and drops the rest
// (the timestamp, the estimated tokens).
type normMessage struct {
	Role        string
	Content     string
	ToolCalls   []string // "id|name|arguments"
	ToolResults []string // "toolCallId|content|isError"
}

// normalizeSent normalizes the messages a model call received.
func normalizeSent(msgs []kernel.Message) []normMessage {
	out := make([]normMessage, len(msgs))
	for i, m := range msgs {
		n := normMessage{Role: string(m.Role), Content: m.Content}
		for _, c := range m.ToolCalls {
			n.ToolCalls = append(n.ToolCalls, fmt.Sprintf("%s|%s|%s", c.ID, c.Name, c.Arguments))
		}
		for _, r := range m.ToolResults {
			n.ToolResults = append(n.ToolResults, fmt.Sprintf("%s|%s|%v", r.ToolCallID, r.Content, r.IsError != nil && *r.IsError))
		}
		out[i] = n
	}
	return out
}

// normalizeTraced normalizes the messages a context-assembly span lists.
func normalizeTraced(msgs []ctxSpanMessage) []normMessage {
	out := make([]normMessage, len(msgs))
	for i, m := range msgs {
		n := normMessage{Role: m.Role, Content: m.Content}
		for _, c := range m.ToolCalls {
			n.ToolCalls = append(n.ToolCalls, fmt.Sprintf("%s|%s|%s", c.ID, c.Name, c.Arguments))
		}
		for _, r := range m.ToolResults {
			n.ToolResults = append(n.ToolResults, fmt.Sprintf("%s|%s|%v", r.ToolCallID, r.Content, r.IsError != nil && *r.IsError))
		}
		out[i] = n
	}
	return out
}

// requireSpanEqualsInput fails unless the traced messages are exactly the messages the
// provider received.
func requireSpanEqualsInput(t *testing.T, cs ctxSpan, sent []kernel.Message) {
	t.Helper()
	if got, want := normalizeTraced(cs.Messages), normalizeSent(sent); !reflect.DeepEqual(got, want) {
		t.Fatalf("the span messages differ from the model input\nspan:  %+v\ninput: %+v", got, want)
	}
	if cs.MessageCount != len(sent) {
		t.Fatalf("messageCount = %d, the model got %d messages", cs.MessageCount, len(sent))
	}
}

// mainModelCalls are the provider calls that are not summarizer calls: a summarizer call is
// the one whose options carry a system prompt.
func mainModelCalls(l *loopCore) []aecmock.MockCall {
	var out []aecmock.MockCall
	for _, c := range l.provider.Calls() {
		if c.Options.SystemPrompt == "" {
			out = append(out, c)
		}
	}
	return out
}

// traceTurns is the script of the recording model: with toolMode the first main call
// asks for test_tool with {"x":1}, and every other main call answers "ok", each with the
// usage 100 in and 50 out, on Chat and on the done chunk of Stream alike.
func traceTurns(toolMode bool, n int) []aecmock.ScriptedTurn {
	usage := &kernel.TokenUsage{InputTokens: 100, OutputTokens: 50}
	var turns []aecmock.ScriptedTurn
	if toolMode {
		turn := toolTurn(toolCallOf("tc-1", "test_tool", `{"x":1}`))
		turn.Usage = usage
		turns = append(turns, turn)
	}
	for len(turns) < n {
		turns = append(turns, aecmock.ScriptedTurn{Content: "ok", Usage: usage})
	}
	return turns
}

// traceMemory is the memory the test seeds directly (interval 0: consolidation never
// runs).
func traceMemory() []memory.Record {
	return []memory.Record{{
		ID: "m1", Kind: memory.KindUserPreference, Scope: memory.ScopeUser, Subject: "user",
		Key: "response_language", Value: "User prefers Vietnamese responses", Status: memory.StatusActive,
		Confidence: memory.ConfidenceExplicit, Source: "conversation", SourceTrust: memory.TrustUserApproved,
		ObservedAt: kernel.MillisToISO(kernel.NowMillis()),
	}}
}

const traceSystemPrompt = "You are a helpful assistant."

// withMemory enables the memory system with the settings of loopMemoryOn.
func withMemory(p *profile.AgentProfile) { p.Memory = loopMemoryOn() }

// withSummarizer answers the compaction summarizer's calls with SUMMARY-TEXT without using
// a turn.
var withSummarizer = aecmock.WithScriptedOptions(aecmock.WithSummaryReply("SUMMARY-TEXT"))

// TestAgentCoreContextTrace checks the context-assembly
// span of a real AgentCore run is the exact input of the model call, with memory injected
// through the real memory system, after compaction, in a tool loop, with the effective
// options, and the model-call span records the streamed token counts. The two paired
// cases (compaction and tool loop, each on the run and the stream path) are subtests
// named with their path.
func TestAgentCoreContextTrace(t *testing.T) {
	t.Run("run(): span messages equal the main call's actual input", func(t *testing.T) {
		l := newLoopCore(t, loopOptions{profile: withMemory, seed: traceMemory()}, traceTurns(false, 1))
		defer l.dispose(t)

		if _, err := l.chat(t.Context(), "which language should responses use?"); err != nil {
			t.Fatalf("Chat: %v", err)
		}

		spans := contextSpans(t, l)
		if len(spans) != 1 {
			t.Fatalf("%d context-assembly spans, want 1", len(spans))
		}
		cs := spans[0]
		main := mainModelCalls(l)
		if len(main) != 1 {
			t.Fatalf("%d main provider calls, want 1", len(main))
		}
		requireSpanEqualsInput(t, cs, main[0].Messages)
		if cs.BaseSystemPrompt == nil || *cs.BaseSystemPrompt != traceSystemPrompt {
			t.Fatalf("baseSystemPrompt = %v, want %q", cs.BaseSystemPrompt, traceSystemPrompt)
		}
		if cs.MemoryBlock == nil || !strings.Contains(*cs.MemoryBlock, "## Recalled memories") || !strings.Contains(*cs.MemoryBlock, "Vietnamese") {
			t.Fatalf("memoryBlock = %v, want the recalled memories heading and the Vietnamese memory", cs.MemoryBlock)
		}
		if !slices.Equal(cs.MemoryRecordIDs, []string{"m1"}) {
			t.Fatalf("memoryRecordIds = %v, want [m1]", cs.MemoryRecordIDs)
		}
		if cs.MemoryInjection != "appended-to-system" {
			t.Fatalf("memoryInjection = %q, want appended-to-system", cs.MemoryInjection)
		}
		if cs.SystemMessageIndex == nil {
			t.Fatal("systemMessageIndex is not defined")
		}
		if got, want := cs.Messages[*cs.SystemMessageIndex].Content, *cs.BaseSystemPrompt+"\n\n"+*cs.MemoryBlock; got != want {
			t.Fatalf("system message = %q, want the base prompt, a blank line and the memory block %q", got, want)
		}
		if cs.MessageCount != len(main[0].Messages) {
			t.Fatalf("messageCount = %d, want %d", cs.MessageCount, len(main[0].Messages))
		}
	})

	t.Run("stream(): span messages equal the main call's actual input", func(t *testing.T) {
		l := newLoopCore(t, loopOptions{profile: withMemory, seed: traceMemory()}, traceTurns(false, 1))
		defer l.dispose(t)

		if _, err := drainStream(l.stream(t.Context(), "which language for responses?")); err != nil {
			t.Fatalf("ChatStream: %v", err)
		}

		spans := contextSpans(t, l)
		if len(spans) != 1 {
			t.Fatalf("%d context-assembly spans, want 1", len(spans))
		}
		cs := spans[0]
		main := mainModelCalls(l)
		if len(main) != 1 {
			t.Fatalf("%d main provider calls, want 1", len(main))
		}
		requireSpanEqualsInput(t, cs, main[0].Messages)
		if cs.MemoryBlock == nil || !strings.Contains(*cs.MemoryBlock, "Vietnamese") {
			t.Fatalf("memoryBlock = %v, want the Vietnamese memory", cs.MemoryBlock)
		}
		if cs.MemoryInjection != "appended-to-system" {
			t.Fatalf("memoryInjection = %q, want appended-to-system", cs.MemoryInjection)
		}
		if cs.MessageCount != len(main[0].Messages) {
			t.Fatalf("messageCount = %d, want %d", cs.MessageCount, len(main[0].Messages))
		}
	})

	for _, path := range []string{"run", "stream"} {
		drive := func(t *testing.T, l *loopCore, message string) error {
			if path == "run" {
				_, err := l.chat(t.Context(), message)
				return err
			}
			_, err := drainStream(l.stream(t.Context(), message))
			return err
		}

		t.Run(fmt.Sprintf("compaction (%s path): the last context span is compacted and equals the last main-call input", path), func(t *testing.T) {
			compacting := func(p *profile.AgentProfile) {
				p.Compactor.ContextWindowTokens = 100
				p.Compactor.Threshold = 0.5
				p.Compactor.ProtectFirstN = 1
				p.Compactor.ProtectLastN = 1
				p.Compactor.MinTailUserMessages = 1
				p.Compactor.TailTargetRatio = 0.01
			}
			l := newLoopCore(t, loopOptions{profile: compacting}, traceTurns(false, 2), withSummarizer)
			defer l.dispose(t)

			if err := drive(t, l, "first"); err != nil {
				t.Fatalf("first turn: %v", err)
			}
			if err := drive(t, l, "second"); err != nil {
				t.Fatalf("second turn: %v", err)
			}

			spans := contextSpans(t, l)
			if len(spans) < 2 {
				t.Fatalf("%d context-assembly spans, want at least 2", len(spans))
			}
			last := spans[len(spans)-1]
			if !last.Compacted {
				t.Fatal("the last span is not compacted")
			}
			if last.SummaryIndex == nil {
				t.Fatal("the compacted span has no summaryIndex")
			}
			if got := last.Messages[*last.SummaryIndex].Content; got != "SUMMARY-TEXT" {
				t.Fatalf("the message at summaryIndex = %q, want SUMMARY-TEXT", got)
			}
			if last.PreparedMessageCount <= last.MessageCount {
				t.Fatalf("prepared %d messages and sent %d: compaction must have dropped some", last.PreparedMessageCount, last.MessageCount)
			}
			main := mainModelCalls(l)
			requireSpanEqualsInput(t, last, main[len(main)-1].Messages)
		})

		t.Run(fmt.Sprintf("tool loop (%s path): the 2nd context span carries tool calls/results and matches the 2nd main-call input", path), func(t *testing.T) {
			testTool := succeedingTool("test_tool")
			l := newLoopCore(t, loopOptions{tools: []toolhub.Handler{testTool}}, traceTurns(true, 2))
			defer l.dispose(t)

			if err := drive(t, l, "use the tool"); err != nil {
				t.Fatalf("turn: %v", err)
			}

			spans := contextSpans(t, l)
			if len(spans) != 2 {
				t.Fatalf("%d context-assembly spans, want 2", len(spans))
			}
			second := spans[1]

			var withCall *ctxSpanMessage
			var withResult *ctxSpanMessage
			for i := range second.Messages {
				if len(second.Messages[i].ToolCalls) > 0 && withCall == nil {
					withCall = &second.Messages[i]
				}
				if len(second.Messages[i].ToolResults) > 0 && withResult == nil {
					withResult = &second.Messages[i]
				}
			}
			if withCall == nil || withCall.ToolCalls[0].ID == "" || withCall.ToolCalls[0].Name != "test_tool" || string(withCall.ToolCalls[0].Arguments) != `{"x":1}` {
				t.Fatalf("the tool call message = %+v, want a call of test_tool with {\"x\":1}", withCall)
			}
			if withResult == nil || withResult.ToolResults[0].ToolCallID != withCall.ToolCalls[0].ID || withResult.ToolResults[0].Content != "ok" {
				t.Fatalf("the tool result message = %+v, want the result ok for the call %s", withResult, withCall.ToolCalls[0].ID)
			}
			main := mainModelCalls(l)
			if len(main) != 2 {
				t.Fatalf("%d main provider calls, want 2", len(main))
			}
			requireSpanEqualsInput(t, second, main[1].Messages)

			def := testTool.Definition()
			wantParams, err := kernel.EncodeJSON(def.Parameters)
			if err != nil {
				t.Fatal(err)
			}
			if len(second.Tools) != 1 || second.Tools[0].Name != "test_tool" || second.Tools[0].Description != def.Description || string(second.Tools[0].Parameters) != string(wantParams) {
				t.Fatalf("span tools = %+v, want the one definition of test_tool", second.Tools)
			}
		})
	}

	t.Run("context span options match the provider-received options minus tools", func(t *testing.T) {
		temperature, maxTokens := 0.3, 1234
		l := newLoopCore(t, loopOptions{profile: func(p *profile.AgentProfile) {
			p.LLMModel = "model-x"
			p.InferenceConfig = &profile.InferenceConfig{Temperature: &temperature, MaxTokens: &maxTokens}
		}}, traceTurns(false, 1))
		defer l.dispose(t)

		if _, err := l.chat(t.Context(), "hello"); err != nil {
			t.Fatalf("Chat: %v", err)
		}

		cs := contextSpans(t, l)[0]
		wantOptions := map[string]any{"model": "model-x", "temperature": 0.3, "maxTokens": 1234.0}
		if !reflect.DeepEqual(cs.Options, wantOptions) {
			t.Fatalf("span options = %v, want %v", cs.Options, wantOptions)
		}
		if cs.Provider == nil || *cs.Provider != l.provider.Name() {
			t.Fatalf("span provider = %v, want %s", cs.Provider, l.provider.Name())
		}

		// What the provider received, without the tools, is exactly the span's options.
		received := mainModelCalls(l)[0].Options
		received.Tools = nil
		raw, err := kernel.EncodeJSON(received)
		if err != nil {
			t.Fatal(err)
		}
		var providerOptions map[string]any
		if err := json.Unmarshal(raw, &providerOptions); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(cs.Options, providerOptions) {
			t.Fatalf("span options = %v, provider options without tools = %v", cs.Options, providerOptions)
		}

		model := attrsJSON(t, l.spans.ended("model-call")[0])
		if got := model["inputMessageCount"]; got != float64(cs.MessageCount) {
			t.Fatalf("model-call inputMessageCount = %v, want %d", got, cs.MessageCount)
		}
		if !reflect.DeepEqual(model["options"], any(cs.Options)) {
			t.Fatalf("model-call options = %v, want the span's %v", model["options"], cs.Options)
		}
		if _, ok := model["inputMessages"]; ok {
			t.Fatal("the model-call span lists its input messages")
		}
	})

	t.Run("stream(): model-call span end records inputTokens/outputTokens from the done chunk", func(t *testing.T) {
		l := newLoopCore(t, loopOptions{}, traceTurns(false, 1))
		defer l.dispose(t)

		if _, err := drainStream(l.stream(t.Context(), "hello")); err != nil {
			t.Fatalf("ChatStream: %v", err)
		}

		model := attrsJSON(t, l.spans.ended("model-call")[0])
		if model["inputTokens"] != 100.0 || model["outputTokens"] != 50.0 {
			t.Fatalf("model-call tokens = %v in, %v out, want 100 and 50", model["inputTokens"], model["outputTokens"])
		}
	})

	t.Run("memoryInjection is none, memoryBlock is undefined, memoryRecordIds is empty", func(t *testing.T) {
		l := newLoopCore(t, loopOptions{}, traceTurns(false, 1))
		defer l.dispose(t)

		if _, err := l.chat(t.Context(), "hello"); err != nil {
			t.Fatalf("Chat: %v", err)
		}

		cs := contextSpans(t, l)[0]
		if cs.MemoryInjection != "none" {
			t.Fatalf("memoryInjection = %q, want none", cs.MemoryInjection)
		}
		if cs.MemoryBlock != nil {
			t.Fatalf("memoryBlock = %q, want it absent", *cs.MemoryBlock)
		}
		if cs.MemoryRecordIDs == nil || len(cs.MemoryRecordIDs) != 0 {
			t.Fatalf("memoryRecordIds = %#v, want an empty list", cs.MemoryRecordIDs)
		}
	})
}
