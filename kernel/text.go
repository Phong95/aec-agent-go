package kernel

// UTF16Len returns the number of UTF-16 code units of s.
//
// The token counters and the mock provider measure text in UTF-16 code units
// (the JS string.length). They must never use len (bytes) or the rune count for
// it: a rune at or above U+10000 (an emoji,
// say) is one rune but two code units. Providers (Phase 8), the aecmock
// MockProvider and the token Counter (Phase 9) use this helper.
//
// A byte that is not valid UTF-8 decodes as U+FFFD and counts as one code unit.
// A JS string cannot hold invalid UTF-8, so there is no JS length to match.
func UTF16Len(s string) int {
	n := 0
	for _, r := range s {
		if r >= 0x10000 {
			n += 2
		} else {
			n++
		}
	}
	return n
}
