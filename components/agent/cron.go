package agent

import (
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/Phong95/aec-agent-go/internal/jsstr"
)

// cronSearchDays is how far ahead nextCronWait looks for the next matching minute: a
// little over four years, so a schedule that matches only on 29 February is found from
// any start. An expression with no match in that window (for example "0 0 31 2 *")
// never matches at all.
//
// An expression that never matches does not fire at all: Start returns ErrCronNoMatch
// instead.
const cronSearchDays = 4*366 + 1

// ErrInvalidCronExpression is matched, with errors.Is, by the error Start returns for
// a cron expression that does not have 5 fields.
// Update (A-126): moved here from scheduled_tasks.go (scheduled tasks removed); the
// job clock reports it for a schedule job's cron expression.
var ErrInvalidCronExpression = errors.New("agent: invalid cron expression")

// ErrCronNoMatch is matched, with errors.Is, by the error Start returns for a cron
// expression that is valid but matches no minute within the search window (about four
// years), for example "0 0 30 2 *". It also matches ErrInvalidCronExpression. Such a
// job never fires.
var ErrCronNoMatch = errors.New("agent: cron expression matches no time")

// cronExprError is the error of a cron expression that does not have 5 fields. Its
// text is capitalised and carries an em dash, so a plain errors.New literal could not
// be used (staticcheck ST1005, as with the other typed texts of this package); the
// source stays ASCII, so the dash is an escape.
type cronExprError struct{ expr string }

func (e *cronExprError) Error() string {
	return "Invalid cron expression \"" + e.expr + "\" — expected 5 fields (minute hour dom month dow)"
}

// Unwrap makes errors.Is(err, ErrInvalidCronExpression) true.
func (*cronExprError) Unwrap() error { return ErrInvalidCronExpression }

// cronFieldError is the error of a cron expression with 5 fields of which one cannot
// be read. It matches ErrInvalidCronExpression.
type cronFieldError struct {
	expr, field, reason string
}

func (e *cronFieldError) Error() string {
	return "agent: invalid cron expression " + strconv.Quote(e.expr) + ": " + e.field + " field: " + e.reason
}

func (*cronFieldError) Unwrap() error { return ErrInvalidCronExpression }

// cronNoMatchError is the error of an expression that matches no time; it matches both
// ErrCronNoMatch and ErrInvalidCronExpression.
type cronNoMatchError struct{ expr string }

func (e *cronNoMatchError) Error() string {
	return "agent: cron expression " + strconv.Quote(e.expr) + " matches no time within four years"
}

func (*cronNoMatchError) Unwrap() []error { return []error{ErrCronNoMatch, ErrInvalidCronExpression} }

// cronField is the set of values one field allows, as a bit set (bit v is value v),
// and whether the field was "*" (needed for the day-of-month / day-of-week rule).
type cronField struct {
	bits uint64
	star bool
}

func (f cronField) has(v int) bool { return f.bits&(1<<uint(v)) != 0 }

// cronSchedule is a parsed 5-field cron expression.
type cronSchedule struct {
	minute, hour, dom, month, dow cronField
}

// cronBounds are the value ranges of the five fields. The day of week accepts 0 to 7,
// where both 0 and 7 are Sunday.
var cronBounds = [5]struct {
	name     string
	min, max int
}{
	{"minute", 0, 59},
	{"hour", 0, 23},
	{"day-of-month", 1, 31},
	{"month", 1, 12},
	{"day-of-week", 0, 7},
}

// parseCron reads a standard 5-field cron expression (minute hour dom month dow). The
// expression is trimmed and split on JS whitespace; a count other than 5 is a
// *cronExprError. Each field is a comma list of items, and an item is
// "*", a value ("5"), a range ("1-5") or either of them with a step ("*/15", "1-10/2",
// "5/10", which means 5 to the field's maximum in steps of 10). Values are decimal
// digits within the field's range; anything else is a *cronFieldError. Months and days
// of the week are numbers only (no JAN or MON names).
//
// Every item is read strictly: "*/5" is every fifth value, "1-5" is the range 1 to 5,
// and "5x" is an error.
func parseCron(expr string) (cronSchedule, error) {
	parts := jsstr.Fields(jsstr.Trim(expr))
	if len(parts) != 5 {
		return cronSchedule{}, &cronExprError{expr: expr}
	}
	var fields [5]cronField
	for i, part := range parts {
		b := cronBounds[i]
		f, reason := parseCronField(part, b.min, b.max)
		if reason != "" {
			return cronSchedule{}, &cronFieldError{expr: expr, field: b.name, reason: reason}
		}
		fields[i] = f
	}
	// 7 is Sunday too.
	if fields[4].has(7) {
		fields[4].bits |= 1
		fields[4].bits &^= 1 << 7
	}
	return cronSchedule{minute: fields[0], hour: fields[1], dom: fields[2], month: fields[3], dow: fields[4]}, nil
}

