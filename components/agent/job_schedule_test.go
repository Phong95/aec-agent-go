package agent_test

import (
	"errors"
	"testing"
	"time"

	"github.com/Phong95/aec-agent-go/components/agent"
	"github.com/Phong95/aec-agent-go/components/agent/job"
)

// A-69 schedule math: the same cases as the core/ts computeNextRun / validateSchedule tests.

func mustTime(t *testing.T, s string) time.Time {
	t.Helper()
	v, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func nextCron(t *testing.T, expr, after, tz string) string {
	t.Helper()
	got, ok, err := agent.NextRun(job.When{Kind: job.KindCron, Expr: expr, TZ: tz}, mustTime(t, after), time.UTC)
	if err != nil || !ok {
		t.Fatalf("NextRun(%q after %s) = %v %v", expr, after, ok, err)
	}
	return got.UTC().Format(time.RFC3339)
}

func TestNextRunStepsAndRanges(t *testing.T) {
	cases := []struct{ expr, after, want string }{
		{"*/15 9-17 * * 1-5", "2026-10-06T09:07:00Z", "2026-10-06T09:15:00Z"},
		{"*/15 9-17 * * 1-5", "2026-10-06T17:50:00Z", "2026-10-07T09:00:00Z"},
		// Friday evening -> Monday morning
		{"0 9 * * 1-5", "2026-10-09T18:00:00Z", "2026-10-12T09:00:00Z"},
	}
	for _, c := range cases {
		if got := nextCron(t, c.expr, c.after, "UTC"); got != c.want {
			t.Errorf("%s after %s = %s, want %s", c.expr, c.after, got, c.want)
		}
	}
}

func TestNextRunTimezone(t *testing.T) {
	// 08:00 in Ho Chi Minh City (UTC+7) is 01:00 UTC.
	if got := nextCron(t, "0 8 * * *", "2026-10-06T00:00:00Z", "Asia/Ho_Chi_Minh"); got != "2026-10-06T01:00:00Z" {
		t.Errorf("got %s", got)
	}
	if got := nextCron(t, "0 8 * * *", "2026-10-06T02:00:00Z", "Asia/Ho_Chi_Minh"); got != "2026-10-07T01:00:00Z" {
		t.Errorf("got %s", got)
	}
}

func TestNextRunMonthlyAndDayOr(t *testing.T) {
	if got := nextCron(t, "0 8 1 * *", "2026-10-06T00:00:00Z", "UTC"); got != "2026-11-01T08:00:00Z" {
		t.Errorf("monthly = %s", got)
	}
	if got := nextCron(t, "0 8 1 * *", "2026-11-01T08:00:00Z", "UTC"); got != "2026-12-01T08:00:00Z" {
		t.Errorf("monthly after a run = %s", got)
	}
	// the 13th OR any Friday: Fri 9 Oct comes before Tue 13 Oct
	if got := nextCron(t, "0 8 13 * 5", "2026-10-06T00:00:00Z", "UTC"); got != "2026-10-09T08:00:00Z" {
		t.Errorf("dom OR dow = %s", got)
	}
}

func TestNextRunEveryAndAt(t *testing.T) {
	after := mustTime(t, "2026-10-06T00:00:00Z")
	got, ok, err := agent.NextRun(job.When{Kind: job.KindEvery, Ms: 3_600_000}, after, nil)
	if err != nil || !ok || !got.Equal(mustTime(t, "2026-10-06T01:00:00Z")) {
		t.Fatalf("every = %v %v %v", got, ok, err)
	}
	once := job.When{Kind: job.KindAt, At: "2026-10-07T09:00:00.000Z"}
	got, ok, err = agent.NextRun(once, after, nil)
	if err != nil || !ok || !got.Equal(mustTime(t, "2026-10-07T09:00:00Z")) {
		t.Fatalf("at = %v %v %v", got, ok, err)
	}
	if _, ok, err := agent.NextRun(once, mustTime(t, "2026-10-07T09:00:00Z"), nil); ok || err != nil {
		t.Fatalf("a passed at time must not fire again: %v %v", ok, err)
	}
}

func TestValidateScheduleRejectsBadInput(t *testing.T) {
	now := mustTime(t, "2026-10-06T00:00:00Z")
	bad := []job.When{
		{Kind: "hourly"},
		{Kind: job.KindCron},
		{Kind: job.KindCron, Expr: "* * * * * *"}, // seconds are not supported
		{Kind: job.KindCron, Expr: "61 * * * *"},
		{Kind: job.KindCron, Expr: "0 0 30 2 *"}, // never fires
		{Kind: job.KindCron, Expr: "0 8 * * *", TZ: "Mars/Olympus"},
		{Kind: job.KindEvery, Ms: 1_000},
		{Kind: job.KindAt, At: "not a time"},
		{Kind: job.KindAt, At: "2001-01-01T00:00:00Z"},
	}
	for _, s := range bad {
		_, err := agent.ValidateSchedule(s, now)
		var je *job.Error
		if !errors.As(err, &je) || je.Code != job.CodeInvalidArgument {
			t.Errorf("ValidateSchedule(%+v) = %v, want an invalid_argument error", s, err)
		}
	}
}

func TestValidateScheduleNormalizes(t *testing.T) {
	now := mustTime(t, "2026-10-06T00:00:00Z")
	cases := []struct{ in, want job.When }{
		{job.When{Kind: job.KindCron, Expr: "  0   8 * *  1 ", TZ: " UTC "}, job.When{Kind: job.KindCron, Expr: "0 8 * * 1", TZ: "UTC"}},
		{job.When{Kind: job.KindEvery, Ms: 90_000}, job.When{Kind: job.KindEvery, Ms: 90_000}},
		{job.When{Kind: job.KindAt, At: "2099-01-01T09:00:00+07:00"}, job.When{Kind: job.KindAt, At: "2099-01-01T02:00:00.000Z"}},
	}
	for _, c := range cases {
		got, err := agent.ValidateSchedule(c.in, now)
		if err != nil || got != c.want {
			t.Errorf("ValidateSchedule(%+v) = %+v %v, want %+v", c.in, got, err, c.want)
		}
	}
}
