package agent

// A-136: the built-in slash commands. Each one works through the agent's managers (passed
// in as builtInCommandDeps) - the commands hold no state of their own.

import (
	"cmp"
	"context"
	"errors"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/Phong95/aec-agent-go/components/agent/agentloop"
	"github.com/Phong95/aec-agent-go/components/agent/command"
	"github.com/Phong95/aec-agent-go/components/agent/subagent"
	"github.com/Phong95/aec-agent-go/components/sandboxhub"
	"github.com/Phong95/aec-agent-go/internal/jsstr"
	"github.com/Phong95/aec-agent-go/kernel"
	"github.com/Phong95/aec-agent-go/kernel/ordered"
)

// The non-ASCII characters of the commands' texts (the source stays ASCII).
var (
	symArrow  = string(rune(0x2192))
	symLQuote = string(rune(0x201c))
	symRQuote = string(rune(0x201d))
)

// commandWorkspace is what /init, /diff and /review use of the agent's sandbox
// (sandboxhub.View has it).
type commandWorkspace interface {
	WorkingDir() string
	Capabilities() []sandboxhub.Capability
	Exec(ctx context.Context, req sandboxhub.ExecRequest, opts sandboxhub.ExecOptions) (sandboxhub.ExecResult, error)
}

// builtInCommandDeps is what the built-in commands use - the agent's managers, as
// functions. A "found" bool false (with a nil error) means there is no such session.
type builtInCommandDeps struct {
	agentName func() string
	// defaultModel is the agent's own model, used by sessions that set none.
	defaultModel func() string
	// listModels returns the models /model offers; ok false when the provider has no
	// catalog (any name is accepted then).
	listModels   func(ctx context.Context) (models []string, ok bool)
	getSession   func(ctx context.Context, sessionID string) (kernel.Session, bool, error)
	listSessions func(ctx context.Context, f kernel.SessionFilter) ([]kernel.Session, error)
	messages     func(ctx context.Context, sessionID string) ([]kernel.Message, error)
	// updateSession is the conversation manager's: it changes the running turn's copy too.
	updateSession    func(ctx context.Context, sessionID string, fields []string, change func(*kernel.Session)) (kernel.Session, bool, error)
	startSessionFrom func(ctx context.Context, sessionID string) (kernel.Session, bool, error)
	isBusy           func(sessionID string) bool
	approvalMode     func(ctx context.Context, sessionID string) (kernel.ApprovalMode, error)
	setApprovalMode  func(ctx context.Context, sessionID string, mode kernel.ApprovalMode) (kernel.Session, bool, error)
	contextUsage     func(ctx context.Context, sessionID string) (agentloop.ContextUsage, bool, error)
	// compact compacts the session's working set now, holding its turn slot.
	compact func(ctx context.Context, sessionID, focus string) (agentloop.CompactNowResult, bool, error)
	// describeWorkspace is "the shared sandbox", "its own sandbox" or "none" - without
	// starting a sandbox.
	describeWorkspace func() string
	sandbox           func(ctx context.Context) (commandWorkspace, error)
	// listCommands is every command, for /help (the command manager's List, in the asking
	// client).
	listCommands func(ac command.AvailabilityContext) []command.Info
}

// The limits of the built-ins.
const (
	// maxListedSessions is the most sessions /resume lists.
	maxListedSessions = 10
	// maxDiffChars and maxReviewDiffChars are the most characters (UTF-16 units) of `git
	// diff` /diff shows and /review sends to the model.
	maxDiffChars       = 20_000
	maxReviewDiffChars = 60_000
	maxTitleLength     = 120
	gitTimeoutMs       = 30_000
	gitMaxOutputBytes  = 2_000_000
)

