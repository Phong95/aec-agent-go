package aecmock

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"sync/atomic"

	"github.com/Phong95/aec-agent-go/components/channelplatformhub/channel"
)

// FakeSentMessage is a message a FakeChannelAdapter "sent". Edit updates it in place.
type FakeSentMessage struct {
	channel.MessageRef
	Target channel.Target
	Text   string
	// Format is OutboundMessage.Format ("" for plain).
	Format  string
	Buttons [][]channel.Button
	// Edits is how many times it was edited.
	Edits int
}

// FakeChannelPlatform is an in-memory chat platform for tests and demos (A-100), the
// FakeChannelPlatform of core/ts: no network. Push messages in with
// FakeChannelAdapter.Receive and read the replies from Sent. A channel whose "token"
// credential is "bad" fails to connect. It can edit its messages (Edit updates Sent), and
// FakeChannelAdapter.Press presses one of their buttons. It is safe for concurrent use.
type FakeChannelPlatform struct {
	name string
	caps channel.Capabilities

	mu       sync.Mutex
	adapters []*FakeChannelAdapter
}

var _ channel.Platform = (*FakeChannelPlatform)(nil)

// NewFakeChannelPlatform returns a platform named name (default "fake") whose adapters
// split replies at maxMessageLength (default 4000).
func NewFakeChannelPlatform(name string, maxMessageLength int) *FakeChannelPlatform {
	if name == "" {
		name = "fake"
	}
	if maxMessageLength <= 0 {
		maxMessageLength = 4000
	}
	return &FakeChannelPlatform{name: name, caps: channel.Capabilities{MaxMessageLength: maxMessageLength}}
}

// NewFakeChannelPlatformWithCapabilities returns a platform named name (default "fake")
// whose adapters report caps (a MaxMessageLength of 0 means 4000), e.g. with Markdown or
// Buttons set.
func NewFakeChannelPlatformWithCapabilities(name string, caps channel.Capabilities) *FakeChannelPlatform {
	p := NewFakeChannelPlatform(name, caps.MaxMessageLength)
	caps.MaxMessageLength = p.caps.MaxMessageLength
	p.caps = caps
	return p
}

// Name returns the platform name.
func (p *FakeChannelPlatform) Name() string { return p.name }

// DisplayName returns "Fake".
func (p *FakeChannelPlatform) DisplayName() string { return "Fake" }

// CredentialKeys returns ["token"].
func (p *FakeChannelPlatform) CredentialKeys() []string { return []string{"token"} }

// New builds an adapter for the channel.
func (p *FakeChannelPlatform) New(ch channel.Channel) (channel.Adapter, error) {
	a := &FakeChannelAdapter{ch: ch.Clone(), caps: p.caps}
	p.mu.Lock()
	p.adapters = append(p.adapters, a)
	p.mu.Unlock()
	return a, nil
}

// Adapters returns every adapter built, oldest first.
func (p *FakeChannelPlatform) Adapters() []*FakeChannelAdapter {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.adapters)
}

// AdapterFor returns the started adapter of a channel, or nil.
func (p *FakeChannelPlatform) AdapterFor(channelID string) *FakeChannelAdapter {
	p.mu.Lock()
	defer p.mu.Unlock()
	for i := len(p.adapters) - 1; i >= 0; i-- {
		if a := p.adapters[i]; a.ch.ID == channelID && a.Started() {
			return a
		}
	}
	return nil
}

// FakeChannelAdapter is the adapter of FakeChannelPlatform.
type FakeChannelAdapter struct {
	ch   channel.Channel
	caps channel.Capabilities

	mu      sync.Mutex
	sink    channel.Sink
	started bool
	sent    []FakeSentMessage
	typing  []channel.Target
	nextID  int
	cmds    []channel.BotCommand
}

var (
	_ channel.Adapter       = (*FakeChannelAdapter)(nil)
	_ channel.TypingSender  = (*FakeChannelAdapter)(nil)
	_ channel.Editor        = (*FakeChannelAdapter)(nil)
	_ channel.CommandSetter = (*FakeChannelAdapter)(nil)
)

// Channel returns the channel the adapter was built for.
func (a *FakeChannelAdapter) Channel() channel.Channel { return a.ch.Clone() }

// Capabilities returns the platform's capabilities.
func (a *FakeChannelAdapter) Capabilities() channel.Capabilities { return a.caps }

// Start connects, unless the token is "bad".
func (a *FakeChannelAdapter) Start(_ context.Context, sink channel.Sink) (channel.AccountInfo, error) {
	token := a.ch.Credentials["token"]
	if token == "bad" {
		return channel.AccountInfo{}, errors.New("invalid token")
	}
	a.mu.Lock()
	a.sink, a.started = sink, true
	a.mu.Unlock()
	return channel.AccountInfo{ID: "bot-" + token, Username: "fake_bot", DisplayName: "Fake Bot"}, nil
}

// Stop disconnects.
func (a *FakeChannelAdapter) Stop(context.Context) error {
	a.mu.Lock()
	a.sink, a.started = nil, false
	a.mu.Unlock()
	return nil
}

// Started reports whether the adapter is connected.
func (a *FakeChannelAdapter) Started() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.started
}

// Send records the message.
func (a *FakeChannelAdapter) Send(_ context.Context, target channel.Target, m channel.OutboundMessage) (channel.MessageRef, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.nextID++
	ref := channel.MessageRef{ChatID: target.ChatID, MessageID: fmt.Sprintf("out-%d", a.nextID)}
	a.sent = append(a.sent, FakeSentMessage{MessageRef: ref, Target: target, Text: m.Text, Format: m.Format, Buttons: cloneButtons(m.Buttons)})
	return ref, nil
}

