package agent

import (
	"cmp"
	"context"
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/Phong95/aec-agent-go/components/agent/conversation"
	"github.com/Phong95/aec-agent-go/components/agent/subagent"
	"github.com/Phong95/aec-agent-go/kernel"
	"github.com/Phong95/aec-agent-go/kernel/ordered"
)

// A-220: what the conversations tool may return per call.
const (
	defaultConversations = 10
	maxConversations     = 25
	defaultReadMessages  = 30
	maxReadMessages      = 100
	// maxReadChars is about how much text one read returns.
	maxReadChars = 16000
	// searchSessions and searchMessages are what a search looks through: the newest
	// conversations, and the end of each.
	searchSessions  = 50
	searchMessages  = 200
	snippetsPerHit  = 3
	snippetLead     = 60
	snippetLength   = 200
	minIDPrefix     = 4
	groupMemberKind = "group-member" // grouphub.MemberSessionSource: grouphub imports this package.
)

var whitespaceRun = regexp.MustCompile(`\s+`)

var _ conversation.Conversations = (*ConversationManager)(nil)

// withVisibility (A-220) says who may see which conversations. Without it the
// conversations tool has nothing to show.
func withVisibility(v conversation.Visibility) ConversationOption {
	return func(c *ConversationManager) { c.visibility = &v }
}