// parseCronField reads one field. It returns the reason when the field is invalid.
func parseCronField(field string, lo, hi int) (cronField, string) {
	var f cronField
	// A field that starts with "*" ("*" or "*/n") counts as unrestricted for the day
	// rule, as in Vixie cron.
	f.star = strings.HasPrefix(field, "*")
	for item := range strings.SplitSeq(field, ",") {
		if item == "" {
			return cronField{}, "empty list item"
		}
		rng, stepText, hasStep := strings.Cut(item, "/")
		step := 1
		if hasStep {
			n, ok := cronNumber(stepText)
			if !ok || n < 1 {
				return cronField{}, "invalid step " + strconv.Quote(stepText)
			}
			step = n
		}
		var from, to int
		switch {
		case rng == "*":
			from, to = lo, hi
		case strings.Contains(rng, "-"):
			a, b, _ := strings.Cut(rng, "-")
			x, okA := cronNumber(a)
			y, okB := cronNumber(b)
			if !okA || !okB {
				return cronField{}, "invalid range " + strconv.Quote(rng)
			}
			if x > y {
				return cronField{}, "range " + strconv.Quote(rng) + " is reversed"
			}
			from, to = x, y
		default:
			x, ok := cronNumber(rng)
			if !ok {
				return cronField{}, "invalid value " + strconv.Quote(rng)
			}
			from, to = x, x
			if hasStep {
				to = hi // "5/10" is 5 to the maximum in steps of 10
			}
		}
		if from < lo || to > hi {
			return cronField{}, strconv.Quote(rng) + " is out of range " + strconv.Itoa(lo) + "-" + strconv.Itoa(hi)
		}
		for v := from; v <= to; v += step {
			f.bits |= 1 << uint(v)
		}
	}
	return f, ""
}

// cronNumber reads a non-empty run of ASCII digits (no sign, no spaces, at most 2
// digits, which covers every field).
func cronNumber(s string) (int, bool) {
	if s == "" || len(s) > 2 {
		return 0, false
	}
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, false
		}
		n = n*10 + int(c-'0')
	}
	return n, true
}

// dayMatches is the standard cron day rule: when both the day of month and the day of
// week are restricted (neither is "*"), a day matches when EITHER does; otherwise both
// must (an unrestricted field matches every day).
func (s cronSchedule) dayMatches(t time.Time) bool {
	dom := s.dom.has(t.Day())
	dow := s.dow.has(int(t.Weekday()))
	if !s.dom.star && !s.dow.star {
		return dom || dow
	}
	return dom && dow
}

// next returns the first minute after now that the schedule matches, read in the
// location of now, or false when there is none within cronSearchDays.
//
// The first candidate is now with its seconds and nanoseconds taken off, plus one
// minute. A month, day or hour that does not match is skipped as a whole (to the first
// minute of the next one, built with time.Date in the location); a minute that does not
// match is stepped with Add(time.Minute), absolute minutes. Across a DST fall-back the
// minute step therefore visits the repeated hour; a wall clock time that
// does not exist in a spring-forward gap is never a candidate.
func (s cronSchedule) next(now time.Time) (time.Time, bool) {
	loc := now.Location()
	// Truncating to the minute keeps the instant's minute: only the seconds and the
	// nanoseconds of the wall clock are taken off, so an instant in the repeated hour
	// of a DST fall-back stays in the same occurrence of that hour.
	t := now.Add(-time.Duration(now.Second())*time.Second - time.Duration(now.Nanosecond())).Add(time.Minute)
	limit := now.AddDate(0, 0, cronSearchDays)
	for !t.After(limit) {
		switch {
		case !s.month.has(int(t.Month())):
			t = time.Date(t.Year(), t.Month()+1, 1, 0, 0, 0, 0, loc)
		case !s.dayMatches(t):
			t = time.Date(t.Year(), t.Month(), t.Day()+1, 0, 0, 0, 0, loc)
		case !s.hour.has(t.Hour()):
			t = t.Add(time.Duration(60-t.Minute()) * time.Minute)
		case !s.minute.has(t.Minute()):
			t = t.Add(time.Minute)
		default:
			return t, true
		}
	}
	return time.Time{}, false
}

// nextCronWait is how long to wait from now until a job with the expression expr fires
// next, read in the location of now. An expression that cannot be read is an error
// that matches ErrInvalidCronExpression (a *cronExprError when the field count is
// wrong); one that matches no time within the search window is an error that
// matches ErrCronNoMatch.
func nextCronWait(expr string, now time.Time) (time.Duration, error) {
	s, err := parseCron(expr)
	if err != nil {
		return 0, err
	}
	t, ok := s.next(now)
	if !ok {
		return 0, &cronNoMatchError{expr: expr}
	}
	return t.Sub(now), nil
}
