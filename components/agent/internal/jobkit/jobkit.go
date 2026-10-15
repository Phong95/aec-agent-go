// Package triggerkit holds the JSON value helpers that the trigger contract
// (components/agent/trigger) and the agent's trigger manager share (A-70): reading a
// path of an event, the subject and revision rules, the JS comparison of the filter,
// and the encoding that makes stored event data match what core/ts stores
// (JSON.stringify of the parsed value). It is internal so none of it becomes API.
//
// Update (A-126): package jobkit - shared by the job contract (components/agent/job)
// and the agent's job manager, now that triggers are jobs.
package jobkit

import (
	"bytes"
	"encoding/json"
	"errors"
	"math"
	"strconv"
	"strings"
	"unicode/utf16"

	"github.com/Phong95/aec-agent-go/internal/jsonx"
	"github.com/Phong95/aec-agent-go/internal/jsstr"
	"github.com/Phong95/aec-agent-go/kernel/ordered"
)

// Decode parses one JSON value of any kind, keeping the key order of objects
// (ordered.Object) and numbers as json.Number. Empty input is JSON null.
func Decode(raw []byte) (any, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil, nil
	}
	// ordered only decodes objects, so the value is wrapped in one.
	var wrapper ordered.Object
	buf := make([]byte, 0, len(raw)+6)
	buf = append(buf, `{"v":`...)
	buf = append(buf, raw...)
	buf = append(buf, '}')
	if err := wrapper.UnmarshalJSON(buf); err != nil {
		return nil, err
	}
	v, ok := wrapper.Get("v")
	if !ok || wrapper.Len() != 1 {
		return nil, errors.New("jobkit: not a single JSON value")
	}
	return v, nil
}

