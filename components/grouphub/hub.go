package grouphub

import (
	"cmp"
	"context"
	"errors"
	"log/slog"
	"slices"
	"strconv"
	"strings"

	"github.com/Phong95/aec-agent-go/components/agent/subagent"
	"github.com/Phong95/aec-agent-go/components/events"
	"github.com/Phong95/aec-agent-go/components/grouphub/group"
	"github.com/Phong95/aec-agent-go/components/persistence"
	"github.com/Phong95/aec-agent-go/internal/jsstr"
	"github.com/Phong95/aec-agent-go/internal/nilcheck"
	"github.com/Phong95/aec-agent-go/kernel"
	"github.com/Phong95/aec-agent-go/kernel/ordered"
)

// Deps is what a Hub needs. Events, Store, Messages, Sessions and Agents are required.
type Deps struct {
	Events events.Bus
	// Store, Messages and Sessions are resolved on every call, so AgentCore.UsePersistence
	// can swap the stores.
	Store    func() persistence.GroupStore
	Messages func() persistence.GroupMessageStore
	Sessions func() persistence.SessionStore
	// MemberMessages returns the members' conversation stores - a deleted group session
	// takes its member sessions' messages with it. Optional.
	MemberMessages func() []persistence.MessageStore
	// Agents returns the agents members can be (AgentCore's orchestration).
	Agents func() AgentHost
	// Runner returns the host's runner for member turns (AgentCore.UseGroupRunner); nil,
	// or a nil runner, means the member's own ChatStream.
	Runner func() MemberRunner
	// Logger receives the hub's warnings. Nil means slog.Default().
	Logger *slog.Logger
}

// Hub is group chats (A-106) - several agents in one conversation (the GroupHub of
// core/ts). It owns:
//   - the groups (PersistenceHub.Groups): create, change, delete, with validation
//     (A-107) and a note in the open session for membership and settings changes
//     (A-113);
//   - their sessions (SessionStore, metadata source "group") and threads
//     (PersistenceHub.GroupMessages), and each member's private session per group
//     session;
//   - group turns: who answers (A-108), lead routing and rounds (A-109), guard rails
//     (A-112), stop and retry (A-115).
//
// It emits group:* events; hosts stream them to the thread's viewers. A deleted agent
// leaves every group (agent:removed). It is safe for concurrent use.
type Hub struct {
	d           Deps
	th          *thread
	turns       *turns
	unsubscribe []events.Unsubscribe
}

// New returns a Hub over d.
func New(d Deps) *Hub {
	if d.Logger == nil {
		d.Logger = slog.Default()
	}
	h := &Hub{d: d}
	h.th = newThread(threadDeps{bus: d.Events, messages: d.Messages, sessions: d.Sessions})
	h.turns = newTurns(turnsDeps{bus: d.Events, th: h.th, sessions: d.Sessions, agents: d.Agents, runner: d.Runner, logger: d.Logger})
	h.unsubscribe = []events.Unsubscribe{
		events.SubscribeCtx(d.Events, events.AgentRemoved, func(ctx context.Context, _ kernel.Event, p events.AgentRemovedPayload) {
			if err := h.removeAgentEverywhere(ctx, p.AgentID); err != nil {
				d.Logger.Warn("group: could not remove an agent from its groups", "agent", p.AgentID, "error", err)
			}
		}),
	}
	return h
}

// Scopes returns the spawn scopes of group leads while they lead a turn
// (agent.Deps.SubagentScopes).
func (h *Hub) Scopes() subagent.Scopes { return h.turns }

// ---------------------------------------------------------------------------
// Groups
// ---------------------------------------------------------------------------

// List returns every group, oldest first.
func (h *Hub) List(ctx context.Context) ([]group.Group, error) { return h.d.Store().List(ctx) }

// Get returns the group, or nil and a nil error when there is none.
func (h *Hub) Get(ctx context.Context, groupID string) (*group.Group, error) {
	return h.d.Store().Get(ctx, groupID)
}

// Create creates a group. A bad definition is a *group.Error (CodeInvalidArgument,
// CodeUnknownAgent).
func (h *Hub) Create(ctx context.Context, def group.Definition) (group.Group, error) {
	g, err := h.validate(def)
	if err != nil {
		return group.Group{}, err
	}
	now := nowISO()
	g.ID, g.CreatedAt, g.UpdatedAt = kernel.NewID(), now, now
	if err := h.d.Store().Save(ctx, g); err != nil {
		return group.Group{}, err
	}
	h.changed(g.ID, group.ChangeCreated)
	return g, nil
}

