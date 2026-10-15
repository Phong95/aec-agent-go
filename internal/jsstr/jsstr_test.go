package jsstr_test

import (
	"slices"
	"testing"

	"github.com/Phong95/aec-agent-go/internal/jsstr"
)

// jsSpace is the JS WhiteSpace plus LineTerminator set, spelled out so the test
// does not restate the implementation's ranges.
var jsSpace = []rune{
	0x09, 0x0A, 0x0B, 0x0C, 0x0D, 0x20, 0xA0, 0x1680,
	0x2000, 0x2001, 0x2002, 0x2003, 0x2004, 0x2005, 0x2006, 0x2007, 0x2008, 0x2009, 0x200A,
	0x2028, 0x2029, 0x202F, 0x205F, 0x3000, 0xFEFF,
}

func TestIsSpace(t *testing.T) {
	set := map[rune]bool{}
	for _, r := range jsSpace {
		set[r] = true
	}
	// Every BMP code point is checked: the set is exactly the listed one.
	for r := rune(0); r <= 0xFFFF; r++ {
		if got := jsstr.IsSpace(r); got != set[r] {
			t.Fatalf("IsSpace(U+%04X) = %v, want %v", r, got, set[r])
		}
	}
	// The two characters Go and JS disagree on.
	if !jsstr.IsSpace(0xFEFF) {
		t.Error("U+FEFF is JS whitespace")
	}
	if jsstr.IsSpace(0x85) {
		t.Error("U+0085 is not JS whitespace (unicode.IsSpace says it is)")
	}
	if jsstr.IsSpace(0x1F600) || jsstr.IsSpace('a') {
		t.Error("non-space reported as space")
	}
}

func TestIsLineTerminator(t *testing.T) {
	for _, r := range []rune{'\n', '\r', 0x2028, 0x2029} {
		if !jsstr.IsLineTerminator(r) {
			t.Errorf("U+%04X is a line terminator", r)
		}
	}
	for _, r := range []rune{' ', '\t', 0x0B, 0x0C, 0x85, 0xA0, 0xFEFF, 'a'} {
		if jsstr.IsLineTerminator(r) {
			t.Errorf("U+%04X is not a line terminator", r)
		}
	}
	if !jsstr.HasLineTerminator("a\U00002028b") || jsstr.HasLineTerminator("a\tb\U000000a0c") {
		t.Error("HasLineTerminator")
	}
}

func TestTrim(t *testing.T) {
	bom, nbsp, nel, ls := "\U0000feff", "\U000000a0", "\u0085", "\U00002028"
	tests := []struct{ in, start, end, both string }{
		{"", "", "", ""},
		{" \t\r\n ", "", "", ""},
		{"  a b  ", "a b  ", "  a b", "a b"},
		{bom + "a" + bom, "a" + bom, bom + "a", "a"},
		{nbsp + "a" + nbsp, "a" + nbsp, nbsp + "a", "a"},
		// U+0085 is not JS whitespace, so it stays.
		{nel + "a" + nel, nel + "a" + nel, nel + "a" + nel, nel + "a" + nel},
		{ls + "a" + ls, "a" + ls, ls + "a", "a"},
		{"a\xffb \xff", "a\xffb \xff", "a\xffb \xff", "a\xffb \xff"},
	}
	for _, tc := range tests {
		if got := jsstr.TrimStart(tc.in); got != tc.start {
			t.Errorf("TrimStart(%q) = %q, want %q", tc.in, got, tc.start)
		}
		if got := jsstr.TrimEnd(tc.in); got != tc.end {
			t.Errorf("TrimEnd(%q) = %q, want %q", tc.in, got, tc.end)
		}
		if got := jsstr.Trim(tc.in); got != tc.both {
			t.Errorf("Trim(%q) = %q, want %q", tc.in, got, tc.both)
		}
	}
}

