// Package profile is the data owner for the AgentProfile contract.
//
// Phase 4 ships the type only, so that the profile golden fixtures can be
// built without a layering violation; spawning and behaviour arrive with the
// agent phases. It imports only the data leaves sandbox and memory, and the
// shared JSON encoder; it never imports the root aec package.
//
// It holds the AgentProfile and the CompactorConfig of the agent loop.
package profile

import (
	"github.com/Phong95/aec-agent-go/components/agent/memorysystem/memory"
	"github.com/Phong95/aec-agent-go/components/agent/skilllearning/learning"
	"github.com/Phong95/aec-agent-go/components/agent/subagent"
	"github.com/Phong95/aec-agent-go/components/sandboxhub"
	"github.com/Phong95/aec-agent-go/components/secrethub/secret"
	"github.com/Phong95/aec-agent-go/internal/jsonx"
)

// InitialContext is the optional seed context of an agent.
type InitialContext struct {
	UserMd   string `json:"userMd,omitempty"`
	MemoryMd string `json:"memoryMd,omitempty"`
	SoulMd   string `json:"soulMd,omitempty"`
}

// CompactorConfig configures the conversation compactor.
type CompactorConfig struct {
	ContextWindowTokens int     `json:"contextWindowTokens"`
	Threshold           float64 `json:"threshold"`
	MinTailUserMessages int     `json:"minTailUserMessages"`
	ProtectFirstN       int     `json:"protectFirstN"`
	// ProactivePruneTokens: proactive prune of tool results; in some cases a tool
	// result is too large, so it can be pruned early, before the threshold is hit.
	ProactivePruneTokens int `json:"proactivePruneTokens"`
	// OldToolResultMinTokens is how many tokens are kept for an old tool result.
	OldToolResultMinTokens int `json:"oldToolResultMinTokens"`
	// KeepToolResults (A-129) is how many of the latest tool results before the current
	// turn the proactive clearing (ProactivePruneTokens) never clears. Nil means 3. A
	// pointer because zero is meaningful.
	KeepToolResults *int `json:"keepToolResults,omitempty"`
	// TailTargetRatio is the ratio of the tail kept for the current conversation.
	// For example, if the conversation took 500k tokens and this value is 0.2
	// (20%), the tail takes 500k * 0.2 = 100k tokens. ProtectLastN and
	// TailTargetRatio are two strategies to calculate how much tail to keep: if
	// ProtectLastN messages are larger than the ratio, the ratio wins; if they
	// are smaller, more messages are added.
	TailTargetRatio float64 `json:"tailTargetRatio"`
	// LeanTailTokenBudget has the same role as TailTargetRatio; set it when an
	// exact number is wanted. A pointer because zero is meaningful.
	LeanTailTokenBudget *int `json:"leanTailTokenBudget,omitempty"`
	ProtectLastN        int  `json:"protectLastN"`

	SummarizeMaxOutputTokens int `json:"summarizeMaxOutputTokens"`
	// SummarizationModel is an optional model override for summarization (for
	// example a cheaper or faster model). Falls back to the agent's default model
	// when unset.
	SummarizationModel string `json:"summarizationModel,omitempty"`
}

// InferenceConfig holds the per-agent default inference parameters: the base
// layer below per-session overrides. Every field is optional and zero is
// meaningful (for example temperature 0), so scalars are pointers.
type InferenceConfig struct {
	MaxTokens     *int     `json:"maxTokens,omitempty"`
	Temperature   *float64 `json:"temperature,omitempty"`
	TopP          *float64 `json:"topP,omitempty"`
	StopSequences []string `json:"stopSequences,omitzero"`
}