// BuiltInCommandSpecs returns the built-in commands' metadata (A-136): names, aliases,
// arguments, safety, and which one a host may replace (Entry.Replaceable: /usage). The
// specs carry no handler; a host command can't take their names (AgentCore.UseCommand
// checks them). The slice is fresh on every call.
func BuiltInCommandSpecs() []command.Entry {
	none := []command.Argument{}
	builtIn := func(spec command.Spec) command.Entry { return command.Entry{Spec: spec, Source: command.SourceBuiltIn} }
	usage := builtIn(command.Spec{Name: "usage", Description: "Show token usage", Arguments: none, Safety: command.SafetyReadOnly})
	usage.Replaceable = true
	return []command.Entry{
		builtIn(command.Spec{Name: "help", Description: "List the commands, or explain one", Arguments: []command.Argument{{Name: "command"}}, Safety: command.SafetyReadOnly}),
		builtIn(command.Spec{
			Name: "clear", Aliases: []string{"new", "reset"},
			Description: "Start a new conversation (this one is kept: /resume brings it back)",
			Arguments:   none, Safety: command.SafetySession,
		}),
		builtIn(command.Spec{
			Name: "compact", Description: "Summarize the conversation so far to free context",
			Arguments: []command.Argument{{Name: "focus", Description: "What the summary should keep in detail", Rest: true}},
			Safety:    command.SafetySession, WhileBusy: command.WhileBusyRefuse,
		}),
		builtIn(command.Spec{
			Name: "resume", Description: "List recent conversations, or go back to one",
			Arguments: []command.Argument{{Name: "conversation", Description: "Its number in the list, its id or its title", Rest: true}},
			Safety:    command.SafetySession,
		}),
		builtIn(command.Spec{
			Name: "rename", Description: "Rename this conversation",
			Arguments: []command.Argument{{Name: "name", Required: true, Rest: true}},
			Safety:    command.SafetySession,
		}),
		builtIn(command.Spec{Name: "status", Description: "Show the model, context, permissions and workspace", Arguments: none, Safety: command.SafetyReadOnly}),
		builtIn(command.Spec{Name: "context", Description: "Show what fills the context window", Arguments: none, Safety: command.SafetyReadOnly}),
		usage,
		builtIn(command.Spec{
			Name: "model", Description: "Show the models, or switch this conversation's model",
			Arguments: []command.Argument{{Name: "model", Description: `A model name, or "default" for the agent's`}},
			Safety:    command.SafetySession,
		}),
		builtIn(command.Spec{
			Name: "permissions", Description: "Show or change how tool calls are approved (ask, yolo, plan)",
			Arguments: []command.Argument{{Name: "mode", Choices: []string{"ask", "yolo", "plan"}}},
			Safety:    command.SafetyPrivileged,
		}),
		builtIn(command.Spec{
			Name: "yolo", Description: "Run tools without asking (on/off)",
			Arguments: []command.Argument{{Name: "switch", Choices: []string{"on", "off"}}},
			Safety:    command.SafetyPrivileged,
		}),
		builtIn(command.Spec{
			Name: "plan", Description: "Plan read-only: tools that change things don't run (off to stop)",
			Arguments: []command.Argument{{Name: "prompt", Description: `What to plan (sent right away), or "off"`, Rest: true}},
			Safety:    command.SafetySession,
		}),
		builtIn(command.Spec{Name: "init", Description: "Create or improve the project's AGENTS.md instructions", Arguments: none, Safety: command.SafetySession}),
		builtIn(command.Spec{Name: "diff", Description: "Show the workspace's uncommitted changes (git)", Arguments: none, Safety: command.SafetyReadOnly}),
		builtIn(command.Spec{
			Name: "review", Description: "Review the uncommitted changes (read-only)",
			Arguments: []command.Argument{{Name: "focus", Rest: true}},
			Safety:    command.SafetyReadOnly,
		}),
		builtIn(command.Spec{
			Name: "copy", Description: "Copy the last reply", Arguments: none, Safety: command.SafetyReadOnly,
			Requires: []command.ClientCapability{command.CapabilityClipboard},
		}),
		builtIn(command.Spec{
			Name: "exit", Aliases: []string{"quit"}, Description: "Close the app", Arguments: none, Safety: command.SafetyReadOnly,
			Requires: []command.ClientCapability{command.CapabilityExit},
		}),
	}
}

// builtInCommands returns the built-in commands, working through d.
func builtInCommands(d builtInCommandDeps) []command.Entry {
	type handler = func(ctx context.Context, rc command.RunContext) (command.Result, error)
	handlers := map[string]handler{
		"help":    func(_ context.Context, rc command.RunContext) (command.Result, error) { return d.help(rc), nil },
		"clear":   d.clear,
		"compact": d.compactCmd,
		"resume":  d.resume,
		"rename":  d.rename,
		"status":  d.status,
		"context": d.contextCmd,
		"usage":   d.usage,
		"model":   d.model,
		"permissions": func(ctx context.Context, rc command.RunContext) (command.Result, error) {
			return d.permissions(ctx, rc, kernel.ApprovalMode(rc.Argv["mode"]))
		},
		"yolo": func(ctx context.Context, rc command.RunContext) (command.Result, error) {
			var wanted kernel.ApprovalMode
			switch rc.Argv["switch"] {
			case "on":
				wanted = kernel.ApprovalModeYolo
			case "off":
				wanted = kernel.ApprovalModeAsk
			}
			return d.permissions(ctx, rc, wanted)
		},
		"plan":   d.plan,
		"init":   d.initCmd,
		"diff":   d.diff,
		"review": d.review,
		"copy":   d.copyCmd,
		"exit": func(context.Context, command.RunContext) (command.Result, error) {
			return command.Result{Text: "Bye.", Action: &command.Action{Type: command.ActionExit}}, nil
		},
	}
	specs := BuiltInCommandSpecs()
	for i := range specs {
		specs[i].Spec.Run = handlers[specs[i].Spec.Name]
	}
	return specs
}

// --- Discovery -----------------------------------------------------------------------

