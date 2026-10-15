package memorysystem

import (
	"context"
	"math"
	"slices"

	"github.com/Phong95/aec-agent-go/components/agent/memorysystem/memory"
	"github.com/Phong95/aec-agent-go/components/persistence"
	"github.com/Phong95/aec-agent-go/internal/jsstr"
)

// ---------------------------------------------------------------------------
// Retrieval - relevance-ranked recall for the current context
//
// Combines the keyword index search with metadata scoring (recency and importance)
// and returns a compact evidence set, not all memories.
// ---------------------------------------------------------------------------

// defaultRecallLimit is the number of memories a query returns when it does not say.
const defaultRecallLimit = 10

// dayMs is the number of milliseconds in a day, the unit of the recency decay.
const dayMs = 1000 * 60 * 60 * 24

// retrieval is the recall component of one agent.
type retrieval struct {
	deps
	store    persistence.MemoryStore
	indexing *indexing
}

// newRetrieval returns the recall component of one agent.
func newRetrieval(d deps, store persistence.MemoryStore, ix *indexing) *retrieval {
	return &retrieval{deps: d, store: store, indexing: ix}
}

// recall returns the memories relevant to the query, best first:
//
//  1. search the index with twice the limit, keep only the querying agent's active
//     records that pass the scope and kinds filters, and score each as
//     0.50 relevance + 0.25 recency + 0.25 importance;
//  2. when the query names a scope or kinds, add the agent's active records of that
//     scope and kinds that the keyword search did not find, scored as
//     0.50 recency + 0.50 importance with a relevance of zero;
//  3. sort by the combined score, stable so that equal scores keep their order, cut
//     to the limit and stamp every returned record with lastRecalledAt.
//
// Each product of the weighted sum is converted to float64 on its own. The Go spec
// lets a compiler fuse x*y+z into one fused multiply-add on some architectures, which
// rounds once instead of twice and would change a score in the last digit; an
// explicit conversion forbids the fusion, so the scores equal the JS ones everywhere.
func (r *retrieval) recall(ctx context.Context, q memory.RetrievalQuery) ([]memory.RetrievalResult, error) {
	limit := defaultRecallLimit
	if q.MaxResults != nil {
		limit = *q.MaxResults
	}
	nowMs := float64(r.opts.now().UnixMilli())

	// Step 1: keyword search for relevance, then load the full records and score.
	matches := r.indexing.search(q.CurrentContext, limit*2)
	results := []memory.RetrievalResult{}
	for _, match := range matches {
		rec, err := r.store.Get(ctx, match.recordID)
		if err != nil {
			return nil, err
		}
		if rec == nil || rec.Status != memory.StatusActive {
			continue
		}
		// Agent filter: only recall the querying agent's own memories.
		if rec.AgentID != q.AgentID {
			continue
		}
		if q.Scope != "" && rec.Scope != q.Scope {
			continue
		}
		if len(q.Kinds) > 0 && !slices.Contains(q.Kinds, rec.Kind) {
			continue
		}

		relevance := match.score
		recency := r.recencyScore(*rec, nowMs)
		importance := importanceScore(*rec)
		combined := float64(relevance*0.50) + float64(recency*0.25) + float64(importance*0.25)
		results = append(results, memory.RetrievalResult{
			Record:          *rec,
			RelevanceScore:  relevance,
			RecencyScore:    recency,
			ImportanceScore: importance,
			CombinedScore:   combined,
		})
	}

	// Step 2: also pull scope-matched records that the keyword search did not find.
	if q.Scope != "" || len(q.Kinds) > 0 {
		filter := persistence.MemoryFilter{
			Kinds:    q.Kinds,
			Statuses: []memory.Status{memory.StatusActive},
			Limit:    limit * 2,
		}
		if q.Scope != "" {
			filter.Scopes = []memory.Scope{q.Scope}
		}
		byMetadata, err := r.indexing.queryByMetadata(ctx, filter)
		if err != nil {
			return nil, err
		}
		seen := map[string]struct{}{}
		for _, res := range results {
			seen[res.Record.ID] = struct{}{}
		}
		for _, rec := range byMetadata {
			if _, dup := seen[rec.ID]; dup {
				continue
			}
			recency := r.recencyScore(rec, nowMs)
			importance := importanceScore(rec)
			results = append(results, memory.RetrievalResult{
				Record:          rec,
				RelevanceScore:  0,
				RecencyScore:    recency,
				ImportanceScore: importance,
				CombinedScore:   float64(recency*0.50) + float64(importance*0.50),
			})
		}
	}

	// Step 3: sort by the combined score and return the top N. The comparator is the
	// JS one, b - a, so a NaN score (an unparsable timestamp) compares equal to
	// everything instead of ordering before it.
	slices.SortStableFunc(results, func(a, b memory.RetrievalResult) int {
		d := b.CombinedScore - a.CombinedScore
		switch {
		case d < 0:
			return -1
		case d > 0:
			return 1
		}
		return 0
	})
	recalled := jsSliceHead(results, limit)
	r.markRecalled(ctx, recalled)
	return recalled, nil
}

