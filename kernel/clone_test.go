package kernel_test

import (
	"encoding/json"
	"testing"

	"github.com/Phong95/aec-agent-go/internal/clonecheck"
	"github.com/Phong95/aec-agent-go/kernel"
	"github.com/Phong95/aec-agent-go/kernel/ordered"
)

// Session.Clone must share nothing with the original: a pointer or map field
// added to Session without a Clone line fails here (RESEARCH Pitfall 12).
func TestSessionCloneIsDeep(t *testing.T) {
	nested := ordered.NewObject()
	nested.Set("k", json.Number("1"))
	meta := ordered.NewObject()
	meta.Set("nested", nested)
	meta.Set("list", []any{"a", nested})
	meta.Set("n", json.Number("2"))
	three, ten, twenty := 3, 10, 20
	sample := kernel.Session{
		ID:                       "s",
		Metadata:                 meta,
		CreatedAt:                1,
		UpdatedAt:                2,
		LLMModel:                 "m",
		ContextWindowTokens:      500_000,
		CurrentTokenUsage:        &kernel.TokenUsage{InputTokens: 1, OutputTokens: 2, CacheReadTokens: &ten, CacheWriteTokens: &twenty},
		CounterCalibration:       &kernel.CounterCalibration{CharsPerToken: 3.5, Samples: 1},
		ConsolidatedMessageCount: &three,
		JobID:                    "job-1",
		ApprovalMode:             kernel.ApprovalModeYolo,
		ReasoningEffort:          "high",
	}
	clonecheck.AssertDeep(t, sample, kernel.Session.Clone)
}

// Message.Clone must share nothing with the original: the tool-call argument
// bytes, the slices and the IsError and Link pointers are all copied.
func TestMessageCloneIsDeep(t *testing.T) {
	isErr := true
	cacheRead, cacheWrite := 3, 4
	sample := kernel.Message{
		Role:        kernel.RoleAssistant,
		Content:     "c",
		Attachments: []kernel.MessageAttachment{{Type: kernel.AttachmentTypeImage, MIMEType: "image/png", Data: "AAAA", Name: "a.png"}},
		Timestamp:   "2026-10-01T08:00:00.000Z",
		ToolCalls: []kernel.ToolCall{
			{ID: "call_1", Name: "read_file", Arguments: kernel.Arguments(`{"b":1,"a":2}`), ArgumentsError: "the arguments must be one JSON object: []", Signature: "sig-1"},
		},
		ToolResults: []kernel.ToolResult{
			{ToolCallID: "call_1", Content: "boom", IsError: &isErr, Link: &kernel.ToolResultLink{URI: "artifact://art-1", Name: "a.md", Type: "text/markdown", Version: 2}, CompactArguments: kernel.Arguments(`{"content":"[ref]"}`)},
		},
		Usage:   &kernel.TokenUsage{InputTokens: 10, OutputTokens: 5, CacheReadTokens: &cacheRead, CacheWriteTokens: &cacheWrite},
		Summary: true,
	}
	clonecheck.AssertDeep(t, sample, kernel.Message.Clone)
}

// Nil stays nil and an empty slice stays empty, so the omitzero encoding of a
// cloned message is unchanged.
func TestMessageCloneKeepsNilVersusEmpty(t *testing.T) {
	nilCalls := kernel.Message{Role: kernel.RoleUser, Content: "x"}.Clone()
	if nilCalls.ToolCalls != nil || nilCalls.ToolResults != nil {
		t.Fatalf("nil slices must stay nil: %#v", nilCalls)
	}
	empty := kernel.Message{Role: kernel.RoleAssistant, ToolCalls: []kernel.ToolCall{}, ToolResults: []kernel.ToolResult{}}.Clone()
	if empty.ToolCalls == nil || empty.ToolResults == nil {
		t.Fatalf("empty slices must stay non-nil: %#v", empty)
	}
}
