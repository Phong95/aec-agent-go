package jsstr_test

import (
	"encoding/json"
	"math"
	"testing"
	"time"

	"github.com/Phong95/aec-agent-go/internal/jsstr"
	"github.com/Phong95/aec-agent-go/internal/wiretest"
)

// dateLegacyRows are the inputs of tests/testdata/golden/jsstr-date-parse.json that V8
// parses through its legacy date parser, which Go does not have: ParseDate returns
// NaN for them and the corpus test asserts exactly that. Anything outside the ES
// date time string format is in this table, so a row that Go gets wrong for any other
// reason fails the test instead of hiding here.
var dateLegacyRows = map[string]string{
	"+2026-03-04":                       "a plus sign with four digits",
	"002026-03-04":                      "six digits without a sign",
	"2026-":                             "a trailing dash",
	"2026-03-":                          "a trailing dash",
	"2026-3-4":                          "unpadded month and day",
	"2026/03/04":                        "slashes",
	"2026/03/04 10:00:00":               "slashes",
	"Mar 4 2026":                        "month name",
	"March 4, 2026":                     "month name",
	"Thu, 01 Jan 1970 00:00:00 GMT":     "RFC 2822 form",
	"Wed Mar 04 2026 10:00:00 GMT+0000": "Date.prototype.toString form",
	"03/04/2026":                        "US order",
	"4 Mar 2026":                        "month name",
	" 2026-03-04":                       "surrounding whitespace",
	"2026-03-04 ":                       "surrounding whitespace",
	"0":                                 "a bare number is a year",
	"1":                                 "a bare number is a month",
	"12":                                "a bare number is a month",
	"\uff12\uff10\uff12\uff16-03-04":    "fullwidth digits",
}

// TestParseDateMatchesGolden replays tests/testdata/golden/jsstr-date-parse.json, recorded
// from Date.parse under TZ=UTC; ParseDate gets time.UTC.
func TestParseDateMatchesGolden(t *testing.T) {
	var rows []struct {
		input string
		want  *float64
	}
	var raw [][2]json.RawMessage
	if err := json.Unmarshal(wiretest.Load(t, "jsstr-date-parse.json"), &raw); err != nil {
		t.Fatal(err)
	}
	for _, r := range raw {
		var in string
		if err := json.Unmarshal(r[0], &in); err != nil {
			t.Fatal(err)
		}
		var want *float64
		if err := json.Unmarshal(r[1], &want); err != nil {
			t.Fatal(err)
		}
		rows = append(rows, struct {
			input string
			want  *float64
		}{in, want})
	}
	if len(rows) < 100 {
		t.Fatalf("only %d rows", len(rows))
	}

	legacySeen := map[string]bool{}
	for _, r := range rows {
		got := jsstr.ParseDate(r.input, time.UTC)
		if why, legacy := dateLegacyRows[r.input]; legacy {
			legacySeen[r.input] = true
			if r.want == nil {
				t.Errorf("%q is listed as a legacy row (%s) but V8 no longer parses it", r.input, why)
			}
			if !math.IsNaN(got) {
				t.Errorf("legacy row %q: ParseDate = %v, the documented exception is NaN", r.input, got)
			}
			continue
		}
		switch {
		case r.want == nil && !math.IsNaN(got):
			t.Errorf("ParseDate(%q) = %v, want NaN", r.input, got)
		case r.want != nil && got != *r.want:
			t.Errorf("ParseDate(%q) = %v, want %v", r.input, got, *r.want)
		}
	}
	for in := range dateLegacyRows {
		if !legacySeen[in] {
			t.Errorf("legacy row %q is not in the corpus", in)
		}
	}
}

func TestParseDateLocal(t *testing.T) {
	loc := time.FixedZone("UTC+7", 7*3600)
	// A date time without a zone is local time, a date alone is UTC.
	if got, want := jsstr.ParseDate("2026-03-04T10:00:00", loc), float64(time.Date(2026, 3, 4, 3, 0, 0, 0, time.UTC).UnixMilli()); got != want {
		t.Errorf("local date time = %v, want %v", got, want)
	}
	if got, want := jsstr.ParseDate("2026-03-04", loc), float64(time.Date(2026, 3, 4, 0, 0, 0, 0, time.UTC).UnixMilli()); got != want {
		t.Errorf("date only = %v, want %v", got, want)
	}
	if got, want := jsstr.ParseDate("2026-03-04T10:00:00Z", loc), float64(time.Date(2026, 3, 4, 10, 0, 0, 0, time.UTC).UnixMilli()); got != want {
		t.Errorf("Z = %v, want %v", got, want)
	}
	if !math.IsNaN(jsstr.ParseDate("", nil)) {
		t.Error("the empty string must be NaN")
	}
}
