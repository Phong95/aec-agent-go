package job

import (
	"bytes"
	"slices"

	"github.com/Phong95/aec-agent-go/components/agent/internal/jobkit"
)

// Clone returns a deep copy: the filter's lists and condition values and the token
// budget are copied, so a caller and a store never share a mutable part of a trigger.
// Add a line here when a pointer, slice or map field is added to Trigger; the clonecheck
// test fails until you do.
// Update (A-126): of a Job - and the hourly limit and NextRunAt.
func (j Job) Clone() Job {
	j.Filter = j.Filter.Clone()
	j.Limits = j.Limits.Clone()
	if j.NextRunAt != nil {
		v := *j.NextRunAt
		j.NextRunAt = &v
	}
	// Update (A-70 agent tools): and the delivery.
	// Update (A-127): a list of them.
	j.Deliveries = slices.Clone(j.Deliveries)
	// Update (A-103): and the action with its arguments.
	if j.Action != nil {
		a := j.Action.Clone()
		j.Action = &a
	}
	return j
}

// Clone returns a deep copy of the action: its arguments are copied (A-103).
func (a Action) Clone() Action {
	if a.Args != nil {
		args, _ := jobkit.CloneValue(a.Args).(map[string]any)
		a.Args = args
	}
	return a
}

// Clone returns a deep copy of the limits: the optional numbers are copied.
func (l Limits) Clone() Limits {
	if l.MaxRunsPerHour != nil {
		v := *l.MaxRunsPerHour
		l.MaxRunsPerHour = &v
	}
	if l.MaxTokensPerDay != nil {
		v := *l.MaxTokensPerDay
		l.MaxTokensPerDay = &v
	}
	// A-103.
	if l.MaxRunMs != nil {
		v := *l.MaxRunMs
		l.MaxRunMs = &v
	}
	if l.PauseAfterFailures != nil {
		v := *l.PauseAfterFailures
		l.PauseAfterFailures = &v
	}
	return l
}

// Clone returns a deep copy of the filter.
func (f Filter) Clone() Filter {
	if f.Types != nil {
		f.Types = slices.Clone(f.Types)
	}
	if f.Subjects != nil {
		f.Subjects = slices.Clone(f.Subjects)
	}
	if f.Where != nil {
		where := make([]Condition, len(f.Where))
		for i, c := range f.Where {
			c.Value = jobkit.CloneValue(c.Value)
			where[i] = c
		}
		f.Where = where
	}
	return f
}

// Clone returns a deep copy: Revision and Data are copied. Add a line here when a
// pointer, slice or map field is added to Event; the clonecheck test fails until you do.
func (e Event) Clone() Event {
	if e.Revision != nil {
		v := *e.Revision
		e.Revision = &v
	}
	if e.Data != nil {
		e.Data = bytes.Clone(e.Data)
	}
	return e
}

// Clone returns a deep copy: EventIDs, Revision and Usage are copied. Add a line here
// when a pointer, slice or map field is added to Run; the clonecheck test fails until
// you do.
func (r Run) Clone() Run {
	if r.EventIDs != nil {
		r.EventIDs = slices.Clone(r.EventIDs)
	}
	if r.Revision != nil {
		v := *r.Revision
		r.Revision = &v
	}
	if r.Usage != nil {
		u := *r.Usage
		r.Usage = &u
	}
	return r
}
