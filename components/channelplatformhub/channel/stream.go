package channel

import (
	"errors"
	"iter"

	"github.com/Phong95/aec-agent-go/kernel"
)

// TurnFromStream runs a TurnRunner's turn from a chat stream (Agent.ChatStream), the
// channelTurnFromStream of core/ts: every chunk goes to onChunk (TurnContext.OnChunk) so
// the chat can show the reply while it is written, and the result is read from the
// stream.
//
// The output is the text written after the last tool call: the agent's final reply, as
// RunResult.Message.Content is for Chat. An "error" chunk (for example "Reached maximum
// reasoning steps.") or an error of the sequence is returned as the error.
func TurnFromStream(stream iter.Seq2[kernel.StreamChunk, error], onChunk func(kernel.StreamChunk)) (TurnResult, error) {
	output := ""
	afterToolCall := false
	var result TurnResult
	for chunk, err := range stream {
		if err != nil {
			if chunk.Type == kernel.ChunkError && chunk.Error != "" {
				return TurnResult{}, errors.New(chunk.Error)
			}
			return TurnResult{}, err
		}
		if onChunk != nil {
			onChunk(chunk)
		}
		switch chunk.Type {
		case kernel.ChunkText:
			if afterToolCall {
				output = ""
				afterToolCall = false
			}
			if chunk.Content != nil {
				output += *chunk.Content
			}
		case kernel.ChunkToolCall, kernel.ChunkToolResult:
			afterToolCall = true
		case kernel.ChunkError:
			text := chunk.Error
			if text == "" {
				text = "The agent run failed"
			}
			return TurnResult{}, errors.New(text)
		case kernel.ChunkDone:
			result = TurnResult{Aborted: chunk.Aborted, StopReason: string(chunk.StopReason)}
			if chunk.Usage != nil {
				result.Usage = &Usage{InputTokens: chunk.Usage.InputTokens, OutputTokens: chunk.Usage.OutputTokens}
			}
		default:
		}
	}
	if !afterToolCall {
		result.Output = output
	}
	return result, nil
}
