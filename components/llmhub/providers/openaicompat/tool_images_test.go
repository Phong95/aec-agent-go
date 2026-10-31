package openaicompat

import (
	"strings"
	"testing"

	"github.com/Phong95/aec-agent-go/components/llmhub"
	"github.com/Phong95/aec-agent-go/kernel"
)

// A-180: a tool message is text only, so the images follow in a user message.
func TestRequestCarriesToolResultImages(t *testing.T) {
	msgs := []kernel.Message{
		{Role: kernel.RoleUser, Content: "Look"},
		{Role: kernel.RoleAssistant, ToolCalls: []kernel.ToolCall{{ID: "c1", Name: "snap", Arguments: kernel.Arguments(`{}`)}}},
		{Role: kernel.RoleAssistant, ToolResults: []kernel.ToolResult{{ToolCallID: "c1", Content: "shot", Images: []kernel.MessageAttachment{
			{Type: kernel.AttachmentTypeImage, MIMEType: "image/png", Data: "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNkYPhfDwAChwGA60e6kgAAAABJRU5ErkJggg=="},
		}}}},
	}
	p, err := New(llmhub.ProviderConfig{APIKey: "k"})
	if err != nil {
		t.Fatal(err)
	}
	b, err := p.buildRequest(msgs, llmhub.ModelOptions{}, false)
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"role":"tool","tool_call_id":"c1","content":"shot"},{"role":"user","content":[{"type":"text","text":"The image returned by tool call c1:"},{"type":"image_url","image_url":{"url":"data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNkYPhfDwAChwGA60e6kgAAAABJRU5ErkJggg=="}}]}`; !strings.Contains(string(b), want) {
		t.Fatalf("body = %s\nwant it to contain %s", b, want)
	}
}
