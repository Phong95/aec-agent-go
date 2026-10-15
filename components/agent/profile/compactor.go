package profile

// DefaultCompactorConfig returns the compactor config an agent normally wants: a
// 200k-token context window compacted at 80%, the system prompt and the first
// message protected, the last 10 messages kept, a tail of at most 20% of the
// threshold and a 4096-token summary. These are the values CreateAgent uses and
// the conformance suite's default compactor.
//
// A zero CompactorConfig is not a usable default: the agent loop treats it as
// compaction disabled. A host that builds an AgentProfile by hand starts from this
// function and changes what it needs.
func DefaultCompactorConfig() CompactorConfig {
	return CompactorConfig{
		ContextWindowTokens:      200_000,
		Threshold:                0.8,
		MinTailUserMessages:      4,
		ProtectFirstN:            2,
		ProactivePruneTokens:     50_000,
		OldToolResultMinTokens:   2_000,
		TailTargetRatio:          0.2,
		ProtectLastN:             10,
		SummarizeMaxOutputTokens: 4_096,
	}
}