// Canonical returns v with every json.Number replaced by its float64 (a JS number),
// at every nesting level, so encoding it gives the bytes JSON.stringify gives for the
// parsed value ("1.50" becomes 1.5).
func Canonical(v any) any {
	switch x := v.(type) {
	case json.Number:
		if f, ok := Number(x); ok {
			return f
		}
		return nil
	case ordered.Object:
		out := ordered.NewObject()
		for k, e := range x.All() {
			out.Set(k, Canonical(e))
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = Canonical(e)
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, e := range x {
			out[k] = Canonical(e)
		}
		return out
	default:
		return v
	}
}

// Marshal encodes v as JSON the way JSON.stringify does (no HTML escaping, numbers as
// JS prints them); nil is null.
func Marshal(v any) (json.RawMessage, error) {
	return jsonx.Marshal(Canonical(v))
}

// ReadPath reads "type", "subject", "source" or "data.<dot.path>" of an event, the
// readPath of core/ts. ok is false when the value is undefined in JS terms (an empty
// subject, a missing key, a step into a value that is not an object or array). An
// array is indexed by a decimal step ("data.items.0").
func ReadPath(typ, subject, source string, data any, path string) (any, bool) {
	head, rest, _ := strings.Cut(path, ".")
	var value any
	switch head {
	case "type":
		value = typ
	case "subject":
		if subject == "" {
			return nil, false
		}
		value = subject
	case "source":
		value = source
	case "data":
		value = data
	default:
		return nil, false
	}
	if !strings.Contains(path, ".") {
		return value, true
	}
	for key := range strings.SplitSeq(rest, ".") {
		switch x := value.(type) {
		case ordered.Object:
			v, ok := x.Get(key)
			if !ok {
				return nil, false
			}
			value = v
		case map[string]any:
			v, ok := x[key]
			if !ok {
				return nil, false
			}
			value = v
		case []any:
			i, err := strconv.Atoi(key)
			if err != nil || i < 0 || i >= len(x) || strconv.Itoa(i) != key {
				return nil, false
			}
			value = x[i]
		default:
			return nil, false
		}
	}
	return value, true
}

// Number is the float64 of a numeric value (json.Number, a float or an integer type).
func Number(v any) (float64, bool) {
	switch x := v.(type) {
	case json.Number:
		f, err := strconv.ParseFloat(string(x), 64)
		if err != nil && !errors.Is(err, strconv.ErrRange) {
			return 0, false
		}
		return f, true
	case float64:
		return x, true
	case float32:
		return float64(x), true
	case int:
		return float64(x), true
	case int8:
		return float64(x), true
	case int16:
		return float64(x), true
	case int32:
		return float64(x), true
	case int64:
		return float64(x), true
	case uint:
		return float64(x), true
	case uint8:
		return float64(x), true
	case uint16:
		return float64(x), true
	case uint32:
		return float64(x), true
	case uint64:
		return float64(x), true
	default:
		return 0, false
	}
}

// FormatNumber is JS String(number).
func FormatNumber(f float64) string {
	switch {
	case math.IsNaN(f):
		return "NaN"
	case math.IsInf(f, 1):
		return "Infinity"
	case math.IsInf(f, -1):
		return "-Infinity"
	case f == 0:
		return "0"
	}
	abs := math.Abs(f)
	if abs >= 1e21 || abs < 1e-6 {
		s := strconv.FormatFloat(f, 'e', -1, 64)
		// Go writes "1e+21" and "1e-07"; JS writes "1e+21" and "1e-7".
		mant, exp, _ := strings.Cut(s, "e")
		sign := exp[:1]
		digits := strings.TrimLeft(exp[1:], "0")
		return mant + "e" + sign + digits
	}
	return strconv.FormatFloat(f, 'f', -1, 64)
}

// ToSubject is a subject read from an event field: a non-blank string (its first 200
// UTF-16 units) or a finite number.
func ToSubject(v any) (string, bool) {
	if s, ok := v.(string); ok {
		if jsstr.Trim(s) == "" {
			return "", false
		}
		return jsstr.TruncateUTF16(s, 200), true
	}
	if f, ok := Number(v); ok && !math.IsInf(f, 0) && !math.IsNaN(f) {
		return FormatNumber(f), true
	}
	return "", false
}

// ToRevision is a revision read from an event field: a finite number, or a decimal
// string of one.
func ToRevision(v any) (float64, bool) {
	if s, ok := v.(string); ok {
		t := jsstr.Trim(s)
		if t == "" {
			return 0, false
		}
		f, err := strconv.ParseFloat(t, 64)
		if err != nil || math.IsInf(f, 0) || math.IsNaN(f) {
			return 0, false
		}
		return f, true
	}
	if f, ok := Number(v); ok && !math.IsInf(f, 0) && !math.IsNaN(f) {
		return f, true
	}
	return 0, false
}

// SameValue is the filter's equality: primitives compare by value (JS ===), and a
// number and its decimal string are equal ("7" and 7), as JSON paths often mix them.
// Objects and arrays never compare equal (JS compares them by reference).
func SameValue(a, b any) bool {
	fa, aNum := Number(a)
	fb, bNum := Number(b)
	switch {
	case aNum && bNum:
		return fa == fb
	case aNum:
		s, ok := b.(string)
		return ok && FormatNumber(fa) == s
	case bNum:
		s, ok := a.(string)
		return ok && FormatNumber(fb) == s
	}
	return ordered.StrictEqual(a, b)
}

// UTF16Len is the JS length of s.
func UTF16Len(s string) int {
	n := 0
	for _, r := range s {
		if r >= 0x10000 {
			n += 2
		} else {
			n++
		}
	}
	return n
}

// SliceUTF16 is JS s.slice(0, n): the first n UTF-16 units of s (a surrogate pair cut
// in half keeps its first half, as JS does, encoded as U+FFFD by Go).
func SliceUTF16(s string, n int) string {
	if UTF16Len(s) <= n {
		return s
	}
	units := utf16.Encode([]rune(s))
	return string(utf16.Decode(units[:n]))
}

// CloneValue deep-copies the container types a decoded or Go-built JSON value can
// hold (ordered.Object, []any, map[string]any, []string).
func CloneValue(v any) any {
	switch x := v.(type) {
	case ordered.Object:
		return x.Clone()
	case []any:
		if x == nil {
			return x
		}
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = CloneValue(e)
		}
		return out
	case map[string]any:
		if x == nil {
			return x
		}
		out := make(map[string]any, len(x))
		for k, e := range x {
			out[k] = CloneValue(e)
		}
		return out
	case []string:
		if x == nil {
			return x
		}
		return append([]string(nil), x...)
	default:
		return v
	}
}
