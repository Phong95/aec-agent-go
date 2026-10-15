// Package wiretest holds the shared helpers of the golden fixture harness:
// loading golden fixtures, asserting byte-identical round trips and producing
// the deterministic clock and IDs the fixtures were recorded with.
//
// It is a non-test file that imports "testing" so that tests in any package
// can use it. It imports only the standard library and internal/jsonx, never
// kernel, so internal tests of package kernel can use it without an import
// cycle.
//
// Refreshing goldens. The two fixture trees are refreshed differently:
//
//   - tests/testdata/written holds records written by Go code (AssertGoWritten).
//     They are refreshed from Go: run the tests with the environment variable
//     AEC_WIRE_UPDATE=1, for example
//     AEC_WIRE_UPDATE=1 go test ./components/persistence/ -run TestGoWritten
//     from core/go, and review the diff.
//   - tests/testdata/golden holds frozen scenario corpora (inputs with the expected
//     outputs) owned by the Go SDK. Nothing in this package rewrites them: Load,
//     AssertRoundTrip and AssertBytes always compare, even when AEC_WIRE_UPDATE
//     is set. A deliberate behaviour change
//     edits the affected JSON by hand in the same commit.
//
// The switch is an environment variable rather than a -update flag because a
// flag registered here would be registered in every test binary that imports
// wiretest, and the root package already owns -update (TestAPISnapshot).
package wiretest

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/Phong95/aec-agent-go/internal/jsonx"
)

// moduleRoot walks up from the working directory to the directory that
// contains go.mod.
func moduleRoot(t testing.TB) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("wiretest: getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("wiretest: go.mod not found above the working directory")
		}
		dir = parent
	}
}

// Dir returns the fixture directory tests/testdata/golden of the module.
func Dir(t testing.TB) string {
	t.Helper()
	return filepath.Join(moduleRoot(t), "tests", "testdata", "golden")
}

// GoDir returns the directory tests/testdata/written of the module, where the records
// written by Go code are kept (the reverse wire check feeds them to other readers of
// the wire format).
func GoDir(t testing.TB) string {
	t.Helper()
	return filepath.Join(moduleRoot(t), "tests", "testdata", "written")
}

// Load reads tests/testdata/golden/<name> and returns its bytes without the single
// trailing LF. It fails if the file contains a carriage return (a checkout
// converted line endings; see .gitattributes) or does not end in exactly one LF.
func Load(t testing.TB, name string) []byte {
	t.Helper()
	path := filepath.Join(Dir(t), name)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("wiretest: read fixture %s: %v", name, err)
	}
	if bytes.IndexByte(data, '\r') >= 0 {
		t.Fatalf("wiretest: fixture %s contains a carriage return: line endings were converted; check .gitattributes marks core/go/tests/testdata/** as -text", name)
	}
	if !bytes.HasSuffix(data, []byte{'\n'}) || bytes.HasSuffix(data, []byte{'\n', '\n'}) {
		t.Fatalf("wiretest: fixture %s must end in exactly one LF", name)
	}
	return bytes.TrimSuffix(data, []byte{'\n'})
}

// AssertRoundTrip decodes the fixture strictly into v (which must be a
// pointer), re-encodes the pointed-to value with jsonx.Marshal and requires the
// bytes to equal the fixture.
func AssertRoundTrip(t testing.TB, name string, v any) {
	t.Helper()
	want := Load(t, name)
	if err := jsonx.UnmarshalStrict(want, v); err != nil {
		t.Fatalf("wiretest: strict decode of %s into %T: %v", name, v, err)
	}
	got, err := jsonx.Marshal(reflect.ValueOf(v).Elem().Interface())
	if err != nil {
		t.Fatalf("wiretest: encode %T: %v", v, err)
	}
	if bytes.Equal(got, want) {
		return
	}
	t.Fatalf("wiretest: %s is not byte-identical after a Go round trip\n got: %s\nwant: %s\n%s%s", name, got, want, diagnostic(got, want), goldenHint())
}

