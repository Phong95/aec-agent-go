// Package viewkit holds the pure helpers that every sandboxhub.View backend needs
// and that must behave exactly like the JavaScript reference: clamping an option to a limit,
// cutting text at a byte count, applying a line range, decoding bytes the way
// JavaScript does. The in-memory sandbox (aecmock) and the host sandbox
// (sandbox/host) both import it, so the two ports cannot drift apart.
//
// It imports only the standard library and internal/jsstr, and it never touches
// the host: every function takes values and returns values.
package viewkit

import (
	"math"
	"math/big"
	"strconv"
	"strings"
	"time"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/Phong95/aec-agent-go/internal/jsstr"
)

// ClampOption is the clamp(value, fallback, max) of the in-memory sandbox: an
// absent or non-positive value gives min(fallback, max), any other value gives
// min(value, max). (NaN and Infinity would also count as absent; a Go int is
// neither.)
func ClampOption(value *int, fallback, maximum int) int {
	if value == nil || *value <= 0 {
		return min(fallback, maximum)
	}
	return min(*value, maximum)
}

// SliceBytes is Buffer.from(text).subarray(0, max).toString("utf8") for a text
// that is longer than max bytes, and text itself otherwise: it cuts the UTF-8
// bytes of text at max and decodes the rest as JavaScript does, so a cut inside a
// multi-byte character leaves one replacement character (U+FFFD) for the
// incomplete sequence.
//
// A negative max follows subarray(0, max): it drops that many bytes from the end
// (the output cap can reach it only after the process was already killed).
func SliceBytes(text string, maximum int) string {
	if len(text) <= maximum {
		return text
	}
	end := maximum
	if end < 0 {
		end = max(len(text)+end, 0)
	}
	return LossyUTF8([]byte(text[:end]))
}

// LossyUTF8 decodes b as UTF-8 the way JavaScript's Buffer.toString and the
// WHATWG decoder do: every maximal invalid subpart becomes one U+FFFD. Go's range
// loop yields one U+FFFD per invalid byte and strings.ToValidUTF8 collapses a run
// of them into one, so neither agrees with JS for a truncated multi-byte
// sequence ("\xe2\x82" is one replacement character in JS, two from a range loop).
// Overlong forms, encoded surrogates and code points above U+10FFFF are invalid
// here exactly as in the decoder.
func LossyUTF8(b []byte) string {
	if utf8.Valid(b) {
		return string(b)
	}
	out := make([]rune, 0, len(b))
	var (
		cp           rune
		seen, needed int
		lower, upper byte = 0x80, 0xBF
	)
	reset := func() {
		cp, seen, needed = 0, 0, 0
		lower, upper = 0x80, 0xBF
	}
	for i := 0; i < len(b); i++ {
		c := b[i]
		if needed == 0 {
			switch {
			case c <= 0x7F:
				out = append(out, rune(c))
			case c >= 0xC2 && c <= 0xDF:
				needed, cp = 1, rune(c&0x1F)
			case c >= 0xE0 && c <= 0xEF:
				if c == 0xE0 {
					lower = 0xA0
				}
				if c == 0xED {
					upper = 0x9F
				}
				needed, cp = 2, rune(c&0x0F)
			case c >= 0xF0 && c <= 0xF4:
				if c == 0xF0 {
					lower = 0x90
				}
				if c == 0xF4 {
					upper = 0x8F
				}
				needed, cp = 3, rune(c&0x07)
			default:
				out = append(out, utf8.RuneError)
			}
			continue
		}
		if c < lower || c > upper {
			// The sequence ended early: one replacement character for what was
			// read, then this byte starts over.
			reset()
			out = append(out, utf8.RuneError)
			i--
			continue
		}
		lower, upper = 0x80, 0xBF
		cp = cp<<6 | rune(c&0x3F)
		seen++
		if seen == needed {
			out = append(out, cp)
			reset()
		}
	}
	if needed != 0 {
		out = append(out, utf8.RuneError)
	}
	return string(out)
}

