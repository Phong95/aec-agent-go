package skilllearning

import (
	"slices"
	"sync"

	"github.com/Phong95/aec-agent-go/components/agent/skilllearning/learning"
)

// ---------------------------------------------------------------------------
// episodeLedger - stage 1 of skill learning: watch each turn on the event bus and keep
// its counts (the site diary). The System feeds it the agent's own events, one session
// at a time, and asks for the episode when the turn ends. It is safe for concurrent use
// (the bus delivers events of different sessions on different goroutines).
//
//   agent-loop:message          -> a turn starts (begin)
//   agent-loop:tool-start       -> one more tool call
//   agent-loop:tool-end         -> isError, or attempt > 0 (it was retried) = a failed call
//   agent-loop:tool-error       -> a failed call (all retries used, invalid arguments,
//                                  output truncated) - "cancelled" is the user's stop, not a failure
//   agent-loop:tool-denied      -> a refused call: a user (or policy) decision, never a failure
//   agent-loop:skill-start      -> a skill was loaded (skill_view): its name
//   agent-loop:complete / max-iterations -> the turn ends (end)
//
// The events carry no call ids, so a call is counted as failed once, at its last event.
// ---------------------------------------------------------------------------

type openTurn struct {
	// token increments per turn, so a late messageStart lookup can't land on a newer turn.
	token        uint64
	startedAt    string
	messageStart int
	toolCalls    int
	failedCalls  int
	deniedCalls  int
	skillsUsed   []string
}

type episodeLedger struct {
	agentID string

	mu        sync.Mutex
	turns     map[string]*openTurn
	nextToken uint64
}

func newEpisodeLedger(agentID string) *episodeLedger {
	return &episodeLedger{agentID: agentID, turns: map[string]*openTurn{}}
}

// begin opens a turn in the session (its user message was saved) and returns its token.
func (l *episodeLedger) begin(sessionID, startedAt string) uint64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.nextToken++
	l.turns[sessionID] = &openTurn{token: l.nextToken, startedAt: startedAt, messageStart: -1, skillsUsed: []string{}}
	return l.nextToken
}

// setMessageStart records where the turn's user message sits in the MessageStore.
func (l *episodeLedger) setMessageStart(sessionID string, token uint64, index int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if t := l.turns[sessionID]; t != nil && t.token == token {
		t.messageStart = index
	}
}

func (l *episodeLedger) with(sessionID string, f func(*openTurn)) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if t := l.turns[sessionID]; t != nil {
		f(t)
	}
}

func (l *episodeLedger) toolStarted(sessionID string) {
	l.with(sessionID, func(t *openTurn) { t.toolCalls++ })
}

func (l *episodeLedger) toolEnded(sessionID string, isError bool, attempt int) {
	if isError || attempt > 0 {
		l.with(sessionID, func(t *openTurn) { t.failedCalls++ })
	}
}

func (l *episodeLedger) toolFailed(sessionID, errText string) {
	if errText != "cancelled" {
		l.with(sessionID, func(t *openTurn) { t.failedCalls++ })
	}
}

func (l *episodeLedger) toolDenied(sessionID string) {
	l.with(sessionID, func(t *openTurn) { t.deniedCalls++ })
}

func (l *episodeLedger) skillStarted(sessionID, skillName string) {
	if skillName == "" {
		return
	}
	l.with(sessionID, func(t *openTurn) {
		if !slices.Contains(t.skillsUsed, skillName) {
			t.skillsUsed = append(t.skillsUsed, skillName)
		}
	})
}

// end closes the turn: its episode, or false when no turn was open in the session (a
// turn that started before skill learning was switched on). messageEnd is the session's
// message count now.
func (l *episodeLedger) end(sessionID string, outcome learning.Outcome, endedAt string, messageEnd int) (learning.Episode, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	t := l.turns[sessionID]
	if t == nil {
		return learning.Episode{}, false
	}
	delete(l.turns, sessionID)
	return learning.Episode{
		AgentID:      l.agentID,
		SessionID:    sessionID,
		MessageStart: t.messageStart,
		MessageEnd:   messageEnd,
		ToolCalls:    t.toolCalls,
		FailedCalls:  t.failedCalls,
		DeniedCalls:  t.deniedCalls,
		SkillsUsed:   slices.Clone(t.skillsUsed),
		Outcome:      outcome,
		StartedAt:    t.startedAt,
		EndedAt:      endedAt,
	}, true
}

// tokenOf returns the open turn's token (0 = none).
func (l *episodeLedger) tokenOf(sessionID string) uint64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	if t := l.turns[sessionID]; t != nil {
		return t.token
	}
	return 0
}

// clear forgets every open turn (skill learning was switched off, or the agent stops).
func (l *episodeLedger) clear() {
	l.mu.Lock()
	defer l.mu.Unlock()
	clear(l.turns)
}

// isHardWon is stage 2: did the turn end normally after enough tool calls, with enough
// of them failing? Only such turns wait for the user's verdict.
func isHardWon(e learning.Episode, c learning.ResolvedConfig) bool {
	return e.Outcome == learning.OutcomeCompleted && e.ToolCalls >= c.MinToolCalls && e.FailedCalls >= c.MinFailedCalls
}

// struggled says whether the turn still struggled (for a learned skill it used).
func struggled(e learning.Episode, c learning.ResolvedConfig) bool {
	return e.Outcome != learning.OutcomeCompleted || e.FailedCalls >= max(1, c.MinFailedCalls)
}
