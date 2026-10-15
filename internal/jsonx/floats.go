package jsonx

import (
	"encoding"
	"encoding/json"
	"math"
	"reflect"
	"sync"
)

// JSON.stringify writes NaN and +/-Infinity as null and -0 as 0. encoding/json
// fails on the first and writes -0 for the second. normalizeFloats rebuilds the
// parts of a value that need it so Marshal matches JS:
//
//   - a float reached through an interface (any, map[string]any, []any, a
//     struct field of type any): NaN and +/-Inf become nil (null), -0 becomes 0;
//   - a typed float field, element or map value (float64 in a struct): -0 becomes
//     0. NaN and +/-Inf cannot become null without changing the declared type, so
//     they still make Marshal return an error.
//
// The caller's value is never mutated: containers on a changed path are copied,
// untouched ones are shared. Types that implement json.Marshaler or
// encoding.TextMarshaler are not entered; they own their encoding (and the
// MarshalJSON methods of this module call Marshal again themselves).

var (
	marshalerType     = reflect.TypeFor[json.Marshaler]()
	textMarshalerType = reflect.TypeFor[encoding.TextMarshaler]()
	needsWalkCache    sync.Map // reflect.Type -> bool
)

// maxWalkDepth stops the walk on a cyclic value; encoding/json reports the
// cycle itself, with a proper error, when it encodes the unchanged value.
const maxWalkDepth = 1000

// normalizeFloats returns v with floats normalised as described above, or v
// itself when nothing needs changing.
func normalizeFloats(v any) any {
	if v == nil {
		return v
	}
	if !needsWalk(reflect.TypeOf(v)) {
		return v
	}
	iv := reflect.ValueOf(&v).Elem() // an interface-kind Value: v is in an "any" position
	out, changed := normalize(iv, 0)
	if !changed {
		return v
	}
	if out.IsNil() {
		return nil
	}
	return out.Interface()
}

func ownsEncoding(t reflect.Type) bool {
	return t.Implements(marshalerType) || reflect.PointerTo(t).Implements(marshalerType) ||
		t.Implements(textMarshalerType) || reflect.PointerTo(t).Implements(textMarshalerType)
}

// needsWalk reports whether a value of type t can contain a float or an
// interface value, i.e. whether normalize can find anything to change.
func needsWalk(t reflect.Type) bool {
	if c, ok := needsWalkCache.Load(t); ok {
		return c.(bool)
	}
	res := computeNeedsWalk(t, map[reflect.Type]bool{})
	needsWalkCache.Store(t, res)
	return res
}

func computeNeedsWalk(t reflect.Type, seen map[reflect.Type]bool) bool {
	if seen[t] {
		return false // a cycle adds nothing the first visit did not find
	}
	seen[t] = true
	switch t.Kind() {
	case reflect.Float32, reflect.Float64, reflect.Interface:
		return true
	case reflect.Pointer, reflect.Slice, reflect.Array, reflect.Map:
		if ownsEncoding(t) {
			return false
		}
		return computeNeedsWalk(t.Elem(), seen)
	case reflect.Struct:
		if ownsEncoding(t) {
			return false
		}
		for i := range t.NumField() {
			f := t.Field(i)
			if f.IsExported() && computeNeedsWalk(f.Type, seen) {
				return true
			}
		}
	default:
	}
	return false
}

func isFloat(k reflect.Kind) bool { return k == reflect.Float32 || k == reflect.Float64 }

// normalize returns the normalised form of v and whether it differs from v.
func normalize(v reflect.Value, depth int) (reflect.Value, bool) {
	if depth > maxWalkDepth {
		return v, false
	}
	t := v.Type()
	switch t.Kind() {
	case reflect.Float32, reflect.Float64:
		if f := v.Float(); f == 0 && math.Signbit(f) {
			return reflect.Zero(t), true
		}
		return v, false

	case reflect.Interface:
		if v.IsNil() {
			return v, false
		}
		e := v.Elem()
		if isFloat(e.Kind()) {
			if f := e.Float(); math.IsNaN(f) || math.IsInf(f, 0) {
				return reflect.Zero(t), true // JS: null
			}
		}
		if !needsWalk(e.Type()) {
			return v, false
		}
		n, changed := normalize(e, depth+1)
		if !changed {
			return v, false
		}
		out := reflect.New(t).Elem()
		out.Set(n)
		return out, true

	case reflect.Pointer:
		if v.IsNil() || ownsEncoding(t) || !needsWalk(t.Elem()) {
			return v, false
		}
		n, changed := normalize(v.Elem(), depth+1)
		if !changed {
			return v, false
		}
		p := reflect.New(t.Elem())
		p.Elem().Set(n)
		return p, true

	case reflect.Slice:
		if v.IsNil() || ownsEncoding(t) || !needsWalk(t.Elem()) {
			return v, false
		}
		var out reflect.Value
		for i := range v.Len() {
			n, changed := normalize(v.Index(i), depth+1)
			if !changed {
				continue
			}
			if !out.IsValid() {
				out = reflect.MakeSlice(t, v.Len(), v.Len())
				reflect.Copy(out, v)
			}
			out.Index(i).Set(n)
		}
		return out, out.IsValid()

	case reflect.Array:
		if ownsEncoding(t) || !needsWalk(t.Elem()) {
			return v, false
		}
		var out reflect.Value
		for i := range v.Len() {
			n, changed := normalize(v.Index(i), depth+1)
			if !changed {
				continue
			}
			if !out.IsValid() {
				out = reflect.New(t).Elem()
				out.Set(v)
			}
			out.Index(i).Set(n)
		}
		return out, out.IsValid()

	case reflect.Map:
		if v.IsNil() || ownsEncoding(t) || !needsWalk(t.Elem()) {
			return v, false
		}
		type change struct{ key, val reflect.Value }
		var changes []change
		for it := v.MapRange(); it.Next(); {
			if n, changed := normalize(it.Value(), depth+1); changed {
				changes = append(changes, change{it.Key(), n})
			}
		}
		if len(changes) == 0 {
			return v, false
		}
		out := reflect.MakeMapWithSize(t, v.Len())
		for it := v.MapRange(); it.Next(); {
			out.SetMapIndex(it.Key(), it.Value())
		}
		for _, c := range changes {
			out.SetMapIndex(c.key, c.val)
		}
		return out, true

	case reflect.Struct:
		if ownsEncoding(t) {
			return v, false
		}
		var out reflect.Value
		for i := range t.NumField() {
			if !t.Field(i).IsExported() {
				continue
			}
			n, changed := normalize(v.Field(i), depth+1)
			if !changed {
				continue
			}
			if !out.IsValid() {
				out = reflect.New(t).Elem()
				out.Set(v)
			}
			out.Field(i).Set(n)
		}
		return out, out.IsValid()

	default:
		return v, false
	}
}
