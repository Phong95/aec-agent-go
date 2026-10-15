package jsstr_test

import (
	"math"
	"testing"

	"github.com/Phong95/aec-agent-go/internal/jsstr"
)

// TestToFixed pins Number.prototype.toFixed on the values where strconv.FormatFloat
// differs: an exact binary tie goes UP in JS and to even in Go. The expected texts are
// what Node 22 prints for the same calls.
func TestToFixed(t *testing.T) {
	for _, tc := range []struct {
		x      float64
		digits int
		want   string
	}{
		{1.25, 1, "1.3"}, // exact tie: JS rounds up, strconv gives 1.2
		{0.25, 1, "0.3"},
		{0.75, 1, "0.8"},
		{1.35, 1, "1.4"}, // not an exact tie (1.350000000000000088...)
		{0.15, 1, "0.1"}, // 0.1499999999999999944...: below the tie, so it rounds down
		{2.5, 0, "3"},
		{0.5, 0, "1"},
		{1.5, 0, "2"},
		{3.5, 0, "4"},
		{1.005, 2, "1.00"}, // 1.00499999999999989...
		{123.456, 2, "123.46"},
		{0, 1, "0.0"},
		{math.Copysign(0, -1), 1, "0.0"},
		{0.04, 1, "0.0"},
		{0.05, 1, "0.1"}, // 0.05000000000000000277...
		{1280.0 / 1024, 1, "1.3"},
		{1048575.0 / 1024, 1, "1024.0"},
		{10, 0, "10"},
		{10, 3, "10.000"},
		{1e20, 1, "100000000000000000000.0"},
		{1e21, 2, "1e+21"},
		{-1.25, 1, "-1.3"},
		{-0.04, 1, "-0.0"},
		{-2.5, 0, "-3"},
		{math.NaN(), 1, "NaN"},
		{math.Inf(1), 1, "Infinity"},
		{math.Inf(-1), 1, "-Infinity"},
	} {
		if got := jsstr.ToFixed(tc.x, tc.digits); got != tc.want {
			t.Errorf("ToFixed(%v, %d) = %q, want %q", tc.x, tc.digits, got, tc.want)
		}
	}
}

// TestToFixedClampsDigits: JS throws a RangeError outside 0 to 100 digits, ToFixed clamps.
func TestToFixedClampsDigits(t *testing.T) {
	if got := jsstr.ToFixed(1234.5, 105); len(got) != len("1234.")+100 {
		t.Errorf("ToFixed(1234.5, 105) has length %d, want %d", len(got), len("1234.")+100)
	}
	if got := jsstr.ToFixed(2.5, -3); got != "3" {
		t.Errorf("ToFixed(2.5, -3) = %q, want \"3\"", got)
	}
}

// TestToFixedTieGoesUp states the tie rule on its own, so a regression to the strconv
// behaviour names the reason.
func TestToFixedTieGoesUp(t *testing.T) {
	if got := jsstr.ToFixed(1.25, 1); got != "1.3" {
		t.Fatalf("ToFixed(1.25, 1) = %q: an exact tie must round up as in JS (strconv gives 1.2)", got)
	}
}

// TestRound is Math.round: halves go toward positive infinity, and 0.49999999999999994
// stays 0 (Math.floor(x + 0.5) would give 1).
func TestRound(t *testing.T) {
	for _, tc := range []struct{ x, want float64 }{
		{0, 0},
		{0.4, 0},
		{0.5, 1},
		{0.6, 1},
		{1.5, 2},
		{2.5, 3},
		{-0.5, 0},
		{-1.5, -1},
		{-2.5, -2},
		{-2.6, -3},
		{0.49999999999999994, 0},
		{120, 120},
		{1 << 53, 1 << 53},
	} {
		if got := jsstr.Round(tc.x); got != tc.want {
			t.Errorf("Round(%v) = %v, want %v", tc.x, got, tc.want)
		}
	}
	if got := jsstr.Round(-0.2); !math.Signbit(got) || got != 0 {
		t.Errorf("Round(-0.2) = %v, want negative zero", got)
	}
	if got := jsstr.Round(math.NaN()); !math.IsNaN(got) {
		t.Errorf("Round(NaN) = %v", got)
	}
	if got := jsstr.Round(math.Inf(1)); !math.IsInf(got, 1) {
		t.Errorf("Round(+Inf) = %v", got)
	}
}