func TestFields(t *testing.T) {
	bom, nel := "\U0000feff", "\u0085"
	tests := []struct {
		in   string
		want []string
	}{
		{"", []string{}},
		{" \t ", []string{}},
		{"a", []string{"a"}},
		{"  a   b\tc\n", []string{"a", "b", "c"}},
		{"a" + bom + "b", []string{"a", "b"}},
		{"a" + nel + "b", []string{"a" + nel + "b"}},
		{"a\U000000a0\U00003000b", []string{"a", "b"}},
	}
	for _, tc := range tests {
		got := jsstr.Fields(tc.in)
		if got == nil || !slices.Equal(got, tc.want) {
			t.Errorf("Fields(%q) = %#v, want %#v", tc.in, got, tc.want)
		}
	}
	got := jsstr.FieldsFunc("a, b,,c ,", func(r rune) bool { return jsstr.IsSpace(r) || r == ',' })
	if !slices.Equal(got, []string{"a", "b", "c"}) {
		t.Errorf("FieldsFunc = %#v", got)
	}
}

func TestLeadingAndDrop(t *testing.T) {
	if n := jsstr.CountLeading("\U000000a0\U000000a0 x"); n != 3 {
		t.Errorf("CountLeading counts runes, got %d", n)
	}
	if n := jsstr.CountLeading("   "); n != 3 {
		t.Errorf("CountLeading of only whitespace = %d", n)
	}
	if got := jsstr.DropRunes("\U000000a0\U000000a0ab", 2); got != "ab" {
		t.Errorf("DropRunes = %q", got)
	}
	if got := jsstr.DropRunes("ab", 5); got != "" {
		t.Errorf("DropRunes beyond the end = %q", got)
	}
	if got := jsstr.DropRunes("ab", 0); got != "ab" {
		t.Errorf("DropRunes(0) = %q", got)
	}
}

func TestTruncateUTF16(t *testing.T) {
	astral := "\U0001F600" // two UTF-16 units
	tests := []struct {
		in   string
		n    int
		want string
	}{
		{"abc", 0, ""},
		{"abc", -1, ""},
		{"abc", 2, "ab"},
		{"abc", 3, "abc"},
		{"abc", 10, "abc"},
		{"a" + astral, 1, "a"},
		// n = 2 would cut the pair in half, so it is dropped whole.
		{"a" + astral, 2, "a"},
		{"a" + astral, 3, "a" + astral},
		{astral + "b", 1, ""},
		{astral + "b", 2, astral},
		{astral + "b", 3, astral + "b"},
		{"\U000000e9\U000000e9", 1, "\U000000e9"},
	}
	for _, tc := range tests {
		if got := jsstr.TruncateUTF16(tc.in, tc.n); got != tc.want {
			t.Errorf("TruncateUTF16(%q, %d) = %q, want %q", tc.in, tc.n, got, tc.want)
		}
	}
}

// TestToLowerMatchesJS compares ToLower with the real String.prototype.toLowerCase
// over the toLower rows of jsstr-corpus.json: U+0130, the final sigma in and out of
// word-final position, the Kelvin sign, case-ignorable characters between the sigma
// and its neighbours, and plain ASCII.
func TestToLowerMatchesJS(t *testing.T) {
	c := loadCorpus(t)
	if len(c.ToLower) < 40 {
		t.Fatalf("only %d toLower rows", len(c.ToLower))
	}
	for _, row := range c.ToLower {
		if got := jsstr.ToLower(row.Text); got != row.Lower {
			t.Errorf("ToLower(%+q) = %+q, want %+q", row.Text, got, row.Lower)
		}
	}
}

func TestToLowerSpecialCases(t *testing.T) {
	tests := []struct{ in, want string }{
		{"\u0130", "i\u0307"},
		{"SK\u0130LL.MD", "ski\u0307ll.md"},
		{"\u212a", "k"},
		{"\u0391\u03a3", "\u03b1\u03c2"},             // word-final sigma
		{"\u0391\u03a3\u0391", "\u03b1\u03c3\u03b1"}, // medial sigma
		{"\u03a3", "\u03c3"},                         // a lone sigma has no preceding letter
	}
	for _, tc := range tests {
		if got := jsstr.ToLower(tc.in); got != tc.want {
			t.Errorf("ToLower(%+q) = %+q, want %+q", tc.in, got, tc.want)
		}
	}
	if got := jsstr.ToLower("Hello, WORLD 123"); got != "hello, world 123" {
		t.Errorf("ASCII: %q", got)
	}
}
