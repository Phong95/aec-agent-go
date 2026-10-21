// Package command holds the contracts of slash commands (A-136): "/compact", "/resume",
// "/permissions yolo"... typed in a conversation and answered by the agent itself, not
// by the model. The typed contract of a command (Spec), the context its handler gets
// (RunContext), and its outcome (Result, with an Action for the client to carry out).
//
// The agent's command manager (package agent) implements Commands: the registry (the
// built-ins, the host's - AgentCore.UseCommand - and the agent's own -
// Agent.Commands().Register), the parser (Parse, shared with the chat-app channels) and
// the dispatcher. Every client uses it: the web app through the server, chat apps
// through the agent's channel manager.
//
// The JSON shapes of Info, Result and Action are the wire shapes of core/ts
// (CommandInfo, CommandResult, CommandAction), so the Go and TypeScript servers answer
// with the same HTTP bodies.
//
// This package is a type-only leaf (it imports only kernel and kernel/ordered), so agent
// and the root can both import it without a cycle.
package command

import (
	"context"

	"github.com/Phong95/aec-agent-go/kernel/ordered"
)

// Safety is what running a command can do, for confirmation and access checks.
type Safety string

// The safety levels. The string values are stable wire values.
const (
	// SafetyReadOnly: only reads (/help, /status, /context, /usage, /diff). Never changes
	// files or session data.
	SafetyReadOnly Safety = "read-only"
	// SafetySession: changes this conversation (/clear, /compact, /rename, /model, /resume).
	SafetySession Safety = "session"
	// SafetyPrivileged: can widen what the agent may do without asking (/permissions
	// yolo). A client confirms it first (Invocation.Confirmed); a chat app lets only the
	// bot's owners run it.
	SafetyPrivileged Safety = "privileged"
)

// ClientCapability is what a client can do itself - a command that needs one is offered
// only where the client says it has it (Invocation.Capabilities).
type ClientCapability string

// The client capabilities. The string values are stable wire values.
const (
	// CapabilityClipboard: copy text for the user (/copy).
	CapabilityClipboard ClientCapability = "clipboard"
	// CapabilityExit: close the client (/exit).
	CapabilityExit ClientCapability = "exit"
)

// WhileBusy is what to do with a command when a turn is running in the conversation.
type WhileBusy string

// The busy rules. The string values are stable wire values.
const (
	// WhileBusyRun (the default, also for "") runs the command at once.
	WhileBusyRun WhileBusy = "run"
	// WhileBusyRefuse answers that the conversation is busy (/compact must not change the
	// working set under a running turn).
	WhileBusyRefuse WhileBusy = "refuse"
)

// Argument is one argument of a command - for usage text, autocomplete and validation.
type Argument struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	// Required: a missing required argument is an "invalid_arguments" result. Default false.
	Required bool `json:"required,omitempty"`
	// Choices are the only accepted values (matched case-insensitively), when there is a
	// fixed set.
	Choices []string `json:"choices,omitempty"`
	// Rest takes the rest of the line (free text, e.g. /compact <focus>). Only the last
	// argument.
	Rest bool `json:"rest,omitempty"`
}

// Spec is a command: its metadata (help, autocomplete) and either a handler (Run) or a
// prompt template (Prompt).
type Spec struct {
	// Name is lower-case letters, digits, "_" and "-", at most 32, starting with a letter
	// or digit; without "/".
	Name string
	// Aliases are other names that run it (/new and /reset for /clear). Same rules as Name.
	Aliases []string
	// Description is one line, for /help and the "/" menu.
	Description string
	// Arguments are checked before the command runs. Nil = any text is accepted as it is
	// (RunContext.Args, a prompt command's $ARGUMENTS); an empty non-nil slice = the
	// command takes none.
	Arguments []Argument
	// Safety is required: a command that changes anything must say so.
	Safety Safety
	// WhileBusy is what to do when a turn is running in the conversation ("" = run).
	WhileBusy WhileBusy
	// Requires are the client capabilities it needs; it is unavailable in a client
	// without them.
	Requires []ClientCapability
	// Available is a further availability check: it returns why the command is
	// unavailable, or "" when it is. Nil = always available.
	Available func(ac AvailabilityContext) string
	// Run runs the command. Exactly one of Run and Prompt. A returned error becomes a
	// "failed" result with the error's message.
	Run func(ctx context.Context, rc RunContext) (Result, error)
	// Prompt makes a prompt command: running it sends this text as the user's next
	// message (an ActionSendPrompt action). "$ARGUMENTS" is replaced with the text after
	// the command.
	Prompt string
	// ReadOnly runs a prompt command's turn read-only (plan mode).
	ReadOnly bool
}

