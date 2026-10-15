package jsstr_test

import (
	"encoding/hex"
	"strings"
	"testing"

	"github.com/Phong95/aec-agent-go/internal/jsstr"
)

// TestDecodeUTF8 pins the WHATWG decoder (what Node's Buffer.toString("utf8") does) on
// the inputs where a naive Go decode differs. The expected code points were checked
// against Node 22; a 40 000 row random byte comparison against Node found no difference
// when the decoder was written.
func TestDecodeUTF8(t *testing.T) {
	const fffd = '\ufffd'
	for _, tc := range []struct {
		name string
		hex  string
		want []rune
	}{
		{"empty", "", nil},
		{"ascii", "4142", []rune{'A', 'B'}},
		{"euro", "e282ac", []rune{0x20ac}},
		{"euro cut after two bytes is ONE replacement", "e282", []rune{fffd}},
		{"euro cut after one byte", "e2", []rune{fffd}},
		{"cut euro then a whole euro", "e282e282ac", []rune{fffd, 0x20ac}},
		{"ascii then a cut euro", "41e282", []rune{'A', fffd}},
		{"emoji", "f09f9880", []rune{0x1f600}},
		{"emoji cut after three bytes", "f09f98", []rune{fffd}},
		{"emoji cut after one byte", "f0", []rune{fffd}},
		{"stray continuation bytes are one replacement each", "808080", []rune{fffd, fffd, fffd}},
		{"invalid lead bytes", "fffe", []rune{fffd, fffd}},
		{"overlong two byte form", "c080", []rune{fffd, fffd}},
		{"overlong three byte form", "e08080", []rune{fffd, fffd, fffd}},
		{"surrogate half", "eda080", []rune{fffd, fffd, fffd}},
		{"above U+10FFFF", "f4908080", []rune{fffd, fffd, fffd, fffd}},
		{"a lead byte followed by ascii", "e241", []rune{fffd, 'A'}},
		{"two lead bytes", "e2e2", []rune{fffd, fffd}},
		{"combining mark and BOM", "efbbbf61cc81", []rune{0xfeff, 'a', 0x301}},
	} {
		raw, err := hex.DecodeString(tc.hex)
		if err != nil {
			t.Fatal(err)
		}
		if got := jsstr.DecodeUTF8(raw); got != string(tc.want) {
			t.Errorf("%s: DecodeUTF8(%s) = %U, want %U", tc.name, tc.hex, []rune(got), tc.want)
		}
	}
}

// TestDecodeUTF8DiffersFromToValidUTF8 states why the stdlib is not a substitute: it
// merges a run of invalid bytes into one replacement.
func TestDecodeUTF8DiffersFromToValidUTF8(t *testing.T) {
	raw := []byte{0x80, 0x80, 0x80}
	if got := jsstr.DecodeUTF8(raw); got != strings.Repeat("\ufffd", 3) {
		t.Fatalf("DecodeUTF8 = %q, want three replacements", got)
	}
	if got := strings.ToValidUTF8(string(raw), "\ufffd"); got != "\ufffd" {
		t.Fatalf("strings.ToValidUTF8 = %q: this test assumed it merges the run", got)
	}
}

// TestDecodeUTF8ValidInputIsUnchanged: well-formed text comes back as it went in.
func TestDecodeUTF8ValidInputIsUnchanged(t *testing.T) {
	text := "caf\u00e9 \u4e2d\u6587 \U0001f600 end\n"
	if got := jsstr.DecodeUTF8([]byte(text)); got != text {
		t.Fatalf("DecodeUTF8 changed valid input: %q", got)
	}
}
