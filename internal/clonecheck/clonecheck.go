// Package clonecheck proves that a Clone method is deep. It is the drift guard
// for RESEARCH Pitfall 12: an explicit Clone is fast and exact, but a pointer,
// slice or map field added to a record later is silently shared between a caller
// and a store until someone remembers to extend Clone. AssertDeep fails the test
// instead.
//
// It is a non-test file that imports "testing" so that tests in any package can
// use it. It imports only the standard library, ordered and internal/jsonx.
package clonecheck

import (
	"bytes"
	"reflect"
	"testing"

	"github.com/Phong95/aec-agent-go/internal/jsonx"
	"github.com/Phong95/aec-agent-go/kernel/ordered"
)

// maxDepth bounds the reflection walks, so a cyclic sample cannot loop.
const maxDepth = 32

// marker is the key AssertDeep adds to every map-like value it mutates.
const marker = "__clonecheck"

var objectType = reflect.TypeFor[ordered.Object]()

// AssertDeep reports a test failure when clone(sample) is not a deep copy of
// sample. It works in four steps:
//
//  1. It walks sample and fails, naming the field path, when an exported field
//     reachable from it holds the zero value (an empty slice, map or Object
//     counts too), so a field added to the type forces the sample to be updated
//     and a forgotten Clone line is caught.
//  2. It takes c := clone(sample) and snapshots jsonx.Marshal(c).
//  3. It mutates everything reachable from sample: pointees, slice and byte-slice
//     elements, map entries (a new key and every existing value), the entries of
//     every ordered.Object (a new key and every existing value, nested values
//     included) and of any value whose pointer type has a Set(string, V) method
//     (which covers ordered.Map[V]).
//  4. It fails when jsonx.Marshal(c) differs from the snapshot.
//
// It reports with t.Errorf and returns, never Fatalf, so a recording fake can
// observe the failure. A clone that shares nothing with the sample passes.
func AssertDeep[T any](t testing.TB, sample T, clone func(T) T) {
	t.Helper()
	root := reflect.ValueOf(&sample).Elem()
	var problems []string
	checkNonZero(root, reflect.TypeFor[T]().String(), 0, &problems)
	if len(problems) > 0 {
		for _, p := range problems {
			t.Errorf("clonecheck: %s", p)
		}
		return
	}
	c := clone(sample)
	before, err := jsonx.Marshal(c)
	if err != nil {
		t.Errorf("clonecheck: encode the clone: %v", err)
		return
	}
	mutate(root, 0)
	after, err := jsonx.Marshal(c)
	if err != nil {
		t.Errorf("clonecheck: encode the clone after mutating the sample: %v", err)
		return
	}
	if !bytes.Equal(before, after) {
		t.Errorf("clonecheck: the clone shares memory with the original (mutating the sample changed it)\n before: %s\n after:  %s", before, after)
	}
}

// lenOf returns the Len of an ordered container (Object or Map), reporting
// whether v is one: a struct with no exported fields and a Len method.
func lenOf(v reflect.Value) (int, bool) {
	if v.Kind() != reflect.Struct {
		return 0, false
	}
	m := v.MethodByName("Len")
	if !m.IsValid() && v.CanAddr() {
		m = v.Addr().MethodByName("Len")
	}
	if !m.IsValid() || m.Type().NumIn() != 0 || m.Type().NumOut() != 1 || m.Type().Out(0).Kind() != reflect.Int {
		return 0, false
	}
	return int(m.Call(nil)[0].Int()), true
}

// checkNonZero is step 1: every exported field reachable from v must be non-zero.
func checkNonZero(v reflect.Value, path string, depth int, problems *[]string) {
	if depth > maxDepth || !v.IsValid() {
		return
	}
	switch v.Kind() {
	case reflect.Pointer:
		if v.IsNil() {
			*problems = append(*problems, path+" is a nil pointer: fill it in the sample")
			return
		}
		checkNonZero(v.Elem(), path, depth+1, problems)
	case reflect.Interface:
		if v.IsNil() {
			*problems = append(*problems, path+" is a nil interface: fill it in the sample")
			return
		}
		checkNonZero(v.Elem(), path, depth+1, problems)
	case reflect.Struct:
		if n, ok := lenOf(v); ok {
			if n == 0 {
				*problems = append(*problems, path+" is an empty ordered container: fill it in the sample")
			}
			return
		}
		for i := range v.NumField() {
			f := v.Type().Field(i)
			if !f.IsExported() {
				continue
			}
			fv := v.Field(i)
			p := path + "." + f.Name
			if fv.IsZero() {
				*problems = append(*problems, p+" is the zero value: fill it in the sample so a forgotten Clone line is caught")
				continue
			}
			checkNonZero(fv, p, depth+1, problems)
		}
	case reflect.Slice:
		if v.Len() == 0 {
			*problems = append(*problems, path+" is an empty slice: fill it in the sample")
			return
		}
		if v.Type().Elem().Kind() == reflect.Uint8 {
			return
		}
		for i := range v.Len() {
			checkNonZero(v.Index(i), path+"[]", depth+1, problems)
		}
	case reflect.Map:
		if v.Len() == 0 {
			*problems = append(*problems, path+" is an empty map: fill it in the sample")
			return
		}
		iter := v.MapRange()
		for iter.Next() {
			checkNonZero(iter.Value(), path+"[]", depth+1, problems)
		}
	default:
		// Scalars are checked as fields of their struct.
	}
}

