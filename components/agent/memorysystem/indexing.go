package memorysystem

import (
	"cmp"
	"context"
	"slices"
	"strings"
	"sync"

	"github.com/Phong95/aec-agent-go/components/agent/memorysystem/memory"
	"github.com/Phong95/aec-agent-go/components/persistence"
	"github.com/Phong95/aec-agent-go/internal/jsstr"
	"github.com/Phong95/aec-agent-go/kernel"
	"github.com/Phong95/aec-agent-go/kernel/ordered"
)

// ---------------------------------------------------------------------------
// Indexing - make memories searchable for Retrieval
//
// The default implementation is an in-memory inverted keyword index that is rebuilt
// from the memory store on demand.
// ---------------------------------------------------------------------------

// scoredMatch is one search hit: the record id and the share of the query terms it
// contains.
type scoredMatch struct {
	recordID string
	score    float64
}

// indexing is the keyword index of one agent's active memories plus a metadata query
// that is always scoped to the agent.
type indexing struct {
	agentID  string
	store    persistence.MemoryStore
	strategy *keywordIndex
}

// newIndexing returns the index of one agent over its memory store.
func newIndexing(agentID string, store persistence.MemoryStore) *indexing {
	return &indexing{agentID: agentID, store: store, strategy: newKeywordIndex()}
}

// rebuild rebuilds the index from all active memories of the agent and returns how many
// were indexed.
func (ix *indexing) rebuild(ctx context.Context) (int, error) {
	records, err := ix.store.Query(ctx, persistence.MemoryFilter{AgentID: ix.agentID, Statuses: []memory.Status{memory.StatusActive}})
	if err != nil {
		return 0, err
	}
	ix.strategy.rebuild(records)
	return len(records), nil
}

// search finds the memories that match the query, best first.
func (ix *indexing) search(query string, limit int) []scoredMatch {
	return ix.strategy.search(query, limit)
}

// queryByMetadata queries the memory store directly, scoped to the agent.
func (ix *indexing) queryByMetadata(ctx context.Context, filter persistence.MemoryFilter) ([]memory.Record, error) {
	filter.AgentID = ix.agentID
	return ix.store.Query(ctx, filter)
}

// keywordIndex is the default in-memory inverted index: it tokenizes value, key,
// subject and tags into lowercase terms and scores a query by how many of its terms a
// record holds. Records keep their insertion order, as a JS Map does, because the
// order is the tie-break of equal scores.
//
// The agent loop searches while the consolidation actor rebuilds, so the index is
// guarded by a read/write mutex; rebuild builds the new index first and swaps it in,
// so a search never sees a half-built one.
type keywordIndex struct {
	mu sync.RWMutex
	// terms maps a record id to its set of lowercase terms, in insertion order.
	terms ordered.Map[map[string]struct{}]
}

func newKeywordIndex() *keywordIndex { return &keywordIndex{} }

// rebuild replaces the index with the records.
func (k *keywordIndex) rebuild(records []memory.Record) {
	fresh := ordered.Map[map[string]struct{}]{}
	for _, r := range records {
		fresh.Set(r.ID, termSet(r))
	}
	k.mu.Lock()
	k.terms = fresh
	k.mu.Unlock()
}

// search scores every record by hits / query terms (a query term that repeats counts
// every time), drops the records without a hit, orders them by score with a stable sort
// so equal scores keep insertion order, and cuts to limit with the semantics of JS
// slice(0, limit).
func (k *keywordIndex) search(query string, limit int) []scoredMatch {
	queryTerms := tokenize(query)
	if len(queryTerms) == 0 {
		return []scoredMatch{}
	}

	k.mu.RLock()
	scores := []scoredMatch{}
	for id, terms := range k.terms.All() {
		hits := 0
		for _, qt := range queryTerms {
			if _, ok := terms[qt]; ok {
				hits++
			}
		}
		if hits > 0 {
			scores = append(scores, scoredMatch{recordID: id, score: float64(hits) / float64(len(queryTerms))})
		}
	}
	k.mu.RUnlock()

	slices.SortStableFunc(scores, func(a, b scoredMatch) int { return cmp.Compare(b.score, a.score) })
	return jsSliceHead(scores, limit)
}

// add indexes one new or updated record.
func (k *keywordIndex) add(r memory.Record) {
	set := termSet(r)
	k.mu.Lock()
	k.terms.Set(r.ID, set)
	k.mu.Unlock()
}

// remove drops a record from the index.
func (k *keywordIndex) remove(id string) {
	k.mu.Lock()
	k.terms.Delete(id)
	k.mu.Unlock()
}

// size is the number of indexed records.
func (k *keywordIndex) size() int {
	k.mu.RLock()
	defer k.mu.RUnlock()
	return k.terms.Len()
}

// termSet is the term set of a record: the tokens of "value key subject tags..."
// joined with single spaces.
func termSet(r memory.Record) map[string]struct{} {
	parts := append([]string{r.Value, r.Key, r.Subject}, r.Tags...)
	set := map[string]struct{}{}
	for _, t := range tokenize(strings.Join(parts, " ")) {
		set[t] = struct{}{}
	}
	return set
}

// tokenize is the keyword tokenizer: text.toLowerCase().split(/[\s\p{P}]+/u) keeping the
// tokens whose UTF-16 length is above 1. The text semantics (JS whitespace, the
// Unicode 16 punctuation class, the Unicode 16 lowercase mapping) come from
// internal/jsstr and the length from kernel.UTF16Len, so a CJK word, an emoji pair
// or a character of the newest Unicode version tokenizes as it does in Node.
func tokenize(text string) []string {
	parts := jsstr.FieldsFunc(jsstr.ToLower(text), func(r rune) bool {
		return jsstr.IsSpace(r) || jsstr.IsPunct(r)
	})
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if kernel.UTF16Len(p) > 1 {
			out = append(out, p)
		}
	}
	return out
}

// jsSliceHead is list.slice(0, limit): a limit of zero or more keeps the first limit
// elements, a negative limit drops that many from the end.
func jsSliceHead[T any](list []T, limit int) []T {
	if limit >= 0 {
		return list[:min(limit, len(list))]
	}
	return list[:max(len(list)+limit, 0)]
}
