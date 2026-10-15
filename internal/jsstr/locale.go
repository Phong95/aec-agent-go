package jsstr

// JS a.localeCompare(b) for ASCII.
//
// The in-memory sandbox sorts directory listings and search results by the full
// path with localeCompare, so the order of punctuation, case and digits is
// behaviour a Go port must reproduce. Go's strings.Compare is byte order and a
// lower-case comparison is not the ICU order: the real order is, for example,
// "a" < "A" < "b" < "B", "a_b" < "a-b" < "a.txt" < "A.txt" and "10" < "2" (no
// numeric collation).
//
// The table below was generated from the golden fixture
// tests/testdata/golden/jsstr-corpus.json (section localeOrder, recorded from the real
// Node localeCompare with the locale "en-US"; Node 22.19, ICU 77.1, CLDR 47) and
// TestLocaleCompareMatchesICU proves it against all three comparison sections of
// that fixture. Regenerate it from the fixture, never by hand.

// asciiPrimaryOrder lists the ASCII characters that carry a weight, in ascending
// primary order. Letters appear once, in lower case: an upper-case letter has the
// same primary weight as its lower-case form and differs only at the tertiary
// level. The control characters U+0000-U+0008, U+000E-U+001F and U+007F are not
// listed because they are completely ignorable.
const asciiPrimaryOrder = "\t\n\v\f\r _-,;:!?.'\"()[]{}@*/\\&#%`^+<=>|~$0123456789abcdefghijklmnopqrstuvwxyz"

// nonASCIIBase is added to a code point above U+007F to give it a primary weight
// after every ASCII character, ordered by code point.
const nonASCIIBase = 1000

// asciiPrimary maps an ASCII byte to its primary weight; 0 means ignorable.
var asciiPrimary = buildASCIIPrimary()

func buildASCIIPrimary() [128]uint16 {
	var t [128]uint16
	rank := uint16(0)
	for i := range len(asciiPrimaryOrder) {
		rank++
		c := asciiPrimaryOrder[i]
		t[c] = rank
		if c >= 'a' && c <= 'z' {
			t[c-'a'+'A'] = rank
		}
	}
	return t
}

// collationKey is one non-ignorable character of a string: its primary weight and
// whether it is an upper-case ASCII letter (the tertiary difference).
type collationKey struct {
	primary uint32
	upper   bool
}

func collationKeys(s string) []collationKey {
	keys := make([]collationKey, 0, len(s))
	for _, r := range s {
		switch {
		case r < 0x80:
			p := asciiPrimary[r]
			if p == 0 {
				continue // ignorable control character: equal to the empty string
			}
			keys = append(keys, collationKey{primary: uint32(p), upper: r >= 'A' && r <= 'Z'})
		default:
			keys = append(keys, collationKey{primary: nonASCIIBase + uint32(r)})
		}
	}
	return keys
}

// LocaleCompare is a.localeCompare(b) for ASCII: -1, 0 or 1. It is the ICU
// multi-level comparison. Primary weights are compared over the whole string
// first with case ignored (so "A" < "ab" and "ab" < "B"), then, when the primary
// weights are all equal, lower case sorts before upper case at the first
// position where the case differs ("ab" < "aB" < "Ab" < "AB"). Control
// characters are ignorable: strings that differ only by them compare 0.
//
// Divergence from ICU: a character above U+007F gets a primary weight after every
// ASCII character, ordered by code point, and accents are not folded. The result
// depends on the Node default locale for non-ASCII text only; the golden fixture
// asserts that the default locale agrees on ASCII.
func LocaleCompare(a, b string) int {
	ka, kb := collationKeys(a), collationKeys(b)
	for i := 0; i < len(ka) && i < len(kb); i++ {
		if ka[i].primary != kb[i].primary {
			if ka[i].primary < kb[i].primary {
				return -1
			}
			return 1
		}
	}
	if len(ka) != len(kb) {
		if len(ka) < len(kb) {
			return -1
		}
		return 1
	}
	for i := range ka {
		if ka[i].upper != kb[i].upper {
			if !ka[i].upper {
				return -1
			}
			return 1
		}
	}
	return 0
}
