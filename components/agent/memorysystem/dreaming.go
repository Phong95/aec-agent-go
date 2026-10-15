package memorysystem

import (
	"context"
	"sync"

	"github.com/Phong95/aec-agent-go/components/agent/memorysystem/memory"
	"github.com/Phong95/aec-agent-go/components/persistence"
	"github.com/Phong95/aec-agent-go/internal/jsstr"
	"github.com/Phong95/aec-agent-go/kernel"
)

// ---------------------------------------------------------------------------
// Dreaming - the consolidation engine
//
// It reads the recent messages of a session from the MessageStore, hands them to
// Memory for the model extraction, and manages the memory lifecycle (expiry). There
// is no staging and no scoring: the model decides what is worth remembering directly
// from the conversation.
// ---------------------------------------------------------------------------

// defaultMemoryExpiryDays is the expiry, in days, of a memory that nobody touched.
const defaultMemoryExpiryDays = 30

// consolidationPromoteReason is the reason of every record the extraction stored or
// reinforced.
const consolidationPromoteReason = "LLM extracted from conversation"

// dreaming is the consolidation component of one agent.
type dreaming struct {
	deps
	messages persistence.MessageStore
	// sessions holds the per-session consolidation checkpoint
	// (Session.ConsolidatedMessageCount).
	sessions persistence.SessionStore
	store    persistence.MemoryStore
	memory   *memoryComponent

	cfgMu sync.RWMutex
	// expiryDays is the configured memoryExpiryDays; 0 means the 30 day default.
	expiryDays float64
}

// newDreaming returns the consolidation component of one agent. cfg is the profile's
// dreaming config, nil for none.
//
// DreamingConfig.MemoryExpiryDays is a plain float64: a zero value means "use the
// default" (30 days).
func newDreaming(d deps, messages persistence.MessageStore, sessions persistence.SessionStore, store persistence.MemoryStore, mem *memoryComponent, cfg *memory.DreamingConfig) *dreaming {
	dr := &dreaming{deps: d, messages: messages, sessions: sessions, store: store, memory: mem}
	if cfg != nil {
		dr.expiryDays = cfg.MemoryExpiryDays
	}
	return dr
}

// updateConfig replaces the expiry. As in newDreaming, 0 selects the default.
func (dr *dreaming) updateConfig(cfg memory.DreamingConfig) {
	dr.cfgMu.Lock()
	dr.expiryDays = cfg.MemoryExpiryDays
	dr.cfgMu.Unlock()
}

// expiryDaysNow is the effective expiry in days.
func (dr *dreaming) expiryDaysNow() float64 {
	dr.cfgMu.RLock()
	defer dr.cfgMu.RUnlock()
	if dr.expiryDays == 0 {
		return defaultMemoryExpiryDays
	}
	return dr.expiryDays
}

// consolidatedMessageCount is the number of messages of the session that a
// consolidation pass already processed (the high-water mark), read from the persisted
// session checkpoint so it survives a restart. The scheduler uses it to compute the
// pending messages without duplicating the bookkeeping.
func (dr *dreaming) consolidatedMessageCount(ctx context.Context, sessionID string) (int, error) {
	session, err := dr.sessions.Get(ctx, sessionID)
	if err != nil {
		return 0, err
	}
	if session == nil || session.ConsolidatedMessageCount == nil {
		return 0, nil
	}
	return *session.ConsolidatedMessageCount, nil
}