// Update changes a group; nil fields of patch stay. With opts.NoteIn (a session of the
// group, usually the one the user has open), the changes are written there as notes
// ("RFI Desk now leads.").
func (h *Hub) Update(ctx context.Context, groupID string, patch group.Patch, opts UpdateOptions) (group.Group, error) {
	before, err := h.require(ctx, groupID)
	if err != nil {
		return group.Group{}, err
	}
	def := group.Definition{
		Name:       deref(patch.Name, before.Name),
		Topic:      deref(patch.Topic, before.Topic),
		Members:    before.Members,
		LeadID:     deref(patch.LeadID, before.LeadID),
		ReplyMode:  deref(patch.ReplyMode, before.ReplyMode),
		Muted:      before.Muted,
		MemberView: deref(patch.MemberView, before.MemberView),
		Limits:     deref(patch.Limits, before.Limits),
	}
	if patch.Members != nil {
		def.Members = patch.Members
	}
	if patch.Muted != nil {
		def.Muted = patch.Muted
	}
	// A member who left is no longer muted.
	def.Muted = slices.DeleteFunc(slices.Clone(def.Muted), func(id string) bool { return !slices.Contains(def.Members, id) })
	g, err := h.validate(def)
	if err != nil {
		return group.Group{}, err
	}
	g.ID, g.CreatedAt, g.UpdatedAt = before.ID, before.CreatedAt, nowISO()
	if err := h.d.Store().Save(ctx, g); err != nil {
		return group.Group{}, err
	}
	h.changed(g.ID, group.ChangeUpdated)
	if opts.NoteIn != "" {
		if s, err := h.sessionOf(ctx, g.ID, opts.NoteIn); err == nil {
			for _, note := range h.describeChange(*before, g) {
				if _, err := h.th.note(ctx, g.ID, s.ID, note); err != nil {
					return g, err
				}
			}
		}
	}
	return g, nil
}

// Delete deletes a group with all its sessions and their threads. Running turns are
// stopped first.
func (h *Hub) Delete(ctx context.Context, groupID string) error {
	if _, err := h.require(ctx, groupID); err != nil {
		return err
	}
	sessions, err := h.ListSessions(ctx, groupID)
	if err != nil {
		return err
	}
	for _, s := range sessions {
		if err := h.DeleteSession(ctx, groupID, s.ID); err != nil {
			return err
		}
	}
	if err := h.d.Store().Delete(ctx, groupID); err != nil {
		return err
	}
	h.changed(groupID, group.ChangeDeleted)
	return nil
}

// ---------------------------------------------------------------------------
// Sessions
// ---------------------------------------------------------------------------

// StartSession starts a session of the group; title is optional.
func (h *Hub) StartSession(ctx context.Context, groupID, title string) (kernel.Session, error) {
	if _, err := h.require(ctx, groupID); err != nil {
		return kernel.Session{}, err
	}
	meta := ordered.NewObject()
	meta.Set("source", SessionSource)
	meta.Set("groupId", groupID)
	if t := jsstr.Trim(title); t != "" {
		meta.Set("title", t)
	}
	return h.d.Sessions().Create(ctx, meta)
}

// ListSessions returns the group's sessions, most recently active first.
func (h *Hub) ListSessions(ctx context.Context, groupID string) ([]kernel.Session, error) {
	filter := ordered.NewObject()
	filter.Set("source", SessionSource)
	filter.Set("groupId", groupID)
	sessions, err := h.d.Sessions().List(ctx, kernel.SessionFilter{Metadata: filter})
	if err != nil {
		return nil, err
	}
	slices.SortStableFunc(sessions, func(a, b kernel.Session) int { return cmp.Compare(b.UpdatedAt, a.UpdatedAt) })
	return sessions, nil
}

// GetSession returns one of the group's sessions, or nil and a nil error.
func (h *Hub) GetSession(ctx context.Context, groupID, sessionID string) (*kernel.Session, error) {
	s, err := h.d.Sessions().Get(ctx, sessionID)
	if err != nil || s == nil {
		return nil, err
	}
	if !IsGroupSession(s) {
		return nil, nil //nolint:nilnil // nil, nil means not one of the group's sessions
	}
	if id, _ := s.Metadata.Get("groupId"); id != groupID {
		return nil, nil //nolint:nilnil // nil, nil means not one of the group's sessions
	}
	return s, nil
}

