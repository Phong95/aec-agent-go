package agent_test

import (
	"errors"
	"testing"

	"github.com/Phong95/aec-agent-go/aecmock"
	"github.com/Phong95/aec-agent-go/components/agent"
	"github.com/Phong95/aec-agent-go/components/llmhub"
	"github.com/Phong95/aec-agent-go/components/toolhub"
	"github.com/Phong95/aec-agent-go/kernel"
	"github.com/Phong95/aec-agent-go/kernel/ordered"
)

// The functional options of Chat, ChatStream, Complete and StartSession.

func TestChatOptions(t *testing.T) {
	t.Run("the model options of the call reach the provider", func(t *testing.T) {
		a, e := newTestAgent(t, testProfile("a"))
		if _, err := a.StartSession(t.Context()); err != nil {
			t.Fatal(err)
		}
		if _, err := a.Chat(t.Context(), "hello",
			agent.WithModel("m1"), agent.WithTemperature(0), agent.WithMaxTokens(64),
			agent.WithTopP(0.5), agent.WithStopSequences("END"), nil); err != nil {
			t.Fatalf("Chat: %v", err)
		}
		calls := e.mocks["primary"].Calls()
		got := calls[len(calls)-1].Options
		if got.Model != "m1" || got.Temperature == nil || *got.Temperature != 0 ||
			got.MaxTokens == nil || *got.MaxTokens != 64 || got.TopP == nil || *got.TopP != 0.5 ||
			len(got.StopSequences) != 1 || got.StopSequences[0] != "END" {
			t.Fatalf("options = %+v, want the overrides of the call", got)
		}
	})

	t.Run("WithProvider sends the call to the named provider", func(t *testing.T) {
		e := newEnv(t, "primary", "other")
		a := e.spawn(t, testProfile("a"))
		if _, err := a.StartSession(t.Context()); err != nil {
			t.Fatal(err)
		}
		res, err := a.Chat(t.Context(), "hello", agent.WithProvider("other"))
		if err != nil {
			t.Fatalf("Chat: %v", err)
		}
		if res.Message.Content != "reply from other" {
			t.Fatalf("reply = %q, want reply from other", res.Message.Content)
		}
	})

	t.Run("WithModelOptions replaces the earlier options and the later ones apply on top", func(t *testing.T) {
		a, e := newTestAgent(t, testProfile("a"))
		if _, err := a.StartSession(t.Context()); err != nil {
			t.Fatal(err)
		}
		raw := llmhub.ModelOptions{Model: "raw", Tools: []toolhub.Definition{}}
		if _, err := a.Chat(t.Context(), "hello",
			agent.WithTemperature(0.9), agent.WithModelOptions(raw), agent.WithMaxTokens(8)); err != nil {
			t.Fatalf("Chat: %v", err)
		}
		calls := e.mocks["primary"].Calls()
		got := calls[len(calls)-1].Options
		if got.Model != "raw" || got.Temperature != nil || got.MaxTokens == nil || *got.MaxTokens != 8 {
			t.Fatalf("options = %+v, want the raw options with the later max tokens", got)
		}
		if got.Tools == nil || len(got.Tools) != 0 {
			t.Fatalf("tools = %v, want the explicit empty list of the raw options", got.Tools)
		}
	})

	t.Run("StartSession stores the metadata and the model of the options", func(t *testing.T) {
		a, e := newTestAgent(t, testProfile("a"))
		var md ordered.Object
		md.Set("owner", "site-7")
		s, err := a.StartSession(t.Context(), agent.WithMetadata(md), agent.WithSessionModel("session-model"))
		if err != nil {
			t.Fatal(err)
		}
		if s.LLMModel != "session-model" {
			t.Fatalf("LLMModel = %q, want session-model", s.LLMModel)
		}
		if v, ok := s.Metadata.Get("owner"); !ok || v != "site-7" {
			t.Fatalf("metadata owner = %v, %v, want site-7", v, ok)
		}
		chat(t, a, "hello")
		calls := e.mocks["primary"].Calls()
		if got := calls[len(calls)-1].Options.Model; got != "session-model" {
			t.Fatalf("model = %q, want the session model", got)
		}
	})
}

// failingAgent is an agent whose provider fails every call in band.
func failingAgent(t *testing.T) (*agent.Agent, *env) {
	t.Helper()
	e := newEnv(t)
	failing := aecmock.NewMockProvider("failing", nil,
		aecmock.WithFailure(kernel.NewModelError("the model is overloaded", kernel.ModelCodeProvider, true)))
	if err := e.llm.Register(failing); err != nil {
		t.Fatal(err)
	}
	p := testProfile("a")
	p.Provider = "failing"
	a := e.spawn(t, p)
	return a, e
}

func TestChatStreamModelFailure(t *testing.T) {
	for _, tc := range []struct {
		name string
		stop bool
	}{
		{"the consumer goes on after the error", false},
		{"the consumer stops at the error", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, e := failingAgent(t)
			session, err := a.StartSession(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			var pairs int
			var got kernel.StreamChunk
			var gotErr error
			for chunk, err := range a.ChatStream(t.Context(), "hello") {
				pairs++
				if err != nil {
					got, gotErr = chunk, err
					if tc.stop {
						break
					}
				}
			}
			if got.Type != kernel.ChunkError || got.Error != "the model is overloaded" {
				t.Fatalf("error pair chunk = %+v, want the provider's error chunk", got)
			}
			var sdkErr *kernel.SDKError
			if !errors.As(gotErr, &sdkErr) || sdkErr.Message != "the model is overloaded" {
				t.Fatalf("error = %v, want a *kernel.SDKError with the chunk text", gotErr)
			}
			if pairs != 1 {
				t.Fatalf("%d pairs, want only the error pair", pairs)
			}
			// The turn was finished, not cleaned up as aborted: the (empty) reply is saved
			// after the user message.
			msgs, err := e.hub.Messages.Get(t.Context(), session.ID)
			if err != nil {
				t.Fatal(err)
			}
			if len(msgs) != 2 || msgs[1].Role != kernel.RoleAssistant {
				t.Fatalf("stored messages = %+v, want the user message and the reply", msgs)
			}
			// The agent is free for the next turn.
			for _, err := range a.ChatStream(t.Context(), "again") {
				if err == nil {
					continue
				}
				if !errors.As(err, &sdkErr) {
					t.Fatalf("second turn error = %v, want the model failure again", err)
				}
			}
		})
	}
}