// consolidate runs a full consolidation pass for a session:
//
//  1. read only the messages added since the last pass (the last total-checkpoint
//     messages, ending at the total);
//  2. drop tool messages and messages carrying tool calls or results, and hand the
//     rest to Memory for the model extraction, which stores the new memories;
//  3. move the session checkpoint to the message count, after the extraction
//     succeeded: an error leaves the checkpoint where it was, so the next pass reads the
//     same messages again;
//  4. expire the agent's own stale memories.
//
// It returns one "promote" result per record the extraction stored or reinforced, then
// one "expire" result per expired record. A session with nothing new returns an empty
// result without a model call.
func (dr *dreaming) consolidate(ctx context.Context, sessionID string) ([]memory.ConsolidationResult, error) {
	results := []memory.ConsolidationResult{}

	// Phase 1: read only the messages added since the last consolidation.
	total, err := dr.messages.Count(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	last, err := dr.consolidatedMessageCount(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	if total <= last {
		return results, nil
	}
	slice, err := dr.messages.GetRange(ctx, sessionID, total, total-last)
	if err != nil {
		return nil, err
	}
	if len(slice.Messages) == 0 {
		return results, nil
	}

	// Phase 2: pass them to Memory for the model extraction. Tool calls and tool
	// results are dropped first to save tokens; the checkpoint still advances past them.
	records, err := dr.memory.evaluateMessages(ctx, conversationalMessages(slice.Messages))
	if err != nil {
		return nil, err
	}
	for _, rec := range records {
		results = append(results, memory.ConsolidationResult{
			RecordID: rec.ID,
			Action:   memory.ActionPromote,
			Reason:   consolidationPromoteReason,
		})
	}

	if err := dr.sessions.SetConsolidatedMessageCount(ctx, sessionID, total); err != nil {
		return nil, err
	}

	// Phase 3: expire stale memories.
	expired, err := dr.expireStale(ctx)
	if err != nil {
		return nil, err
	}
	return append(results, expired...), nil
}

// conversationalMessages returns the messages that are plain conversation: not a
// tool message and carrying no tool calls or tool results. Only these are worth
// sending to the memory extraction.
func conversationalMessages(msgs []kernel.Message) []kernel.Message {
	out := make([]kernel.Message, 0, len(msgs))
	for _, msg := range msgs {
		if msg.Role == kernel.RoleTool || len(msg.ToolCalls) > 0 || len(msg.ToolResults) > 0 {
			continue
		}
		out = append(out, msg)
	}
	return out
}

// expireStale marks the agent's stale active memories as expired. A record with an
// explicit expiresAt expires when that date is in the past; one with an unparsable date
// never expires. Any other record expires when its last touch, the later of
// lastRecalledAt and updatedAt (observedAt without one), is older than the expiry
// window; being recalled counts as a touch, so memories in active use stay alive.
// Timestamps are compared as ISO text, so a lexicographic compare of equal width
// strings is a chronological one.
//
// Only this agent's records are read, and the whole pass holds the write mutex, so a
// recall stamp cannot interleave with a status change.
func (dr *dreaming) expireStale(ctx context.Context) ([]memory.ConsolidationResult, error) {
	results := []memory.ConsolidationResult{}
	days := dr.expiryDaysNow()
	cutoff, ok := isoFromMs(float64(dr.opts.now().UnixMilli()) - days*24*60*60*1000)
	if !ok {
		return nil, &jsTypeError{msg: invalidTimeValue} // the RangeError of toISOString
	}

	dr.writeMu.Lock()
	defer dr.writeMu.Unlock()
	active, err := dr.store.Query(ctx, persistence.MemoryFilter{AgentID: dr.agentID, Statuses: []memory.Status{memory.StatusActive}})
	if err != nil {
		return nil, err
	}
	for _, rec := range active {
		if rec.ExpiresAt != "" {
			if jsstr.ParseDate(rec.ExpiresAt, dr.opts.loc) < float64(dr.opts.now().UnixMilli()) {
				rec.Status = memory.StatusExpired
				if err := dr.store.Update(ctx, rec); err != nil {
					return nil, err
				}
				results = append(results, memory.ConsolidationResult{RecordID: rec.ID, Action: memory.ActionExpire, Reason: "past explicit expiry date"})
			}
			continue
		}

		// Being recalled counts as a touch, so memories in active use stay alive.
		lastWrite := rec.UpdatedAt
		if lastWrite == "" {
			lastWrite = rec.ObservedAt
		}
		lastTouch := lastWrite
		if rec.LastRecalledAt != "" && rec.LastRecalledAt > lastWrite {
			lastTouch = rec.LastRecalledAt
		}
		if lastTouch < cutoff {
			rec.Status = memory.StatusExpired
			if err := dr.store.Update(ctx, rec); err != nil {
				return nil, err
			}
			results = append(results, memory.ConsolidationResult{
				RecordID: rec.ID,
				Action:   memory.ActionExpire,
				Reason:   "not updated or recalled in " + jsNumberString(days) + " days",
			})
		}
	}
	return results, nil
}
