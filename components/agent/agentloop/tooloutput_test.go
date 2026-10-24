package agentloop_test

import (
	"context"
	"strings"
	"testing"

	"github.com/Phong95/aec-agent-go/aecmock"
	"github.com/Phong95/aec-agent-go/components/agent/agentloop"
	"github.com/Phong95/aec-agent-go/components/agent/profile"
	"github.com/Phong95/aec-agent-go/components/sandboxhub"
	"github.com/Phong95/aec-agent-go/components/toolhub"
	"github.com/Phong95/aec-agent-go/kernel"
)

// A-129: keep large tool payloads out of later model requests - calls whose content
// was written to a file carry a reference to it, and old large tool results are
// cleared in batches once they pass ProactivePruneTokens. Same cases as core/ts
// tests/unit/context-token-optimisation.test.ts.

func a129Config() profile.CompactorConfig {
	return profile.CompactorConfig{
		ContextWindowTokens:      1_000_000,
		Threshold:                0.8,
		MinTailUserMessages:      1,
		ProtectFirstN:            1,
		ProactivePruneTokens:     8_000,
		OldToolResultMinTokens:   1_000,
		KeepToolResults:          intPtr(1),
		TailTargetRatio:          0.2,
		ProtectLastN:             4,
		SummarizeMaxOutputTokens: 100,
	}
}

func a129User(content string) kernel.Message {
	return kernel.Message{Role: kernel.RoleUser, Content: content}
}

func a129Call(id string) kernel.Message {
	return kernel.Message{Role: kernel.RoleAssistant, ToolCalls: []kernel.ToolCall{
		{ID: id, Name: "run_code", Arguments: kernel.Arguments(`{"code":"` + id + `"}`)},
	}}
}

func a129Result(id, content string) kernel.Message {
	return kernel.Message{Role: kernel.RoleAssistant, ToolResults: []kernel.ToolResult{{ToolCallID: id, Content: content}}}
}

// a129Count is 1 token per character, so the arithmetic stays readable.
func a129Count(m kernel.Message) int {
	n := len(m.Content)
	for _, r := range m.ToolResults {
		n += len(r.Content)
	}
	return n
}

func a129History() []kernel.Message {
	done := kernel.Message{Role: kernel.RoleAssistant, Content: "done"}
	big := strings.Repeat("x", 5_000)
	return []kernel.Message{
		a129User("one"), a129Call("a"), a129Result("a", big), done,
		a129User("two"), a129Call("b"), a129Result("b", big), done,
		a129User("three"), a129Call("c"), a129Result("c", big), done,
		a129User("four"), a129Call("d"), a129Result("d", big),
	}
}

func TestClearOldToolResultsOffAtZero(t *testing.T) {
	cfg := a129Config()
	cfg.ProactivePruneTokens = 0
	msgs := a129History()
	out := agentloop.NewCompactor(cfg).ClearOldToolResults(msgs, a129Count)
	if out.Cleared != 0 || &out.Messages[0] != &msgs[0] {
		t.Fatalf("cleared %d, want none and the input itself", out.Cleared)
	}
}

func TestClearOldToolResultsClearsOldestToLowMark(t *testing.T) {
	msgs := a129History()
	// Before the current turn ("four"): a, b, c. c is kept (KeepToolResults 1); a + b = 10k > 8k.
	out := agentloop.NewCompactor(a129Config()).ClearOldToolResults(msgs, a129Count)
	if out.Cleared != 2 || out.ClearedTokens != 10_000 {
		t.Fatalf("cleared %d (%d tokens), want 2 (10000)", out.Cleared, out.ClearedTokens)
	}
	note := agentloop.ClearedToolResultNote("run_code", 5_000)
	big := strings.Repeat("x", 5_000)
	for at, want := range map[int]string{2: note, 6: note, 10: big, 14: big} {
		if got := out.Messages[at].ToolResults[0].Content; got != want {
			t.Errorf("message %d = %.40q, want %.40q", at, got, want)
		}
	}
	if out.Messages[2].ToolResults[0].ToolCallID != "a" {
		t.Errorf("the cleared result lost its call id")
	}
	if msgs[2].ToolResults[0].Content != big {
		t.Errorf("the input was changed")
	}
}

