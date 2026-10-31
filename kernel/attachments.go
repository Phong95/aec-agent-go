package kernel

import (
	"encoding/base64"
	"fmt"
	"regexp"
	"slices"
	"strings"
)

// A-165: what a message may carry as attachments (Message.Attachments). The limits fit
// every built-in provider: Anthropic takes at most 5 MB of base64 per image.

// AttachmentTypeImage is the type of an image attachment.
const AttachmentTypeImage = "image"

// ImageMIMETypes are the image types every built-in provider accepts.
var ImageMIMETypes = []string{"image/png", "image/jpeg", "image/webp", "image/gif"}

const (
	// MaxImageBytes is the largest image, in bytes (decoded): 5 MB once base64 encoded.
	MaxImageBytes = 3_750_000
	// MaxImagesPerMessage is the most images one message may carry.
	MaxImagesPerMessage = 4
	// ImageTokenEstimate is the tokens an image is counted as before the provider reports
	// real usage - about what a 1-megapixel image costs on Anthropic and OpenAI.
	ImageTokenEstimate = 1_600
)

// MessageAttachment (A-165) is a file attached to a message for the model to see. Images
// only for now (A-163 adds documents); the data travels inline, base64, so a stored
// conversation keeps it.
type MessageAttachment struct {
	// Type is AttachmentTypeImage.
	Type string `json:"type"`
	// MIMEType is one of ImageMIMETypes.
	MIMEType string `json:"mimeType"`
	// Data is the file's bytes, base64 (no "data:" prefix).
	Data string `json:"data"`
	// Name is the file name the user gave, for display; empty means absent.
	Name string `json:"name,omitempty"`
}

var base64Form = regexp.MustCompile(`^[A-Za-z0-9+/]*={0,2}$`)

// CheckAttachments reports what is wrong with attachments, nil when they are valid: at
// most MaxImagesPerMessage images, each of an ImageMIMETypes type, with non-empty base64
// data of at most MaxImageBytes and a name of at most 255 characters. The same rules as
// core/ts checkAttachments.
func CheckAttachments(attachments []MessageAttachment) error {
	if len(attachments) > MaxImagesPerMessage {
		return fmt.Errorf("at most %d images per message", MaxImagesPerMessage)
	}
	for i, a := range attachments {
		at := fmt.Sprintf("attachments[%d]", i)
		if a.Type != AttachmentTypeImage {
			return fmt.Errorf(`%s.type must be "image"`, at)
		}
		if !slices.Contains(ImageMIMETypes, a.MIMEType) {
			return fmt.Errorf("%s.mimeType must be one of %s", at, strings.Join(ImageMIMETypes, ", "))
		}
		if a.Data == "" || len(a.Data)%4 != 0 || !base64Form.MatchString(a.Data) {
			return fmt.Errorf("%s.data must be base64", at)
		}
		if base64.StdEncoding.DecodedLen(len(a.Data))-strings.Count(a.Data[len(a.Data)-2:], "=") > MaxImageBytes {
			return fmt.Errorf("%s is larger than %d bytes", at, MaxImageBytes)
		}
		if len([]rune(a.Name)) > 255 {
			return fmt.Errorf("%s.name must be a string of at most 255 characters", at)
		}
	}
	return nil
}

// Images returns the image attachments of m (nil when it has none).
func (m Message) Images() []MessageAttachment {
	var out []MessageAttachment
	for _, a := range m.Attachments {
		if a.Type == AttachmentTypeImage {
			out = append(out, a)
		}
	}
	return out
}

// ToolResultImages (A-180) returns the images m's tool results carry, in result order
// (nil when none has any).
func (m Message) ToolResultImages() []MessageAttachment {
	var out []MessageAttachment
	for _, r := range m.ToolResults {
		for _, a := range r.Images {
			if a.Type == AttachmentTypeImage {
				out = append(out, a)
			}
		}
	}
	return out
}

// ImageCount (A-180) is every image m carries - its attachments and its tool results'
// images. Each one counts as ImageTokenEstimate tokens.
func (m Message) ImageCount() int {
	return len(m.Images()) + len(m.ToolResultImages())
}

// WithoutImages (A-180) returns results without their images, for counting their text:
// an image is counted as ImageTokenEstimate, never by its base64 size. results itself is
// returned when none has images; a nil list stays nil.
func WithoutImages(results []ToolResult) []ToolResult {
	if !slices.ContainsFunc(results, func(r ToolResult) bool { return len(r.Images) > 0 }) {
		return results
	}
	out := slices.Clone(results)
	for i := range out {
		out[i].Images = nil
	}
	return out
}

// ToolImagesLabel (A-180) is the line before a tool result's images where a provider
// sends them outside the result (OpenAI-compatible and ChatGPT subscription user
// messages, Gemini parts), so the model knows which call they came from.
func ToolImagesLabel(r ToolResult) string {
	if len(r.Images) == 1 {
		return "The image returned by tool call " + r.ToolCallID + ":"
	}
	return fmt.Sprintf("The %d images returned by tool call %s:", len(r.Images), r.ToolCallID)
}

// DataURL is the attachment as a data: URL (OpenAI-style image input).
func (a MessageAttachment) DataURL() string {
	return "data:" + a.MIMEType + ";base64," + a.Data
}
