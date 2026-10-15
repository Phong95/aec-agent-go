package jsonx

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
)

// Special characters are built from numeric byte values and escape text from
// pieces, so this source never contains a literal separator, control
// character or JSON escape sequence.
var (
	lineSep  = string([]byte{0xe2, 0x80, 0xa8}) // U+2028 as raw UTF-8
	paraSep  = string([]byte{0xe2, 0x80, 0xa9}) // U+2029 as raw UTF-8
	bs       = string(rune(0x5c))               // backslash
	backsp   = string(rune(0x08))
	formfd   = string(rune(0x0c))
	ctl01    = string(rune(0x01))
	badUTF8  = string([]byte{0xff})
	replChar = string([]byte{0xef, 0xbf, 0xbd}) // U+FFFD as raw UTF-8
)

const uEsc = "u" // joined to a backslash to spell a JSON unicode escape

func TestMarshal(t *testing.T) {
	tests := []struct {
		name string
		in   any
		want string
	}{
		{"html stays raw", map[string]string{"s": `<a href="x">&</a>`}, `{"s":"<a href=\"x\">&</a>"}`},
		{"raw U+2028", map[string]string{"s": "a" + lineSep + "b"}, `{"s":"a` + lineSep + `b"}`},
		{"raw U+2029", map[string]string{"s": "a" + paraSep + "b"}, `{"s":"a` + paraSep + `b"}`},
		{"escaped backslash then u2028 text survives", map[string]string{"s": bs + uEsc + "2028"}, `{"s":"` + bs + bs + uEsc + `2028"}`},
		{"escaped backslash then u2029 text survives", map[string]string{"s": "x" + bs + uEsc + "2029"}, `{"s":"x` + bs + bs + uEsc + `2029"}`},
		{"backslash then real separator", map[string]string{"s": bs + lineSep}, `{"s":"` + bs + bs + lineSep + `"}`},
		{"backspace short escape", map[string]string{"s": backsp}, `{"s":"` + bs + `b"}`},
		{"form feed short escape", map[string]string{"s": formfd}, `{"s":"` + bs + `f"}`},
		{"control byte 0x01", map[string]string{"s": ctl01}, `{"s":"` + bs + uEsc + `0001"}`},
		{"invalid utf-8", map[string]string{"s": badUTF8}, `{"s":"` + replChar + `"}`},
		{"two invalid bytes", map[string]string{"s": string([]byte{0xe2, 0x82})}, `{"s":"` + replChar + replChar + `"}`},
		{"valid U+FFFD stays raw", map[string]string{"s": replChar}, `{"s":"` + replChar + `"}`},
		{"escaped backslash then ufffd text survives", map[string]string{"s": bs + uEsc + "fffd"}, `{"s":"` + bs + bs + uEsc + `fffd"}`},
		{"raw message escape lowercase", map[string]any{"r": json.RawMessage(`"` + bs + uEsc + `fffd"`)}, `{"r":"` + replChar + `"}`},
		{"raw message escape uppercase", map[string]any{"r": json.RawMessage(`"` + bs + uEsc + `FFFD"`)}, `{"r":"` + replChar + `"}`},
		{"emoji and diacritics raw", map[string]string{"s": "Ti" + string(rune(0x1ebf)) + "ng " + string(rune(0x1f600))}, `{"s":"Ti` + string(rune(0x1ebf)) + `ng ` + string(rune(0x1f600)) + `"}`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Marshal(tc.in)
			if err != nil {
				t.Fatalf("Marshal: %v", err)
			}
			if string(got) != tc.want {
				t.Fatalf("got  %q\nwant %q", got, tc.want)
			}
			if strings.HasSuffix(string(got), "\n") {
				t.Fatalf("output must not end in a newline: %q", got)
			}
		})
	}
}

