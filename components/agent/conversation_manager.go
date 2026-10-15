package agent

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/Phong95/aec-agent-go/components/persistence"
	"github.com/Phong95/aec-agent-go/kernel"
	"github.com/Phong95/aec-agent-go/kernel/ordered"
)

// ConversationManager is the agent's conversation over a persistence.PersistenceHub, as the
// agentloop.Conversation port. It keeps the active session, the full history and
// the compacted working set in memory and writes through to the hub's stores:
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
// A turn is bound to the session it started on. The loop's Conversation port carries
// no session id, so Agent.Chat and ChatStream put the id of the active session on the
// ctx of the run (withRunSession), and AddMessage, SetCompactedMessages and
// UpdateSession refuse to write when the active session is no longer that one, with an
// error that matches agentloop.ErrSessionNotActive. A host that calls StartSession or
// LoadSession while a turn is running therefore cannot move the rest of that turn
// (messages, compaction, token usage) into the other session: the turn fails at its
// next write instead. A ctx without the binding (a caller that uses the manager
// directly) writes to the active session.
//
// The hub is the one that was current when the agent was spawned (see
// Deps.Persistence). Two mutexes guard the manager. writeMu serializes every write and
// every session switch (StartSession, LoadSession, AddMessage, SetCompactedMessages,
// UpdateSession) and is held across their store calls, so two writes never interleave
// and a LoadSession cannot install a history that misses a message a concurrent
// AddMessage just stored. mu guards the in-memory state and is held only for short
// cache reads and updates, never across a store call, so Session, Messages and
// LLMMessages never wait for the store. Lock order: writeMu, then mu. Every returned
// slice is a deep copy. It is safe for concurrent use.
//
// -> Superseded (A-97): the manager no longer keeps a session, a history or a working
// set in memory, and it is no longer the agentloop.Conversation port. Every turn names
// its session (ChatOption WithChatSession, or the default below) and works on its own
// Conversation, which Open loads from the stores when the turn starts, under that
// session's slot of the agent's turn gate, and which is dropped when the turn ends. The
// in-memory caches, the write rules (store first, cache second) and AddMessage,
// SetCompactedMessages, UpdateSession, LLMMessages and Messages moved to Conversation
// with their comments. Turns of different sessions therefore share no in-memory state
// and run in parallel, and the session binding of a run's ctx described above
// (withRunSession, ErrSessionNotActive) is gone: a turn's Conversation is bound to its
// session by construction, so a StartSession or LoadSession while a turn runs changes
// nothing for that turn. The "active session" set by StartSession and LoadSession is now
// only the default session: the one a Chat or ChatStream without WithChatSession runs in.
// It is guarded by mu; writeMu is gone with the writes.
//
// The manager also starts every turn of the agent (beginTurn): it resolves the turn's
// session (the one named, else the default), waits for that session's slot of the turn
// gate, and loads the session's Conversation. It owns the turn gate and the dispose
// bookkeeping of the chat runs, which used to live on the Agent, so the Agent only runs
// the loop inside the turn; Agent.Dispose closes the turns through closeTurns and
// cancelTurns.
type ConversationManager struct {
	hub *persistence.PersistenceHub
	now func() string

	// mu guards defaultID.
	mu sync.Mutex
	// defaultID is the session a turn that names none runs in, set by StartSession and
	// LoadSession (A-97); empty before the first of them.
	defaultID string

	// (Moved from Agent with their comments.)
	// runLock serializes the chats of the agent: Chat and ChatStream hold its one slot
	// for the whole turn. It is a channel of capacity one, not a mutex, so a chat that
	// waits for its turn honours its ctx and gives up when Dispose starts.
	// -> Superseded (A-97): replaced by turns, a per-session gate: turns of different
	// sessions run in parallel, turns of one session one at a time in arrival order, and
	// loopConfig.maxConcurrentTurns optionally caps the turns of the agent. A waiting turn
	// still honours its ctx and gives up when Dispose starts.
	turns *turnGate
	// runsMu guards closed and the Add of runs, so Dispose never waits on a WaitGroup
	// that a new chat could still join.
	runsMu sync.Mutex
	// closed is set when Dispose starts; Chat and ChatStream then return
	// ErrAgentDisposed.
	closed bool
	// closing is closed when Dispose starts, which releases the chats that wait for
	// runLock (A-97: for their slot of the turn gate).
	closing chan struct{}
	// runs counts the chats that hold or wait for runLock (A-97: the turn gate).
	runs sync.WaitGroup
	// runsCtx ends the ctx of every chat run: Dispose cancels it (cancelRuns) before
	// it waits for the runs. It derives from the lifetime context, so cancelLifetime
	// ends it too.
	runsCtx    context.Context
	cancelRuns context.CancelFunc
}

