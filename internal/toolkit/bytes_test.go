package toolkit_test

import (
	"testing"

	"github.com/Phong95/aec-agent-go/internal/toolkit"
)

// TestFormatBytes pins FormatBytes on its boundaries and on the exact ties, where
// toFixed rounds up and strconv rounds to even.
func TestFormatBytes(t *testing.T) {
	for _, tc := range []struct {
		n    int64
		want string
	}{
		{0, "0 B"},
		{1, "1 B"},
		{1023, "1023 B"},
		{1024, "1.0 KB"},
		{1280, "1.3 KB"}, // 1.25 KB: an exact tie, JS rounds up
		{1536, "1.5 KB"},
		{100_000, "97.7 KB"},
		{1048575, "1024.0 KB"},
		{1048576, "1.0 MB"},
		{1048576 + 524288/2, "1.3 MB"}, // 1.25 MB
		{30_000_000, "28.6 MB"},
		{1 << 30, "1024.0 MB"},
	} {
		if got := toolkit.FormatBytes(tc.n); got != tc.want {
			t.Errorf("FormatBytes(%d) = %q, want %q", tc.n, got, tc.want)
		}
	}
}