// AvailabilityContext is the client a command is listed or run in.
type AvailabilityContext struct {
	// Client is the client running it ("web", "channel", "api"...; "" = "api").
	Client string `json:"client"`
	// Capabilities is what the client can do itself (default none).
	Capabilities []ClientCapability `json:"capabilities,omitempty"`
}

// Invocation is one command run: what the user typed it in, and where.
type Invocation struct {
	// SessionID is the conversation the command was typed in.
	SessionID string `json:"sessionId"`
	// Client is the client running it, e.g. "web" or "channel" (reported in
	// CommandExecuted, checked by Spec.Available).
	Client string `json:"client"`
	// Capabilities is what the client can do itself (default none).
	Capabilities []ClientCapability `json:"capabilities,omitempty"`
	// Confirmed: the user confirmed a privileged action (a dialog in the web app).
	// Without it a privileged action answers with an ActionConfirm result instead of
	// running.
	Confirmed bool `json:"confirmed,omitempty"`
	// SessionScope is the metadata the sessions of this client share (web: {agentId}, a
	// chat: its channel and chat key) - /resume lists only those. Empty = every session
	// of the agent.
	SessionScope ordered.Object `json:"sessionScope,omitzero"`
}

// RunContext is what a command's handler gets. The run's ctx (cancellation) is the
// handler's ctx argument.
type RunContext struct {
	AvailabilityContext
	AgentID   string
	SessionID string
	// Name is the canonical name (an alias resolved).
	Name string
	// Args is the text after the command, trimmed.
	Args string
	// Argv is Args matched to the spec's arguments (validated: required ones are there,
	// choices hold).
	Argv      map[string]string
	Confirmed bool
	// SessionScope is Invocation.SessionScope (empty = every session of the agent).
	SessionScope ordered.Object
}

// ActionType is what the client should do after showing a command's text.
type ActionType string

// The action types. The string values are stable wire values.
const (
	// ActionOpenSession: switch to the conversation SessionID (/clear made it, /resume
	// picked it).
	ActionOpenSession ActionType = "open-session"
	// ActionSendPrompt: send Prompt as the user's next message in the conversation
	// (/init, /review, /plan <prompt>); ReadOnly runs that turn in plan mode.
	ActionSendPrompt ActionType = "send-prompt"
	// ActionCopy: copy Text (/copy).
	ActionCopy ActionType = "copy"
	// ActionExit: close the client (/exit).
	ActionExit ActionType = "exit"
	// ActionConfirm: ask the user (the result's text is the question), then run Command
	// again with Invocation.Confirmed (a privileged action).
	ActionConfirm ActionType = "confirm"
)

// Action is what the client should do after showing a command's text: the fields of
// its Type are set, the others are empty.
type Action struct {
	Type ActionType `json:"type"`
	// SessionID is set for ActionOpenSession.
	SessionID string `json:"sessionId,omitempty"`
	// Prompt and ReadOnly are set for ActionSendPrompt.
	Prompt   string `json:"prompt,omitempty"`
	ReadOnly bool   `json:"readOnly,omitempty"`
	// Text is set for ActionCopy.
	Text string `json:"text,omitempty"`
	// Command is set for ActionConfirm.
	Command string `json:"command,omitempty"`
}

