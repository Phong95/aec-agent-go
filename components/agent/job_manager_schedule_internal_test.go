package agent

import (
	"testing"
	"time"

	"github.com/Phong95/aec-agent-go/components/agent/job"
)

// An every job keeps its cadence from the scheduled time, but a fire that is late (the
// host was busy or down) never schedules into the past: no burst of runs after a pause.
func TestNextEveryKeepsCadenceWithoutBursts(t *testing.T) {
	scheduled := "2026-10-06T10:00:00.000Z"
	w := job.When{Kind: job.KindEvery, Ms: 600_000}
	c := newJobClock(jobClockDeps{})
	base, _ := parseISO(scheduled)
	nextEvery := func(_ job.When, now time.Time) time.Time {
		next, _, _ := c.nextAfterFire(w, scheduled, now)
		return next
	}

	onTime := nextEvery(w, base.Add(2*time.Second))
	if !onTime.Equal(base.Add(10 * time.Minute)) {
		t.Fatalf("on time: next = %s, want the cadence from the scheduled time", onTime)
	}
	late := base.Add(35 * time.Minute)
	if got := nextEvery(w, late); !got.Equal(late.Add(10 * time.Minute)) {
		t.Fatalf("late: next = %s, want now + interval", got)
	}
}
