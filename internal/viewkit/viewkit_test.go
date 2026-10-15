package viewkit_test

import (
	"testing"

	"github.com/Phong95/aec-agent-go/internal/viewkit"
)

func ptr(n int) *int { return &n }

func TestClampOption(t *testing.T) {
	tests := []struct {
		name            string
		value           *int
		fallback, limit int
		want            int
	}{
		{"absent uses the fallback", nil, 10, 100, 10},
		{"absent with the limit below the fallback", nil, 500, 100, 100},
		{"zero is absent", ptr(0), 10, 100, 10},
		{"negative is absent", ptr(-5), 10, 100, 10},
		{"a value below the limit", ptr(40), 10, 100, 40},
		{"a value above the limit is clamped", ptr(400), 10, 100, 100},
		{"a value equal to the limit", ptr(100), 10, 100, 100},
		{"limit zero", ptr(5), 10, 0, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := viewkit.ClampOption(tc.value, tc.fallback, tc.limit); got != tc.want {
				t.Errorf("ClampOption = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestSliceBytes(t *testing.T) {
	const (
		euro  = "\U000020ac" // e2 82 ac
		eacut = "\U000000e9" // c3 a9
		smile = "\U0001f600" // f0 9f 98 80
		fffd  = "\U0000fffd"
	)
	tests := []struct {
		name string
		text string
		max  int
		want string
	}{
		{"fits", "hello", 5, "hello"},
		{"fits with room", "hello", 50, "hello"},
		{"ascii cut", "hello", 3, "hel"},
		{"zero bytes", "hello", 0, ""},
		{"empty", "", 0, ""},
		{"cut after a whole 2-byte character", "a" + eacut + "b", 3, "a" + eacut},
		{"cut inside a 2-byte character", "a" + eacut + "b", 2, "a" + fffd},
		{"cut inside a 3-byte character after one byte", euro, 1, fffd},
		{"cut inside a 3-byte character after two bytes", euro, 2, fffd},
		{"cut inside a 4-byte character after one byte", smile, 1, fffd},
		{"cut inside a 4-byte character after two bytes", smile, 2, fffd},
		{"cut inside a 4-byte character after three bytes", smile, 3, fffd},
		{"whole 4-byte character", smile + "x", 4, smile},
		{"negative max drops bytes from the end", "hello", -2, "hel"},
		{"negative max past the start", "hello", -9, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := viewkit.SliceBytes(tc.text, tc.max); got != tc.want {
				t.Errorf("SliceBytes(%q, %d) = %q, want %q", tc.text, tc.max, got, tc.want)
			}
		})
	}
}

func TestLossyUTF8(t *testing.T) {
	const fffd = "\U0000fffd"
	tests := []struct {
		name string
		in   []byte
		want string
	}{
		{"ascii", []byte("abc"), "abc"},
		{"valid multi-byte", []byte("\xe2\x82\xac"), "\U000020ac"},
		{"truncated 3-byte is one replacement", []byte("\xe2\x82"), fffd},
		{"truncated 3-byte followed by ascii", []byte("\xe2\x82a"), fffd + "a"},
		{"truncated 4-byte", []byte("\xf0\x9f\x98"), fffd},
		{"lone lead byte", []byte("\xc3"), fffd},
		{"stray continuation bytes are one each", []byte("\x80\x80"), fffd + fffd},
		{"overlong 2-byte form", []byte("\xc0\xaf"), fffd + fffd},
		{"overlong 3-byte form", []byte("\xe0\x80\xaf"), fffd + fffd + fffd},
		{"encoded surrogate", []byte("\xed\xa0\x80"), fffd + fffd + fffd},
		{"above U+10FFFF", []byte("\xf4\x90\x80\x80"), fffd + fffd + fffd + fffd},
		{"f5 is never valid", []byte("\xf5\x80"), fffd + fffd},
		{"0xff", []byte("a\xffb"), "a" + fffd + "b"},
		{"empty", nil, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := viewkit.LossyUTF8(tc.in); got != tc.want {
				t.Errorf("LossyUTF8(% x) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestLineRange(t *testing.T) {
	const text = "one\ntwo\nthree\nfour"
	tests := []struct {
		name       string
		start, end *int
		want       string
		truncated  bool
	}{
		{"no range", nil, nil, text, false},
		{"whole range", ptr(1), ptr(4), text, false},
		{"end beyond the file", ptr(1), ptr(99), text, false},
		{"middle", ptr(2), ptr(3), "two\nthree", true},
		{"only start", ptr(3), nil, "three\nfour", true},
		{"only end", nil, ptr(2), "one\ntwo", true},
		{"start zero counts as one", ptr(0), ptr(2), "one\ntwo", true},
		{"negative start counts as one", ptr(-3), ptr(1), "one", true},
		{"start past the end", ptr(9), nil, "", true},
		{"start after end", ptr(3), ptr(2), "", true},
		{"end zero", nil, ptr(0), "", true},
		{"negative end counts from the end", nil, ptr(-1), "one\ntwo\nthree", true},
		{"negative end past the start", ptr(2), ptr(-9), "", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, truncated := viewkit.LineRange(text, tc.start, tc.end)
			if got != tc.want || truncated != tc.truncated {
				t.Errorf("LineRange = (%q, %v), want (%q, %v)", got, truncated, tc.want, tc.truncated)
			}
		})
	}

	t.Run("a text without newline is one line", func(t *testing.T) {
		got, truncated := viewkit.LineRange("solo", ptr(1), ptr(1))
		if got != "solo" || truncated {
			t.Errorf("= (%q, %v)", got, truncated)
		}
	})
	t.Run("a trailing newline leaves an empty last line", func(t *testing.T) {
		got, truncated := viewkit.LineRange("a\nb\n", ptr(2), nil)
		if got != "b\n" || !truncated {
			t.Errorf("= (%q, %v)", got, truncated)
		}
	})
}
