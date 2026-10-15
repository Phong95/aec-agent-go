// Package memorysystem is the memory system of an agent: the pipeline that turns
// finished conversation turns into durable, typed memories and recalls them into a
// later turn.
//
// The pipeline has four components, all unexported, which the System facade composes
// with the consolidation scheduler:
//
//   - Memory (memory.go) asks the model which facts, preferences and decisions of a
//     conversation batch are worth keeping and stores them, reinforcing or
//     superseding a memory with the same key.
//   - Indexing (indexing.go) keeps an in-memory keyword index of the agent's active
//     memories.
//   - Retrieval (retrieval.go) ranks the matches by relevance, recency and
//     importance and stamps every recalled memory.
//   - Dreaming (dreaming.go) is the checkpointed consolidation pass: it reads only the
//     messages added since the last pass, hands them to Memory, advances the session
//     checkpoint and expires stale memories.
//
// The package depends on the model only through the Model port (the agent's
// ModelManager satisfies it) and on persistence only through the store interfaces,
// so it imports neither agent, agentloop, aecmock nor conformance. Every JavaScript
// text behaviour the pipeline relies on (the \s and \p{P} classes of the tokenizer,
// toLowerCase, UTF-16 string length, Date parsing, parseInt) comes from
// internal/jsstr and kernel.UTF16Len, never from the strings or unicode packages
// directly; those disagree with JavaScript on edge cases that the corpora under
// tests/testdata/golden pin down.
//
// System (system.go) is the one exported facade of an agent: Start, Dispose,
// Consolidate, Recall, RecallForTurn and UpdateConfig, plus IntervalOf. The lifecycle
// contract:
//
//   - Start subscribes to the finished turns of the agent on the event bus and, while
//     the consolidation interval is above zero, starts one goroutine that owns the
//     ticker. The subscription is made whatever the interval is, so dirty tracking
//     keeps running while the interval is 0 and a turn that finished while
//     consolidation was disabled is consolidated after UpdateConfig enables it.
//   - Dispose is mandatory for a started System. It unsubscribes, stops the goroutine,
//     drops the queued sessions and waits for the pass that is running; it never
//     starts a final pass. Its ctx bounds the wait.
//   - One pass runs at a time, because the passes run inline in the one goroutine.
//   - Passes run under the lifetime context given to Start, which the owner cancels
//     only after Dispose returned.
//
// Memory values are user data: no log line of this package ever carries a memory
// value or message text, only record and session ids.
//
// Concurrency: one agent's pipeline writes to the store under one write mutex (a
// recall, a consolidation and a scheduler pass may run in parallel). The mutex is
// never held across a model call.
package memorysystem
