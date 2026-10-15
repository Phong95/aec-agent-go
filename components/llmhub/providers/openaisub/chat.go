package openaisub

import (
	"context"
	"strings"

	"github.com/Phong95/aec-agent-go/components/llmhub"
	"github.com/Phong95/aec-agent-go/kernel"
)

// Chat collects Stream into one response: the Codex Responses API requires
// stream: true, so there is no other way to ask.
//
// Two details matter here:
//   - an error chunk is returned as the typed error the stream mapped (auth,
//     rate_limit, timeout, invalid_request or provider, with its own retryability),
//     so an expired token (a 401, which the stream reports as "Invalid or expired
//     OAuth token") is an auth error and not a retryable provider error; the text
//     is the chunk's;
//   - the stop reason reads the final response status, so a response that was cut
//     short or refused can be told apart: an incomplete response is max_tokens
//     (error when a content filter stopped it), and a failed response is the error
//     of the failure (the stream ends with it). Otherwise it is tool_use (when the
//     stream produced a tool call) or end_turn. Since A-88 Chat reads it from the
//     done chunk, where the stream reports it by the same rule.
//
// Usage is the done chunk's, and zero input and output tokens when the stream gave
// none. When the caller's context is done the bare ctx.Err() is returned instead of
// an error (an aborted call is not a model failure).
func (p *Provider) Chat(ctx context.Context, msgs []kernel.Message, opts llmhub.ModelOptions) (kernel.ModelResponse, error) {
	var (
		text  strings.Builder
		calls []kernel.ToolCall
		usage *kernel.TokenUsage
		// stop is the stop reason of the done chunk (A-88).
		stop kernel.StopReason
		res  streamResult
	)
	for chunk, err := range p.stream(ctx, msgs, opts, &res) {
		if err != nil {
			return kernel.ModelResponse{}, err
		}
		switch chunk.Type {
		case kernel.ChunkText:
			if chunk.Content != nil {
				text.WriteString(*chunk.Content)
			}
		case kernel.ChunkToolCall:
			if chunk.ToolCall != nil {
				calls = append(calls, *chunk.ToolCall)
			}
		case kernel.ChunkDone:
			usage = chunk.Usage
			stop = chunk.StopReason
		case kernel.ChunkToolCallStart, kernel.ChunkToolCallDelta, kernel.ChunkToolResult, kernel.ChunkUsage:
			// Progress chunks carry nothing Chat needs: the complete tool_call follows.
			// (A provider never sends usage chunks; the agent loop does: usage is on done.)
		case kernel.ChunkError:
			if res.err != nil {
				return kernel.ModelResponse{}, res.err
			}
			message := chunk.Error
			if message == "" {
				message = "Unknown stream error"
			}
			return kernel.ModelResponse{}, kernel.NewModelError(message, kernel.ModelCodeProvider, true)
		}
	}
	resp := kernel.ModelResponse{
		Message:    kernel.Message{Role: kernel.RoleAssistant, Content: text.String(), ToolCalls: calls},
		StopReason: kernel.StopEndTurn,
	}
	if usage != nil {
		resp.Usage = *usage
	}
	switch {
	case stop != "":
		resp.StopReason = stop
	case len(calls) > 0:
		resp.StopReason = kernel.StopToolUse
	}
	return resp, nil
}
