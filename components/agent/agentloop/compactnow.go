package agentloop

import (
	"context"

	"github.com/Phong95/aec-agent-go/kernel"
)

// CompactNowResult (A-136) is the outcome of a forced compaction (Loop.CompactNow,
// /compact).
type CompactNowResult struct {
	Compacted bool `json:"compacted"`
	// BeforeTokens is the context size before: as last reported, else estimated.
	BeforeTokens int `json:"beforeTokens"`
	// AfterTokens is the estimated context size after (the same as before when not
	// compacted).
	AfterTokens int `json:"afterTokens"`
	// MessagesBefore and MessagesAfter are the messages in the working set before and
	// after.
	MessagesBefore int `json:"messagesBefore"`
	MessagesAfter  int `json:"messagesAfter"`
}

// SessionChanger (A-136) is the optional side of Conversation that changes the session
// with a function and saves it - a change UpdateSession cannot make, since it merges only
// the fields that are set (CompactNow drops CurrentTokenUsage). The agent's Conversation
// implements it.
type SessionChanger interface {
	ChangeSession(ctx context.Context, change func(*kernel.Session)) error
}

// CompactNow (A-136) compacts the session's working set now (/compact), whatever its
// size, with the user's focus ("" for none) added to the summary request. The caller
// holds the session's turn slot. The session's reported context size is dropped once
// compacted (it described the old working set; through SessionChanger, when conv has
// it), so the next turn does not compact again before a response reports anew. The
// writes run under the persist timeout, like a run's.
func (l *Loop) CompactNow(ctx context.Context, conv Conversation, focus string) (CompactNowResult, error) {
	session, ok := conv.Session()
	if !ok {
		return CompactNowResult{}, ErrNoActiveSession
	}
	before, err := l.MeasureContext(ctx, conv)
	if err != nil {
		return CompactNowResult{}, err
	}
	msgs := conv.LLMMessages()
	counter := NewCounter()
	if session.CounterCalibration != nil {
		counter.Seed(session.CounterCalibration.CharsPerToken, session.CounterCalibration.Samples)
	}
	result := l.compactor.forSession(session).CompactNow(ctx, msgs, counter.Count, l.summarizer.summarize, focus)
	if !result.Compacted {
		return CompactNowResult{
			BeforeTokens:   before.UsedTokens,
			AfterTokens:    before.UsedTokens,
			MessagesBefore: len(msgs),
			MessagesAfter:  len(msgs),
		}, nil
	}
	if err := l.persist(ctx, func(pctx context.Context) error {
		return conv.SetCompactedMessages(pctx, result.Messages)
	}); err != nil {
		return CompactNowResult{}, err
	}
	if changer, ok := conv.(SessionChanger); ok {
		if err := l.persist(ctx, func(pctx context.Context) error {
			return changer.ChangeSession(pctx, func(s *kernel.Session) { s.CurrentTokenUsage = nil })
		}); err != nil {
			return CompactNowResult{}, err
		}
	}
	after, err := l.MeasureContext(ctx, conv)
	if err != nil {
		return CompactNowResult{}, err
	}
	return CompactNowResult{
		Compacted:      true,
		BeforeTokens:   before.UsedTokens,
		AfterTokens:    after.UsedTokens,
		MessagesBefore: len(msgs),
		MessagesAfter:  len(result.Messages),
	}, nil
}

// persist runs one write of CompactNow under a context the caller's cancellation cannot
// reach, bounded by the persist timeout (runState.persist, outside a run).
func (l *Loop) persist(ctx context.Context, write func(context.Context) error) error {
	pctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), l.persistTimeout)
	defer cancel()
	return write(pctx)
}

// planning (A-136) reports whether the session is in plan mode (Deps.ApprovalMode): only
// read-only tools are offered and run. No ApprovalMode dep, or an error reading it, is
// not plan mode (the error case asks, as approvalMode does).
func (l *Loop) planning(ctx context.Context, sessionID string) bool {
	if l.deps.ApprovalMode == nil {
		return false
	}
	mode, err := l.deps.ApprovalMode(ctx, sessionID)
	return err == nil && mode == kernel.ApprovalModePlan
}