// ConversationOption configures a ConversationManager.
type ConversationOption func(*ConversationManager)

// WithConversationClock sets the source of the ISO timestamp AddMessage stamps on a
// message that has none (default kernel.NowISO). A nil function is ignored.
func WithConversationClock(nowISO func() string) ConversationOption {
	return func(c *ConversationManager) {
		if nowISO != nil {
			c.now = nowISO
		}
	}
}

// withTurns gives the manager the agent's lifetime context, from which the ctx of every
// chat run derives (so cancelling the lifetime ends the runs), and the live cap of the
// turn gate (loopConfig.maxConcurrentTurns, read on every turn). The Agent passes it; a
// manager built without it has no cap and its runs derive from context.Background().
func withTurns(lifetime context.Context, maxConcurrent func() int) ConversationOption {
	return func(c *ConversationManager) {
		c.turns = newTurnGate(maxConcurrent)
		c.runsCtx, c.cancelRuns = context.WithCancel(lifetime)
	}
}

// -> Superseded (A-97): runSessionKey ("the context key of the session a run is bound
// to"), withRunSession ("returns ctx bound to the session of a run (see
// ConversationManager)") and checkRunSession ("the guard of every write: it fails when
// ctx is bound to a session other than the active one. The caller holds c.mu and has
// checked that c.session is not nil.") are gone with the session binding; see the type
// comment.

// NewConversationManager returns a ConversationManager over hub. It has no active
// session until StartSession or LoadSession succeeds.
// (A-97: "active session" is the default session; the clock option stamps the messages
// of the Conversations Open loads.)
func NewConversationManager(hub *persistence.PersistenceHub, opts ...ConversationOption) *ConversationManager {
	c := &ConversationManager{hub: hub, now: kernel.NowISO, closing: make(chan struct{})}
	for _, opt := range opts {
		if opt != nil {
			opt(c)
		}
	}
	if c.turns == nil {
		c.turns = newTurnGate(func() int { return 0 })
	}
	if c.runsCtx == nil {
		c.runsCtx, c.cancelRuns = context.WithCancel(context.Background())
	}
	return c
}

// StartSession creates a session in the session store and makes it the active one,
// with an empty history and no compacted working set. When llmModel is not empty it
// is stored on the session and the session is updated; when that update fails the
// session stays active and the error is returned.
// (A-97: "active" = the default session of the turns that name none.)
func (c *ConversationManager) StartSession(ctx context.Context, metadata ordered.Object, llmModel string) (kernel.Session, error) {
	s, err := c.hub.Sessions.Create(ctx, metadata)
	if err != nil {
		return kernel.Session{}, err
	}
	if llmModel != "" {
		s.LLMModel = llmModel
	}
	c.mu.Lock()
	c.defaultID = s.ID
	c.mu.Unlock()
	if llmModel != "" {
		if err := c.hub.Sessions.Update(ctx, s.Clone()); err != nil {
			return kernel.Session{}, err
		}
	}
	return s, nil
}

// LoadSession makes the stored session the active one and loads its messages and
// its compacted working set. An empty stored working set means
// none. The bool is false for an unknown id.
//
// An unknown id leaves the active session and its caches as they were, so a miss does
// not deactivate the session and the next Chat does not fail with ErrNoActiveSession.
// When a store call fails the active session is left as it was too, never the new
// session with the old caches.
//
// The reads and the installation of the caches run under the write mutex, so an
// AddMessage that runs at the same time either lands before the read (and is in the
// loaded history) or after the installation (and is appended to it); it is never lost.
//
// -> Superseded (A-97): LoadSession only reads the session and makes it the default
// session; it loads no messages (each turn loads its own Conversation through Open), so
// there are no caches and no write mutex. An unknown id or a failed read still leaves
// the default session as it was.
func (c *ConversationManager) LoadSession(ctx context.Context, sessionID string) (kernel.Session, bool, error) {
	stored, err := c.hub.Sessions.Get(ctx, sessionID)
	if err != nil {
		return kernel.Session{}, false, err
	}
	if stored == nil {
		return kernel.Session{}, false, nil
	}
	c.mu.Lock()
	c.defaultID = stored.ID
	c.mu.Unlock()
	return stored.Clone(), true, nil
}

// DefaultSessionID returns the session a turn that names none runs in (A-97), set by
// StartSession and LoadSession. The bool is false before the first of them.
func (c *ConversationManager) DefaultSessionID() (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.defaultID, c.defaultID != ""
}

// Open loads a session's conversation from the stores for one turn (A-97). Call it once
// the turn holds the session's slot, so no other turn of that session is writing. The
// bool is false, with a nil error, when the session does not exist.
func (c *ConversationManager) Open(ctx context.Context, sessionID string) (*Conversation, bool, error) {
	return loadConversation(ctx, c.hub, c.now, sessionID)
}

