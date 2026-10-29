package gemini

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"

	"github.com/Phong95/aec-agent-go/components/llmhub"
	"github.com/Phong95/aec-agent-go/kernel"
)

// ---------------------------------------------------------------------------
// Google Gemini API response shapes
// ---------------------------------------------------------------------------
// One shape serves generateContent and every event of streamGenerateContent. Only
// what the adapter reads is decoded.

// responsePart is one part of a candidate's content.
type responsePart struct {
	Text *string `json:"text"`
	// Thought is set on a part that holds the model's thinking, which the adapter
	// never shows.
	Thought bool `json:"thought"`
	// ThoughtSignature is the token the model needs back with the part (required for
	// function calls on Gemini 3).
	ThoughtSignature string `json:"thoughtSignature"`
	FunctionCall     *struct {
		ID   string          `json:"id"`
		Name string          `json:"name"`
		Args json.RawMessage `json:"args"`
	} `json:"functionCall"`
}

// usageMetadata is the token usage of a response.
type usageMetadata struct {
	PromptTokenCount     int `json:"promptTokenCount"`
	CandidatesTokenCount int `json:"candidatesTokenCount"`
	// ThoughtsTokenCount is the thinking tokens: billed as output and counted against
	// maxOutputTokens.
	ThoughtsTokenCount int `json:"thoughtsTokenCount"`
}

// apiError is the {code, message, status, details} of an error body or event.
type apiError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Status  string `json:"status"`
	Details []struct {
		Reason string `json:"reason"`
	} `json:"details"`
}

// responseBody is a generateContent response or one stream event.
type responseBody struct {
	Candidates []struct {
		Content *struct {
			Parts []responsePart `json:"parts"`
		} `json:"content"`
		FinishReason string `json:"finishReason"`
	} `json:"candidates"`
	PromptFeedback *struct {
		BlockReason string `json:"blockReason"`
	} `json:"promptFeedback"`
	UsageMetadata *usageMetadata `json:"usageMetadata"`
	// Error is an error the stream reports in place of a response.
	Error *apiError `json:"error"`
}

// parts is the parts of the first candidate.
func (r *responseBody) parts() []responsePart {
	if len(r.Candidates) == 0 || r.Candidates[0].Content == nil {
		return nil
	}
	return r.Candidates[0].Content.Parts
}

// finishReason is the first candidate's finish reason; a prompt blocked before any
// candidate reads as its block reason. Empty when neither was reported.
func (r *responseBody) finishReason() string {
	if len(r.Candidates) > 0 && r.Candidates[0].FinishReason != "" {
		return r.Candidates[0].FinishReason
	}
	if r.PromptFeedback != nil {
		return r.PromptFeedback.BlockReason
	}
	return ""
}

// usage is the token usage, nil when the response reported none. Output counts the
// thinking tokens too.
func (r *responseBody) usage() *kernel.TokenUsage {
	if r.UsageMetadata == nil {
		return nil
	}
	u := r.UsageMetadata
	return &kernel.TokenUsage{InputTokens: u.PromptTokenCount, OutputTokens: u.CandidatesTokenCount + u.ThoughtsTokenCount}
}

// toolCall maps a function call part. The Gemini API does not always give an id;
// the SDK needs one to pair the result, so a missing id is made up. Arguments go
// through llmhub.ParseToolArguments, so arguments that are not one object give a
// call with an ArgumentsError instead of failing the response.
func toolCall(p responsePart) kernel.ToolCall {
	fc := p.FunctionCall
	args, argsErr := llmhub.ParseToolArguments(string(fc.Args))
	id := fc.ID
	if id == "" {
		id = newCallID()
	}
	return kernel.ToolCall{ID: id, Name: fc.Name, Arguments: args, ArgumentsError: argsErr, Signature: p.ThoughtSignature}
}

// newCallID is a random "call_<32 hex digits>" id.
func newCallID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return "call_" + hex.EncodeToString(b[:])
}

// mapStopReason maps a finish reason: STOP (or none) is a normal finish, tool_use
// when the response called a function; MAX_TOKENS is max_tokens; every other
// reason (SAFETY, RECITATION, a blocked prompt, a malformed function call, ...) is
// error.
func mapStopReason(reason string, calledTool bool) kernel.StopReason {
	switch reason {
	case "STOP", "":
		if calledTool {
			return kernel.StopToolUse
		}
		return kernel.StopEndTurn
	case "MAX_TOKENS":
		return kernel.StopMaxTokens
	default:
		return kernel.StopError
	}
}
