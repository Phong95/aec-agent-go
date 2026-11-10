package agent

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/Phong95/aec-agent-go/components/agent/conversation"
	"github.com/Phong95/aec-agent-go/components/agent/job"
	"github.com/Phong95/aec-agent-go/components/agent/subagent"
	"github.com/Phong95/aec-agent-go/components/channelplatformhub/channel"
	"github.com/Phong95/aec-agent-go/components/events"
	"github.com/Phong95/aec-agent-go/kernel"
)

// channelToolPort is what the channel tool gets (toolhub.Context.Channels), wired by the
// Agent (A-219).
type channelToolPort struct{ m *channelManager }

var _ channel.ToolPort = channelToolPort{}

// conversationReach (A-220) says how far the conversations tool reaches for this session.
// Agent-wide only for a direct message from a sender in AllowFrom on a channel set to
// "agent"; every other chat app turn sees its own chat. A chat's session with no channel
// turn running in it (opened from another client) keeps its own chat too: the narrower
// answer when unsure.
func (m *channelManager) conversationReach(session kernel.Session) conversation.Reach {
	m.mu.Lock()
	defer m.mu.Unlock()
	chat, ok := m.activeTurns[session.ID]
	if !ok {
		if source, _ := session.Metadata.Get("source"); source == "channel" {
			return conversation.ReachChat
		}
		return conversation.ReachAgent
	}
	lc := m.live[chat.channelID]
	if lc != nil && chat.chatType == channel.ChatDM &&
		lc.ch.Access.ConversationAccess == channel.ConversationAccessAgent &&
		slices.Contains(lc.ch.Access.AllowFrom, chat.initiator.ID) {
		return conversation.ReachAgent
	}
	return conversation.ReachChat
}

// ToolList is the running channels, for the channel tool.
func (p channelToolPort) ToolList(ctx context.Context, sessionID string) ([]channel.ToolInfo, error) {
	m := p.m
	if err := m.refuseUnattended(ctx, sessionID); err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	out := []channel.ToolInfo{}
	for id, lc := range m.live {
		if m.states[id].Status == channel.StatusFailed {
			continue
		}
		info := channel.ToolInfo{ChannelID: id, Name: lc.ch.Name, Platform: lc.ch.Platform, HasHome: lc.ch.Home != nil}
		if lc.ch.Home != nil {
			info.HomeTitle = lc.ch.Home.Title
		}
		out = append(out, info)
	}
	slices.SortFunc(out, func(a, b channel.ToolInfo) int { return strings.Compare(a.Name, b.Name) })
	return out, nil
}

