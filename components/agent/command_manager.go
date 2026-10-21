package agent

import (
	"context"
	"regexp"
	"slices"
	"strings"
	"sync"

	"github.com/Phong95/aec-agent-go/components/agent/command"
	"github.com/Phong95/aec-agent-go/components/events"
	"github.com/Phong95/aec-agent-go/internal/redact"
)

// commandManagerDeps is what a commandManager needs.
type commandManagerDeps struct {
	agentID string
	bus     events.Bus
	// builtIns are the built-in commands (command_builtins.go), made with this agent's
	// managers; Replaceable marks the ones a host or agent command may replace (/usage).
	builtIns []command.Entry
	// hostCommands returns the host's commands (AgentCore.UseCommand), read on every call
	// so a later UseCommand applies. Nil means none.
	hostCommands func() []command.HostCommand
	// isBusy reports whether a turn is running in the session (the conversation manager's).
	isBusy func(sessionID string) bool
}

// ownCommand is one of the agent's own commands (Register) and its options.
type ownCommand struct {
	entry command.Entry
	opts  command.RegisterOptions
}

// commandManager is an agent's slash commands (A-136), the CommandManager of core/ts:
// the registry, the parser and the dispatcher, shared by every client (the web app
// through the server, chat apps through the channel manager).
//
//   - Registry: the built-ins (command_builtins.go), the host's (AgentCore.UseCommand) and
//     the agent's own (Agent.Commands().Register). Names and aliases are unique across all
//     three; a clash is a command.Error with CodeConflict (a host or agent command may
//     replace only a built-in marked replaceable, with RegisterOptions.Replace).
//   - Parsing: a command only at the start of the text ("/name args"); arguments are
//     matched to the spec (required ones, fixed choices, a free-text rest).
//   - Dispatch: availability in the client (capabilities), the busy rule (WhileBusy), then
//     the handler - or, for a prompt command, an ActionSendPrompt action. User errors
//     (unknown command, bad arguments, unavailable, busy) come back as Result.Error;
//     nothing returns an error. Every run emits CommandExecuted.
//
// It is safe for concurrent use.
type commandManager struct {
	d commandManagerDeps

	// mu guards own.
	mu  sync.RWMutex
	own []ownCommand
}

var _ command.Commands = (*commandManager)(nil)

func newCommandManager(d commandManagerDeps) *commandManager {
	return &commandManager{d: d}
}

// List returns every command with its availability in ac.
func (m *commandManager) List(ac command.AvailabilityContext) []command.Info {
	ac = availabilityContext(ac)
	entries := m.entries()
	out := make([]command.Info, 0, len(entries))
	for _, e := range entries {
		out = append(out, commandInfo(e, ac))
	}
	return out
}

// Get returns one command by name or alias ("/" optional).
func (m *commandManager) Get(name string, ac command.AvailabilityContext) (command.Info, bool) {
	e, ok := m.find(strings.ToLower(strings.TrimPrefix(name, "/")))
	if !ok {
		return command.Info{}, false
	}
	return commandInfo(e, availabilityContext(ac)), true
}

// Register adds a command of the agent's own.
func (m *commandManager) Register(spec command.Spec, opts command.RegisterOptions) error {
	if err := command.Validate(spec); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, o := range m.own {
		if o.entry.Spec.Name == spec.Name {
			return command.NewError(`The agent already has a command "/`+spec.Name+`"`, command.CodeConflict)
		}
	}
	// Checked against everything registered so far, the agent's own commands included.
	if err := command.CheckClash(spec, opts, m.entriesLocked(), command.SourceAgent); err != nil {
		return err
	}
	m.own = append(m.own, ownCommand{entry: command.Entry{Spec: spec, Source: command.SourceAgent}, opts: opts})
	return nil
}

// Unregister removes one of the agent's own commands.
func (m *commandManager) Unregister(name string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := len(m.own)
	m.own = slices.DeleteFunc(m.own, func(o ownCommand) bool { return o.entry.Spec.Name == name })
	return len(m.own) != n
}

// IsCommand reports whether text is a command this agent has.
func (m *commandManager) IsCommand(text string) bool {
	parsed, ok := command.Parse(text)
	if !ok {
		return false
	}
	_, found := m.find(parsed.Name)
	return found
}

// Run runs "/name args" and emits CommandExecuted (not for text that is no command).
func (m *commandManager) Run(ctx context.Context, text string, inv command.Invocation) command.Result {
	parsed, ok := command.Parse(text)
	if !ok {
		return command.Result{Text: `Commands start with "/", e.g. /help.`, Error: command.ResultNotACommand}
	}
	result := m.dispatch(ctx, parsed.Name, parsed.Args, inv)
	name := parsed.Name
	if e, found := m.find(parsed.Name); found {
		name = e.Spec.Name
	}
	if m.d.bus != nil {
		events.Emit(m.d.bus, events.CommandExecuted, inv.SessionID, events.CommandExecutedPayload{
			AgentID: m.d.agentID, SessionID: inv.SessionID, Name: name, Client: inv.Client,
			OK: result.Error == "", Error: string(result.Error),
		})
	}
	return result
}

