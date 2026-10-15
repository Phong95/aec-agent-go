package trace

import (
	"encoding/json"
	"math"
	"reflect"
	"strconv"
)

// jsTruthy reports whether v would be truthy in JavaScript. The span status
// rule `attributes["error"] ? "error" : "ok"` depends on it, so a plain
// `v != nil` check is wrong.
//
// Falsy: nil, false, "", 0 (every integer and float kind, including -0), NaN,
// a json.Number that parses to 0 or NaN, and a nil pointer, map or slice (a
// nil map or slice encodes as JSON null).
//
// Truthy: any other string (including "0" and "false"), any other number
// (including +Inf), true, a non-nil pointer, and maps, slices and structs,
// including empty ones (JS: {} and [] are truthy).
func jsTruthy(v any) bool {
	if v == nil {
		return false
	}
	if n, ok := v.(json.Number); ok {
		f, err := strconv.ParseFloat(n.String(), 64)
		if err != nil {
			return n.String() != "" // not a number: treat as a non-empty string
		}
		return f != 0 && !math.IsNaN(f)
	}
	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.Bool:
		return rv.Bool()
	case reflect.String:
		return rv.Len() > 0
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return rv.Int() != 0
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		return rv.Uint() != 0
	case reflect.Float32, reflect.Float64:
		f := rv.Float()
		return f != 0 && !math.IsNaN(f)
	case reflect.Pointer, reflect.Map, reflect.Slice, reflect.Interface, reflect.Func, reflect.Chan:
		return !rv.IsNil()
	default: // structs, arrays and the rest
		return true
	}
}
