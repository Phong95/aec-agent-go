package agent_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/Phong95/aec-agent-go/aecmock"
	"github.com/Phong95/aec-agent-go/components/skillhub/skill"
	"github.com/Phong95/aec-agent-go/kernel"
)

// TestAgent_SkillViewEndToEnd is the tracer of plan 10-04: an agent whose profile
// enables a skill sees the <available_skills> index at the end of its system message
// on the model call, the model gets the skill_view tool, and the loop dispatches the
// model's skill_view call to the agent's SkillManager, which returns the skill's
// rendered instructions.
func TestAgent_SkillViewEndToEnd(t *testing.T) {
	e := newEnv(t)
	if _, err := e.skills.Register(skill.Package{Content: skillMD("report-format")}, skill.SourceApp); err != nil {
		t.Fatalf("Register: %v", err)
	}
	args, err := kernel.NewArguments([]byte(`{"name":"report-format"}`))
	if err != nil {
		t.Fatal(err)
	}
	// The first call (no tool result yet) asks for the skill, the second ends the turn.
	model := aecmock.NewMockProvider("skill-model", nil, aecmock.WithResponse(func(msgs []kernel.Message) kernel.ModelResponse {
		usage := kernel.TokenUsage{InputTokens: 1, OutputTokens: 1}
		if slices.ContainsFunc(msgs, func(m kernel.Message) bool { return len(m.ToolResults) > 0 }) {
			return kernel.ModelResponse{
				Message:    kernel.Message{Role: kernel.RoleAssistant, Content: "done"},
				Usage:      usage,
				StopReason: kernel.StopEndTurn,
			}
		}
		return kernel.ModelResponse{
			Message: kernel.Message{Role: kernel.RoleAssistant, ToolCalls: []kernel.ToolCall{
				{ID: "call-1", Name: "skill_view", Arguments: args},
			}},
			Usage:      usage,
			StopReason: kernel.StopToolUse,
		}
	}))
	if err := e.llm.Register(model); err != nil {
		t.Fatal(err)
	}

	p := testProfile("skilled")
	p.Provider = "skill-model"
	p.Skills = []string{"report-format"}
	a := e.spawn(t, p)
	session, err := a.StartSession(t.Context())
	if err != nil {
		t.Fatal(err)
	}

	res := chat(t, a, "format the weekly report")
	if res.Iterations != 2 {
		t.Fatalf("Iterations = %d, want 2 (skill_view, then the answer)", res.Iterations)
	}

	calls := model.Calls()
	if len(calls) != 2 {
		t.Fatalf("model calls = %d, want 2", len(calls))
	}
	first := calls[0]
	system := first.Messages[0]
	if system.Role != kernel.RoleSystem {
		t.Fatalf("first message role = %q, want system", system.Role)
	}
	if !strings.HasPrefix(system.Content, "You are a helpful assistant.\n\n## Skills\n") {
		t.Errorf("system message does not start with the prompt and the skills guidance:\n%s", system.Content)
	}
	if !strings.HasSuffix(system.Content, "</available_skills>") {
		t.Errorf("system message must end with the index block:\n%s", system.Content)
	}
	if !strings.Contains(system.Content, "<name>report-format</name>\n<description>Does report-format.</description>") {
		t.Errorf("the index does not name the skill:\n%s", system.Content)
	}
	var offered []string
	for _, d := range first.Options.Tools {
		offered = append(offered, d.Name)
	}
	if !slices.Contains(offered, "skill_view") {
		t.Errorf("tools offered = %v, want skill_view", offered)
	}

	stored, err := e.hub.Messages.Get(t.Context(), session.ID)
	if err != nil {
		t.Fatal(err)
	}
	var result string
	for _, m := range stored {
		for _, r := range m.ToolResults {
			result = r.Content
			if r.IsError != nil && *r.IsError {
				t.Fatalf("the skill_view result is an error: %s", r.Content)
			}
		}
	}
	if !strings.Contains(result, `<skill_content name="report-format">`) || !strings.Contains(result, "Step 1.") {
		t.Errorf("persisted skill_view result = %q, want the rendered skill", result)
	}
}
