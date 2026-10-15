package agent

import (
	"encoding/json"
	"errors"
	"slices"
	"testing"
	"time"
	_ "time/tzdata" // the DST test needs the IANA zones on any runner, so it never depends on the host

	"github.com/Phong95/aec-agent-go/internal/wiretest"
)

// cronRow is one row of the recorded fixture agent-cron.json, recorded under TZ=UTC:
// the expression, the pinned clock in epoch ms and either the wait in ms or the text
// of the error.
type cronRow struct {
	Expr   string  `json:"expr"`
	NowMs  int64   `json:"nowMs"`
	WaitMs *int64  `json:"waitMs"`
	Throws *string `json:"throws"`
}

// cronDivergence is the expected value of a recorded row that the strict cron reader
// reads differently from the fixture: a wait (in ms), or invalid for an expression
// refused at Start (an error matching ErrInvalidCronExpression).
type cronDivergence struct {
	waitMs  int64
	invalid bool
	why     string
}

// cronDivergences are the rows of agent-cron.json where the strict reader deliberately
// differs from the recorded value (the fixture was recorded with a lenient reader and a
// 48 h search window), keyed by expression and clock. Every other row must still equal
// the fixture. The expected waits were checked by hand against the calendar
// (2026-03-04 is a Wednesday).
var cronDivergences = map[cronKey]cronDivergence{
	{",5 * * * *", 1772618400000}:      {invalid: true, why: "Q2: an empty list item is an error (TS: NaN part ignored)"},
	{"5, * * * *", 1772618400000}:      {invalid: true, why: "Q2: an empty list item is an error"},
	{"*/5 * * * *", 1772618400000}:     {waitMs: 300000, why: "Q2: */5 is every fifth minute (TS: NaN, fired after 24 h)"},
	{"0 */2 * * *", 1772618400000}:     {waitMs: 7200000, why: "Q2: */2 hours, next 12:00 (TS: 24 h fallback)"},
	{"0 0 * * 1-5", 1772618400000}:     {waitMs: 50400000, why: "Q2: 1-5 is Monday to Friday, next Thursday 00:00 (TS: 1 only)"},
	{"+5 * * * *", 1772618400000}:      {invalid: true, why: "Q2: a sign is an error (TS parseInt read 5)"},
	{"5x * * * *", 1772618400000}:      {invalid: true, why: "Q2: trailing text is an error (TS parseInt read 5)"},
	{"0x10 * * * *", 1772618400000}:    {invalid: true, why: "Q2: hex-looking text is an error (TS parseInt read 0)"},
	{"-1 * * * *", 1772618400000}:      {invalid: true, why: "Q2: a negative value is an error (TS: never matched)"},
	{"abc * * * *", 1772618400000}:     {invalid: true, why: "Q2: a word is an error (TS: NaN, 24 h fallback)"},
	{"0 0 * * 0", 1772618400000}:       {waitMs: 309600000, why: "Q2: next Sunday 00:00, beyond the TS 48 h window"},
	{"0 0 * * 1", 1772618400000}:       {waitMs: 396000000, why: "Q2: next Monday 00:00, beyond the TS 48 h window"},
	{"0 0 * * 3", 1772618400000}:       {waitMs: 568800000, why: "Q2: next Wednesday 00:00, beyond the TS 48 h window"},
	{"0 0 * * 6", 1772618400000}:       {waitMs: 223200000, why: "Q2: next Saturday 00:00, beyond the TS 48 h window"},
	{"0 0 * * 7", 1772618400000}:       {waitMs: 309600000, why: "Q2: 7 is Sunday (TS: never matched)"},
	{"0 24 * * *", 1772618400000}:      {invalid: true, why: "Q2: hour 24 is out of range (TS: never matched)"},
	{"0 0 1 13 *", 1772618400000}:      {invalid: true, why: "Q2: month 13 is out of range"},
	{"0 0 1 0 *", 1772618400000}:       {invalid: true, why: "Q2: month 0 is out of range"},
	{"0 0 31 * *", 1777550400000}:      {waitMs: 2635200000, why: "Q2: 31 May 00:00, beyond the TS 48 h window"},
	{"0 10 7 3 *", 1772618400000}:      {waitMs: 259200000, why: "Q2: 7 March 10:00, beyond the TS 48 h window"},
	{"0 0 29 2 *", 1803859200000}:      {waitMs: 31536000000, why: "Q2: 29 February 2028, beyond the TS 48 h window"},
	{"0 0 * * 0", 1798761599000}:       {waitMs: 172801000, why: "Q2: Sunday 3 January 2027 00:00, beyond the TS 48 h window"},
	{"  0   8 * *  1 ", 1772618400000}: {waitMs: 424800000, why: "Q2: Monday 9 March 08:00, beyond the TS 48 h window"},
}