func TestClearOldToolResultsStopsUntilHighMarkAgain(t *testing.T) {
	cfg := a129Config()
	cfg.ProactivePruneTokens = 9_000
	cfg.KeepToolResults = intPtr(0)
	c := agentloop.NewCompactor(cfg)
	// a + b + c = 15k > 9k. The low mark is 4.5k: after a and b, 5k is still above it, so c goes too.
	first := c.ClearOldToolResults(a129History(), a129Count)
	if first.Cleared != 3 {
		t.Fatalf("cleared %d, want 3", first.Cleared)
	}
	again := c.ClearOldToolResults(first.Messages, a129Count)
	if again.Cleared != 0 || &again.Messages[0] != &first.Messages[0] {
		t.Fatalf("cleared %d again, want none and the input itself", again.Cleared)
	}
}

func TestClearOldToolResultsNegativeKeepUsesDefault(t *testing.T) {
	cfg := a129Config()
	cfg.KeepToolResults = intPtr(-1)
	// a, b and c are the 3 latest before the current turn, so nothing is clearable.
	if out := agentloop.NewCompactor(cfg).ClearOldToolResults(a129History(), a129Count); out.Cleared != 0 {
		t.Fatalf("cleared %d, want 0", out.Cleared)
	}
}

func TestClearOldToolResultsKeepsSmallResults(t *testing.T) {
	cfg := a129Config()
	cfg.ProactivePruneTokens = 100
	cfg.KeepToolResults = intPtr(0)
	msgs := []kernel.Message{a129User("one"), a129Call("a"), a129Result("a", strings.Repeat("x", 500)), a129User("two")}
	if out := agentloop.NewCompactor(cfg).ClearOldToolResults(msgs, a129Count); out.Cleared != 0 {
		t.Fatalf("cleared %d, want 0", out.Cleared)
	}
}

func TestWithCompactArgumentsSwapsSuccessfulOnes(t *testing.T) {
	msgs := []kernel.Message{
		a129User("save it"),
		{Role: kernel.RoleAssistant, ToolCalls: []kernel.ToolCall{{ID: "a", Name: "write_file", Arguments: kernel.Arguments(`{"content":"LONG"}`)}}},
		{Role: kernel.RoleAssistant, ToolResults: []kernel.ToolResult{{ToolCallID: "a", Content: "ok", CompactArguments: kernel.Arguments(`{"content":"[ref]"}`)}}},
	}
	out := agentloop.WithCompactArguments(msgs)
	if got := out[1].ToolCalls[0]; got.ID != "a" || string(got.Arguments) != `{"content":"[ref]"}` {
		t.Fatalf("call = %s %s, want a {\"content\":\"[ref]\"}", got.ID, got.Arguments)
	}
	if string(msgs[1].ToolCalls[0].Arguments) != `{"content":"LONG"}` {
		t.Fatalf("the input was changed")
	}
}

func TestWithCompactArgumentsIgnoresErrors(t *testing.T) {
	isErr := true
	msgs := []kernel.Message{
		{Role: kernel.RoleAssistant, ToolCalls: []kernel.ToolCall{{ID: "a", Name: "write_file", Arguments: kernel.Arguments(`{"content":"LONG"}`)}}},
		{Role: kernel.RoleAssistant, ToolResults: []kernel.ToolResult{{ToolCallID: "a", Content: "failed", IsError: &isErr, CompactArguments: kernel.Arguments(`{"content":"[ref]"}`)}}},
	}
	if out := agentloop.WithCompactArguments(msgs); &out[0] != &msgs[0] {
		t.Fatalf("want the input itself")
	}
}

// bigTool answers every call with 20,000 characters and compact arguments.
func bigTool() *fakeTool {
	return newFakeTool("big_tool", func(context.Context, kernel.Arguments, toolhub.Context) (toolhub.Output, error) {
		return toolhub.Output{Content: strings.Repeat("x", 20_000), CompactArguments: kernel.Arguments(`{"content":"[4 KB written to out.txt]"}`)}, nil
	})
}

func bigCall(id string) kernel.ToolCall {
	return kernel.ToolCall{ID: id, Name: "big_tool", Arguments: kernel.Arguments(`{"content":"` + strings.Repeat("y", 4_000) + `"}`)}
}