// Edit replaces a sent message's text, format and buttons (none given = removed).
func (a *FakeChannelAdapter) Edit(_ context.Context, ref channel.MessageRef, m channel.OutboundMessage) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	for i := range a.sent {
		if a.sent[i].ChatID == ref.ChatID && a.sent[i].MessageID == ref.MessageID {
			a.sent[i].Text, a.sent[i].Format, a.sent[i].Buttons = m.Text, m.Format, cloneButtons(m.Buttons)
			a.sent[i].Edits++
			return nil
		}
	}
	return fmt.Errorf("aecmock: no message %s to edit", ref.MessageID)
}

func cloneButtons(rows [][]channel.Button) [][]channel.Button {
	if len(rows) == 0 {
		return nil
	}
	out := make([][]channel.Button, len(rows))
	for i, row := range rows {
		out[i] = slices.Clone(row)
	}
	return out
}

// SendTyping records the typing action.
func (a *FakeChannelAdapter) SendTyping(_ context.Context, target channel.Target) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.typing = append(a.typing, target)
	return nil
}

// SetCommands records the published commands.
func (a *FakeChannelAdapter) SetCommands(_ context.Context, commands []channel.BotCommand) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.cmds = slices.Clone(commands)
	return nil
}

// Commands returns the commands last published with SetCommands.
func (a *FakeChannelAdapter) Commands() []channel.BotCommand {
	a.mu.Lock()
	defer a.mu.Unlock()
	return slices.Clone(a.cmds)
}

// Sent returns the messages sent so far, oldest first.
func (a *FakeChannelAdapter) Sent() []FakeSentMessage {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := slices.Clone(a.sent)
	for i := range out {
		out[i].Buttons = cloneButtons(out[i].Buttons)
	}
	return out
}

// SentTexts returns the texts sent so far, oldest first.
func (a *FakeChannelAdapter) SentTexts() []string {
	var out []string
	for _, m := range a.Sent() {
		out = append(out, m.Text)
	}
	return out
}

// Typing returns the typing actions sent so far.
func (a *FakeChannelAdapter) Typing() []channel.Target {
	a.mu.Lock()
	defer a.mu.Unlock()
	return slices.Clone(a.typing)
}

// errFakeNotStarted is returned by Receive and Report on an adapter that is not started.
var errFakeNotStarted = errors.New("aecmock: the fake channel adapter is not started")

// Receive delivers an inbound message as the platform would. It fails when the adapter
// is not started.
func (a *FakeChannelAdapter) Receive(m channel.InboundMessage) error {
	a.mu.Lock()
	sink := a.sink
	a.mu.Unlock()
	if sink == nil {
		return errFakeNotStarted
	}
	sink.Message(m)
	return nil
}

// Press presses the button labelled label on the newest sent message that has it, as
// sender in that message's chat of type chatType. It fails when the adapter is not
// started or no message has such a button.
func (a *FakeChannelAdapter) Press(label string, sender channel.Sender, chatType channel.ChatType) error {
	a.mu.Lock()
	sink := a.sink
	var press *channel.ButtonPress
	for i := len(a.sent) - 1; i >= 0 && press == nil; i-- {
		for _, row := range a.sent[i].Buttons {
			for _, b := range row {
				if b.Label == label && press == nil {
					press = &channel.ButtonPress{
						Chat: channel.Chat{ID: a.sent[i].ChatID, Type: chatType}, Sender: sender, MessageID: a.sent[i].MessageID, Data: b.Data,
					}
				}
			}
		}
	}
	a.mu.Unlock()
	if sink == nil {
		return errFakeNotStarted
	}
	if press == nil {
		return fmt.Errorf("aecmock: no button %q", label)
	}
	sink.Button(*press)
	return nil
}

// Report reports a status as the platform would; AdapterFailed also stops the adapter.
// It fails when the adapter is not started.
func (a *FakeChannelAdapter) Report(status channel.AdapterStatus, errText string) error {
	a.mu.Lock()
	sink := a.sink
	if status == channel.AdapterFailed {
		a.started = false
	}
	a.mu.Unlock()
	if sink == nil {
		return errFakeNotStarted
	}
	sink.Status(status, errText)
	return nil
}

var fakeMessageID atomic.Int64

// FakeDirectMessage is a direct message from senderID in the chat "chat-<senderID>", for
// FakeChannelAdapter.Receive.
func FakeDirectMessage(senderID, text string) channel.InboundMessage {
	return channel.InboundMessage{
		MessageID: fmt.Sprintf("in-%d", fakeMessageID.Add(1)),
		Chat:      channel.Chat{ID: "chat-" + senderID, Type: channel.ChatDM},
		Sender:    channel.Sender{ID: senderID, Name: "User " + senderID},
		Text:      text,
	}
}

// FakeGroupMessage is a group-chat message from senderID in groupID (titled "Group
// <groupID>"), for FakeChannelAdapter.Receive.
func FakeGroupMessage(groupID, senderID, text string) channel.InboundMessage {
	return channel.InboundMessage{
		MessageID: fmt.Sprintf("in-%d", fakeMessageID.Add(1)),
		Chat:      channel.Chat{ID: groupID, Type: channel.ChatGroup, Title: "Group " + groupID},
		Sender:    channel.Sender{ID: senderID, Name: "User " + senderID},
		Text:      text,
	}
}
