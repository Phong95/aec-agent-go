package agent

import (
	"context"
	"slices"
	"sync"

	"github.com/Phong95/aec-agent-go/components/agent/agentloop"
	"github.com/Phong95/aec-agent-go/components/persistence"
	"github.com/Phong95/aec-agent-go/kernel"
)

// Conversation is one session's conversation for the length of one turn (A-97): the
// session record, its full history and its compacted LLM working set, as the
// agentloop.Conversation port.
//
// A Conversation is loaded from the stores when a turn starts (ConversationManager.Open,
// called once the turn holds that session's slot of the agent's turn gate) and dropped
// when the turn ends, so every turn sees what the stores hold, including writes the host
// made between turns, and two sessions of one agent never share in-memory state. Every
// write goes straight to the stores.
//
// The write rules are the ones the ConversationManager had before A-97 (moved here with
// the methods):
//
//   - AddMessage stamps the timestamp when it is empty (from the injectable clock),
//     appends to the history store and, when a compacted working set exists, to
//     that too, bumps the session's UpdatedAt and updates the session.
//   - SetCompactedMessages replaces the working set.
//   - LLMMessages returns the working set when there is one, the history otherwise.
//   - UpdateSession merges CurrentTokenUsage and CounterCalibration into the stored
//     session and persists it (the agentloop.Conversation contract).
//
// The write order is store first, cache second (the other order would leave the cache
// one message ahead of the store when the store fails). A write the store refuses
// leaves the cache as it was and returns the error, so the cache never shows what the
// store does not hold. When the history append succeeds and the working-set append or
// the session update after it fails, the cache holds what the store holds (the history
// message), and the error is returned.
//
// writeMu serializes the writes and is held across their store calls; mu guards the
// in-memory state and is held only for short cache reads and updates, never across a
// store call. Lock order: writeMu, then mu. Every returned slice is a deep copy. It is
// safe for concurrent use, but the agent hands one Conversation to one turn only.
type Conversation struct {
	hub *persistence.PersistenceHub
	now func() string

	// writeMu serializes the store writes (see above).
	writeMu sync.Mutex

	mu        sync.Mutex
	session   kernel.Session
	messages  []kernel.Message
	compacted []kernel.Message // the working set; meaningful only when hasWorkingSet
	// hasWorkingSet distinguishes an empty compacted working set from none: a
	// message is appended to the working set whenever it exists, even when it is empty.
	hasWorkingSet bool
}

var _ agentloop.Conversation = (*Conversation)(nil)

// loadConversation reads a session's conversation from the stores: the session, its
// messages and its compacted working set (an empty stored working set means none). The
// bool is false, with a nil error, when the session does not exist.
func loadConversation(ctx context.Context, hub *persistence.PersistenceHub, now func() string, sessionID string) (*Conversation, bool, error) {
	stored, err := hub.Sessions.Get(ctx, sessionID)
	if err != nil {
		return nil, false, err
	}
	if stored == nil {
		return nil, false, nil
	}
	// Load the messages from their dedicated stores.
	msgs, err := hub.Messages.Get(ctx, sessionID)
	if err != nil {
		return nil, false, err
	}
	working, err := hub.CompactedMessages.Get(ctx, sessionID)
	if err != nil {
		return nil, false, err
	}
	c := &Conversation{hub: hub, now: now, session: stored.Clone()}
	// An empty history stays nil, as the history of a fresh session always was.
	if len(msgs) > 0 {
		c.messages = cloneMessageSlice(msgs)
	}
	c.hasWorkingSet = len(working) > 0
	if c.hasWorkingSet {
		c.compacted = cloneMessageSlice(working)
	}
	return c, true, nil
}

// Session returns a copy of the conversation's session. The bool is always true: a
// Conversation exists only for a session that exists.
func (c *Conversation) Session() (kernel.Session, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.session.Clone(), true
}

// LLMMessages returns the model's working set: the compacted messages when a
// compaction was stored, the full history otherwise.
func (c *Conversation) LLMMessages() []kernel.Message {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.hasWorkingSet {
		return cloneMessageSlice(c.compacted)
	}
	return cloneMessageSlice(c.messages)
}

