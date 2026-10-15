package agentloop

import (
	"context"
	"slices"

	"github.com/Phong95/aec-agent-go/components/events"
	"github.com/Phong95/aec-agent-go/components/llmhub"
	"github.com/Phong95/aec-agent-go/components/toolhub"
	"github.com/Phong95/aec-agent-go/kernel"
)

// measureInput is what measureContext needs: the exact model input and how it was
// built.
type measureInput struct {
	sessionID string
	model     string
	// iteration is set when measured inside a run (0 = on demand).
	iteration int
	// sections are the labelled parts of the always-loaded system message.
	sections []ContextSection
	// messages are the messages sent to the model (or that would be sent).
	messages []kernel.Message
	// recalled is the recalled memories added to the system message, empty for none.
	recalled string
	tools    []toolhub.Definition
	// response is the model's answer when measured after a response: it joins the
	// context, counted from reported.OutputTokens when there is a reported usage.
	response *kernel.Message
	// reported is the provider's usage: input = context sent, output = generated.
	reported *kernel.TokenUsage
	// window and threshold are the session's effective compactor config.
	window    int
	threshold float64
	counter   *Counter
}

// measureContext (A-104) returns what fills a session's context window, by category.
// Every category is an estimate except ContextOther: when the provider reported a
// usage, UsedTokens is that real total and ContextOther is the part of it the estimates
// do not cover (never below 0; nothing is scaled). Without a reported usage, UsedTokens
// is the estimate.
//
// The system message is split into the sections, the recalled memories and whatever a
// middleware added (counted as ContextSystemPrompt). A message the compactor marked
// Summary counts as ContextSummary; every other non-system message as ContextMessages.
// The same rules as the TS SDK (agent-loop/functions/context-usage.ts).
func measureContext(in measureInput) ContextUsage {
	text := func(s string) int {
		if s == "" {
			return 0
		}
		return in.counter.countFresh(kernel.Message{Role: kernel.RoleSystem, Content: s})
	}
	tokens := map[ContextCategoryKey]int{}

	sectionTotal := 0
	for _, s := range in.sections {
		n := text(s.Text)
		tokens[s.Key] += n
		sectionTotal += n
	}
	memories := text(in.recalled)
	tokens[ContextMemories] += memories

	systemTotal := 0
	for _, m := range in.messages {
		switch {
		case m.Role == kernel.RoleSystem:
			systemTotal += in.counter.Count(m)
		case m.Summary:
			tokens[ContextSummary] += in.counter.Count(m)
		default:
			tokens[ContextMessages] += in.counter.Count(m)
		}
	}
	// What the system messages hold beyond the sections and memories (a middleware's
	// additions, separators) is part of the system prompt.
	tokens[ContextSystemPrompt] += max(0, systemTotal-sectionTotal-memories)

	tools := make([]ContextToolTokens, len(in.tools))
	for i, d := range in.tools {
		n := 0
		if b, err := kernel.EncodeJSON(toolTokensShape{Name: d.Name, Description: d.Description, Parameters: d.Parameters}); err == nil {
			n = text(string(b))
		}
		tools[i] = ContextToolTokens{Name: d.Name, Tokens: n}
		tokens[ContextTools] += n
	}

	var reported *kernel.TokenUsage
	if in.reported != nil && in.reported.InputTokens+in.reported.OutputTokens > 0 {
		reported = in.reported
	}
	if in.response != nil {
		if reported != nil {
			tokens[ContextMessages] += reported.OutputTokens
		} else {
			tokens[ContextMessages] += in.counter.Count(*in.response)
		}
	}

	estimated := 0
	for _, n := range tokens {
		estimated += n
	}
	used, source := estimated, UsedEstimated
	if reported != nil {
		used, source = reported.InputTokens+reported.OutputTokens, UsedReported
	}
	tokens[ContextOther] += max(0, used-estimated)

	keys := ContextCategoryKeys()
	categories := make([]ContextCategory, len(keys))
	for i, k := range keys {
		categories[i] = ContextCategory{Key: k, Tokens: tokens[k]}
	}
	return ContextUsage{
		SessionID:           in.sessionID,
		Model:               in.model,
		ContextWindowTokens: in.window,
		CompactAtTokens:     int(float64(in.window) * in.threshold),
		UsedTokens:          used,
		UsedSource:          source,
		FreeTokens:          max(0, in.window-used),
		Categories:          categories,
		Tools:               tools,
		Iteration:           in.iteration,
	}
}

// toolTokensShape is what a tool definition costs: its name, description and
// parameters as JSON (the fields the TS SDK counts).
type toolTokensShape struct {
	Name        string             `json:"name"`
	Description string             `json:"description"`
	Parameters  toolhub.JSONSchema `json:"parameters"`
}

// prepareContext returns the prepared messages and the system message's labelled
// sections; a Context without Prepare (SectionedContext) gives no sections.
func (l *Loop) prepareContext(ctx context.Context, history []kernel.Message, mc MiddlewareContext) ([]kernel.Message, []ContextSection, error) {
	if sc, ok := l.deps.Context.(SectionedContext); ok {
		return sc.Prepare(ctx, history, mc)
	}
	prepared, err := l.deps.Context.PrepareMessages(ctx, history, mc)
	return prepared, nil, err
}

