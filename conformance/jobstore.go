package conformance

import (
	"encoding/json"
	"reflect"
	"slices"
	"strconv"
	"sync"
	"testing"

	"github.com/Phong95/aec-agent-go/components/agent/job"
	"github.com/Phong95/aec-agent-go/components/persistence"
	"github.com/Phong95/aec-agent-go/conformance/internal/check"
	"github.com/Phong95/aec-agent-go/kernel"
)

// JobStoreFactory returns a persistence.JobStore. See the package comment for the
// freshness rules of the store suites.
type JobStoreFactory func(t *testing.T) persistence.JobStore

const (
	trigLater = "2026-10-10T00:02:00.000Z"
	trigLease = "2026-10-10T00:10:00.000Z"
)

// triggerFixture builds a trigger with unique ids (and agent and name), changed by mut.
func triggerFixture(mut func(*job.Job)) job.Job {
	t := job.Job{
		ID: "trigger-" + kernel.NewID(), AgentID: uniqueAgentID(), Name: "name-" + kernel.NewID(), Enabled: true, Version: 1,
		When: job.When{Kind: job.KindEvent}, Filter: job.Filter{Types: []string{"job.failed"}},
		Prompt: "Investigate", Coalesce: job.Coalesce{QuietMs: 5_000, MaxWaitMs: 60_000, MaxBatch: 20},
		Limits:      job.Limits{MaxRunsPerHour: intRef(10), MaxConcurrentRuns: 1, MaxAttempts: 3, RunDeadlineMs: 3_600_000, CooldownMs: 0, MaxDepth: 0},
		SessionMode: job.SessionNew, CreatedBy: job.CreatedByUser,
		CreatedAt: "2026-10-01T00:00:00.000Z", UpdatedAt: "2026-10-01T00:00:00.000Z",
	}
	if mut != nil {
		mut(&t)
	}
	return t
}

// triggerEventFixture builds an accepted event of t with unique ids, changed by mut.
func triggerEventFixture(t job.Job, mut func(*job.Event)) job.Event {
	e := job.Event{
		ID: "event-" + kernel.NewID(), JobID: t.ID, AgentID: t.AgentID, Source: "webhook", SourceEventID: "delivery-" + kernel.NewID(),
		Type: "thing.happened", Time: "2026-10-10T00:00:00.000Z", ReceivedAt: "2026-10-10T00:00:00.000Z",
		Data: json.RawMessage(`{"ok":true}`), CorrelationID: "corr-" + kernel.NewID(), Depth: 0, Status: job.EventAccepted,
	}
	if mut != nil {
		mut(&e)
	}
	return e
}

// acceptOpts are the overrides of the run plan of triggerAcceptInput.
type acceptOpts struct {
	notBefore, deadline, joinNotBefore, createdAt string
	maxBatch                                      int
}

// triggerAcceptInput is the input that accepts e with a fresh run plan.
func triggerAcceptInput(t job.Job, e job.Event, o acceptOpts) persistence.AcceptJobEventInput {
	or := func(v, def string) string {
		if v == "" {
			return def
		}
		return v
	}
	createdAt := or(o.createdAt, e.ReceivedAt)
	maxBatch := o.maxBatch
	if maxBatch == 0 {
		maxBatch = 20
	}
	return persistence.AcceptJobEventInput{
		Event: e,
		Run: &persistence.JobRunPlan{
			NewRun: job.Run{
				ID: "run-" + kernel.NewID(), JobID: t.ID, AgentID: t.AgentID, JobVersion: t.Version,
				BucketKey: e.Subject, Status: job.RunPending, EventIDs: []string{e.ID}, Revision: e.Revision,
				CorrelationID: e.CorrelationID, Depth: e.Depth,
				NotBefore: or(o.notBefore, "2026-10-10T00:00:05.000Z"), Deadline: or(o.deadline, "2026-10-10T00:01:00.000Z"),
				ExpiresAt: "2026-10-10T01:00:00.000Z", Attempts: 0, MaxAttempts: 3, CreatedAt: createdAt, UpdatedAt: createdAt,
			},
			JoinNotBefore: or(o.joinNotBefore, "2026-10-10T00:00:05.000Z"),
			MaxBatch:      maxBatch,
		},
	}
}

func revisionOf(v float64) *float64 { return &v }

// sameEvent compares two events with their Data compared as JSON values, so a store
// that re-encodes the JSON (Postgres jsonb) passes.
func sameEvent(t testing.TB, got, want job.Event, msg string) {
	t.Helper()
	var gd, wd any
	_ = json.Unmarshal(got.Data, &gd)
	_ = json.Unmarshal(want.Data, &wd)
	if !reflect.DeepEqual(gd, wd) {
		t.Errorf("%s: data %s, want %s", msg, got.Data, want.Data)
	}
	got.Data, want.Data = nil, nil
	check.DeepEqual(t, got, want, msg)
}

func triggerRunIDs(runs []job.Run) []string {
	out := make([]string, len(runs))
	for i, r := range runs {
		out[i] = r.ID
	}
	return out
}

func triggerEventIDs(evs []job.Event) []string {
	out := make([]string, len(evs))
	for i, e := range evs {
		out[i] = e.ID
	}
	return out
}

