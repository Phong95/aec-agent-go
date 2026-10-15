// Package check holds the stdlib-only assertion helpers of the conformance
// suites. It exists so the public conformance package needs neither testify
// nor go-cmp: a consumer that imports conformance gets no extra dependency.
//
// Failures use t.Errorf (the test continues) except NoErr, which uses
// t.Fatalf because the code after a failed call usually cannot run.
package check

import (
	"fmt"
	"reflect"
	"testing"
)

// describe renders the optional message arguments of a helper.
func describe(msg []any) string {
	if len(msg) == 0 {
		return ""
	}
	if f, ok := msg[0].(string); ok {
		return ": " + fmt.Sprintf(f, msg[1:]...)
	}
	return ": " + fmt.Sprint(msg...)
}

// Equal reports an error when got != want.
func Equal[T comparable](t testing.TB, got, want T, msg ...any) {
	t.Helper()
	if got != want {
		t.Errorf("got %v, want %v%s", got, want, describe(msg))
	}
}

// NotEqual reports an error when got == other.
func NotEqual[T comparable](t testing.TB, got, other T, msg ...any) {
	t.Helper()
	if got == other {
		t.Errorf("got %v, want a different value%s", got, describe(msg))
	}
}

// True reports an error when v is false.
func True(t testing.TB, v bool, msg ...any) {
	t.Helper()
	if !v {
		t.Errorf("expected true%s", describe(msg))
	}
}

// False reports an error when v is true.
func False(t testing.TB, v bool, msg ...any) {
	t.Helper()
	if v {
		t.Errorf("expected false%s", describe(msg))
	}
}

// NoErr stops the test when err is not nil.
func NoErr(t testing.TB, err error, msg ...any) {
	t.Helper()
	if err != nil {
		t.Fatalf("unexpected error: %v%s", err, describe(msg))
	}
}

// Err reports an error when err is nil.
func Err(t testing.TB, err error, msg ...any) {
	t.Helper()
	if err == nil {
		t.Errorf("expected an error%s", describe(msg))
	}
}

// Len reports an error when s does not have n elements.
func Len[T any](t testing.TB, s []T, n int, msg ...any) {
	t.Helper()
	if len(s) != n {
		t.Errorf("got %d elements, want %d%s", len(s), n, describe(msg))
	}
}

// DeepEqual reports an error when got and want are not reflect.DeepEqual.
func DeepEqual(t testing.TB, got, want any, msg ...any) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %#v, want %#v%s", got, want, describe(msg))
	}
}
