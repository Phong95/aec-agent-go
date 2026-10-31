package llmhub

import "regexp"

// A-178: the thinking text of Claude models, on the Anthropic API and on Bedrock.
//
// The Claude 5 family (and Fable, Mythos) thinks on every request, but returns the
// thinking blocks with empty text unless the request sets thinking.display
// "summarized". On those models the setting only makes the text readable: the model
// thinks the same way without it. Older models are left alone: there the setting
// would turn thinking on, and every tool-use turn would then have to send the
// thinking blocks back, which the providers do not do (they never send reasoning back
// to a model). The same rule as core/ts claude-thinking.ts.

// ClaudeThinkingDisplay is the thinking setting sent to a Claude model that thinks by
// default: {"type":"adaptive","display":"summarized"}.
type ClaudeThinkingDisplay struct {
	Type    string `json:"type"`
	Display string `json:"display"`
}

// claudeThinksByDefault matches "claude-<family>-<major 5 or later>", then the end of
// the id or a separator ("-5", "-20260101", "-v1:0", "@20260101"), in a bare id and
// in a Bedrock id ("us.anthropic.claude-opus-5-5-v1:0").
var claudeThinksByDefault = regexp.MustCompile(`(?i)(?:^|[./])claude-(?:opus|sonnet|haiku|fable|mythos)-[5-9](?:$|[-.@:])`)

// ClaudeThinksByDefault reports whether a Claude model id (bare or Bedrock) names a
// model that thinks by default.
func ClaudeThinksByDefault(model string) bool {
	return claudeThinksByDefault.MatchString(model)
}

// ClaudeThinking is the thinking setting for a request to model: the summarized
// display for a model that thinks by default, nil for every other model.
func ClaudeThinking(model string) *ClaudeThinkingDisplay {
	if !ClaudeThinksByDefault(model) {
		return nil
	}
	return &ClaudeThinkingDisplay{Type: "adaptive", Display: "summarized"}
}
