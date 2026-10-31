package gemini

import (
	"strings"
	"testing"

	"github.com/Phong95/aec-agent-go/components/llmhub"
	"github.com/Phong95/aec-agent-go/kernel"
)

// A-180: the images follow the function response, in the same user turn.
func TestRequestCarriesToolResultImages(t *testing.T) {
	msgs := []kernel.Message{
		{Role: kernel.RoleUser, Content: "Look"},
		{Role: kernel.RoleAssistant, ToolCalls: []kernel.ToolCall{{ID: "c1", Name: "snap", Arguments: kernel.Arguments(`{}`)}}},
		{Role: kernel.RoleAssistant, ToolResults: []kernel.ToolResult{{ToolCallID: "c1", Content: "shot", Images: []kernel.MessageAttachment{
			{Type: kernel.AttachmentTypeImage, MIMEType: "image/png", Data: "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNkYPhfDwAChwGA60e6kgAAAABJRU5ErkJggg=="},
		}}}},
	}
	b, err := buildBody(msgs, llmhub.ModelOptions{}, "")
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"role":"user","parts":[{"functionResponse":{"name":"snap","response":{"output":"shot"}}},{"text":"The image returned by tool call c1:"},{"inlineData":{"mimeType":"image/png","data":"iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNkYPhfDwAChwGA60e6kgAAAABJRU5ErkJggg=="}}]}`; !strings.Contains(string(b), want) {
		t.Fatalf("body = %s\nwant it to contain %s", b, want)
	}
}
