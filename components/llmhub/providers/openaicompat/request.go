package openaicompat

import (
	"github.com/Phong95/aec-agent-go/components/llmhub"
	"github.com/Phong95/aec-agent-go/components/toolhub"
	"github.com/Phong95/aec-agent-go/internal/jsonx"
	"github.com/Phong95/aec-agent-go/kernel"
)

// requestBody is the JSON the provider sends to POST <base>/chat/completions, with
// the keys in a fixed order (model, max_tokens, messages, temperature, top_p, stop,
// tools, and for a stream stream and stream_options): a key with no value is
// absent. max_tokens is absent when ModelOptions.MaxTokens is nil (A-88).
//
// Stream is set for a streaming call so that the flag sits before stream_options. The
// SDK sets "stream":true itself for a streaming call and replaces the existing key in
// place, so the position holds.
//
// The typed SDK parameters are not used for the body because they change the wire
// (an assistant message with null content loses the key), and an API-equal request
// is not enough when a recorded golden is compared byte for byte.
type requestBody struct {
	Model string `json:"model"`
	// MaxTokens is ModelOptions.MaxTokens. The defaultMaxTokens (4096) that was
	// used when ModelOptions.MaxTokens is nil is gone (A-88): the key is then
	// omitted, so the server's default applies instead of cutting large tool calls
	// short. An explicit zero is sent.
	MaxTokens *int `json:"max_tokens,omitempty"`
	// Messages is always present, an empty list encodes [].
	Messages    []any    `json:"messages"`
	Temperature *float64 `json:"temperature,omitempty"`
	TopP        *float64 `json:"top_p,omitempty"`
	// Stop keeps an empty non-nil list as [] and omits nil.
	Stop  []string    `json:"stop,omitzero"`
	Tools []toolParam `json:"tools,omitempty"`
	// ReasoningEffort (A-164) is ModelOptions.ReasoningEffort, absent when not set. The
	// server rejects a level the model does not accept.
	ReasoningEffort string `json:"reasoning_effort,omitempty"`
	// Reasoning (A-177) carries the effort instead of ReasoningEffort when the
	// provider uses ReasoningObjectParam (OpenRouter).
	Reasoning *reasoningParam `json:"reasoning,omitempty"`
	// PromptCacheKey (A-207) is ModelOptions.PromptCacheKey for an endpoint that takes it
	// (WithPromptCacheKey), absent otherwise or while caching is off.
	PromptCacheKey string `json:"prompt_cache_key,omitempty"`
	// SessionID (A-207) is ModelOptions.PromptCacheKey as OpenRouter's sticky-routing key
	// (WithSessionID), absent otherwise or while caching is off.
	SessionID     string         `json:"session_id,omitempty"`
	Stream        *bool          `json:"stream,omitempty"`
	StreamOptions *streamOptions `json:"stream_options,omitempty"`
}

// reasoningParam is {effort}, OpenRouter's reasoning object.
type reasoningParam struct {
	Effort string `json:"effort"`
}

// streamOptions is {include_usage:true}: the final chunk of a stream then carries
// the token usage.
type streamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

// systemParam is {role:"system", content}.
type systemParam struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// userParam is {role:"user", content}.
type userParam struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// userPartsParam (A-165) is a user message whose content is a list of parts (text and
// images).
type userPartsParam struct {
	Role    string `json:"role"`
	Content []any  `json:"content"`
}

// textContentPart is {type:"text", text}.
type textContentPart struct {
	Type string `json:"type"`
	Text string `json:"text"`
	// CacheControl (A-207) is OpenRouter's prompt-cache marker; absent when nil.
	CacheControl *cacheControl `json:"cache_control,omitempty"`
}

// cacheControl (A-207) is {type:"ephemeral"}.
type cacheControl struct {
	Type string `json:"type"`
}

// ephemeral (A-207) is the marker every cached part carries.
var ephemeral = &cacheControl{Type: "ephemeral"}

// partsParam (A-207) is a system or user message whose content is a list of parts (a
// message marked for the prompt cache).
type partsParam struct {
	Role    string `json:"role"`
	Content []any  `json:"content"`
}

// toolPartsParam (A-207) is a tool message whose content is a list of parts (marked for
// the prompt cache).
type toolPartsParam struct {
	Role       string `json:"role"`
	ToolCallID string `json:"tool_call_id"`
	Content    []any  `json:"content"`
}

// imageURLPart is {type:"image_url", image_url:{url}}.
type imageURLPart struct {
	Type     string   `json:"type"`
	ImageURL imageURL `json:"image_url"`
	// CacheControl (A-207) is OpenRouter's prompt-cache marker; absent when nil.
	CacheControl *cacheControl `json:"cache_control,omitempty"`
}

// imageURL is {url}: a data: URL.
type imageURL struct {
	URL string `json:"url"`
}

// toolMessageParam is {role:"tool", tool_call_id, content}, the answer to one call.
type toolMessageParam struct {
	Role       string `json:"role"`
	ToolCallID string `json:"tool_call_id"`
	Content    string `json:"content"`
}

