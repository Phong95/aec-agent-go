package persistence

import (
	"cmp"
	"context"
	"slices"
	"sync"

	"github.com/Phong95/aec-agent-go/components/agent/job"
	"github.com/Phong95/aec-agent-go/kernel/ordered"
)

// MemoryJobStore is the default in-memory JobStore, the zero-config default for
// A-70. Triggers, the inbox and the run queue live in process memory and are lost on
// restart; a durable store (Postgres in the servers) is what makes the inbox durable.
// Every method runs under one lock, so each is atomic within the process. It deep-copies
// records on the way in and out and is safe for concurrent use.
// Update (A-126): the in-memory JobStore - jobs of every kind, their inbox and runs.
type MemoryJobStore struct {
	mu     sync.Mutex
	jobs   ordered.Map[job.Job]
	events ordered.Map[job.Event]
	runs   ordered.Map[job.Run]
}

// NewMemoryJobStore returns an empty store.
func NewMemoryJobStore() *MemoryJobStore { return &MemoryJobStore{} }

var _ JobStore = (*MemoryJobStore)(nil)

// -- Triggers --

// Save upserts a copy of the job.
func (s *MemoryJobStore) Save(_ context.Context, t job.Job) error {
	cp := t.Clone()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.jobs.Set(cp.ID, cp)
	return nil
}

// Get returns a copy of the trigger, or nil and a nil error for an unknown id.
func (s *MemoryJobStore) Get(_ context.Context, id string) (*job.Job, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.jobs.Get(id)
	if !ok {
		return nil, nil //nolint:nilnil // nil, nil means an unknown trigger
	}
	cp := t.Clone()
	return &cp, nil
}

// FindByName returns a copy of the agent's trigger with that name, or nil.
func (s *MemoryJobStore) FindByName(_ context.Context, agentID, name string) (*job.Job, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, t := range s.jobs.All() {
		if t.AgentID == agentID && t.Name == name {
			cp := t.Clone()
			return &cp, nil
		}
	}
	return nil, nil //nolint:nilnil // nil, nil means no such trigger
}

// ListByAgent returns copies of the agent's triggers, oldest created first (ties by id).
func (s *MemoryJobStore) ListByAgent(_ context.Context, agentID string) ([]job.Job, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []job.Job{}
	for _, t := range s.jobs.All() {
		if t.AgentID == agentID {
			out = append(out, t.Clone())
		}
	}
	slices.SortStableFunc(out, func(a, b job.Job) int {
		return cmp.Or(cmp.Compare(a.CreatedAt, b.CreatedAt), cmp.Compare(a.ID, b.ID))
	})
	return out, nil
}

// Delete removes the trigger with its events and runs; an unknown id is a no-op.
func (s *MemoryJobStore) Delete(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.deleteLocked(id)
	return nil
}

func (s *MemoryJobStore) deleteLocked(id string) {
	s.jobs.Delete(id)
	for _, eid := range s.events.Keys() {
		if e, ok := s.events.Get(eid); ok && e.JobID == id {
			s.events.Delete(eid)
		}
	}
	for _, rid := range s.runs.Keys() {
		if r, ok := s.runs.Get(rid); ok && r.JobID == id {
			s.runs.Delete(rid)
		}
	}
}

// UpdateJobState applies a run-state patch to the job (see JobStore).
func (s *MemoryJobStore) UpdateJobState(_ context.Context, id string, p JobStatePatch) (*job.Job, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	j, ok := s.jobs.Get(id)
	if !ok {
		return nil, nil //nolint:nilnil // nil, nil means an unknown job
	}
	next := j.Clone()
	switch {
	case p.ClearNextRunAt:
		next.NextRunAt = nil
	case p.NextRunAt != nil:
		v := *p.NextRunAt
		next.NextRunAt = &v
	}
	if p.Enabled != nil {
		next.Enabled = *p.Enabled
	}
	if p.LastRunAt != nil {
		next.LastRunAt = *p.LastRunAt
	}
	if p.LastStatus != nil {
		next.LastStatus = *p.LastStatus
	}
	switch {
	case p.ClearLastError:
		next.LastError = ""
	case p.LastError != nil:
		next.LastError = *p.LastError
	}
	if p.SessionID != nil {
		next.SessionID = *p.SessionID
	}
	s.jobs.Set(id, next.Clone())
	return &next, nil
}

