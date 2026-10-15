package memory

// ---------------------------------------------------------------------------
// Memory pipeline data types: consolidation (dreaming), retrieval and learning.
// Data only - the behaviour arrives with the memory system in a later phase.
// ---------------------------------------------------------------------------

// ---------------------------------------------------------------------------
// Dreaming / Consolidation
// ---------------------------------------------------------------------------

// ConsolidationAction is what the dreaming pass decided for one record.
type ConsolidationAction string

// The consolidation actions.
const (
	ActionKeep      ConsolidationAction = "keep"
	ActionMerge     ConsolidationAction = "merge"
	ActionSupersede ConsolidationAction = "supersede"
	ActionExpire    ConsolidationAction = "expire"
	ActionDelete    ConsolidationAction = "delete"
	ActionPromote   ConsolidationAction = "promote"
)

// ConsolidationResult is the outcome of consolidating one record.
type ConsolidationResult struct {
	RecordID     string              `json:"recordId"`
	Action       ConsolidationAction `json:"action"`
	Reason       string              `json:"reason"`
	MergedWith   string              `json:"mergedWith,omitempty"`
	SupersededBy string              `json:"supersededBy,omitempty"`
}

// ---------------------------------------------------------------------------
// Retrieval - relevance-ranked recall
// ---------------------------------------------------------------------------

// RetrievalQuery asks for the memories relevant to the current context.
type RetrievalQuery struct {
	AgentID        string `json:"agentId"`
	SessionID      string `json:"sessionId"`
	CurrentContext string `json:"currentContext"`
	Scope          Scope  `json:"scope,omitempty"`
	Kinds          []Kind `json:"kinds,omitzero"`
	// MaxResults is a pointer because an explicit 0 is meaningful.
	MaxResults *int `json:"maxResults,omitempty"`
}

// RetrievalResult is one recalled record with its scores.
type RetrievalResult struct {
	Record          Record  `json:"record"`
	RelevanceScore  float64 `json:"relevanceScore"`
	RecencyScore    float64 `json:"recencyScore"`
	ImportanceScore float64 `json:"importanceScore"`
	CombinedScore   float64 `json:"combinedScore"`
}

// ---------------------------------------------------------------------------
// Learning - feedback signals from memory usage
// ---------------------------------------------------------------------------

// FeedbackSignalType says what happened to a recalled memory.
type FeedbackSignalType string

// The feedback signal types.
const (
	SignalAccepted       FeedbackSignalType = "accepted"
	SignalCorrected      FeedbackSignalType = "corrected"
	SignalCited          FeedbackSignalType = "cited"
	SignalTaskSucceeded  FeedbackSignalType = "task_succeeded"
	SignalTaskFailed     FeedbackSignalType = "task_failed"
	SignalNeverRetrieved FeedbackSignalType = "never_retrieved"
	SignalContradicted   FeedbackSignalType = "contradicted"
)

// FeedbackSignal is one feedback signal about a memory.
type FeedbackSignal struct {
	MemoryID   string             `json:"memoryId"`
	SignalType FeedbackSignalType `json:"signalType"`
	SessionID  string             `json:"sessionId"`
	Timestamp  string             `json:"timestamp"`
	Detail     string             `json:"detail,omitempty"`
}