// ToolSend posts text to a channel's home chat for the channel tool. Unlike a job's
// delivery it reports what happened, with an error whose message is for the model.
//
// Adding the text to the chat's conversation is not waited for: it waits for that chat's
// turn, and two turns each posting to the other's chat would then wait for each other.
func (p channelToolPort) ToolSend(ctx context.Context, sessionID string, in channel.SendInput) (channel.SendResult, error) {
	m := p.m
	if err := m.refuseUnattended(ctx, sessionID); err != nil {
		return channel.SendResult{}, err
	}
	all, err := m.List(ctx)
	if err != nil {
		return channel.SendResult{}, err
	}
	ch, err := pickHome(all, in.Channel)
	if err != nil {
		return channel.SendResult{}, err
	}
	target := channel.Target{ChatID: ch.Home.ChatID, ThreadID: ch.Home.ThreadID}
	sent := channel.SendResult{ChannelID: ch.ID, Name: ch.Name, Platform: ch.Platform, HomeTitle: ch.Home.Title}

	m.mu.Lock()
	chat, inTurn := m.activeTurns[sessionID]
	m.mu.Unlock()
	if inTurn && chat.channelID == ch.ID && isHomeTarget(ch.Home, chat.target) {
		sent.Here = true
		return sent, nil
	}
	failed := func(errText string, code channel.ErrorCode) error {
		m.d.logger.Warn("[channels] could not send", "agent", m.d.agentID, "channel", ch.ID, "error", errText)
		events.Emit(m.d.bus, events.ChannelFailed, sessionID, events.ChannelFailedPayload{
			AgentID: m.d.agentID, ChannelID: ch.ID, Platform: ch.Platform, ChatID: target.ChatID, Error: errText,
		})
		return channel.NewError(errText, code, ch.ID)
	}
	// A channel still connecting (right after a restart) is waited for.
	m.lockOps(ch.ID)()
	m.mu.Lock()
	lc := m.live[ch.ID]
	m.mu.Unlock()
	if lc == nil || m.State(ch.ID).Status == channel.StatusFailed {
		return channel.SendResult{}, failed(fmt.Sprintf(`"%s" is not connected`, ch.Name), channel.CodeNotRunning)
	}
	refs, err := m.sendFormatted(ctx, lc, target, in.Text)
	if err != nil {
		return channel.SendResult{}, failed(platformLabel(lc.platform)+" did not take the message: "+err.Error(), channel.CodeConnectFailed)
	}
	events.Emit(m.d.bus, events.ChannelMessageSent, sessionID, events.ChannelMessageSentPayload{
		AgentID: m.d.agentID, ChannelID: ch.ID, Platform: ch.Platform, ChatID: target.ChatID, Parts: len(refs),
	})
	stored := ch
	platform := lc.platform
	m.goTracked(func() {
		m.recordSent(m.recordCtx, stored, platform, target, in.Text, "the sent message") //nolint:contextcheck // outlives the tool call on purpose; ended by close
	})
	sent.Parts = len(refs)
	return sent, nil
}

// refuseUnattended stops job runs and subagents: they have no user to approve a send, and
// jobs have deliveries.
func (m *channelManager) refuseUnattended(ctx context.Context, sessionID string) error {
	session, err := m.d.sessions.Get(ctx, sessionID)
	if err != nil {
		return err
	}
	if session != nil && (session.JobID != "" || subagent.IsSession(session)) {
		return channel.NewError("job runs and subagents cannot post to chats", channel.CodeInvalidArgument, "")
	}
	return nil
}

// pickHome is the channel the tool named, which must have a home chat. The error's message
// lists the choices.
func pickHome(channels []channel.Channel, name string) (channel.Channel, error) {
	invalid := func(message string) error { return channel.NewError(message, channel.CodeInvalidArgument, "") }
	ref := func(c channel.Channel) job.HomeChat {
		return job.HomeChat{ChannelID: c.ID, Name: c.Name, Platform: c.Platform}
	}
	var withHome []channel.Channel
	var homes []job.HomeChat
	for _, c := range channels {
		if c.Home != nil {
			withHome = append(withHome, c)
			homes = append(homes, ref(c))
		}
	}
	if name == "" {
		switch len(withHome) {
		case 1:
			return withHome[0], nil
		case 0:
			return channel.Channel{}, invalid(job.HomesText(homes))
		}
		return channel.Channel{}, invalid("several channels have a home chat, so name one — " + job.HomesText(homes))
	}
	picked, errText := job.MatchHomes(homes, []string{name})
	if errText != "" {
		for _, c := range channels {
			if ref(c).Named(name) {
				return channel.Channel{}, invalid(fmt.Sprintf(`"%s" has no home chat yet (the user sends /sethome in the chat that should get messages)`, c.Name))
			}
		}
		return channel.Channel{}, invalid(errText)
	}
	if len(picked) > 1 {
		return channel.Channel{}, invalid(fmt.Sprintf(`"%s" matches several channels, so use its name %s %s`, name, "—", job.HomesText(homes)))
	}
	for _, c := range withHome {
		if c.ID == picked[0].ChannelID {
			return c, nil
		}
	}
	return channel.Channel{}, invalid(job.HomesText(homes))
}
