package kernel

import (
	"bytes"
	"strings"
	"testing"
)

func TestNewArguments(t *testing.T) {
	sep := string([]byte{0xE2, 0x80, 0xA8})  // U+2028 as raw UTF-8
	repl := string([]byte{0xEF, 0xBF, 0xBD}) // U+FFFD as raw UTF-8

	tests := []struct {
		name    string
		in      string
		want    string // encoded result
		wantErr string // substring of the error; empty means success
	}{
		{name: "empty", in: "", want: "{}"},
		{name: "whitespace only", in: " \t\r\n ", want: "{}"},
		{name: "null", in: "null", want: "{}"},
		{name: "null with whitespace", in: "  null\n", want: "{}"},
		{name: "empty object", in: "{}", want: "{}"},
		{name: "compacts whitespace", in: "{ \"b\" : 1 ,\n \"a\" : [ 1 , 2 ] }", want: `{"b":1,"a":[1,2]}`},
		{name: "keeps html characters raw", in: `{"x":"<a>&"}`, want: `{"x":"<a>&"}`},
		{name: "keeps U+2028 raw", in: `{"x":"` + sep + `"}`, want: `{"x":"` + sep + `"}`},
		{name: "normalises an escaped U+2028 to raw", in: `{"x":"\u2028"}`, want: `{"x":"` + sep + `"}`},
		{name: "normalises an escaped U+2029 to raw", in: `{"x":"\u2029"}`, want: `{"x":"` + string([]byte{0xE2, 0x80, 0xA9}) + `"}`},
		{name: "keeps an escaped backslash before u2028", in: `{"x":"\\u2028"}`, want: `{"x":"\\u2028"}`},
		{name: "normalises an escaped U+FFFD to raw", in: `{"x":"\ufffd"}`, want: `{"x":"` + repl + `"}`},
		{name: "normalises an uppercase escaped U+FFFD to raw", in: `{"x":"\uFFFD"}`, want: `{"x":"` + repl + `"}`},
		{name: "keeps an escaped backslash before ufffd", in: `{"x":"\\ufffd"}`, want: `{"x":"\\ufffd"}`},
		{name: "keeps number spellings", in: `{"a":1.50,"b":1e21,"c":-0,"d":0}`, want: `{"a":1.50,"b":1e21,"c":-0,"d":0}`},
		{name: "keeps zero values", in: `{"n":0,"f":false,"s":""}`, want: `{"n":0,"f":false,"s":""}`},
		{name: "keeps whitespace inside strings", in: `{"x":" a  b "}`, want: `{"x":" a  b "}`},
		{name: "invalid json", in: `{"a":`, wantErr: "not valid JSON"},
		{name: "trailing data", in: `{} {}`, wantErr: "not valid JSON"},
		{name: "trailing garbage", in: `{}x`, wantErr: "not valid JSON"},
		{name: "unicode space is not json whitespace", in: "\xc2\xa0{}", wantErr: "not valid JSON"},
		{name: "array", in: `[1]`, wantErr: "must be a JSON object"},
		{name: "string", in: `"x"`, wantErr: "must be a JSON object"},
		{name: "number", in: `1`, wantErr: "must be a JSON object"},
		{name: "boolean", in: `true`, wantErr: "must be a JSON object"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := NewArguments([]byte(tc.in))
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("want an error containing %q, got value %q and error %v", tc.wantErr, got, err)
				}
				if got != nil {
					t.Fatalf("an error must return no value, got %q", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			encoded, err := EncodeJSON(got)
			if err != nil {
				t.Fatalf("encode: %v", err)
			}
			if string(encoded) != tc.want {
				t.Fatalf("encoded = %s, want %s", encoded, tc.want)
			}
		})
	}
}

func TestNewArgumentsFreshCopy(t *testing.T) {
	in := []byte(`{"a":1}`)
	got, err := NewArguments(in)
	if err != nil {
		t.Fatal(err)
	}
	in[5] = '9' // change the caller's slice after the call
	if string(got) != `{"a":1}` {
		t.Fatalf("result aliases the input: %s", got)
	}
	got[5] = '7' // and the other way round
	if string(in) != `{"a":9}` {
		t.Fatalf("input aliases the result: %s", in)
	}
}

func TestArgumentsDecode(t *testing.T) {
	type target struct {
		A int    `json:"a"`
		B string `json:"b"`
	}

	t.Run("empty decodes as an empty object", func(t *testing.T) {
		v := target{A: 5}
		if err := Arguments(nil).Decode(&v); err != nil {
			t.Fatal(err)
		}
		if v.A != 5 || v.B != "" {
			t.Fatalf("empty arguments changed the target: %+v", v)
		}
		var m map[string]any
		if err := (Arguments{}).Decode(&m); err != nil || m == nil || len(m) != 0 {
			t.Fatalf("empty arguments must decode to an empty map: %v %v", m, err)
		}
	})

	t.Run("unknown keys are ignored", func(t *testing.T) {
		var v target
		if err := Arguments(`{"a":1,"extra":true,"b":"x"}`).Decode(&v); err != nil {
			t.Fatal(err)
		}
		if v.A != 1 || v.B != "x" {
			t.Fatalf("decoded %+v", v)
		}
	})

	t.Run("non-object bytes are an error", func(t *testing.T) {
		for _, in := range []string{`[1]`, `"x"`, `1`, `true`, `null`} {
			var v target
			err := Arguments(in).Decode(&v)
			if err == nil || !strings.Contains(err.Error(), "must be a JSON object") {
				t.Errorf("Decode(%s) = %v, want a must-be-an-object error", in, err)
			}
		}
	})

	t.Run("the receiver is not modified", func(t *testing.T) {
		a := Arguments(`{ "a" : 1 }`)
		before := bytes.Clone(a)
		var v target
		if err := a.Decode(&v); err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(a, before) {
			t.Fatalf("Decode modified the arguments: %s", a)
		}
	})

	t.Run("a nil target is an error, not a panic", func(t *testing.T) {
		if err := Arguments(`{}`).Decode(nil); err == nil {
			t.Fatal("want an error for a nil target")
		}
	})
}

// TestArgumentsUnmarshalJSONNormalisesAndTrimsJSONWhitespaceOnly pins WR-01 on
// the decode side and IN-02: an escaped separator is normalised to raw, so a
// decode followed by an encode is stable, and a Unicode space is not trimmed.
func TestArgumentsUnmarshalJSONNormalisesAndTrimsJSONWhitespaceOnly(t *testing.T) {
	var a Arguments
	if err := a.UnmarshalJSON([]byte(" {\"x\":\"\u2028\"}\n")); err != nil {
		t.Fatal(err)
	}
	want := "{\"x\":\"" + string([]byte{0xE2, 0x80, 0xA8}) + "\"}"
	if string(a) != want {
		t.Fatalf("Arguments = %q, want %q", a, want)
	}
	encoded, err := EncodeJSON(a)
	if err != nil || string(encoded) != want {
		t.Fatalf("re-encode = %q, %v; want the same bytes", encoded, err)
	}
	var b Arguments
	if err := b.UnmarshalJSON([]byte("\xc2\xa0{}")); err == nil {
		t.Fatalf("a leading U+00A0 must not be trimmed as whitespace, got %q", b)
	}
}