// UpdateSession renames or pins a session. An unknown session is a *group.Error
// (CodeNotFound).
func (h *Hub) UpdateSession(ctx context.Context, groupID, sessionID string, fields SessionFields) (kernel.Session, error) {
	if _, err := h.sessionOf(ctx, groupID, sessionID); err != nil {
		return kernel.Session{}, err
	}
	updated, err := h.th.updateMetadata(ctx, sessionID, func(meta ordered.Object) ordered.Object {
		if fields.Title != nil {
			if title := jsstr.TruncateUTF16(jsstr.Trim(*fields.Title), 200); title != "" {
				meta.Set("title", title)
			} else {
				meta.Delete("title")
			}
		}
		if fields.Pinned != nil {
			if *fields.Pinned {
				meta.Set("pinned", true)
			} else {
				meta.Delete("pinned")
			}
		}
		return meta
	})
	if err != nil {
		return kernel.Session{}, err
	}
	if updated == nil {
		return kernel.Session{}, group.NewError("no such group session", group.CodeNotFound)
	}
	return *updated, nil
}

// DeleteSession deletes a session: its thread, its member sessions (and their messages)
// and the session. A running turn is stopped first.
func (h *Hub) DeleteSession(ctx context.Context, groupID, sessionID string) error {
	if _, err := h.sessionOf(ctx, groupID, sessionID); err != nil {
		return err
	}
	h.turns.stop(sessionID, "")
	if err := h.turns.idle(ctx, sessionID); err != nil {
		return err
	}
	h.th.settle(sessionID)
	sessions := h.d.Sessions()
	filter := ordered.NewObject()
	filter.Set("source", MemberSessionSource)
	filter.Set("groupSessionId", sessionID)
	members, err := sessions.List(ctx, kernel.SessionFilter{Metadata: filter})
	if err != nil {
		return err
	}
	for _, m := range members {
		if h.d.MemberMessages != nil {
			for _, store := range h.d.MemberMessages() {
				if !nilcheck.IsNil(store) {
					_ = store.Clear(ctx, m.ID) // best effort, like core/ts
				}
			}
		}
		if err := sessions.Delete(ctx, m.ID); err != nil {
			return err
		}
	}
	if err := h.d.Messages().DeleteBySession(ctx, sessionID); err != nil {
		return err
	}
	if err := sessions.Delete(ctx, sessionID); err != nil {
		return err
	}
	h.turns.forgetSession(sessionID)
	h.th.forget(sessionID)
	return nil
}

// Messages returns the session's thread, in order; afterSeq > 0 keeps only later
// messages.
func (h *Hub) Messages(ctx context.Context, groupID, sessionID string, afterSeq int) ([]group.Message, error) {
	if _, err := h.sessionOf(ctx, groupID, sessionID); err != nil {
		return nil, err
	}
	return h.th.list(ctx, sessionID, afterSeq)
}

// MemberSessions returns every current member's private session in a group session,
// created when missing - a host attaches its approval channel to each before a turn
// streams.
func (h *Hub) MemberSessions(ctx context.Context, groupID, sessionID string) ([]MemberSession, error) {
	g, err := h.require(ctx, groupID)
	if err != nil {
		return nil, err
	}
	if _, err := h.sessionOf(ctx, groupID, sessionID); err != nil {
		return nil, err
	}
	out := []MemberSession{}
	for _, agentID := range g.Members {
		if _, ok := h.turns.agent(agentID); !ok {
			continue
		}
		id, err := h.turns.memberSession(ctx, *g, sessionID, agentID)
		if err != nil {
			return nil, err
		}
		out = append(out, MemberSession{AgentID: agentID, SessionID: id})
	}
	return out, nil
}

// MemberSessionOf returns where a member session belongs (hosts route its approvals to
// the group's thread), or nil and a nil error.
func (h *Hub) MemberSessionOf(ctx context.Context, sessionID string) (*MemberSessionRef, error) {
	s, err := h.d.Sessions().Get(ctx, sessionID)
	if err != nil || !IsGroupMemberSession(s) {
		return nil, err
	}
	str := func(key string) string {
		v, _ := s.Metadata.Get(key)
		out, _ := v.(string)
		return out
	}
	ref := MemberSessionRef{GroupID: str("groupId"), GroupSessionID: str("groupSessionId"), AgentID: str("agentId")}
	if ref.GroupID == "" || ref.GroupSessionID == "" || ref.AgentID == "" {
		return nil, nil //nolint:nilnil // nil, nil means not a member session
	}
	return &ref, nil
}

// ---------------------------------------------------------------------------
// Turns
// ---------------------------------------------------------------------------

