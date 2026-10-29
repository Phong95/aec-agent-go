package llmhub

import "github.com/Phong95/aec-agent-go/kernel"

// EstimateTokens is the token estimate shared by the CountTokens method of all
// four providers: for every message the UTF-16 length of its
// content, plus the UTF-16 length of the JSON of its tool calls and of its tool
// results when it has any, summed, divided by 4 and rounded up.
//
// Lengths are UTF-16 code units: an emoji counts 2. A nil ToolCalls or ToolResults slice adds nothing and an empty
// non-nil one adds the two characters of "[]". The JSON is the wire encoding (kernel.EncodeJSON), so a call that
// carries an ArgumentsError counts it. A part that cannot be encoded (arguments
// that are not valid JSON, which no provider builds) adds nothing, because an
// estimate has no error to return.
//
// The provider CountTokens methods return the bare ctx.Err() first when the
// context is done, then call this.
func EstimateTokens(msgs []kernel.Message) int {
	total := 0
	for _, m := range msgs {
		total += kernel.UTF16Len(m.Content)
		if m.ToolCalls != nil {
			if b, err := kernel.EncodeJSON(m.ToolCalls); err == nil {
				total += kernel.UTF16Len(string(b))
			}
		}
		if m.ToolResults != nil {
			if b, err := kernel.EncodeJSON(m.ToolResults); err == nil {
				total += kernel.UTF16Len(string(b))
			}
		}
	}
	// A-165: an image is counted as kernel.ImageTokenEstimate, never by its base64 size.
	images := 0
	for _, m := range msgs {
		images += len(m.Images())
	}
	return (total+3)/4 + images*kernel.ImageTokenEstimate
}