// toolDefinitions returns the agent's tools, plus skill_view when it has skills.
// A-105: a subagent's session sees only the tools its scope allows.
func (l *Loop) toolDefinitions(ctx context.Context, sessionID string) ([]toolhub.Definition, error) {
	toolDefs := slices.Clone(l.deps.Tools.List())
	if inScope := l.toolScope(sessionID); inScope != nil {
		toolDefs = slices.DeleteFunc(toolDefs, func(d toolhub.Definition) bool { return !inScope(d) })
	}
	if toolDefs == nil {
		toolDefs = []toolhub.Definition{}
	}
	if l.deps.Skills != nil {
		skillDefs, err := l.deps.Skills.ToolDefinitions(ctx)
		if err != nil {
			return nil, err
		}
		toolDefs = append(toolDefs, skillDefs...)
	}
	return toolDefs, nil
}

// resolvedModel returns the model a call with opts goes to (the manager's defaults
// merged in, when Models resolves options).
func (l *Loop) resolvedModel(opts llmhub.ModelOptions) string {
	if res, ok := l.deps.Models.(OptionResolver); ok {
		if m := res.ResolveOptions(opts).Model; m != "" {
			return m
		}
	}
	return opts.Model
}

// emitContextUsage emits what fills the session's context window, right after
// ModelResponded (A-104). usage is the provider's reported usage of this response (nil
// or zero = the total is estimated).
func (l *Loop) emitContextUsage(r *runState, iteration int, in modelInput, response kernel.Message, usage *kernel.TokenUsage) {
	recalled := ""
	if r.recalled != nil && r.recalled.Content != nil {
		recalled = *r.recalled.Content
	}
	cfg := r.compactor.config
	u := measureContext(measureInput{
		sessionID: r.sessionID,
		model:     l.resolvedModel(in.options),
		iteration: iteration,
		sections:  in.sections,
		messages:  in.messages,
		recalled:  recalled,
		tools:     in.toolDefs,
		response:  &response,
		reported:  usage,
		window:    cfg.ContextWindowTokens,
		threshold: cfg.Threshold,
		counter:   r.counter,
	})
	emit(r, events.ContextUsage, contextUsagePayload(r.agentID, u))
}

// contextUsagePayload is u in the wire shape of the ContextUsage event.
func contextUsagePayload(agentID string, u ContextUsage) events.ContextUsagePayload {
	categories := make([]events.ContextCategory, len(u.Categories))
	for i, c := range u.Categories {
		categories[i] = events.ContextCategory{Key: string(c.Key), Tokens: c.Tokens}
	}
	tools := make([]events.ContextToolTokens, len(u.Tools))
	for i, t := range u.Tools {
		tools[i] = events.ContextToolTokens(t)
	}
	return events.ContextUsagePayload{
		AgentID:             agentID,
		SessionID:           u.SessionID,
		Model:               u.Model,
		ContextWindowTokens: u.ContextWindowTokens,
		CompactAtTokens:     u.CompactAtTokens,
		UsedTokens:          u.UsedTokens,
		UsedSource:          u.UsedSource,
		FreeTokens:          u.FreeTokens,
		Categories:          categories,
		Tools:               tools,
		Iteration:           u.Iteration,
	}
}

// MeasureContext (A-104) measures what fills the session of conv now, without a model
// call or any write: the same assembly as a model call (system message, working set,
// tools), without compaction or memory recall (recalled memories depend on the next
// message, so they count 0). UsedTokens is the session's last reported usage when it
// has one. The caller loads the conversation; it needs no turn slot.
func (l *Loop) MeasureContext(ctx context.Context, conv Conversation) (ContextUsage, error) {
	session, ok := conv.Session()
	if !ok {
		return ContextUsage{}, ErrNoActiveSession
	}
	counter := NewCounter()
	if session.CounterCalibration != nil {
		counter.Seed(session.CounterCalibration.CharsPerToken, session.CounterCalibration.Samples)
	}
	usage := kernel.TokenUsage{}
	if session.CurrentTokenUsage != nil {
		usage = session.CurrentTokenUsage.Clone()
	}
	mc := MiddlewareContext{Session: session.Clone(), TokenBudget: TokenBudget, Usage: usage}
	prepared, sections, err := l.prepareContext(ctx, withoutLeadingSystem(conv.LLMMessages()), mc)
	if err != nil {
		return ContextUsage{}, err
	}
	tools, err := l.toolDefinitions(ctx, session.ID)
	if err != nil {
		return ContextUsage{}, err
	}
	cfg, _ := sessionCompactorConfig(l.compactor.config, session)
	return measureContext(measureInput{
		sessionID: session.ID,
		model:     l.resolvedModel(llmhub.ModelOptions{Model: session.LLMModel}),
		sections:  sections,
		messages:  repairToolPairs(prepared),
		tools:     tools,
		reported:  session.CurrentTokenUsage,
		window:    cfg.ContextWindowTokens,
		threshold: cfg.Threshold,
		counter:   counter,
	}), nil
}