// DeleteByAgent removes every trigger of the agent with its events and runs.
func (s *MemoryJobStore) DeleteByAgent(_ context.Context, agentID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, id := range s.jobs.Keys() {
		if t, ok := s.jobs.Get(id); ok && t.AgentID == agentID {
			s.deleteLocked(id)
		}
	}
	for _, eid := range s.events.Keys() {
		if e, ok := s.events.Get(eid); ok && e.AgentID == agentID {
			s.events.Delete(eid)
		}
	}
	for _, rid := range s.runs.Keys() {
		if r, ok := s.runs.Get(rid); ok && r.AgentID == agentID {
			s.runs.Delete(rid)
		}
	}
	return nil
}

// -- Inbox and queue --

// Accept dedupes the event, stores it and joins or opens its run (see JobStore).
func (s *MemoryJobStore) Accept(_ context.Context, in AcceptJobEventInput) (job.AcceptResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	incoming := in.Event
	for _, e := range s.events.All() {
		if e.JobID == incoming.JobID && e.Source == incoming.Source && e.SourceEventID == incoming.SourceEventID {
			return job.AcceptResult{Duplicate: true, Event: e.Clone()}, nil
		}
	}
	event := incoming.Clone()
	var run *job.Run
	if event.Status == job.EventAccepted && in.Run != nil {
		plan := in.Run
		var open *job.Run
		for _, r := range s.runs.All() {
			if r.JobID == plan.NewRun.JobID && r.BucketKey == plan.NewRun.BucketKey && r.Status == job.RunPending && r.LeaseOwner == "" {
				cp := r.Clone()
				open = &cp
				break
			}
		}
		if open != nil {
			joined := *open
			joined.EventIDs = append(joined.EventIDs, event.ID)
			// A full batch closes: it runs at its NotBefore, and the next event opens a new run.
			if len(joined.EventIDs) >= plan.MaxBatch {
				joined.Status = job.RunWaiting
			}
			joined.NotBefore = min(plan.JoinNotBefore, open.Deadline)
			joined.Depth = max(open.Depth, event.Depth)
			joined.Revision = maxRevision(open.Revision, event.Revision)
			joined.UpdatedAt = plan.NewRun.UpdatedAt
			run = &joined
		} else {
			fresh := plan.NewRun.Clone()
			fresh.EventIDs = []string{event.ID}
			if plan.MaxBatch <= 1 {
				fresh.Status = job.RunWaiting
			}
			run = &fresh
		}
		s.runs.Set(run.ID, run.Clone())
		event.RunID = run.ID
	}
	s.events.Set(event.ID, event.Clone())
	return job.AcceptResult{Event: event, Run: run}, nil
}

// Claim leases the soonest due run of the agent (see JobStore).
func (s *MemoryJobStore) Claim(_ context.Context, in ClaimJobRunInput) (*job.Run, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var next *job.Run
	for _, r := range s.runs.All() {
		if r.AgentID != in.AgentID || slices.Contains(in.ExcludeJobIDs, r.JobID) {
			continue
		}
		due := ((r.Status == job.RunPending || r.Status == job.RunWaiting) && r.LeaseOwner == "" && r.NotBefore <= in.Now) ||
			(r.Status == job.RunRunning && r.LeaseUntil != "" && r.LeaseUntil < in.Now)
		if !due {
			continue
		}
		if next == nil || cmp.Or(cmp.Compare(r.NotBefore, next.NotBefore), cmp.Compare(r.CreatedAt, next.CreatedAt), cmp.Compare(r.ID, next.ID)) < 0 {
			cp := r
			next = &cp
		}
	}
	if next == nil {
		return nil, nil //nolint:nilnil // nil, nil means nothing is due
	}
	claimed := next.Clone()
	claimed.Status = job.RunRunning
	claimed.LeaseOwner = in.Owner
	claimed.LeaseUntil = in.LeaseUntil
	claimed.Attempts++
	claimed.UpdatedAt = in.Now
	s.runs.Set(claimed.ID, claimed.Clone())
	return &claimed, nil
}

