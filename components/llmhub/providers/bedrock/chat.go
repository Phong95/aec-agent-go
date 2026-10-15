package bedrock

import (
	"context"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime/types"

	"github.com/Phong95/aec-agent-go/components/llmhub"
	"github.com/Phong95/aec-agent-go/kernel"
)

// Chat sends one non-streaming Converse request and maps the answer.
//
// A failure is a *kernel.ModelError (mapError). When the caller's context is done
// the bare ctx.Err() is returned instead, so errors.Is(err, context.Canceled)
// works: an aborted call is not a model failure.
func (p *Provider) Chat(ctx context.Context, msgs []kernel.Message, opts llmhub.ModelOptions) (kernel.ModelResponse, error) {
	if err := ctx.Err(); err != nil {
		return kernel.ModelResponse{}, err
	}
	req, err := p.buildRequest(msgs, opts)
	if err != nil {
		return kernel.ModelResponse{}, kernel.WithCause(kernel.NewModelError(err.Error(), kernel.ModelCodeProvider, false), err)
	}
	out, err := p.converse.Converse(ctx, req.converseInput())
	if err != nil {
		if cerr := ctx.Err(); cerr != nil {
			return kernel.ModelResponse{}, cerr
		}
		return kernel.ModelResponse{}, mapError(err, req.modelID)
	}
	return mapResponse(out), nil
}

// mapResponse maps a Converse response: the text blocks are joined (an empty string
// when there are none) and every toolUse block becomes a tool call whose id and name
// are "" when the response has none. A call's input document is read as JSON
// (numbers as float64), written with jsonx (keys sorted) and turned into Arguments
// by kernel.NewArguments; an input that is not an object, or cannot be read,
// becomes empty arguments plus an ArgumentsError, so a response is never failed for
// it. A missing input is {}.
//
// The usage is the input and output token counts, or 0 when the response carries
// none; there are no cache counts. The stop reason is mapped by mapStopReason.
func mapResponse(out *bedrockruntime.ConverseOutput) kernel.ModelResponse {
	var text strings.Builder
	var calls []kernel.ToolCall
	var usage kernel.TokenUsage
	stop := ""
	if out != nil {
		stop = string(out.StopReason)
		if out.Usage != nil {
			usage = kernel.TokenUsage{
				InputTokens:  int(aws.ToInt32(out.Usage.InputTokens)),
				OutputTokens: int(aws.ToInt32(out.Usage.OutputTokens)),
			}
		}
		if msg, ok := out.Output.(*types.ConverseOutputMemberMessage); ok {
			for _, block := range msg.Value.Content {
				switch b := block.(type) {
				case *types.ContentBlockMemberText:
					text.WriteString(b.Value)
				case *types.ContentBlockMemberToolUse:
					args, argsErr := toolArguments(b.Value)
					calls = append(calls, kernel.ToolCall{
						ID:             aws.ToString(b.Value.ToolUseId),
						Name:           aws.ToString(b.Value.Name),
						Arguments:      args,
						ArgumentsError: argsErr,
					})
				default:
					// Images, documents and reasoning blocks are dropped.
				}
			}
		}
	}
	return kernel.ModelResponse{
		Message:    kernel.Message{Role: kernel.RoleAssistant, Content: text.String(), ToolCalls: calls},
		Usage:      usage,
		StopReason: mapStopReason(stop),
	}
}

// toolArguments reads the input document of a toolUse block.
func toolArguments(b types.ToolUseBlock) (kernel.Arguments, string) {
	if b.Input == nil {
		return nil, ""
	}
	raw, err := fromDocument(b.Input)
	if err != nil {
		return nil, "the arguments could not be read: " + err.Error()
	}
	if string(raw) == "null" {
		// A null input is no arguments ({}).
		return nil, ""
	}
	return llmhub.ParseToolArguments(string(raw))
}

// mapStopReason maps the Converse stop reason onto the four kernel reasons:
//   - end_turn, stop_sequence (a normal end on a caller's stop sequence), an absent
//     reason and any reason this version does not know are end_turn;
//   - tool_use is tool_use;
//   - max_tokens and model_context_window_exceeded are max_tokens;
//   - guardrail_intervened and content_filtered are error: a guardrail or a filter
//     replaced or cut the answer.
func mapStopReason(reason string) kernel.StopReason {
	switch reason {
	case "tool_use":
		return kernel.StopToolUse
	case "max_tokens", "model_context_window_exceeded":
		return kernel.StopMaxTokens
	case "guardrail_intervened", "content_filtered":
		return kernel.StopError
	default:
		// end_turn, stop_sequence, "" and unknown reasons.
		return kernel.StopEndTurn
	}
}