// NormalizeLineSeparators rewrites the escapes of U+2028, U+2029 and U+FFFD, is
// escape-aware, and hands back its argument itself when there is nothing to do.
func TestNormalizeLineSeparators(t *testing.T) {
	t.Run("no escape returns the same slice", func(t *testing.T) {
		in := []byte(`{"s":"` + bs + uEsc + `00e9 ` + bs + `n"}`)
		got := NormalizeLineSeparators(in)
		if &got[0] != &in[0] || len(got) != len(in) {
			t.Fatalf("expected the argument itself, got a copy: %q", got)
		}
	})
	for _, tc := range []struct {
		name, in, want string
	}{
		{"U+2028", bs + uEsc + "2028", lineSep},
		{"U+2029", bs + uEsc + "2029", paraSep},
		{"U+FFFD lowercase", bs + uEsc + "fffd", replChar},
		{"U+FFFD uppercase", bs + uEsc + "FFFD", replChar},
		{"U+FFFD mixed case", bs + uEsc + "fFfD", replChar},
		{"escaped backslash before u2028 kept", bs + bs + uEsc + "2028", bs + bs + uEsc + "2028"},
		{"escaped backslash before ufffd kept", bs + bs + uEsc + "fffd", bs + bs + uEsc + "fffd"},
		{"other escape kept", bs + uEsc + "fffe", bs + uEsc + "fffe"},
		{"truncated escape kept", bs + uEsc + "fff", bs + uEsc + "fff"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := []byte(`"a` + tc.in + `b"`)
			got := NormalizeLineSeparators(in)
			if want := `"a` + tc.want + `b"`; string(got) != want {
				t.Fatalf("got  %q\nwant %q", got, want)
			}
		})
	}
	t.Run("input is never modified", func(t *testing.T) {
		in := []byte(`"` + bs + uEsc + `fffd"`)
		orig := string(in)
		_ = NormalizeLineSeparators(in)
		if string(in) != orig {
			t.Fatalf("input modified: %q", in)
		}
	})
}

func TestMarshalNeverEndsInNewline(t *testing.T) {
	for _, v := range []any{1, "x", nil, []int{}, map[string]any{}} {
		got, err := Marshal(v)
		if err != nil {
			t.Fatalf("Marshal(%v): %v", v, err)
		}
		if len(got) > 0 && got[len(got)-1] == '\n' {
			t.Fatalf("Marshal(%v) ends in newline: %q", v, got)
		}
	}
}

func TestMarshalError(t *testing.T) {
	if _, err := Marshal(make(chan int)); err == nil {
		t.Fatal("expected an error for an unsupported type")
	}
}

func TestUnmarshalStrict(t *testing.T) {
	type target struct {
		A int `json:"a"`
	}
	t.Run("ok", func(t *testing.T) {
		var v target
		if err := UnmarshalStrict([]byte(`{"a":1}`), &v); err != nil || v.A != 1 {
			t.Fatalf("err=%v v=%+v", err, v)
		}
	})
	t.Run("trailing whitespace ok", func(t *testing.T) {
		var v target
		if err := UnmarshalStrict([]byte("{\"a\":1} \n"), &v); err != nil {
			t.Fatalf("err=%v", err)
		}
	})
	t.Run("unknown field rejected", func(t *testing.T) {
		var v target
		if err := UnmarshalStrict([]byte(`{"a":1,"b":2}`), &v); err == nil {
			t.Fatal("expected unknown-field error")
		}
	})
	t.Run("trailing value rejected", func(t *testing.T) {
		var v target
		if err := UnmarshalStrict([]byte(`{"a":1}{"a":2}`), &v); err == nil {
			t.Fatal("expected trailing-data error")
		}
	})
	t.Run("trailing garbage rejected", func(t *testing.T) {
		var v target
		if err := UnmarshalStrict([]byte(`{"a":1} x`), &v); err == nil {
			t.Fatal("expected trailing-data error")
		}
	})
	t.Run("malformed returns error not panic", func(t *testing.T) {
		var v target
		if err := UnmarshalStrict([]byte(`{"a":`), &v); err == nil {
			t.Fatal("expected error")
		}
	})
}

