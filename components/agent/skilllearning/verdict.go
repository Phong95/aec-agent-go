package skilllearning

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"strings"

	"github.com/Phong95/aec-agent-go/components/llmhub"
	"github.com/Phong95/aec-agent-go/kernel"
)

// ---------------------------------------------------------------------------
// Verdict - did the user accept the hard-won turn? Read from the user's next message:
// a cheap rule first, and one small model call only when the rule can't tell.
// A correction ("no, the totals are wrong") is never learned from.
// ---------------------------------------------------------------------------

type replyVerdict string

const (
	verdictAccept  replyVerdict = "accept"
	verdictCorrect replyVerdict = "correct"
	verdictUnclear replyVerdict = "unclear"
)

// Kept ASCII and identical to core/ts (skill-learning/components/verdict.ts); (?i) is
// the JS "i" flag.
var (
	correctionPattern = regexp.MustCompile(`(?i)\b(no|nope|wrong|incorrect|not (?:right|correct|what|working|quite)|doesn'?t work|didn'?t work|isn'?t working|not working|broken|fails?|failed|failing|error|errors|bug|still|redo|try again|undo|revert|missing)\b`)
	acceptPattern     = regexp.MustCompile(`(?i)\b(thanks|thank you|thx|ty|great|perfect|awesome|nice|good job|well done|works|worked|working now|looks good|lgtm|excellent|exactly|cool|brilliant|amazing|love it|that'?s it)\b`)
)

// classifyReply is the rule: one clear signal one way or the other; both or neither is
// "unclear".
func classifyReply(text string) replyVerdict {
	t := strings.TrimSpace(text)
	if t == "" {
		return verdictUnclear
	}
	correct := correctionPattern.MatchString(t)
	accept := acceptPattern.MatchString(t)
	switch {
	case correct && !accept:
		return verdictCorrect
	case accept && !correct:
		return verdictAccept
	default:
		return verdictUnclear
	}
}

// VerdictSystemPrompt is the system prompt of the model judge of a reply (byte-identical
// to core/ts VERDICT_SYSTEM_PROMPT).
const VerdictSystemPrompt = `You judge whether a user accepted the result of an AI agent's task.

You receive the agent's final reply for the task and the user's next message.

Answer "accept" when the user is satisfied or simply moves on to a different request.
Answer "correct" when the user says the result is wrong, incomplete or broken, asks to fix or redo it, or repeats the same request.
Answer "unclear" when you cannot tell.

Respond with ONLY a JSON object (no markdown fences): {"verdict": "accept" | "correct" | "unclear"}`

// The most characters of the agent's reply and of the user's message the judge sees.
const (
	maxReplyChars = 2000
	maxUserChars  = 2000
)

// Model is the model access of the learner (the agent's ModelManager).
type Model interface {
	Chat(ctx context.Context, msgs []kernel.Message, opts llmhub.ModelOptions) (kernel.ModelResponse, error)
}

func intPtr(n int) *int { return &n }

// judgeReply is the model judge, for a reply the rule could not classify. It fails on a
// malformed answer.
func judgeReply(ctx context.Context, m Model, agentReply, userMessage string) (replyVerdict, error) {
	res, err := m.Chat(ctx, []kernel.Message{
		{Role: kernel.RoleSystem, Content: VerdictSystemPrompt},
		{Role: kernel.RoleUser, Content: "AGENT REPLY:\n" + tail(agentReply, maxReplyChars) + "\n\nUSER MESSAGE:\n" + head(userMessage, maxUserChars)},
	}, llmhub.ModelOptions{MaxTokens: intPtr(4096)}) // reasoning models spend output tokens before they answer
	if err != nil {
		return verdictUnclear, err
	}
	if res.StopReason == kernel.StopMaxTokens {
		return verdictUnclear, errors.New("verdict call truncated")
	}
	parsed, err := parseJSONObject(res.Message.Content, "verdict")
	if err != nil {
		return verdictUnclear, err
	}
	switch parsed["verdict"] {
	case "accept":
		return verdictAccept, nil
	case "correct":
		return verdictCorrect, nil
	default:
		return verdictUnclear, nil
	}
}

var (
	fenceStart = regexp.MustCompile("(?i)^```(?:json)?\\s*")
	fenceEnd   = regexp.MustCompile("\\s*```$")
)

// parseJSONObject parses a model's JSON object answer (fences stripped; a {...} wrapped
// in prose is taken out). It fails on empty or non-object output.
func parseJSONObject(content, what string) (map[string]any, error) {
	text := strings.TrimSpace(content)
	text = strings.TrimSpace(fenceEnd.ReplaceAllString(fenceStart.ReplaceAllString(text, ""), ""))
	if text == "" {
		return nil, errors.New(what + " returned empty content")
	}
	if !strings.HasPrefix(text, "{") {
		start, end := strings.Index(text, "{"), strings.LastIndex(text, "}")
		if start < 0 || end <= start {
			return nil, errors.New(what + " did not return a JSON object")
		}
		text = text[start : end+1]
	}
	var parsed any
	if err := json.Unmarshal([]byte(text), &parsed); err != nil {
		return nil, err
	}
	obj, ok := parsed.(map[string]any)
	if !ok {
		return nil, errors.New(what + " did not return a JSON object")
	}
	return obj, nil
}

// head returns the first n runes of s; tail the last n.
func head(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

func tail(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[len(r)-n:])
}

// unmarshal decodes JSON bytes into v.
func unmarshal(data []byte, v any) error { return json.Unmarshal(data, v) }

// ParseJSONObject parses a model's JSON object answer (fences stripped; a {...} wrapped
// in prose is taken out). It fails on empty or non-object output; what names the call in
// the error.
func ParseJSONObject(content, what string) (map[string]any, error) {
	return parseJSONObject(content, what)
}
