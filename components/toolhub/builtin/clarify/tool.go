package clarify

import (
	"context"
	"slices"

	"github.com/Phong95/aec-agent-go/components/toolhub"
	"github.com/Phong95/aec-agent-go/internal/jsstr"
	"github.com/Phong95/aec-agent-go/internal/nilcheck"
	"github.com/Phong95/aec-agent-go/internal/toolkit"
	"github.com/Phong95/aec-agent-go/kernel"
	"github.com/Phong95/aec-agent-go/kernel/ordered"
)

// The limits of the tool. The two character limits count UTF-16 code units, as the
// JS String length does.
const (
	maxChoices       = 4
	maxQuestionChars = 1000
	maxChoiceChars   = 200
)

// Tool is clarify: it asks the user a clarifying question through the
// host's InteractionHandler and waits for the answer. It is for genuine ambiguity only
// (which project, which option, a missing required detail), not for the confirmation of
// a consequential action, which the harness gates itself through the tool risk.
//
// A failure is never retried by the harness: asking again would show the user the
// question twice. Every failure is therefore an output with retryable false.
//
// A Tool holds no state, so it is safe for concurrent use. Build one with NewTool.
type Tool struct{}

var _ toolhub.Handler = (*Tool)(nil)

// NewTool returns the clarify tool.
func NewTool() *Tool { return &Tool{} }

// Name returns the plugin name.
func (*Tool) Name() string { return "tool:clarify" }

// Version returns the plugin version.
func (*Tool) Version() string { return "1.0.0" }

// Definition returns the tool definition (a fresh value on every call): a read-only
// tool with a required question and optional choices.
func (*Tool) Definition() toolhub.Definition {
	return toolhub.Definition{
		Name: "clarify",
		Description: "Ask the user a clarifying question and wait for their answer. Use only when the request is " +
			"genuinely ambiguous or missing information you cannot find with other tools, and a wrong guess " +
			"would waste work. Ask one focused question; optionally offer up to 4 short choices " +
			"(the user may still answer in their own words).",
		Parameters: toolhub.JSONSchema{
			Type: "object",
			Properties: toolhub.NewProperties(
				toolhub.Property{Name: "question", Schema: toolhub.JSONSchema{Type: "string", Description: "The question to ask the user"}},
				toolhub.Property{Name: "choices", Schema: toolhub.JSONSchema{
					Type:        "array",
					Items:       &toolhub.JSONSchema{Type: "string"},
					Description: "Optional answer options (max 4)",
				}},
			),
			Required: []string{"question"},
		},
		Risk: toolhub.RiskReadOnly,
		// A-137: a subagent cannot ask the user.
		Subagent: toolhub.SubagentNever,
	}
}

// Execute asks the user. The question is trimmed with the JS trim; the choices are the
// string elements of the argument, each trimmed and cut to 200 UTF-16 units, with the
// empty ones dropped, the duplicates removed in first-seen order and at most four kept.
//
// The session and agent ids of the request come from the tool context, never from the
// model's arguments (T-06-24). Execute never returns an error: every failure is an
// error output, a failing or cancelled handler included, so the model reads it and does
// not ask again.
func (*Tool) Execute(ctx context.Context, args kernel.Arguments, tc toolhub.Context) (toolhub.Output, error) {
	a := toolkit.DecodeArgs(args)
	raw, _ := a.String("question")
	question := jsstr.Trim(raw)
	if question == "" {
		return toolkit.Fail(`clarify: "question" must be a non-empty string`), nil
	}
	if kernel.UTF16Len(question) > maxQuestionChars {
		return toolkit.Fail("clarify: question is too long (max 1000 characters)"), nil
	}

	choices := cleanChoices(a.Strings("choices"))

	if nilcheck.IsNil(tc.Interaction) {
		return toolkit.Fail("clarify is unavailable: no interactive user is connected to this session. " +
			"Proceed with the most reasonable assumption and state it explicitly in your answer, " +
			"or end your turn by asking the question in your reply."), nil
	}

	req := toolhub.ClarifyRequest{SessionID: tc.SessionID, AgentID: tc.AgentID, Question: question}
	if len(choices) > 0 {
		// Choices is omitted from the request when there are none.
		req.Choices = slices.Clone(choices)
	}
	resp, err := tc.Interaction.AskUser(ctx, req)
	if err != nil {
		//nolint:nilerr // a failing or cancelled handler is a tool-level error output the model reads, never a returned error
		return toolkit.Fail("clarify failed: " + err.Error() + ". " +
			"Do not ask again in this turn; proceed with a stated assumption or ask in your reply."), nil
	}

	answer := jsstr.Trim(resp.Answer)
	meta := ordered.NewObject()
	meta.Set("question", question)
	// The possibly empty list, never null: metadata choices is always an array.
	meta.Set("choices", slices.Clone(choices))
	if answer == "" {
		meta.Set("answered", false)
		return toolkit.Result("The user did not answer. Proceed with the most reasonable assumption and state it explicitly.", meta), nil
	}
	meta.Set("answered", true)
	meta.Set("answer", answer)
	return toolkit.Result("User's answer: "+answer, meta), nil
}

// cleanChoices is the choice cleanup: strings only (the caller already dropped the
// rest), each trimmed and cut to 200 UTF-16 units, empty ones dropped, duplicates
// removed keeping the first, and the first four kept. It never returns nil.
func cleanChoices(raw []string) []string {
	out := make([]string, 0, maxChoices)
	for _, c := range raw {
		c = jsstr.TruncateUTF16(jsstr.Trim(c), maxChoiceChars)
		if c == "" || slices.Contains(out, c) {
			continue
		}
		out = append(out, c)
		if len(out) == maxChoices {
			break
		}
	}
	return out
}
