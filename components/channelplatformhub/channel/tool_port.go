package channel

import "context"

// ToolInfo is a running channel, as the channel tool shows it (A-219). Never a chat id: the
// model cannot name a chat.
type ToolInfo struct {
	ChannelID string
	Name      string
	Platform  string
	HasHome   bool
	// HomeTitle is the home chat's label, when it has one.
	HomeTitle string
}

// SendInput is one post of the channel tool.
type SendInput struct {
	Text string
	// Channel is the channel's name, platform or id, ignoring case. It may be empty when
	// exactly one channel has a home chat.
	Channel string
}

// SendResult says what a post did.
type SendResult struct {
	ChannelID string
	Name      string
	Platform  string
	// HomeTitle is the home chat's label, when it has one.
	HomeTitle string
	// Parts is how many chat messages the text became; 0 when Here is true.
	Parts int
	// Here means the home chat is the chat this turn came from, so nothing was posted: the
	// reply goes there anyway.
	Here bool
}

// ToolPort is toolhub.Context.Channels (A-219): what the channel tool may do, and nothing
// more. Channels is the host's API (credentials, delete, any chat id), so it is not handed
// to a tool. A request/response port rather than pub-sub, because the tool must tell the
// model in the same call whether the message arrived. Each call names the session asking:
// job runs and subagents are refused here.
type ToolPort interface {
	// ToolList is the running channels, by name.
	ToolList(ctx context.Context, sessionID string) ([]ToolInfo, error)
	// ToolSend posts the text, exactly as given, to a channel's home chat. The error's
	// message is for the model.
	ToolSend(ctx context.Context, sessionID string, in SendInput) (SendResult, error)
}