func TestLoopSendsCompactArgumentsAndKeepsOriginal(t *testing.T) {
	prof := testProfile("test-agent")
	prof.Compactor = a129Config()
	prof.Compactor.ProactivePruneTokens = 0
	r := newRig(t, rigOptions{prof: &prof, handlers: []toolhub.Handler{bigTool()}},
		toolTurn(bigCall("tc-0")), aecmock.ScriptedTurn{Content: "done"})
	if _, err := r.run(t, "go"); err != nil {
		t.Fatal(err)
	}
	calls := mainCalls(r.models.Calls())
	if len(calls) != 2 {
		t.Fatalf("%d main calls, want 2", len(calls))
	}
	var sent kernel.ToolCall
	for _, m := range calls[1].Messages {
		if len(m.ToolCalls) > 0 {
			sent = m.ToolCalls[0]
		}
	}
	if string(sent.Arguments) != `{"content":"[4 KB written to out.txt]"}` {
		t.Fatalf("sent arguments = %.60s, want the compact ones", sent.Arguments)
	}
	for _, m := range r.conv.Messages() {
		if len(m.ToolCalls) > 0 && !strings.Contains(string(m.ToolCalls[0].Arguments), strings.Repeat("y", 100)) {
			t.Fatalf("the stored call lost its original arguments")
		}
	}
}

func TestLoopClearsOldToolResultsBelowThreshold(t *testing.T) {
	prof := testProfile("test-agent")
	prof.Compactor = a129Config()
	// A 20,000-character result counts 2,500..10,000 tokens (the counter's ratio is
	// clamped to 2..8), so turn four always finds the results of turns one and two
	// cleared, whichever turn passed the 4,000-token high mark first.
	prof.Compactor.ProactivePruneTokens = 4_000
	var turns []aecmock.ScriptedTurn
	for _, id := range []string{"tc-1", "tc-2", "tc-3", "tc-4"} {
		turns = append(turns, toolTurn(bigCall(id)), aecmock.ScriptedTurn{Content: "done"})
	}
	r := newRig(t, rigOptions{prof: &prof, handlers: []toolhub.Handler{bigTool()}}, turns...)
	for _, q := range []string{"one", "two", "three", "four"} {
		if _, err := r.run(t, q); err != nil {
			t.Fatalf("run %q: %v", q, err)
		}
	}

	calls := mainCalls(r.models.Calls())
	var results []kernel.ToolResult
	for _, m := range calls[6].Messages {
		results = append(results, m.ToolResults...)
	}
	if len(results) != 3 {
		t.Fatalf("turn four's first request has %d tool results, want 3", len(results))
	}
	for i, want := range []bool{true, true, false} {
		if cleared := strings.HasPrefix(results[i].Content, "[Old big_tool output cleared"); cleared != want {
			t.Errorf("result %d cleared = %v, want %v", i+1, cleared, want)
		}
	}

	s, _ := r.conv.Session()
	stored, err := r.hub.CompactedMessages.Get(t.Context(), s.ID)
	if err != nil {
		t.Fatal(err)
	}
	cleared := 0
	for _, m := range stored {
		for _, res := range m.ToolResults {
			if strings.HasPrefix(res.Content, "[Old big_tool") {
				cleared++
			}
		}
	}
	if cleared != 2 {
		t.Errorf("the stored working set has %d cleared results, want 2", cleared)
	}
	for _, m := range r.conv.Messages() {
		for _, res := range m.ToolResults {
			if strings.HasPrefix(res.Content, "[Old") {
				t.Fatalf("the history (what the UI shows) was cleared too")
			}
		}
	}
}

// ---------------------------------------------------------------------------
// A-129 item 4: tool output capped at the source
// ---------------------------------------------------------------------------

func TestCutToolOutputKeepsShortOutputAndZeroLimit(t *testing.T) {
	if got := agentloop.CutToolOutput("abc", 10, ""); got != "abc" {
		t.Fatalf("got %q", got)
	}
	long := strings.Repeat("x", 100)
	if got := agentloop.CutToolOutput(long, 0, ""); got != long {
		t.Fatalf("a limit of 0 changed the output")
	}
}

