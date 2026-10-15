package job

import "strconv"

// Describe returns a short English description of a schedule, for logs and tool
// replies: `cron "0 8 * * 1" (Asia/Ho_Chi_Minh)`, "every 2 hours", "once at <time>".
// It is the describeSchedule of core/ts. Update (A-126): a When that is not a schedule
// is its kind.
func Describe(w When) string {
	switch w.Kind {
	case KindCron:
		out := `cron "` + w.Expr + `"`
		if w.TZ != "" {
			out += " (" + w.TZ + ")"
		}
		return out
	case KindEvery:
		return "every " + formatDuration(w.Ms)
	case KindAt:
		return "once at " + w.At
	default:
		return string(w.Kind)
	}
}

// formatDuration names the largest whole unit of ms: days, hours, minutes, else seconds.
func formatDuration(ms int64) string {
	units := []struct {
		size int64
		name string
	}{{86_400_000, "day"}, {3_600_000, "hour"}, {60_000, "minute"}}
	for _, u := range units {
		if ms%u.size == 0 {
			n := ms / u.size
			if n == 1 {
				return "1 " + u.name
			}
			return strconv.FormatInt(n, 10) + " " + u.name + "s"
		}
	}
	return strconv.FormatInt((ms+500)/1000, 10) + " seconds"
}
