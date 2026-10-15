package ordered

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"iter"
	"slices"
	"strconv"

	"github.com/Phong95/aec-agent-go/internal/jsonx"
)

// MaxDepth is the deepest nesting (arrays and objects counted together, the
// top-level object being depth 1) that Object.UnmarshalJSON accepts. It is a
// documented divergence: JSON.parse is limited only by the engine stack, while
// metadata and tool output can come from model- or tool-produced JSON of
// untrusted nesting, so the Go decoder returns an error instead of recursing
// without bound.
const MaxDepth = 1000

// Object is a JS plain object: string keys in insertion order and JSON-native
// values. It is the type of every free-form object field that is persisted or
// exchanged on the wire (Session.Metadata, SessionFilter.Metadata,
// ToolOutput.metadata, SandboxSpec.options), so the key order of the value
// survives a decode and an encode at every nesting level.
//
// Decoded values are only nil, bool, string, json.Number, []any and Object, never
// map[string]any: a json.Number keeps the exact spelling of a number, and an
// Object keeps the order of a nested object. A Go author may store other
// JSON-encodable values with Set.
//
// Set does NOT apply the JS integer-like-key ordering (JS lists keys such as "1"
// and "2" first, ascending, whatever the insertion order): a Go author controls
// the order. The SKILL.md parser applies the JS order itself.
//
// The zero Object is absent (IsZero is true) and encodes as {}; NewObject makes
// an explicit empty object. An Object is not safe for concurrent mutation; its
// owner locks.
type Object struct {
	keys []string
	vals map[string]any
}

// NewObject returns an explicit empty object. Unlike the zero Object it is not
// IsZero, so an omitzero field keeps its {}.
func NewObject() Object {
	return Object{vals: make(map[string]any)}
}

// Set stores v under k. An existing key keeps its position and gets the new
// value, a new key is appended (JS property assignment). It works on the zero
// Object.
func (o *Object) Set(k string, v any) {
	if o.vals == nil {
		o.vals = make(map[string]any)
	}
	if _, ok := o.vals[k]; !ok {
		o.keys = append(o.keys, k)
	}
	o.vals[k] = v
}

// Delete removes k. A key that is set again afterwards is appended at the end.
func (o *Object) Delete(k string) {
	if _, ok := o.vals[k]; !ok {
		return
	}
	delete(o.vals, k)
	if i := slices.Index(o.keys, k); i >= 0 {
		o.keys = slices.Delete(o.keys, i, i+1)
	}
}

// Get returns the value stored under k.
func (o Object) Get(k string) (any, bool) {
	v, ok := o.vals[k]
	return v, ok
}

// Has reports whether k is present.
func (o Object) Has(k string) bool {
	_, ok := o.vals[k]
	return ok
}

// Len returns the number of entries.
func (o Object) Len() int {
	return len(o.keys)
}

// Keys returns a fresh slice of the keys in insertion order.
func (o Object) Keys() []string {
	return slices.Clone(o.keys)
}

// All iterates the entries in insertion order. The object must not be modified
// while the iteration runs.
func (o Object) All() iter.Seq2[string, any] {
	return func(yield func(string, any) bool) {
		for _, k := range o.keys {
			if !yield(k, o.vals[k]) {
				return
			}
		}
	}
}

// IsZero reports whether the object is absent: true only for an Object that was
// never initialised, so an omitzero field drops an absent object and keeps an
// explicit {}: an absent optional property is not the same as an empty one.
func (o Object) IsZero() bool {
	return o.vals == nil
}

// Clone returns a deep copy: nested Object, []any, map[string]any and []string
// values are copied, scalars and json.Number by value. Cloning the zero Object
// gives the zero Object.
func (o Object) Clone() Object {
	if o.vals == nil {
		return Object{}
	}
	out := Object{keys: slices.Clone(o.keys), vals: make(map[string]any, len(o.vals))}
	for k, v := range o.vals {
		out.vals[k] = cloneValue(v)
	}
	return out
}

// cloneValue deep-copies the container types an Object can hold.
func cloneValue(v any) any {
	switch x := v.(type) {
	case Object:
		return x.Clone()
	case []any:
		if x == nil {
			return x
		}
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = cloneValue(e)
		}
		return out
	case map[string]any:
		if x == nil {
			return x
		}
		out := make(map[string]any, len(x))
		for k, e := range x {
			out[k] = cloneValue(e)
		}
		return out
	case []string:
		return slices.Clone(x)
	default:
		return v
	}
}

// MarshalJSON writes the entries as a JSON object in insertion order, each key
// and each value encoded with jsonx so the bytes match JSON.stringify (NaN or Inf
// inside an any becomes null). A zero Object writes {}.
//
// It has a value receiver on purpose: encoding/json ignores a pointer-receiver
// marshaler on a field it cannot address, which would write {} for an Object held
// by value in a struct field or inside a []any.
func (o Object) MarshalJSON() ([]byte, error) {
	if len(o.keys) == 0 {
		return []byte("{}"), nil
	}
	var buf bytes.Buffer
	buf.WriteByte('{')
	for i, k := range o.keys {
		if i > 0 {
			buf.WriteByte(',')
		}
		kb, err := jsonx.Marshal(k)
		if err != nil {
			return nil, err
		}
		vb, err := jsonx.Marshal(o.vals[k])
		if err != nil {
			return nil, err
		}
		buf.Write(kb)
		buf.WriteByte(':')
		buf.Write(vb)
	}
	buf.WriteByte('}')
	return buf.Bytes(), nil
}

