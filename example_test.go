package aec_test

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"

	aec "github.com/Phong95/aec-agent-go"
	"github.com/Phong95/aec-agent-go/aecmock"
	"github.com/Phong95/aec-agent-go/components/agent"
	"github.com/Phong95/aec-agent-go/components/agent/profile"
	"github.com/Phong95/aec-agent-go/components/llmhub"
	"github.com/Phong95/aec-agent-go/components/toolhub"
	"github.com/Phong95/aec-agent-go/kernel"
)

// The examples run offline: a scripted provider stands in for a real model. A real
// program blank-imports the providers instead and names one in AgentConfig:
//
//	import _ "github.com/Phong95/aec-agent-go/components/llmhub/providers/all"
//
//	a, err := aec.CreateAgent(ctx, aec.AgentConfig{Provider: "anthropic", APIKey: key, Model: "claude-sonnet-4-20250514"})

// scriptedRegistry returns a provider registry whose provider "scripted" answers with
// the given turns, in order, whatever the configuration.
func scriptedRegistry(turns ...aecmock.ScriptedTurn) *llmhub.Registry {
	provider := aecmock.NewScriptedProvider("scripted", turns)
	reg := llmhub.NewRegistry()
	reg.Register("scripted", func(llmhub.ProviderConfig) (llmhub.Provider, error) { return provider, nil })
	return reg
}

// mustDispose disposes the agent; Dispose is mandatory, because the memory system
// runs a goroutine under the agent's lifetime.
func mustDispose(ctx context.Context, a *agent.Agent) {
	if err := a.Dispose(ctx); err != nil {
		log.Fatal(err)
	}
}

// CreateAgent builds a working agent in one call. Start a session, then chat.
func ExampleCreateAgent() {
	ctx := context.Background()
	reg := scriptedRegistry(aecmock.ScriptedTurn{Content: "Hello! How can I help with your project?"})

	a, err := aec.CreateAgent(ctx, aec.AgentConfig{
		Provider:     "scripted",
		APIKey:       "key",
		Model:        "model",
		SystemPrompt: "You are a helpful site assistant.",
	}, aec.WithProviderRegistry(reg))
	if err != nil {
		log.Fatal(err)
	}
	defer mustDispose(ctx, a)

	if _, err := a.StartSession(ctx); err != nil {
		log.Fatal(err)
	}
	res, err := a.Chat(ctx, "Hi")
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(res.Message.Content)
	// Output: Hello! How can I help with your project?
}

// ChatStream yields the answer as it is written: text chunks, then a done chunk. A
// failure ends the sequence with a non-nil error (for a provider failure, together
// with its error chunk), so checking err is enough.
func ExampleCreateAgent_stream() {
	ctx := context.Background()
	reg := scriptedRegistry(aecmock.ScriptedTurn{Content: "Streaming works."})

	a, err := aec.CreateAgent(ctx, aec.AgentConfig{Provider: "scripted", APIKey: "key", Model: "model"},
		aec.WithProviderRegistry(reg))
	if err != nil {
		log.Fatal(err)
	}
	defer mustDispose(ctx, a)

	if _, err := a.StartSession(ctx); err != nil {
		log.Fatal(err)
	}
	var text strings.Builder
	for chunk, err := range a.ChatStream(ctx, "Hi") {
		if err != nil {
			log.Fatal(err)
		}
		switch chunk.Type {
		case kernel.ChunkText:
			if chunk.Content != nil {
				text.WriteString(*chunk.Content)
			}
		case kernel.ChunkDone:
			fmt.Println("done:", text.String())
		default:
			// Tool-call progress, tool calls and tool results are not needed here.
		}
	}
	// Output: done: Streaming works.
}

// A provider failure ends ChatStream with its error chunk and a non-nil error in the
// same pair; nothing follows it.
func ExampleCreateAgent_streamFailure() {
	ctx := context.Background()
	reg := scriptedRegistry(aecmock.ScriptedTurn{Err: errors.New("the model is overloaded")})

	a, err := aec.CreateAgent(ctx, aec.AgentConfig{Provider: "scripted", APIKey: "key", Model: "model"},
		aec.WithProviderRegistry(reg))
	if err != nil {
		log.Fatal(err)
	}
	defer mustDispose(ctx, a)

	if _, err := a.StartSession(ctx); err != nil {
		log.Fatal(err)
	}
	for chunk, err := range a.ChatStream(ctx, "Hi") {
		if err != nil {
			fmt.Println("chunk:", chunk.Type)
			fmt.Println("error:", err)
			break
		}
	}
	// Output:
	// chunk: error
	// error: the model is overloaded
}

// AgentConfig.LLM takes a provider built in code, and AgentConfig.Profile changes the
// profile before the agent is spawned. Per-call model overrides are options of Chat.
func ExampleCreateAgent_options() {
	ctx := context.Background()
	provider := aecmock.NewScriptedProvider("scripted", []aecmock.ScriptedTurn{{Content: "Noted."}})

	a, err := aec.CreateAgent(ctx, aec.AgentConfig{
		LLM:   provider,
		Model: "model",
		Profile: func(p *profile.AgentProfile) {
			p.LoopConfig = &profile.LoopConfig{MaxIterations: llmhub.Int(4)}
		},
	})
	if err != nil {
		log.Fatal(err)
	}
	defer mustDispose(ctx, a)

	if _, err := a.StartSession(ctx, agent.WithSessionModel("model")); err != nil {
		log.Fatal(err)
	}
	res, err := a.Chat(ctx, "Log the delivery.", agent.WithTemperature(0.2), agent.WithMaxTokens(256))
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(a.Profile().Name, res.Message.Content)
	// Output: scripted-agent Noted.
}

// clockTool is a custom tool: a ToolHandler has a plugin name and version, a
// definition the model sees, and Execute.
type clockTool struct{}

func (clockTool) Name() string    { return "tool:site_clock" }
func (clockTool) Version() string { return "1.0.0" }

func (clockTool) Definition() toolhub.Definition {
	return toolhub.Definition{
		Name:        "site_clock",
		Description: "Returns the current time on site.",
		Parameters:  toolhub.JSONSchema{Type: "object"},
	}
}

func (clockTool) Execute(context.Context, kernel.Arguments, toolhub.Context) (toolhub.Output, error) {
	return toolhub.Output{Content: "07:30"}, nil
}

// A tool passed in AgentConfig.Tools is offered to the model; when the model calls
// it, the loop runs Execute and hands the result back inside the same turn.
func ExampleCreateAgent_customTool() {
	ctx := context.Background()
	reg := scriptedRegistry(
		aecmock.ScriptedTurn{ToolCalls: []kernel.ToolCall{{ID: "call-1", Name: "site_clock", Arguments: kernel.Arguments(`{}`)}}},
		aecmock.ScriptedTurn{Content: "It is 07:30 on site."},
	)

	a, err := aec.CreateAgent(ctx, aec.AgentConfig{
		Provider: "scripted", APIKey: "key", Model: "model",
		Tools: []toolhub.Handler{clockTool{}},
	}, aec.WithProviderRegistry(reg))
	if err != nil {
		log.Fatal(err)
	}
	defer mustDispose(ctx, a)

	if _, err := a.StartSession(ctx); err != nil {
		log.Fatal(err)
	}
	res, err := a.Chat(ctx, "What time is it on site?")
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(res.Message.Content)
	fmt.Println("iterations:", res.Iterations)
	// Output:
	// It is 07:30 on site.
	// iterations: 2
}