// assistantParam is {role:"assistant", content, tool_calls}. Content is a pointer
// without omitempty so that an assistant message with no text writes "content":null
// (an empty content is sent as null), and ToolCalls is left out when empty.
type assistantParam struct {
	Role      string          `json:"role"`
	Content   *string         `json:"content"`
	ToolCalls []toolCallParam `json:"tool_calls,omitempty"`
}

// toolCallParam is {id, type:"function", function:{name, arguments}}.
type toolCallParam struct {
	ID       string       `json:"id"`
	Type     string       `json:"type"`
	Function functionCall `json:"function"`
}

// functionCall is the function member of a tool call: Arguments is the JSON text of
// the call's arguments object, as a string.
type functionCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// toolParam is {type:"function", function:{name, description, parameters}}.
type toolParam struct {
	Type     string       `json:"type"`
	Function functionSpec `json:"function"`
}

// functionSpec is the function member of a tool: Parameters is the whole schema as
// authored (type, properties, required, ...), passed through unchanged.
type functionSpec struct {
	Name        string             `json:"name"`
	Description string             `json:"description"`
	Parameters  toolhub.JSONSchema `json:"parameters"`
}

// argumentsText is the JSON text of a call's arguments: the stored object, and {}
// when there is none (an Arguments value that is empty encodes as {}).
func argumentsText(a kernel.Arguments) string {
	if len(a) == 0 {
		return "{}"
	}
	return string(a)
}

// mapMessages maps the conversation onto Chat Completions messages. The toolResults
// of every message are kept, and an assistant message keeps its own text and tool
// calls when it has toolResults:
//   - ModelOptions.SystemPrompt comes first as a system message, and every message
//     with role system is appended as a system message too (the OpenAI wire keeps
//     them all, unlike Anthropic where the last one wins);
//   - an assistant message becomes {role:"assistant", content, tool_calls} with
//     content null when it has no text and tool_calls left out when it has no
//     calls; the arguments of a call are sent as the JSON text of the object. An
//     assistant message with toolResults keeps its text and calls (it is left out
//     only when it has neither) and is followed by one tool message per result;
//   - toolResults on a message of any other role (tool, user) become one tool
//     message per result; a user message keeps its text as a user message after
//     them, a tool message is its results only. An empty list sends no tool
//     message;
//   - every other message becomes a user message with the content as a string.
func mapMessages(msgs []kernel.Message, opts llmhub.ModelOptions) []any {
	mapped := make([]any, 0, len(msgs)+1)
	if opts.SystemPrompt != "" {
		mapped = append(mapped, systemParam{Role: "system", Content: opts.SystemPrompt})
	}
	for _, msg := range msgs {
		if msg.Role == kernel.RoleSystem {
			mapped = append(mapped, systemParam{Role: "system", Content: msg.Content})
			continue
		}
		if msg.Role == kernel.RoleAssistant {
			var calls []toolCallParam
			for _, tc := range msg.ToolCalls {
				calls = append(calls, toolCallParam{
					ID:       tc.ID,
					Type:     "function",
					Function: functionCall{Name: tc.Name, Arguments: argumentsText(tc.Arguments)},
				})
			}
			var content *string
			if msg.Content != "" {
				content = &msg.Content
			}
			if content != nil || len(calls) > 0 || len(msg.ToolResults) == 0 {
				mapped = append(mapped, assistantParam{Role: "assistant", Content: content, ToolCalls: calls})
			}
			mapped = appendToolMessages(mapped, msg.ToolResults)
			continue
		}
		if len(msg.ToolResults) > 0 {
			mapped = appendToolMessages(mapped, msg.ToolResults)
			if msg.Role != kernel.RoleTool && msg.Content != "" {
				mapped = append(mapped, userParam{Role: "user", Content: msg.Content})
			}
			continue
		}
		// A-165: attached images as image_url parts (data: URLs) after the text.
		if images := msg.Images(); len(images) > 0 {
			parts := make([]any, 0, len(images)+1)
			if msg.Content != "" {
				parts = append(parts, textContentPart{Type: "text", Text: msg.Content})
			}
			for _, img := range images {
				parts = append(parts, imageURLPart{Type: "image_url", ImageURL: imageURL{URL: img.DataURL()}})
			}
			mapped = append(mapped, userPartsParam{Role: "user", Content: parts})
			continue
		}
		mapped = append(mapped, userParam{Role: "user", Content: msg.Content})
	}
	return mapped
}

