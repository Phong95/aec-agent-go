package kernel

import (
	"bytes"
	"encoding/json"
	"errors"

	"github.com/Phong95/aec-agent-go/internal/jsonx"
)

// Arguments holds the exact JSON bytes of a tool call's arguments object as
// the model sent them. Keeping raw bytes preserves key order, string escaping
// and number spellings (0, 1.5, 1e+21) that decoding into map[string]any would
// lose. It is decided in Phase 4 so that the exact-bytes requirement of the
// tool layer (TLS-07) needs no breaking change later.
//
// An Arguments value is immutable once built: it is a slice, so code that
// receives one must never modify it in place. Providers build it with
// NewArguments, the single sanctioned constructor, which guarantees a JSON
// object in compact form.
type Arguments json.RawMessage

// errArgumentsNotObject is the text shared by every rejection of a non-object
// arguments value.
const errArgumentsNotObject = "kernel: tool call arguments must be a JSON object"

// jsonSpace is the set of insignificant JSON whitespace bytes. Trimming with
// it, instead of bytes.TrimSpace, keeps Unicode spaces such as U+00A0 from
// being mistaken for whitespace.
const jsonSpace = " \t\r\n"

// NewArguments builds Arguments from the raw argument text a provider received
// from the model. It is the provider boundary of the exact-bytes rule (TLS-07):
//
//   - empty input (after trimming JSON whitespace) and the literal null give an
//     empty Arguments, which encodes as {};
//   - input that is not valid JSON is an error;
//   - a valid value that is not an object (array, string, number, boolean) is an
//     error, because tool arguments are always an object and a stored non-object
//     would make the persisted Message undecodable;
//   - a valid object is returned as a freshly allocated compact copy. Compacting
//     removes only insignificant whitespace outside strings and never escapes
//     <, > or & or the U+2028 separator, so the bytes a tool receives equal the
//     bytes that are persisted. Key order, number spellings and zero values are
//     untouched.
//
// One normalisation is deliberate: a 6-character escape of U+2028, U+2029 or
// U+FFFD (either letter case in the hex digits) inside a string is rewritten to
// the raw UTF-8 character. The encoders of this module write those characters
// raw, as JSON.stringify does (jsonx rewrites the U+FFFD escape on every encode
// so that Go 1.25, Go 1.27 and JSON.stringify agree, gap G-12-1), and cannot
// tell an escape that was already in a raw value from one they introduced
// themselves; normalising here is what keeps the executed bytes equal to the
// persisted bytes. The string value is identical, and it is what a JSON parse
// followed by JSON.stringify produces.
func NewArguments(raw []byte) (Arguments, error) {
	trimmed := bytes.Trim(raw, jsonSpace)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return nil, nil
	}
	if !json.Valid(trimmed) {
		return nil, errors.New("kernel: tool call arguments are not valid JSON")
	}
	if trimmed[0] != '{' {
		return nil, errors.New(errArgumentsNotObject)
	}
	var buf bytes.Buffer
	buf.Grow(len(trimmed))
	if err := json.Compact(&buf, trimmed); err != nil {
		return nil, err
	}
	return Arguments(jsonx.NormalizeLineSeparators(buf.Bytes())), nil
}

// Decode unmarshals the arguments into v, which must be a non-nil pointer. An
// empty Arguments decodes as {}. Decoding is lenient: unknown keys are ignored,
// because a model may send extra ones. Non-object bytes return an error. The
// receiver is never modified.
func (a Arguments) Decode(v any) error {
	trimmed := bytes.Trim(a, jsonSpace)
	if len(trimmed) == 0 {
		trimmed = []byte("{}")
	}
	if trimmed[0] != '{' {
		return errors.New(errArgumentsNotObject)
	}
	return json.Unmarshal(trimmed, v)
}

// MarshalJSON returns {} when empty, else the stored bytes unchanged.
func (a Arguments) MarshalJSON() ([]byte, error) {
	if len(a) == 0 {
		return []byte("{}"), nil
	}
	return a, nil
}

// UnmarshalJSON copies the input bytes into a fresh slice: the decoder reuses
// its buffer, so keeping a reference would corrupt the value, and appending
// into the receiver's old backing array would overwrite earlier copies of the
// value. A JSON null is treated as an empty value (it re-encodes as {}); any
// other non-object value is rejected, because tool arguments are always an
// object. It never panics; malformed input is rejected by the surrounding
// decoder.
//
// Like NewArguments it rewrites an escaped U+2028, U+2029 or U+FFFD to the raw
// character so that decoding and re-encoding gives the same bytes.
func (a *Arguments) UnmarshalJSON(data []byte) error {
	trimmed := bytes.Trim(data, jsonSpace)
	if bytes.Equal(trimmed, []byte("null")) {
		*a = nil
		return nil
	}
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return errors.New("kernel: tool call arguments must be a JSON object")
	}
	*a = Arguments(jsonx.NormalizeLineSeparators(append([]byte(nil), trimmed...)))
	return nil
}
