// Package jsstr holds the JavaScript string semantics that the SKILL.md parser
// and the artifact view need.
//
// Go's unicode.IsSpace, strings.TrimSpace, strings.Fields and the RE2 \s class
// do not agree with JavaScript: JS whitespace includes U+FEFF and excludes
// U+0085, Go is the other way round. The SDK trims, splits and matches with
// the JS set, so code that used the standard library would accept or reject
// different inputs. Everything here follows the ECMAScript definitions (the
// WhiteSpace and LineTerminator productions), not the Go ones.
//
// Phase 10 adds what the memory pipeline needs on top: IsPunct and the Unicode 16
// tables (unicode16.go, generated from a recording of Node 22.19 because Go 1.25
// knows Unicode 15 and Go 1.27 Unicode 17), ParseInt (parseInt(s, 10)) and ParseDate
// (the ES date time string subset of Date.parse).
//
// The case and punctuation functions read only those generated tables, which are the
// complete V8 tables of Node 22.19.0 (Unicode 16.0): the package does not import
// Go's unicode package, so its results do not depend on the Unicode version of the
// Go toolchain (gap G-12-1; TestGeneratedTablesComparable enforces it).
package jsstr

import (
	"cmp"
	"slices"
	"strings"
	"unicode/utf8"
)

// IsSpace reports whether r is JS whitespace: what the regular expression class
// \s matches and what String.prototype.trim removes. It is U+0009-U+000D, U+0020,
// U+00A0, U+1680, U+2000-U+200A, U+2028, U+2029, U+202F, U+205F, U+3000 and
// U+FEFF. It does not include U+0085, which unicode.IsSpace does.
func IsSpace(r rune) bool {
	switch {
	case r >= 0x09 && r <= 0x0D:
		return true
	case r >= 0x2000 && r <= 0x200A:
		return true
	}
	switch r {
	case 0x20, 0xA0, 0x1680, 0x2028, 0x2029, 0x202F, 0x205F, 0x3000, 0xFEFF:
		return true
	}
	return false
}

// IsLineTerminator reports whether r is a JS line terminator: LF, CR, U+2028 or
// U+2029. These are the only characters the regular expression "." does not
// match (RE2's "." excludes just LF).
func IsLineTerminator(r rune) bool {
	return r == '\n' || r == '\r' || r == 0x2028 || r == 0x2029
}

// HasLineTerminator reports whether s contains a JS line terminator.
func HasLineTerminator(s string) bool {
	return strings.ContainsAny(s, "\n\r\U00002028\U00002029")
}

// TrimStart removes leading JS whitespace (String.prototype.trimStart). A byte
// that is not valid UTF-8 is never whitespace and stays.
func TrimStart(s string) string {
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		if !IsSpace(r) {
			return s[i:]
		}
		i += size
	}
	return ""
}

// TrimEnd removes trailing JS whitespace (String.prototype.trimEnd).
func TrimEnd(s string) string {
	for len(s) > 0 {
		r, size := utf8.DecodeLastRuneInString(s)
		if !IsSpace(r) {
			return s
		}
		s = s[:len(s)-size]
	}
	return s
}

// Trim removes leading and trailing JS whitespace (String.prototype.trim).
func Trim(s string) string {
	return TrimEnd(TrimStart(s))
}

// Fields is s.split(/\s+/) with the empty strings dropped, which is what
// JS writes as .split(/\s+/).filter(Boolean): the pieces of s between runs of
// JS whitespace.
func Fields(s string) []string {
	return FieldsFunc(s, IsSpace)
}

// FieldsFunc splits s around every maximal run of runes for which sep reports
// true and drops the empty pieces, so it covers split(/[\s,]+/).filter(Boolean)
// as well. The result is never nil.
func FieldsFunc(s string, sep func(rune) bool) []string {
	out := []string{}
	start := -1
	for i, r := range s {
		if sep(r) {
			if start >= 0 {
				out = append(out, s[start:i])
				start = -1
			}
		} else if start < 0 {
			start = i
		}
	}
	if start >= 0 {
		out = append(out, s[start:])
	}
	return out
}

// CountLeading returns the number of leading JS-whitespace runes of s, which is
// the indentation JS computes as raw.length - raw.trimStart().length (every JS
// whitespace character is a single UTF-16 unit, so runes and units agree).
func CountLeading(s string) int {
	n := 0
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		if !IsSpace(r) {
			return n
		}
		n++
		i += size
	}
	return n
}

// DropRunes returns s without its first n runes (s.slice(n) when the first n
// units are single-unit characters, as the indentation of a line is). It returns
// "" when s has fewer than n runes.
func DropRunes(s string, n int) string {
	i := 0
	for ; n > 0 && i < len(s); n-- {
		_, size := utf8.DecodeRuneInString(s[i:])
		i += size
	}
	return s[i:]
}

// TruncateUTF16 returns s cut to at most n UTF-16 code units. JS slice may cut
// a surrogate pair in half and leave a lone surrogate, which a Go string cannot
// hold, so a pair that would be split is dropped whole: the result is the
// longest prefix of s that fits in n units and ends on a character boundary. An
// n of zero or less gives "".
func TruncateUTF16(s string, n int) string {
	if n <= 0 {
		return ""
	}
	units := 0
	for i, r := range s {
		w := 1
		if r >= 0x10000 {
			w = 2
		}
		if units+w > n {
			return s[:i]
		}
		units += w
	}
	return s
}