func (d builtInCommandDeps) help(rc command.RunContext) command.Result {
	commands := d.listCommands(rc.AvailabilityContext)
	wanted := strings.ToLower(strings.TrimPrefix(rc.Argv["command"], "/"))
	if wanted != "" {
		i := slices.IndexFunc(commands, func(c command.Info) bool { return c.Name == wanted || slices.Contains(c.Aliases, wanted) })
		if i < 0 {
			return command.Result{Text: "There is no command /" + wanted + ". Send /help to see the commands.", Error: command.ResultNotFound}
		}
		c := commands[i]
		lines := []string{c.Usage + " " + emDash + " " + c.Description}
		if len(c.Aliases) > 0 {
			lines = append(lines, "Also: "+slashed(c.Aliases))
		}
		for _, a := range c.Arguments {
			var detail []string
			if a.Description != "" {
				detail = append(detail, a.Description)
			}
			if len(a.Choices) > 0 {
				detail = append(detail, "one of "+strings.Join(a.Choices, ", "))
			}
			line := bullet + " " + a.Name
			if !a.Required {
				line += " (optional)"
			}
			if len(detail) > 0 {
				line += ": " + strings.Join(detail, "; ")
			}
			lines = append(lines, line)
		}
		lines = append(lines, safetyText(c.Safety))
		if !c.Available {
			lines = append(lines, "Not available here: "+c.UnavailableReason+".")
		}
		return command.Result{Text: strings.Join(lines, "\n")}
	}
	lines := []string{"Commands:"}
	var unavailable []command.Info
	for _, c := range commands {
		if c.Available {
			lines = append(lines, commandHelpLine(c))
		} else {
			unavailable = append(unavailable, c)
		}
	}
	if len(unavailable) > 0 {
		lines = append(lines, "", "Not available here:")
		for _, c := range unavailable {
			lines = append(lines, c.Usage+" "+emDash+" "+c.UnavailableReason)
		}
	}
	lines = append(lines, "", "Send /help <command> for details.")
	return command.Result{Text: strings.Join(lines, "\n")}
}

// safetyText explains a command's safety in /help <command>.
func safetyText(s command.Safety) string {
	switch s {
	case command.SafetyReadOnly:
		return "Read-only: it changes nothing."
	case command.SafetySession:
		return "Changes this conversation."
	default:
		return "Can let the agent do more without asking, so it asks you to confirm first."
	}
}

// commandHelpLine is "/clear (/new, /reset) - Start a new conversation" (an em dash).
func commandHelpLine(c command.Info) string {
	aliases := ""
	if len(c.Aliases) > 0 {
		aliases = " (" + slashed(c.Aliases) + ")"
	}
	return c.Usage + aliases + " " + emDash + " " + c.Description
}

// slashed is "/a, /b".
func slashed(names []string) string {
	out := make([]string, len(names))
	for i, n := range names {
		out[i] = "/" + n
	}
	return strings.Join(out, ", ")
}

// --- Session and context -----------------------------------------------------------

func (d builtInCommandDeps) clear(ctx context.Context, rc command.RunContext) (command.Result, error) {
	session, ok, err := d.startSessionFrom(ctx, rc.SessionID)
	if err != nil {
		return command.Result{}, err
	}
	if !ok {
		return notFoundResult(), nil
	}
	return command.Result{
		Text:   "Started a new conversation. The previous one is kept: /resume lists it.",
		Action: &command.Action{Type: command.ActionOpenSession, SessionID: session.ID},
	}, nil
}

func (d builtInCommandDeps) compactCmd(ctx context.Context, rc command.RunContext) (command.Result, error) {
	res, ok, err := d.compact(ctx, rc.SessionID, rc.Argv["focus"])
	if err != nil {
		return command.Result{}, err
	}
	if !ok {
		return notFoundResult(), nil
	}
	if !res.Compacted {
		return command.Result{Text: "Nothing to compact yet: the conversation is short enough to keep as it is."}, nil
	}
	return command.Result{
		Text: "Compacted the conversation: " + strconv.Itoa(res.MessagesBefore) + " messages " + symArrow + " " + strconv.Itoa(res.MessagesAfter) +
			" (context about " + tokensText(res.BeforeTokens) + " " + symArrow + " " + tokensText(res.AfterTokens) + " tokens). " +
			"The full history is kept; the agent now works from the summary and the latest messages.",
	}, nil
}

// resumable is the sessions /resume offers: the client's scope, newest first, no job or
// subagent runs, not this one.
func (d builtInCommandDeps) resumable(ctx context.Context, rc command.RunContext) ([]kernel.Session, error) {
	sessions, err := d.listSessions(ctx, kernel.SessionFilter{Metadata: rc.SessionScope})
	if err != nil {
		return nil, err
	}
	sessions = slices.DeleteFunc(sessions, func(s kernel.Session) bool {
		return s.ID == rc.SessionID || s.JobID != "" || subagent.IsSession(&s)
	})
	slices.SortStableFunc(sessions, func(a, b kernel.Session) int { return cmp.Compare(b.UpdatedAt, a.UpdatedAt) })
	return sessions, nil
}

