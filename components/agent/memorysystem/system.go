package memorysystem

import (
	"context"
	"errors"
	"math"
	"sync"
	"time"

	"github.com/Phong95/aec-agent-go/components/agent/memorysystem/memory"
	"github.com/Phong95/aec-agent-go/components/events"
	"github.com/Phong95/aec-agent-go/components/persistence"
)

// ---------------------------------------------------------------------------
// System - the memory facade of one agent
//
// It composes the four pipeline components and the consolidation scheduler: Start and
// Dispose drive the scheduler, Consolidate runs one Dreaming pass followed by an index
// rebuild, Recall and RecallForTurn rank the agent's memories for a query, and
// UpdateConfig swaps the config at runtime and re-arms the scheduler.
// ---------------------------------------------------------------------------

// defaultRecallMaxResults is the number of memories a turn recalls when the profile
// does not say.
const defaultRecallMaxResults = 8

// maxIntervalMs is the longest timer interval in milliseconds: 2^31-1 ms, the largest
// delay a signed 32-bit millisecond timer holds, so the interval is capped there.
const maxIntervalMs = 2_147_483_647

// Deps is what a System needs from the agent that owns it.
type Deps struct {
	// AgentID is the owning agent: every record the System writes carries it, every
	// read is filtered by it, and the scheduler reacts only to the events that carry
	// it.
	AgentID string
	// Persistence is the persistence hub the agent captured when it was spawned
	// (it is captured at construction, so a later hub swap does not reach a live
	// agent). The Sessions, Messages and Memory stores are used.
	Persistence *persistence.PersistenceHub
	// Events is the bus on which the finished turns of the agent are published. The
	// scheduler subscribes to it in Start.
	Events events.Bus
	// Model is the agent's model access (its ModelManager).
	Model Model
	// MemoryFiles returns the agent's current USER.md, MEMORY.md and SOUL.md, read
	// before each compaction. Optional: nil means all are empty.
	MemoryFiles func() CompactedMemoryFiles
	// OnMemoryFilesCompacted is called after a consolidation pass rebuilt USER.md,
	// MEMORY.md and/or SOUL.md, with only the rebuilt files set; the owner (the Agent) applies
	// and saves them. Optional: nil means no compaction runs.
	OnMemoryFilesCompacted func(ctx context.Context, files CompactedMemoryFiles) error
}

// CompactedMemoryFiles is the initial context files a consolidation pass compacts. A
// nil field is a file that is not set (or, in an update, not rebuilt).
type CompactedMemoryFiles struct {
	UserMd   *string
	MemoryMd *string
	SoulMd   *string
}

// System is the memory system of one agent. Its methods are safe for concurrent use.
//
// The consolidation scheduler listens on the event bus from Start on, so a System
// that was started MUST be disposed: Dispose unsubscribes, stops the scheduler's
// goroutine and waits for a pass that is running. A System that is never started
// has no goroutine and nothing to dispose.
type System struct {
	agentID string
	opts    options

	memoryFiles            func() CompactedMemoryFiles
	onMemoryFilesCompacted func(ctx context.Context, files CompactedMemoryFiles) error

	memory    *memoryComponent
	dreaming  *dreaming
	indexing  *indexing
	retrieval *retrieval
	sched     *scheduler

	cfgMu sync.RWMutex
	cfg   memory.Config

	// indexMu makes the lazy index build single-flight and guards indexBuilt. The
	// keyword index is in memory only: it is empty after a restart until the first
	// rebuild, so Recall builds it lazily and memories persisted before the restart
	// are recallable without waiting for a consolidation pass.
	indexMu    sync.Mutex
	indexBuilt bool
}

