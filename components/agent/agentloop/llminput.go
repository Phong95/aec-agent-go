package agentloop

import (
	"bytes"
	"slices"
	"strconv"

	"github.com/Phong95/aec-agent-go/components/agent/memorysystem/memory"
	"github.com/Phong95/aec-agent-go/components/llmhub"
	"github.com/Phong95/aec-agent-go/kernel"
	"github.com/Phong95/aec-agent-go/kernel/ordered"
)

// The values of the memoryInjection attribute of the context-assembly span.
const (
	injectionNone              = "none"
	injectionAppendedToSystem  = "appended-to-system"
	injectionNewSystemMessage  = "new-system-message"
	memorySeparatorBeforeBlock = "\n\n"
)

// recalledContent is the text a recall asks to inject: empty when there is no
// recall, no content or empty content (an empty string is nothing to inject).
func recalledContent(recalled *memory.RecalledMemories) string {
	if recalled == nil || recalled.Content == nil {
		return ""
	}
	return *recalled.Content
}

// MaxToolImagesInContext (A-180) is the most tool-result images one model request carries.
// A screenshot loop takes several per round; older ones are rarely worth their tokens
// (kernel.ImageTokenEstimate each), and the model can take a new one.
const MaxToolImagesInContext = 6

// withToolImages (A-180) returns msgs with only the newest tool-result images kept: whole
// results' images, newest first, while they fit in keep (0 for a model that can't see
// images). A result whose images are left out says so in its content, so the model knows
// to take a new screenshot. The stored conversation keeps every image. It returns msgs
// itself when nothing is left out, and never modifies a message of msgs.
func withToolImages(msgs []kernel.Message, keep int) []kernel.Message {
	left := max(0, keep)
	var out []kernel.Message
	for i := len(msgs) - 1; i >= 0; i-- {
		if len(msgs[i].ToolResultImages()) == 0 {
			continue
		}
		var results []kernel.ToolResult
		// The later result of a message is the newer one, so it is kept first.
		for j := len(msgs[i].ToolResults) - 1; j >= 0; j-- {
			r := msgs[i].ToolResults[j]
			n := len(r.Images)
			if n == 0 {
				continue
			}
			if n <= left {
				left -= n
				continue
			}
			left = 0
			if results == nil {
				results = slices.Clone(msgs[i].ToolResults)
			}
			why := "this model can't see images"
			if keep > 0 {
				why = "an older image, no longer shown to save context; take a new screenshot to look again"
			}
			noun := "images"
			if n == 1 {
				noun = "image"
			}
			results[j].Images = nil
			results[j].Content = r.Content + "\n\n(" + strconv.Itoa(n) + " " + noun + " not shown: " + why + ".)"
		}
		if results == nil {
			continue
		}
		if out == nil {
			out = slices.Clone(msgs)
		}
		out[i].ToolResults = results
	}
	if out == nil {
		return msgs
	}
	return out
}

// withCompactArguments (A-129) returns msgs with each tool call's arguments replaced
// by the compact ones its successful result carries (ToolResult.CompactArguments -
// e.g. a written file's content replaced by a reference to the file). The stored
// conversation keeps the originals for the UI. The same input always gives the same
// output, so a call once replaced reads the same in every later request and the
// prompt cache keeps it. It returns msgs itself when nothing is replaced and never
// changes msgs or the messages in it; call ids are kept, so calls and results stay
// paired.
//
// A-175: only calls from earlier turns are replaced; the current turn (from the latest
// user message on) keeps its full arguments. Models that saw their own calls of this
// turn shortened took the reference for a placeholder they wrote, and copied it as the
// content of their next write. A turn's calls are replaced once the next turn starts.
func withCompactArguments(msgs []kernel.Message) []kernel.Message {
	compact := map[string]kernel.Arguments{}
	earlier := msgs
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == kernel.RoleUser {
			earlier = msgs[:i]
			break
		}
	}
	for _, m := range earlier {
		for _, r := range m.ToolResults {
			if len(r.CompactArguments) > 0 && (r.IsError == nil || !*r.IsError) {
				compact[r.ToolCallID] = r.CompactArguments
			}
		}
	}
	if len(compact) == 0 {
		return msgs
	}
	var out []kernel.Message
	for i, m := range msgs {
		if !slices.ContainsFunc(m.ToolCalls, func(c kernel.ToolCall) bool { _, ok := compact[c.ID]; return ok }) {
			continue
		}
		if out == nil {
			out = slices.Clone(msgs)
		}
		replaced := m.Clone()
		for j, c := range replaced.ToolCalls {
			if args, ok := compact[c.ID]; ok {
				replaced.ToolCalls[j].Arguments = slices.Clone(args)
			}
		}
		out[i] = replaced
	}
	if out == nil {
		return msgs
	}
	return out
}

