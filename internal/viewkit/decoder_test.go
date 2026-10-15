package viewkit_test

import (
	"strings"
	"testing"

	"github.com/Phong95/aec-agent-go/internal/viewkit"
)

// decoderInputs mixes ASCII, two, three and four byte characters with every kind of
// invalid input the WHATWG decoder distinguishes: stray continuation bytes, bad lead
// bytes, truncated sequences, overlong forms, an encoded surrogate and a code point
// above U+10FFFF.
var decoderInputs = map[string]string{
	"ascii":              "plain text\n",
	"two byte":           "h\U000000e9llo w\U000000f6rld",
	"three byte":         "\U000065e5\U0000672c\U00008a9e \U000020ac",
	"four byte":          "a\U0001f600b\U0001f680c",
	"stray continuation": "a\x80b\xbfc",
	"invalid lead":       "\xff\xfe a \xc0\xaf",
	"truncated two":      "x\xc3",
	"truncated three":    "x\xe2\x82",
	"truncated four":     "x\xf0\x9f\x98",
	"truncated then ok":  "\xe2\x82\xe2\x82\xac end",
	"overlong three":     "\xe0\x80\x80 \xe0\x9f\xbf",
	"surrogate":          "\xed\xa0\x80 \xed\xbf\xbf",
	"above max":          "\xf4\x90\x80\x80 \xf5\x80\x80\x80",
	"four byte bounds":   "\xf0\x80\x80\x80 \xf0\x9f\x98\x80",
	"empty":              "",
	"mixed":              "ok \xe2\x82\xac bad \xff \xf0\x9f\x98\x80 cut \xe6\x97",
}

// decodeSplit feeds data to a Decoder in the given pieces and returns the joined output.
func decodeSplit(data []byte, cuts ...int) string {
	var d viewkit.Decoder
	var out strings.Builder
	start := 0
	for _, cut := range append(cuts, len(data)) {
		out.WriteString(d.Write(data[start:cut]))
		start = cut
	}
	out.WriteString(d.End())
	return out.String()
}

// TestDecoderEverySplitPoint cuts every input at every position (and at every pair
// of positions) and requires the output to be LossyUTF8 of the whole input.
func TestDecoderEverySplitPoint(t *testing.T) {
	for name, input := range decoderInputs {
		t.Run(name, func(t *testing.T) {
			data := []byte(input)
			want := viewkit.LossyUTF8(data)
			for i := 0; i <= len(data); i++ {
				if got := decodeSplit(data, i); got != want {
					t.Fatalf("split at %d: got %q, want %q", i, got, want)
				}
				for j := i; j <= len(data); j++ {
					if got := decodeSplit(data, i, j); got != want {
						t.Fatalf("splits at %d and %d: got %q, want %q", i, j, got, want)
					}
				}
			}
		})
	}
}

// TestDecoderByteAtATime is the worst case: every byte arrives alone.
func TestDecoderByteAtATime(t *testing.T) {
	for name, input := range decoderInputs {
		t.Run(name, func(t *testing.T) {
			data := []byte(input)
			cuts := make([]int, 0, len(data))
			for i := 1; i < len(data); i++ {
				cuts = append(cuts, i)
			}
			if got, want := decodeSplit(data, cuts...), viewkit.LossyUTF8(data); got != want {
				t.Fatalf("got %q, want %q", got, want)
			}
		})
	}
}

// TestDecoderHoldsBackAnIncompleteCharacter shows what a per-chunk decode gets
// wrong: a euro sign split across two writes comes out whole.
func TestDecoderHoldsBackAnIncompleteCharacter(t *testing.T) {
	var d viewkit.Decoder
	if got := d.Write([]byte("a\xe2\x82")); got != "a" {
		t.Errorf("first write = %q, want %q (the two bytes of the euro sign are held back)", got, "a")
	}
	if got := d.Write([]byte("\xacb")); got != "\U000020acb" {
		t.Errorf("second write = %q, want %q", got, "\U000020acb")
	}
	if got := d.End(); got != "" {
		t.Errorf("End = %q, want nothing left", got)
	}
}

// TestDecoderEndFlushesADanglingSequence: a stream that ends inside a character
// ends with one replacement character, and the Decoder is reusable afterwards.
func TestDecoderEndFlushesADanglingSequence(t *testing.T) {
	var d viewkit.Decoder
	if got := d.Write([]byte("x\xf0\x9f")); got != "x" {
		t.Errorf("write = %q, want %q", got, "x")
	}
	if got := d.End(); got != "\U0000fffd" {
		t.Errorf("End = %q, want one replacement character", got)
	}
	if got := d.End(); got != "" {
		t.Errorf("a second End = %q, want nothing", got)
	}
	if got := d.Write([]byte("ok")); got != "ok" {
		t.Errorf("write after End = %q, want %q", got, "ok")
	}
}

func TestDecoderZeroValueAndEmptyWrites(t *testing.T) {
	var d viewkit.Decoder
	if got := d.Write(nil); got != "" {
		t.Errorf("Write(nil) = %q", got)
	}
	if got := d.Write([]byte{}); got != "" {
		t.Errorf("Write(empty) = %q", got)
	}
	if got := d.End(); got != "" {
		t.Errorf("End = %q", got)
	}
}

func TestIncompleteTail(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want int
	}{
		{"empty", "", 0},
		{"ascii", "abc", 0},
		{"complete two byte", "a\xc3\xa9", 0},
		{"lone lead of two", "a\xc3", 1},
		{"lone lead of three", "a\xe2", 1},
		{"two of three", "a\xe2\x82", 2},
		{"complete three", "a\xe2\x82\xac", 0},
		{"two of four", "a\xf0\x9f", 2},
		{"three of four", "a\xf0\x9f\x98", 3},
		{"complete four", "a\xf0\x9f\x98\x80", 0},
		{"stray continuation", "a\x80", 0},
		{"too many continuations", "a\xc3\xa9\xa9", 0},
		{"invalid lead", "a\xff", 0},
		{"overlong lead", "a\xc0", 0},
		{"E0 with a low second byte", "a\xe0\x80", 0},
		{"ED with a surrogate second byte", "a\xed\xa0", 0},
		{"F0 with a low second byte", "a\xf0\x80", 0},
		{"F4 with a high second byte", "a\xf4\x90", 0},
		{"F4 with a valid second byte", "a\xf4\x8f", 2},
		{"continuations without a lead in reach", "\x80\x80\x80\x80", 0},
	}
	for _, tc := range tests {
		if got := viewkit.IncompleteTail([]byte(tc.in)); got != tc.want {
			t.Errorf("%s: IncompleteTail(%q) = %d, want %d", tc.name, tc.in, got, tc.want)
		}
	}
}
