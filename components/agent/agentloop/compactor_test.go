package agentloop_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"testing"

	"github.com/Phong95/aec-agent-go/aecmock"
	"github.com/Phong95/aec-agent-go/components/agent/agentloop"
	"github.com/Phong95/aec-agent-go/components/agent/profile"
	"github.com/Phong95/aec-agent-go/components/llmhub"
	"github.com/Phong95/aec-agent-go/components/toolhub"
	"github.com/Phong95/aec-agent-go/internal/jsonx"
	"github.com/Phong95/aec-agent-go/internal/wiretest"
	"github.com/Phong95/aec-agent-go/kernel"
)

// x3Profile is the compaction setup of trace case X3: a window of 100
// tokens at threshold 0.5 compacts at 50 tokens, the protected head is one message
// and the tail one message.
func x3Profile() *profile.AgentProfile {
	p := testProfile("test-agent")
	p.Compactor.ContextWindowTokens = 100
	p.Compactor.Threshold = 0.5
	p.Compactor.ProtectFirstN = 1
	p.Compactor.ProtectLastN = 1
	p.Compactor.TailTargetRatio = 0.01
	return &p
}

// mainCalls are the model calls that are not summary calls (a summary call is the
// one whose options carry a system prompt).
func mainCalls(calls []aecmock.MockCall) []aecmock.MockCall {
	var out []aecmock.MockCall
	for _, c := range calls {
		if c.Options.SystemPrompt == "" {
			out = append(out, c)
		}
	}
	return out
}

func contents(msgs []kernel.Message) []string {
	out := make([]string, len(msgs))
	for i, m := range msgs {
		out[i] = m.Content
	}
	return out
}

// TestRun_CompactsAtThreshold is success criterion 4 end to end: the usage of the
// first turn is written to the session, puts the second turn past the threshold (the
// compactor reads the session's current usage), and the second turn is compacted to
// [head, summary, tail] and persisted. The second run's own usage counts only its
// own model call.
func TestRun_CompactsAtThreshold(t *testing.T) {
	usage := &kernel.TokenUsage{InputTokens: 100, OutputTokens: 50}
	r := newRig(t, rigOptions{prof: x3Profile()},
		aecmock.ScriptedTurn{Content: "first answer", Usage: usage},
		aecmock.ScriptedTurn{Content: "second answer", Usage: usage},
	)
	r.models.With(aecmock.WithSummaryReply("SUMMARY-TEXT"))
	ctx := t.Context()

	// Run 1: nothing to compact yet, no summary call.
	if _, err := r.run(t, "first question"); err != nil {
		t.Fatalf("run 1: %v", err)
	}
	if got := len(r.models.Calls()); got != 1 {
		t.Fatalf("run 1 made %d model calls, want 1 (no summary call below the threshold)", got)
	}
	s, _ := r.conv.Session()
	if s.CurrentTokenUsage == nil || s.CurrentTokenUsage.InputTokens != 100 || s.CurrentTokenUsage.OutputTokens != 50 {
		t.Fatalf("session usage after run 1 = %+v, want 100 in and 50 out", s.CurrentTokenUsage)
	}
	if s.CounterCalibration == nil || s.CounterCalibration.Samples != 1 {
		t.Fatalf("session calibration after run 1 = %+v, want one sample", s.CounterCalibration)
	}
	if got := s.CounterCalibration.CharsPerToken; got < 2 || got > 8 {
		t.Fatalf("calibrated ratio %v is outside 2..8", got)
	}
	if hist, err := r.hub.CompactedMessages.Get(ctx, s.ID); err != nil || len(hist) != 0 {
		t.Fatalf("a compacted set exists after run 1: %v, %v", hist, err)
	}

	// Run 2: the session reports 150 tokens, past the threshold of 50.
	res, err := r.run(t, "second question")
	if err != nil {
		t.Fatalf("run 2: %v", err)
	}
	if res.Usage.InputTokens != 100 || res.Usage.OutputTokens != 50 {
		t.Fatalf("run 2 usage = %+v, want its own 100/50 only (TS adds the session's 100/50)", res.Usage)
	}
	calls := r.models.Calls()
	if len(calls) != 3 {
		t.Fatalf("model calls = %d, want main, summary, main", len(calls))
	}
	summary := calls[1]
	if summary.Options.SystemPrompt == "" || len(summary.Messages) != 1 || summary.Messages[0].Content != "[ASSISTANT] first answer" {
		t.Fatalf("summary call = %+v, want one user message holding the middle", summary)
	}
	mains := mainCalls(calls)
	input := contents(mains[1].Messages)
	if want := []string{"first question", "SUMMARY-TEXT", "second question"}; !slices.Equal(input, want) {
		t.Fatalf("model input of run 2 = %v, want %v", input, want)
	}
	if mains[1].Messages[1].Role != kernel.RoleAssistant {
		t.Fatalf("the summary message is %s, want assistant", mains[1].Messages[1].Role)
	}

	// The compacted working set was persisted, and the answer joined it.
	stored, err := r.hub.CompactedMessages.Get(ctx, s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"first question", "SUMMARY-TEXT", "second question", "second answer"}; !slices.Equal(contents(stored), want) {
		t.Fatalf("stored compacted messages = %v, want %v", contents(stored), want)
	}
	// The full history is untouched.
	if got := len(r.conv.Messages()); got != 4 {
		t.Fatalf("history has %d messages, want 4", got)
	}

	// The compaction span reports the real values.
	end, ok := r.sink.endAttrsAt("compaction", 1)
	if !ok {
		t.Fatal("no second compaction span")
	}
	if v, _ := end.Get("triggered"); v != true {
		t.Fatalf("compaction triggered = %v", v)
	}
	if v, _ := end.Get("messageCountAfter"); v != 3 {
		t.Fatalf("messageCountAfter = %v, want 3", v)
	}
}

