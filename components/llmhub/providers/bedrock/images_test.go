package bedrock

import (
	"encoding/base64"
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime/types"

	"github.com/Phong95/aec-agent-go/components/llmhub"
	"github.com/Phong95/aec-agent-go/kernel"
)

// A-165: an attached image is sent as image bytes in the format Converse names, before the text.
func TestMapMessagesCarriesAttachedImages(t *testing.T) {
	_, mapped, err := mapMessages([]kernel.Message{{Role: kernel.RoleUser, Content: "What is this?", Attachments: []kernel.MessageAttachment{
		{Type: kernel.AttachmentTypeImage, MIMEType: "image/png", Data: "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNkYPhfDwAChwGA60e6kgAAAABJRU5ErkJggg=="},
	}}}, llmhub.ModelOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(mapped) != 1 || len(mapped[0].Content) != 2 {
		t.Fatalf("mapped = %+v", mapped)
	}
	img, ok := mapped[0].Content[0].(*types.ContentBlockMemberImage)
	if !ok || img.Value.Format != types.ImageFormatPng {
		t.Fatalf("first block = %#v", mapped[0].Content[0])
	}
	src, ok := img.Value.Source.(*types.ImageSourceMemberBytes)
	if !ok || base64.StdEncoding.EncodeToString(src.Value) != "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNkYPhfDwAChwGA60e6kgAAAABJRU5ErkJggg==" {
		t.Fatalf("source = %#v", img.Value.Source)
	}
	if text, ok := mapped[0].Content[1].(*types.ContentBlockMemberText); !ok || text.Value != "What is this?" {
		t.Fatalf("second block = %#v", mapped[0].Content[1])
	}
}
