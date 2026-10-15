package agentloop

import (
	"context"
	"log/slog"
	"time"

	"github.com/Phong95/aec-agent-go/components/agent/memorysystem/memory"
	"github.com/Phong95/aec-agent-go/kernel"
)

// This file re-exports unexported helpers for the external test package
// agentloop_test, which needs the aecmock fakes and so cannot be package agentloop
// (aecmock imports agentloop). It is compiled only into tests.

// RaceAbort exposes raceAbort.
func RaceAbort[T any](ctx context.Context, work func() (T, error)) (T, bool, error) {
	return raceAbort(ctx, work)
}

// AbortableDelay exposes abortableDelay.
func AbortableDelay(ctx context.Context, d time.Duration) { abortableDelay(ctx, d) }

// Backoff exposes backoff, and MaxBackoff its cap.
func Backoff(baseMs, attempt int) time.Duration { return backoff(baseMs, attempt) }

// MaxBackoff is the cap of one wait between two attempts.
const MaxBackoff = maxBackoff

// Summarize runs the summarizer over models.
func Summarize(ctx context.Context, models Models, msgs []kernel.Message, maxOutputTokens int) (string, error) {
	return summarizer{models: models}.summarize(ctx, msgs, maxOutputTokens, "")
}

// RepairToolPairs exposes repairToolPairs.
func RepairToolPairs(msgs []kernel.Message) []kernel.Message { return repairToolPairs(msgs) }

// CarriesToolResults exposes carriesToolResults.
func CarriesToolResults(m kernel.Message) bool { return carriesToolResults(m) }

// CounterCacheCap is the bound of the counter cache.
const CounterCacheCap = maxCounterCacheEntries

// CounterCacheLen is the number of counts the counter currently caches.
func CounterCacheLen(c *Counter) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.cache)
}

// SetCompactorLogger sets the logger a Compactor warns through.
func SetCompactorLogger(c *Compactor, lg *slog.Logger) { c.logger = lg }

// WithRecalledMemories exposes withRecalledMemories.
func WithRecalledMemories(msgs []kernel.Message, recalled *memory.RecalledMemories) []kernel.Message {
	return withRecalledMemories(msgs, recalled)
}
