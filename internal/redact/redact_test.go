package redact

import (
	"errors"
	"runtime"
	"strings"
	"testing"
)

// TestURL is the table of WR-201 and IN-01: only scheme and host survive, and every
// value net/url cannot split into scheme and host (opaque, scheme-less,
// unparseable) is replaced whole. TOKEN stands for a credential in any position.
func TestURL(t *testing.T) {
	for _, tc := range []struct{ name, raw, want string }{
		{"host only", "https://example.test", "https://example.test"},
		{"root path", "https://example.test/", "https://example.test/"},
		{"path", "https://example.test/v1/TOKEN/", "https://example.test/REDACTED"},
		{"host and port", "http://localhost:8080/v1/TOKEN", "http://localhost:8080/REDACTED"},
		{"userinfo", "https://bob:TOKEN@example.test", "https://REDACTED@example.test"},
		{"user only", "https://TOKEN@example.test", "https://REDACTED@example.test"},
		{"query", "https://example.test?key=TOKEN", "https://example.test?REDACTED"},
		{"empty query", "https://example.test/?", "https://example.test/"},
		{"fragment", "https://example.test#TOKEN", "https://example.test"},
		{"everything", "https://bob:TOKEN@example.test/v1/TOKEN?k=TOKEN#TOKEN", "https://REDACTED@example.test/REDACTED?REDACTED"},

		// The WR-201 cases: scheme-less host:port/path parses as scheme = host and
		// Opaque = port/path, and Path stays empty.
		{"scheme-less host and port", "localhost:8080/v1/TOKEN", Marker},
		{"scheme-less host, port, short path", "gw.example:8443/TOKEN", Marker},
		{"scheme-less host and bare port", "localhost:8080", Marker},
		{"opaque", "https:TOKEN", Marker},
		{"mailto", "mailto:x", Marker},
		{"opaque with userinfo", "user:TOKEN@host/v1", Marker},
		{"host and path", "host/path/TOKEN", Marker},
		{"network-path reference", "//host/TOKEN", Marker},
		{"absolute path", "/v1/TOKEN", Marker},
		{"scheme-less ip and port", "127.0.0.1:8080/TOKEN", Marker},
		{"unparseable host", "http://[::1/TOKEN", Marker},
		{"bad escape", "https://ok.test/%zz/TOKEN", Marker},
		{"empty", "", Marker},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := URL(tc.raw)
			if got != tc.want {
				t.Errorf("URL(%q) = %q, want %q", tc.raw, got, tc.want)
			}
			if strings.Contains(got, "TOKEN") {
				t.Errorf("URL(%q) = %q shows a credential", tc.raw, got)
			}
		})
	}
}

// runtimeErr is a user type that implements runtime.Error. Its message is user
// text, so Panic must keep only its type (10-SECURITY UF-1).
type runtimeErr struct{ msg string }

func (e runtimeErr) Error() string { return e.msg }
func (runtimeErr) RuntimeError()   {}

// recovered runs f and returns the value it panicked with, so the runtime cases
// below are real runtime panics and not hand-built values.
func recovered(f func()) (v any) {
	defer func() { v = recover() }()
	f()
	return nil
}

// TestPanic pins the rule of Panic: a recovered panic value can carry user, model
// or memory text, so only its type is shown; the message appears only for a
// runtime.Error whose type is declared in package runtime itself.
func TestPanic(t *testing.T) {
	idx := 5
	var nilMap map[string]int
	var iface any = "text"
	for _, tc := range []struct {
		name     string
		v        any
		want     string // exact result, or a prefix when prefix is true
		pre      bool
		leak     string // a substring that must not appear
		contains string // a substring that must appear in every Go release
	}{
		{name: "string", v: "secret-text", want: "string", leak: "secret-text"},
		{name: "error", v: errors.New("secret-error"), want: "*errors.errorString", leak: "secret-error"},
		{name: "user runtime error", v: runtimeErr{"secret-runtime"}, want: "redact.runtimeErr", leak: "secret-runtime"},
		{name: "pointer user runtime error", v: &runtimeErr{"secret-runtime"}, want: "*redact.runtimeErr", leak: "secret-runtime"},
		{name: "nil", v: nil, want: "<nil>"},
		{name: "index out of range", v: recovered(func() { _ = []int{1}[idx] }), want: "runtime.boundsError: runtime error: index out of range", pre: true},
		{name: "nil map write", v: recovered(func() { nilMap["k"] = 1 }), want: "runtime.plainError: assignment to entry in nil map", pre: true},
		{name: "type assertion", v: recovered(func() { _ = iface.(int) }), want: "*runtime.TypeAssertionError: interface conversion", pre: true},
		// The text after ": " is the running Go's own message: Go 1.27 writes it with a
		// "runtime error: " prefix that Go 1.25 does not. The rule (type, ": ", the
		// runtime's message) is what is pinned, not one release's wording.
		{name: "panic nil", v: recovered(func() { panic(nil) }), want: "*runtime.PanicNilError: " + new(runtime.PanicNilError).Error(), contains: "panic called with nil argument"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := Panic(tc.v)
			if tc.pre {
				if !strings.HasPrefix(got, tc.want) {
					t.Errorf("Panic = %q, want prefix %q", got, tc.want)
				}
			} else if got != tc.want {
				t.Errorf("Panic = %q, want %q", got, tc.want)
			}
			if tc.leak != "" && strings.Contains(got, tc.leak) {
				t.Errorf("Panic = %q shows %q", got, tc.leak)
			}
		})
	}
}