func (d builtInCommandDeps) resume(ctx context.Context, rc command.RunContext) (command.Result, error) {
	sessions, err := d.resumable(ctx, rc)
	if err != nil {
		return command.Result{}, err
	}
	wanted := jsstr.Trim(rc.Argv["conversation"])
	listed := sessions[:min(len(sessions), maxListedSessions)]
	if wanted == "" {
		if len(listed) == 0 {
			return command.Result{Text: "There are no other conversations to resume."}, nil
		}
		lines := []string{"Recent conversations:"}
		for i, s := range listed {
			id := s.ID
			if len(id) > 8 {
				id = id[:8]
			}
			lines = append(lines, strconv.Itoa(i+1)+". "+titleOf(s)+" "+midDot+" "+utcMinute(s.UpdatedAt)+" "+midDot+" "+id)
		}
		lines = append(lines, "", "Resume one with /resume <number>, /resume <id> or /resume <title>.")
		return command.Result{Text: strings.Join(lines, "\n")}, nil
	}
	pick, problem := pickSession(listed, sessions, wanted)
	if problem != "" {
		return command.Result{Text: problem, Error: command.ResultNotFound}, nil
	}
	return command.Result{
		Text:   "Resumed " + symLQuote + titleOf(pick) + symRQuote + ".",
		Action: &command.Action{Type: command.ActionOpenSession, SessionID: pick.ID},
	}, nil
}

var digitsOnly = regexp.MustCompile(`^\d+$`)

// pickSession finds the session /resume <wanted> means: a number in the list, an id (or
// an id prefix of 4 or more), an exact title, or one title that contains it. problem is
// why none was picked.
func pickSession(listed, all []kernel.Session, wanted string) (kernel.Session, string) {
	if digitsOnly.MatchString(wanted) {
		n, err := strconv.Atoi(wanted)
		if err == nil && n >= 1 && n <= len(listed) {
			return listed[n-1], ""
		}
		return kernel.Session{}, "There is no conversation " + wanted + " in the list. Send /resume to see it."
	}
	lower := jsstr.ToLower(wanted)
	var byID, exact, partial []kernel.Session
	for _, s := range all {
		if s.ID == wanted || (kernel.UTF16Len(wanted) >= 4 && strings.HasPrefix(s.ID, wanted)) {
			byID = append(byID, s)
		}
		title := jsstr.ToLower(titleOf(s))
		if title == lower {
			exact = append(exact, s)
		}
		if strings.Contains(title, lower) {
			partial = append(partial, s)
		}
	}
	switch {
	case len(byID) == 1:
		return byID[0], ""
	case len(exact) >= 1:
		return exact[0], ""
	case len(partial) == 1:
		return partial[0], ""
	case len(partial) > 1 || len(byID) > 1:
		return kernel.Session{}, `More than one conversation matches "` + wanted + `". Send /resume and pick one by its number.`
	}
	return kernel.Session{}, `No conversation matches "` + wanted + `". Send /resume to see the list.`
}

func (d builtInCommandDeps) rename(ctx context.Context, rc command.RunContext) (command.Result, error) {
	title := jsstr.TruncateUTF16(strings.Join(jsstr.Fields(rc.Argv["name"]), " "), maxTitleLength)
	_, ok, err := d.updateSession(ctx, rc.SessionID, []string{"title"}, func(s *kernel.Session) {
		s.Metadata = s.Metadata.Clone()
		s.Metadata.Set("title", title)
	})
	if err != nil {
		return command.Result{}, err
	}
	if !ok {
		return notFoundResult(), nil
	}
	return command.Result{Text: "Renamed this conversation to " + symLQuote + title + symRQuote + "."}, nil
}

// --- Diagnostics ---------------------------------------------------------------------

func (d builtInCommandDeps) status(ctx context.Context, rc command.RunContext) (command.Result, error) {
	session, ok, err := d.getSession(ctx, rc.SessionID)
	if err != nil {
		return command.Result{}, err
	}
	if !ok {
		return notFoundResult(), nil
	}
	mode, err := d.approvalMode(ctx, rc.SessionID)
	if err != nil {
		return command.Result{}, err
	}
	usage, hasUsage, err := d.contextUsage(ctx, rc.SessionID)
	if err != nil {
		return command.Result{}, err
	}
	lines := []string{
		"Agent: " + d.agentName(),
		"Conversation: " + titleOf(session) + " (" + session.ID + ")",
		"Started: " + utcMinute(session.CreatedAt) + " UTC",
		d.modelLine(session),
	}
	if hasUsage {
		lines = append(lines, "Context: "+contextLine(usage))
	}
	lines = append(lines, "Permissions: "+modeText(mode), "Workspace: "+d.describeWorkspace())
	if d.isBusy(rc.SessionID) {
		lines = append(lines, "A reply is running in this conversation.")
	}
	return command.Result{Text: strings.Join(lines, "\n")}, nil
}

