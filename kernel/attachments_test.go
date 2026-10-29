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