// Messages returns a session's full history, read from the message store (A-97).
func (c *ConversationManager) Messages(ctx context.Context, sessionID string) ([]kernel.Message, error) {
	return c.hub.Messages.Get(ctx, sessionID)
}

// ListSessions returns the sessions of the session store that match the filter's
// metadata. There is no agent filter: every session of the store is a
// candidate.
func (c *ConversationManager) ListSessions(ctx context.Context, f kernel.SessionFilter) ([]kernel.Session, error) {
	return c.hub.Sessions.List(ctx, f)
}

// -> Superseded (A-97): Session ("returns a copy of the active session, or false when
// there is none"), LLMMessages, Messages (the in-memory ones), activeFor ("returns a copy
// of the active session for a write bound to ctx, or the error of the write:
// agentloop.ErrNoActiveSession, or the ErrSessionNotActive of checkRunSession. The
// caller holds writeMu, so the session cannot change until the write is over."),
// AddMessage, SetCompactedMessages,
// UpdateSession and cloneMessageSlice moved to Conversation (conversation.go) with their
// comments; DefaultSessionID replaces Session for the default session, and Messages above
// reads a session's history from the store.

// ---------------------------------------------------------------------------
// Turns - when a turn may run, and the conversation it works on (A-97)
// ---------------------------------------------------------------------------

// conversationTurn is a turn that holds its session, from beginTurn: the run's ctx, the
// session's Conversation loaded for this turn, and release, which ends the turn (the
// session's next turn may start and the agent slot is freed).
// -> Superseded: the run's ctx is no longer a field; beginTurn returns it next to the
// turn, so the ctx a caller passes on visibly derives from its own (contextcheck).
type conversationTurn struct {
	conversation *Conversation
	release      func()
}

// beginTurn starts one turn: it resolves the session (sessionID, else the default
// session of StartSession and LoadSession), waits for its turn (beginRun) and loads its
// Conversation (openConversation). On success the caller must call release once the
// turn ends. The errors are those of beginRun and openConversation; when the load fails
// the turn is released before the error is returned. The returned ctx is the run's
// ctx (derived from ctx): the turn's work runs under it.
func (c *ConversationManager) beginTurn(ctx context.Context, sessionID string) (context.Context, *conversationTurn, error) {
	id := c.resolveSession(sessionID)
	runCtx, release, err := c.beginRun(ctx, id)
	if err != nil {
		return nil, nil, err
	}
	conv, err := c.openConversation(runCtx, id)
	if err != nil {
		release()
		return nil, nil, err
	}
	return runCtx, &conversationTurn{conversation: conv, release: release}, nil
}

// AppendMessage adds a message to a session outside a model turn (A-100 phase 3: a
// scheduled job's result posted in a chat joins that chat's conversation, so a reply
// there has it). It waits for the session like a turn does (beginTurn), so it lands
// between turns, never inside one.
func (c *ConversationManager) AppendMessage(ctx context.Context, sessionID string, m kernel.Message) error {
	if sessionID == "" {
		return errors.New("AppendMessage: no session id")
	}
	runCtx, turn, err := c.beginTurn(ctx, sessionID)
	if err != nil {
		return err
	}
	defer turn.release()
	return turn.conversation.AddMessage(runCtx, m)
}

// (Moved from Agent with their comments.)
// resolveSession is the session a turn runs in (A-97): WithChatSession, else the
// default session of StartSession and LoadSession, else "" (no session).
func (c *ConversationManager) resolveSession(sessionID string) string {
	if sessionID != "" {
		return sessionID
	}
	id, _ := c.DefaultSessionID()
	return id
}

// conversationLoadTimeout bounds the load of a turn's conversation (A-97). The load
// runs under context.WithoutCancel of the run's ctx, like the loop's writes: a turn
// whose ctx is already done when it starts still saves the user message and ends at
// its first safe point (A-65), so its conversation must load.
const conversationLoadTimeout = 10 * time.Second

// openConversation loads the conversation of the session a turn holds (A-97). An
// unknown session is an error that matches ErrSessionNotFound.
func (c *ConversationManager) openConversation(ctx context.Context, sessionID string) (*Conversation, error) {
	lctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), conversationLoadTimeout)
	defer cancel()
	conv, ok, err := c.Open(lctx, sessionID)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, &sessionNotFoundError{id: sessionID}
	}
	return conv, nil
}

// RunningTurns is how many turns of the agent run now, across all its sessions (A-97).
// A turn that waits for its session or for a slot under loopConfig.maxConcurrentTurns
// is not counted.
func (c *ConversationManager) RunningTurns() int { return c.turns.runningTurns() }

