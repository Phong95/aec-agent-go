package anthropic

import (
	"strings"

	sdk "github.com/anthropics/anthropic-sdk-go"

	"github.com/Phong95/aec-agent-go/components/llmhub"
	"github.com/Phong95/aec-agent-go/components/toolhub"
	"github.com/Phong95/aec-agent-go/internal/jsonx"
	"github.com/Phong95/aec-agent-go/kernel"
)

const (
	// defaultModel is the model a provider built without cfg.Model uses.
	defaultModel = "claude-sonnet-4-20250514"
	// defaultMaxTokens is used when ModelOptions.MaxTokens is nil: the shared
	// llmhub.DefaultMaxOutputTokens (A-88).
	defaultMaxTokens = llmhub.DefaultMaxOutputTokens
)

// requestBody is the JSON the provider sends to POST /v1/messages, with the keys in
// a fixed order (model, max_tokens, system, messages, temperature, top_p,
// stop_sequences, tools) and fixed omit rules: a key with no value is absent. The
// stream flag is not part of it: the SDK appends "stream":true last for a
// streaming call.
//
// The typed SDK parameters are not used for the body because they change the wire
// (the SDK writes content as block arrays, the provider writes a string), and an
// API-equal request is not enough when a recorded golden is compared byte for byte.
type requestBody struct {
	Model     string `json:"model"`
	MaxTokens int    `json:"max_tokens"`
	// System is omitted when empty.
	System string `json:"system,omitempty"`
	// Messages is always present, an empty list encodes [].
	Messages    []messageParam `json:"messages"`
	Temperature *float64       `json:"temperature,omitempty"`
	TopP        *float64       `json:"top_p,omitempty"`
	// StopSequences keeps an empty non-nil list as [] and omits nil.
	StopSequences []string    `json:"stop_sequences,omitzero"`
	Tools         []toolParam `json:"tools,omitempty"`
	// OutputConfig (A-164) carries the reasoning effort; absent when it is not set.
	OutputConfig *outputConfig `json:"output_config,omitempty"`
	// Thinking (A-178) asks a model that thinks by default for its summarized thinking
	// text (llmhub.ClaudeThinksByDefault); absent for every other model.
	Thinking *llmhub.ClaudeThinkingDisplay `json:"thinking,omitempty"`
}

// outputConfig (A-164) is output_config: the effort level, passed as given (the API
// rejects one the model does not accept).
type outputConfig struct {
	Effort string `json:"effort"`
}

// mapOutputConfig (A-164) is output_config for opts: nil when no reasoning effort is set.
func mapOutputConfig(opts llmhub.ModelOptions) *outputConfig {
	if opts.ReasoningEffort == "" {
		return nil
	}
	return &outputConfig{Effort: opts.ReasoningEffort}
}

// messageParam is one entry of messages. Content is a string for a plain message
// and a list of blocks for an assistant message or a tool-result message.
type messageParam struct {
	Role    string `json:"role"`
	Content any    `json:"content"`
}

// textBlock is {type:"text", text}.
type textBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// imageBlock (A-165) is {type:"image", source:{type:"base64", media_type, data}}.
type imageBlock struct {
	Type   string      `json:"type"`
	Source imageSource `json:"source"`
}

// imageSource is the base64 source of an imageBlock.
type imageSource struct {
	Type      string `json:"type"`
	MediaType string `json:"media_type"`
	Data      string `json:"data"`
}

// userContent (A-165) is a user message's content: the text alone, or with images the
// blocks of its images before its text, as Anthropic recommends.
func userContent(msg kernel.Message) any {
	images := msg.Images()
	if len(images) == 0 {
		return msg.Content
	}
	blocks := make([]any, 0, len(images)+1)
	for _, img := range images {
		blocks = append(blocks, imageBlock{Type: "image", Source: imageSource{Type: "base64", MediaType: img.MIMEType, Data: img.Data}})
	}
	if msg.Content != "" {
		blocks = append(blocks, textBlock{Type: "text", Text: msg.Content})
	}
	return blocks
}

// toolUseBlock is {type:"tool_use", id, name, input}.
type toolUseBlock struct {
	Type  string           `json:"type"`
	ID    string           `json:"id"`
	Name  string           `json:"name"`
	Input kernel.Arguments `json:"input"`
}

// toolResultBlock is {type:"tool_result", tool_use_id, content, is_error}. IsError
// is a pointer so an explicit false is kept and an absent value is omitted.
type toolResultBlock struct {
	Type      string `json:"type"`
	ToolUseID string `json:"tool_use_id"`
	// Content is the result's text, or (A-180) with images the blocks of its text and
	// then its images (toolResultContent).
	Content any   `json:"content"`
	IsError *bool `json:"is_error,omitempty"`
}

// toolResultContent (A-180) is a tool result's content: the text alone, or with images
// a text block and then the blocks of its images - inside the result, where Anthropic
// takes them.
func toolResultContent(tr kernel.ToolResult) any {
	if len(tr.Images) == 0 {
		return tr.Content
	}
	blocks := make([]any, 0, len(tr.Images)+1)
	blocks = append(blocks, textBlock{Type: "text", Text: tr.Content})
	for _, img := range tr.Images {
		blocks = append(blocks, imageBlock{Type: "image", Source: imageSource{Type: "base64", MediaType: img.MIMEType, Data: img.Data}})
	}
	return blocks
}

