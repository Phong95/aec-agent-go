package grouphub

import (
	"context"
	"encoding/json"
	"maps"
	"math"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/Phong95/aec-agent-go/components/events"
	"github.com/Phong95/aec-agent-go/components/grouphub/group"
	"github.com/Phong95/aec-agent-go/components/persistence"
	"github.com/Phong95/aec-agent-go/kernel"
	"github.com/Phong95/aec-agent-go/kernel/ordered"
)

// threadDeps is what a thread needs.
type threadDeps struct {
	bus      events.Bus
	messages func() persistence.GroupMessageStore
	sessions func() persistence.SessionStore
}

// sessionLock orders the writes of one group session; refs counts its holders and
// waiters, so an idle lock is dropped.
type sessionLock struct {
	mu   sync.Mutex
	refs int
}

// thread is the threads of group sessions (A-106): it appends and updates their
// messages (one at a time per session, so Seq follows the order things happened),
// announces them on the bus, and keeps each member's cursor - the last thread message
// it was told about - in the group session's metadata (memberSeen). Used by the Hub
// (notes) and its turns. It is safe for concurrent use.
type thread struct {
	d threadDeps

	mu    sync.Mutex
	locks map[string]*sessionLock
}

func newThread(d threadDeps) *thread {
	return &thread{d: d, locks: map[string]*sessionLock{}}
}

// inOrder runs task as the session's next write (appends, updates, metadata writes).
func (t *thread) inOrder(sessionID string, task func()) {
	t.mu.Lock()
	l := t.locks[sessionID]
	if l == nil {
		l = &sessionLock{}
		t.locks[sessionID] = l
	}
	l.refs++
	t.mu.Unlock()

	l.mu.Lock()
	defer func() {
		l.mu.Unlock()
		t.mu.Lock()
		l.refs--
		if l.refs == 0 && t.locks[sessionID] == l {
			delete(t.locks, sessionID)
		}
		t.mu.Unlock()
	}()
	task()
}

// nowISO is the current time as an ISO string (ms precision, like Date.toISOString).
func nowISO() string { return kernel.MillisToISO(time.Now().UnixMilli()) }

// append adds a message to the session's thread (m.ID is set when empty, the times
// always) and emits group:message-added.
func (t *thread) append(ctx context.Context, m group.Message) (group.Message, error) {
	var stored group.Message
	var err error
	t.inOrder(m.SessionID, func() {
		now := nowISO()
		if m.ID == "" {
			m.ID = kernel.NewID()
		}
		m.CreatedAt, m.UpdatedAt = now, now
		stored, err = t.d.messages().Append(ctx, m)
		if err != nil {
			return
		}
		events.Emit(t.d.bus, events.GroupMessageAdded, stored.SessionID, events.GroupMessagePayload{GroupID: stored.GroupID, Message: stored.Clone()})
	})
	return stored, err
}

// update stores a changed message (its UpdatedAt set now) and emits
// group:message-updated.
func (t *thread) update(ctx context.Context, m group.Message) (group.Message, error) {
	var err error
	t.inOrder(m.SessionID, func() {
		m.UpdatedAt = nowISO()
		if err = t.d.messages().Update(ctx, m); err != nil {
			return
		}
		events.Emit(t.d.bus, events.GroupMessageUpdated, m.SessionID, events.GroupMessagePayload{GroupID: m.GroupID, Message: m.Clone()})
	})
	return m, err
}

// note appends a note (kind "event") to the thread.
func (t *thread) note(ctx context.Context, groupID, sessionID, text string) (group.Message, error) {
	return t.append(ctx, group.Message{SessionID: sessionID, GroupID: groupID, Kind: group.KindEvent, Text: text})
}

// delta announces more text of a streaming answer; the message is stored when it ends.
func (t *thread) delta(m group.Message, text string) {
	if text == "" {
		return
	}
	events.Emit(t.d.bus, events.GroupMessageDelta, m.SessionID, events.GroupMessageDeltaPayload{
		GroupID: m.GroupID, MessageID: m.ID, AgentID: m.AgentID, Text: text,
	})
}

func (t *thread) presence(sessionID string, p events.GroupPresencePayload) {
	events.Emit(t.d.bus, events.GroupPresence, sessionID, p)
}

// settle waits for the session's pending writes.
func (t *thread) settle(sessionID string) { t.inOrder(sessionID, func() {}) }