func TestCutToolOutputKeepsHeadAndTail(t *testing.T) {
	content := strings.Repeat("H", 600) + strings.Repeat("M", 1_000) + strings.Repeat("T", 400)
	out := agentloop.CutToolOutput(content, 1_000, ".tool-output/tc-1.txt")
	if !strings.HasPrefix(out, strings.Repeat("H", 600)+"\n\n[… 1000 characters cut here") {
		t.Fatalf("head = %.80q", out)
	}
	if !strings.HasSuffix(out, "]\n\n"+strings.Repeat("T", 400)) {
		t.Fatalf("the tail is not kept")
	}
	if !strings.Contains(out, "The full output (2000 characters) is saved in .tool-output/tc-1.txt") || strings.Contains(out, "M") {
		t.Fatalf("note = %q", out[600:760])
	}
	if !strings.Contains(agentloop.CutToolOutput(strings.Repeat("x", 2_000), 1_000, ""), "Run the tool again with a narrower request") {
		t.Fatalf("no narrower-request note without a saved file")
	}
}

func TestCutToolOutputNeverSplitsASurrogatePair(t *testing.T) {
	// 😀 is 2 UTF-16 code units. A limit of 10 keeps 6 units of head and 4 of tail: 3 and 2 emoji.
	out := agentloop.CutToolOutput(strings.Repeat("😀", 20), 10, "")
	if !strings.HasPrefix(out, "😀😀😀\n\n[… 30 characters cut") || !strings.HasSuffix(out, "]\n\n😀😀") {
		t.Fatalf("got %q", out)
	}
}

func TestToolOutputPathIsPathSafe(t *testing.T) {
	if got := agentloop.ToolOutputPath("call/../x y"); got != ".tool-output/call____x_y.txt" {
		t.Fatalf("got %q", got)
	}
}

var hugeOutput = strings.Repeat("a", 30_000) + strings.Repeat("b", 30_000)

// runHuge runs one call of a tool that answers 60,000 characters and returns the result
// the model got next.
func runHuge(t *testing.T, sandboxes agentloop.Sandboxes, limit *int) string {
	t.Helper()
	prof := testProfile("test-agent")
	prof.Compactor = a129Config()
	prof.Compactor.ProactivePruneTokens = 0
	prof.LoopConfig = &profile.LoopConfig{MaxToolResultChars: limit}
	huge := constTool("huge", toolhub.Output{Content: hugeOutput})
	r := newRig(t, rigOptions{prof: &prof, handlers: []toolhub.Handler{huge}, sandboxes: sandboxes},
		toolTurn(kernel.ToolCall{ID: "tc-huge", Name: "huge", Arguments: kernel.Arguments(`{}`)}), aecmock.ScriptedTurn{Content: "done"})
	if _, err := r.run(t, "go"); err != nil {
		t.Fatal(err)
	}
	calls := mainCalls(r.models.Calls())
	for _, m := range calls[len(calls)-1].Messages {
		if len(m.ToolResults) > 0 {
			return m.ToolResults[0].Content
		}
	}
	t.Fatal("no tool result reached the model")
	return ""
}

func TestLoopCutsLongResultAndSavesItInAnIsolatedSandbox(t *testing.T) {
	sb := newFakeSandboxes(t, sandboxhub.KindDocker, "docker:own")
	got := runHuge(t, sb, nil)
	if kernel.UTF16Len(got) >= 51_000 || !strings.HasPrefix(got, strings.Repeat("a", 30_000)) || !strings.HasSuffix(got, strings.Repeat("b", 20_000)) {
		t.Fatalf("result not cut to its head and tail (%d characters)", len(got))
	}
	if !strings.Contains(got, ".tool-output/tc-huge.txt") {
		t.Fatalf("the note does not name the saved file")
	}
	saved, err := sb.view.ReadFile(t.Context(), ".tool-output/tc-huge.txt", sandboxhub.ReadOptions{})
	if err != nil || saved.Content != hugeOutput {
		t.Fatalf("saved file: %v, %d characters", err, len(saved.Content))
	}
}

func TestLoopOnlyCutsOnAHostFolder(t *testing.T) {
	sb := newFakeSandboxes(t, sandboxhub.KindHost, "host:own")
	got := runHuge(t, sb, nil)
	if !strings.Contains(got, "Run the tool again with a narrower request") {
		t.Fatalf("got the note %q", got[30_000:30_200])
	}
	if _, err := sb.view.ReadFile(t.Context(), ".tool-output/tc-huge.txt", sandboxhub.ReadOptions{}); err == nil {
		t.Fatalf("a file was written to the host folder")
	}
}

func TestLoopKeepsResultsWholeWithLimitZero(t *testing.T) {
	if got := runHuge(t, nil, intPtr(0)); got != hugeOutput {
		t.Fatalf("the result was changed (%d characters)", len(got))
	}
}