// ToLower is String.prototype.toLowerCase. It maps each code point with the complete
// V8 table of Node 22.19.0 (Unicode 16.0; the generated lowerMappings of unicode16.go,
// not Go's unicode.ToLower, whose tables follow the Go toolchain: Go 1.25 knows Unicode
// 15, Go 1.27 Unicode 17, gap G-12-1), and for the two places where the ECMAScript full
// case mapping (Unicode SpecialCasing, without a locale) differs from the simple one:
// U+0130 (capital I with dot above) becomes "i" followed by U+0307, and U+03A3
// (capital sigma) becomes the final sigma U+03C2 when it ends a word, where the
// Final_Sigma rule holds, and the ordinary sigma U+03C3 otherwise. The Kelvin sign
// U+212A lowercases to "k" in both. A byte that is not valid UTF-8 becomes
// U+FFFD, as it would in a JS string decoded from the same bytes.
func ToLower(s string) string {
	ascii := true
	for i := range len(s) {
		if c := s[i]; c >= utf8.RuneSelf {
			ascii = false
			break
		}
	}
	if ascii {
		return strings.ToLower(s)
	}
	runes := []rune(s)
	var b strings.Builder
	b.Grow(len(s))
	for i, r := range runes {
		switch r {
		case 0x130:
			b.WriteString("i\U00000307")
		case 0x3A3:
			if isFinalSigma(runes, i) {
				b.WriteRune(0x3C2)
			} else {
				b.WriteRune(0x3C3)
			}
		default:
			if to, ok := lookupLower16(r); ok {
				b.WriteRune(to)
			} else {
				b.WriteRune(r) // V8 maps nothing else
			}
		}
	}
	return b.String()
}

// IsPunct reports whether r is in the Unicode general category P (punctuation), which
// is what the regular expression class \p{P} matches under the u flag. It searches the
// complete V8 table of Node 22.19.0 (Unicode 16.0; the generated punctRanges of
// unicode16.go) and not Go's unicode.P, which follows the Go toolchain (Go 1.25 lacks 13
// code points that V8 has, Go 1.27 has one that V8 lacks, gap G-12-1). The
// tokenizer of the memory index splits on /[\s\p{P}]+/u, so a punctuation mark that
// Go and V8 disagree on would split tokens differently.
func IsPunct(r rune) bool {
	return inRanges(punctRanges, r)
}

// inRanges reports whether r lies in one of the sorted, non-overlapping inclusive ranges.
func inRanges(table []runeRange, r rune) bool {
	_, found := slices.BinarySearchFunc(table, r, func(rg runeRange, target rune) int {
		switch {
		case rg.hi < target:
			return -1
		case rg.lo > target:
			return 1
		}
		return 0
	})
	return found
}

// SupplementVersion reports which Node release and Unicode version the tables of
// unicode16.go (the complete V8 tables, formerly a supplement to Go's) were generated
// under (for example "v22.19.0" and "16.0"). A
// caller that depends on the tables can log it; TestUnicode16Supplement fails when the
// recorded fixture comes from another Unicode version.
func SupplementVersion() (node, unicodeVersion string) {
	return supplementNode, supplementUnicode
}

// lookupLower16 finds r in the complete Unicode 16 (Node 22.19.0) lowercase table.
func lookupLower16(r rune) (rune, bool) {
	i, found := slices.BinarySearchFunc(lowerMappings, r, func(m lowerMapping, target rune) int {
		return cmp.Compare(m.from, target)
	})
	if !found {
		return 0, false
	}
	return lowerMappings[i].to, true
}

// isFinalSigma is the ECMAScript Final_Sigma condition for the capital sigma at
// runes[i]: it is preceded by a cased letter (with any case-ignorable characters
// between) and not followed by a cased letter (again past case-ignorables).
func isFinalSigma(runes []rune, i int) bool {
	before := false
	for j := i - 1; j >= 0; j-- {
		if isCaseIgnorable(runes[j]) {
			continue
		}
		before = isCased(runes[j])
		break
	}
	if !before {
		return false
	}
	for j := i + 1; j < len(runes); j++ {
		if isCaseIgnorable(runes[j]) {
			continue
		}
		return !isCased(runes[j])
	}
	return true
}

// isCased is the Unicode Cased property: Lowercase, Uppercase or titlecase letters. It
// searches the complete V8 table of Node 22.19.0 (Unicode 16.0; the generated casedRanges
// of unicode16.go) and not Go's tables, which follow the Go toolchain (gap G-12-1).
func isCased(r rune) bool {
	return inRanges(casedRanges, r)
}

// isCaseIgnorable is the Unicode Case_Ignorable property: nonspacing and enclosing
// marks, format characters, modifier letters and symbols, and the word-break
// MidLetter, MidNumLet and Single_Quote characters (apostrophe, full stop, colon,
// middle dot and their relatives). It searches the complete V8 table of Node 22.19.0
// (Unicode 16.0; the generated caseIgnorableRanges of unicode16.go), which holds that
// whole family, and not Go's tables, which follow the Go toolchain (gap G-12-1).
func isCaseIgnorable(r rune) bool {
	return inRanges(caseIgnorableRanges, r)
}