// RenewLease extends the lease of a run owner still holds.
func (s *MemoryJobStore) RenewLease(_ context.Context, runID, owner, leaseUntil string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.runs.Get(runID)
	if !ok || r.LeaseOwner != owner || r.Status != job.RunRunning {
		return false, nil
	}
	r.LeaseUntil = leaseUntil
	s.runs.Set(runID, r)
	return true, nil
}

// UpdateRun applies a guarded patch (see JobStore).
func (s *MemoryJobStore) UpdateRun(_ context.Context, runID, owner string, patch JobRunPatch, now string) (*job.Run, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	stored, ok := s.runs.Get(runID)
	if !ok {
		return nil, nil //nolint:nilnil // nil, nil means unknown run
	}
	guard := stored.LeaseOwner == owner
	if owner == "" {
		guard = stored.LeaseOwner == "" && (stored.Status == job.RunPending || stored.Status == job.RunWaiting)
	}
	if !guard {
		return nil, nil //nolint:nilnil // nil, nil means the guard failed
	}
	next := stored.Clone()
	applyJobRunPatch(&next, patch)
	next.UpdatedAt = now
	s.runs.Set(runID, next.Clone())
	return &next, nil
}

// applyJobRunPatch sets the fields of patch on r.
func applyJobRunPatch(r *job.Run, p JobRunPatch) {
	setString := func(dst *string, v *string) {
		if v != nil {
			*dst = *v
		}
	}
	if p.Status != nil {
		r.Status = *p.Status
	}
	setString(&r.NotBefore, p.NotBefore)
	if p.Attempts != nil {
		r.Attempts = *p.Attempts
	}
	setString(&r.SessionID, p.SessionID)
	setString(&r.StartedAt, p.StartedAt)
	setString(&r.ResultAt, p.ResultAt)
	setString(&r.Output, p.Output)
	if p.Silent != nil {
		r.Silent = *p.Silent
	}
	if p.Action != nil {
		r.Action = *p.Action
	}
	if p.Usage != nil {
		u := *p.Usage
		r.Usage = &u
	}
	setString(&r.StopReason, p.StopReason)
	setString(&r.Error, p.Error)
	setString(&r.Reason, p.Reason)
	setString(&r.DeliveredAt, p.DeliveredAt)
	setString(&r.FinishedAt, p.FinishedAt)
	for _, f := range p.Clear {
		switch f {
		case JobRunError:
			r.Error = ""
		case JobRunReason:
			r.Reason = ""
		}
	}
	if p.ReleaseLease {
		r.LeaseOwner, r.LeaseUntil = "", ""
	}
}

// InsertRun adds a copy of the run (a replay).
func (s *MemoryJobStore) InsertRun(_ context.Context, r job.Run) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.runs.Set(r.ID, r.Clone())
	return nil
}

// -- History --

// GetRun returns a copy of the run, or nil.
func (s *MemoryJobStore) GetRun(_ context.Context, id string) (*job.Run, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.runs.Get(id)
	if !ok {
		return nil, nil //nolint:nilnil // nil, nil means unknown run
	}
	cp := r.Clone()
	return &cp, nil
}

