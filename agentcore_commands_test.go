package aec_test

import (
	"context"
	"errors"
	"testing"

	"github.com/Phong95/aec-agent-go/aecmock"
	"github.com/Phong95/aec-agent-go/components/agent/command"
	"github.com/Phong95/aec-agent-go/kernel"
)

// A-136: AgentCore.UseCommand - the host's slash commands, offered by every agent (the
// host part of tests/unit/commands.test.ts of core/ts).

func TestAgentCore_UseCommandAddsAHostCommandToEveryAgent(t *testing.T) {
	core := subagentCore(t)
	if err := core.UseLLMProvider(aecmock.NewMockProvider("primary", func([]kernel.Message) string { return "y" })); err != nil {
		t.Fatal(err)
	}
	a, err := core.SpawnAgent(t.Context(), subagentCoreProfile("site", nil))
	if err != nil {
		t.Fatal(err)
	}
	session, err := a.StartSession(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	reply := func(text string) func(context.Context, command.RunContext) (command.Result, error) {
		return func(context.Context, command.RunContext) (command.Result, error) {
			return command.Result{Text: text}, nil
		}
	}

	// A built-in that is not replaceable, or a name a built-in takes.
	err = core.UseCommand(command.Spec{Name: "help", Description: "x", Safety: command.SafetyReadOnly, Run: reply("")}, command.RegisterOptions{Replace: true})
	if err == nil || err.Error() != "/help can't be replaced" {
		t.Fatalf("replace /help: %v", err)
	}
	err = core.UseCommand(command.Spec{Name: "x", Aliases: []string{"new"}, Description: "x", Safety: command.SafetyReadOnly, Run: reply("")}, command.RegisterOptions{})
	if !errors.Is(err, &command.Error{Code: command.CodeConflict}) {
		t.Fatalf("alias of /clear: %v", err)
	}
	if err := core.UseCommand(command.Spec{Name: "Bad Name", Description: "x", Safety: command.SafetyReadOnly, Run: reply("")}, command.RegisterOptions{}); !errors.Is(err, &command.Error{Code: command.CodeInvalid}) {
		t.Fatalf("bad name: %v", err)
	}

	// A host replaces the replaceable /usage (it has token accounting); every agent,
	// spawned before or after, offers it.
	if err := core.UseCommand(command.Spec{Name: "usage", Description: "Tokens and cost", Safety: command.SafetyReadOnly, Run: reply("host usage")}, command.RegisterOptions{Replace: true}); err != nil {
		t.Fatal(err)
	}
	if err := core.UseCommand(command.Spec{Name: "standup", Description: "Daily summary", Safety: command.SafetyReadOnly, Prompt: "Summarize today: $ARGUMENTS"}, command.RegisterOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := core.UseCommand(command.Spec{Name: "standup", Description: "again", Safety: command.SafetyReadOnly, Prompt: "x"}, command.RegisterOptions{}); err == nil {
		t.Fatal("a second host /standup must conflict")
	}
	inv := command.Invocation{SessionID: session.ID, Client: "web"}
	if got := a.Commands().Run(t.Context(), "/usage", inv); got.Text != "host usage" {
		t.Fatalf("/usage = %+v", got)
	}
	if info, ok := a.Commands().Get("usage", command.AvailabilityContext{}); !ok || info.Source != command.SourceHost {
		t.Fatalf("usage info = %+v %v", info, ok)
	}
	got := a.Commands().Run(t.Context(), "/standup site B", inv)
	if got.Action == nil || got.Action.Type != command.ActionSendPrompt || got.Action.Prompt != "Summarize today: site B" {
		t.Fatalf("/standup = %+v", got)
	}
	// The agent's own commands can't take a host command's name.
	if err := a.Commands().Register(command.Spec{Name: "usage", Description: "x", Safety: command.SafetyReadOnly, Run: reply("")}, command.RegisterOptions{}); !errors.Is(err, &command.Error{Code: command.CodeConflict}) {
		t.Fatalf("agent /usage: %v", err)
	}
}
