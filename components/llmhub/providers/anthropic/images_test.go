package anthropic

import (
	"strings"
	"testing"

	"github.com/Phong95/aec-agent-go/components/llmhub"
	"github.com/Phong95/aec-agent-go/kernel"
)

// A-165: an attached image is sent as the provider's image part.
func TestRequestCarriesAttachedImages(t *testing.T) {
	msgs := []kernel.Message{{Role: kernel.RoleUser, Content: "What is this?", Attachments: []kernel.MessageAttachment{
		{Type: kernel.AttachmentTypeImage, MIMEType: "image/png", Data: "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNkYPhfDwAChwGA60e6kgAAAABJRU5ErkJggg==", Name: "dot.png"},
	}}}
	p, err := New(llmhub.ProviderConfig{APIKey: "k"})
	if err != nil {
		t.Fatal(err)
	}
	b, _, err := p.buildRequest(msgs, llmhub.ModelOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"role":"user","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNkYPhfDwAChwGA60e6kgAAAABJRU5ErkJggg=="}},{"type":"text","text":"What is this?"}]}`; !strings.Contains(string(b), want) {
		t.Fatalf("body = %s\nwant it to contain %s", b, want)
	}
}