// cronKey identifies a recorded row.
type cronKey struct {
	expr  string
	nowMs int64
}

// TestCronMatchesGolden replays agent-cron.json: nextCronWait equals the recorded
// fixture on every row, wait or error text, except on the rows of cronDivergences,
// where nextCronWait reads standard cron syntax, searches about four years ahead and
// refuses what it cannot read. The wrong field count keeps the recorded error text.
func TestCronMatchesGolden(t *testing.T) {
	var rows []cronRow
	if err := json.Unmarshal(wiretest.Load(t, "agent-cron.json"), &rows); err != nil {
		t.Fatalf("decode agent-cron: %v", err)
	}
	if len(rows) < 20 {
		t.Fatalf("the corpus holds %d rows, want at least 20", len(rows))
	}
	var waits, errs int
	used := map[cronKey]bool{}
	for _, r := range rows {
		now := time.UnixMilli(r.NowMs).In(time.UTC)
		got, err := nextCronWait(r.Expr, now)
		if d, ok := cronDivergences[cronKey{r.Expr, r.NowMs}]; ok {
			used[cronKey{r.Expr, r.NowMs}] = true
			switch {
			case d.invalid:
				if !errors.Is(err, ErrInvalidCronExpression) {
					t.Errorf("%s: nextCronWait(%q) = %v, %v, want an invalid expression error", d.why, r.Expr, got, err)
				}
			case err != nil:
				t.Errorf("%s: nextCronWait(%q) error = %v, want %d ms", d.why, r.Expr, err, d.waitMs)
			case got != time.Duration(d.waitMs)*time.Millisecond:
				t.Errorf("%s: nextCronWait(%q) = %v, want %v", d.why, r.Expr, got, time.Duration(d.waitMs)*time.Millisecond)
			}
			continue
		}
		switch {
		case r.Throws != nil:
			errs++
			if err == nil {
				t.Errorf("nextCronWait(%q, %s) = %v, want the TS error %q", r.Expr, now, got, *r.Throws)
				continue
			}
			if err.Error() != *r.Throws {
				t.Errorf("nextCronWait(%q) error = %q, want %q", r.Expr, err.Error(), *r.Throws)
			}
			if !errors.Is(err, ErrInvalidCronExpression) {
				t.Errorf("nextCronWait(%q) error does not match ErrInvalidCronExpression", r.Expr)
			}
		case r.WaitMs != nil:
			waits++
			if err != nil {
				t.Errorf("nextCronWait(%q, %s) error = %v, want a wait of %d ms", r.Expr, now, err, *r.WaitMs)
				continue
			}
			if want := time.Duration(*r.WaitMs) * time.Millisecond; got != want {
				t.Errorf("nextCronWait(%q, %s) = %v, want %v (TS)", r.Expr, now.Format(time.RFC3339Nano), got, want)
			}
		default:
			t.Errorf("row %q has neither waitMs nor throws", r.Expr)
		}
	}
	for k, d := range cronDivergences {
		if !used[k] {
			t.Errorf("divergence %q at %d (%s) matches no recorded row: the corpus changed", k.expr, k.nowMs, d.why)
		}
	}
	if waits < 20 || errs < 5 {
		t.Fatalf("the corpus holds %d waits and %d errors that agree with TS, want at least 20 and 5", waits, errs)
	}
}