// ResultCode is why a command did not do what was asked (Result.Error).
type ResultCode string

// The result codes. The string values are stable wire values (the CommandErrorCode of
// core/ts).
const (
	// ResultNotACommand: not a command at all (no leading "/").
	ResultNotACommand      ResultCode = "not_a_command"
	ResultUnknownCommand   ResultCode = "unknown_command"
	ResultInvalidArguments ResultCode = "invalid_arguments"
	// ResultUnavailable: not available in this client (needs a capability it lacks) or
	// for this agent.
	ResultUnavailable ResultCode = "unavailable"
	// ResultBusy: a turn is running in the conversation and the command can't run under it.
	ResultBusy       ResultCode = "busy"
	ResultNotAllowed ResultCode = "not_allowed"
	ResultNotFound   ResultCode = "not_found"
	// ResultFailed: the handler returned an error.
	ResultFailed ResultCode = "failed"
)

// Result is the outcome of a command: text for the user, and maybe something for the
// client to do.
type Result struct {
	// Text is plain text (light Markdown) to show the user.
	Text   string  `json:"text"`
	Action *Action `json:"action,omitempty"`
	// Error is set when the command did not do what was asked ("" when it did).
	Error ResultCode `json:"error,omitempty"`
}

// Source is where a command comes from.
type Source string

// The command sources. The string values are stable wire values.
const (
	SourceBuiltIn Source = "built-in"
	// SourceHost: AgentCore.UseCommand.
	SourceHost Source = "host"
	// SourceAgent: Agent.Commands().Register.
	SourceAgent Source = "agent"
)

// Info is a command as listed for help and autocomplete.
type Info struct {
	Name        string   `json:"name"`
	Aliases     []string `json:"aliases"`
	Description string   `json:"description"`
	// Usage is e.g. "/compact [focus]".
	Usage     string     `json:"usage"`
	Arguments []Argument `json:"arguments"`
	Safety    Safety     `json:"safety"`
	Source    Source     `json:"source"`
	// Available: can it run in the client that asked (AvailabilityContext)?
	Available bool `json:"available"`
	// UnavailableReason is why not, when unavailable.
	UnavailableReason string `json:"unavailableReason,omitempty"`
}

// RegisterOptions are the options of registering a command.
type RegisterOptions struct {
	// Replace replaces a built-in command of the same name (only those marked
	// replaceable, e.g. /usage, so a host with token accounting can report it). Default
	// false: a clash is a CodeConflict Error.
	Replace bool `json:"replace,omitempty"`
}

// HostCommand is one of the host's commands (AgentCore.UseCommand), offered by every
// agent of the host.
type HostCommand struct {
	Spec    Spec
	Options RegisterOptions
}

// Entry is a registered command and where it came from.
type Entry struct {
	Spec   Spec
	Source Source
	// Replaceable marks a built-in a host or agent command may replace (/usage).
	Replaceable bool
}

// Commands is an agent's slash commands (Agent.Commands).
type Commands interface {
	// List returns the commands, with their availability in the given client.
	List(ac AvailabilityContext) []Info
	// Get returns one command by name or alias ("/" optional); false when there is none.
	Get(name string, ac AvailabilityContext) (Info, bool)
	// Register adds a command to this agent. An Error with CodeConflict when its name or
	// an alias is taken (by a built-in, a host command or another of the agent's),
	// CodeInvalid for a bad name or a spec without exactly one of Run and Prompt.
	Register(spec Spec, opts RegisterOptions) error
	// Unregister removes one of the agent's own commands; false when there is none by
	// that name.
	Unregister(name string) bool
	// IsCommand reports whether text is a command this agent has ("/name ...", a name or
	// an alias).
	IsCommand(text string) bool
	// Run runs "/name args". User errors (unknown command, bad arguments, unavailable,
	// busy) come back as Result.Error; it never returns an error for them.
	Run(ctx context.Context, text string, inv Invocation) Result
}
