// Package ordered holds the insertion-ordered JSON containers of the SDK: Map,
// an insertion-ordered, string-keyed map with a JSON codec, and Object, a JS
// plain object (see object.go). It is public, not internal, so that server/go,
// a separate module, can use the same types.
//
// It is the Go counterpart of the JS Map and of object-property order, which
// the tool hub, the registries and JSON schemas rely on: the order in which
// keys were first set is the order they are listed, encoded and iterated. A
// plain Go map would sort keys on the wire and iterate randomly, which changes
// both the bytes of a schema and which violation a policy reports first.
//
// A Map is not safe for concurrent mutation; its owner locks.
package ordered

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"iter"
	"slices"

	"github.com/Phong95/aec-agent-go/internal/jsonx"
)

// Map is an insertion-ordered map from string keys to V. The zero value is an
// empty map ready to use.
type Map[V any] struct {
	keys []string
	vals map[string]V
}

// New returns an empty Map.
func New[V any]() *Map[V] {
	return &Map[V]{}
}

// Set stores v under k. Setting an existing key replaces the value and keeps
// the key's position (JS Map.set); a new key is appended. It reports whether an
// existing entry was replaced.
func (m *Map[V]) Set(k string, v V) (replaced bool) {
	if m.vals == nil {
		m.vals = make(map[string]V)
	}
	if _, ok := m.vals[k]; ok {
		m.vals[k] = v
		return true
	}
	m.keys = append(m.keys, k)
	m.vals[k] = v
	return false
}

// Get returns the value stored under k.
func (m *Map[V]) Get(k string) (V, bool) {
	if m == nil {
		var zero V
		return zero, false
	}
	v, ok := m.vals[k]
	return v, ok
}

// Has reports whether k is present.
func (m *Map[V]) Has(k string) bool {
	_, ok := m.Get(k)
	return ok
}

// Delete removes k. A key that is set again afterwards is appended at the end.
func (m *Map[V]) Delete(k string) {
	if m == nil {
		return
	}
	if _, ok := m.vals[k]; !ok {
		return
	}
	delete(m.vals, k)
	if i := slices.Index(m.keys, k); i >= 0 {
		m.keys = slices.Delete(m.keys, i, i+1)
	}
}

// Len returns the number of entries.
func (m *Map[V]) Len() int {
	if m == nil {
		return 0
	}
	return len(m.keys)
}

// Keys returns a fresh slice of the keys in insertion order.
func (m *Map[V]) Keys() []string {
	if m == nil {
		return nil
	}
	return slices.Clone(m.keys)
}

// Values returns a fresh slice of the values in insertion order.
func (m *Map[V]) Values() []V {
	if m == nil {
		return nil
	}
	out := make([]V, 0, len(m.keys))
	for _, k := range m.keys {
		out = append(out, m.vals[k])
	}
	return out
}

// All iterates the entries in insertion order. The map must not be modified
// while the iteration runs.
func (m *Map[V]) All() iter.Seq2[string, V] {
	return func(yield func(string, V) bool) {
		if m == nil {
			return
		}
		for _, k := range m.keys {
			if !yield(k, m.vals[k]) {
				return
			}
		}
	}
}

// Clone returns a copy with the same key order and the same entries. The values
// are copied by assignment: a V that holds pointers, slices or maps is not
// deep-copied, so owners of such a V clone the values themselves.
func (m Map[V]) Clone() Map[V] {
	if m.vals == nil {
		return Map[V]{}
	}
	out := Map[V]{keys: slices.Clone(m.keys), vals: make(map[string]V, len(m.vals))}
	for k, v := range m.vals {
		out.vals[k] = v
	}
	return out
}

// MarshalJSON writes the entries as a JSON object in insertion order. An empty
// map writes {}. Keys and values are encoded with jsonx so the bytes match
// JSON.stringify.
//
// It has a value receiver on purpose: encoding/json ignores a pointer-receiver
// marshaler on a field it cannot address, which would write {} for a Map held by
// value in a struct. A *Map reaches it through the method set as well.
func (m Map[V]) MarshalJSON() ([]byte, error) {
	if len(m.keys) == 0 {
		return []byte("{}"), nil
	}
	var buf bytes.Buffer
	buf.WriteByte('{')
	for i, k := range m.keys {
		if i > 0 {
			buf.WriteByte(',')
		}
		kb, err := jsonx.Marshal(k)
		if err != nil {
			return nil, err
		}
		vb, err := jsonx.Marshal(m.vals[k])
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

// UnmarshalJSON replaces the contents with the entries of a JSON object, in
// the order they appear. Each value is decoded strictly (an unknown field
// inside a nested value fails). A duplicate key keeps its first position with
// the last value, as JSON.parse does. Any input that is not an object is an
// error. On error the map is left unchanged.
//
// The input null leaves the receiver unchanged, like Object.UnmarshalJSON (the
// encoding/json convention for an addressable non-pointer field), so a record
// that carries "metadata": null decodes to the empty map instead of failing.
func (m *Map[V]) UnmarshalJSON(data []byte) error {
	if bytes.Equal(bytes.TrimSpace(data), []byte("null")) {
		return nil
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return errors.New("ordered: JSON value is not an object")
	}
	var next Map[V]
	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			return err
		}
		key, ok := keyTok.(string)
		if !ok {
			return errors.New("ordered: JSON object key is not a string")
		}
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return err
		}
		var v V
		if err := jsonx.UnmarshalStrict(raw, &v); err != nil {
			return err
		}
		next.Set(key, v)
	}
	if _, err := dec.Token(); err != nil { // the closing brace
		return err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return errors.New("ordered: unexpected data after the JSON object")
	}
	*m = next
	return nil
}
