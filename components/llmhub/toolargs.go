package llmhub

import (
	"encoding/json"
	"strings"

	"github.com/Phong95/aec-agent-go/internal/jsstr"
	"github.com/Phong95/aec-agent-go/kernel"
)

// rawPreviewUnits is how much of unusable arguments is quoted back to the model
// (and to traces), in UTF-16 code units.
const rawPreviewUnits = 200

// jsonWhitespace is the set JSON.parse skips around a value. It is not the JS
// trim set: a non-breaking space or U+FEFF before a value is invalid JSON.
const jsonWhitespace = " \t\r\n"

// ParseToolArguments turns the raw JSON text a model streamed for one tool call
// into the Arguments and ArgumentsError fields of a kernel.ToolCall. It never fails and never panics: a model can produce
// invalid JSON (an unescaped quote or newline in a long string) or a value that
// is not an object, and that must not fail the whole response. Such a call
// carries nil Arguments (encoded as {}) and an error text, which the harness
// answers with a tool error so the model can try again.
//
//   - text that is empty after JS trim (which also strips U+00A0 and U+FEFF) is
//     no arguments: nil, "";
//   - text that is not valid JSON gives nil and "the arguments are not valid
//     JSON (<reason>): <preview>". The reason is Go's encoding/json text;
//   - valid JSON that is not one object (null, an array, a string, a number, a
//     boolean) gives nil and "the arguments must be one JSON object: <preview>";
//   - one valid object gives its compact bytes from kernel.NewArguments and "".
//
// The preview is the first 200 UTF-16 code units of the raw text (units, not
// runes) followed by U+2026 when the text is longer.
// A cut that falls inside a surrogate pair drops the whole pair, because a Go
// string cannot hold half of one.
//
// Bytes that are not valid UTF-8 are read as U+FFFD first, as they would be in
// a JS string decoded from the same bytes, so neither the arguments nor the
// error text can carry invalid UTF-8.
func ParseToolArguments(raw string) (kernel.Arguments, string) {
	raw = strings.ToValidUTF8(raw, "\ufffd")
	if jsstr.Trim(raw) == "" {
		return nil, ""
	}
	text := []byte(strings.Trim(raw, jsonWhitespace))
	if !json.Valid(text) {
		reason := "invalid JSON"
		var v any
		if err := json.Unmarshal(text, &v); err != nil {
			reason = err.Error()
		}
		return nil, "the arguments are not valid JSON (" + reason + "): " + preview(raw)
	}
	// Valid JSON: an object starts with a brace, every other value (null, array,
	// string, number, boolean) does not. Decoding is not needed, and would fail
	// on a number such as 1e999 that JS reads as Infinity.
	if text[0] != '{' {
		return nil, notObject(raw)
	}
	args, err := kernel.NewArguments(text)
	if err != nil {
		return nil, notObject(raw)
	}
	return args, ""
}

// notObject is the error text for valid JSON that is not one object.
func notObject(raw string) string {
	return "the arguments must be one JSON object: " + preview(raw)
}

// preview is raw cut to rawPreviewUnits UTF-16 units, with an ellipsis when cut.
func preview(raw string) string {
	if kernel.UTF16Len(raw) > rawPreviewUnits {
		return jsstr.TruncateUTF16(raw, rawPreviewUnits) + "\u2026"
	}
	return raw
}
