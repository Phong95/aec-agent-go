package agentloop

import (
	"context"

	"github.com/Phong95/aec-agent-go/components/agent/memorysystem/memory"
	"github.com/Phong95/aec-agent-go/components/trace"
	"github.com/Phong95/aec-agent-go/kernel"
)

// recallMemories asks the memory system for the memories that fit this turn, once
// per run. It returns nil when the agent has no memory system
// (no span is started then) and when the recall fails: a recall failure must never
// block the turn, so it is recorded on the span and logged, and the run goes on
// without memories.
//
// The recall is traced as a memory-recall span, a child of the run span, so recall
// latency, hit rate, scores and the injected size can be measured per turn. The
// span records ids, labels and scores, never the memory text (the context-assembly
// span does carry the injected memory block, so a trace sink receives
// prompts and memory content). The warning on failure carries the agent id, the
// session id and the error text only: a memory error can name a record, and the
// recalled content is never logged.
func (l *Loop) recallMemories(ctx context.Context, r *runState, userMessage string) *memory.RecalledMemories {
	if l.deps.Memory == nil {
		return nil
	}
	span := r.trace.span("memory-recall", trace.KindMemoryRecall, obj(
		"queryLength", kernel.UTF16Len(userMessage),
		"queryPreview", userMessage,
	))
	// Raced against ctx, and a panic of the memory system (a third-party port) is
	// recovered by raceAbort into an error, so a recall never fails or delays the
	// turn. The abandoned call of a stopped run is dropped; the loop's first safe
	// point ends the run right after.
	recalled, stopped, err := raceAbort(ctx, func() (memory.RecalledMemories, error) {
		return l.deps.Memory.RecallForTurn(ctx, userMessage, r.sessionID)
	})
	if stopped {
		span.End(obj("aborted", true))
		return nil
	}
	if err != nil {
		span.End(obj("error", err.Error()))
		l.logger.Warn("agentloop: memory recall failed",
			"agentId", r.agentID, "sessionId", r.sessionID, "error", err.Error())
		return nil
	}
	recalled = recalled.Clone()

	content := ""
	if recalled.Content != nil {
		content = *recalled.Content
	}
	end := obj(
		"recalledCount", len(recalled.RecordIDs),
		"maxResults", recalled.MaxResults,
		"hit", len(recalled.RecordIDs) > 0,
	)
	if recalled.Skipped != "" {
		end.Set("skipped", recalled.Skipped)
	}
	end.Set("indexRebuilt", recalled.IndexRebuilt)
	if recalled.IndexRebuildMs != nil {
		end.Set("indexRebuildMs", *recalled.IndexRebuildMs)
	}
	if recalled.IndexedCount != nil {
		end.Set("indexedCount", *recalled.IndexedCount)
	}
	injectedTokens := 0
	if content != "" {
		injectedTokens = r.counter.countFresh(kernel.Message{Role: kernel.RoleSystem, Content: content})
	}
	end.Set("injectedChars", kernel.UTF16Len(content))
	end.Set("injectedTokens", injectedTokens)
	if len(recalled.Memories) > 0 {
		end.Set("topScore", recalled.Memories[0].CombinedScore)
	}
	memories := recalled.Memories
	if memories == nil {
		memories = []memory.RecalledMemoryTrace{} // an array on the wire, never null
	}
	end.Set("memories", memories)
	span.End(end)
	return &recalled
}