func (m *commandManager) dispatch(ctx context.Context, name, args string, inv command.Invocation) command.Result {
	entry, ok := m.find(name)
	if !ok {
		near := ""
		if c := m.closest(name); c != "" {
			near = " Did you mean /" + c + "?"
		}
		return command.Result{
			Text:  "There is no command /" + name + "." + near + " Send /help to see the commands.",
			Error: command.ResultUnknownCommand,
		}
	}
	spec := entry.Spec
	ac := availabilityContext(command.AvailabilityContext{Client: inv.Client, Capabilities: inv.Capabilities})
	if reason := unavailableReason(spec, ac); reason != "" {
		return command.Result{Text: "/" + spec.Name + " isn't available here: " + reason + ".", Error: command.ResultUnavailable}
	}

	argv, problem := matchArguments(spec, args)
	if problem != "" {
		return command.Result{Text: problem + " Usage: " + command.Usage(spec), Error: command.ResultInvalidArguments}
	}

	if spec.WhileBusy == command.WhileBusyRefuse && m.d.isBusy != nil && m.d.isBusy(inv.SessionID) {
		return command.Result{
			Text:  "A reply is running in this conversation. Send /" + spec.Name + " again when it has finished, or stop it first.",
			Error: command.ResultBusy,
		}
	}

	if spec.Run == nil {
		return command.Result{
			Text: "Running /" + spec.Name + symEllipsis,
			Action: &command.Action{
				Type:     command.ActionSendPrompt,
				Prompt:   strings.ReplaceAll(spec.Prompt, "$ARGUMENTS", args),
				ReadOnly: spec.ReadOnly,
			},
		}
	}

	rc := command.RunContext{
		AvailabilityContext: ac,
		AgentID:             m.d.agentID,
		SessionID:           inv.SessionID,
		Name:                spec.Name,
		Args:                args,
		Argv:                argv,
		Confirmed:           inv.Confirmed,
		SessionScope:        inv.SessionScope.Clone(),
	}
	return runCommand(ctx, spec, rc)
}

// runCommand runs a handler: an error - or a panic - is a "failed" result with its text.
func runCommand(ctx context.Context, spec command.Spec, rc command.RunContext) (result command.Result) {
	defer func() {
		if r := recover(); r != nil {
			result = command.Result{Text: "/" + spec.Name + " failed: panic: " + redact.Panic(r), Error: command.ResultFailed}
		}
	}()
	res, err := spec.Run(ctx, rc)
	if err != nil {
		return command.Result{Text: "/" + spec.Name + " failed: " + err.Error(), Error: command.ResultFailed}
	}
	return res
}

// entries is every command, in order: built-ins (or what replaced them), then the
// host's, then the agent's. A host or agent command registered with Replace takes the
// place of the replaceable built-in of the same name.
func (m *commandManager) entries() []command.Entry {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.entriesLocked()
}

// entriesLocked is entries; the caller holds mu.
func (m *commandManager) entriesLocked() []command.Entry {
	var host []command.HostCommand
	if m.d.hostCommands != nil {
		host = m.d.hostCommands()
	}
	replacing := map[string]command.Entry{}
	for _, h := range host {
		if h.Options.Replace {
			replacing[h.Spec.Name] = command.Entry{Spec: h.Spec, Source: command.SourceHost}
		}
	}
	for _, o := range m.own {
		if o.opts.Replace {
			replacing[o.entry.Spec.Name] = o.entry
		}
	}
	out := make([]command.Entry, 0, len(m.d.builtIns)+len(host)+len(m.own))
	builtInNames := map[string]bool{}
	for _, b := range m.d.builtIns {
		builtInNames[b.Spec.Name] = true
		if r, ok := replacing[b.Spec.Name]; ok && b.Replaceable {
			out = append(out, r)
			continue
		}
		out = append(out, b)
	}
	hostNames := map[string]bool{}
	for _, h := range host {
		hostNames[h.Spec.Name] = true
		if !builtInNames[h.Spec.Name] {
			out = append(out, command.Entry{Spec: h.Spec, Source: command.SourceHost})
		}
	}
	for _, o := range m.own {
		if !builtInNames[o.entry.Spec.Name] && !hostNames[o.entry.Spec.Name] {
			out = append(out, o.entry)
		}
	}
	return out
}

// find returns the command named name, else the one with that alias.
func (m *commandManager) find(name string) (command.Entry, bool) {
	entries := m.entries()
	for _, e := range entries {
		if e.Spec.Name == name {
			return e, true
		}
	}
	for _, e := range entries {
		if slices.Contains(e.Spec.Aliases, name) {
			return e, true
		}
	}
	return command.Entry{}, false
}

