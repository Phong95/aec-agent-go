package bedrock

import (
	"encoding/base64"
	"fmt"
	"math"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime/types"

	"github.com/Phong95/aec-agent-go/components/llmhub"
	"github.com/Phong95/aec-agent-go/components/toolhub"
	"github.com/Phong95/aec-agent-go/internal/jsonx"
	"github.com/Phong95/aec-agent-go/kernel"
)

// defaultMaxTokens is used when ModelOptions.MaxTokens is nil: the shared
// llmhub.DefaultMaxOutputTokens (A-88).
const defaultMaxTokens = llmhub.DefaultMaxOutputTokens

// emptyToolResult stands in for a tool result with no text: Bedrock rejects an
// empty text block in a tool result.
const emptyToolResult = "[no output]"

// request is what Converse and ConverseStream have in common: the same fields go
// into the two AWS input types, which differ only in name.
type request struct {
	modelID   string
	messages  []types.Message
	system    []types.SystemContentBlock
	inference *types.InferenceConfiguration
	tools     *types.ToolConfiguration
}

// converseInput is the input of a Converse call.
func (r request) converseInput() *bedrockruntime.ConverseInput {
	return &bedrockruntime.ConverseInput{
		ModelId:         aws.String(r.modelID),
		Messages:        r.messages,
		System:          r.system,
		InferenceConfig: r.inference,
		ToolConfig:      r.tools,
	}
}

// streamInput is the input of a ConverseStream call.
func (r request) streamInput() *bedrockruntime.ConverseStreamInput {
	return &bedrockruntime.ConverseStreamInput{
		ModelId:         aws.String(r.modelID),
		Messages:        r.messages,
		System:          r.system,
		InferenceConfig: r.inference,
		ToolConfig:      r.tools,
	}
}

// buildRequest builds the request of one call, for both Chat and Stream:
// the system blocks and messages (mapMessages), the resolved model id, an
// inferenceConfig that is always present with maxTokens defaulting to
// defaultMaxTokens (16384, A-88; it was 4096) and
// temperature, topP and stopSequences only when given, and a toolConfig only when
// there are tools.
func (p *Provider) buildRequest(msgs []kernel.Message, opts llmhub.ModelOptions) (request, error) {
	system, mapped, err := mapMessages(msgs, opts)
	if err != nil {
		return request{}, err
	}
	tools, err := mapTools(opts.Tools)
	if err != nil {
		return request{}, err
	}
	model := opts.Model
	if model == "" {
		model = p.defaultModel
	}
	maxTokens := defaultMaxTokens
	if opts.MaxTokens != nil {
		maxTokens = *opts.MaxTokens
	}
	inference := &types.InferenceConfiguration{
		MaxTokens:     aws.Int32(saturateInt32(maxTokens)),
		StopSequences: opts.StopSequences,
	}
	// Temperature and TopP are float32 on the AWS wire type: a value that is not
	// exactly representable (0.123456789) is written rounded.
	if opts.Temperature != nil {
		inference.Temperature = aws.Float32(float32(*opts.Temperature))
	}
	if opts.TopP != nil {
		inference.TopP = aws.Float32(float32(*opts.TopP))
	}
	return request{
		modelID:   p.resolveModelID(model),
		messages:  mapped,
		system:    system,
		inference: inference,
		tools:     tools,
	}, nil
}

// saturateInt32 converts n to int32, clamping at the limits of the type: the AWS
// field is 32 bits and a larger value is invalid anyway.
func saturateInt32(n int) int32 {
	if n > math.MaxInt32 {
		return math.MaxInt32
	}
	if n < math.MinInt32 {
		return math.MinInt32
	}
	return int32(n) //nolint:gosec // clamped to the int32 range above
}

