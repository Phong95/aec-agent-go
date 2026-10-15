package toolkit

import (
	"encoding/json"
	"math"
	"strconv"

	"github.com/Phong95/aec-agent-go/kernel"
)

// Args is the decoded top level of a tool call's arguments object. Every key keeps its
// raw JSON, and a getter decodes only the key it was asked for. That is the point of the
// type: decoding the whole object into a map[string]any fails as a whole when ONE value
// is out of range (a number such as 1e999 that JS reads as Infinity), and a tool that
// returns on that error would lose every other argument. Args keeps them all and only
// the getter of the one field rejects it.
//
// The zero Args is an empty argument object. An Args is read-only and safe for
// concurrent use.
type Args struct {
	raw map[string]json.RawMessage
}

// DecodeArgs reads the argument object of a tool call once. Invalid JSON and a value
// that is not an object give an empty Args (a tool also runs with no model behind it,
// in tests and in scripts), so every getter then reports the key as absent.
func DecodeArgs(args kernel.Arguments) Args {
	var raw map[string]json.RawMessage
	if err := args.Decode(&raw); err != nil {
		return Args{}
	}
	return Args{raw: raw}
}

// Has reports whether the key is present, whatever its value (a JSON null counts).
func (a Args) Has(key string) bool {
	_, ok := a.raw[key]
	return ok
}

// String returns the value of key when it is a JSON string, and false for an absent
// key, a null and any other kind of value (JS typeof args[key] === "string"). The string
// is returned as it is: a caller that wants the JS trim() applies jsstr.Trim.
func (a Args) String(key string) (string, bool) {
	raw, ok := a.raw[key]
	if !ok || !isKind(raw, '"') {
		return "", false
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return "", false
	}
	return s, true
}

// Number returns the value of key when it is a finite JSON number (JS typeof value ===
// "number" && Number.isFinite(value)). A string, a null, an absent key, and a number
// beyond the double range, which JS reads as Infinity, all give false.
func (a Args) Number(key string) (float64, bool) {
	raw, ok := a.raw[key]
	if !ok || len(raw) == 0 {
		return 0, false
	}
	// Only a JSON number literal starts with a digit or a minus sign. Decoding a quoted
	// "5" into a json.Number would succeed, and a string is not a number here.
	if c := raw[0]; c != '-' && (c < '0' || c > '9') {
		return 0, false
	}
	f, err := strconv.ParseFloat(string(raw), 64)
	if err != nil || math.IsInf(f, 0) || math.IsNaN(f) {
		return 0, false
	}
	return f, true
}

// Bool returns the value of key when it is the JSON true or false.
func (a Args) Bool(key string) (bool, bool) {
	raw, ok := a.raw[key]
	if !ok {
		return false, false
	}
	switch string(raw) {
	case "true":
		return true, true
	case "false":
		return false, true
	default:
		return false, false
	}
}

// Strings returns the string elements of the JSON array under key, in order; an
// element that is not a string is dropped (JS Array.isArray(value) then a typeof filter).
// It returns nil when the key is absent or not an array, and a non-nil empty slice for
// an array with no string in it.
func (a Args) Strings(key string) []string {
	raw, ok := a.raw[key]
	if !ok || !isKind(raw, '[') {
		return nil
	}
	var elems []json.RawMessage
	if err := json.Unmarshal(raw, &elems); err != nil {
		return nil
	}
	out := make([]string, 0, len(elems))
	for _, e := range elems {
		if !isKind(e, '"') {
			continue
		}
		var s string
		if err := json.Unmarshal(e, &s); err == nil {
			out = append(out, s)
		}
	}
	return out
}

// Raw returns the raw JSON of key, for a tool that reads a structured value (a list of
// objects) the other getters do not cover. The bool is false when the key is absent; a JSON
// null is present and returns the bytes null. The slice is a copy.
func (a Args) Raw(key string) (json.RawMessage, bool) {
	raw, ok := a.raw[key]
	if !ok {
		return nil, false
	}
	return append(json.RawMessage(nil), raw...), true
}

// isKind reports whether the raw JSON value starts with the byte. json.RawMessage values
// taken from a decoded object carry no leading white space, so the first byte tells the
// kind of the value.
func isKind(raw json.RawMessage, first byte) bool {
	return len(raw) > 0 && raw[0] == first
}