// New returns the memory system of one agent. cfg is the profile's memory config; it
// is deep-cloned, so the caller's value is never aliased. The options pin the clock,
// the id function, the logger and the location (see Option).
func New(deps Deps, cfg memory.Config, opts ...Option) (*System, error) {
	switch {
	case deps.AgentID == "":
		return nil, errors.New("memorysystem: Deps.AgentID is required")
	case deps.Persistence == nil:
		return nil, errors.New("memorysystem: Deps.Persistence is required")
	case deps.Persistence.Sessions == nil || deps.Persistence.Messages == nil || deps.Persistence.Memory == nil:
		return nil, errors.New("memorysystem: Deps.Persistence needs the Sessions, Messages and Memory stores")
	case deps.Events == nil:
		return nil, errors.New("memorysystem: Deps.Events is required")
	case deps.Model == nil:
		return nil, errors.New("memorysystem: Deps.Model is required")
	}

	cfg = cfg.Clone()
	d := newPipelineDeps(deps.AgentID, newOptions(opts...))
	hub := deps.Persistence

	s := &System{
		agentID: deps.AgentID, opts: d.opts, cfg: cfg,
		memoryFiles: deps.MemoryFiles, onMemoryFilesCompacted: deps.OnMemoryFilesCompacted,
	}
	s.memory = newMemory(d, deps.Model, hub.Memory)
	s.dreaming = newDreaming(d, hub.Messages, hub.Sessions, hub.Memory, s.memory, cfg.Dreaming)
	s.indexing = newIndexing(deps.AgentID, hub.Memory)
	s.retrieval = newRetrieval(d, hub.Memory, s.indexing)
	s.sched = newScheduler(schedulerDeps{
		agentID:  deps.AgentID,
		bus:      deps.Events,
		messages: hub.Messages,
		logger:   d.opts.logger,
		config:   s.config,
		checkpoint: func(ctx context.Context, sessionID string) (int, error) {
			return s.dreaming.consolidatedMessageCount(ctx, sessionID)
		},
		consolidate: s.Consolidate,
		passTimeout: d.opts.passTimeout,
	})
	return s, nil
}

// newPipelineDeps builds the shared wiring of one agent's pipeline: the agent id, the resolved
// options and the one write mutex.
func newPipelineDeps(agentID string, o options) deps {
	return deps{agentID: agentID, opts: o, writeMu: &sync.Mutex{}}
}

// config returns the current memory config. The returned value is a shallow copy of a
// config that is never mutated after it was stored (UpdateConfig replaces it
// wholesale), so reading its fields and pointers is safe.
func (s *System) config() memory.Config {
	s.cfgMu.RLock()
	defer s.cfgMu.RUnlock()
	return s.cfg
}

// Start starts the trigger-driven consolidation: it subscribes to the finished turns
// of the agent on the event bus and, while the consolidation interval is above zero,
// starts the scheduler's goroutine.
//
// lifetime is the context of every background pass. It should outlive Dispose, which
// waits for a pass that is running: cancel it only after Dispose returned. Passes
// are not cancelled by Dispose. A nil lifetime is replaced by context.Background(),
// so a mistake cannot make every pass panic on a nil context for ever. Start has an effect only once, and none after
// Dispose.
//
// The subscription is made whatever the interval is: a turn that finishes while the
// interval is zero is remembered and consolidated after UpdateConfig enables the
// interval (re-arming on a config update depends on it).
func (s *System) Start(lifetime context.Context) { //nolint:contextcheck // a nil lifetime is replaced by Background, as the other tolerant entry points do
	if lifetime == nil {
		lifetime = context.Background()
	}
	s.sched.start(lifetime)
}

// Dispose stops the triggers and waits for a pass that is running. It never starts a
// new pass: the queued sessions are dropped. When ctx ends before the pass finished,
// Dispose returns the ctx error and the pass keeps running under the lifetime context
// handed to Start until it ends by itself; cancel that context to cut it short.
//
// Dispose is mandatory for a started System: without it the scheduler's goroutine
// and the bus subscriptions live as long as the process. It is safe to call more
// than once.
func (s *System) Dispose(ctx context.Context) error {
	return s.sched.stop(ctx)
}

