package bedrock

import (
	"encoding/base64"
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime/types"

	"github.com/Phong95/aec-agent-go/components/llmhub"
	"github.com/Phong95/aec-agent-go/kernel"
)

// A-180: a tool result's images go inside its toolResult block, after its text.
func TestMapMessagesCarriesToolResultImages(t *testing.T) {
	const png = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNkYPhfDwAChwGA60e6kgAAAABJRU5ErkJggg=="
	_, mapped, err := mapMessages([]kernel.Message{
		{Role: kernel.RoleUser, Content: "Look"},
		{Role: kernel.RoleAssistant, ToolCalls: []kernel.ToolCall{{ID: "c1", Name: "snap", Arguments: kernel.Arguments(`{}`)}}},
		{Role: kernel.RoleAssistant, ToolResults: []kernel.ToolResult{{ToolCallID: "c1", Content: "shot", Images: []kernel.MessageAttachment{
			{Type: kernel.AttachmentTypeImage, MIMEType: "image/png", Data: png},
		}}}},
	}, llmhub.ModelOptions{})
	if err != nil {
		t.Fatal(err)
	}
	result, ok := mapped[len(mapped)-1].Content[0].(*types.ContentBlockMemberToolResult)
	if !ok || len(result.Value.Content) != 2 {
		t.Fatalf("last message = %#v", mapped[len(mapped)-1])
	}
	if text, ok := result.Value.Content[0].(*types.ToolResultContentBlockMemberText); !ok || text.Value != "shot" {
		t.Fatalf("first block = %#v", result.Value.Content[0])
	}
	img, ok := result.Value.Content[1].(*types.ToolResultContentBlockMemberImage)
	if !ok || img.Value.Format != types.ImageFormatPng {
		t.Fatalf("second block = %#v", result.Value.Content[1])
	}
	if src, ok := img.Value.Source.(*types.ImageSourceMemberBytes); !ok || base64.StdEncoding.EncodeToString(src.Value) != png {
		t.Fatalf("source = %#v", img.Value.Source)
	}
}
