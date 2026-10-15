package kernel

import "testing"

// TestUTF16Len checks the UTF-16 code unit count. Every non-ASCII string is built from rune or
// byte values so this source stays ASCII-only.
func TestUTF16Len(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want int
	}{
		{"empty", "", 0},
		{"ascii", "abc", 3},
		{"two byte rune", string(rune(0xe9)), 1},
		{"three byte rune", string(rune(0x1ec7)), 1},
		{"supplementary plane rune", string(rune(0x1f600)), 2},
		{"mixed", "a" + string(rune(0xe9)) + string(rune(0x1ec7)) + string(rune(0x1f600)) + "z", 6},
		{"two supplementary runes", string([]rune{0x1f600, 0x10000}), 4},
		{"invalid byte counts one", string([]byte{'a', 0xff, 'b'}), 3},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := UTF16Len(tt.in); got != tt.want {
				t.Errorf("UTF16Len(%q) = %d, want %d", tt.in, got, tt.want)
			}
		})
	}
}