// closest is the nearest command name (edit distance at most 2, or a prefix match), for
// "did you mean"; "" when none is near.
func (m *commandManager) closest(name string) string {
	best, bestD := "", 0
	for _, e := range m.entries() {
		for _, n := range append([]string{e.Spec.Name}, e.Spec.Aliases...) {
			d := 1
			if !strings.HasPrefix(n, name) {
				d = editDistance(name, n)
			}
			if d <= 2 && (best == "" || d < bestD) {
				best, bestD = e.Spec.Name, d
			}
		}
	}
	return best
}

// availabilityContext fills the defaults: the client "api".
func availabilityContext(ac command.AvailabilityContext) command.AvailabilityContext {
	if ac.Client == "" {
		ac.Client = "api"
	}
	ac.Capabilities = slices.Clone(ac.Capabilities)
	return ac
}

// unavailableReason is why spec can't run in ac ("" when it can).
func unavailableReason(spec command.Spec, ac command.AvailabilityContext) string {
	var missing []command.ClientCapability
	for _, c := range spec.Requires {
		if !slices.Contains(ac.Capabilities, c) {
			missing = append(missing, c)
		}
	}
	if len(missing) > 0 {
		switch {
		case slices.Contains(missing, command.CapabilityClipboard):
			return "this app can't copy to your clipboard"
		case slices.Contains(missing, command.CapabilityExit):
			return "there is nothing to exit in this app " + emDash + " close it, or start a new conversation with /clear"
		default:
			names := make([]string, len(missing))
			for i, c := range missing {
				names[i] = string(c)
			}
			return "it needs " + strings.Join(names, ", ")
		}
	}
	if spec.Available != nil {
		return spec.Available(ac)
	}
	return ""
}

// commandInfo is an entry as listed in ac.
func commandInfo(e command.Entry, ac command.AvailabilityContext) command.Info {
	spec := e.Spec
	reason := unavailableReason(spec, ac)
	aliases := slices.Clone(spec.Aliases)
	if aliases == nil {
		aliases = []string{}
	}
	args := slices.Clone(spec.Arguments)
	if args == nil {
		args = []command.Argument{}
	}
	return command.Info{
		Name:              spec.Name,
		Aliases:           aliases,
		Description:       spec.Description,
		Usage:             command.Usage(spec),
		Arguments:         args,
		Safety:            spec.Safety,
		Source:            e.Source,
		Available:         reason == "",
		UnavailableReason: reason,
	}
}

// firstWord splits the first word off the arguments: the word and the rest, trimmed at
// its start.
var firstWord = regexp.MustCompile(`^(\S+)\s*([\s\S]*)$`)

// matchArguments matches the text after the command to the spec's arguments. It returns
// the values by argument name, or why the text does not fit (problem not empty).
func matchArguments(spec command.Spec, args string) (argv map[string]string, problem string) {
	argv = map[string]string{}
	// No declared arguments: any text goes to the command as it is.
	if spec.Arguments == nil {
		return argv, ""
	}
	rest := strings.TrimSpace(args)
	for _, def := range spec.Arguments {
		if rest == "" {
			if def.Required {
				return nil, "/" + spec.Name + " needs " + article(def.Name) + " " + def.Name + "."
			}
			continue
		}
		var value string
		if def.Rest {
			value, rest = rest, ""
		} else {
			mm := firstWord.FindStringSubmatch(rest)
			value, rest = mm[1], mm[2]
		}
		if len(def.Choices) > 0 {
			i := slices.IndexFunc(def.Choices, func(c string) bool { return strings.EqualFold(c, value) })
			if i < 0 {
				return nil, `"` + value + `" isn't one of ` + strings.Join(def.Choices, ", ") + "."
			}
			value = def.Choices[i]
		}
		argv[def.Name] = value
	}
	if rest != "" {
		if len(spec.Arguments) == 0 {
			return nil, "/" + spec.Name + " takes no arguments."
		}
		return nil, `Unexpected "` + rest + `".`
	}
	return argv, ""
}

// article is "an" before a vowel, else "a".
func article(word string) string {
	if word != "" && strings.ContainsRune("aeiouAEIOU", rune(word[0])) {
		return "an"
	}
	return "a"
}

// editDistance is the Levenshtein distance of two short names.
func editDistance(a, b string) int {
	ar, br := []rune(a), []rune(b)
	row := make([]int, len(br)+1)
	for j := range row {
		row[j] = j
	}
	for i := 1; i <= len(ar); i++ {
		prev := row[0]
		row[0] = i
		for j := 1; j <= len(br); j++ {
			cur := row[j]
			cost := 1
			if ar[i-1] == br[j-1] {
				cost = 0
			}
			row[j] = min(row[j]+1, row[j-1]+1, prev+cost)
			prev = cur
		}
	}
	return row[len(br)]
}
