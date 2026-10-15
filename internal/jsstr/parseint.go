package jsstr

import (
	"math"
	"strconv"
)

// ParseInt is parseInt(s, 10): it skips leading JS whitespace, reads an optional
// sign and then the longest run of ASCII decimal digits, and ignores the rest of the
// string. It returns NaN when there is no digit, so "*/5" is NaN while "1-5" is 1,
// "08" is 8, "0x10" is 0 (the radix is fixed to 10, so no hex prefix) and "-0" is
// negative zero. A digit run too long for a float64 is correctly rounded, or
// infinite beyond the float64 range, as in JS.
//
// The scheduled-job cron matcher of the agent relies on this exact leniency:
// its field handling is bug for bug that of JS parseInt.
func ParseInt(s string) float64 {
	s = TrimStart(s)
	sign := 1.0
	if s != "" && (s[0] == '+' || s[0] == '-') {
		if s[0] == '-' {
			sign = -1
		}
		s = s[1:]
	}
	end := 0
	for end < len(s) && s[end] >= '0' && s[end] <= '9' {
		end++
	}
	if end == 0 {
		return math.NaN()
	}
	f, _ := strconv.ParseFloat(s[:end], 64) // a range error leaves +Inf, which is what JS gives
	return sign * f
}
