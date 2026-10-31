package anthropic

import (
	"context"
	"strings"

	sdk "github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"

	"github.com/Phong95/aec-agent-go/components/llmhub"
	"github.com/Phong95/aec-agent-go/kernel"
)

// Chat sends one non-streaming request and maps the answer.
//
// A failure is a *kernel.ModelError (mapError). When the caller's context is done
// the bare ctx.Err() is returned instead, so errors.Is(err, context.Canceled)
// works: an aborted call is not a model failure.
func (p *Provider) Chat(ctx context.Context, msgs []kernel.Message, opts llmhub.ModelOptions) (kernel.ModelResponse, error) {
	if err := ctx.Err(); err != nil {
		return kernel.ModelResponse{}, err
	}
	body, params, err := p.buildRequest(msgs, opts)
	if err != nil {
		return kernel.ModelResponse{}, kernel.WithCause(kernel.NewModelError(err.Error(), kernel.ModelCodeProvider, false), err)
	}
	msg, err := p.client.Messages.New(ctx, params, option.WithRequestBody("application/json", body))
	if err != nil {
		if cerr := ctx.Err(); cerr != nil {
			return kernel.ModelResponse{}, cerr
		}
		return kernel.ModelResponse{}, mapError(err)
	}
	return mapResponse(msg), nil
}

// mapResponse maps the answer: the text blocks are joined (an empty string
// when there are none), every tool_use block becomes a tool call, and thinking
// blocks are dropped. The cache token counts are set only when the response
// carries the field, so a present zero stays 0 and an absent field stays absent.
// A JSON null counts as absent.
//
// A tool call's arguments are the block's input object re-encoded through the
// same path as a streamed call (llmhub.ParseToolArguments), which cannot fail: an
// input that is not an object becomes empty arguments plus an ArgumentsError.
func mapResponse(msg *sdk.Message) kernel.ModelResponse {
	var text, reasoning strings.Builder
	var calls []kernel.ToolCall
	for _, block := range msg.Content {
		switch block.Type {
		case "text":
			text.WriteString(block.Text)
		case "thinking":
			reasoning.WriteString(block.Thinking)
		case "tool_use":
			args, argsErr := llmhub.ParseToolArguments(string(block.Input))
			calls = append(calls, kernel.ToolCall{ID: block.ID, Name: block.Name, Arguments: args, ArgumentsError: argsErr})
		}
	}
	usage := kernel.TokenUsage{
		InputTokens:  int(msg.Usage.InputTokens),
		OutputTokens: int(msg.Usage.OutputTokens),
	}
	if msg.Usage.JSON.CacheReadInputTokens.Valid() {
		v := int(msg.Usage.CacheReadInputTokens)
		usage.CacheReadTokens = &v
	}
	if msg.Usage.JSON.CacheCreationInputTokens.Valid() {
		v := int(msg.Usage.CacheCreationInputTokens)
		usage.CacheWriteTokens = &v
	}
	return kernel.ModelResponse{
		Message:    kernel.Message{Role: kernel.RoleAssistant, Content: text.String(), ToolCalls: calls, Reasoning: reasoning.String()},
		Usage:      usage,
		StopReason: mapStopReason(string(msg.StopReason)),
	}
}

// mapStopReason maps the Anthropic stop reason onto the four kernel reasons. Only a
// refusal is an error:
//   - end_turn, stop_sequence (a normal end on a caller's stop sequence), an absent
//     or null reason and any reason this version does not know are end_turn: the
//     answer is complete as far as the client can tell, and a new reason the API
//     adds must not fail every call;
//   - pause_turn is end_turn too: the API paused a long server-tool turn and the
//     content so far is valid; a caller continues it by sending the answer back,
//     which is what a finished turn allows, where "error" would discard it;
//   - tool_use is tool_use;
//   - max_tokens and model_context_window_exceeded are max_tokens: the answer was
//     cut by a token limit;
//   - refusal is error: the model declined and the content is not an answer.
func mapStopReason(reason string) kernel.StopReason {
	switch reason {
	case "tool_use":
		return kernel.StopToolUse
	case "max_tokens", "model_context_window_exceeded":
		return kernel.StopMaxTokens
	case "refusal":
		return kernel.StopError
	default:
		// end_turn, stop_sequence, pause_turn, "" and unknown reasons.
		return kernel.StopEndTurn
	}
}
