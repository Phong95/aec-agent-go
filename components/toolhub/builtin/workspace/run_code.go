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

// maxCodeBytes is the longest program run_code accepts, in UTF-8 bytes.
const maxCodeBytes = 200_000

// languages are the languages run_code offers, in the order the schema lists them.
var languages = []string{"python", "javascript", "shell"}

// interpreterFor is the interpreter command of a language, or nil for an unknown one.
// The program is passed on stdin ("-" and "-s"), never interpolated into a command line.
func interpreterFor(language string) []string {
	switch language {
	case "python":
		return []string{"python3", "-"}
	case "javascript":
		return []string{"node", "-"}
	case "shell":
		return []string{"sh", "-s"}
	}
	return nil
}

// RunCodeTool is run_code: it runs a short program (python, javascript
// or shell) in the agent's sandbox with the same bounds as terminal. It is for
// calculations, data wrangling and quick checks without writing a file first. The
// program goes to the interpreter on stdin through sandboxhub.View.Exec. Build one with
// NewRunCodeTool. The tool holds only its resolved options, so it is safe for
// concurrent use.
type RunCodeTool struct {
	opts resolved
}

var _ toolhub.Handler = (*RunCodeTool)(nil)

// NewRunCodeTool returns the run_code tool for the given options.
func NewRunCodeTool(o Options) *RunCodeTool { return &RunCodeTool{opts: resolve(o)} }

// Name returns the plugin name.
func (*RunCodeTool) Name() string { return "tool:run-code" }

// Version returns the plugin version.
func (*RunCodeTool) Version() string { return "1.0.0" }

// Definition returns the tool definition (a fresh value on every call): a draft-risk
// tool on the sandbox operation exec. The descriptions print the default and the
// maximum timeout of the options, in whole seconds.
func (t *RunCodeTool) Definition() toolhub.Definition {
	return toolhub.Definition{
		Name: "run_code",
		Description: "Run a short program in your sandbox and get its output " + emDash + " python (python3), javascript (node) or shell (sh). " +
			"Runs in your working directory, so it can read and write your workspace files. Print what you need to see. " +
			"Default timeout " + seconds(t.opts.defaultTimeoutMs) + " s.",
		Parameters: toolhub.JSONSchema{
			Type: "object",
			Properties: toolhub.NewProperties(
				toolhub.Property{Name: "language", Schema: toolhub.JSONSchema{Type: "string", Enum: append([]string(nil), languages...), Description: "Programming language"}},
				toolhub.Property{Name: "code", Schema: toolhub.JSONSchema{Type: "string", Description: "The program source"}},
				toolhub.Property{Name: "cwd", Schema: toolhub.JSONSchema{Type: "string", Description: "Directory to run in (default: your working directory)"}},
				toolhub.Property{Name: "timeout_seconds", Schema: toolhub.JSONSchema{Type: "number", Description: "Kill the program after this many seconds (max " + seconds(t.opts.maxTimeoutMs) + ")"}},
				// A-101: secrets as environment variables.
				secretsProperty(),
			),
			Required: []string{"language", "code"},
		},
		Risk:             toolhub.RiskDraft,
		SandboxOperation: sandboxhub.OperationExec,
		// A-103: a tool job may run it on a schedule (its exact arguments approved up front).
		Schedulable: true,
	}
}

// Execute runs the program. The language must be one of python, javascript and shell;
// the code must not be blank and is at most 200 000 bytes; the cwd is trimmed with the
// JS trim. Exit code 127 means the interpreter is missing, and the result says so.
func (t *RunCodeTool) Execute(ctx context.Context, args kernel.Arguments, tc toolhub.Context) (toolhub.Output, error) {
	a := toolkit.DecodeArgs(args)
	language, _ := a.String("language")
	interpreter := interpreterFor(language)
	if interpreter == nil {
		return argError("run_code", `"language" must be one of `+strings.Join(languages, ", ")), nil
	}
	code, _ := a.String("code")
	if jsstr.Trim(code) == "" {
		return argError("run_code", `"code" must be a non-empty string`), nil
	}
	if len(code) > maxCodeBytes {
		return argError("run_code", "code is too long (max "+strconv.Itoa(maxCodeBytes)+" bytes) "+emDash+" write it to a file and run it with terminal"), nil
	}
	rawCwd, _ := a.String("cwd")
	cwd := jsstr.Trim(rawCwd)
	secretVars, failed := secretEnv(ctx, "run_code", a, tc)
	if failed != nil {
		return *failed, nil
	}

	return withExecSandbox(ctx, tc, "run_code", func(env sandboxhub.View) (toolhub.Output, error) {
		timeoutMs := timeoutFrom(a, t.opts, env)
		result, err := env.Exec(ctx, sandboxhub.ExecRequest{
			Command:        interpreter,
			Stdin:          code,
			Cwd:            cwd,
			Env:            secretVars,
			TimeoutMs:      timeoutMs,
			MaxOutputBytes: execOutputLimit(t.opts, env),
		}, sandboxhub.ExecOptions{})
		if err != nil {
			return toolhub.Output{}, err
		}
		output := formatExecResult(language+" program", result, timeoutMs, t.opts.maxDisplayOutputBytes)
		if result.ExitCode != nil && *result.ExitCode == 127 {
			output.Content += "\n" + interpreter[0] + " is not installed in this sandbox."
		}
		return output, nil
	}), nil
}