// TestCron_FieldSyntax: a field is standard cron syntax, not a lenient integer
// reading. "*/5" is every fifth minute, "1-5" is a range, steps and lists combine, and
// what a lenient reader would accept ("5x", "+3", "0x10", "-1", "1e3", an empty list
// item) is an error that matches ErrInvalidCronExpression. The table keeps the lenient
// reading of each field in a comment.
func TestCron_FieldSyntax(t *testing.T) {
	cases := []struct {
		field string
		want  []int // the minutes the field allows; nil means the field is an error
	}{
		{"5", []int{5}},
		{"*/5", []int{0, 5, 10, 15, 20, 25, 30, 35, 40, 45, 50, 55}}, // lenient: NaN, never
		{"1-5", []int{1, 2, 3, 4, 5}},                                // lenient: 1 only
		{"1-10/3", []int{1, 4, 7, 10}},
		{"50/4", []int{50, 54, 58}},
		{"1,2,3", []int{1, 2, 3}},
		{"0-2,57-59", []int{0, 1, 2, 57, 58, 59}},
		{"08", []int{8}},
		{"59", []int{59}},
		{"60", nil},   // out of range
		{" 7x", nil},  // lenient: 7
		{"+3", nil},   // lenient: 3
		{"-1", nil},   // lenient: -1, never
		{"0x10", nil}, // lenient: 0
		{"1e3", nil},  // lenient: 1
		{"abc", nil},  // lenient: NaN, never
		{",5", nil},   // lenient: the empty part is NaN, the other matches
		{"5,", nil},
		{"5-1", nil}, // reversed range
		{"*/0", nil}, // zero step
		{"*/x", nil},
	}
	for _, c := range cases {
		t.Run(c.field, func(t *testing.T) {
			s, err := parseCron(c.field + " * * * *")
			if c.want == nil {
				if err == nil || !errors.Is(err, ErrInvalidCronExpression) {
					t.Fatalf("parseCron(minute %q) = %v, want an error matching ErrInvalidCronExpression", c.field, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseCron(minute %q): %v", c.field, err)
			}
			var got []int
			for m := range 60 {
				if s.minute.has(m) {
					got = append(got, m)
				}
			}
			if !slices.Equal(got, c.want) {
				t.Errorf("minute %q allows %v, want %v", c.field, got, c.want)
			}
		})
	}
}

// matchesAt reports whether the schedule matches the minute of t.
func matchesAt(s cronSchedule, t time.Time) bool {
	return s.minute.has(t.Minute()) && s.hour.has(t.Hour()) && s.month.has(int(t.Month())) && s.dayMatches(t)
}

// TestCron_FieldsAreRead: the fields are the five clock readings: weekday 0 and 7
// are Sunday, month is 1 to 12, an out of range value is an
// error, and when both day fields are restricted either one matches (standard cron).
func TestCron_FieldsAreRead(t *testing.T) {
	sunday := time.Date(2026, 3, 8, 6, 30, 0, 0, time.UTC) // Sunday 8 March 2026
	for _, c := range []struct {
		name string
		expr string
		want bool
	}{
		{"all five", "30 6 8 3 0", true},
		{"weekday 7 is Sunday too", "30 6 * 3 7", true},
		{"month is one based", "30 6 8 2 *", false},
		{"hour is 24 hour", "30 18 8 3 *", false},
		{"day of month alone", "30 6 9 3 *", false},
		{"either day field matches when both are restricted", "30 6 9 3 0", true},
		{"neither day field matches", "30 6 9 3 1", false},
		{"day of week alone", "30 6 * * 1-5", false},
		{"stepped day of month counts as unrestricted", "30 6 */1 3 1", false},
	} {
		s, err := parseCron(c.expr)
		if err != nil {
			t.Fatalf("%s: parseCron(%q): %v", c.name, c.expr, err)
		}
		if got := matchesAt(s, sunday); got != c.want {
			t.Errorf("%s: %q matches Sunday 06:30 = %v, want %v", c.name, c.expr, got, c.want)
		}
	}
	for _, expr := range []string{"0 24 * * *", "0 0 32 * *", "0 0 1 13 *", "0 0 1 0 *", "0 0 * * 8"} {
		if _, err := parseCron(expr); !errors.Is(err, ErrInvalidCronExpression) {
			t.Errorf("parseCron(%q) = %v, want an out of range error", expr, err)
		}
	}
}

// TestCron_NoMatchIsAnError: an expression that never matches (30 February) is an
// error matching ErrCronNoMatch and is never fired; one that matches only on
// 29 February is found years ahead.
func TestCron_NoMatchIsAnError(t *testing.T) {
	now := time.Date(2026, 3, 4, 10, 0, 0, 0, time.UTC)
	_, err := nextCronWait("0 0 30 2 *", now)
	if !errors.Is(err, ErrCronNoMatch) || !errors.Is(err, ErrInvalidCronExpression) {
		t.Fatalf("nextCronWait(30 February) = %v, want ErrCronNoMatch", err)
	}
	got, err := nextCronWait("0 0 29 2 *", now)
	if err != nil {
		t.Fatal(err)
	}
	if want := time.Date(2028, 2, 29, 0, 0, 0, 0, time.UTC).Sub(now); got != want {
		t.Errorf("29 February: wait %v, want %v", got, want)
	}
}

// TestCron_DSTFallBackDivergence documents the one place the locked Add(time.Minute)
// step differs from the recorded fixture. The recording stepped wall clock minutes;
// nextCronWait steps absolute minutes. In America/New_York on the
// fall-back day, 2026-11-01, the clock goes from 02:00 EDT back to 01:00 EST. From
// 01:59:30 EDT, the expression "30 1 * * *":
//
//   - a wall clock step skips the repeated hour (its next wall clock minute after
//     01:59 is 02:00 EST) and waits 88230 s, until 01:30 EST of the next day (recorded
//     under TZ=America/New_York);
//   - nextCronWait steps one absolute minute to 01:00 EST, finds 01:30 EST in the
//     repeated hour and waits 1830 s.
//
// The test asserts the absolute-minute value, and the recorded value as the documented
// one. The spring forward side agrees with the recording: a nonexistent hour is
// skipped by both.
func TestCron_DSTFallBackDivergence(t *testing.T) {
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatalf("LoadLocation: %v", err)
	}

	const tsWaitSeconds = 88230 // wall clock step, recorded under TZ=America/New_York
	now := time.Date(2026, 11, 1, 1, 59, 30, 0, ny)
	if _, off := now.Zone(); off != -4*3600 {
		t.Fatalf("the start instant is at offset %d s, want EDT (-14400)", off)
	}
	got, err := nextCronWait("30 1 * * *", now)
	if err != nil {
		t.Fatalf("nextCronWait: %v", err)
	}
	if want := 1830 * time.Second; got != want {
		t.Errorf("fall-back: wait = %v, want %v (Add step)", got, want)
	}
	if got == tsWaitSeconds*time.Second {
		t.Errorf("fall-back: wait equals the TS value %d s, so the step no longer diverges from the recorded corpus", tsWaitSeconds)
	}

	// Spring forward, 2026-03-08: 02:00 EST jumps to 03:00 EDT. From 01:59:30 EST the
	// expression "30 2 * * *" matches no real minute that day (02:30 does not exist), so
	// both steps wait for 02:30 of the next day: 84630 s, recorded under
	// TZ=America/New_York.
	spring := time.Date(2026, 3, 8, 1, 59, 30, 0, ny)
	got, err = nextCronWait("30 2 * * *", spring)
	if err != nil {
		t.Fatalf("nextCronWait spring: %v", err)
	}
	if want := 84630 * time.Second; got != want {
		t.Errorf("spring forward: wait = %v, want %v (agrees with TS)", got, want)
	}
}