// markRecalled stamps lastRecalledAt on every recalled record, in the results and in
// the store. Memories in active use must not expire, and a failed write must never
// break recall itself, so a failure is logged (the record id only) and the recall still
// returns.
//
// The store write re-reads each record under the write mutex and sets only
// lastRecalledAt on that fresh copy. A recall and a consolidation may run in
// parallel, so writing back the copy read earlier could overwrite a status that a
// consolidation set in between. A record that was deleted in the meantime stays
// deleted.
func (r *retrieval) markRecalled(ctx context.Context, results []memory.RetrievalResult) {
	if len(results) == 0 {
		return
	}
	now := isoNow(r.opts.now)
	r.writeMu.Lock()
	defer r.writeMu.Unlock()
	for i := range results {
		results[i].Record.LastRecalledAt = now
		id := results[i].Record.ID
		cur, err := r.store.Get(ctx, id)
		if err == nil && cur != nil {
			cur.LastRecalledAt = now
			err = r.store.Update(ctx, *cur)
		}
		if err != nil {
			r.opts.logger.Warn("memory: failed to mark a record as recalled", "agent", r.agentID, "record", id, "error", err)
		}
	}
}

// recencyScore is the linear decay over 30 days from the last write (updatedAt, or
// observedAt without one): 1 now, 0 at 30 days and beyond, and above 1 for a timestamp
// in the future. An unparsable timestamp is NaN, as in JavaScript.
func (r *retrieval) recencyScore(rec memory.Record, nowMs float64) float64 {
	lastTouch := rec.UpdatedAt
	if lastTouch == "" {
		lastTouch = rec.ObservedAt
	}
	ageMs := nowMs - jsstr.ParseDate(lastTouch, r.opts.loc)
	ageDays := ageMs / dayMs
	return math.Max(0, 1-ageDays/30)
}

// importanceScore is derived from the confidence and the trust of the record: the
// mean of the two weights, an unknown value weighing 0.
func importanceScore(rec memory.Record) float64 {
	confidenceWeights := map[memory.Confidence]float64{
		memory.ConfidenceExplicit: 1.0, memory.ConfidenceVerified: 0.8, memory.ConfidenceInferred: 0.5, memory.ConfidenceUnverified: 0.2,
	}
	trustWeights := map[memory.SourceTrust]float64{
		memory.TrustUserApproved: 1.0, memory.TrustVerifiedInternal: 0.8, memory.TrustTeamAuthored: 0.6,
		memory.TrustExternalDocument: 0.3, memory.TrustUntrusted: 0.1, memory.TrustModelSpeculation: 0.1,
	}
	c := confidenceWeights[rec.Confidence]
	t := trustWeights[rec.SourceTrust]
	return (c + t) / 2
}