// LineRange applies the line range of readFile: the content is split on "\n"
// (a text with no newline is one line), start is max(1, startLine), end is
// min(lines, endLine), and the result is the lines start..end joined with "\n".
// It reports truncated when start > 1 or end < lines. With both bounds nil the
// content is returned unchanged and not truncated.
//
// JavaScript slice(start-1, end) is followed exactly: an end at or below 0 counts
// from the end of the list (an end of -1 drops the last line), and a start past
// the end gives the empty string.
func LineRange(content string, startLine, endLine *int) (string, bool) {
	if startLine == nil && endLine == nil {
		return content, false
	}
	lines := splitLines(content)
	start := 1
	if startLine != nil {
		start = max(1, *startLine)
	}
	end := len(lines)
	if endLine != nil {
		end = min(len(lines), *endLine)
	}
	truncated := start > 1 || end < len(lines)
	from, to := start-1, end
	if to < 0 {
		to = max(len(lines)+to, 0)
	}
	if from >= to {
		return "", truncated
	}
	return joinLines(lines[from:to]), truncated
}

func splitLines(s string) []string {
	var lines []string
	start := 0
	for i := range len(s) {
		if s[i] == '\n' {
			lines = append(lines, s[start:i])
			start = i + 1
		}
	}
	return append(lines, s[start:])
}

func joinLines(lines []string) string {
	n := len(lines) - 1
	for _, l := range lines {
		n += len(l)
	}
	buf := make([]byte, 0, n)
	for i, l := range lines {
		if i > 0 {
			buf = append(buf, '\n')
		}
		buf = append(buf, l...)
	}
	return string(buf)
}

// JSNumber is JavaScript Number(text): the text is trimmed with the JS whitespace
// set; "" is 0; "Infinity" with an optional sign is infinite; 0x, 0o and 0b
// prefixes read an unsigned integer in that base; anything else must be a decimal
// literal (digits with an optional fraction and exponent, ".5" and "5." included,
// no underscores, no "inf" or "nan"). Everything else is NaN. The in-memory
// sleep takes its operand through it.
func JSNumber(text string) float64 {
	s := jsstr.Trim(text)
	if s == "" {
		return 0
	}
	if len(s) > 2 && s[0] == '0' {
		switch s[1] {
		case 'x', 'X':
			return parseRadix(s[2:], 16)
		case 'o', 'O':
			return parseRadix(s[2:], 8)
		case 'b', 'B':
			return parseRadix(s[2:], 2)
		}
	}
	rest, negative := s, false
	if rest[0] == '+' || rest[0] == '-' {
		negative = rest[0] == '-'
		rest = rest[1:]
	}
	if rest == "Infinity" {
		if negative {
			return math.Inf(-1)
		}
		return math.Inf(1)
	}
	if !isDecimalLiteral(rest) {
		return math.NaN()
	}
	f, _ := strconv.ParseFloat(s, 64) // a range error still returns the infinity or zero JS gives
	return f
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }

// isDecimalLiteral reports whether s is an unsigned decimal literal (Infinity
// aside): digits [. digits] [e [sign] digits], or . digits [exponent].
func isDecimalLiteral(s string) bool {
	i, digits := 0, 0
	for i < len(s) && isDigit(s[i]) {
		i++
		digits++
	}
	if i < len(s) && s[i] == '.' {
		i++
		for i < len(s) && isDigit(s[i]) {
			i++
			digits++
		}
	}
	if digits == 0 {
		return false
	}
	if i < len(s) && (s[i] == 'e' || s[i] == 'E') {
		i++
		if i < len(s) && (s[i] == '+' || s[i] == '-') {
			i++
		}
		exp := 0
		for i < len(s) && isDigit(s[i]) {
			i++
			exp++
		}
		if exp == 0 {
			return false
		}
	}
	return i == len(s)
}

