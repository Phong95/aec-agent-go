package agentloop

import (
	"context"

	"github.com/Phong95/aec-agent-go/kernel"
)

// ContextCategoryKey names a category of a ContextUsage (A-104).
type ContextCategoryKey string

// The categories of a ContextUsage. Every one is an estimate (the session-calibrated
// Counter) except ContextOther: the part of the provider's reported total the
// estimates do not cover.
const (
	ContextSystemPrompt ContextCategoryKey = "system_prompt"
	ContextMemoryFiles  ContextCategoryKey = "memory_files"
	ContextSkills       ContextCategoryKey = "skills"
	ContextAgents       ContextCategoryKey = "agents"
	ContextSecrets      ContextCategoryKey = "secrets"
	ContextTools        ContextCategoryKey = "tools"
	ContextMemories     ContextCategoryKey = "memories"
	ContextMessages     ContextCategoryKey = "messages"
	ContextSummary      ContextCategoryKey = "summary"
	ContextOther        ContextCategoryKey = "other"
)

// ContextCategoryKeys returns the categories in the order a ContextUsage always
// lists them. The slice is fresh on every call.
func ContextCategoryKeys() []ContextCategoryKey {
	return []ContextCategoryKey{
		ContextSystemPrompt, ContextMemoryFiles, ContextSkills, ContextAgents, ContextSecrets,
		ContextTools, ContextMemories, ContextMessages, ContextSummary, ContextOther,
	}
}

// ContextCategory is the tokens of one category.
type ContextCategory struct {
	Key    ContextCategoryKey `json:"key"`
	Tokens int                `json:"tokens"`
}

// ContextToolTokens is the estimated tokens of one tool definition sent.
type ContextToolTokens struct {
	Name   string `json:"name"`
	Tokens int    `json:"tokens"`
}

// The UsedSource values of a ContextUsage.
const (
	UsedReported  = "reported"
	UsedEstimated = "estimated"
)

// ContextUsage (A-104) is what fills a session's context window: the window, where
// compaction starts, the tokens used and free, and the tokens of each category.
type ContextUsage struct {
	SessionID string `json:"sessionId"`
	// Model is the model the context was measured for, empty when unknown.
	Model string `json:"model,omitempty"`
	// ContextWindowTokens is the window the session plans around (its override, else
	// the agent's compactor).
	ContextWindowTokens int `json:"contextWindowTokens"`
	// CompactAtTokens is where compaction starts: ContextWindowTokens × Threshold.
	CompactAtTokens int `json:"compactAtTokens"`
	// UsedTokens is the reported context size (input + output of the response) when
	// the provider gave one, else the estimate.
	UsedTokens int `json:"usedTokens"`
	// UsedSource is UsedReported or UsedEstimated.
	UsedSource string `json:"usedSource"`
	// FreeTokens is ContextWindowTokens − UsedTokens, at least 0.
	FreeTokens int `json:"freeTokens"`
	// Categories holds every category in ContextCategoryKeys order, zero-token ones
	// included.
	Categories []ContextCategory `json:"categories"`
	// Tools holds the estimated tokens of each tool definition sent, in the order sent.
	Tools []ContextToolTokens `json:"tools"`
	// Iteration is the loop iteration it was measured after; 0 when measured on demand.
	Iteration int `json:"iteration,omitempty"`
}

// ContextSectionKey names a labelled part of the always-loaded system message.
type ContextSectionKey = ContextCategoryKey

// ContextSection is a labelled part of the always-loaded system message (A-104):
// ContextSystemPrompt, ContextMemoryFiles, ContextSkills, ContextAgents or
// ContextSecrets.
type ContextSection struct {
	Key  ContextSectionKey `json:"key"`
	Text string            `json:"text"`
}

// SectionedContext is the optional side of Context (A-104): Prepare is
// PrepareMessages plus the labelled sections the system message was built from, which
// the loop measures for the context breakdown. Without it the whole system message
// counts as ContextSystemPrompt.
type SectionedContext interface {
	Prepare(ctx context.Context, msgs []kernel.Message, mc MiddlewareContext) ([]kernel.Message, []ContextSection, error)
}
