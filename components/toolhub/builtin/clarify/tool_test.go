package clarify_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/Phong95/aec-agent-go/aecmock"
	"github.com/Phong95/aec-agent-go/components/toolhub"
	"github.com/Phong95/aec-agent-go/components/toolhub/builtin/clarify"
	"github.com/Phong95/aec-agent-go/conformance"
	"github.com/Phong95/aec-agent-go/kernel"
)

// run executes the clarify tool with the given JSON arguments over a context with session
// s1 and agent a1 (the base context of the fixtures) and the interaction handler, which may be nil.
func run(t *testing.T, args string, interaction toolhub.InteractionHandler) toolhub.Output {
	t.Helper()
	a, err := kernel.NewArguments([]byte(args))
	if err != nil {
		t.Fatalf("arguments %s: %v", args, err)
	}
	out, err := clarify.NewTool().Execute(t.Context(), a, toolhub.Context{
		SessionID:   "s1",
		AgentID:     "a1",
		Emit:        func(string, any) {},
		Interaction: interaction,
	})
	if err != nil {
		t.Fatalf("Execute returned an error: %v", err)
	}
	return out
}

func isError(o toolhub.Output) bool { return o.IsError != nil && *o.IsError }

func notRetryable(o toolhub.Output) bool { return o.Retryable != nil && !*o.Retryable }

// TestClarifyTool holds the four ClarifyTool unit cases of the interaction-control
// suite under their original titles.
func TestClarifyTool(t *testing.T) {
	t.Run("asks the user through the interaction handler and returns the answer", func(t *testing.T) {
		interaction := aecmock.NewScriptedInteraction()
		out := run(t, `{"question":" Which tower? ","choices":["Tower A","Tower B","Tower A","",3,"C","D","E"]}`, interaction)

		asked := interaction.Asked()
		if len(asked) != 1 {
			t.Fatalf("AskUser was called %d times, want once", len(asked))
		}
		want := toolhub.ClarifyRequest{SessionID: "s1", AgentID: "a1", Question: "Which tower?", Choices: []string{"Tower A", "Tower B", "C", "D"}}
		if got := asked[0]; got.SessionID != want.SessionID || got.AgentID != want.AgentID || got.Question != want.Question || !slices.Equal(got.Choices, want.Choices) {
			t.Errorf("request = %+v, want %+v", got, want)
		}
		if out.Content != "User's answer: Tower A" {
			t.Errorf("content = %q, want %q", out.Content, "User's answer: Tower A")
		}
		if isError(out) {
			t.Error("the output is an error")
		}
	})

	t.Run("fails without retry when no interactive user is connected", func(t *testing.T) {
		out := run(t, `{"question":"Which tower?"}`, nil)
		if !isError(out) || !notRetryable(out) {
			t.Errorf("output = %+v, want an error that is not retryable", out)
		}
		if !strings.Contains(out.Content, "no interactive user") {
			t.Errorf("content = %q, want it to mention no interactive user", out.Content)
		}
	})

	t.Run("fails without retry when the handler throws", func(t *testing.T) {
		interaction := aecmock.NewScriptedInteraction(aecmock.WithAnswerFunc(func(context.Context, toolhub.ClarifyRequest) (toolhub.ClarifyResponse, error) {
			return toolhub.ClarifyResponse{}, errors.New("user left")
		}))
		out := run(t, `{"question":"Which tower?"}`, interaction)
		if !isError(out) || !notRetryable(out) {
			t.Errorf("output = %+v, want an error that is not retryable", out)
		}
		if !strings.Contains(out.Content, "user left") {
			t.Errorf("content = %q, want it to carry the handler message", out.Content)
		}
	})

	t.Run("handles an empty answer and an empty question", func(t *testing.T) {
		interaction := aecmock.NewScriptedInteraction(aecmock.WithAnswerFunc(func(context.Context, toolhub.ClarifyRequest) (toolhub.ClarifyResponse, error) {
			return toolhub.ClarifyResponse{Answer: "  "}, nil
		}))
		empty := run(t, `{"question":"Which tower?"}`, interaction)
		if isError(empty) {
			t.Error("an empty answer is not an error")
		}
		if !strings.Contains(empty.Content, "did not answer") {
			t.Errorf("content = %q, want it to say the user did not answer", empty.Content)
		}
		blank := run(t, `{"question":""}`, nil)
		if !isError(blank) || !notRetryable(blank) {
			t.Errorf("empty question output = %+v, want an error that is not retryable", blank)
		}
	})
}

