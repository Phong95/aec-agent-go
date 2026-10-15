package aecmock

import (
	"context"
	"sync"

	"github.com/Phong95/aec-agent-go/components/agent/agentloop"
	"github.com/Phong95/aec-agent-go/components/persistence"
	"github.com/Phong95/aec-agent-go/kernel"
	"github.com/Phong95/aec-agent-go/kernel/ordered"
)

// Conversation is a conversation manager over a persistence.PersistenceHub, as the
// agentloop.Conversation port the loop uses in tests. It keeps the session, the
// full history and the compacted working set in memory and writes through to the
// hub's stores:
//
//   - AddMessage stamps the timestamp when it is empty (from the injectable
//     clock), appends to the history store and, when a compacted working set
//     exists, to that too, bumps the session's UpdatedAt and updates the session.
//   - SetCompactedMessages replaces the working set.
//   - LLMMessages returns the working set when there is one, the history
//     otherwise.
//   - UpdateSession merges CurrentTokenUsage and CounterCalibration into the
//     stored session and persists it (the agentloop.Conversation contract).
//
// A mutex guards the in-memory state and every returned slice is a deep copy, so
// the loop and a test can read and change what they get freely. It is safe for
// concurrent use.
type Conversation struct {
	hub *persistence.PersistenceHub
	now func() string

	mu        sync.Mutex
	session   *kernel.Session
	messages  []kernel.Message
	compacted []kernel.Message // the working set; meaningful only when hasWorkingSet
	// hasWorkingSet distinguishes an empty compacted working set from none: a
	// message is appended to the working set whenever it exists, even when empty.
	hasWorkingSet bool
}

var _ agentloop.Conversation = (*Conversation)(nil)

// ConversationOption configures a Conversation.
type ConversationOption func(*Conversation)

// WithConversationClock sets the source of the ISO timestamp AddMessage stamps on
// a message that has none (default kernel.NowISO). A test that compares persisted
// messages uses a constant.
func WithConversationClock(nowISO func() string) ConversationOption {
	return func(c *Conversation) {
		if nowISO != nil {
			c.now = nowISO
		}
	}
}

// NewConversation returns a Conversation over hub. It has no session until
// StartSession is called.
func NewConversation(hub *persistence.PersistenceHub, opts ...ConversationOption) *Conversation {
	c := &Conversation{hub: hub, now: kernel.NowISO}
	for _, opt := range opts {
		if opt != nil {
			opt(c)
		}
	}
	return c
}

// StartSession creates a session in the hub's session store and makes it the
// active one, with an empty history and no compacted working set.
func (c *Conversation) StartSession(ctx context.Context, metadata ordered.Object) (kernel.Session, error) {
	s, err := c.hub.Sessions.Create(ctx, metadata)
	if err != nil {
		return kernel.Session{}, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.session = &s
	c.messages = nil
	c.compacted = nil
	c.hasWorkingSet = false
	return s.Clone(), nil
}

// Session returns a copy of the active session, or false when there is none.
func (c *Conversation) Session() (kernel.Session, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.session == nil {
		return kernel.Session{}, false
	}
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
// well, then bumps the session's UpdatedAt and saves the session. With no active
// session it returns agentloop.ErrNoActiveSession.
func (c *Conversation) AddMessage(ctx context.Context, m kernel.Message) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.session == nil {
		return agentloop.ErrNoActiveSession
	}
	id := c.session.ID
	m = m.Clone()
	if m.Timestamp == "" {
		m.Timestamp = c.now()
	}
	c.messages = append(c.messages, m)
	if err := c.hub.Messages.Append(ctx, id, m); err != nil {
		return err
	}
	if c.hasWorkingSet {
		c.compacted = append(c.compacted, m.Clone())
		if err := c.hub.CompactedMessages.Append(ctx, id, m); err != nil {
			return err
		}
	}
	c.session.UpdatedAt = kernel.NowMillis()
	return c.hub.Sessions.Update(ctx, *c.session)
}

// SetCompactedMessages replaces the model's working set. With no active session
// it returns agentloop.ErrNoActiveSession.
func (c *Conversation) SetCompactedMessages(ctx context.Context, msgs []kernel.Message) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.session == nil {
		return agentloop.ErrNoActiveSession
	}
	c.compacted = cloneMessageSlice(msgs)
	c.hasWorkingSet = true
	return c.hub.CompactedMessages.Set(ctx, c.session.ID, msgs)
}

// UpdateSession merges the usage fields of s into the active session and
// persists it: CurrentTokenUsage and CounterCalibration are copied when s has
// them, nothing else is touched. With no active session it returns
// agentloop.ErrNoActiveSession.
func (c *Conversation) UpdateSession(ctx context.Context, s kernel.Session) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.session == nil {
		return agentloop.ErrNoActiveSession
	}
	merged := s.Clone()
	if merged.CurrentTokenUsage != nil {
		c.session.CurrentTokenUsage = merged.CurrentTokenUsage
	}
	if merged.CounterCalibration != nil {
		c.session.CounterCalibration = merged.CounterCalibration
	}
	return c.hub.Sessions.Update(ctx, c.session.Clone())
}