// WR-02: JSON.stringify writes NaN and +/-Infinity as null and -0 as 0.
func TestMarshalFloatsLikeJS(t *testing.T) {
	nan, inf, negZero := math.NaN(), math.Inf(1), math.Copysign(0, -1)
	type attrs struct {
		A map[string]any `json:"a"`
		B any            `json:"b"`
		C float64        `json:"c"`
		D *float64       `json:"d"`
		E []any          `json:"e"`
		F []float64      `json:"f"`
	}
	nz := negZero
	tests := []struct {
		name string
		in   any
		want string
	}{
		{"bare NaN", nan, `null`},
		{"bare +Inf", inf, `null`},
		{"bare -Inf", math.Inf(-1), `null`},
		{"bare -0", negZero, `0`},
		{"plain float untouched", 1.5, `1.5`},
		{"map values", map[string]any{"nan": nan, "inf": inf, "ninf": math.Inf(-1), "nz": negZero, "ok": 2.5}, `{"inf":null,"nan":null,"ninf":null,"nz":0,"ok":2.5}`},
		{"float32", map[string]any{"x": float32(math.NaN()), "y": float32(1.25)}, `{"x":null,"y":1.25}`},
		{"nested map and slice", map[string]any{"l": []any{nan, 1.0, map[string]any{"z": negZero}}}, `{"l":[null,1,{"z":0}]}`},
		{"typed -0 in struct", attrs{C: negZero, A: map[string]any{"k": nan}, B: inf, D: &nz, E: []any{negZero, nan}, F: []float64{negZero, 3}}, `{"a":{"k":null},"b":null,"c":0,"d":0,"e":[0,null],"f":[0,3]}`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Marshal(tc.in)
			if err != nil {
				t.Fatalf("Marshal: %v", err)
			}
			if string(got) != tc.want {
				t.Fatalf("got  %s\nwant %s", got, tc.want)
			}
		})
	}
}

// The caller's value must not be mutated by the normalisation.
func TestMarshalFloatsDoesNotMutateInput(t *testing.T) {
	in := map[string]any{"nan": math.NaN(), "nested": map[string]any{"nz": math.Copysign(0, -1)}, "list": []any{math.Inf(1)}}
	if _, err := Marshal(in); err != nil {
		t.Fatal(err)
	}
	if v, ok := in["nan"].(float64); !ok || !math.IsNaN(v) {
		t.Fatalf("input map mutated: %v", in["nan"])
	}
	if v := in["nested"].(map[string]any)["nz"].(float64); !math.Signbit(v) {
		t.Fatal("nested map mutated")
	}
	if v := in["list"].([]any)[0].(float64); !math.IsInf(v, 1) {
		t.Fatal("slice mutated")
	}
}

// A typed NaN cannot become null without changing the declared type, so it is
// still reported as an error rather than silently written as something else.
func TestMarshalTypedNaNStillErrors(t *testing.T) {
	type s struct {
		F float64 `json:"f"`
	}
	if _, err := Marshal(s{F: math.NaN()}); err == nil {
		t.Fatal("expected an error for a typed NaN field")
	}
}

// Types that own their encoding are not entered, and cyclic values do not hang
// the walk (encoding/json reports the cycle).
func TestMarshalFloatsSkipsMarshalersAndCycles(t *testing.T) {
	got, err := Marshal(map[string]any{"raw": json.RawMessage(`-0`), "ok": 1})
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != `{"ok":1,"raw":-0}` {
		t.Fatalf("RawMessage must pass through untouched, got %s", got)
	}

	cyc := map[string]any{}
	cyc["self"] = cyc
	if _, err := Marshal(cyc); err == nil {
		t.Fatal("expected an error for a cyclic value")
	}
}