// toolParam is {name, description, input_schema}.
type toolParam struct {
	Name        string      `json:"name"`
	Description string      `json:"description"`
	InputSchema inputSchema `json:"input_schema"`
}

// inputSchema is {type:"object", properties, required}. Only these two members of
// the tool's parameter schema are sent.
type inputSchema struct {
	Type       string              `json:"type"`
	Properties *toolhub.Properties `json:"properties,omitempty"`
	Required   []string            `json:"required,omitzero"`
}

// mapMessages maps the conversation onto the Messages API. Every system message is
// kept alongside ModelOptions.SystemPrompt, the toolResults of a message whose role
// is not assistant are sent, and an assistant message with toolResults keeps its
// own text and tool calls:
//   - the system text is ModelOptions.SystemPrompt followed by the content of every
//     system message, in order, joined with a newline; empty parts are skipped;
//   - an assistant message becomes text and tool_use blocks, and is left out when it
//     has no block;
//   - toolResults, on a message of any role, become one user message of tool_result
//     blocks, which follows the assistant message of the same entry (the calls come
//     before their results, as the API requires); an empty list sends nothing;
//   - a user message with toolResults keeps its own text as a text block after the
//     results, in the same user message; a tool message is its results only;
//   - every other message becomes a user message with the content as a string.
func mapMessages(msgs []kernel.Message, opts llmhub.ModelOptions) (system string, mapped []messageParam) {
	var systemParts []string
	if opts.SystemPrompt != "" {
		systemParts = append(systemParts, opts.SystemPrompt)
	}
	mapped = make([]messageParam, 0, len(msgs))
	for _, msg := range msgs {
		if msg.Role == kernel.RoleSystem {
			if msg.Content != "" {
				systemParts = append(systemParts, msg.Content)
			}
			continue
		}
		if msg.Role == kernel.RoleAssistant {
			blocks := make([]any, 0, 1+len(msg.ToolCalls))
			if msg.Content != "" {
				blocks = append(blocks, textBlock{Type: "text", Text: msg.Content})
			}
			for _, tc := range msg.ToolCalls {
				blocks = append(blocks, toolUseBlock{Type: "tool_use", ID: tc.ID, Name: tc.Name, Input: tc.Arguments})
			}
			if len(blocks) > 0 {
				mapped = append(mapped, messageParam{Role: "assistant", Content: blocks})
			}
			if len(msg.ToolResults) > 0 {
				mapped = append(mapped, messageParam{Role: "user", Content: resultBlocks(msg.ToolResults, nil)})
			}
			continue
		}
		if len(msg.ToolResults) > 0 {
			var text *textBlock
			if msg.Role != kernel.RoleTool && msg.Content != "" {
				text = &textBlock{Type: "text", Text: msg.Content}
			}
			mapped = append(mapped, messageParam{Role: "user", Content: resultBlocks(msg.ToolResults, text)})
			continue
		}
		mapped = append(mapped, messageParam{Role: "user", Content: userContent(msg)})
	}
	return strings.Join(systemParts, "\n"), mapped
}

// resultBlocks is the content of a user message that answers tool calls: one
// tool_result block per result, then text when it is not nil.
func resultBlocks(results []kernel.ToolResult, text *textBlock) []any {
	blocks := make([]any, 0, len(results)+1)
	for _, tr := range results {
		blocks = append(blocks, toolResultBlock{Type: "tool_result", ToolUseID: tr.ToolCallID, Content: toolResultContent(tr), IsError: tr.IsError})
	}
	if text != nil {
		blocks = append(blocks, *text)
	}
	return blocks
}

// mapTools maps the tool definitions: nothing when there are no tools, else one
// entry per tool with the name, the description and the properties and required list of its
// parameter schema.
func mapTools(tools []toolhub.Definition) []toolParam {
	if len(tools) == 0 {
		return nil
	}
	out := make([]toolParam, 0, len(tools))
	for _, t := range tools {
		out = append(out, toolParam{
			Name:        t.Name,
			Description: t.Description,
			InputSchema: inputSchema{Type: "object", Properties: t.Parameters.Properties, Required: t.Parameters.Required},
		})
	}
	return out
}

// buildRequest returns the exact request body of one call and the typed SDK
// parameters that go with it. The typed Model and MaxTokens are set on the
// parameters so that the SDK's pre-request checks run on the real values (the
// non-streaming guard that refuses a very large max_tokens);
// the body itself replaces the serialised parameters (option.WithRequestBody).
func (p *Provider) buildRequest(msgs []kernel.Message, opts llmhub.ModelOptions) ([]byte, sdk.MessageNewParams, error) {
	model := opts.Model
	if model == "" {
		model = p.defaultModel
	}
	maxTokens := defaultMaxTokens
	if opts.MaxTokens != nil {
		maxTokens = *opts.MaxTokens
	}
	system, mapped := mapMessages(msgs, opts)
	body, err := jsonx.Marshal(requestBody{
		Model:         model,
		MaxTokens:     maxTokens,
		System:        system,
		Messages:      mapped,
		Temperature:   opts.Temperature,
		TopP:          opts.TopP,
		StopSequences: opts.StopSequences,
		Tools:         mapTools(opts.Tools),
		OutputConfig:  mapOutputConfig(opts),
		Thinking:      llmhub.ClaudeThinking(model),
	})
	if err != nil {
		return nil, sdk.MessageNewParams{}, err
	}
	return body, sdk.MessageNewParams{Model: model, MaxTokens: int64(maxTokens)}, nil
}