// TestClarify_NilAndTypedNilHandler: a typed nil InteractionHandler is no handler. Calling
// it would panic, so the tool must report the unavailable text instead.
func TestClarify_NilAndTypedNilHandler(t *testing.T) {
	var typedNil *aecmock.ScriptedInteraction
	for name, h := range map[string]toolhub.InteractionHandler{"nil": nil, "typed nil": typedNil} {
		t.Run(name, func(t *testing.T) {
			out := run(t, `{"question":"Which tower?"}`, h)
			if !isError(out) || !notRetryable(out) || !strings.HasPrefix(out.Content, "clarify is unavailable:") {
				t.Errorf("output = %+v, want the unavailable text as a non-retryable error", out)
			}
		})
	}
}

// TestClarify_CtxErrorIsNonRetryable: a handler that waits for the run to stop returns
// the ctx error, and the tool reports it as a failed clarify that the harness must not
// retry (CONTEXT "Interaction handler"). An abort reason from the handler lands in the
// same branch.
func TestClarify_CtxErrorIsNonRetryable(t *testing.T) {
	interaction := aecmock.NewScriptedInteraction(aecmock.WithAnswerFunc(func(ctx context.Context, _ toolhub.ClarifyRequest) (toolhub.ClarifyResponse, error) {
		<-ctx.Done()
		return toolhub.ClarifyResponse{}, ctx.Err()
	}))
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	args, err := kernel.NewArguments([]byte(`{"question":"Which tower?"}`))
	if err != nil {
		t.Fatal(err)
	}
	out, err := clarify.NewTool().Execute(ctx, args, toolhub.Context{SessionID: "s1", AgentID: "a1", Interaction: interaction})
	if err != nil {
		t.Fatalf("Execute returned an error: %v", err)
	}
	if !isError(out) || !notRetryable(out) {
		t.Errorf("output = %+v, want an error that is not retryable", out)
	}
	want := "clarify failed: context canceled. Do not ask again in this turn; proceed with a stated assumption or ask in your reply."
	if out.Content != want {
		t.Errorf("content = %q, want %q", out.Content, want)
	}
}

// TestClarify_RequestKeepsContextIds: the ids of the request come from the tool context,
// never from the model's arguments (T-06-24).
func TestClarify_RequestKeepsContextIds(t *testing.T) {
	interaction := aecmock.NewScriptedInteraction()
	run(t, `{"question":"Q","sessionId":"evil","agentId":"evil"}`, interaction)
	asked := interaction.Asked()
	if len(asked) != 1 || asked[0].SessionID != "s1" || asked[0].AgentID != "a1" {
		t.Fatalf("requests = %+v, want the ids of the tool context", asked)
	}
}

// withInteraction decorates a handler so the conformance suite, which builds its own tool
// context with no interaction handler, also reaches the tool's handler branch.
type withInteraction struct {
	toolhub.Handler
	interaction toolhub.InteractionHandler
}

func (w withInteraction) Execute(ctx context.Context, args kernel.Arguments, tc toolhub.Context) (toolhub.Output, error) {
	tc.Interaction = w.interaction
	return w.Handler.Execute(ctx, args, tc)
}

// TestClarifyConformance runs the tool-handler conformance suite on clarify twice: with no
// interaction handler (the unavailable path) and with a scripted one (the answer path).
func TestClarifyConformance(t *testing.T) {
	args, err := kernel.NewArguments([]byte(`{"question":"Which tower?","choices":["Tower A","Tower B"]}`))
	if err != nil {
		t.Fatal(err)
	}
	t.Run("without a handler", func(t *testing.T) {
		conformance.ToolHandler(t, func(*testing.T) toolhub.Handler { return clarify.NewTool() }, args)
	})
	t.Run("with a handler", func(t *testing.T) {
		conformance.ToolHandler(t, func(*testing.T) toolhub.Handler {
			return withInteraction{Handler: clarify.NewTool(), interaction: aecmock.NewScriptedInteraction()}
		}, args)
	})
}