// TriggerStore runs the conformance suite for persistence.TriggerStore implementations
// (event triggers, A-70), the testTriggerStoreConformance suite of core/ts.
// Update (A-126): JobStore - for persistence.JobStore implementations, the
// testJobStoreConformance suite of core/ts: the trigger cases for jobs of every kind,
// plus what the job manager needs for schedule jobs (A-69): every When round-trips,
// UpdateJobState writes only run state, HasActiveRun, and a clock tick deduped on its
// scheduled time.
//
// It checks the invariants the trigger manager relies on: triggers round-trip and list
// per agent; Accept dedupes per trigger + source + source event id and joins or opens
// one run per trigger + bucket (respecting MaxBatch, the deadline and leases); Claim
// leases due runs and takes over expired leases; UpdateRun is guarded by the lease
// owner; usage, LastSucceeded and Prune behave; deletes cascade. A durable
// implementation must also make Accept, Claim and UpdateRun atomic - this suite checks
// the results, including two claims racing for one run.
//
//nolint:gocyclo,maintidx // one subtest per case of the core/ts suite
func JobStore(t *testing.T, newStore JobStoreFactory) {
	t.Helper()

	fresh := func(t *testing.T) persistence.JobStore {
		t.Helper()
		s := newStore(t)
		check.NoErr(t, kernel.Initialize(t.Context(), s))
		return s
	}
	saved := func(t *testing.T, s persistence.JobStore, tr job.Job) job.Job {
		t.Helper()
		check.NoErr(t, s.Save(t.Context(), tr), "save %s", tr.ID)
		return tr
	}
	accept := func(t *testing.T, s persistence.JobStore, in persistence.AcceptJobEventInput) job.AcceptResult {
		t.Helper()
		res, err := s.Accept(t.Context(), in)
		check.NoErr(t, err, "accept")
		return res
	}
	claim := func(t *testing.T, s persistence.JobStore, agentID, now, owner, leaseUntil string, exclude ...string) *job.Run {
		t.Helper()
		r, err := s.Claim(t.Context(), persistence.ClaimJobRunInput{AgentID: agentID, Now: now, Owner: owner, LeaseUntil: leaseUntil, ExcludeJobIDs: exclude})
		check.NoErr(t, err, "claim")
		return r
	}
	update := func(t *testing.T, s persistence.JobStore, runID, owner string, p persistence.JobRunPatch, now string) *job.Run {
		t.Helper()
		r, err := s.UpdateRun(t.Context(), runID, owner, p, now)
		check.NoErr(t, err, "updateRun")
		return r
	}
	getRun := func(t *testing.T, s persistence.JobStore, id string) *job.Run {
		t.Helper()
		r, err := s.GetRun(t.Context(), id)
		check.NoErr(t, err, "getRun")
		return r
	}
	listRuns := func(t *testing.T, s persistence.JobStore, triggerID string, limit int) []job.Run {
		t.Helper()
		r, err := s.ListRuns(t.Context(), triggerID, limit)
		check.NoErr(t, err, "listRuns")
		return r
	}
	listEvents := func(t *testing.T, s persistence.JobStore, triggerID string, limit int) []job.Event {
		t.Helper()
		r, err := s.ListEvents(t.Context(), triggerID, limit)
		check.NoErr(t, err, "listEvents")
		return r
	}
	getEvents := func(t *testing.T, s persistence.JobStore, ids ...string) []job.Event {
		t.Helper()
		r, err := s.GetEvents(t.Context(), ids)
		check.NoErr(t, err, "getEvents")
		return r
	}
	status := func(st job.RunStatus) *job.RunStatus { return &st }
	str := func(s string) *string { return &s }

	// -- Lifecycle --

	t.Run("initialize() and dispose() resolve without error", func(t *testing.T) {
		s := newStore(t)
		check.NoErr(t, kernel.Initialize(t.Context(), s))
		check.NoErr(t, kernel.Dispose(t.Context(), s))
	})

	// -- Triggers --

	t.Run("save() + get() round-trip every field, and save() replaces", func(t *testing.T) {
		s := fresh(t)
		budget := 9_000
		tr := triggerFixture(func(tr *job.Job) {
			tr.Description = "why"
			tr.When = job.When{Kind: job.KindWebhook, Signature: job.SignatureGithub, SubjectPath: "data.pr", RevisionPath: "data.rev"}
			tr.Filter = job.Filter{Types: []string{"a.*"}, Subjects: []string{"s1"}, Where: []job.Condition{
				{Path: "data.x", Op: job.OpIn, Value: []any{float64(1), "2"}}, {Path: "subject", Op: job.OpExists},
			}}
			tr.Limits = job.Limits{MaxRunsPerHour: intRef(4), MaxConcurrentRuns: 2, MaxAttempts: 5, RunDeadlineMs: 120_000, MaxTokensPerDay: &budget, CooldownMs: 1_000, MaxDepth: 1}
			tr.Secret = "whsec_abc"
			// A-70 agent tools: who made it and where its results go.
			tr.CreatedBy = job.CreatedByAgent
			tr.CreatedInSessionID = "session-1"
			// Update (A-127): several of them.
			tr.Deliveries = job.Deliveries{
				{ChannelID: "channel-1", Target: "chat-9", ThreadID: "t-2", When: "on-output"},
				{ChannelID: "channel-2", Target: job.DeliveryHome, When: "on-output"},
			}
			// Update (A-126): and the run state of a job.
			tr.SessionMode, tr.SessionID, tr.LastRunAt, tr.LastStatus, tr.LastError = job.SessionReuse, "session-r", "2026-10-09T00:00:00.000Z", job.RunFailed, "boom"
		})
		saved(t, s, tr)
		got, err := s.Get(t.Context(), tr.ID)
		check.NoErr(t, err)
		if got == nil {
			t.Fatal("trigger is absent")
		}
		check.DeepEqual(t, *got, tr, "round-trip")

		changed := tr.Clone()
		changed.Enabled, changed.Version, changed.Prompt, changed.Secret = false, 2, "changed", ""
		changed.Deliveries = nil
		saved(t, s, changed)
		got, err = s.Get(t.Context(), tr.ID)
		check.NoErr(t, err)
		if got == nil {
			t.Fatal("trigger is absent")
		}
		check.DeepEqual(t, *got, changed, "replaced")
		missing, err := s.Get(t.Context(), "missing")
		check.NoErr(t, err)
		check.True(t, missing == nil, "unknown id must be nil")
	})

	t.Run("listByAgent() is per agent, oldest first; findByName() finds within the agent", func(t *testing.T) {
		s := fresh(t)
		agentID := uniqueAgentID()
		second := triggerFixture(func(tr *job.Job) {
			tr.AgentID, tr.Name, tr.CreatedAt = agentID, "b", "2026-10-02T00:00:00.000Z"
		})
		first := triggerFixture(func(tr *job.Job) {
			tr.AgentID, tr.Name, tr.CreatedAt = agentID, "a", "2026-10-01T00:00:00.000Z"
		})
		other := triggerFixture(func(tr *job.Job) { tr.Name = "a" })
		for _, tr := range []job.Job{second, first, other} {
			saved(t, s, tr)
		}
		list, err := s.ListByAgent(t.Context(), agentID)
		check.NoErr(t, err)
		ids := make([]string, len(list))
		for i, tr := range list {
			ids[i] = tr.ID
		}
		check.True(t, slices.Equal(ids, []string{first.ID, second.ID}), "order %v", ids)
		found, err := s.FindByName(t.Context(), agentID, "a")
		check.NoErr(t, err)
		check.True(t, found != nil && found.ID == first.ID, "findByName")
		none, err := s.FindByName(t.Context(), agentID, "zzz")
		check.NoErr(t, err)
		check.True(t, none == nil, "unknown name must be nil")
	})

	// -- Accept --

	t.Run("accept() stores an event, opens a run for it, and links them", func(t *testing.T) {
		s := fresh(t)
		tr := saved(t, s, triggerFixture(nil))
		e := triggerEventFixture(tr, func(e *job.Event) {
			e.Subject, e.Revision, e.Data = "job-1", revisionOf(3), json.RawMessage(`{"nested":{"n":1},"list":["x"]}`)
		})

		res := accept(t, s, triggerAcceptInput(tr, e, acceptOpts{}))

		check.False(t, res.Duplicate, "duplicate")
		if res.Run == nil {
			t.Fatal("no run")
		}
		r := res.Run
		check.True(t, r.JobID == tr.ID && r.BucketKey == "job-1" && r.Status == job.RunPending && slices.Equal(r.EventIDs, []string{e.ID}) &&
			r.Revision != nil && *r.Revision == 3, "run %+v", *r)
		want := e.Clone()
		want.RunID = r.ID
		sameEvent(t, res.Event, want, "accepted event")
		stored := getEvents(t, s, e.ID)
		check.Len(t, stored, 1)
		if len(stored) == 1 {
			sameEvent(t, stored[0], want, "stored event")
		}
		got := getRun(t, s, r.ID)
		if got == nil {
			t.Fatal("run is absent")
		}
		check.DeepEqual(t, *got, *r, "stored run")
	})

	t.Run("accept() of the same source event again is a duplicate and changes nothing", func(t *testing.T) {
		s := fresh(t)
		tr := saved(t, s, triggerFixture(nil))
		first := accept(t, s, triggerAcceptInput(tr, triggerEventFixture(tr, func(e *job.Event) { e.SourceEventID = "delivery-1" }), acceptOpts{}))

		again := accept(t, s, triggerAcceptInput(tr, triggerEventFixture(tr, func(e *job.Event) {
			e.SourceEventID, e.Data = "delivery-1", json.RawMessage(`"other"`)
		}), acceptOpts{}))

		check.True(t, again.Duplicate, "duplicate")
		check.Equal(t, again.Event.ID, first.Event.ID)
		check.True(t, again.Run == nil, "a duplicate has no run")
		check.Len(t, listEvents(t, s, tr.ID, 0), 1)
		r := getRun(t, s, first.Run.ID)
		check.True(t, r != nil && slices.Equal(r.EventIDs, []string{first.Event.ID}), "run unchanged")
	})

	t.Run("the same source event id from another source or for another trigger is not a duplicate", func(t *testing.T) {
		s := fresh(t)
		t1 := saved(t, s, triggerFixture(nil))
		t2 := saved(t, s, triggerFixture(func(tr *job.Job) { tr.AgentID = t1.AgentID }))
		accept(t, s, triggerAcceptInput(t1, triggerEventFixture(t1, func(e *job.Event) { e.SourceEventID, e.Source = "x", "webhook" }), acceptOpts{}))

		check.False(t, accept(t, s, triggerAcceptInput(t1, triggerEventFixture(t1, func(e *job.Event) { e.SourceEventID, e.Source = "x", "manual" }), acceptOpts{})).Duplicate, "other source")
		check.False(t, accept(t, s, triggerAcceptInput(t2, triggerEventFixture(t2, func(e *job.Event) { e.SourceEventID, e.Source = "x", "webhook" }), acceptOpts{})).Duplicate, "other trigger")
	})

	t.Run("an event for the same bucket joins the open run: ids appended, revision and depth max, notBefore capped by the deadline", func(t *testing.T) {
		s := fresh(t)
		tr := saved(t, s, triggerFixture(nil))
		a := triggerEventFixture(tr, func(e *job.Event) { e.Subject, e.Revision, e.Depth = "s", revisionOf(5), 0 })
		first := accept(t, s, triggerAcceptInput(tr, a, acceptOpts{notBefore: "2026-10-10T00:00:05.000Z", deadline: "2026-10-10T00:01:00.000Z"}))
		b := triggerEventFixture(tr, func(e *job.Event) { e.Subject, e.Revision, e.Depth = "s", revisionOf(2), 1 })

		joined := accept(t, s, triggerAcceptInput(tr, b, acceptOpts{joinNotBefore: "2026-10-10T00:02:00.000Z"}))

		if joined.Run == nil {
			t.Fatal("no run")
		}
		r := joined.Run
		check.Equal(t, r.ID, first.Run.ID)
		check.True(t, slices.Equal(r.EventIDs, []string{a.ID, b.ID}) && r.Revision != nil && *r.Revision == 5 && r.Depth == 1 &&
			r.NotBefore == "2026-10-10T00:01:00.000Z", "joined run %+v", *r)
		check.Equal(t, joined.Event.RunID, first.Run.ID)
		c := triggerEventFixture(tr, func(e *job.Event) { e.Subject = "s" })
		sooner := accept(t, s, triggerAcceptInput(tr, c, acceptOpts{joinNotBefore: "2026-10-10T00:00:30.000Z"}))
		if sooner.Run == nil {
			t.Fatal("no run")
		}
		check.True(t, slices.Equal(sooner.Run.EventIDs, []string{a.ID, b.ID, c.ID}) && sooner.Run.NotBefore == "2026-10-10T00:00:30.000Z" &&
			sooner.Run.Revision != nil && *sooner.Run.Revision == 5, "run %+v", *sooner.Run)
	})

	t.Run("a different bucket, a full run or a leased run opens a new run", func(t *testing.T) {
		s := fresh(t)
		tr := saved(t, s, triggerFixture(nil))
		s1 := accept(t, s, triggerAcceptInput(tr, triggerEventFixture(tr, func(e *job.Event) { e.Subject = "s1" }), acceptOpts{maxBatch: 2}))
		s2 := accept(t, s, triggerAcceptInput(tr, triggerEventFixture(tr, func(e *job.Event) { e.Subject = "s2" }), acceptOpts{maxBatch: 2}))
		check.NotEqual(t, s2.Run.ID, s1.Run.ID)

		// The second event fills the batch: the run closes ("waiting") and takes no more.
		full := accept(t, s, triggerAcceptInput(tr, triggerEventFixture(tr, func(e *job.Event) { e.Subject = "s1" }), acceptOpts{maxBatch: 2}))
		check.True(t, full.Run != nil && full.Run.ID == s1.Run.ID && full.Run.Status == job.RunWaiting, "full run closes")
		third := accept(t, s, triggerAcceptInput(tr, triggerEventFixture(tr, func(e *job.Event) { e.Subject = "s1" }), acceptOpts{maxBatch: 2}))
		check.NotEqual(t, third.Run.ID, s1.Run.ID)
		check.Len(t, third.Run.EventIDs, 1)

		// The newest open run of s1 is claimed: the next event opens another one.
		claim(t, s, tr.AgentID, trigLater, "w1", trigLease)
		claim(t, s, tr.AgentID, trigLater, "w1", trigLease)
		claim(t, s, tr.AgentID, trigLater, "w1", trigLease)
		after := accept(t, s, triggerAcceptInput(tr, triggerEventFixture(tr, func(e *job.Event) { e.Subject = "s1" }), acceptOpts{}))
		check.False(t, slices.Contains([]string{s1.Run.ID, third.Run.ID, s2.Run.ID}, after.Run.ID), "a leased run takes no events")
	})

	t.Run("a filtered or suppressed event is stored without a run", func(t *testing.T) {
		s := fresh(t)
		tr := saved(t, s, triggerFixture(nil))
		e := triggerEventFixture(tr, func(e *job.Event) { e.Status, e.Reason = job.EventFiltered, "type does not match" })

		res := accept(t, s, persistence.AcceptJobEventInput{Event: e})

		check.False(t, res.Duplicate)
		check.True(t, res.Event.Status == job.EventFiltered && res.Event.Reason == "type does not match", "event %+v", res.Event)
		check.True(t, res.Run == nil, "no run")
		check.Equal(t, res.Event.RunID, "")
		check.Len(t, listRuns(t, s, tr.ID, 0), 0)
	})

	// -- Claim / RenewLease / UpdateRun --

	t.Run("claim() leases the soonest due run of the agent and counts the attempt", func(t *testing.T) {
		s := fresh(t)
		tr := saved(t, s, triggerFixture(nil))
		later := accept(t, s, triggerAcceptInput(tr, triggerEventFixture(tr, func(e *job.Event) { e.Subject = "later" }), acceptOpts{notBefore: "2026-10-10T00:00:09.000Z"}))
		sooner := accept(t, s, triggerAcceptInput(tr, triggerEventFixture(tr, func(e *job.Event) { e.Subject = "sooner" }), acceptOpts{notBefore: "2026-10-10T00:00:03.000Z"}))
		accept(t, s, triggerAcceptInput(tr, triggerEventFixture(tr, func(e *job.Event) { e.Subject = "future" }), acceptOpts{notBefore: "2027-01-01T00:00:00.000Z"}))
		other := saved(t, s, triggerFixture(nil))
		accept(t, s, triggerAcceptInput(other, triggerEventFixture(other, nil), acceptOpts{}))

		claimed := claim(t, s, tr.AgentID, "2026-10-10T00:00:10.000Z", "w1", trigLease)

		if claimed == nil {
			t.Fatal("nothing claimed")
		}
		check.True(t, claimed.ID == sooner.Run.ID && claimed.Status == job.RunRunning && claimed.LeaseOwner == "w1" &&
			claimed.LeaseUntil == trigLease && claimed.Attempts == 1, "claimed %+v", *claimed)
		check.Equal(t, claimed.StartedAt, "", "claim does not set startedAt")
		next := claim(t, s, tr.AgentID, "2026-10-10T00:00:10.000Z", "w1", trigLease)
		check.True(t, next != nil && next.ID == later.Run.ID, "second claim")
		check.True(t, claim(t, s, tr.AgentID, "2026-10-10T00:00:10.000Z", "w1", trigLease) == nil, "nothing else due")
	})

	t.Run("claim() skips excluded triggers", func(t *testing.T) {
		s := fresh(t)
		tr := saved(t, s, triggerFixture(nil))
		accept(t, s, triggerAcceptInput(tr, triggerEventFixture(tr, nil), acceptOpts{}))
		check.True(t, claim(t, s, tr.AgentID, trigLater, "w1", trigLease, tr.ID) == nil, "excluded")
		check.True(t, claim(t, s, tr.AgentID, trigLater, "w1", trigLease, "other") != nil, "not excluded")
	})

	t.Run("two claims racing for one due run: exactly one gets it", func(t *testing.T) {
		s := fresh(t)
		tr := saved(t, s, triggerFixture(nil))
		accept(t, s, triggerAcceptInput(tr, triggerEventFixture(tr, nil), acceptOpts{}))

		var wg sync.WaitGroup
		got := make([]*job.Run, 2)
		errs := make([]error, 2)
		for i, owner := range []string{"w1", "w2"} {
			wg.Go(func() {
				got[i], errs[i] = s.Claim(t.Context(), persistence.ClaimJobRunInput{AgentID: tr.AgentID, Now: trigLater, Owner: owner, LeaseUntil: trigLease})
			})
		}
		wg.Wait()
		check.NoErr(t, errs[0])
		check.NoErr(t, errs[1])
		n := 0
		for _, r := range got {
			if r != nil {
				n++
			}
		}
		check.Equal(t, n, 1, "claims that got the run")
	})

	t.Run("a run whose lease expired is taken over (attempt counted); a live lease is not", func(t *testing.T) {
		s := fresh(t)
		tr := saved(t, s, triggerFixture(nil))
		res := accept(t, s, triggerAcceptInput(tr, triggerEventFixture(tr, nil), acceptOpts{}))
		claim(t, s, tr.AgentID, trigLater, "dead", "2026-10-10T00:05:00.000Z")

		check.True(t, claim(t, s, tr.AgentID, "2026-10-10T00:04:00.000Z", "w2", trigLease) == nil, "a live lease is kept")
		taken := claim(t, s, tr.AgentID, "2026-10-10T00:06:00.000Z", "w2", "2026-10-10T00:07:00.000Z")

		check.True(t, taken != nil && taken.ID == res.Run.ID && taken.LeaseOwner == "w2" && taken.Attempts == 2, "taken over")
		held, err := s.RenewLease(t.Context(), res.Run.ID, "dead", trigLease)
		check.NoErr(t, err)
		check.False(t, held, "the dead worker cannot renew")
		held, err = s.RenewLease(t.Context(), res.Run.ID, "w2", "2026-10-10T00:09:00.000Z")
		check.NoErr(t, err)
		check.True(t, held, "the holder renews")
		r := getRun(t, s, res.Run.ID)
		check.True(t, r != nil && r.LeaseUntil == "2026-10-10T00:09:00.000Z", "lease extended")
	})

	t.Run("updateRun() is guarded by the lease owner, applies the patch, clears fields and releases the lease", func(t *testing.T) {
		s := fresh(t)
		tr := saved(t, s, triggerFixture(nil))
		res := accept(t, s, triggerAcceptInput(tr, triggerEventFixture(tr, nil), acceptOpts{}))
		claim(t, s, tr.AgentID, trigLater, "w1", trigLease)
		id := res.Run.ID

		check.True(t, update(t, s, id, "w2", persistence.JobRunPatch{Status: status(job.RunFailed)}, trigLater) == nil, "another owner")
		check.True(t, update(t, s, id, "", persistence.JobRunPatch{Status: status(job.RunStopped)}, trigLater) == nil, "no owner on a leased run")

		cp := update(t, s, id, "w1", persistence.JobRunPatch{
			SessionID: str("s-1"), StartedAt: str(trigLater), ResultAt: str(trigLater), Output: str("done"),
			Usage: &job.Usage{InputTokens: 7, OutputTokens: 3}, StopReason: str("max_tokens"), Error: str("old"), Reason: str("Needs approval: x"),
		}, "2026-10-10T00:03:00.000Z")
		if cp == nil {
			t.Fatal("checkpoint refused")
		}
		check.True(t, cp.Status == job.RunRunning && cp.LeaseOwner == "w1" && cp.SessionID == "s-1" && cp.Output == "done" &&
			cp.Usage != nil && *cp.Usage == (job.Usage{InputTokens: 7, OutputTokens: 3}) && cp.Reason == "Needs approval: x" &&
			cp.UpdatedAt == "2026-10-10T00:03:00.000Z", "checkpoint %+v", *cp)

		finished := update(t, s, id, "w1", persistence.JobRunPatch{
			Status: status(job.RunSucceeded), FinishedAt: str(trigLater), DeliveredAt: str(trigLater), ReleaseLease: true,
			Clear: []persistence.JobRunField{persistence.JobRunError, persistence.JobRunReason},
		}, trigLater)
		if finished == nil {
			t.Fatal("finish refused")
		}
		check.True(t, finished.Status == job.RunSucceeded && finished.DeliveredAt == trigLater && finished.Output == "done", "finished %+v", *finished)
		check.True(t, finished.LeaseOwner == "" && finished.LeaseUntil == "" && finished.Error == "" && finished.Reason == "", "cleared %+v", *finished)
		stored := getRun(t, s, id)
		if stored == nil {
			t.Fatal("run is absent")
		}
		check.DeepEqual(t, *stored, *finished, "stored run")
	})

	t.Run("updateRun(null) changes only a queued, unleased run", func(t *testing.T) {
		s := fresh(t)
		tr := saved(t, s, triggerFixture(nil))
		res := accept(t, s, triggerAcceptInput(tr, triggerEventFixture(tr, nil), acceptOpts{}))

		stopped := update(t, s, res.Run.ID, "", persistence.JobRunPatch{Status: status(job.RunStopped), FinishedAt: str(trigLater)}, trigLater)
		check.True(t, stopped != nil && stopped.Status == job.RunStopped, "stopped")
		check.True(t, update(t, s, res.Run.ID, "", persistence.JobRunPatch{Status: status(job.RunFailed)}, trigLater) == nil, "a finished run")
		check.True(t, update(t, s, "missing", "", persistence.JobRunPatch{Status: status(job.RunFailed)}, trigLater) == nil, "an unknown run")
	})

	t.Run("a released run waits for notBefore, then is claimable again", func(t *testing.T) {
		s := fresh(t)
		tr := saved(t, s, triggerFixture(nil))
		res := accept(t, s, triggerAcceptInput(tr, triggerEventFixture(tr, nil), acceptOpts{}))
		claim(t, s, tr.AgentID, trigLater, "w1", trigLease)
		update(t, s, res.Run.ID, "w1", persistence.JobRunPatch{Status: status(job.RunWaiting), NotBefore: str("2026-10-10T01:00:00.000Z"), Error: str("503"), ReleaseLease: true}, trigLater)

		check.True(t, claim(t, s, tr.AgentID, "2026-10-10T00:59:00.000Z", "w1", trigLease) == nil, "before notBefore")
		again := claim(t, s, tr.AgentID, "2026-10-10T01:00:00.000Z", "w1", trigLease)
		check.True(t, again != nil && again.ID == res.Run.ID && again.Attempts == 2 && again.Error == "503", "claimed again")
		// A waiting run never takes new events: they open their own run.
		update(t, s, res.Run.ID, "w1", persistence.JobRunPatch{Status: status(job.RunWaiting), ReleaseLease: true}, trigLater)
		next := accept(t, s, triggerAcceptInput(tr, triggerEventFixture(tr, nil), acceptOpts{}))
		check.NotEqual(t, next.Run.ID, res.Run.ID)
	})

	t.Run("insertRun() adds a run (a replay) that lists with the others, newest first", func(t *testing.T) {
		s := fresh(t)
		tr := saved(t, s, triggerFixture(nil))
		res := accept(t, s, triggerAcceptInput(tr, triggerEventFixture(tr, nil), acceptOpts{createdAt: "2026-10-10T00:00:00.000Z"}))
		replay := res.Run.Clone()
		replay.ID, replay.Status, replay.Attempts, replay.ReplayOf = "run-"+kernel.NewID(), job.RunWaiting, 0, res.Run.ID
		replay.CreatedAt, replay.UpdatedAt = "2026-10-10T00:10:00.000Z", "2026-10-10T00:10:00.000Z"

		check.NoErr(t, s.InsertRun(t.Context(), replay))

		got := getRun(t, s, replay.ID)
		if got == nil {
			t.Fatal("replay is absent")
		}
		check.DeepEqual(t, *got, replay, "replay")
		check.True(t, slices.Equal(triggerRunIDs(listRuns(t, s, tr.ID, 0)), []string{replay.ID, res.Run.ID}), "order")
		check.Len(t, listRuns(t, s, tr.ID, 1), 1)
	})

	// -- History --

	t.Run("listEvents() is newest first; getEvents() keeps the order asked and skips unknown ids", func(t *testing.T) {
		s := fresh(t)
		tr := saved(t, s, triggerFixture(nil))
		old := triggerEventFixture(tr, func(e *job.Event) { e.ReceivedAt = "2026-10-10T00:00:01.000Z" })
		recent := triggerEventFixture(tr, func(e *job.Event) { e.ReceivedAt = "2026-10-10T00:00:02.000Z" })
		accept(t, s, triggerAcceptInput(tr, old, acceptOpts{}))
		accept(t, s, triggerAcceptInput(tr, recent, acceptOpts{}))

		check.True(t, slices.Equal(triggerEventIDs(listEvents(t, s, tr.ID, 0)), []string{recent.ID, old.ID}), "newest first")
		check.True(t, slices.Equal(triggerEventIDs(listEvents(t, s, tr.ID, 1)), []string{recent.ID}), "limit")
		check.True(t, slices.Equal(triggerEventIDs(getEvents(t, s, old.ID, "missing", recent.ID)), []string{old.ID, recent.ID}), "getEvents order")
	})

	t.Run("usageSince() counts runs whose turn started since, and their tokens", func(t *testing.T) {
		s := fresh(t)
		tr := saved(t, s, triggerFixture(nil))
		for _, subject := range []string{"a", "b", "c"} {
			accept(t, s, triggerAcceptInput(tr, triggerEventFixture(tr, func(e *job.Event) { e.Subject = subject }), acceptOpts{}))
		}
		// Two of the three runs start; the third stays queued (not counted).
		for i, startedAt := range []string{"2026-10-10T00:00:00.000Z", "2026-10-10T01:00:00.000Z"} {
			claimed := claim(t, s, tr.AgentID, trigLater, "w", trigLease)
			if claimed == nil {
				t.Fatal("nothing claimed")
			}
			update(t, s, claimed.ID, "w", persistence.JobRunPatch{StartedAt: str(startedAt), Usage: &job.Usage{InputTokens: 10 * (i + 1), OutputTokens: 1}}, trigLater)
		}

		u, err := s.UsageSince(t.Context(), tr.ID, "2026-10-10T00:30:00.000Z")
		check.NoErr(t, err)
		check.Equal(t, u, persistence.JobUsage{Runs: 1, Tokens: 21})
		u, err = s.UsageSince(t.Context(), tr.ID, "2026-10-09T00:00:00.000Z")
		check.NoErr(t, err)
		check.Equal(t, u, persistence.JobUsage{Runs: 2, Tokens: 32})
		u, err = s.UsageSince(t.Context(), "other", "2026-10-09T00:00:00.000Z")
		check.NoErr(t, err)
		check.Equal(t, u, persistence.JobUsage{})
	})

	t.Run("lastSucceeded() is the newest succeeded run of the bucket", func(t *testing.T) {
		s := fresh(t)
		tr := saved(t, s, triggerFixture(nil))
		finish := func(subject string, st job.RunStatus, finishedAt string) string {
			res := accept(t, s, triggerAcceptInput(tr, triggerEventFixture(tr, func(e *job.Event) { e.Subject = subject }), acceptOpts{}))
			claim(t, s, tr.AgentID, trigLater, "w", trigLease)
			update(t, s, res.Run.ID, "w", persistence.JobRunPatch{Status: status(st), FinishedAt: str(finishedAt), ReleaseLease: true}, trigLater)
			return res.Run.ID
		}
		finish("s", job.RunSucceeded, "2026-10-10T00:00:01.000Z")
		newest := finish("s", job.RunSucceeded, "2026-10-10T00:00:03.000Z")
		finish("s", job.RunFailed, "2026-10-10T00:00:09.000Z")
		finish("other", job.RunSucceeded, "2026-10-10T00:00:10.000Z")

		last, err := s.LastSucceeded(t.Context(), tr.ID, "s")
		check.NoErr(t, err)
		check.True(t, last != nil && last.ID == newest, "newest succeeded")
		none, err := s.LastSucceeded(t.Context(), tr.ID, "none")
		check.NoErr(t, err)
		check.True(t, none == nil, "no run for the bucket")
	})

	t.Run("prune() keeps the newest events and finished runs, and never queued runs or their events", func(t *testing.T) {
		s := fresh(t)
		tr := saved(t, s, triggerFixture(nil))
		var finished []string
		for i := range 3 {
			n := strconv.Itoa(i)
			at := "2026-10-10T00:00:0" + n + ".000Z"
			res := accept(t, s, triggerAcceptInput(tr, triggerEventFixture(tr, func(e *job.Event) { e.Subject, e.ReceivedAt = "f"+n, at }), acceptOpts{createdAt: at}))
			claim(t, s, tr.AgentID, trigLater, "w", trigLease)
			update(t, s, res.Run.ID, "w", persistence.JobRunPatch{Status: status(job.RunSucceeded), FinishedAt: str(trigLater), ReleaseLease: true}, trigLater)
			finished = append(finished, res.Run.ID)
		}
		queued := accept(t, s, triggerAcceptInput(tr, triggerEventFixture(tr, func(e *job.Event) { e.Subject, e.ReceivedAt = "q", "2026-10-09T00:00:00.000Z" }), acceptOpts{notBefore: "2027-01-01T00:00:00.000Z"}))

		check.NoErr(t, s.Prune(t.Context(), tr.ID, 1, 1))

		runs := triggerRunIDs(listRuns(t, s, tr.ID, 0))
		check.True(t, slices.Contains(runs, queued.Run.ID), "the queued run is kept")
		check.True(t, slices.Contains(runs, finished[2]), "the newest finished run is kept")
		check.False(t, slices.Contains(runs, finished[0]), "an old finished run is pruned")
		check.Len(t, runs, 2)
		evs := triggerEventIDs(listEvents(t, s, tr.ID, 0))
		check.True(t, slices.Contains(evs, queued.Event.ID), "the queued run's event is kept")
		check.Len(t, evs, 2)
	})

	t.Run("delete() and deleteByAgent() remove triggers with their events and runs", func(t *testing.T) {
		s := fresh(t)
		tr := saved(t, s, triggerFixture(nil))
		keep := saved(t, s, triggerFixture(func(k *job.Job) { k.AgentID = tr.AgentID }))
		res := accept(t, s, triggerAcceptInput(tr, triggerEventFixture(tr, nil), acceptOpts{}))
		accept(t, s, triggerAcceptInput(keep, triggerEventFixture(keep, nil), acceptOpts{}))

		check.NoErr(t, s.Delete(t.Context(), tr.ID))

		gone, err := s.Get(t.Context(), tr.ID)
		check.NoErr(t, err)
		check.True(t, gone == nil, "trigger deleted")
		check.True(t, getRun(t, s, res.Run.ID) == nil, "run deleted")
		check.Len(t, getEvents(t, s, res.Event.ID), 0)
		check.Len(t, listEvents(t, s, keep.ID, 0), 1)

		check.NoErr(t, s.DeleteByAgent(t.Context(), tr.AgentID))
		list, err := s.ListByAgent(t.Context(), tr.AgentID)
		check.NoErr(t, err)
		check.Len(t, list, 0)
		check.Len(t, listRuns(t, s, keep.ID, 0), 0)
		check.Len(t, listEvents(t, s, keep.ID, 0), 0)
	})

	// -- Schedule jobs (A-69 / A-126) --

	t.Run("save() + get() round-trip each schedule kind", func(t *testing.T) {
		s := fresh(t)
		agentID := uniqueAgentID()
		next := "2027-01-04T01:00:00.000Z"
		kinds := []job.When{
			{Kind: job.KindCron, Expr: "0 8 * * 1", TZ: "Asia/Ho_Chi_Minh"},
			{Kind: job.KindEvery, Ms: 3_600_000},
			{Kind: job.KindAt, At: "2027-01-01T09:00:00.000Z"},
		}
		for i, w := range kinds {
			j := saved(t, s, triggerFixture(func(j *job.Job) {
				j.AgentID, j.When, j.Filter, j.Limits = agentID, w, job.Filter{}, job.DefaultScheduleLimits()
				if i < 2 {
					j.NextRunAt = &next
				} else {
					j.Enabled = false
				}
			}))
			got, err := s.Get(t.Context(), j.ID)
			check.NoErr(t, err)
			check.True(t, got != nil && reflect.DeepEqual(*got, j), "schedule job "+strconv.Itoa(i)+" round-trips")
		}
	})

	t.Run("A-103: save() + get() round-trip a tool job's action and the new limits; updateRun() stores a run's action", func(t *testing.T) {
		s := fresh(t)
		runMs, pause := 600_000, 3
		tr := triggerFixture(func(j *job.Job) {
			j.When, j.Filter, j.Prompt = job.When{Kind: job.KindEvery, Ms: 60_000}, job.Filter{}, ""
			j.Action = &job.Action{Kind: job.ActionTool, Tool: "terminal", FollowUp: job.FollowUpOnFailure, Args: map[string]any{
				"command": "python report.py", "cwd": "/work", "flags": []any{float64(1), "x", map[string]any{"deep": true}},
			}}
			j.Limits = job.Limits{MaxConcurrentRuns: 1, MaxAttempts: 1, RunDeadlineMs: 120_000, MaxRunMs: &runMs, PauseAfterFailures: &pause}
		})
		saved(t, s, tr)
		got, err := s.Get(t.Context(), tr.ID)
		check.NoErr(t, err)
		if got == nil {
			t.Fatal("job is absent")
		}
		check.DeepEqual(t, *got, tr, "round-trip")

		// Back to a prompt job: no action.
		prompt := tr.Clone()
		prompt.Prompt, prompt.Version, prompt.Action = "p", 2, nil
		saved(t, s, prompt)
		got, err = s.Get(t.Context(), tr.ID)
		check.NoErr(t, err)
		if got == nil {
			t.Fatal("job is absent")
		}
		check.DeepEqual(t, *got, prompt, "replaced")

		res := accept(t, s, triggerAcceptInput(prompt, triggerEventFixture(prompt, nil), acceptOpts{}))
		claim(t, s, prompt.AgentID, trigLater, "w1", trigLease)
		check.True(t, res.Run.Action == "", "a new run has no action")
		action := job.ActionTool
		marked := update(t, s, res.Run.ID, "w1", persistence.JobRunPatch{Action: &action, Silent: boolRef(true)}, trigLater)
		if marked == nil {
			t.Fatal("update refused")
		}
		check.True(t, marked.Action == job.ActionTool && marked.Silent, "marked %+v", *marked)
		stored := getRun(t, s, res.Run.ID)
		check.True(t, stored != nil && stored.Action == job.ActionTool, "stored action")
	})

	t.Run("updateJobState() writes only the run state and leaves the configuration alone", func(t *testing.T) {
		s := fresh(t)
		next := "2026-10-10T00:01:00.000Z"
		j := saved(t, s, triggerFixture(func(j *job.Job) {
			j.When, j.Filter, j.Limits, j.NextRunAt, j.LastError = job.When{Kind: job.KindEvery, Ms: 60_000}, job.Filter{}, job.DefaultScheduleLimits(), &next, "old"
		}))
		later := "2026-10-10T00:02:00.000Z"
		updated, err := s.UpdateJobState(t.Context(), j.ID, persistence.JobStatePatch{
			NextRunAt: &later, LastRunAt: ptrTo(trigLater), LastStatus: ptrTo(job.RunRunning), SessionID: ptrTo("s-9"),
		})
		check.NoErr(t, err)
		want := j.Clone()
		want.NextRunAt, want.LastRunAt, want.LastStatus, want.SessionID = &later, trigLater, job.RunRunning, "s-9"
		check.True(t, updated != nil && reflect.DeepEqual(*updated, want), "state written")

		cleared, err := s.UpdateJobState(t.Context(), j.ID, persistence.JobStatePatch{
			LastStatus: ptrTo(job.RunSucceeded), ClearLastError: true, Enabled: ptrTo(false), ClearNextRunAt: true,
		})
		check.NoErr(t, err)
		check.True(t, cleared != nil && cleared.LastStatus == job.RunSucceeded && !cleared.Enabled && cleared.NextRunAt == nil && cleared.LastError == "", "state cleared")
		check.True(t, cleared.Prompt == j.Prompt && cleared.Version == j.Version && cleared.UpdatedAt == j.UpdatedAt, "configuration untouched")
		got, err := s.Get(t.Context(), j.ID)
		check.NoErr(t, err)
		check.True(t, got != nil && reflect.DeepEqual(*got, *cleared), "stored")
		missing, err := s.UpdateJobState(t.Context(), "missing", persistence.JobStatePatch{LastStatus: ptrTo(job.RunFailed)})
		check.NoErr(t, err)
		check.True(t, missing == nil, "unknown job")
	})

	t.Run("hasActiveRun() is true while a run is queued or running, false once it finished", func(t *testing.T) {
		s := fresh(t)
		tr := saved(t, s, triggerFixture(nil))
		active := func() bool {
			b, err := s.HasActiveRun(t.Context(), tr.ID)
			check.NoErr(t, err)
			return b
		}
		check.True(t, !active(), "no run yet")
		res := accept(t, s, triggerAcceptInput(tr, triggerEventFixture(tr, nil), acceptOpts{}))
		check.True(t, active(), "queued")
		claim(t, s, tr.AgentID, trigLater, "w", trigLease)
		check.True(t, active(), "running")
		update(t, s, res.Run.ID, "w", persistence.JobRunPatch{Status: ptrTo(job.RunSucceeded), FinishedAt: ptrTo(trigLater), ReleaseLease: true}, trigLater)
		check.True(t, !active(), "finished")
	})

	t.Run("a clock tick for the same scheduled time is a duplicate (a time runs at most once)", func(t *testing.T) {
		s := fresh(t)
		j := saved(t, s, triggerFixture(func(j *job.Job) { j.When, j.Filter = job.When{Kind: job.KindEvery, Ms: 60_000}, job.Filter{} }))
		tick := func(id string) job.Event {
			return triggerEventFixture(j, func(e *job.Event) { e.Source, e.SourceEventID, e.Type = "schedule", id, "job.tick" })
		}
		first := accept(t, s, triggerAcceptInput(j, tick("2026-10-10T00:01:00.000Z"), acceptOpts{maxBatch: 1}))
		again := accept(t, s, triggerAcceptInput(j, tick("2026-10-10T00:01:00.000Z"), acceptOpts{maxBatch: 1}))
		next := accept(t, s, triggerAcceptInput(j, tick("2026-10-10T00:02:00.000Z"), acceptOpts{maxBatch: 1}))
		// maxBatch 1: a tick's run is closed at once - it takes no other tick.
		check.True(t, first.Run != nil && first.Run.Status == job.RunWaiting && len(first.Run.EventIDs) == 1, "tick run closed")
		check.True(t, again.Duplicate, "same time is a duplicate")
		check.True(t, next.Run != nil && next.Run.ID != first.Run.ID, "next time opens a new run")
	})

	t.Run("records are copied in and out", func(t *testing.T) {
		s := fresh(t)
		tr := saved(t, s, triggerFixture(nil))
		res := accept(t, s, triggerAcceptInput(tr, triggerEventFixture(tr, nil), acceptOpts{}))
		res.Run.EventIDs[0] = "mutated"
		res.Event.Data[0] = 'X'
		tr.Filter.Types[0] = "mutated"
		r := getRun(t, s, res.Run.ID)
		check.True(t, r != nil && r.EventIDs[0] != "mutated", "run copied out")
		evs := getEvents(t, s, res.Event.ID)
		check.True(t, len(evs) == 1 && evs[0].Data[0] != 'X', "event copied out")
		got, err := s.Get(t.Context(), tr.ID)
		check.NoErr(t, err)
		check.True(t, got != nil && got.Filter.Types[0] != "mutated", "trigger copied in")
	})
}

func intRef(v int) *int { return &v }

func boolRef(v bool) *bool { return &v }

func ptrTo[T any](v T) *T { return &v }
