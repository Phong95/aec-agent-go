// Package memory is the data owner for the memory contracts of the SDK.
//
// Phase 4 holds only the subset that an AgentProfile needs: the memory
// configuration. DreamingConfig lives here, next to the dreaming behaviour's
// other contracts, because Config references it (architecture cut 5); the Dreaming
// behaviour itself arrives with the memory phase.
//
// Phase 6 adds the memory record and the pipeline data types (types_record.go,
// types_pipeline.go): Record and its enums, and the consolidation, retrieval and
// feedback shapes. The persistence package needs them for the MemoryStore
// contract. They are data only; the consolidation behaviour stays with the
// memory system phase.
//
// Phase 9 adds the recall result (types_recall.go): RecalledMemories and
// RecalledMemoryTrace, which the agent loop's Memory port returns once per run and
// records on its trace.
//
// It holds the memory configuration (types.go), the record (types_record.go), the
// pipeline shapes (types_pipeline.go) and the recall result (types_recall.go).
package memory

// ConsolidationTrigger says when to auto-trigger consolidation.
type ConsolidationTrigger struct {
	// IntervalMinutes runs consolidation on a recurring interval in minutes
	// (0 = disabled). Each tick consolidates every session of the agent that has
	// unprocessed messages. A pointer because zero is meaningful.
	IntervalMinutes *float64 `json:"intervalMinutes,omitempty"`
}

// RecallConfig says how recalled memories are injected into the agent loop.
type RecallConfig struct {
	// MaxResults is the max memories injected into the model context per turn
	// (default 8, 0 = recall disabled). A pointer because zero is meaningful.
	MaxResults *int `json:"maxResults,omitempty"`
}

// DreamingConfig configures the dreaming pass.
type DreamingConfig struct {
	// MemoryExpiryDays: memories not updated in this many days get expired.
	MemoryExpiryDays float64 `json:"memoryExpiryDays"`
}

// Config is the per-agent memory configuration.
type Config struct {
	// Enabled turns the memory system on or off.
	Enabled bool `json:"enabled"`
	// UpdateSoul updates the soul over time or not (in some cases the user wants it fixed).
	UpdateSoul bool `json:"updateSoul"`
	// UpdateUser updates the user over time or not (in some cases the user wants it fixed).
	UpdateUser bool `json:"updateUser"`
	// Consolidation is when to auto-trigger consolidation (dreaming pass).
	Consolidation *ConsolidationTrigger `json:"consolidation,omitempty"`
	Dreaming      *DreamingConfig       `json:"dreaming,omitempty"`
	// Recall is how recalled memories are injected into the agent loop.
	Recall *RecallConfig `json:"recall,omitempty"`
}
