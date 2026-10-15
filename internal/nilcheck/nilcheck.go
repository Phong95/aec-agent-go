// Package nilcheck holds the single typed-nil check of the SDK. It decides what
// counts as an absent value: a nil interface and an interface holding a nil
// pointer are treated alike, since the latter is the Go spelling of "absent but
// typed". Kernel (lifecycle hooks), persistence (hub
// defaults) and the hubs that take a store getter share this one check instead of
// three copies.
//
// It imports only the standard library.
package nilcheck

import "reflect"

// IsNil reports whether v is a nil interface or an interface holding a nil
// pointer, map, slice, func or channel (the typed-nil case).
func IsNil(v any) bool {
	if v == nil {
		return true
	}
	switch rv := reflect.ValueOf(v); rv.Kind() {
	case reflect.Pointer, reflect.Map, reflect.Slice, reflect.Func, reflect.Chan, reflect.Interface:
		return rv.IsNil()
	default:
		return false
	}
}
