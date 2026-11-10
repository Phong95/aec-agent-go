// Package channels holds the channel tool (A-219): the agent posts a message to one of its
// chat apps when the user asks. It reaches them only through toolhub.Context.Channels.
package channels

import (
	"context"
	"slices"
	"strconv"
	"strings"

	"github.com/Phong95/aec-agent-go/components/channelplatformhub/channel"
	"github.com/Phong95/aec-agent-go/components/toolhub"
	"github.com/Phong95/aec-agent-go/internal/nilcheck"
	"github.com/Phong95/aec-agent-go/internal/toolkit"
	"github.com/Phong95/aec-agent-go/kernel"
	"github.com/Phong95/aec-agent-go/kernel/ordered"
)

var actions = []string{"list", "send"}

// Actions returns the actions of the channel tool, in the order of its enum (CHANNEL_ACTIONS
// of core/ts), as a fresh slice.
func Actions() []string { return slices.Clone(actions) }

// ActionOf is the call's action, trimmed and lower-cased; "" when it is not one of Actions.
func ActionOf(args kernel.Arguments) string {
	s, _ := toolkit.DecodeArgs(args).String("action")
	action := strings.ToLower(strings.TrimSpace(s))
	if slices.Contains(actions, action) {
		return action
	}
	return ""
}

// Tool is the channel tool: post a message to one of the agent's chat apps when the user
// asks, from any conversation ("summarize this and send it to me on Telegram").
//
// Only home chats, chosen by channel: the model never sees or types a chat id, so it cannot
// be talked into posting to an arbitrary chat. send is consequential, so it asks the user
// first and the prompt shows the channel and the text; list is read-only per call. The
// definition states the worst case. Subagents never get it (the child exclusion list).
type Tool struct{}

var (
	_ toolhub.Handler    = (*Tool)(nil)
	_ toolhub.CallRisker = (*Tool)(nil)
)

// NewTool returns the channel tool.
func NewTool() *Tool { return &Tool{} }

// Name returns the plugin name.
func (*Tool) Name() string { return "tool:channel" }

// Version returns the plugin version.
func (*Tool) Version() string { return "1.0.0" }

// RiskFor is the risk of one call: list is read-only; send (and an unknown action) is
// consequential.
func (*Tool) RiskFor(args kernel.Arguments) toolhub.Risk {
	if ActionOf(args) == "list" {
		return toolhub.RiskReadOnly
	}
	return toolhub.RiskConsequential
}

// Definition returns the tool definition (a fresh value on every call).
func (*Tool) Definition() toolhub.Definition {
	return toolhub.Definition{
		Name: "channel",
		Description: "Post a message to a chat app (Telegram, Slack, ...) when the user asks you to send something there. Actions: send " +
			"(text, and channel when several have a home chat) posts the text exactly as you write it to that channel's home chat, " +
			"after asking the user · list shows the running channels and which have a home chat. Write the whole message in text: " +
			"the chat sees nothing else from this conversation. To answer in the chat you are talking in, just reply.",
		Parameters: toolhub.JSONSchema{
			Type: "object",
			Properties: toolhub.NewProperties(
				toolhub.Property{Name: "action", Schema: toolhub.JSONSchema{Type: "string", Enum: slices.Clone(actions), Description: "send or list"}},
				toolhub.Property{Name: "text", Schema: toolhub.JSONSchema{Type: "string", Description: "send: the exact message to post"}},
				toolhub.Property{Name: "channel", Schema: toolhub.JSONSchema{Type: "string", Description: "send: the channel's name or platform; may be left out when only one channel has a home chat"}},
			),
			Required: []string{"action"},
		},
		Risk:     toolhub.RiskConsequential,
		Subagent: toolhub.SubagentNever,
	}
}

// Execute runs one call.
func (*Tool) Execute(ctx context.Context, args kernel.Arguments, tc toolhub.Context) (toolhub.Output, error) {
	action := ActionOf(args)
	if action == "" {
		return toolkit.Fail(`channel: "action" must be one of ` + strings.Join(actions, ", ")), nil
	}
	if nilcheck.IsNil(tc.Channels) {
		return toolkit.Fail("channel: chat apps are not available here"), nil
	}
	a := toolkit.DecodeArgs(args)
	meta := ordered.NewObject()
	meta.Set("action", action)
	if action == "list" {
		list, err := tc.Channels.ToolList(ctx, tc.SessionID)
		if err != nil {
			return failed(action, err), nil
		}
		meta.Set("count", len(list))
		return toolkit.Result(listText(list), meta), nil
	}
	text, _ := a.String("text")
	if strings.TrimSpace(text) == "" {
		return toolkit.Fail(`channel send: "text" is required`), nil
	}
	name, _ := a.String("channel")
	result, err := tc.Channels.ToolSend(ctx, tc.SessionID, channel.SendInput{Text: text, Channel: strings.TrimSpace(name)})
	if err != nil {
		return failed(action, err), nil
	}
	meta.Set("channelId", result.ChannelID)
	meta.Set("parts", result.Parts)
	return toolkit.Result(sentText(result), meta), nil
}

// failed is never retried on its own: part of a long message may already be in the chat.
func failed(action string, err error) toolhub.Output {
	return toolkit.Fail("channel " + action + " failed: " + err.Error())
}

func listText(channels []channel.ToolInfo) string {
	if len(channels) == 0 {
		return "No chat app is connected to you."
	}
	lines := []string{"Connected chat apps:"}
	for _, c := range channels {
		state := "no home chat, the user sends /sethome in the chat that should get messages"
		if c.HasHome {
			state = "home chat set"
			if c.HomeTitle != "" {
				state = `home chat "` + c.HomeTitle + `"`
			}
		}
		lines = append(lines, `- "`+c.Name+`" (`+c.Platform+"): "+state)
	}
	return strings.Join(lines, "\n")
}

func sentText(r channel.SendResult) string {
	if r.Here {
		return `This is the home chat of "` + r.Name + `", the chat you are talking in. Nothing was posted: just reply here.`
	}
	chat := "the home chat"
	if r.HomeTitle != "" {
		chat = `"` + r.HomeTitle + `"`
	}
	plural := "s"
	if r.Parts == 1 {
		plural = ""
	}
	return "Sent to " + chat + " on " + r.Name + " (" + r.Platform + "), in " + strconv.Itoa(r.Parts) + " message" + plural + "."
}

// ToolNames returns the names of the channel tools, for agent allowlists, as a fresh slice
// (CHANNEL_TOOL_NAMES of core/ts).
func ToolNames() []string { return []string{"channel"} }

// NewTools returns the channel tool, in a list like the other factories. Register it with
// AgentCore.UseTool.
func NewTools() []toolhub.Handler { return []toolhub.Handler{NewTool()} }