// chatRunKey is the context key under which the ctx of a chat run carries its agent,
// so a nested chat and a Dispose from inside a run can be recognised.
// (A-97: the value is a *runChain, the agent and session of every chat run the ctx is
// inside, so a nested chat on the same session is refused and one on another session
// runs.)
// (The agent is identified by its ConversationManager, one per agent.)
type chatRunKey struct{}

// runChain is one chat run of the ctx chain (A-97): its agent and session, and the run
// it is nested in (nil for the outermost one).
type runChain struct {
	owner     *ConversationManager
	sessionID string
	parent    *runChain
}

// runChainOf returns the chat runs ctx is inside, innermost first, or nil.
func runChainOf(ctx context.Context) *runChain {
	r, _ := ctx.Value(chatRunKey{}).(*runChain)
	return r
}

// insideRun reports whether ctx is inside a chat run of a; with sessionID not empty, a
// run of a in that session.
// (a is now the agent's ConversationManager c.)
func (c *ConversationManager) insideRun(ctx context.Context, sessionID string) bool {
	for r := runChainOf(ctx); r != nil; r = r.parent {
		if r.owner == c && (sessionID == "" || r.sessionID == sessionID) {
			return true
		}
	}
	return false
}

// beginRun waits for the agent's turn (runLock) and returns the ctx of the run and the
// function that ends it. The run ctx is ctx, marked with chatRunKey, and cancelled when
// Dispose cancels the runs. See Chat for the errors.
// (A-97: it waits for sessionID's slot of the turn gate instead; an empty sessionID is
// ErrNoActiveSession, after the ErrAgentDisposed check.)
func (c *ConversationManager) beginRun(ctx context.Context, sessionID string) (context.Context, func(), error) {
	if sessionID != "" && c.insideRun(ctx, sessionID) {
		return nil, nil, ErrReentrantChat
	}
	c.runsMu.Lock()
	if c.closed {
		c.runsMu.Unlock()
		return nil, nil, ErrAgentDisposed
	}
	if sessionID == "" {
		c.runsMu.Unlock()
		return nil, nil, ErrNoActiveSession
	}
	c.runs.Add(1)
	c.runsMu.Unlock()

	// An idle agent runs the turn even with a done ctx (the loop then ends it Aborted,
	// the A-65 contract); only a turn that has to wait gives up on ctx.
	// (A-97: an idle session, with a free slot under the cap.)
	// A turn started from inside a running turn of this agent skips the cap wait (the
	// outer turn holds a slot and waits for this one).
	if err := c.turns.acquire(ctx, sessionID, c.insideRun(ctx, ""), c.closing); err != nil {
		c.runs.Done()
		return nil, nil, err
	}
	// The turn may have come free at the moment Dispose started.
	select {
	case <-c.closing:
		c.turns.release(sessionID)
		c.runs.Done()
		return nil, nil, ErrAgentDisposed
	default:
	}

	chain := &runChain{owner: c, sessionID: sessionID, parent: runChainOf(ctx)}
	runCtx, cancel := context.WithCancel(context.WithValue(ctx, chatRunKey{}, chain))
	stop := context.AfterFunc(c.runsCtx, cancel)
	release := func() {
		stop()
		cancel()
		c.turns.release(sessionID)
		c.runs.Done()
	}
	return runCtx, release, nil
}

// closeTurns is the chat step of Agent.Dispose: no chat starts from here on, the
// waiting ones give up (ErrAgentDisposed), and the running ones are cancelled and
// waited for, in any session. Not from inside one of those chats: it would wait for
// itself, and cancelling its ctx now would fail every later wait of Dispose at once.
// It returns the ctx error when ctx ends before the runs did. Calling it again does
// nothing more than wait.
func (c *ConversationManager) closeTurns(ctx context.Context) error {
	c.runsMu.Lock()
	if !c.closed {
		c.closed = true
		close(c.closing)
	}
	c.runsMu.Unlock()
	if c.insideRun(ctx, "") {
		return nil
	}
	c.cancelRuns()
	return c.waitRuns(ctx)
}

// cancelTurns ends the ctx of every chat run, the last step of Agent.Dispose (a run that
// closeTurns did not cancel, because Dispose was called from inside it, ends here).
func (c *ConversationManager) cancelTurns() { c.cancelRuns() }

// waitRuns waits until no chat holds or waits for the turn, or until ctx ends (its
// error is then returned, and the helper goroutine ends with the last run).
func (c *ConversationManager) waitRuns(ctx context.Context) error {
	done := make(chan struct{})
	go func() {
		defer close(done)
		c.runs.Wait()
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
