package profile_test

import (
	"testing"

	"github.com/Phong95/aec-agent-go/components/agent/memorysystem/memory"
	"github.com/Phong95/aec-agent-go/components/agent/profile"
	"github.com/Phong95/aec-agent-go/components/agent/subagent"
	"github.com/Phong95/aec-agent-go/components/sandboxhub"
	"github.com/Phong95/aec-agent-go/components/secrethub/secret"
	"github.com/Phong95/aec-agent-go/internal/clonecheck"
)

// fullProfile fills every field of an AgentProfile and of every sub-config.
func fullProfile() profile.AgentProfile {
	lean, maxTokens, maxIter, retries, backoff, maxResults, concurrent, keep := 30000, 4096, 25, 0, 500, 8, 4, 2
	temperature, topP, cpus, interval := 0.0, 0.9, 0.5, 60.0
	mem, pids := 512, 128
	return profile.AgentProfile{
		ID: "agent-full", Name: "Full", SystemPrompt: "p", Provider: "anthropic", LLMModel: "claude-sonnet",
		Tools:  []string{"read_file", "write_file"},
		Skills: []string{"review"},
		InitialContext: &profile.InitialContext{
			UserMd: "# User", MemoryMd: "# Memory", SoulMd: "# Soul",
		},
		Sandbox: sandboxhub.Selection{
			Mode: sandboxhub.SelectionOwn,
			Own: &sandboxhub.OwnConfig{
				Kind: sandboxhub.KindDocker, Image: "node:22", Network: sandboxhub.NetworkAllowlist,
				Limits: &sandboxhub.LimitsOverride{Cpus: &cpus, MemoryMb: &mem, Pids: &pids},
			},
		},
		Compactor: profile.CompactorConfig{
			ContextWindowTokens: 200000, Threshold: 0.75, MinTailUserMessages: 2, ProtectFirstN: 3,
			ProactivePruneTokens: 20000, OldToolResultMinTokens: 200, KeepToolResults: &keep, TailTargetRatio: 0.2,
			LeanTailTokenBudget: &lean, ProtectLastN: 20, SummarizeMaxOutputTokens: 4000,
			SummarizationModel: "claude-haiku",
		},
		InferenceConfig: &profile.InferenceConfig{
			MaxTokens: &maxTokens, Temperature: &temperature, TopP: &topP, StopSequences: []string{"END", "STOP"},
		},
		LoopConfig: &profile.LoopConfig{MaxIterations: &maxIter, MaxToolRetries: &retries, RetryBackoffMs: &backoff, MaxConcurrentTurns: &concurrent, MaxToolResultChars: &keep},
		Memory: memory.Config{
			Enabled: true, UpdateSoul: true, UpdateUser: true,
			Consolidation: &memory.ConsolidationTrigger{IntervalMinutes: &interval},
			Dreaming:      &memory.DreamingConfig{MemoryExpiryDays: 30},
			Recall:        &memory.RecallConfig{MaxResults: &maxResults},
		},
		// A-105: who the agent may delegate to.
		Subagents: &subagent.Config{
			Allow: []subagent.TargetConfig{{AgentID: "researcher", Access: subagent.AccessWrite}},
			// A-105 revision: the subagent types it may use.
			Types:         []string{"general-purpose"},
			MaxConcurrent: 2, MaxRunsPerTurn: 4, MaxTokensPerTurn: 1000, MaxToolCallsPerTurn: 10,
			MaxElapsedMsPerTurn: 60_000, ChildTimeoutMs: 30_000,
		},
		// A-101: the global secrets it may use.
		Secrets: &secret.Config{Use: []string{"WEATHER_KEY"}},
	}
}

// AgentProfile.Clone must share nothing with the original: a pointer or slice
// field added to a profile or its sub-configs without a Clone line fails here.
func TestAgentProfileCloneIsDeep(t *testing.T) {
	clonecheck.AssertDeep(t, fullProfile(), profile.AgentProfile.Clone)
}

// Nil Tools and Skills stay nil; the encoder writes them as [] either way.
func TestAgentProfileCloneKeepsNil(t *testing.T) {
	got := profile.AgentProfile{ID: "a"}.Clone()
	if got.Tools != nil || got.Skills != nil || got.InitialContext != nil || got.InferenceConfig != nil || got.LoopConfig != nil {
		t.Fatalf("nil fields must stay nil: %#v", got)
	}
}