// withRecalledMemories adds the recalled memories to the model input: a blank line and
// the content are appended to the LAST system message, or, when there is none, a new
// system message holding the content is put in front. It returns msgs itself, the same
// slice, when there is nothing to inject, and never changes msgs or the messages in it.
func withRecalledMemories(msgs []kernel.Message, recalled *memory.RecalledMemories) []kernel.Message {
	content := recalledContent(recalled)
	if content == "" {
		return msgs
	}
	idx := lastSystemIndex(msgs)
	if idx == -1 {
		return append([]kernel.Message{{Role: kernel.RoleSystem, Content: content}}, msgs...)
	}
	out := slices.Clone(msgs)
	out[idx].Content += memorySeparatorBeforeBlock + content
	return out
}

// lastSystemIndex is the index of the last system message, -1 when there is none.
func lastSystemIndex(msgs []kernel.Message) int {
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == kernel.RoleSystem {
			return i
		}
	}
	return -1
}

// llmInputParams is what describeLlmInput needs to know about one iteration.
type llmInputParams struct {
	iteration int
	// prepared are the messages the context port produced, before compaction.
	prepared []kernel.Message
	// compact is the compaction result: its messages are the base of the model input.
	compact CompactorResult
	// llmMessages is the exact model input.
	llmMessages []kernel.Message
	recalled    *memory.RecalledMemories
	options     llmhub.ModelOptions
	// counter is the run's token counter (A-97: per run, not per loop).
	counter *Counter
}

// llmInput is the description of one model input: the attributes of the
// context-assembly span, and the facts the model-call span repeats.
type llmInput struct {
	attrs ordered.Object
	call  callTrace
}

