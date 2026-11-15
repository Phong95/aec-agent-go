package kernel

import (
	"strings"
	"testing"
)

// A-165: checks of images attached to a message.
func TestCheckAttachments(t *testing.T) {
	img := MessageAttachment{Type: AttachmentTypeImage, MIMEType: "image/png", Data: "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNkYPhfDwAChwGA60e6kgAAAABJRU5ErkJggg==", Name: "dot.png"}
	tooMany := make([]MessageAttachment, MaxImagesPerMessage+1)
	for i := range tooMany {
		tooMany[i] = img
	}
	big := img
	big.Data = strings.Repeat("A", (MaxImageBytes+3+2)/3*4)
	cases := []struct {
		name  string
		in    []MessageAttachment
		valid bool
	}{
		{"one image", []MessageAttachment{img}, true},
		{"up to the limit", tooMany[:MaxImagesPerMessage], true},
		{"too many", tooMany, false},
		{"a document", []MessageAttachment{{Type: "document", MIMEType: "image/png", Data: img.Data}}, false},
		{"an svg", []MessageAttachment{{Type: AttachmentTypeImage, MIMEType: "image/svg+xml", Data: img.Data}}, false},
		{"empty data", []MessageAttachment{{Type: AttachmentTypeImage, MIMEType: "image/png"}}, false},
		{"a data URL", []MessageAttachment{{Type: AttachmentTypeImage, MIMEType: "image/png", Data: img.DataURL()}}, false},
		{"too large", []MessageAttachment{big}, false},
		{"a long name", []MessageAttachment{{Type: AttachmentTypeImage, MIMEType: "image/png", Data: img.Data, Name: strings.Repeat("x", 256)}}, false},
	}
	for _, c := range cases {
		if err := CheckAttachments(c.in); (err == nil) != c.valid {
			t.Errorf("%s: err = %v, want valid = %v", c.name, err, c.valid)
		}
	}
}

// A-222: CheckAttachmentsUpTo checks the count against the agent's limit; below 1 means
// MaxImagesPerMessage.
func TestCheckAttachmentsUpTo(t *testing.T) {
	img := MessageAttachment{Type: AttachmentTypeImage, MIMEType: "image/png", Data: "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNkYPhfDwAChwGA60e6kgAAAABJRU5ErkJggg=="}
	nine := make([]MessageAttachment, 9)
	for i := range nine {
		nine[i] = img
	}
	if err := CheckAttachmentsUpTo(nine[:8], 8); err != nil {
		t.Errorf("8 images up to 8: %v", err)
	}
	if err := CheckAttachmentsUpTo(nine, 8); err == nil || err.Error() != "at most 8 images per message" {
		t.Errorf("9 images up to 8: err = %v", err)
	}
	if err := CheckAttachmentsUpTo(nine[:MaxImagesPerMessage+1], 0); err == nil {
		t.Errorf("%d images with no limit: want the default limit", MaxImagesPerMessage+1)
	}
	if err := CheckAttachmentsUpTo(nine[:2], 1); err == nil {
		t.Error("2 images up to 1: want an error")
	}
}
