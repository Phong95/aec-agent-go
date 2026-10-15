package jsstr_test

import (
	"encoding/json"
	"math"
	"strconv"
	"testing"

	"github.com/Phong95/aec-agent-go/internal/jsstr"
	"github.com/Phong95/aec-agent-go/internal/wiretest"
)

// jsNumberText is String(n) for the values ParseInt returns: NaN, an integer (negative
// zero prints as 0) or an infinity. Only the integer form needs the exponent rule of
// JS for 1e21 and above.
func jsNumberText(f float64) string {
	switch {
	case math.IsNaN(f):
		return "NaN"
	case math.IsInf(f, 1):
		return "Infinity"
	case math.IsInf(f, -1):
		return "-Infinity"
	case f == 0:
		return "0"
	case math.Abs(f) >= 1e21:
		// Go and JS agree here: shortest digits and a signed exponent of at least two digits.
		return strconv.FormatFloat(f, 'e', -1, 64)
	}
	return strconv.FormatFloat(f, 'f', -1, 64)
}

// TestParseIntMatchesGolden replays tests/testdata/golden/jsstr-parseint.json: every input
// was run through the real parseInt(s, 10) of Node.
func TestParseIntMatchesGolden(t *testing.T) {
	var rows [][2]string
	if err := json.Unmarshal(wiretest.Load(t, "jsstr-parseint.json"), &rows); err != nil {
		t.Fatal(err)
	}
	if len(rows) < 50 {
		t.Fatalf("only %d rows", len(rows))
	}
	for _, r := range rows {
		if got := jsNumberText(jsstr.ParseInt(r[0])); got != r[1] {
			t.Errorf("ParseInt(%q) = %s, want %s", r[0], got, r[1])
		}
	}
}

func TestParseInt(t *testing.T) {
	for in, want := range map[string]string{
		"5": "5", "*/5": "NaN", "1-5": "1", " 7x": "7", "+3": "3", "-1": "-1", "": "NaN", "08": "8",
		"0x10": "0", "1e3": "1", "\ufeff9": "9", "\u00857": "NaN",
	} {
		if got := jsNumberText(jsstr.ParseInt(in)); got != want {
			t.Errorf("ParseInt(%q) = %s, want %s", in, got, want)
		}
	}
	if got := jsstr.ParseInt("-0"); got != 0 || !math.Signbit(got) {
		t.Errorf("ParseInt(\"-0\") = %v, want negative zero", got)
	}
}