// modelLine is the "Model: ..." line of /status and /model.
func (d builtInCommandDeps) modelLine(session kernel.Session) string {
	if session.LLMModel != "" {
		return "Model: " + session.LLMModel
	}
	name := d.defaultModel()
	if name == "" {
		name = "the provider's default"
	}
	return "Model: " + name + " (the agent's default)"
}

func (d builtInCommandDeps) contextCmd(ctx context.Context, rc command.RunContext) (command.Result, error) {
	usage, ok, err := d.contextUsage(ctx, rc.SessionID)
	if err != nil {
		return command.Result{}, err
	}
	if !ok {
		return notFoundResult(), nil
	}
	lines := []string{
		"Context: " + contextLine(usage),
		"Compaction starts at " + tokensText(usage.CompactAtTokens) + " tokens; /compact summarizes now.",
		"",
	}
	for _, c := range usage.Categories {
		if c.Tokens > 0 {
			lines = append(lines, bullet+" "+categoryLabel(c.Key)+": "+tokensText(c.Tokens))
		}
	}
	return command.Result{Text: strings.Join(lines, "\n")}, nil
}

// categoryLabel names a context category in /context.
func categoryLabel(key agentloop.ContextCategoryKey) string {
	switch key {
	case agentloop.ContextSystemPrompt:
		return "System prompt"
	case agentloop.ContextMemoryFiles:
		return "Memory files"
	case agentloop.ContextSkills:
		return "Skills"
	case agentloop.ContextAgents:
		return "Subagents"
	case agentloop.ContextSecrets:
		return "Secrets"
	case agentloop.ContextTools:
		return "Tools"
	case agentloop.ContextMemories:
		return "Recalled memories"
	case agentloop.ContextMessages:
		return "Messages"
	case agentloop.ContextSummary:
		return "Summary"
	default:
		return "Other"
	}
}

func (d builtInCommandDeps) usage(ctx context.Context, rc command.RunContext) (command.Result, error) {
	session, ok, err := d.getSession(ctx, rc.SessionID)
	if err != nil {
		return command.Result{}, err
	}
	if !ok {
		return notFoundResult(), nil
	}
	var lines []string
	if last := session.CurrentTokenUsage; last != nil {
		var cache []string
		if last.CacheReadTokens != nil && *last.CacheReadTokens != 0 {
			cache = append(cache, tokensText(*last.CacheReadTokens)+" read from cache")
		}
		if last.CacheWriteTokens != nil && *last.CacheWriteTokens != 0 {
			cache = append(cache, tokensText(*last.CacheWriteTokens)+" written to cache")
		}
		line := "Last response: " + tokensText(last.InputTokens) + " input, " + tokensText(last.OutputTokens) + " output tokens"
		if len(cache) > 0 {
			line += " (" + strings.Join(cache, ", ") + ")"
		}
		lines = append(lines, line+".")
	} else {
		lines = append(lines, "No model response in this conversation yet.")
	}
	ctxUsage, hasUsage, err := d.contextUsage(ctx, rc.SessionID)
	if err != nil {
		return command.Result{}, err
	}
	if hasUsage {
		lines = append(lines, "Context: "+contextLine(ctxUsage))
	}
	lines = append(lines, "Totals and cost aren't tracked by this host.")
	return command.Result{Text: strings.Join(lines, "\n")}, nil
}

// --- Runtime controls ----------------------------------------------------------------

