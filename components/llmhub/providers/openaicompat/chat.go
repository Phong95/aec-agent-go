package openaicompat

import (
	"context"

	sdk "github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"

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
	body, err := p.buildRequest(msgs, opts, false)
	if err != nil {
		return kernel.ModelResponse{}, kernel.WithCause(kernel.NewModelError(err.Error(), kernel.ModelCodeProvider, false), err)
	}
	var fail failure
	res, err := p.chat.New(ctx, sdk.ChatCompletionNewParams{},
		option.WithRequestBody("application/json", body), option.WithMiddleware(fail.middleware))
	if err != nil {
		if cerr := ctx.Err(); cerr != nil {
			return kernel.ModelResponse{}, cerr
		}
		return kernel.ModelResponse{}, mapError(err, &fail)
	}
	return mapResponse(res)
}

// mapResponse maps a chat completion: the first choice is the answer. Its text
// is "" when the content is null, its tool calls are the ones of type function
// (anything else, such as a custom tool call, is dropped), each built with
// llmhub.ParseToolArguments so that arguments that are not a JSON object become empty
// arguments plus an ArgumentsError instead of failing the call, and its finish
// reason is mapped by mapFinishReason. The usage is the prompt and completion token
// counts, or 0 when the response carries none; there are no cache counts.
//
// A response with no choice is a provider error that is not retryable, with the
// text "the response has no choices".
func mapResponse(res *sdk.ChatCompletion) (kernel.ModelResponse, error) {
	if len(res.Choices) == 0 {
		return kernel.ModelResponse{}, kernel.NewModelError("the response has no choices", kernel.ModelCodeProvider, false)
	}
	choice := res.Choices[0]
	var calls []kernel.ToolCall
	for _, tc := range choice.Message.ToolCalls {
		if tc.Type != "function" {
			continue
		}
		args, argsErr := llmhub.ParseToolArguments(tc.Function.Arguments)
		calls = append(calls, kernel.ToolCall{ID: tc.ID, Name: tc.Function.Name, Arguments: args, ArgumentsError: argsErr})
	}
	stop := mapFinishReason(choice.FinishReason)
	if stop == kernel.StopEndTurn && len(calls) > 0 {
		// Some OpenAI-compatible servers (Ollama among them) answer a tool call with
		// finish_reason "stop": the calls decide, so the agent loop runs them.
		stop = kernel.StopToolUse
	}
	return kernel.ModelResponse{
		Message: kernel.Message{Role: kernel.RoleAssistant, Content: choice.Message.Content, ToolCalls: calls},
		Usage: kernel.TokenUsage{
			InputTokens:  int(res.Usage.PromptTokens),
			OutputTokens: int(res.Usage.CompletionTokens),
		},
		StopReason: stop,
	}, nil
}

// mapFinishReason maps the finish reason onto the four kernel reasons:
//   - stop, an absent or null reason (several OpenAI-compatible servers send none)
//     and any reason this version does not know are end_turn;
//   - tool_calls and the legacy function_call are tool_use;
//   - length is max_tokens;
//   - content_filter is error: the answer was withheld or cut by a filter.
func mapFinishReason(reason string) kernel.StopReason {
	switch reason {
	case "tool_calls", "function_call":
		return kernel.StopToolUse
	case "length":
		return kernel.StopMaxTokens
	case "content_filter":
		return kernel.StopError
	default:
		// stop, "" and unknown reasons.
		return kernel.StopEndTurn
	}
}
