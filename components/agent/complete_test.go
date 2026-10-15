package agent_test

import (
	"reflect"
	"testing"

	"github.com/Phong95/aec-agent-go/components/agent"
	"github.com/Phong95/aec-agent-go/components/agent/profile"
	"github.com/Phong95/aec-agent-go/kernel"
)

// Agent.Complete is one model call with the agent's provider and model, outside the
// agent loop. The four subtests carry the titles of the shared agent-complete cases.

func completePrompt() []kernel.Message {
	return []kernel.Message{{Role: kernel.RoleUser, Content: "Title this conversation."}}
}

func TestComplete(t *testing.T) {
	// The title has an em dash after "messages"; the source stays ASCII, so it is an escape.
	t.Run("sends exactly the given messages \u2014 no system prompt, tools or history", func(t *testing.T) {
		a, e := newTestAgent(t, testProfile("a"))
		if _, err := a.StartSession(t.Context()); err != nil {
			t.Fatal(err)
		}
		chat(t, a, "hello")
		primary := e.mocks["primary"]
		before := len(primary.Calls())

		resp, err := a.Complete(t.Context(), completePrompt())
		if err != nil {
			t.Fatalf("Complete: %v", err)
		}
		if resp.Message.Content != "reply from primary" {
			t.Fatalf("reply = %q, want reply from primary", resp.Message.Content)
		}
		calls := primary.Calls()
		if len(calls) != before+1 {
			t.Fatalf("provider calls = %d, want %d", len(calls), before+1)
		}
		got := calls[len(calls)-1]
		if !reflect.DeepEqual(got.Messages, completePrompt()) {
			t.Fatalf("messages = %+v, want exactly the prompt", got.Messages)
		}
		if got.Options.Tools != nil {
			t.Fatalf("options.Tools = %v, want none", got.Options.Tools)
		}
	})

	t.Run("does not add anything to the session", func(t *testing.T) {
		a, e := newTestAgent(t, testProfile("a"))
		session, err := a.StartSession(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		chat(t, a, "hello")
		before, err := e.hub.Messages.Get(t.Context(), session.ID)
		if err != nil {
			t.Fatal(err)
		}

		if _, err := a.Complete(t.Context(), completePrompt(), agent.WithCompleteSession(session)); err != nil {
			t.Fatalf("Complete: %v", err)
		}

		after, err := e.hub.Messages.Get(t.Context(), session.ID)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(before, after) {
			t.Fatalf("session messages changed:\nbefore %+v\nafter  %+v", before, after)
		}
		if len(after) != 2 {
			t.Fatalf("the session holds %d messages, want the 2 of the chat", len(after))
		}
	})

	t.Run("uses the agent's provider, model and inference defaults", func(t *testing.T) {
		temperature := 0.3
		p := testProfile("a")
		p.Provider = "other"
		p.LLMModel = "agent-model"
		p.InferenceConfig = &profile.InferenceConfig{Temperature: &temperature}
		e := newEnv(t, "primary", "other")
		a := e.spawn(t, p)

		maxTokens := 50
		if _, err := a.Complete(t.Context(), completePrompt(), agent.WithMaxTokens(maxTokens)); err != nil {
			t.Fatalf("Complete: %v", err)
		}

		if n := len(e.mocks["primary"].Calls()); n != 0 {
			t.Fatalf("the primary provider was called %d times, want 0", n)
		}
		calls := e.mocks["other"].Calls()
		if len(calls) != 1 {
			t.Fatalf("other provider calls = %d, want 1", len(calls))
		}
		o := calls[0].Options
		if o.Model != "agent-model" || o.Temperature == nil || *o.Temperature != 0.3 || o.MaxTokens == nil || *o.MaxTokens != 50 {
			t.Fatalf("options = %+v, want model agent-model, temperature 0.3, maxTokens 50", o)
		}
	})

	t.Run("applies the session's model override, and an explicit model wins over both", func(t *testing.T) {
		p := testProfile("a")
		p.LLMModel = "agent-model"
		a, e := newTestAgent(t, p)
		session, err := a.StartSession(t.Context(), agent.WithSessionModel("session-model"))
		if err != nil {
			t.Fatal(err)
		}

		if _, err := a.Complete(t.Context(), completePrompt(), agent.WithCompleteSession(session)); err != nil {
			t.Fatal(err)
		}
		if _, err := a.Complete(t.Context(), completePrompt(), agent.WithModel("explicit-model"), agent.WithCompleteSession(session)); err != nil {
			t.Fatal(err)
		}
		if _, err := a.Complete(t.Context(), completePrompt()); err != nil {
			t.Fatal(err)
		}

		var models []string
		for _, c := range e.mocks["primary"].Calls() {
			models = append(models, c.Options.Model)
		}
		want := []string{"session-model", "explicit-model", "agent-model"}
		if !reflect.DeepEqual(models, want) {
			t.Fatalf("models = %v, want %v", models, want)
		}
	})
}