// failureLog is a logger whose output a test can read.
func failureLog() (*slog.Logger, *bytes.Buffer) {
	var buf bytes.Buffer
	return slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})), &buf
}

// TestSummarizerFailureSkipsCompaction: a summary that cannot be produced skips the
// compaction, the run carries on with the uncompacted messages, and the warning
// names the agent and the error but never the conversation.
func TestSummarizerFailureSkipsCompaction(t *testing.T) {
	usage := &kernel.TokenUsage{InputTokens: 100, OutputTokens: 50}
	lg, buf := failureLog()
	r := newRig(t, rigOptions{prof: x3Profile(), loopOpts: []agentloop.Option{agentloop.WithLogger(lg)}},
		aecmock.ScriptedTurn{Content: "SECRET-ANSWER", Usage: usage},
		aecmock.ScriptedTurn{Content: "second answer", Usage: usage},
	)
	r.models.With(aecmock.WithSummaryReply("")) // an empty summary is a failure
	if _, err := r.run(t, "SECRET-QUESTION"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.run(t, "second question"); err != nil {
		t.Fatalf("a failed summary must not fail the run: %v", err)
	}

	mains := mainCalls(r.models.Calls())
	if want := []string{"SECRET-QUESTION", "SECRET-ANSWER", "second question"}; !slices.Equal(contents(mains[1].Messages), want) {
		t.Fatalf("model input = %v, want the uncompacted messages", contents(mains[1].Messages))
	}
	s, _ := r.conv.Session()
	if stored, err := r.hub.CompactedMessages.Get(t.Context(), s.ID); err != nil || len(stored) != 0 {
		t.Fatalf("a compacted set was persisted: %v, %v", stored, err)
	}

	logged := buf.String()
	if got := strings.Count(logged, "level=WARN"); got != 1 {
		t.Fatalf("%d warnings logged, want 1:\n%s", got, logged)
	}
	for _, want := range []string{"agentId=test-agent", "Summary model returned an empty summary"} {
		if !strings.Contains(logged, want) {
			t.Fatalf("the warning lacks %q:\n%s", want, logged)
		}
	}
	for _, secret := range []string{"SECRET-QUESTION", "SECRET-ANSWER", "second question"} {
		if strings.Contains(logged, secret) {
			t.Fatalf("the warning leaks conversation content %q:\n%s", secret, logged)
		}
	}
}

// A model error of the summary call is skipped the same way.
func TestSummarizerModelErrorSkipsCompaction(t *testing.T) {
	usage := &kernel.TokenUsage{InputTokens: 100, OutputTokens: 50}
	lg, buf := failureLog()
	r := newRig(t, rigOptions{prof: x3Profile(), loopOpts: []agentloop.Option{agentloop.WithLogger(lg)}},
		aecmock.ScriptedTurn{Content: "first", Usage: usage},
		aecmock.ScriptedTurn{Content: "second", Usage: usage},
	)
	r.models.With(aecmock.WithSummaryError(kernel.NewModelError("summary model down", kernel.ModelCodeProvider, false)))
	if _, err := r.run(t, "q1"); err != nil {
		t.Fatal(err)
	}
	res, err := r.run(t, "q2")
	if err != nil || res.Message.Content != "second" {
		t.Fatalf("run 2 = %+v, %v", res, err)
	}
	if !strings.Contains(buf.String(), "summary model down") {
		t.Fatalf("the model error is not logged:\n%s", buf.String())
	}
}

// compactorEntry is one case of the recorded fixture agentloop-compactor.json: a
// Compactor with the Counter at its default ratio and a summarize stub.
type compactorEntry struct {
	Name      string                  `json:"name"`
	Config    profile.CompactorConfig `json:"config"`
	Messages  []kernel.Message        `json:"messages"`
	Usage     kernel.TokenUsage       `json:"usage"`
	Summarize *struct {
		Throws string `json:"throws"`
	} `json:"summarize"`
	Result struct {
		Compacted     bool             `json:"compacted"`
		Messages      []kernel.Message `json:"messages"`
		SummarySource string           `json:"summarySource"`
		TailTokens    int              `json:"tailTokens"`
		SummaryIndex  *int             `json:"summaryIndex"`
	} `json:"result"`
	SameMessages   bool `json:"sameMessages"`
	SummarizeCalls []struct {
		Messages        []kernel.Message `json:"messages"`
		MaxOutputTokens int              `json:"maxOutputTokens"`
		Model           *string          `json:"model"`
	} `json:"summarizeCalls"`
}

// encodeMessages renders messages as wire bytes: the comparison that tells nil
// from empty lists, as the wire does.
func encodeMessages(t *testing.T, msgs []kernel.Message) string {
	t.Helper()
	if msgs == nil {
		msgs = []kernel.Message{}
	}
	b, err := kernel.EncodeJSON(msgs)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// sameSlice reports whether a and b are the same slice (same first element, same length).
func sameSlice(a, b []kernel.Message) bool {
	return len(a) == len(b) && (len(a) == 0 || &a[0] == &b[0])
}

// TestCompactorMatchesGolden replays agentloop-compactor.json: for each case the result
// (messages, summary source, tail tokens, summary index), whether the input slice
// itself was returned, and what the summarize callback was asked.
func TestCompactorMatchesGolden(t *testing.T) {
	var f struct {
		Entries []compactorEntry `json:"entries"`
	}
	if err := jsonx.UnmarshalStrict(wiretest.Load(t, "agentloop-compactor.json"), &f); err != nil {
		t.Fatalf("decode fixture: %v", err)
	}
	if len(f.Entries) < 14 {
		t.Fatalf("the corpus has %d entries, want at least 14", len(f.Entries))
	}
	for _, e := range f.Entries {
		t.Run(e.Name, func(t *testing.T) {
			lg, _ := failureLog()
			c := agentloop.NewCompactor(e.Config)
			agentloop.SetCompactorLogger(c, lg)

			type call struct {
				msgs  []kernel.Message
				max   int
				model string
			}
			var calls []call
			summarize := func(_ context.Context, msgs []kernel.Message, maxOut int, model string) (string, error) {
				calls = append(calls, call{msgs: slices.Clone(msgs), max: maxOut, model: model})
				if e.Summarize != nil {
					return "", errors.New(e.Summarize.Throws)
				}
				return "CORPUS-SUMMARY", nil
			}
			in := slices.Clone(e.Messages)
			got := c.CompactIfNeeded(t.Context(), in, e.Usage, agentloop.NewCounter().Count, summarize)

			want := e.Result
			if got.Compacted != want.Compacted || got.SummarySource != want.SummarySource || got.TailTokens != want.TailTokens {
				t.Fatalf("result = {compacted %v, source %q, tail %d}, TS = {%v, %q, %d}", got.Compacted, got.SummarySource, got.TailTokens, want.Compacted, want.SummarySource, want.TailTokens)
			}
			if want.SummaryIndex != nil && got.SummaryIndex != *want.SummaryIndex {
				t.Fatalf("summaryIndex = %d, TS = %d", got.SummaryIndex, *want.SummaryIndex)
			}
			if g, w := encodeMessages(t, got.Messages), encodeMessages(t, want.Messages); g != w {
				t.Fatalf("messages differ from TS\n got: %s\nwant: %s", g, w)
			}
			if same := sameSlice(got.Messages, in); same != e.SameMessages {
				t.Fatalf("returned the input slice = %v, TS = %v", same, e.SameMessages)
			}
			if len(calls) != len(e.SummarizeCalls) {
				t.Fatalf("summarize called %d times, TS %d", len(calls), len(e.SummarizeCalls))
			}
			for i, w := range e.SummarizeCalls {
				g := calls[i]
				wantModel := ""
				if w.Model != nil {
					wantModel = *w.Model
				}
				if g.max != w.MaxOutputTokens || g.model != wantModel {
					t.Fatalf("summarize call %d: max %d model %q, TS max %d model %q", i, g.max, g.model, w.MaxOutputTokens, wantModel)
				}
				if gm, wm := encodeMessages(t, g.msgs), encodeMessages(t, w.Messages); gm != wm {
					t.Fatalf("summarize call %d messages differ from TS\n got: %s\nwant: %s", i, gm, wm)
				}
			}
		})
	}
}

// pairCount is the counter of the tool-pair-compaction cases (T1 to T3): every
// message costs 10 tokens, except one that carries a long tool result, which costs
// 5000.
func pairCount(m kernel.Message) int {
	for _, r := range m.ToolResults {
		if kernel.UTF16Len(r.Content) > 100 {
			return 5000
		}
	}
	return 10
}

// pairCompactor is the compactor of those cases, with the given lean tail budget.
func pairCompactor(budget int) *agentloop.Compactor {
	return agentloop.NewCompactor(profile.CompactorConfig{
		ContextWindowTokens:      1000,
		Threshold:                0.5,
		ProtectFirstN:            1,
		OldToolResultMinTokens:   2000,
		TailTargetRatio:          0.2,
		LeanTailTokenBudget:      &budget,
		ProtectLastN:             0,
		SummarizeMaxOutputTokens: 100,
	})
}

var overThreshold = kernel.TokenUsage{InputTokens: 900}

func callMsg(id string) kernel.Message {
	return kernel.Message{Role: kernel.RoleAssistant, ToolCalls: []kernel.ToolCall{{ID: id, Name: "write_file", Arguments: kernel.Arguments(`{"path":"` + id + `.html"}`)}}}
}

func resultMsg(id, content string) kernel.Message {
	return kernel.Message{Role: kernel.RoleAssistant, ToolResults: []kernel.ToolResult{{ToolCallID: id, Content: content}}}
}

func fixedSummary(context.Context, []kernel.Message, int, string) (string, error) {
	return "summary", nil
}

// assertWellPaired fails when a tool result has no earlier call.
func assertWellPaired(t *testing.T, msgs []kernel.Message) {
	t.Helper()
	called := map[string]bool{}
	for _, m := range msgs {
		for _, r := range m.ToolResults {
			if !called[r.ToolCallID] {
				t.Fatalf("result %s has no earlier call", r.ToolCallID)
			}
		}
		for _, c := range m.ToolCalls {
			called[c.ID] = true
		}
	}
}

// T1: the tail is widened instead of cutting between a call and its result.
func TestCompactor_WidensTailForToolPair(t *testing.T) {
	msgs := []kernel.Message{
		{Role: kernel.RoleUser, Content: "make a landing page"},
		callMsg("a"), resultMsg("a", "ok"),
		callMsg("b"), resultMsg("b", "ok"),
		{Role: kernel.RoleAssistant, Content: "done"},
		{Role: kernel.RoleUser, Content: "and another"},
		callMsg("c"), resultMsg("c", "ok"),
	}
	out := pairCompactor(10).CompactIfNeeded(t.Context(), msgs, overThreshold, pairCount, fixedSummary)
	if !out.Compacted {
		t.Fatal("not compacted")
	}
	tail := out.Messages[2:] // [head, summary, ...tail]
	if len(tail) != 2 || tail[0].ToolCalls[0].ID != "c" || tail[1].ToolResults[0].ToolCallID != "c" {
		t.Fatalf("tail = %+v, want the call and the result of c", tail)
	}
	assertWellPaired(t, out.Messages)
}

// T2: nothing is compacted when keeping the pair leaves nothing to summarise, and
// the input slice itself is returned.
func TestCompactor_NoCompactionWhenMiddleEmpty(t *testing.T) {
	msgs := []kernel.Message{{Role: kernel.RoleUser, Content: "hi"}, callMsg("a"), resultMsg("a", "ok")}
	out := pairCompactor(10).CompactIfNeeded(t.Context(), msgs, overThreshold, pairCount, fixedSummary)
	if out.Compacted {
		t.Fatal("compacted")
	}
	if !sameSlice(out.Messages, msgs) {
		t.Fatal("the result is not the input slice")
	}
}

// T3: a large old tool result carried on ToolResults is cleared before summarising,
// on a copy: the caller's message is not touched.
func TestCompactor_ClearsOldToolResults(t *testing.T) {
	big := strings.Repeat("x", 500)
	msgs := []kernel.Message{
		{Role: kernel.RoleUser, Content: "start"},
		callMsg("a"), resultMsg("a", big),
		{Role: kernel.RoleAssistant, Content: "done"},
		{Role: kernel.RoleUser, Content: "next"},
	}
	var summarised []kernel.Message
	out := pairCompactor(10).CompactIfNeeded(t.Context(), msgs, overThreshold, pairCount, func(_ context.Context, m []kernel.Message, _ int, _ string) (string, error) {
		summarised = slices.Clone(m)
		return "summary", nil
	})
	if !out.Compacted {
		t.Fatal("not compacted")
	}
	var pruned *kernel.Message
	for i := range summarised {
		if len(summarised[i].ToolResults) > 0 {
			pruned = &summarised[i]
		}
	}
	if pruned == nil || pruned.ToolResults[0].Content != "[Old tool output cleared to save context space]" {
		t.Fatalf("the summarizer saw %+v, want the tool result cleared", summarised)
	}
	if msgs[2].ToolResults[0].Content != big {
		t.Fatal("the caller's message was changed: pruning must work on copies")
	}
}

// A legacy tool message keeps its result in Content, which is the one cleared.
func TestCompactor_ClearsALegacyToolMessage(t *testing.T) {
	msgs := []kernel.Message{
		{Role: kernel.RoleUser, Content: "start"},
		{Role: kernel.RoleTool, Content: strings.Repeat("y", 500)},
		{Role: kernel.RoleAssistant, Content: "done"},
		{Role: kernel.RoleUser, Content: "next"},
	}
	count := func(m kernel.Message) int {
		if m.Role == kernel.RoleTool && len(m.Content) > 100 {
			return 5000
		}
		return 10
	}
	var summarised []kernel.Message
	pairCompactor(10).CompactIfNeeded(t.Context(), msgs, overThreshold, count, func(_ context.Context, m []kernel.Message, _ int, _ string) (string, error) {
		summarised = slices.Clone(m)
		return "summary", nil
	})
	if len(summarised) != 2 || summarised[0].Content != "[Old tool output cleared to save context space]" {
		t.Fatalf("summarizer input = %+v", summarised)
	}
}

// A nil summarize or a nil count do not panic: the first skips, the second counts
// with a default counter.
func TestCompactor_NilCallbacks(t *testing.T) {
	msgs := []kernel.Message{{Role: kernel.RoleUser, Content: "a"}, {Role: kernel.RoleAssistant, Content: "b"}, {Role: kernel.RoleUser, Content: "c"}}
	c := agentloop.NewCompactor(profile.CompactorConfig{ContextWindowTokens: 10, Threshold: 0.5, ProtectFirstN: 1, ProtectLastN: 1, TailTargetRatio: 0.01})
	if out := c.CompactIfNeeded(t.Context(), msgs, overThreshold, nil, nil); out.Compacted || !sameSlice(out.Messages, msgs) {
		t.Fatalf("nil summarize compacted: %+v", out)
	}
	if out := c.CompactIfNeeded(t.Context(), msgs, overThreshold, nil, fixedSummary); !out.Compacted || out.SummaryIndex != 1 {
		t.Fatalf("nil count: %+v", out)
	}
}

// TestSecondCompactionKeepsOneSystemMessage: with protectFirstN 2 the first compaction
// stores the compacted working set without the system message that the context manager
// put in front of the history, so the next run's fresh one is the only one and the
// third model input starts with exactly one system message. Storing it would make the
// third model input start with two (the recorded fixture transcript shows that; checked
// below).
func TestSecondCompactionKeepsOneSystemMessage(t *testing.T) {
	prof := x3Profile()
	prof.Compactor.ProtectFirstN = 2
	usage := &kernel.TokenUsage{InputTokens: 100, OutputTokens: 50}
	r := newRig(t, rigOptions{prof: prof, context: aecmock.NewPromptContext(*prof)},
		aecmock.ScriptedTurn{Content: "first answer", Usage: usage},
		aecmock.ScriptedTurn{Content: "second answer", Usage: usage},
		aecmock.ScriptedTurn{Content: "third answer", Usage: usage},
	)
	r.models.With(aecmock.WithSummaryReply("SUMMARY-TEXT"))
	for _, q := range []string{"first question", "second question", "third question"} {
		if _, err := r.run(t, q); err != nil {
			t.Fatalf("run %q: %v", q, err)
		}
		// The stored working set never holds a system message.
		s, _ := r.conv.Session()
		stored, err := r.hub.CompactedMessages.Get(t.Context(), s.ID)
		if err != nil {
			t.Fatal(err)
		}
		for i, m := range stored {
			if m.Role == kernel.RoleSystem {
				t.Fatalf("after %q the stored working set holds a system message at %d", q, i)
			}
		}
	}
	mains := mainCalls(r.models.Calls())
	if len(mains) != 3 {
		t.Fatalf("%d main calls, want 3", len(mains))
	}
	for n, call := range mains {
		systems := 0
		for _, m := range call.Messages {
			if m.Role == kernel.RoleSystem {
				systems++
			}
		}
		if systems != 1 || call.Messages[0].Role != kernel.RoleSystem {
			t.Fatalf("model input %d = %v, want exactly one system message, first", n+1, contents(call.Messages))
		}
	}
	got := contents(mains[2].Messages)
	if want := []string{prof.SystemPrompt, "first question", "SUMMARY-TEXT", "third question"}; !slices.Equal(got, want) {
		t.Fatalf("third model input = %v, want %v", got, want)
	}

	// The recorded fixture transcript did not: its third context-assembly span lists
	// two system messages first (the behaviour this test guards against).
	var fx struct {
		Transcript struct {
			Spans []struct {
				Phase      string `json:"phase"`
				Name       string `json:"name"`
				Attributes struct {
					Messages []struct {
						Role string `json:"role"`
					} `json:"messages"`
				} `json:"attributes"`
			} `json:"spans"`
		} `json:"transcript"`
	}
	if err := json.Unmarshal(wiretest.Load(t, "agentloop-run-second-compaction.json"), &fx); err != nil {
		t.Fatal(err)
	}
	ends := 0
	for _, s := range fx.Transcript.Spans {
		if s.Phase != "end" || s.Name != "context-assembly" {
			continue
		}
		if ends++; ends != 3 {
			continue
		}
		list := s.Attributes.Messages
		if len(list) < 2 {
			t.Fatalf("the TS context-assembly span lists %v", list)
		}
		for i := range 2 {
			if list[i].Role != "system" {
				t.Fatalf("TS message %d of the third model input is %v, want system", i, list[i].Role)
			}
		}
	}
	if ends != 3 {
		t.Fatalf("the TS transcript has %d context-assembly spans, want 3", ends)
	}
}

// The compactor reads the latest usage, and a zero compactor config is disabled.

// TestRun_CompactorReadsTheLatestUsage: the compactor is fed the context window
// size the latest response reported, not a running total. Three calls of 20/10 within
// one run sum to 60, past the threshold of 50, but the window is 30: no compaction.
func TestRun_CompactorReadsTheLatestUsage(t *testing.T) {
	usage := &kernel.TokenUsage{InputTokens: 20, OutputTokens: 10}
	echo := constTool("echo", toolhub.Output{Content: "ok"})
	turn := func(id string) aecmock.ScriptedTurn {
		tt := toolTurn(callOf(id, "echo", "{}"))
		tt.Usage = usage
		return tt
	}
	rg := newRig(t, rigOptions{prof: x3Profile(), handlers: []toolhub.Handler{echo}},
		turn("tc-1"), turn("tc-2"), aecmock.ScriptedTurn{Content: "done", Usage: usage})
	rg.models.With(aecmock.WithSummaryReply("SUMMARY-TEXT"))
	res, err := rg.run(t, "go")
	if err != nil {
		t.Fatal(err)
	}
	if res.Usage.InputTokens != 60 || res.Usage.OutputTokens != 30 {
		t.Fatalf("usage = %+v, want the three calls summed", res.Usage)
	}
	if got := len(rg.models.Calls()); got != 3 {
		t.Fatalf("%d model calls, want 3 and no summary call", got)
	}
	for n := range 3 {
		attrs, ok := rg.sink.endAttrsAt("compaction", n)
		if !ok {
			t.Fatalf("no compaction span %d", n)
		}
		if v, _ := attrs.Get("triggered"); v != false {
			t.Fatalf("compaction %d triggered", n)
		}
	}
}

// TestCompactor_ZeroConfigIsDisabled: a config without a positive context window or
// threshold never compacts, whatever the usage.
func TestCompactor_ZeroConfigIsDisabled(t *testing.T) {
	msgs := []kernel.Message{
		{Role: kernel.RoleSystem, Content: "sys"},
		{Role: kernel.RoleUser, Content: "one"},
		{Role: kernel.RoleAssistant, Content: "two"},
		{Role: kernel.RoleUser, Content: "three"},
	}
	huge := kernel.TokenUsage{InputTokens: 1_000_000, OutputTokens: 1_000_000}
	zeroWindow := profile.DefaultCompactorConfig()
	zeroWindow.ContextWindowTokens = 0
	zeroThreshold := profile.DefaultCompactorConfig()
	zeroThreshold.Threshold = 0
	negativeThreshold := profile.DefaultCompactorConfig()
	negativeThreshold.Threshold = -1
	for name, cfg := range map[string]profile.CompactorConfig{
		"zero config":        {},
		"zero window":        zeroWindow,
		"zero threshold":     zeroThreshold,
		"negative threshold": negativeThreshold,
	} {
		t.Run(name, func(t *testing.T) {
			called := false
			summarize := func(context.Context, []kernel.Message, int, string) (string, error) {
				called = true
				return "S", nil
			}
			out := agentloop.NewCompactor(cfg).CompactIfNeeded(t.Context(), msgs, huge, nil, summarize)
			if out.Compacted || called || len(out.Messages) != len(msgs) {
				t.Fatalf("result = %+v, summarize called %v: want no compaction", out, called)
			}
		})
	}
	t.Run("default config compacts past the threshold", func(t *testing.T) {
		cfg := profile.DefaultCompactorConfig()
		cfg.ProtectFirstN, cfg.ProtectLastN, cfg.TailTargetRatio = 1, 1, 0.000001
		summarize := func(context.Context, []kernel.Message, int, string) (string, error) { return "S", nil }
		out := agentloop.NewCompactor(cfg).CompactIfNeeded(t.Context(), msgs, huge, nil, summarize)
		if !out.Compacted {
			t.Fatalf("result = %+v, want a compaction", out)
		}
	})
}

// TestRun_SessionContextWindowOverride: a session's ContextWindowTokens replaces the
// agent's window for its runs. With X3's window of 100 the second turn (150 tokens)
// compacts; raised to 1000 by the session (threshold 500) it does not.
func TestRun_SessionContextWindowOverride(t *testing.T) {
	usage := &kernel.TokenUsage{InputTokens: 100, OutputTokens: 50}
	r := newRig(t, rigOptions{prof: x3Profile()},
		aecmock.ScriptedTurn{Content: "first answer", Usage: usage},
		aecmock.ScriptedTurn{Content: "second answer", Usage: usage},
	)
	r.models.With(aecmock.WithSummaryReply("SUMMARY-TEXT"))

	if _, err := r.run(t, "first question"); err != nil {
		t.Fatalf("run 1: %v", err)
	}
	conv := &windowConversation{Conversation: r.conv, window: 1000}
	if _, err := r.loop.Run(t.Context(), "second question", "s-1", llmhub.ModelOptions{}, agentloop.RunOptions{Conversation: conv}); err != nil {
		t.Fatalf("run 2: %v", err)
	}
	if got := len(r.models.Calls()); got != 2 {
		t.Fatalf("model calls = %d, want 2 (no summary call under the session's window)", got)
	}
	if got := len(mainCalls(r.models.Calls())[1].Messages); got != 3 {
		t.Fatalf("run 2 sent %d messages, want the full history of 3", got)
	}
}

// windowConversation is a conversation whose session carries a context window override.
type windowConversation struct {
	*aecmock.Conversation
	window int
}

func (c *windowConversation) Session() (kernel.Session, bool) {
	s, ok := c.Conversation.Session()
	s.ContextWindowTokens = c.window
	return s, ok
}
