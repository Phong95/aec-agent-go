// Package jsonx is the single choke point for JSON that matches JSON.stringify
// byte for byte.
//
// JSON.stringify and encoding/json differ in ways that break byte-for-byte
// equality: Go escapes <, > and & as < style sequences by default, always
// escapes U+2028/U+2029, and appends a trailing newline in Encoder.Encode.
// A fourth difference depends on the Go release: for a Go string holding
// invalid UTF-8, Go 1.25 writes the 6-character escape of U+FFFD for every
// invalid byte and Go 1.27 writes the raw character, while JSON.stringify of
// the decoded JS string writes the raw character (gap G-12-1).
// Marshal removes all four differences.
//
// Every MarshalJSON method in this module must call jsonx.Marshal, never the
// stdlib top-level marshal function. A MarshalJSON that calls the stdlib
// function internally leaks the HTML escape for '<' into the output and the
// outer encoder (even with SetEscapeHTML(false)) does not undo it.
package jsonx

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
)

// Marshal encodes v as compact JSON exactly the way JSON.stringify would:
// no HTML escaping, raw U+2028/U+2029, no trailing newline, and floats reached
// through an interface value written the JS way (NaN and +/-Inf as null, -0 as
// 0; see floats.go for the exact scope). Invalid UTF-8 in a Go string is written
// as one raw U+FFFD per invalid byte on every Go release.
func Marshal(v any) ([]byte, error) {
	v = normalizeFloats(v)
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false) // Go would write '<' as a < escape; JS does not
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	out := bytes.TrimSuffix(buf.Bytes(), []byte{'\n'}) // Encode appends '\n'; JSON.stringify does not
	return restoreLineSeparators(out), nil
}

// NormalizeLineSeparators rewrites the 6-character escapes for U+2028, U+2029
// and U+FFFD (either letter case in the hex digits) in an already-encoded JSON
// value to raw UTF-8, the form JSON.stringify writes. The name predates the
// U+FFFD case (gap G-12-1). Marshal applies the same rewrite to everything it
// encodes, so a raw JSON value (json.RawMessage, kernel.Arguments) that is run
// through this function first has the same bytes before and after an encode.
// The input is never modified; when it holds no such escape it is returned as is.
func NormalizeLineSeparators(b []byte) []byte {
	return restoreLineSeparators(b)
}

// restoreLineSeparators turns the 6-character escapes for U+2028 and U+2029
// back into raw UTF-8 bytes. Go ALWAYS escapes them (even with
// SetEscapeHTML(false)); JSON.stringify never does. The 6-character escape of
// U+FFFD is rewritten the same way, because Go 1.25 writes it for every invalid
// UTF-8 byte and Go 1.27 writes the raw character: rewriting makes the bytes the
// same on every Go release and equal to JSON.stringify's (gap G-12-1). An escape
// inside a raw value is rewritten too, because an encoder cannot tell it from one
// it wrote itself. The scan is escape-aware so that an escaped backslash
// followed by the text u2028 or ufffd is left untouched. The input slice itself
// is returned when nothing was rewritten.
func restoreLineSeparators(b []byte) []byte {
	var out []byte // allocated at the first rewrite, so an untouched value is never copied
	for i := 0; i < len(b); i++ {
		if b[i] != '\\' || i+1 >= len(b) {
			if out != nil {
				out = append(out, b[i])
			}
			continue
		}
		if raw := rawForEscape(b[i:]); raw != nil {
			if out == nil {
				out = make([]byte, 0, len(b))
				out = append(out, b[:i]...)
			}
			out = append(out, raw...)
			i += 5
			continue
		}
		if out != nil {
			out = append(out, b[i], b[i+1]) // copy the escaped pair whole (handles `\\`)
		}
		i++
	}
	if out == nil {
		return b
	}
	return out
}

// rawForEscape returns the raw UTF-8 bytes for the 6-character escape at the
// start of b (U+2028 = E2 80 A8, U+2029 = E2 80 A9, U+FFFD = EF BF BD), or nil
// when b does not start with one of those three escapes.
func rawForEscape(b []byte) []byte {
	if len(b) < 6 || b[0] != '\\' || b[1] != 'u' {
		return nil
	}
	switch {
	case b[2] == '2' && b[3] == '0' && b[4] == '2' && b[5] == '8':
		return []byte{0xE2, 0x80, 0xA8}
	case b[2] == '2' && b[3] == '0' && b[4] == '2' && b[5] == '9':
		return []byte{0xE2, 0x80, 0xA9}
	case (b[2] == 'f' || b[2] == 'F') && (b[3] == 'f' || b[3] == 'F') && (b[4] == 'f' || b[4] == 'F') && (b[5] == 'd' || b[5] == 'D'):
		return []byte{0xEF, 0xBF, 0xBD}
	}
	return nil
}

// UnmarshalStrict decodes data into v, rejecting unknown fields and any
// non-whitespace data after the first JSON value. A field added to a recorded
// fixture therefore fails Go tests instead of being silently dropped.
//
// "Strict" has two limits inherited from encoding/json: object keys match
// struct fields case-insensitively (a key "Type" fills a field tagged "type"),
// and a duplicate key is accepted, the last one winning. Byte-identical
// round-trip tests catch both, because the re-encode would differ from the input.
func UnmarshalStrict(data []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("jsonx: unexpected data after top-level value")
		}
		return err
	}
	return nil
}