// Consolidate runs one consolidation pass for the session: Dreaming reads the new
// messages, extracts and stores the memories and expires stale ones, then the index is
// rebuilt. It returns an empty slice when memory is disabled.
func (s *System) Consolidate(ctx context.Context, sessionID string) ([]memory.ConsolidationResult, error) {
	if !s.config().Enabled {
		return []memory.ConsolidationResult{}, nil
	}
	results, err := s.dreaming.consolidate(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	// Rebuild the index after consolidation (promotions, expirations, merges).
	if _, err := s.indexing.rebuild(ctx); err != nil {
		return nil, err
	}
	s.indexMu.Lock()
	s.indexBuilt = true
	s.indexMu.Unlock()
	// Saved memories changed: rebuild the compacted files from them.
	if len(results) > 0 {
		if err := s.compactMemoryFiles(ctx); err != nil {
			return nil, err
		}
	}
	return results, nil
}

// compactMemoryFiles runs the two actions after a consolidation pass: update the
// compacted user memory (USER.md, only when Config.UpdateUser allows it) and update the
// compacted memory (MEMORY.md), then hands the rebuilt files to OnMemoryFilesCompacted.
// Update: a third action refines the soul (SOUL.md, only when Config.UpdateSoul allows it).
//
// A failed compaction is logged, not returned: the pass itself succeeded and its
// checkpoint already advanced, so failing here would only hide its results. The error
// of OnMemoryFilesCompacted is returned.
func (s *System) compactMemoryFiles(ctx context.Context) error {
	if s.onMemoryFilesCompacted == nil {
		return nil
	}
	var current CompactedMemoryFiles
	if s.memoryFiles != nil {
		current = s.memoryFiles()
	}
	var files CompactedMemoryFiles

	if s.config().UpdateUser {
		if text, ok, err := s.memory.compactUserMemory(ctx, deref(current.UserMd)); err != nil {
			s.opts.logger.Error("memory: USER.md compaction failed", "agent", s.agentID, "err", err)
		} else if ok && text != deref(current.UserMd) {
			files.UserMd = &text
		}
	}
	if text, ok, err := s.memory.compactMemory(ctx, deref(current.MemoryMd)); err != nil {
		s.opts.logger.Error("memory: MEMORY.md compaction failed", "agent", s.agentID, "err", err)
	} else if ok && text != deref(current.MemoryMd) {
		files.MemoryMd = &text
	}
	if s.config().UpdateSoul {
		if text, ok, err := s.memory.compactSoul(ctx, deref(current.SoulMd)); err != nil {
			s.opts.logger.Error("memory: SOUL.md compaction failed", "agent", s.agentID, "err", err)
		} else if ok && text != deref(current.SoulMd) {
			files.SoulMd = &text
		}
	}

	if files.UserMd == nil && files.MemoryMd == nil && files.SoulMd == nil {
		return nil
	}
	return s.onMemoryFilesCompacted(ctx, files)
}

// deref is *p, or "" for nil.
func deref(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// Recall returns the memories relevant to the query, best first. It returns an empty
// slice when memory is disabled.
func (s *System) Recall(ctx context.Context, q memory.RetrievalQuery) ([]memory.RetrievalResult, error) {
	if !s.config().Enabled {
		return []memory.RetrievalResult{}, nil
	}
	if _, err := s.ensureIndex(ctx); err != nil {
		return nil, err
	}
	return s.retrieval.recall(ctx, q)
}

// indexRebuild is the stats of a lazy index build.
type indexRebuild struct {
	durationMs float64
	count      int
}

// ensureIndex builds the index when it is still cold and returns the stats of the
// build, or nil when it was already built. It is single-flight: concurrent callers
// queue on the mutex, so exactly one of them sees the rebuild and reports indexRebuilt.
// The duration is measured on the pipeline clock and rounded to whole milliseconds.
func (s *System) ensureIndex(ctx context.Context) (*indexRebuild, error) {
	s.indexMu.Lock()
	defer s.indexMu.Unlock()
	if s.indexBuilt {
		return nil, nil
	}
	started := s.opts.now()
	count, err := s.indexing.rebuild(ctx)
	if err != nil {
		return nil, err
	}
	s.indexBuilt = true
	elapsed := float64(s.opts.now().Sub(started)) / float64(time.Millisecond)
	return &indexRebuild{durationMs: math.Round(elapsed), count: count}, nil
}

// RecallForTurn recalls, for one agent-loop turn, the memories relevant to the user's
// message, formatted as a system-prompt section. Content is nil when memory or recall
// is disabled or nothing relevant was found; the other fields are the stats for the
// loop's memory-recall trace span. It satisfies the agentloop.Memory port.
func (s *System) RecallForTurn(ctx context.Context, userMessage, sessionID string) (memory.RecalledMemories, error) {
	cfg := s.config()
	maxResults := defaultRecallMaxResults
	if cfg.Recall != nil && cfg.Recall.MaxResults != nil {
		maxResults = *cfg.Recall.MaxResults
	}
	out := memory.RecalledMemories{
		RecordIDs:  []string{},
		Memories:   []memory.RecalledMemoryTrace{},
		MaxResults: maxResults,
	}
	if !cfg.Enabled {
		out.Skipped = memory.SkippedMemoryDisabled
		return out, nil
	}
	if maxResults <= 0 {
		out.Skipped = memory.SkippedRecallDisabled
		return out, nil
	}

	rebuild, err := s.ensureIndex(ctx)
	if err != nil {
		return memory.RecalledMemories{}, err
	}
	results, err := s.retrieval.recall(ctx, memory.RetrievalQuery{
		AgentID:        s.agentID,
		SessionID:      sessionID,
		CurrentContext: userMessage,
		MaxResults:     &maxResults,
	})
	if err != nil {
		return memory.RecalledMemories{}, err
	}

	if len(results) > 0 {
		content := formatRecalledMemories(results)
		out.Content = &content
	}
	for _, r := range results {
		out.RecordIDs = append(out.RecordIDs, r.Record.ID)
		out.Memories = append(out.Memories, memory.RecalledMemoryTrace{
			ID:              r.Record.ID,
			Kind:            string(r.Record.Kind),
			Key:             r.Record.Key,
			CombinedScore:   finiteOrZero(r.CombinedScore),
			RelevanceScore:  finiteOrZero(r.RelevanceScore),
			RecencyScore:    finiteOrZero(r.RecencyScore),
			ImportanceScore: finiteOrZero(r.ImportanceScore),
		})
	}
	if rebuild != nil {
		out.IndexRebuilt = true
		out.IndexRebuildMs = &rebuild.durationMs
		out.IndexedCount = &rebuild.count
	}
	return out, nil
}

// finiteOrZero keeps a score that is a finite number and turns NaN and the infinities
// into 0. A record whose timestamp does not parse has a NaN recency score (as in JS,
// where JSON.stringify writes null for it), and a NaN in the typed float fields of the
// recall trace makes the span unencodable (WR-B04). Only the trace is sanitised:
// Recall still returns the raw scores.
func finiteOrZero(v float64) float64 {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return 0
	}
	return v
}

// UpdateConfig replaces the memory config at runtime, merges the dreaming config into
// the consolidation component and re-arms the scheduler: an interval that became
// positive starts the scheduler's goroutine, a changed one restarts its period and a
// disabled one stops it. The config is deep-cloned.
func (s *System) UpdateConfig(cfg memory.Config) {
	cfg = cfg.Clone()
	s.cfgMu.Lock()
	s.cfg = cfg
	s.cfgMu.Unlock()
	if cfg.Dreaming != nil {
		s.dreaming.updateConfig(*cfg.Dreaming)
	}
	s.sched.rearm()
}

// IntervalOf is the effective consolidation interval of a config, zero meaning
// disabled. Memory disabled, no trigger or no interval, and an interval that is NaN,
// infinite, zero or negative are all disabled. Any other interval is clamped to at
// least one minute and at most 2^31-1 ms: a sub-minute value fires after 60 s and an
// overflow-sized value after 2147483647 ms.
//
// The result is a whole number of milliseconds. The cap times a million nanoseconds is
// about 2.1e15, far below the 9.2e18 that a time.Duration holds, so the conversion
// cannot overflow.
func IntervalOf(cfg memory.Config) time.Duration {
	if !cfg.Enabled || cfg.Consolidation == nil || cfg.Consolidation.IntervalMinutes == nil {
		return 0
	}
	m := *cfg.Consolidation.IntervalMinutes
	if math.IsNaN(m) || math.IsInf(m, 0) || m <= 0 {
		return 0
	}
	ms := math.Min(math.Max(m, 1)*60_000, maxIntervalMs)
	return time.Duration(int64(ms)) * time.Millisecond
}