// Messages returns the full history (what a UI shows).
func (c *Conversation) Messages() []kernel.Message {
	c.mu.Lock()
	defer c.mu.Unlock()
	return cloneMessageSlice(c.messages)
}

// AddMessage appends m to the history and, when there is a working set, to it as
// well, then bumps the session's UpdatedAt and saves the session.
// (Before A-97, on the ConversationManager: "With no active session it returns
// agentloop.ErrNoActiveSession ("No active session")." A Conversation always has its
// session.)
//
// Each cache update follows the store write it mirrors (see the type comment): a
// failing history append changes nothing in memory.
func (c *Conversation) AddMessage(ctx context.Context, m kernel.Message) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	c.mu.Lock()
	session := c.session.Clone()
	working := c.hasWorkingSet
	c.mu.Unlock()
	id := session.ID
	m = m.Clone()
	if m.Timestamp == "" {
		m.Timestamp = c.now()
	}
	if err := c.hub.Messages.Append(ctx, id, m); err != nil {
		return err
	}
	c.mu.Lock()
	c.messages = append(c.messages, m)
	c.mu.Unlock()
	if working {
		if err := c.hub.CompactedMessages.Append(ctx, id, m); err != nil {
			return err
		}
		c.mu.Lock()
		c.compacted = append(c.compacted, m.Clone())
		c.mu.Unlock()
	}
	session.UpdatedAt = kernel.NowMillis()
	if err := c.hub.Sessions.Update(ctx, session.Clone()); err != nil {
		return err
	}
	c.mu.Lock()
	c.session.UpdatedAt = session.UpdatedAt
	c.mu.Unlock()
	return nil
}

// SetCompactedMessages replaces the model's working set. Only the agent loop should
// call it; a UI always reads Messages. The cache changes only when the store accepted
// the new set. (Before A-97: with no active session it
// returned agentloop.ErrNoActiveSession.)
func (c *Conversation) SetCompactedMessages(ctx context.Context, msgs []kernel.Message) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	c.mu.Lock()
	id := c.session.ID
	c.mu.Unlock()
	if err := c.hub.CompactedMessages.Set(ctx, id, msgs); err != nil {
		return err
	}
	c.mu.Lock()
	c.compacted = cloneMessageSlice(msgs)
	c.hasWorkingSet = true
	c.mu.Unlock()
	return nil
}

// UpdateSession merges the usage fields of s into the session and persists it:
// CurrentTokenUsage and CounterCalibration are copied when s has them, nothing else
// is touched (the agentloop.Conversation contract). The cache changes only when the
// store accepted the session. (Before A-97: with no active session it
// returned agentloop.ErrNoActiveSession.)
//
// The loop calls it right after the AddMessage that follows each usage record, so
// the session written here already carries the UpdatedAt AddMessage bumped. An
// abort that happens after a usage record and before any message write reaches
// here too (Phase 9 IN-03), with whatever usage the run recorded.
func (c *Conversation) UpdateSession(ctx context.Context, s kernel.Session) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	c.mu.Lock()
	session := c.session.Clone()
	c.mu.Unlock()
	merged := s.Clone()
	if merged.CurrentTokenUsage != nil {
		session.CurrentTokenUsage = merged.CurrentTokenUsage
	}
	if merged.CounterCalibration != nil {
		session.CounterCalibration = merged.CounterCalibration
	}
	if err := c.hub.Sessions.Update(ctx, session.Clone()); err != nil {
		return err
	}
	c.mu.Lock()
	c.session = session
	c.mu.Unlock()
	return nil
}

// cloneMessageSlice deep-copies a message slice (nil stays nil).
func cloneMessageSlice(msgs []kernel.Message) []kernel.Message {
	if msgs == nil {
		return nil
	}
	out := slices.Clone(msgs)
	for i := range out {
		out[i] = out[i].Clone()
	}
	return out
}
