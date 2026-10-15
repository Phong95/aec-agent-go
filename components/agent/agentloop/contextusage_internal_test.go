package agentloop

import (
	"math"
	"slices"
	"strings"
	"testing"

	"github.com/Phong95/aec-agent-go/components/toolhub"
	"github.com/Phong95/aec-agent-go/kernel"
)

// A-104: measureContext, the same cases as the TS SDK (tests/unit/context-usage.test.ts).

func byKey(u ContextUsage) map[ContextCategoryKey]int {
	out := map[ContextCategoryKey]int{}
	for _, c := range u.Categories {
		out[c.Key] = c.Tokens
	}
	return out
}

func sumCategories(u ContextUsage) int {
	n := 0
	for _, c := range u.Categories {
		n += c.Tokens
	}
	return n
}

func measureFixture() (measureInput, func(string) int, int) {
	c := NewCounter()
	tok := func(s string) int { return c.countFresh(kernel.Message{Role: kernel.RoleSystem, Content: s}) }
	tool := toolhub.Definition{Name: "t", Description: "d", Parameters: toolhub.JSONSchema{Type: "object"}}
	b, err := kernel.EncodeJSON(toolTokensShape{Name: "t", Description: "d", Parameters: tool.Parameters})
	if err != nil {
		panic(err)
	}
	in := measureInput{
		sessionID: "s-1",
		sections: []ContextSection{
			{Key: ContextSystemPrompt, Text: "PROMPT"},
			{Key: ContextMemoryFiles, Text: "FILES"},
		},
		recalled:  "MEMORIES",
		tools:     []toolhub.Definition{tool},
		window:    1000,
		threshold: 0.8,
		counter:   c,
	}
	return in, tok, tok(string(b))
}

const systemText = "PROMPT\n\nFILES\n\nMEMORIES"

func TestMeasureContext_SplitsAndEstimates(t *testing.T) {
	in, tok, toolTokens := measureFixture()
	in.messages = []kernel.Message{
		{Role: kernel.RoleSystem, Content: systemText},
		{Role: kernel.RoleAssistant, Content: "SUMMARY", Summary: true},
		{Role: kernel.RoleUser, Content: "hello"},
	}
	u := measureContext(in)
	c := byKey(u)

	var keys []ContextCategoryKey
	for _, cat := range u.Categories {
		keys = append(keys, cat.Key)
	}
	if !slices.Equal(keys, ContextCategoryKeys()) {
		t.Fatalf("category order = %v", keys)
	}
	system := tok(systemText)
	if c[ContextMemoryFiles] != tok("FILES") || c[ContextMemories] != tok("MEMORIES") {
		t.Fatalf("files %d memories %d", c[ContextMemoryFiles], c[ContextMemories])
	}
	// The system prompt section plus what the sections do not account for.
	if want := tok("PROMPT") + max(0, system-tok("PROMPT")-tok("FILES")-tok("MEMORIES")); c[ContextSystemPrompt] != want {
		t.Fatalf("system_prompt = %d, want %d", c[ContextSystemPrompt], want)
	}
	if c[ContextSummary] != tok("SUMMARY") || c[ContextMessages] != tok("hello") {
		t.Fatalf("summary %d messages %d", c[ContextSummary], c[ContextMessages])
	}
	if c[ContextTools] != toolTokens || len(u.Tools) != 1 || u.Tools[0] != (ContextToolTokens{Name: "t", Tokens: toolTokens}) {
		t.Fatalf("tools = %d %+v", c[ContextTools], u.Tools)
	}
	if c[ContextOther] != 0 || u.UsedSource != UsedEstimated || u.UsedTokens != sumCategories(u) {
		t.Fatalf("other %d source %s used %d", c[ContextOther], u.UsedSource, u.UsedTokens)
	}
	if u.CompactAtTokens != 800 || u.FreeTokens != 1000-u.UsedTokens || u.Iteration != 0 {
		t.Fatalf("compactAt %d free %d iteration %d", u.CompactAtTokens, u.FreeTokens, u.Iteration)
	}
}

func TestMeasureContext_ReportedTotalAndOther(t *testing.T) {
	in, tok, toolTokens := measureFixture()
	in.iteration = 2
	in.messages = []kernel.Message{{Role: kernel.RoleSystem, Content: systemText}, {Role: kernel.RoleUser, Content: "hello"}}
	in.response = &kernel.Message{Role: kernel.RoleAssistant, Content: "answer"}
	in.reported = &kernel.TokenUsage{InputTokens: 300, OutputTokens: 40}
	u := measureContext(in)
	c := byKey(u)

	// The answer joins the context at its reported size.
	if c[ContextMessages] != tok("hello")+40 {
		t.Fatalf("messages = %d", c[ContextMessages])
	}
	estimated := tok(systemText) + tok("hello") + 40 + toolTokens
	if u.UsedSource != UsedReported || u.UsedTokens != 340 || c[ContextOther] != 340-estimated {
		t.Fatalf("source %s used %d other %d (estimated %d)", u.UsedSource, u.UsedTokens, c[ContextOther], estimated)
	}
	if sumCategories(u) != 340 || u.Iteration != 2 {
		t.Fatalf("sum %d iteration %d", sumCategories(u), u.Iteration)
	}
}

func TestMeasureContext_NeverNegative(t *testing.T) {
	in, _, _ := measureFixture()
	in.window, in.threshold = 10, 0.5
	// Reported 20: under the estimates (system message alone is larger), over the window.
	in.messages = []kernel.Message{{Role: kernel.RoleSystem, Content: systemText + systemText}}
	in.reported = &kernel.TokenUsage{InputTokens: 15, OutputTokens: 5}
	u := measureContext(in)
	if byKey(u)[ContextOther] != 0 || u.FreeTokens != 0 {
		t.Fatalf("other %d free %d", byKey(u)[ContextOther], u.FreeTokens)
	}
}

// A-104 audit: the provider's input tokens include the tool definitions, so calibration
// counts their characters too (the same cases as the TS counter-calibration test).
func TestCalibrate_CountsToolDefinitions(t *testing.T) {
	msgs := []kernel.Message{
		{Role: kernel.RoleSystem, Content: strings.Repeat("s", 400)},
		{Role: kernel.RoleUser, Content: strings.Repeat("u", 400)},
	}
	tool := toolhub.Definition{Name: "big_tool", Description: strings.Repeat("d", 100), Parameters: toolhub.JSONSchema{Type: "object"}}
	tools := slices.Repeat([]toolhub.Definition{tool}, 10)
	toolChars := toolDefinitionChars(tool) * 10
	// The provider reports 4 chars per token over everything sent.
	input := (800 + toolChars) / 4

	with := NewCounter()
	with.Calibrate(msgs, kernel.TokenUsage{InputTokens: input}, tools...)
	if got, want := with.Calibration().CharsPerToken, float64(800+toolChars)/float64(input); math.Abs(got-want) > 1e-9 {
		t.Fatalf("ratio with tools = %v, want %v", got, want)
	}

	without := NewCounter()
	without.Calibrate(msgs, kernel.TokenUsage{InputTokens: input})
	if got := without.Calibration().CharsPerToken; got >= 4 {
		t.Fatalf("ratio without tools = %v, want it driven below 4 (the old behaviour)", got)
	}
}
