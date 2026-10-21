package agent

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/Phong95/aec-agent-go/components/agent/command"
	"github.com/Phong95/aec-agent-go/components/sandboxhub"
)

// A-136: the developer workflow commands (/diff, /review, /init) over a stub workspace -
// the "developer workflow commands" part of tests/unit/commands.test.ts of core/ts.

// gitAnswer is what the stub's git answers for its arguments (after "git --no-pager").
type gitAnswer struct {
	exitCode int
	stdout   string
	stderr   string
}

// stubWorkspace is a workspace whose git answers from git.
type stubWorkspace struct {
	git func(args []string) gitAnswer
}

func (stubWorkspace) WorkingDir() string { return "/workspace/site" }

func (stubWorkspace) Capabilities() []sandboxhub.Capability {
	return []sandboxhub.Capability{sandboxhub.CapabilityExec}
}

func (w stubWorkspace) Exec(_ context.Context, req sandboxhub.ExecRequest, _ sandboxhub.ExecOptions) (sandboxhub.ExecResult, error) {
	a := w.git(req.Command[2:])
	code := a.exitCode
	return sandboxhub.ExecResult{ExitCode: &code, Stdout: a.stdout, Stderr: a.stderr, DurationMs: 1}, nil
}

// builtInsOver is a command manager whose built-ins work in a workspace with git.
func builtInsOver(git func(args []string) gitAnswer) func(text string) command.Result {
	var m *commandManager
	m = newCommandManager(commandManagerDeps{
		agentID: "a1",
		builtIns: builtInCommands(builtInCommandDeps{
			sandbox:           func(context.Context) (commandWorkspace, error) { return stubWorkspace{git: git}, nil },
			describeWorkspace: func() string { return "its own sandbox" },
			listCommands:      func(ac command.AvailabilityContext) []command.Info { return m.List(ac) },
		}),
		isBusy: func(string) bool { return false },
	})
	return func(text string) command.Result {
		return m.Run(context.Background(), text, command.Invocation{SessionID: "s1", Client: "web"})
	}
}

func TestBuiltInCommandSpecsAreValid(t *testing.T) {
	for _, e := range builtInCommands(builtInCommandDeps{}) {
		if err := command.Validate(e.Spec); err != nil {
			t.Errorf("/%s: %v", e.Spec.Name, err)
		}
	}
}

func TestDiffShowsTheUncommittedChangesOrSaysWhyNot(t *testing.T) {
	run := builtInsOver(func(args []string) gitAnswer {
		if args[0] == "status" {
			return gitAnswer{stdout: " M plan.md\n"}
		}
		if slices.Contains(args, "--cached") {
			return gitAnswer{}
		}
		return gitAnswer{stdout: "diff --git a/plan.md b/plan.md\n+Level 3\n"}
	})
	shown := run("/diff").Text
	if !strings.Contains(shown, " M plan.md") || !strings.Contains(shown, "+Level 3") {
		t.Fatalf("/diff = %q", shown)
	}

	clean := builtInsOver(func([]string) gitAnswer { return gitAnswer{} })
	if got := clean("/diff").Text; got != "No uncommitted changes in the workspace." {
		t.Fatalf("clean /diff = %q", got)
	}
	notRepo := builtInsOver(func([]string) gitAnswer { return gitAnswer{exitCode: 128, stderr: "fatal: not a git repository"} })
	if got := notRepo("/diff"); got != (command.Result{Text: "The workspace (/workspace/site) is not a git repository.", Error: command.ResultUnavailable}) {
		t.Fatalf("not a repository = %+v", got)
	}
	noGit := builtInsOver(func([]string) gitAnswer { return gitAnswer{exitCode: 127, stderr: "git: not found"} })
	if got := noGit("/diff").Text; got != "git isn't available in the agent's workspace." {
		t.Fatalf("no git = %q", got)
	}
}

func TestReviewSendsAReadOnlyPromptAndInitAsksForAgentsMd(t *testing.T) {
	run := builtInsOver(func(args []string) gitAnswer {
		if args[0] == "status" {
			return gitAnswer{stdout: " M a.ts"}
		}
		if slices.Contains(args, "--cached") {
			return gitAnswer{}
		}
		return gitAnswer{stdout: "+const x = 1;"}
	})
	review := run("/review error handling")
	if review.Action == nil || review.Action.Type != command.ActionSendPrompt || !review.Action.ReadOnly {
		t.Fatalf("/review = %+v", review)
	}
	for _, want := range []string{"do not change any file", "Focus on: error handling", "+const x = 1;"} {
		if !strings.Contains(review.Action.Prompt, want) {
			t.Fatalf("/review prompt does not contain %q:\n%s", want, review.Action.Prompt)
		}
	}

	initResult := run("/init")
	if initResult.Action == nil || initResult.Action.Type != command.ActionSendPrompt || !strings.Contains(initResult.Action.Prompt, "AGENTS.md file in the workspace root (/workspace/site)") {
		t.Fatalf("/init = %+v", initResult)
	}
}

func TestEditDistanceAndArgumentMatching(t *testing.T) {
	if d := editDistance("stauts", "status"); d != 2 {
		t.Fatalf("editDistance = %d", d)
	}
	spec := command.Spec{Name: "permissions", Arguments: []command.Argument{{Name: "mode", Choices: []string{"ask", "yolo", "plan"}}}}
	if argv, problem := matchArguments(spec, "YOLO"); problem != "" || argv["mode"] != "yolo" {
		t.Fatalf("matchArguments = %v %q", argv, problem)
	}
	if _, problem := matchArguments(spec, "maybe"); problem != `"maybe" isn't one of ask, yolo, plan.` {
		t.Fatalf("problem = %q", problem)
	}
	if _, problem := matchArguments(spec, "ask now"); problem != `Unexpected "now".` {
		t.Fatalf("problem = %q", problem)
	}
	if argv, problem := matchArguments(command.Spec{Name: "x"}, "anything at all"); problem != "" || len(argv) != 0 {
		t.Fatalf("free text = %v %q", argv, problem)
	}
}