// describeLlmInput builds the full breakdown of the exact model input of one
// iteration, for the context-assembly span: the messages with their estimated
// tokens, the tools, the effective options, the provider, the memory injection and
// the token estimate by part. The attribute keys are in a fixed order and a key
// without a value is left out.
//
// It never fails: tracing must not break the loop. A provider
// that cannot be named just leaves the provider attribute out, and Models that
// implement neither OptionResolver nor ProviderNamer are described by the options
// they were given.
func (l *Loop) describeLlmInput(p llmInputParams) llmInput {
	resolved := p.options
	if res, ok := l.deps.Models.(OptionResolver); ok {
		resolved = res.ResolveOptions(p.options)
	}
	// The resolved tools win, then the options' own; the tools are traced by name,
	// description and parameters only.
	defs := resolved.Tools
	if defs == nil {
		defs = p.options.Tools
	}
	tools := make([]any, len(defs))
	toolNames := make([]string, len(defs))
	for i, d := range defs {
		tools[i] = obj("name", d.Name, "description", d.Description, "parameters", d.Parameters)
		toolNames[i] = d.Name
	}

	provider, hasProvider := "", false
	if namer, ok := l.deps.Models.(ProviderNamer); ok {
		if name, err := namer.ResolveProviderName(p.options.Provider); err == nil {
			provider, hasProvider = name, true
		}
	}

	// The memory injection mode mirrors withRecalledMemories' own targeting.
	content := recalledContent(p.recalled)
	injection := injectionNone
	if content != "" {
		injection = injectionNewSystemMessage
		if lastSystemIndex(p.compact.Messages) != -1 {
			injection = injectionAppendedToSystem
		}
	}

	// The summary index shifts by one when memory injection put a brand-new system
	// message in front.
	summaryIndex := 0
	if p.compact.Compacted {
		summaryIndex = p.compact.SummaryIndex
		if injection == injectionNewSystemMessage {
			summaryIndex++
		}
	}

	// The system message that memory injection rewrote (or prepended) is new on every
	// call, so it is counted fresh; every other message is counted once and cached.
	freshIdx := -1
	if injection != injectionNone {
		freshIdx = lastSystemIndex(p.llmMessages)
	}
	traced := make([]any, len(p.llmMessages))
	messagesTokens := 0
	for i, m := range p.llmMessages {
		var n int
		if i == freshIdx {
			n = p.counter.countFresh(m)
		} else {
			n = p.counter.Count(m)
		}
		messagesTokens += n
		traced[i] = traceMessage(m, n)
	}

	memoryTokens := 0
	if p.recalled != nil && p.recalled.Content != nil && *p.recalled.Content != "" {
		memoryTokens = p.counter.countFresh(kernel.Message{Role: kernel.RoleSystem, Content: *p.recalled.Content})
	}
	toolTokens := 0
	if len(tools) > 0 {
		// The token cost of the tool list is its JSON, counted as a system message.
		if b, err := kernel.EncodeJSON(tools); err == nil {
			toolTokens = p.counter.countFresh(kernel.Message{Role: kernel.RoleSystem, Content: string(b)})
		}
	}

	recordIDs := []string{}
	recalledCount := 0
	if p.recalled != nil {
		recalledCount = len(p.recalled.RecordIDs)
		recordIDs = slices.Clone(p.recalled.RecordIDs)
		if recordIDs == nil {
			recordIDs = []string{}
		}
	}

	effective := optionsObject(resolved)

	a := obj(
		"iteration", p.iteration,
		"messageCount", len(p.llmMessages),
		"preparedMessageCount", len(p.prepared),
		"compacted", p.compact.Compacted,
	)
	if p.compact.Compacted {
		a.Set("summaryIndex", summaryIndex)
	}
	a.Set("recalledMemories", recalledCount)
	a.Set("memoryRecordIds", recordIDs)
	if p.recalled != nil && p.recalled.Content != nil {
		a.Set("memoryBlock", *p.recalled.Content)
	}
	a.Set("memoryInjection", injection)
	a.Set("memoryTokens", memoryTokens)
	// The LAST system message of the actual model input.
	if i := lastSystemIndex(p.llmMessages); i != -1 {
		a.Set("systemMessageIndex", i)
	}
	// The base system prompt: the last system message BEFORE memory injection.
	if i := lastSystemIndex(p.compact.Messages); i != -1 {
		a.Set("baseSystemPrompt", p.compact.Messages[i].Content)
	}
	a.Set("messages", traced)
	a.Set("tools", tools)
	a.Set("toolNames", toolNames)
	if hasProvider {
		a.Set("provider", provider)
	}
	a.Set("options", effective)
	a.Set("estimatedTokens", obj(
		"messages", messagesTokens,
		"memory", memoryTokens,
		"tools", toolTokens,
		"total", messagesTokens+toolTokens,
	))

	return llmInput{
		attrs: a,
		call:  callTrace{toolNames: toolNames, provider: provider, hasProv: hasProvider, model: resolved.Model, options: effective},
	}
}

// traceMessage is one message as the context-assembly span shows it: role, content,
// the timestamp when it has one, the tool calls (id, name, arguments) and tool
// results (tool call id, content, error flag) when present, and its token
// estimate. A present but empty list is kept.
func traceMessage(m kernel.Message, tokens int) ordered.Object {
	o := obj("role", string(m.Role), "content", m.Content)
	if m.Timestamp != "" {
		o.Set("timestamp", m.Timestamp)
	}
	if m.ToolCalls != nil {
		calls := make([]any, len(m.ToolCalls))
		for i, c := range m.ToolCalls {
			calls[i] = obj("id", c.ID, "name", c.Name, "arguments", kernel.Arguments(bytes.Clone(c.Arguments)))
		}
		o.Set("toolCalls", calls)
	}
	if m.ToolResults != nil {
		results := make([]any, len(m.ToolResults))
		for i, r := range m.ToolResults {
			res := obj("toolCallId", r.ToolCallID, "content", r.Content)
			if r.IsError != nil {
				res.Set("isError", *r.IsError)
			}
			results[i] = res
		}
		o.Set("toolResults", results)
	}
	o.Set("estimatedTokens", tokens)
	return o
}