// AssertBytes requires got to equal the fixture bytes exactly. Use it for
// scenario tests that build the value through Go code instead of decoding it.
func AssertBytes(t testing.TB, name string, got []byte) {
	t.Helper()
	want := Load(t, name)
	if bytes.Equal(got, want) {
		return
	}
	t.Fatalf("wiretest: output differs from fixture %s\n got: %s\nwant: %s\n%s%s", name, got, want, diagnostic(got, want), goldenHint())
}

// AssertGoWritten compares got, bytes produced by REAL Go code (a store, a hub),
// with tests/testdata/written/<name>, which must hold exactly those bytes plus one LF,
// and fails with the AssertBytes diagnostic when they differ. When the
// environment variable AEC_WIRE_UPDATE is 1 it instead rewrites the file (got
// plus exactly one LF, creating the directory) and logs which file it rewrote.
// It is an environment variable, not a flag, so no flag leaks into importers; CI
// never sets it, so a stale Go-written file fails the build.
func AssertGoWritten(t testing.TB, name string, got []byte) {
	t.Helper()
	dir := GoDir(t)
	path := filepath.Join(dir, name)
	if os.Getenv("AEC_WIRE_UPDATE") == "1" {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			t.Fatalf("wiretest: create %s: %v", dir, err)
		}
		out := append(bytes.Clone(got), 0x0A)
		if err := os.WriteFile(path, out, 0o600); err != nil {
			t.Fatalf("wiretest: write %s: %v", name, err)
		}
		t.Logf("wiretest: rewrote %s", name)
		return
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("wiretest: read Go-written record %s: %v (rerun with AEC_WIRE_UPDATE=1 to create it)", name, err)
	}
	if bytes.IndexByte(data, 0x0D) >= 0 {
		t.Fatalf("wiretest: Go-written record %s contains a carriage return: line endings were converted; check .gitattributes marks core/go/tests/testdata/** as -text", name)
	}
	if !bytes.HasSuffix(data, []byte{0x0A}) || bytes.HasSuffix(data, []byte{0x0A, 0x0A}) {
		t.Fatalf("wiretest: Go-written record %s must end in exactly one LF", name)
	}
	want := bytes.TrimSuffix(data, []byte{0x0A})
	if bytes.Equal(got, want) {
		return
	}
	t.Fatalf("wiretest: Go output differs from %s\n got: %s\nwant: %s\n%s\nrerun with AEC_WIRE_UPDATE=1 to refresh the Go-written record", name, got, want, diagnostic(got, want))
}

// goldenHint explains, when AEC_WIRE_UPDATE is set, why a golden fixture was
// compared instead of rewritten: tests/testdata/golden is edited by hand only.
func goldenHint() string {
	if os.Getenv("AEC_WIRE_UPDATE") != "1" {
		return ""
	}
	return "\nAEC_WIRE_UPDATE does not rewrite tests/testdata/golden: those fixtures are frozen; edit the JSON by hand when the behaviour change is deliberate"
}

// diagnostic reports whether the two byte strings are at least semantically
// equal JSON (same data, different bytes) or genuinely different.
func diagnostic(got, want []byte) string {
	var g, w any
	if err := jsonx.UnmarshalStrict(got, &g); err != nil {
		return fmt.Sprintf("normalised diff: got is not JSON: %v", err)
	}
	if err := jsonx.UnmarshalStrict(want, &w); err != nil {
		return fmt.Sprintf("normalised diff: want is not JSON: %v", err)
	}
	if reflect.DeepEqual(g, w) {
		return "normalised diff: values are DeepEqual, only the bytes (key order, escaping or number spelling) differ"
	}
	return "normalised diff: values differ"
}

// Clock returns a clock producing start, start+step, start+2*step on
// successive calls. It matches the patched Date.now in tools/wiregen.
func Clock(start, step int64) func() int64 {
	next := start
	return func() int64 {
		v := next
		next += step
		return v
	}
}

// IDs returns an ID generator producing 00000000-0000-4000-8000-000000000001,
// ...0002 and so on. It matches the patched crypto.randomUUID in tools/wiregen.
func IDs() func() string {
	n := 0
	return func() string {
		n++
		return fmt.Sprintf("00000000-0000-4000-8000-%012d", n)
	}
}