// appendToolMessages appends one {role:"tool"} message per result.
//
// A-180: a tool message is text only, so the results' images follow in one user message,
// each result's after a line naming its call (kernel.ToolImagesLabel).
func appendToolMessages(mapped []any, results []kernel.ToolResult) []any {
	for _, tr := range results {
		mapped = append(mapped, toolMessageParam{Role: "tool", ToolCallID: tr.ToolCallID, Content: tr.Content})
	}
	var parts []any
	for _, tr := range results {
		if len(tr.Images) == 0 {
			continue
		}
		parts = append(parts, textContentPart{Type: "text", Text: kernel.ToolImagesLabel(tr)})
		for _, img := range tr.Images {
			parts = append(parts, imageURLPart{Type: "image_url", ImageURL: imageURL{URL: img.DataURL()}})
		}
	}
	if len(parts) > 0 {
		mapped = append(mapped, userPartsParam{Role: "user", Content: parts})
	}
	return mapped
}

// mapTools maps the tool definitions: nothing when there are no tools, else one
// function entry per tool.
func mapTools(tools []toolhub.Definition) []toolParam {
	if len(tools) == 0 {
		return nil
	}
	out := make([]toolParam, 0, len(tools))
	for _, t := range tools {
		out = append(out, toolParam{
			Type:     "function",
			Function: functionSpec{Name: t.Name, Description: t.Description, Parameters: t.Parameters},
		})
	}
	return out
}

// withCacheControl (A-207): OpenRouter caches Claude and Gemini models only when the
// request carries cache_control, so the last system message and the last message get their
// text as a part with the marker (the last system message covers the tools and the system
// prompt, the last message the growing conversation). Other providers and models are left
// as they are.
func (p *Provider) withCacheControl(mapped []any, model string, opts llmhub.ModelOptions) []any {
	if p.cacheControl != OpenRouterCacheControl || !llmhub.PromptCachingOn(opts) || !llmhub.OpenRouterCacheControl(model) {
		return mapped
	}
	lastSystem := -1
	for i := len(mapped) - 1; i >= 0; i-- {
		if _, ok := mapped[i].(systemParam); ok {
			lastSystem = i
			break
		}
	}
	if lastSystem >= 0 {
		mapped[lastSystem] = markLastPart(mapped[lastSystem])
	}
	if n := len(mapped); n > 0 && n-1 != lastSystem {
		mapped[n-1] = markLastPart(mapped[n-1])
	}
	return mapped
}

// markLastPart (A-207) is a system, user or tool message with cache_control on its last
// content part: string content becomes one text part with the marker. An assistant
// message or an empty text is left as it is.
func markLastPart(m any) any {
	switch v := m.(type) {
	case systemParam:
		if v.Content == "" {
			return v
		}
		return partsParam{Role: v.Role, Content: []any{textContentPart{Type: "text", Text: v.Content, CacheControl: ephemeral}}}
	case userParam:
		if v.Content == "" {
			return v
		}
		return partsParam{Role: v.Role, Content: []any{textContentPart{Type: "text", Text: v.Content, CacheControl: ephemeral}}}
	case toolMessageParam:
		if v.Content == "" {
			return v
		}
		return toolPartsParam{Role: v.Role, ToolCallID: v.ToolCallID, Content: []any{textContentPart{Type: "text", Text: v.Content, CacheControl: ephemeral}}}
	case userPartsParam:
		if len(v.Content) == 0 {
			return v
		}
		parts := append([]any(nil), v.Content...)
		switch part := parts[len(parts)-1].(type) {
		case textContentPart:
			part.CacheControl = ephemeral
			parts[len(parts)-1] = part
		case imageURLPart:
			part.CacheControl = ephemeral
			parts[len(parts)-1] = part
		}
		return userPartsParam{Role: v.Role, Content: parts}
	}
	return m
}

// buildRequest returns the exact request body of one call. A streaming body ends
// with "stream":true and "stream_options":{"include_usage":true}.
func (p *Provider) buildRequest(msgs []kernel.Message, opts llmhub.ModelOptions, stream bool) ([]byte, error) {
	model := opts.Model
	if model == "" {
		model = p.defaultModel
	}
	body := requestBody{
		Model:       model,
		MaxTokens:   opts.MaxTokens,
		Messages:    p.withCacheControl(mapMessages(msgs, opts), model, opts), // A-207
		Temperature: opts.Temperature,
		TopP:        opts.TopP,
		Stop:        opts.StopSequences,
		Tools:       mapTools(opts.Tools),
	}
	// A-164; A-177: OpenRouter takes the effort in its reasoning object.
	if opts.ReasoningEffort != "" {
		if p.reasoningParam == ReasoningObjectParam {
			body.Reasoning = &reasoningParam{Effort: opts.ReasoningEffort}
		} else {
			body.ReasoningEffort = opts.ReasoningEffort
		}
	}
	// A-207: prompt_cache_key for the endpoints that take it, while caching is on.
	if opts.PromptCacheKey != "" && llmhub.PromptCachingOn(opts) {
		if p.promptCacheKey {
			body.PromptCacheKey = opts.PromptCacheKey
		}
		if p.sessionID {
			body.SessionID = opts.PromptCacheKey
		}
	}
	if stream {
		on := true
		body.Stream = &on
		body.StreamOptions = &streamOptions{IncludeUsage: true}
	}
	return jsonx.Marshal(body)
}
