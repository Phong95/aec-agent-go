package toolkit_test

import (
	"math"
	"slices"
	"testing"

	"github.com/Phong95/aec-agent-go/internal/toolkit"
	"github.com/Phong95/aec-agent-go/kernel"
)

// decode builds Args from raw JSON text the way a provider hands it to a tool: through
// kernel.NewArguments, or as plain bytes when the text is not an object (a tool also
// runs with bytes no provider checked).
func decode(t *testing.T, raw string) toolkit.Args {
	t.Helper()
	return toolkit.DecodeArgs(kernel.Arguments(raw))
}

// TestArgs_HugeNumberKeepsTheOtherKeys is the regression of RESEARCH Pitfall 7: one
// out-of-range number (JS reads 1e999 as Infinity) must not discard the other arguments.
func TestArgs_HugeNumberKeepsTheOtherKeys(t *testing.T) {
	a := decode(t, `{"name":"x","n":1e999,"flag":true,"list":["a","b"]}`)
	if s, ok := a.String("name"); !ok || s != "x" {
		t.Errorf("String(name) = %q, %v, want x, true", s, ok)
	}
	if f, ok := a.Number("n"); ok {
		t.Errorf("Number(n) = %v, true, want absent for 1e999 (Number.isFinite is false)", f)
	}
	if b, ok := a.Bool("flag"); !ok || !b {
		t.Errorf("Bool(flag) = %v, %v, want true, true", b, ok)
	}
	if got := a.Strings("list"); !slices.Equal(got, []string{"a", "b"}) {
		t.Errorf("Strings(list) = %q, want [a b]", got)
	}
	if !a.Has("n") {
		t.Error("Has(n) = false, want true: the key is present, only its value is not finite")
	}
}

func TestArgs_String(t *testing.T) {
	a := decode(t, `{"s":"  padded  ","e":"","n":5,"null":null,"b":true,"arr":["x"],"obj":{"a":1},"esc":"a\"b\\c\n\u00e9\ud83d\ude00"}`)
	for key, want := range map[string]string{"s": "  padded  ", "e": "", "esc": "a\"b\\c\n\u00e9\U0001F600"} {
		if got, ok := a.String(key); !ok || got != want {
			t.Errorf("String(%q) = %q, %v, want %q, true", key, got, ok, want)
		}
	}
	for _, key := range []string{"n", "null", "b", "arr", "obj", "missing"} {
		if got, ok := a.String(key); ok {
			t.Errorf("String(%q) = %q, true, want absent: not a JSON string", key, got)
		}
	}
}

func TestArgs_Number(t *testing.T) {
	a := decode(t, `{"i":3,"neg":-2.5,"zero":0,"negzero":-0,"exp":1.5e3,"big":1e308,"huge":1e999,"hugeneg":-1e999,"str":"5","strnum":"1e3","null":null,"b":true,"arr":[1]}`)
	for key, want := range map[string]float64{"i": 3, "neg": -2.5, "zero": 0, "exp": 1500, "big": 1e308} {
		if got, ok := a.Number(key); !ok || got != want {
			t.Errorf("Number(%q) = %v, %v, want %v, true", key, got, ok, want)
		}
	}
	if got, ok := a.Number("negzero"); !ok || got != 0 || !math.Signbit(got) {
		t.Errorf("Number(negzero) = %v, %v, want -0, true", got, ok)
	}
	for _, key := range []string{"huge", "hugeneg", "str", "strnum", "null", "b", "arr", "missing"} {
		if got, ok := a.Number(key); ok {
			t.Errorf("Number(%q) = %v, true, want absent: not a finite JSON number", key, got)
		}
	}
}

func TestArgs_Bool(t *testing.T) {
	a := decode(t, `{"t":true,"f":false,"s":"true","n":1,"null":null}`)
	if b, ok := a.Bool("t"); !ok || !b {
		t.Errorf("Bool(t) = %v, %v", b, ok)
	}
	if b, ok := a.Bool("f"); !ok || b {
		t.Errorf("Bool(f) = %v, %v", b, ok)
	}
	for _, key := range []string{"s", "n", "null", "missing"} {
		if b, ok := a.Bool(key); ok {
			t.Errorf("Bool(%q) = %v, true, want absent", key, b)
		}
	}
}

