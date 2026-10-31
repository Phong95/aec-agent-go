package agentloop

import (
	"testing"

	"github.com/Phong95/aec-agent-go/kernel"
)

// A-180: only the newest tool-result images reach the model.

func toolImageMessages(counts ...int) []kernel.Message {
	img := kernel.MessageAttachment{Type: kernel.AttachmentTypeImage, MIMEType: "image/png", Data: "AAAA"}
	var msgs []kernel.Message
	for i, n := range counts {
		id := string(rune('a' + i))
		images := make([]kernel.MessageAttachment, n)
		for j := range images {
			images[j] = img
		}
		msgs = append(msgs,
			kernel.Message{Role: kernel.RoleAssistant, ToolCalls: []kernel.ToolCall{{ID: id, Name: "snap", Arguments: kernel.Arguments(`{}`)}}},
			kernel.Message{Role: kernel.RoleAssistant, ToolResults: []kernel.ToolResult{{ToolCallID: id, Content: "shot", Images: images}}},
		)
	}
	return msgs
}

func imageTotal(msgs []kernel.Message) int {
	n := 0
	for _, m := range msgs {
		n += len(m.ToolResultImages())
	}
	return n
}

func TestWithToolImagesKeepsTheNewest(t *testing.T) {
	msgs := toolImageMessages(4, 3, 3)
	out := withToolImages(msgs, 6)
	if got := imageTotal(out); got != 6 {
		t.Fatalf("images kept = %d, want 6", got)
	}
	if len(out[5].ToolResults[0].Images) != 3 || len(out[3].ToolResults[0].Images) != 3 {
		t.Fatalf("newest results lost images: %+v", out)
	}
	dropped := out[1].ToolResults[0]
	want := "shot\n\n(4 images not shown: an older image, no longer shown to save context; take a new screenshot to look again.)"
	if dropped.Images != nil || dropped.Content != want {
		t.Fatalf("dropped = %+v", dropped)
	}
	if imageTotal(msgs) != 10 {
		t.Fatal("the input messages were changed")
	}
}

func TestWithToolImagesSendsNoneToAModelThatCannotSee(t *testing.T) {
	out := withToolImages(toolImageMessages(1), 0)
	if r := out[1].ToolResults[0]; r.Images != nil || r.Content != "shot\n\n(1 image not shown: this model can't see images.)" {
		t.Fatalf("result = %+v", r)
	}
}

func TestWithToolImagesReturnsTheSameSliceWhenEverythingFits(t *testing.T) {
	msgs := toolImageMessages(2)
	if out := withToolImages(msgs, MaxToolImagesInContext); &out[0] != &msgs[0] {
		t.Fatal("a copy was returned")
	}
}