// LoopConfig holds the per-agent agent-loop limits. Omitted fields fall back to
// the SDK defaults (MAX_ITERATIONS, MAX_TOOL_RETRIES, RETRY_BACKOFF_MS, and
// MAX_CONCURRENT_TURNS since A-97). Zero is meaningful, so every field is a pointer.
type LoopConfig struct {
	// MaxIterations is the max model/tool round-trips per turn (integer >= 1).
	MaxIterations *int `json:"maxIterations,omitempty"`
	// MaxToolRetries is the retries per failing tool/skill call, so total
	// attempts = retries + 1 (integer >= 0).
	MaxToolRetries *int `json:"maxToolRetries,omitempty"`
	// RetryBackoffMs is the base delay of the exponential backoff,
	// delay = base x 2^attempt (integer >= 0).
	RetryBackoffMs *int `json:"retryBackoffMs,omitempty"`
	// MaxConcurrentTurns (A-97) is the most turns this agent runs at once across its
	// sessions (integer >= 0; 0 or omitted = no cap). Turns of one session always run
	// one at a time.
	MaxConcurrentTurns *int `json:"maxConcurrentTurns,omitempty"`
	// MaxToolResultChars (A-129) is the most characters of one tool result the model is
	// given (integer >= 0; 0 = no limit; default 50 000). A longer result keeps its head
	// and tail, and in an isolated sandbox the full output is saved to a file the model
	// can read.
	MaxToolResultChars *int `json:"maxToolResultChars,omitempty"`
}

// AgentProfile is the configuration for a spawnable agent instance.
type AgentProfile struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	SystemPrompt string `json:"systemPrompt"`
	Provider     string `json:"provider,omitempty"`
	LLMModel     string `json:"llmModel,omitempty"`
	// Tools and Skills are required collections: nil encodes as [].
	Tools          []string        `json:"tools"`
	Skills         []string        `json:"skills"`
	InitialContext *InitialContext `json:"initialContext,omitempty"`
	// Sandbox is where file/terminal tools run. Omitted (the zero Selection) =
	// the shared sandbox (default); "none" = no sandbox; own = a dedicated
	// sandbox for this agent.
	Sandbox         sandboxhub.Selection `json:"sandbox,omitzero"`
	Compactor       CompactorConfig      `json:"compactor"`
	InferenceConfig *InferenceConfig     `json:"inferenceConfig,omitempty"`
	LoopConfig      *LoopConfig          `json:"loopConfig,omitempty"`
	Memory          memory.Config        `json:"memory"`
	// SkillLearning (A-166) turns a hard-won success the user confirmed into a skill (a
	// draft for review, or saved at once in auto mode). Nil = off.
	SkillLearning *learning.Config `json:"skillLearning,omitempty"`
	// Subagents (A-105) is the agents this one may hand bounded tasks to through
	// spawn_subagent, and the budget all children of one of its turns share. Nil = no
	// subagents.
	Subagents *subagent.Config `json:"subagents,omitempty"`
	// Secrets (A-101) is the global secrets this agent may use (it can always use its
	// own). Nil = only its own.
	Secrets *secret.Config `json:"secrets,omitempty"`
}

// MarshalJSON encodes the profile in its stable wire form. Nil Tools and Skills
// are replaced by empty slices because the fields are required arrays. It has
// a value receiver so both AgentProfile and *AgentProfile encode correctly.
func (p AgentProfile) MarshalJSON() ([]byte, error) {
	type alias AgentProfile // drops the methods, so this does not recurse
	a := alias(p)
	if a.Tools == nil {
		a.Tools = []string{}
	}
	if a.Skills == nil {
		a.Skills = []string{}
	}
	return jsonx.Marshal(a)
}

// ResolvedLoopConfig is the agent-loop limits fully resolved for a single run or
// dispatch: no optional fields. It is a runtime value, not wire data, so its fields
// carry no JSON tags.
type ResolvedLoopConfig struct {
	MaxIterations  int
	MaxToolRetries int
	RetryBackoffMs int
	// MaxConcurrentTurns (A-97) is the most turns one agent runs at once across its
	// sessions; 0 means no cap.
	MaxConcurrentTurns int
	// MaxToolResultChars (A-129) is the most characters of one tool result the model is
	// given; 0 means no limit.
	MaxToolResultChars int
}
