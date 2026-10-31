package llmhub_test

import (
	"testing"

	"github.com/Phong95/aec-agent-go/components/llmhub"
	"github.com/Phong95/aec-agent-go/kernel"
)

// A-165: whether a listed model takes images.
func TestModelAcceptsImages(t *testing.T) {
	yes, no := true, false
	cases := []struct {
		name          string
		model         llmhub.ModelRecord
		accepts, know bool
	}{
		{"vision", llmhub.ModelRecord{Capabilities: &llmhub.ModelCapabilities{Vision: &yes}}, true, true},
		{"image input", llmhub.ModelRecord{InputModalities: []string{"text", "image"}}, true, true},
		{"no vision", llmhub.ModelRecord{Capabilities: &llmhub.ModelCapabilities{Vision: &no}}, false, true},
		{"text only", llmhub.ModelRecord{InputModalities: []string{"text"}}, false, true},
		{"unknown", llmhub.ModelRecord{Capabilities: &llmhub.ModelCapabilities{ToolCalling: &yes}}, false, false},
	}
	for _, c := range cases {
		accepts, known := llmhub.ModelAcceptsImages(c.model)
		if accepts != c.accepts || known != c.know {
			t.Errorf("%s: got %v, %v", c.name, accepts, known)
		}
	}
}

// A-165: an image counts as kernel.ImageTokenEstimate, never by its base64 size.
func TestEstimateTokensCountsImages(t *testing.T) {
	text := kernel.Message{Role: kernel.RoleUser, Content: "What is this?"}
	withImage := text
	withImage.Attachments = []kernel.MessageAttachment{{Type: kernel.AttachmentTypeImage, MIMEType: "image/png", Data: "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNkYPhfDwAChwGA60e6kgAAAABJRU5ErkJggg=="}}
	if got, want := llmhub.EstimateTokens([]kernel.Message{withImage}), llmhub.EstimateTokens([]kernel.Message{text})+kernel.ImageTokenEstimate; got != want {
		t.Fatalf("EstimateTokens = %d, want %d", got, want)
	}
}

// A-180: a tool result's image counts as kernel.ImageTokenEstimate, not by its base64 size.
func TestEstimateTokensCountsToolResultImages(t *testing.T) {
	plain := kernel.Message{Role: kernel.RoleAssistant, ToolResults: []kernel.ToolResult{{ToolCallID: "c1", Content: "shot"}}}
	withImage := plain.Clone()
	withImage.ToolResults[0].Images = []kernel.MessageAttachment{{Type: kernel.AttachmentTypeImage, MIMEType: "image/png", Data: "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNkYPhfDwAChwGA60e6kgAAAABJRU5ErkJggg=="}}
	if got, want := llmhub.EstimateTokens([]kernel.Message{withImage}), llmhub.EstimateTokens([]kernel.Message{plain})+kernel.ImageTokenEstimate; got != want {
		t.Fatalf("EstimateTokens = %d, want %d", got, want)
	}
}
