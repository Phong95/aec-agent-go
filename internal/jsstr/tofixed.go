package jsstr

import (
	"math"
	"math/big"
	"strconv"
	"strings"
)

// ToFixed is Number.prototype.toFixed(digits): x written with exactly digits digits
// after the decimal point, with the JS rounding rule. The spec picks the integer n for
// which n / 10^digits is closest to the EXACT value of x, and the LARGER n when two are
// equally close. That is why strconv.FormatFloat(x, 'f', digits, 64) is not a
// substitute: it rounds an exact tie to even, so 1.25 gives "1.2" there and "1.3" in
// JS (1.25 is exact in binary, so it is a real tie), and 0.25 gives "0.2" and "0.3".
//
// The exact value of x comes from math/big, so the result is right for every finite
// float and never depends on a binary-to-decimal approximation. digits is clamped to
// the 0 to 100 range JS accepts (JS throws a RangeError outside it). A value of 1e21 or
// more is written the way Number.prototype.toString writes it, as JS does; NaN and the
// infinities give "NaN", "Infinity" and "-Infinity". A negative value keeps its sign,
// including "-0.0" for a small negative one, as JS does, and negative zero gives "0.0".
func ToFixed(x float64, digits int) string {
	digits = min(max(digits, 0), 100)
	switch {
	case math.IsNaN(x):
		return "NaN"
	case math.IsInf(x, 1):
		return "Infinity"
	case math.IsInf(x, -1):
		return "-Infinity"
	}
	sign := ""
	if x < 0 {
		sign, x = "-", -x
	}
	if x >= 1e21 {
		return sign + strconv.FormatFloat(x, 'e', -1, 64)
	}
	r := new(big.Rat).SetFloat64(x)
	r.Mul(r, new(big.Rat).SetInt(new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(digits)), nil)))
	r.Add(r, big.NewRat(1, 2))
	// Floor of a non-negative rational: integer division of the numerator by the
	// denominator. Adding one half first turns the floor into round-half-up.
	s := new(big.Int).Div(r.Num(), r.Denom()).String()
	if digits == 0 {
		return sign + s
	}
	if len(s) <= digits {
		s = strings.Repeat("0", digits-len(s)+1) + s
	}
	return sign + s[:len(s)-digits] + "." + s[len(s)-digits:]
}

// Round is Math.round: the nearest integer, with an exact half rounded toward positive
// infinity (2.5 gives 3 and -2.5 gives -2). It is not math.Round, which rounds a half
// away from zero, and it is not math.Floor(x + 0.5), which gives 1 for
// 0.49999999999999994 because the sum rounds up. NaN, the infinities and every value
// whose magnitude is 2^52 or more (already an integer) are returned as they are, and a
// negative value that rounds to zero gives negative zero, as in JS.
func Round(x float64) float64 {
	if math.IsNaN(x) || math.IsInf(x, 0) || math.Abs(x) >= 1<<52 {
		return x
	}
	r := math.Floor(x)
	if x-r >= 0.5 {
		r++
	}
	if r == 0 && (x < 0 || math.Signbit(x)) {
		return math.Copysign(0, -1)
	}
	return r
}
