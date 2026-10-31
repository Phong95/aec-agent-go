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
	Reasoning     *reasoningParam `json:"reasoning,omitempty"`
	Stream        *bool           `json:"stream,omitempty"`
	StreamOptions *streamOptions  `json:"stream_options,omitempty"`
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
}

// imageURLPart is {type:"image_url", image_url:{url}}.
type imageURLPart struct {
	Type     string   `json:"type"`
	ImageURL imageURL `json:"image_url"`
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
func appendToolMessages(mapped []any, results []kernel.ToolResult) []any {
	for _, tr := range results {
		mapped = append(mapped, toolMessageParam{Role: "tool", ToolCallID: tr.ToolCallID, Content: tr.Content})
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
		Messages:    mapMessages(msgs, opts),
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
	if stream {
		on := true
		body.Stream = &on
		body.StreamOptions = &streamOptions{IncludeUsage: true}
	}
	return jsonx.Marshal(body)
}