// Send sends a user message to a group session and runs the group turn it sets off; it
// returns when every member is done. Its progress streams as group:* events. Ending ctx
// stops the turn (like Stop). Refusals decided before anything runs are *group.Error
// (CodeBusy while another turn runs in the session).
func (h *Hub) Send(ctx context.Context, groupID, sessionID string, req SendRequest) (TurnResult, error) {
	g, err := h.require(ctx, groupID)
	if err != nil {
		return TurnResult{}, err
	}
	s, err := h.sessionOf(ctx, groupID, sessionID)
	if err != nil {
		return TurnResult{}, err
	}
	return h.turns.send(ctx, *g, *s, req)
}

// Retry runs a member's answer again, in place.
func (h *Hub) Retry(ctx context.Context, groupID, sessionID, messageID string) (TurnResult, error) {
	g, err := h.require(ctx, groupID)
	if err != nil {
		return TurnResult{}, err
	}
	s, err := h.sessionOf(ctx, groupID, sessionID)
	if err != nil {
		return TurnResult{}, err
	}
	m, err := h.d.Messages().Get(ctx, messageID)
	if err != nil {
		return TurnResult{}, err
	}
	if m == nil || m.SessionID != sessionID {
		return TurnResult{}, group.NewError("no such message in this session", group.CodeNotFound)
	}
	return h.turns.retry(ctx, *g, *s, *m)
}

// Stop stops the session's running turn, or (agentID != "") one member of it. False when
// nothing was running.
func (h *Hub) Stop(sessionID, agentID string) bool { return h.turns.stop(sessionID, agentID) }

// IsBusy reports whether a group turn runs in the session.
func (h *Hub) IsBusy(sessionID string) bool { return h.turns.isBusy(sessionID) }

// Presence returns what each member of the session's running turn is doing (empty when
// idle).
func (h *Hub) Presence(sessionID string) []events.GroupPresencePayload {
	return h.turns.presenceOf(sessionID)
}

// Running returns every running group turn, with who is doing what.
func (h *Hub) Running() []RunningTurn { return h.turns.running() }

// Dispose stops every running turn and stops listening.
func (h *Hub) Dispose() {
	h.turns.dispose()
	for _, off := range h.unsubscribe {
		off()
	}
	h.unsubscribe = nil
}

// ---------------------------------------------------------------------------
// Internals
// ---------------------------------------------------------------------------

func (h *Hub) require(ctx context.Context, groupID string) (*group.Group, error) {
	g, err := h.d.Store().Get(ctx, groupID)
	if err != nil {
		return nil, err
	}
	if g == nil {
		return nil, group.NewError("no such group", group.CodeNotFound)
	}
	return g, nil
}

func (h *Hub) sessionOf(ctx context.Context, groupID, sessionID string) (*kernel.Session, error) {
	s, err := h.GetSession(ctx, groupID, sessionID)
	if err != nil {
		return nil, err
	}
	if s == nil {
		return nil, group.NewError("no such session in this group", group.CodeNotFound)
	}
	return s, nil
}

var (
	replyModes  = []group.ReplyMode{group.ReplyLead, group.ReplyAll, group.ReplyMention}
	memberViews = []group.MemberView{group.ViewFull, group.ViewAddressed}
)

// validate checks and normalizes a definition (A-107); the id and times are left
// empty.
func (h *Hub) validate(def group.Definition) (group.Group, error) {
	invalid := func(msg string) (group.Group, error) {
		return group.Group{}, group.NewError(msg, group.CodeInvalidArgument)
	}
	name := jsstr.Trim(def.Name)
	if name == "" {
		return invalid(`"name" is required`)
	}
	if n := kernel.UTF16Len(name); n > group.MaxNameChars {
		return invalid(`"name" is ` + strconv.Itoa(n) + " characters; keep it under " + strconv.Itoa(group.MaxNameChars))
	}
	topic := jsstr.Trim(def.Topic)
	if n := kernel.UTF16Len(topic); n > group.MaxTopicChars {
		return invalid(`"topic" is ` + strconv.Itoa(n) + " characters; keep it under " + strconv.Itoa(group.MaxTopicChars))
	}
	members := []string{}
	for _, m := range def.Members {
		if m = jsstr.Trim(m); m != "" && !slices.Contains(members, m) {
			members = append(members, m)
		}
	}
	if len(members) < 2 {
		return invalid("a group needs at least 2 members")
	}
	if len(members) > group.MaxMembers {
		return invalid("a group has at most " + strconv.Itoa(group.MaxMembers) + " members")
	}
	var unknown []string
	for _, id := range members {
		if _, ok := h.turns.agent(id); !ok {
			unknown = append(unknown, id)
		}
	}
	if len(unknown) > 0 {
		return group.Group{}, group.NewError("not a running agent: "+strings.Join(unknown, ", "), group.CodeUnknownAgent)
	}
	leadID := jsstr.Trim(def.LeadID)
	if !slices.Contains(members, leadID) {
		return invalid("the lead must be one of the members")
	}
	replyMode := def.ReplyMode
	if replyMode == "" {
		replyMode = group.ReplyLead
	}
	if !slices.Contains(replyModes, replyMode) {
		return invalid(`"replyMode" must be one of lead, all, mention`)
	}
	memberView := def.MemberView
	if memberView == "" {
		memberView = group.ViewFull
	}
	if !slices.Contains(memberViews, memberView) {
		return invalid(`"memberView" must be one of full, addressed`)
	}
	muted := []string{}
	for _, m := range def.Muted {
		if m = jsstr.Trim(m); !slices.Contains(muted, m) {
			muted = append(muted, m)
		}
	}
	if slices.Contains(muted, leadID) {
		return invalid("the lead can't be muted")
	}
	var strangers []string
	for _, id := range muted {
		if !slices.Contains(members, id) {
			strangers = append(strangers, id)
		}
	}
	if len(strangers) > 0 {
		return invalid("muted agents must be members: " + strings.Join(strangers, ", "))
	}
	return group.Group{Name: name, Topic: topic, Members: members, LeadID: leadID, ReplyMode: replyMode, Muted: muted, MemberView: memberView, Limits: def.Limits}, nil
}