func TestArgs_Strings(t *testing.T) {
	a := decode(t, `{"mixed":["a",1,null,"b",["c"],{"d":1},true,"e"],"empty":[],"none":[1,2],"str":"a","obj":{"0":"a"},"null":null,"unicode":["\u00e9","\ud83d\ude00"]}`)
	if got := a.Strings("mixed"); !slices.Equal(got, []string{"a", "b", "e"}) {
		t.Errorf("Strings(mixed) = %q, want [a b e]: non-strings are dropped, order is kept", got)
	}
	if got := a.Strings("empty"); got == nil || len(got) != 0 {
		t.Errorf("Strings(empty) = %#v, want a non-nil empty slice", got)
	}
	if got := a.Strings("none"); got == nil || len(got) != 0 {
		t.Errorf("Strings(none) = %#v, want a non-nil empty slice", got)
	}
	for _, key := range []string{"str", "obj", "null", "missing"} {
		if got := a.Strings(key); got != nil {
			t.Errorf("Strings(%q) = %#v, want nil: not an array", key, got)
		}
	}
	if got := a.Strings("unicode"); !slices.Equal(got, []string{"\u00e9", "\U0001F600"}) {
		t.Errorf("Strings(unicode) = %q", got)
	}
}

func TestArgs_Has(t *testing.T) {
	a := decode(t, `{"a":1,"n":null}`)
	for key, want := range map[string]bool{"a": true, "n": true, "missing": false} {
		if got := a.Has(key); got != want {
			t.Errorf("Has(%q) = %v, want %v", key, got, want)
		}
	}
}

// TestArgs_InvalidInputIsEmpty: invalid JSON and a value that is not an object give an
// empty Args, which reports every key as absent, and the zero Args works the same.
func TestArgs_InvalidInputIsEmpty(t *testing.T) {
	for name, raw := range map[string]string{
		"invalid":    `{"a":`,
		"array":      `["a"]`,
		"string":     `"a"`,
		"number":     `5`,
		"null":       `null`,
		"garbage":    `not json`,
		"empty":      ``,
		"empty obj":  `{}`,
		"whitespace": `   `,
	} {
		t.Run(name, func(t *testing.T) {
			a := decode(t, raw)
			if a.Has("a") {
				t.Error("Has(a) = true on an empty Args")
			}
			if _, ok := a.String("a"); ok {
				t.Error("String reports a value on an empty Args")
			}
			if _, ok := a.Number("a"); ok {
				t.Error("Number reports a value on an empty Args")
			}
			if _, ok := a.Bool("a"); ok {
				t.Error("Bool reports a value on an empty Args")
			}
			if a.Strings("a") != nil {
				t.Error("Strings reports a value on an empty Args")
			}
		})
	}
	var zero toolkit.Args
	if zero.Has("a") {
		t.Error("the zero Args has a key")
	}
	if _, ok := zero.String("a"); ok {
		t.Error("the zero Args has a string")
	}
}

// TestArgs_DuplicateKeyLastWins: JSON.parse keeps the last of two equal keys.
func TestArgs_DuplicateKeyLastWins(t *testing.T) {
	a := decode(t, `{"k":"first","k":"second"}`)
	if s, _ := a.String("k"); s != "second" {
		t.Errorf("String(k) = %q, want second", s)
	}
}

// TestArgs_DoesNotModifyTheArguments: decoding leaves the argument bytes untouched.
func TestArgs_DoesNotModifyTheArguments(t *testing.T) {
	raw := kernel.Arguments(`{"name":"x","n":1e999,"list":["a"]}`)
	before := string(raw)
	a := toolkit.DecodeArgs(raw)
	a.String("name")
	a.Number("n")
	a.Strings("list")
	if string(raw) != before {
		t.Errorf("arguments changed from %s to %s", before, raw)
	}
}

func TestArgs_Raw(t *testing.T) {
	a := decode(t, `{"files":[{"path":"a","content":"b"}],"null":null,"n":1e999}`)
	if raw, ok := a.Raw("files"); !ok || string(raw) != `[{"path":"a","content":"b"}]` {
		t.Errorf("Raw(files) = %s, %v", raw, ok)
	}
	if raw, ok := a.Raw("null"); !ok || string(raw) != "null" {
		t.Errorf("Raw(null) = %s, %v, want null, true: the key is present", raw, ok)
	}
	if raw, ok := a.Raw("n"); !ok || string(raw) != "1e999" {
		t.Errorf("Raw(n) = %s, %v, want the number as written", raw, ok)
	}
	if raw, ok := a.Raw("missing"); ok || raw != nil {
		t.Errorf("Raw(missing) = %s, %v, want absent", raw, ok)
	}
	// The returned slice is a copy: changing it does not change a later read.
	raw, _ := a.Raw("files")
	raw[0] = 'X'
	if again, _ := a.Raw("files"); again[0] != '[' {
		t.Errorf("Raw returned a shared slice: %s", again)
	}
}
