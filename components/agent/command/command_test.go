package command_test

import (
	"context"
	"errors"
	"testing"

	"github.com/Phong95/aec-agent-go/components/agent/command"
	"github.com/Phong95/aec-agent-go/kernel"
)

// A-136: the parser and the spec checks.

func TestParse(t *testing.T) {
	cases := []struct {
		text string
		want command.Parsed
	}{
		{"/compact keep the RFI list", command.Parsed{Name: "compact", Args: "keep the RFI list"}},
		{"  /Web-Research  roofs ", command.Parsed{Name: "web-research", Args: "roofs"}},
		{"/new@site_bot", command.Parsed{Name: "new", Target: "site_bot"}},
	}
	for _, c := range cases {
		got, ok := command.Parse(c.text)
		if !ok || got != c.want {
			t.Errorf("Parse(%q) = %+v, %v; want %+v", c.text, got, ok, c.want)
		}
	}
	for _, text := range []string{"please /compact", "/path/to/file is broken", "/"} {
		if got, ok := command.Parse(text); ok {
			t.Errorf("Parse(%q) = %+v, want no command", text, got)
		}
	}
}

func TestValidateAndUsage(t *testing.T) {
	run := func(context.Context, command.RunContext) (command.Result, error) { return command.Result{}, nil }
	if err := command.Validate(command.Spec{Name: "Bad Name", Run: run}); !errors.Is(err, &command.Error{Code: command.CodeInvalid}) {
		t.Errorf("bad name: %v", err)
	}
	if err := command.Validate(command.Spec{Name: "both", Run: run, Prompt: "x"}); err == nil || err.Error() != `Command "/both" needs exactly one of run and prompt` {
		t.Errorf("both: %v", err)
	}
	if err := command.Validate(command.Spec{Name: "x", Run: run, Arguments: []command.Argument{{Name: "a", Rest: true}, {Name: "b"}}}); err == nil {
		t.Error("a rest argument that is not the last must be invalid")
	}
	var sdk *kernel.SDKError
	if err := command.Validate(command.Spec{Name: "neither"}); !errors.As(err, &sdk) {
		t.Errorf("errors.As to *kernel.SDKError: %v", err)
	}
	spec := command.Spec{Name: "permissions", Arguments: []command.Argument{{Name: "mode", Choices: []string{"ask", "yolo", "plan"}}}}
	if got := command.Usage(spec); got != "/permissions [ask|yolo|plan]" {
		t.Errorf("Usage = %q", got)
	}
	if got := command.Usage(command.Spec{Name: "rename", Arguments: []command.Argument{{Name: "name", Required: true, Rest: true}}}); got != "/rename <name>" {
		t.Errorf("Usage = %q", got)
	}
}

func TestCheckClash(t *testing.T) {
	entries := []command.Entry{
		{Spec: command.Spec{Name: "clear", Aliases: []string{"new"}}, Source: command.SourceBuiltIn},
		{Spec: command.Spec{Name: "usage"}, Source: command.SourceBuiltIn, Replaceable: true},
	}
	err := command.CheckClash(command.Spec{Name: "x", Aliases: []string{"new"}}, command.RegisterOptions{}, entries, command.SourceAgent)
	if err == nil || err.Error() != "/new is already a built-in command (an alias of /clear); agent commands need their own names" {
		t.Errorf("alias clash: %v", err)
	}
	if err := command.CheckClash(command.Spec{Name: "usage"}, command.RegisterOptions{Replace: true}, entries, command.SourceHost); err != nil {
		t.Errorf("replacing /usage: %v", err)
	}
	if err := command.CheckClash(command.Spec{Name: "clear"}, command.RegisterOptions{Replace: true}, entries, command.SourceHost); err == nil || err.Error() != "/clear can't be replaced" {
		t.Errorf("replacing /clear: %v", err)
	}
	var ce *command.Error
	if !errors.As(command.CheckClash(command.Spec{Name: "usage"}, command.RegisterOptions{}, entries, command.SourceHost), &ce) || ce.Code != command.CodeConflict || ce.Name() != "CommandError" {
		t.Errorf("conflict: %+v", ce)
	}
}