func (t *thread) list(ctx context.Context, sessionID string, afterSeq int) ([]group.Message, error) {
	return t.d.messages().List(ctx, sessionID, afterSeq)
}

// unseen returns what agentID has not been told yet: the thread after its cursor, read
// in order. Its own answers and hand-offs are skipped (they are in its own session
// already), and the read stops at another member's answer that is still being written -
// so the member is told it once it is done. It returns the messages to tell and the new
// cursor.
func (t *thread) unseen(ctx context.Context, sessionID, agentID string) ([]group.Message, int, error) {
	t.settle(sessionID)
	session, err := t.d.sessions().Get(ctx, sessionID)
	if err != nil {
		return nil, 0, err
	}
	cursor := seenOf(session, agentID)
	all, err := t.list(ctx, sessionID, cursor)
	if err != nil {
		return nil, 0, err
	}
	var out []group.Message
	for _, m := range all {
		own := (m.Kind == group.KindAgent || m.Kind == group.KindHandoff) && m.AgentID == agentID
		if !own && m.Kind == group.KindAgent && m.Status == group.StatusRunning {
			break
		}
		if !own {
			out = append(out, m)
		}
		cursor = m.Seq
	}
	return out, cursor, nil
}

// markSeen moves members' cursors forward (never back).
func (t *thread) markSeen(ctx context.Context, sessionID string, cursors map[string]int) error {
	_, err := t.updateMetadata(ctx, sessionID, func(meta ordered.Object) ordered.Object {
		seen := ordered.NewObject()
		if v, ok := meta.Get("memberSeen"); ok {
			if o, ok := v.(ordered.Object); ok {
				seen = o.Clone()
			}
		}
		for _, id := range slices.Sorted(maps.Keys(cursors)) {
			if cursors[id] > numberOf(seen, id) {
				seen.Set(id, cursors[id])
			}
		}
		meta.Set("memberSeen", seen)
		return meta
	})
	return err
}

// lastSeq is the last Seq of the session's thread (0 when empty).
func (t *thread) lastSeq(ctx context.Context, sessionID string) (int, error) {
	t.settle(sessionID)
	all, err := t.list(ctx, sessionID, 0)
	if err != nil || len(all) == 0 {
		return 0, err
	}
	return all[len(all)-1].Seq, nil
}

// updateMetadata changes a group session's metadata (a copy handed to change), in order
// with the session's other writes. It returns nil and a nil error when there is no such
// session.
func (t *thread) updateMetadata(ctx context.Context, sessionID string, change func(ordered.Object) ordered.Object) (*kernel.Session, error) {
	var out *kernel.Session
	var err error
	t.inOrder(sessionID, func() {
		sessions := t.d.sessions()
		var s *kernel.Session
		s, err = sessions.Get(ctx, sessionID)
		if err != nil || s == nil {
			return
		}
		meta := s.Metadata.Clone()
		if meta.IsZero() {
			meta = ordered.NewObject()
		}
		s.Metadata = change(meta)
		s.UpdatedAt = time.Now().UnixMilli()
		if err = sessions.Update(ctx, *s); err != nil {
			return
		}
		out = s
	})
	return out, err
}

// forget drops a deleted session's write lock when it is idle.
func (t *thread) forget(sessionID string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if l := t.locks[sessionID]; l != nil && l.refs == 0 {
		delete(t.locks, sessionID)
	}
}

// seenOf is the cursor of agentID in the session's memberSeen (0 when none).
func seenOf(s *kernel.Session, agentID string) int {
	if s == nil {
		return 0
	}
	v, ok := s.Metadata.Get("memberSeen")
	if !ok {
		return 0
	}
	o, ok := v.(ordered.Object)
	if !ok {
		return 0
	}
	return numberOf(o, agentID)
}

// numberOf reads a whole number of o (a Go int, or a decoded JSON number); 0 when absent
// or not a finite number.
func numberOf(o ordered.Object, key string) int {
	v, ok := o.Get(key)
	if !ok {
		return 0
	}
	var f float64
	switch x := v.(type) {
	case int:
		return x
	case int64:
		return int(x)
	case float64:
		f = x
	case json.Number:
		parsed, err := strconv.ParseFloat(string(x), 64)
		if err != nil {
			return 0
		}
		f = parsed
	default:
		return 0
	}
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return 0
	}
	return int(f)
}
