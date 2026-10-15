package openaisub

import (
	"net/http"

	"github.com/Phong95/aec-agent-go/components/llmhub"
	"github.com/Phong95/aec-agent-go/components/toolhub"
	"github.com/Phong95/aec-agent-go/internal/jsonx"
	"github.com/Phong95/aec-agent-go/kernel"
)

// ---------------------------------------------------------------------------
// ChatGPT subscription Responses API request shapes
// ---------------------------------------------------------------------------
// The chatgpt.com/backend-api/codex/responses endpoint is not part of the public
// OpenAI SDK - it is the internal Responses API used by ChatGPT's own Codex client,
// reachable with a subscription OAuth bearer token instead of an API key. The
// shapes below are intentionally minimal (only what this adapter writes), with the
// keys in a fixed order.

// textPart is {type, text}: "input_text" for a system or user message and
// "output_text" for an assistant message.
type textPart struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// messageItem is {role, content: [textPart]}.
type messageItem struct {
	Role    string     `json:"role"`
	Content []textPart `json:"content"`
}

// functionCallItem is {type:"function_call", call_id, name, arguments}, where
// arguments is the JSON text of the call's arguments object.
type functionCallItem struct {
	Type      string `json:"type"`
	CallID    string `json:"call_id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// functionCallOutputItem is {type:"function_call_output", call_id, output}.
type functionCallOutputItem struct {
	Type   string `json:"type"`
	CallID string `json:"call_id"`
	Output string `json:"output"`
}

// toolParam is a flat {type:"function", name, description, parameters}: unlike
// Chat Completions there is no function wrapper, and parameters is the whole
// parameter schema as authored.
type toolParam struct {
	Type        string             `json:"type"`
	Name        string             `json:"name"`
	Description string             `json:"description"`
	Parameters  toolhub.JSONSchema `json:"parameters"`
}

// requestBody is the JSON the provider sends to the Responses endpoint, with the
// keys in a fixed order (model, input, store, stream, tools): tools is absent when
// there are none. There is no instructions, temperature or max_tokens: the adapter
// sends none of them.
type requestBody struct {
	Model string `json:"model"`
	// Input is always present, an empty list encodes [].
	Input  []any       `json:"input"`
	Store  bool        `json:"store"`
	Stream bool        `json:"stream"`
	Tools  []toolParam `json:"tools,omitempty"`
}

// inputItems maps the conversation onto Responses input items. The toolResults of
// every message are kept, and an assistant message's text comes before its function
// calls:
//   - ModelOptions.SystemPrompt comes first as a system message, then every system
//     message in order (an empty one too: it is not skipped);
//   - an assistant message becomes an output_text message (only when the content is
//     not empty), then its function_call items, then its function_call_output items,
//     so the text the model wrote before calling comes first and every call comes
//     before its output;
//   - toolResults on a message of any other role (tool, user) become
//     function_call_output items; a user message keeps its text as an input_text
//     message after them, a tool message is its outputs only;
//   - every other message becomes a user message with the content as input_text.
func inputItems(msgs []kernel.Message, opts llmhub.ModelOptions) []any {
	input := make([]any, 0, len(msgs)+1)
	if opts.SystemPrompt != "" {
		input = append(input, messageItem{Role: "system", Content: []textPart{{Type: "input_text", Text: opts.SystemPrompt}}})
	}
	for _, msg := range msgs {
		switch {
		case msg.Role == kernel.RoleSystem:
			input = append(input, messageItem{Role: "system", Content: []textPart{{Type: "input_text", Text: msg.Content}}})
		case msg.Role == kernel.RoleAssistant:
			if msg.Content != "" {
				input = append(input, messageItem{Role: "assistant", Content: []textPart{{Type: "output_text", Text: msg.Content}}})
			}
			for _, tc := range msg.ToolCalls {
				input = append(input, functionCallItem{Type: "function_call", CallID: tc.ID, Name: tc.Name, Arguments: argumentsText(tc.Arguments)})
			}
			input = appendOutputs(input, msg.ToolResults)
		case len(msg.ToolResults) > 0:
			input = appendOutputs(input, msg.ToolResults)
			if msg.Role != kernel.RoleTool && msg.Content != "" {
				input = append(input, messageItem{Role: "user", Content: []textPart{{Type: "input_text", Text: msg.Content}}})
			}
		default:
			input = append(input, messageItem{Role: "user", Content: []textPart{{Type: "input_text", Text: msg.Content}}})
		}
	}
	return input
}

// appendOutputs appends one function_call_output item per result.
func appendOutputs(input []any, results []kernel.ToolResult) []any {
	for _, tr := range results {
		input = append(input, functionCallOutputItem{Type: "function_call_output", CallID: tr.ToolCallID, Output: tr.Content})
	}
	return input
}

// argumentsText is the compact JSON encoding of tc.arguments: the compact bytes of the arguments
// object, {} when there are none.
func argumentsText(a kernel.Arguments) string {
	if len(a) == 0 {
		return "{}"
	}
	return string(a)
}

// mapTools maps the tool definitions: nothing when there are no tools, else one flat entry
// per tool.
func mapTools(tools []toolhub.Definition) []toolParam {
	if len(tools) == 0 {
		return nil
	}
	out := make([]toolParam, 0, len(tools))
	for _, t := range tools {
		out = append(out, toolParam{Type: "function", Name: t.Name, Description: t.Description, Parameters: t.Parameters})
	}
	return out
}

// buildBody returns the exact request body of one call. The Codex Responses API
// requires stream: true, so it is always sent (Chat collects the stream).
func (p *Provider) buildBody(msgs []kernel.Message, opts llmhub.ModelOptions) ([]byte, error) {
	model := opts.Model
	if model == "" {
		model = p.model
	}
	return jsonx.Marshal(requestBody{
		Model:  model,
		Input:  inputItems(msgs, opts),
		Store:  false,
		Stream: true,
		Tools:  mapTools(opts.Tools),
	})
}

// buildHeaders are the headers of a Responses request, from one snapshot of the
// credentials: the bearer token, the JSON content type, the experimental Responses
// flag, and the account id when there is one. Header names are in net/http's
// canonical form (OpenAI-Beta is sent as Openai-Beta); servers match names without
// regard to case.
func buildHeaders(c creds) http.Header {
	h := http.Header{}
	h.Set("Authorization", "Bearer "+c.token)
	h.Set("Content-Type", "application/json")
	h.Set("OpenAI-Beta", "responses=experimental")
	if c.account != "" {
		h.Set("ChatGPT-Account-Id", c.account)
	}
	return h
}

// buildModelsHeaders are the headers of the models GET request. This mirrors the
// server's working request and deliberately does not reuse buildHeaders, which adds
// the Responses-only OpenAI-Beta header (and a content type for a body the GET does
// not have).
func buildModelsHeaders(c creds) http.Header {
	h := http.Header{}
	h.Set("Authorization", "Bearer "+c.token)
	h.Set("originator", "aec-agent")
	if c.account != "" {
		h.Set("ChatGPT-Account-Id", c.account)
	}
	return h
}
