package agent_test

import (
	"context"
	"strings"
	"testing"

	"github.com/Phong95/aec-agent-go/aecmock"
	"github.com/Phong95/aec-agent-go/components/llmhub"
	"github.com/Phong95/aec-agent-go/components/persistence"
	"github.com/Phong95/aec-agent-go/components/toolhub"
	"github.com/Phong95/aec-agent-go/kernel"
)

// A-180: a tool result can carry images the model looks at (a screenshot of a page it
// built); a model whose record says it can't see gets none, and no needsVision tool.

// snapTool returns its images with a short text.
type snapTool struct{ images []kernel.MessageAttachment }

func (snapTool) Name() string    { return "tool:snap" }
func (snapTool) Version() string { return "1.0.0" }
func (snapTool) Definition() toolhub.Definition {
	return toolhub.Definition{Name: "snap", Description: "Screenshot", Parameters: toolhub.JSONSchema{Type: "object"}, Risk: toolhub.RiskReadOnly}
}

func (s snapTool) Execute(context.Context, kernel.Arguments, toolhub.Context) (toolhub.Output, error) {
	return toolhub.Output{Content: "shot", Images: s.images}, nil
}

// lookTool needs a model that can see.
type lookTool struct{}

func (lookTool) Name() string    { return "tool:look" }
func (lookTool) Version() string { return "1.0.0" }
func (lookTool) Definition() toolhub.Definition {
	return toolhub.Definition{Name: "look", Description: "Needs vision", Parameters: toolhub.JSONSchema{Type: "object"}, Risk: toolhub.RiskReadOnly, NeedsVision: true}
}

func (lookTool) Execute(context.Context, kernel.Arguments, toolhub.Context) (toolhub.Output, error) {
	return toolhub.Output{Content: "ok"}, nil
}

// toolImageEnv spawns an agent on a provider that calls snap once, then answers. vision
// nil stores no provider record.
func toolImageEnv(t *testing.T, images []kernel.MessageAttachment, vision *bool) (*env, string, *aecmock.MockProvider) {
	t.Helper()
	e := newEnv(t)
	scripted := aecmock.NewMockProvider("scripted", nil, aecmock.WithResponse(func(msgs []kernel.Message) kernel.ModelResponse {
		if len(msgs) > 0 && len(msgs[len(msgs)-1].ToolResults) > 0 {
			return kernel.ModelResponse{Message: kernel.Message{Role: kernel.RoleAssistant, Content: "a dot"}, StopReason: kernel.StopEndTurn}
		}
		return kernel.ModelResponse{
			Message:    kernel.Message{Role: kernel.RoleAssistant, ToolCalls: []kernel.ToolCall{{ID: "c1", Name: "snap", Arguments: kernel.Arguments(`{}`)}}},
			StopReason: kernel.StopToolUse,
		}
	}))
	mustOK(t, e.llm.Register(scripted))
	mustOK(t, e.tools.Register(snapTool{images: images}))
	mustOK(t, e.tools.Register(lookTool{}))
	if vision != nil {
		mustOK(t, e.hub.Providers.Save(t.Context(), persistence.ProviderRecord{
			Name: "scripted", Type: persistence.ProviderOpenAICompatible,
			Models: []llmhub.ModelRecord{{ID: "m1", Provider: "scripted", Capabilities: &llmhub.ModelCapabilities{Vision: vision}}},
		}))
	}
	p := testProfile("vision")
	p.Provider, p.LLMModel, p.Tools = "scripted", "m1", []string{"snap", "look"}
	a := e.spawn(t, p)
	s, err := a.StartSession(t.Context())
	mustOK(t, err)
	_, err = a.Chat(t.Context(), "Look at it")
	mustOK(t, err)
	return e, s.ID, scripted
}

func toolNames(defs []toolhub.Definition) string {
	names := make([]string, 0, len(defs))
	for _, d := range defs {
		names = append(names, d.Name)
	}
	return strings.Join(names, ",")
}

func sentResult(t *testing.T, call aecmock.MockCall) kernel.ToolResult {
	t.Helper()
	for _, m := range call.Messages {
		if len(m.ToolResults) > 0 {
			return m.ToolResults[0]
		}
	}
	t.Fatalf("no tool result in %+v", call.Messages)
	return kernel.ToolResult{}
}

func TestToolImagesReachAModelThatSees(t *testing.T) {
	yes := true
	e, sessionID, scripted := toolImageEnv(t, []kernel.MessageAttachment{dotPNG}, &yes)
	calls := scripted.Calls()
	if got := toolNames(calls[0].Options.Tools); got != "snap,look" {
		t.Fatalf("tools = %s", got)
	}
	if r := sentResult(t, calls[1]); len(r.Images) != 1 || r.Content != "shot" {
		t.Fatalf("result sent = %+v", r)
	}
	stored, err := e.hub.Messages.Get(t.Context(), sessionID)
	mustOK(t, err)
	found := false
	for _, m := range stored {
		for _, r := range m.ToolResults {
			found = found || len(r.Images) == 1
		}
	}
	if !found {
		t.Fatalf("stored = %+v", stored)
	}
}

func TestToolImagesLeftOutForAModelThatCannotSee(t *testing.T) {
	no := false
	_, _, scripted := toolImageEnv(t, []kernel.MessageAttachment{dotPNG}, &no)
	calls := scripted.Calls()
	if got := toolNames(calls[0].Options.Tools); got != "snap" {
		t.Fatalf("tools = %s", got)
	}
	r := sentResult(t, calls[1])
	if len(r.Images) != 0 || !strings.Contains(r.Content, "this model can't see images") {
		t.Fatalf("result sent = %+v", r)
	}
}

func TestToolImagesWithoutARecordCountAsSeen(t *testing.T) {
	_, _, scripted := toolImageEnv(t, []kernel.MessageAttachment{dotPNG}, nil)
	if r := sentResult(t, scripted.Calls()[1]); len(r.Images) != 1 {
		t.Fatalf("result sent = %+v", r)
	}
}

func TestInvalidToolImagesAreLeftOutWithANote(t *testing.T) {
	svg := dotPNG
	svg.MIMEType = "image/svg+xml"
	_, _, scripted := toolImageEnv(t, []kernel.MessageAttachment{svg}, nil)
	r := sentResult(t, scripted.Calls()[1])
	if len(r.Images) != 0 || !strings.HasPrefix(r.Content, "shot\n\n(The images this tool returned were left out: ") {
		t.Fatalf("result sent = %+v", r)
	}
}
