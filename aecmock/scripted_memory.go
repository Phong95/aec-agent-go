package aecmock

import (
	"context"
	"slices"
	"sync"

	"github.com/Phong95/aec-agent-go/components/agent/agentloop"
	"github.com/Phong95/aec-agent-go/components/agent/memorysystem/memory"
)

// RecallCall is one RecallForTurn call a ScriptedMemory received.
type RecallCall struct {
	UserMessage string
	SessionID   string
}

// ScriptedMemory is a deterministic memory system for the agent loop tests, as the
// agentloop.Memory port: the n-th RecallForTurn call returns the n-th scripted
// result and the last one repeats (like the scripted tools). With no results it
// recalls nothing: no content, empty id and memory lists. It records every call and
// is safe for concurrent use. The zero value is not usable; create one with
// NewScriptedMemory.
type ScriptedMemory struct {
	mu      sync.Mutex
	results []memory.RecalledMemories
	next    int
	calls   []RecallCall
	err     error
}

var _ agentloop.Memory = (*ScriptedMemory)(nil)

// NewScriptedMemory returns a ScriptedMemory that answers with results in order.
func NewScriptedMemory(results ...memory.RecalledMemories) *ScriptedMemory {
	m := &ScriptedMemory{results: make([]memory.RecalledMemories, len(results))}
	for i, r := range results {
		m.results[i] = r.Clone()
	}
	return m
}

// ScriptedMemoryOption changes how a ScriptedMemory behaves; apply it with With.
type ScriptedMemoryOption func(*ScriptedMemory)

// With applies the options and returns m. Call it before the memory is used.
func (m *ScriptedMemory) With(opts ...ScriptedMemoryOption) *ScriptedMemory {
	for _, opt := range opts {
		if opt != nil {
			opt(m)
		}
	}
	return m
}

// WithRecallError makes every RecallForTurn call fail with err, so a test can show
// that a failed recall never fails the turn.
func WithRecallError(err error) ScriptedMemoryOption {
	return func(m *ScriptedMemory) { m.err = err }
}

// Calls returns the recorded calls in order. The result is a copy.
func (m *ScriptedMemory) Calls() []RecallCall {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Clone(m.calls)
}

// RecallForTurn records the call and returns the next scripted result (a copy), or
// the scripted error.
func (m *ScriptedMemory) RecallForTurn(_ context.Context, userMessage, sessionID string) (memory.RecalledMemories, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls = append(m.calls, RecallCall{UserMessage: userMessage, SessionID: sessionID})
	if m.err != nil {
		return memory.RecalledMemories{}, m.err
	}
	if len(m.results) == 0 {
		return memory.RecalledMemories{RecordIDs: []string{}, Memories: []memory.RecalledMemoryTrace{}}, nil
	}
	r := m.results[min(m.next, len(m.results)-1)]
	m.next++
	return r.Clone(), nil
}
