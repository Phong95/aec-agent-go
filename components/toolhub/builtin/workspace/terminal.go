package workspace

import (
	"context"
	"strconv"
	"strings"

	"github.com/Phong95/aec-agent-go/components/sandboxhub"
	"github.com/Phong95/aec-agent-go/components/toolhub"
	"github.com/Phong95/aec-agent-go/internal/jsstr"
	"github.com/Phong95/aec-agent-go/internal/toolkit"
	"github.com/Phong95/aec-agent-go/kernel"
)

// maxCommandChars is the longest command terminal accepts, in UTF-16 code units (the JS
// String length).
const maxCommandChars = 10_000

// TerminalTool is terminal: it runs one command in the agent's sandbox
// with bounded time and output, no server environment variables and a working directory
// inside the workspace. It uses the sandbox's shell when the sandbox offers one (the
// exec.shell capability); otherwise it runs a simple command as argv and refuses shell
// syntax. The command runs only through sandboxhub.View.Exec. Build one with
// NewTerminalTool. The tool holds only its resolved options, so it is safe for
// concurrent use.
type TerminalTool struct {
	opts resolved
}

var _ toolhub.Handler = (*TerminalTool)(nil)

// NewTerminalTool returns the terminal tool for the given options.
func NewTerminalTool(o Options) *TerminalTool { return &TerminalTool{opts: resolve(o)} }

// Name returns the plugin name.
func (*TerminalTool) Name() string { return "tool:terminal" }

// Version returns the plugin version.
func (*TerminalTool) Version() string { return "1.0.0" }

// Definition returns the tool definition (a fresh value on every call): a draft-risk
// tool on the sandbox operation exec. The descriptions print the default and the
// maximum timeout of the options, in whole seconds (the JS Math.round).
func (t *TerminalTool) Definition() toolhub.Definition {
	return toolhub.Definition{
		Name: "terminal",
		Description: "Run a shell command in your sandbox (Linux) and get its exit code and output " + emDash + " e.g. build, test, git, " +
			"file conversions. Starts in your working directory. No network unless the sandbox allows it. " +
			"Default timeout " + seconds(t.opts.defaultTimeoutMs) + " s; long output is shortened to its start and end. " +
			"Do not start interactive programs or servers that never exit.",
		Parameters: toolhub.JSONSchema{
			Type: "object",
			Properties: toolhub.NewProperties(
				toolhub.Property{Name: "command", Schema: toolhub.JSONSchema{Type: "string", Description: `The command line, e.g. "npm test" or "ls -la src | head"`}},
				toolhub.Property{Name: "cwd", Schema: toolhub.JSONSchema{Type: "string", Description: "Directory to run in (default: your working directory)"}},
				toolhub.Property{Name: "timeout_seconds", Schema: toolhub.JSONSchema{Type: "number", Description: "Kill the command after this many seconds (max " + seconds(t.opts.maxTimeoutMs) + ")"}},
				// A-101: secrets as environment variables.
				secretsProperty(),
			),
			Required: []string{"command"},
		},
		Risk:             toolhub.RiskDraft,
		SandboxOperation: sandboxhub.OperationExec,
		// A-103: a tool job may run it on a schedule (its exact arguments approved up front).
		Schedulable: true,
	}
}

// seconds is Math.round(ms / 1000) as text.
func seconds(ms int) string {
	return strconv.FormatFloat(jsstr.Round(float64(ms)/1000), 'f', -1, 64)
}

// Execute runs the command. The command and the cwd are trimmed with the JS trim. With a
// shell the whole command line is one shell command; without one it is split into argv
// (quotes and backslashes are understood), and a command that needs shell syntax is
// refused with an explanation, because running it without a shell would mean something
// else. The timeout is timeout_seconds bounded by the options and by the sandbox, and
// the sandbox's own output ceiling stops a runaway command; the model sees a head and
// tail excerpt of the output.
func (t *TerminalTool) Execute(ctx context.Context, args kernel.Arguments, tc toolhub.Context) (toolhub.Output, error) {
	a := toolkit.DecodeArgs(args)
	rawCommand, _ := a.String("command")
	command := jsstr.Trim(rawCommand)
	if command == "" {
		return argError("terminal", `"command" must be a non-empty string`), nil
	}
	if kernel.UTF16Len(command) > maxCommandChars {
		return argError("terminal", "command is too long (max "+strconv.Itoa(maxCommandChars)+" characters)"), nil
	}
	rawCwd, _ := a.String("cwd")
	cwd := jsstr.Trim(rawCwd)
	secretVars, failed := secretEnv(ctx, "terminal", a, tc)
	if failed != nil {
		return *failed, nil
	}

	return withExecSandbox(ctx, tc, "terminal", func(env sandboxhub.View) (toolhub.Output, error) {
		var argv []string
		shell := false
		if hasCapability(env, sandboxhub.CapabilityExecShell) {
			argv = []string{command}
			shell = true
		} else {
			argv = splitArgv(command)
			if argv == nil {
				return argError("terminal", "this sandbox runs commands without a shell "+emDash+
					" pipes, redirects, variables and command chaining are not available. Run one program with its arguments."), nil
			}
		}
		timeoutMs := timeoutFrom(a, t.opts, env)
		result, err := env.Exec(ctx, sandboxhub.ExecRequest{
			Command:        argv,
			Shell:          &shell,
			Cwd:            cwd,
			Env:            secretVars,
			TimeoutMs:      timeoutMs,
			MaxOutputBytes: execOutputLimit(t.opts, env),
		}, sandboxhub.ExecOptions{})
		if err != nil {
			return toolhub.Output{}, err
		}
		return formatExecResult(command, result, timeoutMs, t.opts.maxDisplayOutputBytes), nil
	}), nil
}

// shellSpecial are the characters that only a shell gives a meaning to: pipes,
// redirects, chaining, substitution, globs, braces and the home directory.
const shellSpecial = "|&;<>()$`*?{}~"

// splitArgv splits a simple command line into argv. Single and double
// quotes group words, a backslash escapes the next character outside single quotes, and
// white space (JS \s) separates words. It returns nil when the line needs a shell: it
// holds an unquoted shell character, a quote that is never closed, or no word at all.
func splitArgv(command string) []string {
	var argv []string
	var current strings.Builder
	var quote rune
	hasToken := false
	chars := []rune(command)
	for i := 0; i < len(chars); i++ {
		ch := chars[i]
		if quote != 0 {
			switch {
			case ch == quote:
				quote = 0
			case ch == '\\' && quote == '"' && i+1 < len(chars):
				i++
				current.WriteRune(chars[i])
			default:
				current.WriteRune(ch)
			}
			continue
		}
		switch {
		case ch == '"' || ch == '\'':
			quote = ch
			hasToken = true
		case jsstr.IsSpace(ch):
			if hasToken {
				argv = append(argv, current.String())
			}
			current.Reset()
			hasToken = false
		case strings.ContainsRune(shellSpecial, ch):
			return nil
		case ch == '\\' && i+1 < len(chars):
			i++
			current.WriteRune(chars[i])
			hasToken = true
		default:
			current.WriteRune(ch)
			hasToken = true
		}
	}
	if quote != 0 {
		return nil
	}
	if hasToken {
		argv = append(argv, current.String())
	}
	return argv
}