func (d builtInCommandDeps) model(ctx context.Context, rc command.RunContext) (command.Result, error) {
	session, ok, err := d.getSession(ctx, rc.SessionID)
	if err != nil {
		return command.Result{}, err
	}
	if !ok {
		return notFoundResult(), nil
	}
	agentModel := d.defaultModel()
	var models []string
	if d.listModels != nil {
		if list, listed := d.listModels(ctx); listed {
			models = list
		}
	}
	wanted := jsstr.Trim(rc.Argv["model"])
	if wanted == "" {
		lines := []string{d.modelLine(session)}
		if len(models) > 0 {
			lines = append(lines, "", "Available:")
			for _, m := range models[:min(len(models), maxListedModels)] {
				lines = append(lines, bullet+" "+m)
			}
			if len(models) > maxListedModels {
				lines = append(lines, symEllipsis+" and "+strconv.Itoa(len(models)-maxListedModels)+" more")
			}
		}
		lines = append(lines, "", "Switch with /model <name>; /model default goes back to the agent's model.")
		return command.Result{Text: strings.Join(lines, "\n")}, nil
	}
	next := ""
	if jsstr.ToLower(wanted) != "default" {
		i := slices.IndexFunc(models, func(m string) bool { return jsstr.ToLower(m) == jsstr.ToLower(wanted) })
		if len(models) > 0 && i < 0 {
			return command.Result{Text: `There is no model "` + wanted + `". Send /model to see the list.`, Error: command.ResultInvalidArguments}, nil
		}
		next = wanted
		if i >= 0 {
			next = models[i]
		}
	}
	_, ok, err = d.updateSession(ctx, rc.SessionID, []string{"llmModel"}, func(s *kernel.Session) { s.LLMModel = next })
	if err != nil {
		return command.Result{}, err
	}
	if !ok {
		return notFoundResult(), nil
	}
	busy := ""
	if d.isBusy(rc.SessionID) {
		busy = " The reply that is running keeps its model; the next one uses this."
	}
	if next != "" {
		return command.Result{Text: "This conversation now uses " + next + "." + busy}, nil
	}
	if agentModel != "" {
		return command.Result{Text: "This conversation now uses the agent's model (" + agentModel + ")." + busy}, nil
	}
	return command.Result{Text: "This conversation now uses the agent's model." + busy}, nil
}

// modeText describes an approval mode, after "Permissions: ".
func modeText(mode kernel.ApprovalMode) string {
	switch mode {
	case kernel.ApprovalModeYolo:
		return "YOLO " + emDash + " tools run without asking"
	case kernel.ApprovalModePlan:
		return "plan " + emDash + " read-only: tools that change things don't run"
	default:
		return "ask " + emDash + " tools that need approval ask first"
	}
}

// permissions is /permissions [ask|yolo|plan] and /yolo [on|off] - show or set the
// session's approval mode. wanted "" shows it.
func (d builtInCommandDeps) permissions(ctx context.Context, rc command.RunContext, wanted kernel.ApprovalMode) (command.Result, error) {
	current, err := d.approvalMode(ctx, rc.SessionID)
	if err != nil {
		return command.Result{}, err
	}
	yolo := rc.Name == "yolo"
	if wanted == "" && yolo {
		// A-135's /yolo answers, kept.
		if current == kernel.ApprovalModeYolo {
			return command.Result{Text: "YOLO mode is on: tools run without asking. Send /yolo off to ask again."}, nil
		}
		return command.Result{Text: "YOLO mode is off: tools that need approval ask here first. Send /yolo on to run them without asking."}, nil
	}
	if wanted == "" {
		return command.Result{Text: strings.Join([]string{
			"Permissions: " + modeText(current) + ".",
			"",
			bullet + " /permissions ask " + emDash + " tools that need approval ask first (the default)",
			bullet + " /permissions yolo " + emDash + " tools run without asking, until you switch back",
			bullet + " /permissions plan " + emDash + " read-only planning: tools that change things don't run",
		}, "\n")}, nil
	}
	if wanted == kernel.ApprovalModeYolo && current != kernel.ApprovalModeYolo && !rc.Confirmed {
		confirm := "/permissions yolo"
		if yolo {
			confirm = "/yolo on"
		}
		return command.Result{
			Text: "Run tools without asking in this conversation? The agent can then write files and run commands " +
				emDash + " on your computer too, when its workspace is a host folder " + emDash + " until you switch back.",
			Action: &command.Action{Type: command.ActionConfirm, Command: confirm},
		}, nil
	}
	_, ok, err := d.setApprovalMode(ctx, rc.SessionID, wanted)
	if err != nil {
		var se *kernel.SessionError
		if errors.As(err, &se) && se.Code == kernel.SessionCodeNotAllowed {
			return command.Result{Text: se.Message, Error: command.ResultNotAllowed}, nil
		}
		return command.Result{}, err
	}
	if !ok {
		return notFoundResult(), nil
	}
	if yolo {
		if wanted == kernel.ApprovalModeYolo {
			return command.Result{Text: "YOLO mode is on: tools in this conversation run without asking, until /yolo off."}, nil
		}
		return command.Result{Text: "YOLO mode is off: tools that need approval ask here first."}, nil
	}
	return command.Result{Text: "Permissions: " + modeText(wanted) + "."}, nil
}

func (d builtInCommandDeps) plan(ctx context.Context, rc command.RunContext) (command.Result, error) {
	prompt := jsstr.Trim(rc.Argv["prompt"])
	if jsstr.ToLower(prompt) == "off" {
		_, ok, err := d.setApprovalMode(ctx, rc.SessionID, kernel.ApprovalModeAsk)
		if err != nil {
			return command.Result{}, err
		}
		if !ok {
			return notFoundResult(), nil
		}
		return command.Result{Text: "Plan mode is off. Permissions: " + modeText(kernel.ApprovalModeAsk) + "."}, nil
	}
	_, ok, err := d.setApprovalMode(ctx, rc.SessionID, kernel.ApprovalModePlan)
	if err != nil {
		return command.Result{}, err
	}
	if !ok {
		return notFoundResult(), nil
	}
	text := "Plan mode is on: the agent can look around but tools that change things don't run. Send /plan off to carry the plan out."
	if prompt != "" {
		return command.Result{Text: text, Action: &command.Action{Type: command.ActionSendPrompt, Prompt: prompt}}, nil
	}
	return command.Result{Text: text}, nil
}

