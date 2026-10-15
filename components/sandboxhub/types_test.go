package sandboxhub

import (
	"strings"
	"testing"

	"github.com/Phong95/aec-agent-go/internal/jsonx"
)

func TestSelectionRoundTripsAllWireForms(t *testing.T) {
	forms := []string{
		`"shared"`,
		`"none"`,
		`{"own":{}}`,
		`{"own":{"image":"node:22"}}`,
		`{"own":{"kind":"docker","image":"node:22","network":"allowlist","limits":{"cpus":0.5,"memoryMb":512,"pids":128}}}`,
		`{"own":{"limits":{"pids":0}}}`,
	}
	for _, in := range forms {
		t.Run(in, func(t *testing.T) {
			var s Selection
			if err := jsonx.UnmarshalStrict([]byte(in), &s); err != nil {
				t.Fatalf("decode: %v", err)
			}
			out, err := jsonx.Marshal(s)
			if err != nil {
				t.Fatalf("encode: %v", err)
			}
			if string(out) != in {
				t.Fatalf("got %s, want %s", out, in)
			}
		})
	}
}

func TestSelectionModes(t *testing.T) {
	var s Selection
	if err := jsonx.UnmarshalStrict([]byte(`"shared"`), &s); err != nil || s.Mode != SelectionShared || s.Own != nil {
		t.Fatalf("shared: %+v err=%v", s, err)
	}
	if err := jsonx.UnmarshalStrict([]byte(`"none"`), &s); err != nil || s.Mode != SelectionNone || s.Own != nil {
		t.Fatalf("none: %+v err=%v", s, err)
	}
	if err := jsonx.UnmarshalStrict([]byte(`{"own":{"image":"x"}}`), &s); err != nil || s.Mode != SelectionOwn || s.Own == nil || s.Own.Image != "x" {
		t.Fatalf("own: %+v err=%v", s, err)
	}
}

// The six malformed inputs of the plan must be rejected with an error, never a panic.
func TestSelectionRejectsMalformed(t *testing.T) {
	bad := []string{`"other"`, `42`, `null`, `[]`, `{}`, `{"own":1}`}
	for _, in := range bad {
		t.Run(in, func(t *testing.T) {
			var s Selection
			if err := s.UnmarshalJSON([]byte(in)); err == nil {
				t.Fatalf("expected an error for %s, got %+v", in, s)
			}
		})
	}
}

// Further hostile shapes: extra keys, null own, unknown own fields, trailing data.
func TestSelectionRejectsMoreShapes(t *testing.T) {
	bad := []string{
		`{"own":{},"extra":1}`,
		`{"own":null}`,
		`{"own":[]}`,
		`{"own":{"unknown":1}}`,
		`{"shared":{}}`,
		`""`,
		`true`,
	}
	for _, in := range bad {
		t.Run(in, func(t *testing.T) {
			var s Selection
			if err := s.UnmarshalJSON([]byte(in)); err == nil {
				t.Fatalf("expected an error for %s, got %+v", in, s)
			}
		})
	}
}

func TestSelectionMarshalZeroIsError(t *testing.T) {
	if _, err := jsonx.Marshal(Selection{}); err == nil {
		t.Fatal("marshalling a zero Selection must fail")
	}
}

func TestSelectionMarshalInconsistentStates(t *testing.T) {
	cases := map[string]Selection{
		"own without config":    {Mode: SelectionOwn},
		"shared with config":    {Mode: SelectionShared, Own: &OwnConfig{}},
		"none with config":      {Mode: SelectionNone, Own: &OwnConfig{}},
		"unknown mode":          {Mode: "bogus"},
		"unknown mode (config)": {Mode: "bogus", Own: &OwnConfig{}},
	}
	for name, s := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := jsonx.Marshal(s); err == nil {
				t.Fatalf("expected an error for %+v", s)
			}
		})
	}
}

// The own config keeps '<' unescaped like JSON.stringify.
func TestSelectionOwnNoHTMLEscape(t *testing.T) {
	s := Selection{Mode: SelectionOwn, Own: &OwnConfig{Image: "a<b>&c"}}
	out, err := jsonx.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), `a<b>&c`) {
		t.Fatalf("html characters were escaped: %s", out)
	}
}