// UnmarshalJSON replaces the contents with the entries of a JSON object, in the
// order they appear, at every nesting level. Numbers keep their spelling as
// json.Number. A duplicate key keeps its first position with the last value, as
// JSON.parse does. The input null leaves the receiver unchanged (the
// encoding/json convention). Any other input that is not an object, trailing
// data, malformed JSON, or a nesting depth above MaxDepth is an error prefixed
// "ordered: ", and the receiver is left unchanged.
func (o *Object) UnmarshalJSON(data []byte) error {
	if bytes.Equal(bytes.TrimSpace(data), []byte("null")) {
		return nil
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	tok, err := dec.Token()
	if err != nil {
		return decodeErr(err)
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return errors.New("ordered: JSON value is not an object")
	}
	next, err := decodeObjectBody(dec, 1)
	if err != nil {
		return err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return errors.New("ordered: unexpected data after the JSON object")
	}
	*o = next
	return nil
}

// decodeErr prefixes a decoder error and turns a bare io.EOF (input that ends
// inside a value) into io.ErrUnexpectedEOF.
func decodeErr(err error) error {
	if errors.Is(err, io.EOF) {
		err = io.ErrUnexpectedEOF
	}
	return fmt.Errorf("ordered: %w", err)
}

// decodeValue reads one JSON value; depth is the depth of the container that
// holds it.
func decodeValue(dec *json.Decoder, depth int) (any, error) {
	tok, err := dec.Token()
	if err != nil {
		return nil, decodeErr(err)
	}
	d, ok := tok.(json.Delim)
	if !ok {
		return tok, nil // nil, bool, string or json.Number
	}
	switch d {
	case '{':
		return decodeObjectBody(dec, depth+1)
	case '[':
		return decodeArrayBody(dec, depth+1)
	default:
		return nil, errors.New("ordered: unexpected closing delimiter")
	}
}

// decodeObjectBody reads the entries of an object whose opening brace was
// consumed; depth is the depth of that object.
func decodeObjectBody(dec *json.Decoder, depth int) (Object, error) {
	if depth > MaxDepth {
		return Object{}, fmt.Errorf("ordered: JSON nesting is deeper than %d levels", MaxDepth)
	}
	out := NewObject()
	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			return Object{}, decodeErr(err)
		}
		key, ok := keyTok.(string)
		if !ok {
			return Object{}, errors.New("ordered: JSON object key is not a string")
		}
		v, err := decodeValue(dec, depth)
		if err != nil {
			return Object{}, err
		}
		out.Set(key, v)
	}
	if tok, err := dec.Token(); err != nil { // the closing brace
		return Object{}, decodeErr(err)
	} else if d, ok := tok.(json.Delim); !ok || d != '}' {
		return Object{}, errors.New("ordered: malformed JSON object")
	}
	return out, nil
}

// decodeArrayBody reads the elements of an array whose opening bracket was
// consumed; depth is the depth of that array. An empty array is a non-nil empty
// slice so it encodes as [].
func decodeArrayBody(dec *json.Decoder, depth int) ([]any, error) {
	if depth > MaxDepth {
		return nil, fmt.Errorf("ordered: JSON nesting is deeper than %d levels", MaxDepth)
	}
	out := []any{}
	for dec.More() {
		v, err := decodeValue(dec, depth)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	if tok, err := dec.Token(); err != nil { // the closing bracket
		return nil, decodeErr(err)
	} else if d, ok := tok.(json.Delim); !ok || d != ']' {
		return nil, errors.New("ordered: malformed JSON array")
	}
	return out, nil
}

// StrictEqual is JS === on decoded JSON values, the comparison used to filter
// sessions by metadata. nil equals nil; bools and strings are equal by value;
// numbers are equal numerically across json.Number, int, int64 and float64 (a JS
// number is a double); an Object, a []any or a map is never equal to anything,
// because JS compares those by reference. It never panics.
func StrictEqual(a, b any) bool {
	switch x := a.(type) {
	case nil:
		return b == nil
	case bool:
		y, ok := b.(bool)
		return ok && x == y
	case string:
		y, ok := b.(string)
		return ok && x == y
	default:
		fa, okA := asNumber(a)
		fb, okB := asNumber(b)
		return okA && okB && fa == fb
	}
}

// asNumber converts the numeric forms a decoded or Go-built value can have to a
// float64 (JS number). NaN compares unequal to itself, like JS.
func asNumber(v any) (float64, bool) {
	switch x := v.(type) {
	case json.Number:
		f, err := strconv.ParseFloat(string(x), 64)
		if err != nil && !errors.Is(err, strconv.ErrRange) {
			return 0, false
		}
		return f, true
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
	case float32:
		return float64(x), true
	case float64:
		return x, true
	default:
		return 0, false
	}
}
