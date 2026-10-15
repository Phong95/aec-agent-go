package agent

import (
	"strconv"
	"strings"
	"time"

	"github.com/Phong95/aec-agent-go/components/agent/job"
	"github.com/Phong95/aec-agent-go/internal/jsstr"
)

// ---------------------------------------------------------------------------
// Schedule math of prompt jobs (A-69): validation and next run times. Cron
// expressions are read by the same parser as the code jobs (cron.go): ranges,
// steps and lists, the day of week 0 to 7, and the classic day-of-month OR
// day-of-week rule. Unlike core/ts (croner), month and weekday names are not
// accepted.
// Update (A-126): over job.When; used by the job clock (job_clock.go).
// ---------------------------------------------------------------------------

// isoLayout is the shape of JS toISOString, the timestamp shape of every job and
// run record.
const isoLayout = "2006-01-02T15:04:05.000Z"

// isoTime formats t like JS toISOString.
func isoTime(t time.Time) string { return t.UTC().Format(isoLayout) }

// parseISO reads an ISO 8601 time with a zone (the shapes JS Date.parse accepts for
// a full timestamp: "Z" or an offset, with or without fractional seconds).
func parseISO(s string) (time.Time, bool) {
	t, err := time.Parse(time.RFC3339Nano, s)
	return t, err == nil
}

// scheduleError is an invalid_argument *job.Error.
func scheduleError(message string) *job.Error {
	return job.NewError(message, job.CodeInvalidArgument, "")
}

// ValidateSchedule checks a schedule from untrusted input and returns it in normalized
// form (a cron expression with single spaces, an at time as UTC toISOString). now is
// the current time, against which an at time must be in the future. Every failure is a
// *job.Error with CodeInvalidArgument: an unknown kind, a cron expression that
// does not parse or never fires, an unknown timezone, an interval below
// job.MinIntervalMs, or an at time that is not a valid ISO time or is not in the
// future.
func ValidateSchedule(s job.When, now time.Time) (job.When, error) {
	switch s.Kind {
	case job.KindCron:
		expr := strings.Join(jsstr.Fields(jsstr.Trim(s.Expr)), " ")
		if expr == "" {
			return job.When{}, scheduleError(`cron schedule needs "expr", e.g. "0 8 * * 1" (Mondays 08:00)`)
		}
		tz := jsstr.Trim(s.TZ)
		loc := time.UTC
		if tz != "" {
			l, err := time.LoadLocation(tz)
			if err != nil {
				return job.When{}, scheduleError(`unknown timezone "` + tz + `" ` + emDash + ` use an IANA name such as "Asia/Ho_Chi_Minh"`)
			}
			loc = l
		}
		cs, err := parseCron(expr)
		if err != nil {
			return job.When{}, scheduleError(`invalid cron expression "` + expr + `": ` + err.Error())
		}
		// Seconds are not supported (5 fields), so runs are at least a minute apart; check
		// that it fires at all.
		if _, ok := cs.next(now.In(loc)); !ok {
			return job.When{}, scheduleError(`cron expression "` + expr + `" never fires`)
		}
		return job.When{Kind: job.KindCron, Expr: expr, TZ: tz}, nil
	case job.KindEvery:
		if s.Ms < job.MinIntervalMs {
			return job.When{}, scheduleError("interval must be at least " + strconv.Itoa(job.MinIntervalMs/1000) + " seconds")
		}
		return job.When{Kind: job.KindEvery, Ms: s.Ms}, nil
	case job.KindAt:
		at := jsstr.Trim(s.At)
		t, ok := parseISO(at)
		if !ok {
			return job.When{}, scheduleError(`"at" schedule needs "at" as an ISO 8601 time, e.g. "2026-10-07T09:00:00Z"`)
		}
		if !t.After(now) {
			return job.When{}, scheduleError(`"at" time ` + at + " is in the past")
		}
		return job.When{Kind: job.KindAt, At: isoTime(t)}, nil
	default:
		return job.When{}, scheduleError(`schedule.kind must be "cron", "every" or "at"`)
	}
}

// NextRun returns the first run time strictly after `after`, and false when the
// schedule never fires again (an at time that has passed, a cron expression with no
// further match). loc is the location of a cron schedule that names no TZ (nil means
// time.Local). An expression or timezone that cannot be read is an error.
func NextRun(s job.When, after time.Time, loc *time.Location) (time.Time, bool, error) {
	switch s.Kind {
	case job.KindCron:
		if s.TZ != "" {
			l, err := time.LoadLocation(s.TZ)
			if err != nil {
				return time.Time{}, false, scheduleError(`unknown timezone "` + s.TZ + `"`)
			}
			loc = l
		}
		if loc == nil {
			loc = time.Local
		}
		cs, err := parseCron(s.Expr)
		if err != nil {
			return time.Time{}, false, scheduleError(`invalid cron expression "` + s.Expr + `": ` + err.Error())
		}
		t, ok := cs.next(after.In(loc))
		return t, ok, nil
	case job.KindEvery:
		if s.Ms <= 0 {
			return time.Time{}, false, scheduleError("interval must be positive")
		}
		return after.Add(time.Duration(s.Ms) * time.Millisecond), true, nil
	case job.KindAt:
		t, ok := parseISO(s.At)
		if !ok {
			return time.Time{}, false, scheduleError(`invalid "at" time "` + s.At + `"`)
		}
		if !t.After(after) {
			return time.Time{}, false, nil
		}
		return t, true, nil
	default:
		return time.Time{}, false, scheduleError(`unknown schedule kind "` + string(s.Kind) + `"`)
	}
}