// ListVisible is the conversations this session may see, newest first (A-220). A chat that
// was opened and never used is not a conversation.
func (c *ConversationManager) ListVisible(ctx context.Context, sessionID string, limit int) ([]conversation.Summary, error) {
	sessions, err := c.visibleTo(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	limit = clampCount(limit, defaultConversations, maxConversations)
	out := []conversation.Summary{}
	for i := range sessions {
		summary, err := c.summaryOf(ctx, &sessions[i])
		if err != nil {
			return nil, err
		}
		if summary.MessageCount == 0 {
			continue
		}
		out = append(out, summary)
		if len(out) == limit {
			break
		}
	}
	return out, nil
}

// SearchVisible is the conversations with messages matching the query's words, most words
// first, then newest (A-220). It scans the newest conversations rather than an index: it
// serves "we just discussed X", and substring matching handles languages a stemmer would
// not.
func (c *ConversationManager) SearchVisible(ctx context.Context, sessionID, query string, limit int) ([]conversation.SearchHit, error) {
	terms := uniqueTerms(query)
	if len(terms) == 0 {
		return nil, kernel.NewSessionError("a search needs some words to look for", kernel.SessionCodeInvalid, false)
	}
	sessions, err := c.visibleTo(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	if len(sessions) > searchSessions {
		sessions = sessions[:searchSessions]
	}
	type scored struct {
		hit      conversation.SearchHit
		termsHit int
	}
	var found []scored
	for i := range sessions {
		if ctx.Err() != nil {
			break
		}
		s := &sessions[i]
		slice, err := c.hub.Messages.GetRange(ctx, s.ID, -1, searchMessages)
		if err != nil {
			return nil, err
		}
		snippets, termsHit := matchMessages(slice.Messages, slice.Total-len(slice.Messages), terms)
		if termsHit > 0 {
			found = append(found, scored{
				hit:      conversation.SearchHit{ID: s.ID, Title: conversationTitle(s), Source: conversationSource(s), UpdatedAt: s.UpdatedAt, Snippets: snippets},
				termsHit: termsHit,
			})
		}
	}
	slices.SortStableFunc(found, func(a, b scored) int {
		return cmp.Or(cmp.Compare(b.termsHit, a.termsHit), cmp.Compare(b.hit.UpdatedAt, a.hit.UpdatedAt))
	})
	limit = clampCount(limit, defaultConversations, maxConversations)
	out := []conversation.SearchHit{}
	for i := 0; i < len(found) && i < limit; i++ {
		out = append(out, found[i].hit)
	}
	return out, nil
}

// ReadVisible is one visible conversation's user and assistant text, the newest limit
// messages before index before (A-220).
func (c *ConversationManager) ReadVisible(ctx context.Context, sessionID, ref string, before, limit int) (conversation.Page, error) {
	sessions, err := c.visibleTo(ctx, sessionID)
	if err != nil {
		return conversation.Page{}, err
	}
	session, err := pickConversation(sessions, ref)
	if err != nil {
		return conversation.Page{}, err
	}
	from := -1
	if before > 0 {
		from = before
	}
	slice, err := c.hub.Messages.GetRange(ctx, session.ID, from, clampCount(limit, defaultReadMessages, maxReadMessages))
	if err != nil {
		return conversation.Page{}, err
	}
	end := slice.Total
	if before > 0 && before < slice.Total {
		end = before
	}
	first := end - len(slice.Messages)
	// Newest first, so the character limit drops the oldest; a page always holds one message.
	messages := []conversation.Message{}
	chars := 0
	oldest := first + len(slice.Messages)
	for i := len(slice.Messages) - 1; i >= 0; i-- {
		room := maxReadChars - chars
		if room <= 0 {
			break
		}
		oldest = first + i
		m := &slice.Messages[i]
		if !readableMessage(m) {
			continue
		}
		text := m.Content
		runes := []rune(text)
		if len(runes) > room {
			text = string(runes[:room]) + "… [cut]"
		}
		messages = append(messages, conversation.Message{Index: first + i, Role: m.Role, Text: text, Timestamp: m.Timestamp})
		chars += len(runes)
	}
	slices.Reverse(messages)
	summary, err := c.summaryOf(ctx, session)
	if err != nil {
		return conversation.Page{}, err
	}
	page := conversation.Page{Conversation: summary, Messages: messages}
	if oldest > 0 {
		page.Before = oldest
	}
	return page, nil
}

// visibleTo is every conversation this session may see, newest first (A-220): worked out
// again on each call from the session itself. The filter always has agentId, because an
// unfiltered list is every agent's sessions; job runs, subagents and group members are
// never in it.
func (c *ConversationManager) visibleTo(ctx context.Context, sessionID string) ([]kernel.Session, error) {
	v := c.visibility
	if v == nil {
		return nil, kernel.NewSessionError("conversations are not available here", kernel.SessionCodeNotAllowed, false)
	}
	caller, ok, err := c.GetSession(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, kernel.NewSessionError("this conversation no longer exists", kernel.SessionCodeNotFound, false)
	}
	if caller.JobID != "" || hiddenConversation(&caller) {
		return nil, kernel.NewSessionError("job runs, subagents and group members cannot read other conversations", kernel.SessionCodeNotAllowed, false)
	}
	filter := conversationHostScope(v, caller)
	filter.Set("agentId", v.AgentID)
	if v.Reach(caller) == conversation.ReachChat {
		channelID, _ := caller.Metadata.Get("channelId")
		chatKey, _ := caller.Metadata.Get("chatKey")
		id, idOK := channelID.(string)
		key, keyOK := chatKey.(string)
		if !idOK || !keyOK {
			return []kernel.Session{}, nil
		}
		filter.Set("source", "channel")
		filter.Set("channelId", id)
		filter.Set("chatKey", key)
	}
	sessions, err := c.hub.Sessions.List(ctx, kernel.SessionFilter{Metadata: filter})
	if err != nil {
		return nil, err
	}
	sessions = slices.DeleteFunc(sessions, func(s kernel.Session) bool {
		return s.ID == caller.ID || s.JobID != "" || hiddenConversation(&s)
	})
	slices.SortStableFunc(sessions, func(a, b kernel.Session) int { return cmp.Compare(b.UpdatedAt, a.UpdatedAt) })
	return sessions, nil
}

func (c *ConversationManager) summaryOf(ctx context.Context, s *kernel.Session) (conversation.Summary, error) {
	count, err := c.hub.Messages.Count(ctx, s.ID)
	if err != nil {
		return conversation.Summary{}, err
	}
	return conversation.Summary{ID: s.ID, Title: conversationTitle(s), Source: conversationSource(s), UpdatedAt: s.UpdatedAt, MessageCount: count}, nil
}

// conversationHostScope is a copy of the host's extra filter, so setting agentId on it
// never changes an object the host holds.
func conversationHostScope(v *conversation.Visibility, caller kernel.Session) ordered.Object {
	if v.Scope != nil {
		if scope := v.Scope(); scope != nil {
			if extra := scope(caller); !extra.IsZero() {
				return extra.Clone()
			}
		}
	}
	return ordered.NewObject()
}

// hiddenConversation is a subagent's or a group member's session.
func hiddenConversation(s *kernel.Session) bool {
	if subagent.IsSession(s) {
		return true
	}
	source, _ := s.Metadata.Get("source")
	return source == groupMemberKind
}

// clampCount is a count the model asked for, within limits; 0 or less means the default.
func clampCount(value, fallback, maxValue int) int {
	if value < 1 {
		return fallback
	}
	return min(value, maxValue)
}

func conversationTitle(s *kernel.Session) string {
	if title, ok := s.Metadata.Get("title"); ok {
		if text, isText := title.(string); isText && strings.TrimSpace(text) != "" {
			return strings.TrimSpace(text)
		}
	}
	return "(untitled)"
}

func conversationSource(s *kernel.Session) string {
	if platform, ok := s.Metadata.Get("platform"); ok {
		if text, isText := platform.(string); isText && text != "" {
			return text
		}
	}
	return "web"
}

// readableMessage is what people said to each other: no tool calls, tool results or
// reasoning, and no compaction summaries.
func readableMessage(m *kernel.Message) bool {
	return (m.Role == kernel.RoleUser || m.Role == kernel.RoleAssistant) && !m.Summary && strings.TrimSpace(m.Content) != ""
}

// pickConversation is the visible conversation with this id or unique id prefix; anything
// else reads as not found.
func pickConversation(visible []kernel.Session, ref string) (*kernel.Session, error) {
	wanted := strings.TrimSpace(ref)
	var byPrefix []*kernel.Session
	for i := range visible {
		s := &visible[i]
		if s.ID == wanted {
			return s, nil
		}
		if len(wanted) >= minIDPrefix && strings.HasPrefix(s.ID, wanted) {
			byPrefix = append(byPrefix, s)
		}
	}
	switch {
	case len(byPrefix) == 1:
		return byPrefix[0], nil
	case len(byPrefix) > 1:
		ids := make([]string, 0, 5)
		for _, s := range byPrefix[:min(5, len(byPrefix))] {
			ids = append(ids, s.ID)
		}
		return nil, kernel.NewSessionError(fmt.Sprintf(`"%s" matches %d conversations: %s`, wanted, len(byPrefix), strings.Join(ids, ", ")), kernel.SessionCodeInvalid, false)
	}
	return nil, kernel.NewSessionError(fmt.Sprintf(`no conversation "%s" found`, wanted), kernel.SessionCodeNotFound, false)
}

func uniqueTerms(query string) []string {
	var terms []string
	for _, t := range strings.Fields(strings.ToLower(query)) {
		if !slices.Contains(terms, t) {
			terms = append(terms, t)
		}
	}
	return terms
}

// matchMessages is the best snippets of a conversation's messages and how many of the
// terms it hit (0 when nothing matched). first is the index of messages[0].
func matchMessages(messages []kernel.Message, first int, terms []string) ([]conversation.Snippet, int) {
	type hit struct {
		snippet conversation.Snippet
		count   int
	}
	seen := map[string]bool{}
	var hits []hit
	for i := range messages {
		m := &messages[i]
		if !readableMessage(m) {
			continue
		}
		lower := strings.ToLower(m.Content)
		count, position := 0, -1
		for _, t := range terms {
			if at := strings.Index(lower, t); at >= 0 {
				count++
				seen[t] = true
				if position < 0 {
					position = at
				}
			}
		}
		if count > 0 {
			hits = append(hits, hit{count: count, snippet: conversation.Snippet{Index: first + i, Role: m.Role, Text: snippetAt(m.Content, lower, position)}})
		}
	}
	slices.SortStableFunc(hits, func(a, b hit) int {
		return cmp.Or(cmp.Compare(b.count, a.count), cmp.Compare(b.snippet.Index, a.snippet.Index))
	})
	if len(hits) > snippetsPerHit {
		hits = hits[:snippetsPerHit]
	}
	snippets := make([]conversation.Snippet, 0, len(hits))
	for _, h := range hits {
		snippets = append(snippets, h.snippet)
	}
	slices.SortStableFunc(snippets, func(a, b conversation.Snippet) int { return cmp.Compare(a.Index, b.Index) })
	return snippets, len(seen)
}

// snippetAt is the text around a byte position of its lower-cased form, cut on runes.
func snippetAt(text, lower string, position int) string {
	runes := []rune(text)
	at := min(len([]rune(lower[:position])), len(runes))
	start := max(0, at-snippetLead)
	end := min(len(runes), start+snippetLength)
	part := strings.TrimSpace(whitespaceRun.ReplaceAllString(string(runes[start:end]), " "))
	if start > 0 {
		part = "…" + part
	}
	if end < len(runes) {
		part += "…"
	}
	return part
}