// mapMessages maps the SDK messages to the Converse format.
//   - ModelOptions.SystemPrompt comes first as a system block, then every system
//     message with text, appended in order; the system array is left out of the
//     request when it is empty;
//   - a message with toolResults, whatever its role, becomes a user message of
//     toolResult blocks: the text is "[no output]" when empty and the status is
//     error or success. The message's own text and tool calls are kept: an assistant
//     message is sent first as itself (text and toolUse blocks), then the results;
//     a user message keeps its text as a text block after the results; a tool
//     message is its results only. An empty list sends no result message;
//   - an assistant message gets a text block when it has text and one toolUse block
//     per call, and is dropped when it has no block at all, because Bedrock rejects
//     an empty content array;
//   - every other message becomes a user message with one text block, and is
//     dropped when its text is empty.
//
// No text block is ever empty: Bedrock rejects {text: ""}.
func mapMessages(msgs []kernel.Message, opts llmhub.ModelOptions) ([]types.SystemContentBlock, []types.Message, error) {
	var system []types.SystemContentBlock
	if opts.SystemPrompt != "" {
		system = append(system, &types.SystemContentBlockMemberText{Value: opts.SystemPrompt})
	}
	mapped := []types.Message{}
	for _, msg := range msgs {
		if msg.Role == kernel.RoleSystem {
			if msg.Content != "" {
				system = append(system, &types.SystemContentBlockMemberText{Value: msg.Content})
			}
			continue
		}

		// Tool results are always a user message, whatever the original role:
		// Bedrock requires them to come from the user role.
		if len(msg.ToolResults) > 0 {
			if msg.Role == kernel.RoleAssistant {
				// The calls come before their results: the assistant part first.
				assistant, err := assistantMessage(msg)
				if err != nil {
					return nil, nil, err
				}
				if assistant != nil {
					mapped = append(mapped, *assistant)
				}
			}
			content := make([]types.ContentBlock, 0, len(msg.ToolResults)+1)
			for _, tr := range msg.ToolResults {
				text := tr.Content
				if text == "" {
					// Guard: Bedrock rejects empty text in tool result content blocks.
					text = emptyToolResult
				}
				status := types.ToolResultStatusSuccess
				if tr.IsError != nil && *tr.IsError {
					status = types.ToolResultStatusError
				}
				content = append(content, &types.ContentBlockMemberToolResult{Value: types.ToolResultBlock{
					ToolUseId: aws.String(tr.ToolCallID),
					Content:   []types.ToolResultContentBlock{&types.ToolResultContentBlockMemberText{Value: text}},
					Status:    status,
				}})
			}
			if msg.Role == kernel.RoleUser && msg.Content != "" {
				content = append(content, &types.ContentBlockMemberText{Value: msg.Content})
			}
			mapped = append(mapped, types.Message{Role: types.ConversationRoleUser, Content: content})
			continue
		}

		// Assistant messages: text and toolUse in one message, with empty-content
		// guards.
		if msg.Role == kernel.RoleAssistant {
			assistant, err := assistantMessage(msg)
			if err != nil {
				return nil, nil, err
			}
			if assistant != nil {
				mapped = append(mapped, *assistant)
			}
			continue
		}

		// User and other messages.
		// A-165: attached images before the text, as raw bytes in the format Converse names.
		var content []types.ContentBlock
		for _, img := range msg.Images() {
			data, err := base64.StdEncoding.DecodeString(img.Data)
			if err != nil {
				return nil, nil, kernel.NewModelError("an attached image is not valid base64", kernel.ModelCodeInvalidRequest, false)
			}
			content = append(content, &types.ContentBlockMemberImage{Value: types.ImageBlock{
				Format: types.ImageFormat(strings.TrimPrefix(img.MIMEType, "image/")),
				Source: &types.ImageSourceMemberBytes{Value: data},
			}})
		}
		if msg.Content != "" {
			content = append(content, &types.ContentBlockMemberText{Value: msg.Content})
		}
		if len(content) > 0 {
			mapped = append(mapped, types.Message{Role: types.ConversationRoleUser, Content: content})
		}
	}
	return system, mapped, nil
}

// assistantMessage is the assistant message of msg: a text block when it has text
// and one toolUse block per call. It is nil when there is no block, because Bedrock
// rejects a message with an empty content array.
func assistantMessage(msg kernel.Message) (*types.Message, error) {
	var content []types.ContentBlock
	if msg.Content != "" {
		content = append(content, &types.ContentBlockMemberText{Value: msg.Content})
	}
	for _, tc := range msg.ToolCalls {
		input, err := toDocument(argumentsJSON(tc.Arguments))
		if err != nil {
			return nil, fmt.Errorf("the arguments of tool call %q are not a JSON object: %w", tc.ID, err)
		}
		content = append(content, &types.ContentBlockMemberToolUse{Value: types.ToolUseBlock{
			ToolUseId: aws.String(tc.ID),
			Name:      aws.String(tc.Name),
			Input:     input,
		}})
	}
	// Only push when there is at least one content block: Bedrock rejects a
	// message with an empty content array.
	if len(content) == 0 {
		return nil, nil
	}
	return &types.Message{Role: types.ConversationRoleAssistant, Content: content}, nil
}

// argumentsJSON is the JSON text of a call's arguments: the stored object, and {}
// when there is none.
func argumentsJSON(a kernel.Arguments) []byte {
	if len(a) == 0 {
		return []byte("{}")
	}
	return []byte(a)
}

// inputSchema is the schema object put in a tool spec: {type:"object",
// properties, required}, with properties and required left out when the tool
// declares none.
type inputSchema struct {
	Type       string              `json:"type"`
	Properties *toolhub.Properties `json:"properties,omitempty"`
	Required   []string            `json:"required,omitzero"`
}

// mapTools maps the tools: nothing when there are no tools, else one toolSpec
// per tool with the schema as a smithy document (keys sorted, see toDocument).
func mapTools(tools []toolhub.Definition) (*types.ToolConfiguration, error) {
	if len(tools) == 0 {
		return nil, nil
	}
	specs := make([]types.Tool, 0, len(tools))
	for _, t := range tools {
		raw, err := jsonx.Marshal(inputSchema{Type: "object", Properties: t.Parameters.Properties, Required: t.Parameters.Required})
		if err != nil {
			return nil, fmt.Errorf("the schema of tool %q cannot be encoded: %w", t.Name, err)
		}
		doc, err := toDocument(raw)
		if err != nil {
			return nil, fmt.Errorf("the schema of tool %q is not a JSON object: %w", t.Name, err)
		}
		specs = append(specs, &types.ToolMemberToolSpec{Value: types.ToolSpecification{
			Name:        aws.String(t.Name),
			Description: aws.String(t.Description),
			InputSchema: &types.ToolInputSchemaMemberJson{Value: doc},
		}})
	}
	return &types.ToolConfiguration{Tools: specs}, nil
}
