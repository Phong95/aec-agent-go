package memory

// RecalledMemoryTrace is the per-memory trace detail of one recall: ids, labels
// and scores only, never the memory value. The span of a traced run lists these.
type RecalledMemoryTrace struct {
	ID              string  `json:"id"`
	Kind            string  `json:"kind"`
	Key             string  `json:"key"`
	CombinedScore   float64 `json:"combinedScore"`
	RelevanceScore  float64 `json:"relevanceScore"`
	RecencyScore    float64 `json:"recencyScore"`
	ImportanceScore float64 `json:"importanceScore"`
}

// The reasons a recall did not run (the values of RecalledMemories.Skipped).
const (
	// SkippedMemoryDisabled: the agent's memory system is switched off.
	SkippedMemoryDisabled = "memory-disabled"
	// SkippedRecallDisabled: recall is switched off (MaxResults is zero or less).
	SkippedRecallDisabled = "recall-disabled"
)

// RecalledMemories is the outcome of one turn's recall: what to inject into the
// model context, plus the statistics the trace records. The agent loop asks for it
// once per run, through its Memory port.
//
// The json tags follow the runtime key order of the recall result, which is the order
// the trace shows. RecordIDs and Memories are arrays on the wire, so a producer sets
// them to empty non-nil slices when nothing was recalled; a nil slice would encode as
// null.
type RecalledMemories struct {
	// Content is the system prompt section to append to the model context. Nil
	// (null on the wire) when there is nothing to inject.
	Content *string `json:"content"`
	// RecordIDs are the ids of the recalled records, best first.
	RecordIDs []string `json:"recordIds"`
	// Memories is the trace detail of each recalled record, in the same order.
	Memories []RecalledMemoryTrace `json:"memories"`
	// MaxResults is the most memories this recall may return.
	MaxResults int `json:"maxResults"`
	// IndexRebuilt is true when this call had to build the in-memory index first (a
	// cold start).
	IndexRebuilt bool `json:"indexRebuilt"`
	// Skipped says why recall did not run (SkippedMemoryDisabled or
	// SkippedRecallDisabled); empty when it ran.
	Skipped string `json:"skipped,omitempty"`
	// IndexRebuildMs is how long the index rebuild took. Set only when IndexRebuilt.
	IndexRebuildMs *float64 `json:"indexRebuildMs,omitempty"`
	// IndexedCount is the number of active memories loaded into the index. Set only
	// when IndexRebuilt.
	IndexedCount *int `json:"indexedCount,omitempty"`
}