// ListRuns returns copies of the trigger's runs, newest created first.
func (s *MemoryJobStore) ListRuns(_ context.Context, jobID string, limit int) ([]job.Run, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []job.Run{}
	for _, r := range s.runs.All() {
		if r.JobID == jobID {
			out = append(out, r.Clone())
		}
	}
	slices.SortStableFunc(out, newestRunFirst)
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// HasActiveRun says whether the job has a run queued or running.
func (s *MemoryJobStore) HasActiveRun(_ context.Context, jobID string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, r := range s.runs.All() {
		if r.JobID == jobID && !isFinishedRun(r) {
			return true, nil
		}
	}
	return false, nil
}

// ListEvents returns copies of the trigger's events, newest received first.
func (s *MemoryJobStore) ListEvents(_ context.Context, jobID string, limit int) ([]job.Event, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := s.eventsOfLocked(jobID)
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	for i := range out {
		out[i] = out[i].Clone()
	}
	return out, nil
}

// eventsOfLocked returns the trigger's events (not copied), newest received first.
func (s *MemoryJobStore) eventsOfLocked(jobID string) []job.Event {
	out := []job.Event{}
	for _, e := range s.events.All() {
		if e.JobID == jobID {
			out = append(out, e)
		}
	}
	slices.SortStableFunc(out, func(a, b job.Event) int {
		return cmp.Or(cmp.Compare(b.ReceivedAt, a.ReceivedAt), cmp.Compare(b.ID, a.ID))
	})
	return out
}

// GetEvents returns copies of the events, in the order of ids.
func (s *MemoryJobStore) GetEvents(_ context.Context, ids []string) ([]job.Event, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []job.Event{}
	for _, id := range ids {
		if e, ok := s.events.Get(id); ok {
			out = append(out, e.Clone())
		}
	}
	return out, nil
}

// UsageSince counts the trigger's runs started at or after since, and their tokens.
func (s *MemoryJobStore) UsageSince(_ context.Context, jobID, since string) (JobUsage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var u JobUsage
	for _, r := range s.runs.All() {
		if r.JobID != jobID || r.StartedAt == "" || r.StartedAt < since {
			continue
		}
		u.Runs++
		if r.Usage != nil {
			u.Tokens += r.Usage.InputTokens + r.Usage.OutputTokens
		}
	}
	return u, nil
}

// LastSucceeded returns a copy of the newest succeeded run of the bucket, or nil.
func (s *MemoryJobStore) LastSucceeded(_ context.Context, jobID, bucketKey string) (*job.Run, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var found *job.Run
	for _, r := range s.runs.All() {
		if r.JobID != jobID || r.BucketKey != bucketKey || r.Status != job.RunSucceeded || r.FinishedAt == "" {
			continue
		}
		if found == nil || cmp.Or(cmp.Compare(r.FinishedAt, found.FinishedAt), cmp.Compare(r.ID, found.ID)) > 0 {
			cp := r
			found = &cp
		}
	}
	if found == nil {
		return nil, nil //nolint:nilnil // nil, nil means none
	}
	cp := found.Clone()
	return &cp, nil
}

// Prune keeps the newest keepEvents events and keepRuns finished runs of the trigger;
// queued and running runs, and the events they hold, are kept.
func (s *MemoryJobStore) Prune(_ context.Context, jobID string, keepEvents, keepRuns int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	var finished []job.Run
	held := map[string]bool{}
	for _, r := range s.runs.All() {
		if r.JobID != jobID {
			continue
		}
		if isFinishedRun(r) {
			finished = append(finished, r)
		} else {
			for _, id := range r.EventIDs {
				held[id] = true
			}
		}
	}
	slices.SortStableFunc(finished, newestRunFirst)
	for _, old := range finished[min(max(0, keepRuns), len(finished)):] {
		s.runs.Delete(old.ID)
	}
	events := s.eventsOfLocked(jobID)
	for _, old := range events[min(max(0, keepEvents), len(events)):] {
		if !held[old.ID] {
			s.events.Delete(old.ID)
		}
	}
	return nil
}

func isFinishedRun(r job.Run) bool {
	return r.Status != job.RunPending && r.Status != job.RunWaiting && r.Status != job.RunRunning
}

func newestRunFirst(a, b job.Run) int {
	return cmp.Or(cmp.Compare(b.CreatedAt, a.CreatedAt), cmp.Compare(b.ID, a.ID))
}

func maxRevision(a, b *float64) *float64 {
	switch {
	case a == nil && b == nil:
		return nil
	case a == nil:
		v := *b
		return &v
	case b == nil:
		v := *a
		return &v
	default:
		v := max(*a, *b)
		return &v
	}
}