// parseRadix reads an unsigned integer in the base; any other character gives NaN.
func parseRadix(digits string, base int) float64 {
	if digits == "" || strings.ContainsAny(digits, "+-_") {
		return math.NaN()
	}
	n, ok := new(big.Int).SetString(digits, base)
	if !ok {
		return math.NaN()
	}
	f, _ := new(big.Float).SetInt(n).Float64()
	return f
}

// maxTimerDelay is the largest delay setTimeout honours, 2^31-1 milliseconds.
const maxTimerDelay = 2147483647

// TimeoutDelay is the delay Node's setTimeout uses for the given milliseconds: a
// value that is not a number, is below 1 or is above 2^31-1 becomes 1 ms (Node also
// prints a TimeoutOverflowWarning), and a fraction is truncated. The in-memory
// exec uses it for its timeout timer and for sleep, which is why "sleep abc",
// "sleep -1" and "sleep 1e10" all return after about a millisecond. The
// host and Docker exec timeouts used it too, so a ceiling (Limits.MaxExecTimeoutMs)
// that is zero or above 2^31-1 made every command time out after 1 ms; that is
// fixed, and they use ExecTimeoutMs and ExecTimeout instead.
func TimeoutDelay(ms float64) time.Duration {
	if !(ms >= 1 && ms <= maxTimerDelay) {
		return time.Millisecond
	}
	return time.Duration(math.Trunc(ms)) * time.Millisecond
}

// ExecTimeoutMs is the timeout of one exec in milliseconds: the requested timeout capped
// by the sandbox ceiling (Limits.MaxExecTimeoutMs). A ceiling of 0 or below is no cap, so
// the request is used as it is (such a ceiling never turns into a 1 ms timer). The request itself is validated as positive by the caller.
func ExecTimeoutMs(requestedMs, ceilingMs int) int {
	if ceilingMs <= 0 {
		return requestedMs
	}
	return min(requestedMs, ceilingMs)
}

// ExecTimeout is the timer duration of an exec timeout of ms milliseconds. Go timers take
// any duration, so there is no 2^31-1 ms rule; a value too large
// for a time.Duration saturates at the largest one, and a value below 1 ms is 1 ms (the
// callers pass positive values only).
func ExecTimeout(ms int) time.Duration {
	if ms < 1 {
		return time.Millisecond
	}
	if int64(ms) > math.MaxInt64/int64(time.Millisecond) {
		return time.Duration(math.MaxInt64)
	}
	return time.Duration(ms) * time.Millisecond
}

// GlobMatch reports whether name matches the filename glob. Every character is
// literal except "*", which matches any run of characters that are not line
// terminators, and "?", which matches exactly one UTF-16 code unit that is not a
// line terminator; the whole name must match. This is globToRegExp
// (^...$ with * as .* and ? as .) as a matcher: it works on UTF-16 units, so "?"
// does not match a whole astral character any better than JS does, and the four
// JS line terminators (LF, CR, U+2028, U+2029) are the only units a wildcard
// refuses. With foldCase both sides are lower-cased first (the JS "i" flag; only
// the Windows host uses it).
func GlobMatch(glob, name string, foldCase bool) bool {
	if foldCase {
		glob, name = jsstr.ToLower(glob), jsstr.ToLower(name)
	}
	g := utf16.Encode([]rune(glob))
	n := utf16.Encode([]rune(name))
	isTerminator := func(u uint16) bool { return u == '\n' || u == '\r' || u == 0x2028 || u == 0x2029 }

	gi, ni := 0, 0
	star, mark := -1, 0
	for ni < len(n) {
		switch {
		case gi < len(g) && g[gi] == '*':
			star, mark = gi, ni
			gi++
		case gi < len(g) && g[gi] == '?' && !isTerminator(n[ni]):
			gi++
			ni++
		case gi < len(g) && g[gi] != '?' && g[gi] == n[ni]:
			gi++
			ni++
		case star >= 0 && !isTerminator(n[mark]):
			// Let the last star take one more unit and retry after it.
			mark++
			ni = mark
			gi = star + 1
		default:
			return false
		}
	}
	for gi < len(g) && g[gi] == '*' {
		gi++
	}
	return gi == len(g)
}