// --- Developer workflow --------------------------------------------------------------

func (d builtInCommandDeps) initCmd(ctx context.Context, _ command.RunContext) (command.Result, error) {
	ws, err := d.sandbox(ctx)
	if err != nil {
		return noWorkspaceResult(err), nil
	}
	return command.Result{
		Text: "Asking the agent to write AGENTS.md" + symEllipsis,
		Action: &command.Action{
			Type: command.ActionSendPrompt,
			Prompt: "Create an AGENTS.md file in the workspace root (" + ws.WorkingDir() + ") with instructions for working in this project. " +
				"If one exists, read it first and improve it instead of replacing what is there.\n\n" +
				"Look around first (README, build and package files, the folder layout, tests, CI). Then write, concisely:\n" +
				"- what the project is and how it is organized;\n" +
				"- the commands to build, test, lint and run it (a single test too);\n" +
				"- conventions the code follows that are not obvious from one file;\n" +
				"- anything to avoid.\n" +
				"Only write what you found in the files; do not guess. Keep it short enough to read in a minute.",
		},
	}, nil
}

func (d builtInCommandDeps) diff(ctx context.Context, _ command.RunContext) (command.Result, error) {
	changes, problem, err := d.gitChanges(ctx)
	if err != nil {
		return command.Result{}, err
	}
	if problem != "" {
		return command.Result{Text: problem, Error: command.ResultUnavailable}, nil
	}
	if changes.status == "" {
		return command.Result{Text: "No uncommitted changes in the workspace."}, nil
	}
	lines := []string{"Uncommitted changes:", "```", changes.status, "```"}
	if changes.diff != "" {
		lines = append(lines, "```diff", clipText(changes.diff, maxDiffChars), "```")
	}
	return command.Result{Text: strings.Join(lines, "\n")}, nil
}

func (d builtInCommandDeps) review(ctx context.Context, rc command.RunContext) (command.Result, error) {
	changes, problem, err := d.gitChanges(ctx)
	if err != nil {
		return command.Result{}, err
	}
	if problem != "" {
		return command.Result{Text: problem, Error: command.ResultUnavailable}, nil
	}
	if changes.status == "" {
		return command.Result{Text: "Nothing to review: the workspace has no uncommitted changes."}, nil
	}
	focus := ""
	if f := rc.Argv["focus"]; f != "" {
		focus = "\n\nFocus on: " + f
	}
	return command.Result{
		Text: "Asking the agent for a read-only review" + symEllipsis,
		Action: &command.Action{
			Type:     command.ActionSendPrompt,
			ReadOnly: true,
			Prompt: "Review the uncommitted changes in the workspace below. This is a read-only review: do not change any file. " +
				"Read the files around the changes when you need more context.\n\n" +
				"Look for bugs, security problems, missing error handling, missing tests and code that does not match " +
				"the project's conventions. List each finding with its file and line, the problem and a suggested fix, " +
				"most severe first. Say so plainly when you find nothing worth changing." +
				focus +
				"\n\ngit status:\n```\n" + changes.status + "\n```\n\n" +
				"git diff:\n```diff\n" + clipText(changes.diff, maxReviewDiffChars) + "\n```",
		},
	}, nil
}

// gitStatus is the workspace's uncommitted changes.
type gitStatus struct{ status, diff string }

var notARepository = regexp.MustCompile(`(?i)not a git repository`)

