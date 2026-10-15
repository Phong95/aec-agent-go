package toolkit

import (
	"strconv"

	"github.com/Phong95/aec-agent-go/internal/jsstr"
)

// FormatBytes is the formatBytes of the workspace helpers: a byte count for the
// model, as "<n> B" below 1 KB, then KB and MB with one decimal. The decimal is the JS
// toFixed(1), whose exact ties round up (1280 bytes is "1.3 KB"; strconv would print
// "1.2 KB"), so it comes from jsstr.ToFixed. There is no GB step: a count of 1 GB
// prints as "1024.0 MB".
func FormatBytes(n int64) string {
	switch {
	case n < 1024:
		return strconv.FormatInt(n, 10) + " B"
	case n < 1024*1024:
		return jsstr.ToFixed(float64(n)/1024, 1) + " KB"
	default:
		return jsstr.ToFixed(float64(n)/1024/1024, 1) + " MB"
	}
}