// mutate is step 3: it changes everything reachable from v in place. v must be
// addressable for its direct scalar fields to change; memory behind pointers,
// slices, maps and ordered containers is changed whatever the receiver.
func mutate(v reflect.Value, depth int) {
	if depth > maxDepth || !v.IsValid() {
		return
	}
	switch v.Kind() {
	case reflect.Pointer:
		if !v.IsNil() {
			mutate(v.Elem(), depth+1)
		}
	case reflect.Interface:
		if v.IsNil() {
			return
		}
		e := reflect.New(v.Elem().Type()).Elem()
		e.Set(v.Elem())
		mutate(e, depth+1)
		if v.CanSet() {
			v.Set(e)
		}
	case reflect.Struct:
		mutateStruct(v, depth)
	case reflect.Slice, reflect.Array:
		for i := range v.Len() {
			mutate(v.Index(i), depth+1)
		}
	case reflect.Map:
		mutateMap(v, depth)
	default:
		mutateScalar(v)
	}
}

func mutateStruct(v reflect.Value, depth int) {
	if v.Type() == objectType {
		if !v.CanAddr() {
			return
		}
		o, ok := v.Addr().Interface().(*ordered.Object)
		if !ok {
			return
		}
		// Existing keys first: the clone encodes its own key list, so only a
		// shared value map shows a change to an existing key.
		for _, k := range o.Keys() {
			val, _ := o.Get(k)
			o.Set(k, mutatedCopy(val, depth+1))
		}
		o.Set(marker, true)
		return
	}
	if v.CanAddr() {
		if set, ok := v.Addr().Type().MethodByName("Set"); ok && set.Type.NumIn() == 3 && set.Type.In(1).Kind() == reflect.String {
			ptr := v.Addr()
			if keys := ptr.MethodByName("Keys"); keys.IsValid() && keys.Type().NumIn() == 0 && keys.Type().NumOut() == 1 {
				if names, ok := keys.Call(nil)[0].Interface().([]string); ok {
					for _, k := range names {
						ptr.MethodByName("Set").Call([]reflect.Value{reflect.ValueOf(k), fillNonZero(set.Type.In(2))})
					}
				}
			}
			ptr.MethodByName("Set").Call([]reflect.Value{reflect.ValueOf(marker), fillNonZero(set.Type.In(2))})
			return
		}
	}
	for i := range v.NumField() {
		if !v.Type().Field(i).IsExported() {
			continue
		}
		mutate(v.Field(i), depth+1)
	}
}

func mutateMap(v reflect.Value, depth int) {
	if v.IsNil() {
		return
	}
	for _, k := range v.MapKeys() {
		tmp := reflect.New(v.Type().Elem()).Elem()
		tmp.Set(v.MapIndex(k))
		mutate(tmp, depth+1)
		v.SetMapIndex(k, tmp)
	}
	if v.Type().Key().Kind() == reflect.String {
		v.SetMapIndex(reflect.ValueOf(marker).Convert(v.Type().Key()), fillNonZero(v.Type().Elem()))
	}
}

// mutatedCopy returns val changed: containers are mutated in place (so shared
// memory changes) and scalars come back with a different value.
func mutatedCopy(val any, depth int) any {
	if val == nil {
		return true
	}
	rv := reflect.New(reflect.TypeOf(val)).Elem()
	rv.Set(reflect.ValueOf(val))
	mutate(rv, depth)
	return rv.Interface()
}

func mutateScalar(v reflect.Value) {
	if !v.CanSet() {
		return
	}
	switch v.Kind() {
	case reflect.Bool:
		v.SetBool(!v.Bool())
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		v.SetInt(v.Int() + 1)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		v.SetUint(v.Uint() + 1)
	case reflect.Float32, reflect.Float64:
		v.SetFloat(v.Float() + 1)
	case reflect.String:
		v.SetString(v.String() + "!")
	default:
		// Other kinds (chan, func, unsafe pointer) carry no data to change.
	}
}

// fillNonZero builds a non-zero value of typ.
func fillNonZero(typ reflect.Type) reflect.Value {
	v := reflect.New(typ).Elem()
	switch typ.Kind() {
	case reflect.Bool:
		v.SetBool(true)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		v.SetInt(1)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		v.SetUint(1)
	case reflect.Float32, reflect.Float64:
		v.SetFloat(1)
	case reflect.String:
		v.SetString("x")
	case reflect.Pointer:
		v.Set(reflect.New(typ.Elem()))
		v.Elem().Set(fillNonZero(typ.Elem()))
	case reflect.Struct:
		for i := range v.NumField() {
			if typ.Field(i).IsExported() {
				v.Field(i).Set(fillNonZero(typ.Field(i).Type))
			}
		}
	case reflect.Slice:
		v.Set(reflect.MakeSlice(typ, 1, 1))
		v.Index(0).Set(fillNonZero(typ.Elem()))
	case reflect.Map:
		v.Set(reflect.MakeMap(typ))
		v.SetMapIndex(fillNonZero(typ.Key()), fillNonZero(typ.Elem()))
	case reflect.Interface:
		if typ.NumMethod() == 0 {
			v.Set(reflect.ValueOf("x"))
		}
	default:
		// Other kinds stay zero.
	}
	return v
}