// gitChanges is `git status --short` and the diff (staged and unstaged) of the workspace,
// run directly in the sandbox - fixed, read-only commands, so no approval is asked.
// problem says why they could not run (a user-facing text); err is a failure of the diff
// commands.
func (d builtInCommandDeps) gitChanges(ctx context.Context) (gitStatus, string, error) {
	ws, err := d.sandbox(ctx)
	if err != nil {
		return gitStatus{}, noWorkspaceResult(err).Text, nil
	}
	if !slices.Contains(ws.Capabilities(), sandboxhub.CapabilityExec) {
		return gitStatus{}, "The agent's workspace can't run git.", nil
	}
	git := func(args ...string) (sandboxhub.ExecResult, error) {
		return ws.Exec(ctx, sandboxhub.ExecRequest{
			Command:        append([]string{"git", "--no-pager"}, args...),
			TimeoutMs:      gitTimeoutMs,
			MaxOutputBytes: gitMaxOutputBytes,
		}, sandboxhub.ExecOptions{})
	}
	status, err := git("status", "--short")
	if err != nil {
		return gitStatus{}, "git could not run in the workspace: " + err.Error(), nil //nolint:nilerr // a git that could not start is told to the user, not a failure of the command
	}
	code := -1
	if status.ExitCode != nil {
		code = *status.ExitCode
	}
	if code == 127 {
		return gitStatus{}, "git isn't available in the agent's workspace.", nil
	}
	if code != 0 {
		stderr := jsstr.Trim(status.Stderr)
		if notARepository.MatchString(stderr) {
			return gitStatus{}, "The workspace (" + ws.WorkingDir() + ") is not a git repository.", nil
		}
		if stderr != "" {
			return gitStatus{}, "git status failed: " + stderr + ".", nil
		}
		return gitStatus{}, "git status failed.", nil
	}
	unstaged, err := git("diff", "--no-color")
	if err != nil {
		return gitStatus{}, "", err
	}
	staged, err := git("diff", "--no-color", "--cached")
	if err != nil {
		return gitStatus{}, "", err
	}
	var diffs []string
	for _, out := range []string{staged.Stdout, unstaged.Stdout} {
		if jsstr.Trim(out) != "" {
			diffs = append(diffs, out)
		}
	}
	return gitStatus{status: jsstr.TrimEnd(status.Stdout), diff: jsstr.TrimEnd(strings.Join(diffs, "\n"))}, "", nil
}

// --- Client --------------------------------------------------------------------------

func (d builtInCommandDeps) copyCmd(ctx context.Context, rc command.RunContext) (command.Result, error) {
	messages, err := d.messages(ctx, rc.SessionID)
	if err != nil {
		return command.Result{}, err
	}
	for i := len(messages) - 1; i >= 0; i-- {
		m := messages[i]
		if m.Role == kernel.RoleAssistant && jsstr.Trim(m.Content) != "" && !m.Summary {
			return command.Result{Text: "Copied the last reply.", Action: &command.Action{Type: command.ActionCopy, Text: m.Content}}, nil
		}
	}
	return command.Result{Text: "Nothing to copy yet: there is no reply in this conversation.", Error: command.ResultNotFound}, nil
}

// --- Helpers -------------------------------------------------------------------------

func notFoundResult() command.Result {
	return command.Result{Text: "This conversation no longer exists.", Error: command.ResultNotFound}
}

func noWorkspaceResult(err error) command.Result {
	detail := ""
	if err != nil && err.Error() != "" {
		detail = " (" + err.Error() + ")"
	}
	return command.Result{Text: "The agent has no workspace to work in" + detail + ".", Error: command.ResultUnavailable}
}

// titleOf is a session's title, or "Untitled conversation".
func titleOf(s kernel.Session) string {
	if v, ok := s.Metadata.Get("title"); ok {
		if title, isString := v.(string); isString && jsstr.Trim(title) != "" {
			return jsstr.Trim(title)
		}
	}
	return "Untitled conversation"
}

// utcMinute is epoch ms as "2006-01-02 15:04" (UTC).
func utcMinute(ms int64) string {
	return time.UnixMilli(ms).UTC().Format("2006-01-02 15:04")
}

// contextLine is "12.3k of 200k tokens (6%, estimated)".
func contextLine(u agentloop.ContextUsage) string {
	pct := 0.0
	if u.ContextWindowTokens > 0 {
		pct = jsstr.Round(float64(u.UsedTokens) / float64(u.ContextWindowTokens) * 100)
	}
	estimated := ""
	if u.UsedSource == agentloop.UsedEstimated {
		estimated = ", estimated"
	}
	return tokensText(u.UsedTokens) + " of " + tokensText(u.ContextWindowTokens) + " tokens (" + strconv.FormatFloat(pct, 'f', -1, 64) + "%" + estimated + ")"
}

// tokensText is 12345 as "12.3k".
func tokensText(n int) string {
	switch {
	case n >= 1_000_000:
		return strings.TrimSuffix(jsstr.ToFixed(float64(n)/1_000_000, 1), ".0") + "M"
	case n >= 1_000:
		return strings.TrimSuffix(jsstr.ToFixed(float64(n)/1_000, 1), ".0") + "k"
	}
	return strconv.Itoa(n)
}

// clipText keeps the first max UTF-16 units of text and says how many were left out.
func clipText(text string, maxUnits int) string {
	n := kernel.UTF16Len(text)
	if n <= maxUnits {
		return text
	}
	return jsstr.TruncateUTF16(text, maxUnits) + "\n" + symEllipsis + " (" + strconv.Itoa(n-maxUnits) + " more characters left out)"
}

// scopeOf is a session scope from key/value pairs (for the clients' invocations).
func scopeOf(pairs ...string) ordered.Object {
	o := ordered.NewObject()
	for i := 0; i+1 < len(pairs); i += 2 {
		o.Set(pairs[i], pairs[i+1])
	}
	return o
}