var replyLabels = map[group.ReplyMode]string{
	group.ReplyLead:    "Lead routes",
	group.ReplyAll:     "Everyone answers",
	group.ReplyMention: "Only when mentioned",
}

// describeChange returns the notes a change leaves in the thread.
func (h *Hub) describeChange(before, after group.Group) []string {
	name := h.turns.nameOf
	quote := func(s string) string { return "“" + s + "”" }
	var notes []string
	if before.Name != after.Name {
		notes = append(notes, "The group is now called "+quote(after.Name)+".")
	}
	if before.Topic != after.Topic {
		if after.Topic != "" {
			notes = append(notes, "The group's purpose is now: "+after.Topic)
		} else {
			notes = append(notes, "The group's purpose was cleared.")
		}
	}
	for _, id := range after.Members {
		if !slices.Contains(before.Members, id) {
			notes = append(notes, "You added "+name(id)+".")
		}
	}
	for _, id := range before.Members {
		if !slices.Contains(after.Members, id) {
			notes = append(notes, "You removed "+name(id)+".")
		}
	}
	if before.LeadID != after.LeadID {
		notes = append(notes, name(after.LeadID)+" now leads.")
	}
	if before.ReplyMode != after.ReplyMode {
		notes = append(notes, "Who answers is now "+quote(replyLabels[after.ReplyMode])+".")
	}
	for _, id := range after.Muted {
		if !slices.Contains(before.Muted, id) {
			notes = append(notes, name(id)+" is muted.")
		}
	}
	for _, id := range before.Muted {
		if !slices.Contains(after.Muted, id) && slices.Contains(after.Members, id) {
			notes = append(notes, name(id)+" is back.")
		}
	}
	if before.MemberView != after.MemberView {
		if after.MemberView == group.ViewAddressed {
			notes = append(notes, "Members now see only what is addressed to them.")
		} else {
			notes = append(notes, "Members now see the whole thread.")
		}
	}
	return notes
}

// removeAgentEverywhere is the agent:removed subscriber: the agent leaves every group; a
// lead hands over to the next member.
func (h *Hub) removeAgentEverywhere(ctx context.Context, agentID string) error {
	groups, err := h.d.Store().List(ctx)
	if err != nil {
		return err
	}
	var errs []error
	for _, g := range groups {
		if !slices.Contains(g.Members, agentID) {
			continue
		}
		next := g.Clone()
		next.Members = slices.DeleteFunc(next.Members, func(id string) bool { return id == agentID })
		if next.LeadID == agentID {
			next.LeadID = ""
			if len(next.Members) > 0 {
				next.LeadID = next.Members[0]
			}
		}
		next.Muted = slices.DeleteFunc(next.Muted, func(id string) bool { return id == agentID || id == next.LeadID })
		next.UpdatedAt = nowISO()
		if err := h.d.Store().Save(ctx, next); err != nil {
			errs = append(errs, err)
			continue
		}
		h.changed(g.ID, group.ChangeUpdated)
	}
	return errors.Join(errs...)
}

func (h *Hub) changed(groupID string, change group.Change) {
	events.Emit(h.d.Events, events.GroupChanged, "", events.GroupChangedPayload{GroupID: groupID, Change: change})
}

// deref is *p, or def when p is nil.
func deref[T any](p *T, def T) T {
	if p == nil {
		return def
	}
	return *p
}
