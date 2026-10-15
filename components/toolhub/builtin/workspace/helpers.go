package workspace

import (
	"context"
	"errors"
	"slices"

	"github.com/Phong95/aec-agent-go/components/sandboxhub"
	"github.com/Phong95/aec-agent-go/components/toolhub"
	"github.com/Phong95/aec-agent-go/internal/toolkit"
	"github.com/Phong95/aec-agent-go/kernel"
	"github.com/Phong95/aec-agent-go/kernel/ordered"
)

// The non-ASCII characters of the tool texts are built from code points, so this source
// stays ASCII-only.
var (
	emDash   = string(rune(0x2014)) // U+2014 EM DASH
	midDot   = string(rune(0x00b7)) // U+00B7 MIDDLE DOT
	ellipsis = string(rune(0x2026)) // U+2026 HORIZONTAL ELLIPSIS
)

// hints are the advice appended to a SandboxError, by code. A code that is
// not listed has no hint.
var hints = map[kernel.SandboxErrorCode]string{
	kernel.SandboxCodePathEscape: "Only paths inside your workspace are allowed " + emDash +
		` relative paths like "src/app.ts", or "/workspace/shared/` + ellipsis + `" for files shared with other agents.`,
	kernel.SandboxCodeDisabled:               "This agent has no sandbox. Ask the user to enable one in the agent's Sandbox settings.",
	kernel.SandboxCodeNotConfigured:          "No sandbox is configured on this server.",
	kernel.SandboxCodeEnvironmentUnavailable: "The sandbox is not available right now (e.g. Docker is not running).",
	kernel.SandboxCodeNotFound:               "Check the path with list_files.",
	kernel.SandboxCodeConflict:               "The file changed since you read it " + emDash + " read it again before editing.",
}

// withSandbox runs fn against the agent's sandbox. Every failure
// becomes an output the model can act on, so the result is never an error:
//
//   - no sandbox for the agent: "<tool>: this agent has no workspace", an em dash and "no
//     sandbox is available", not retryable;
//   - a *kernel.SandboxError: "<tool> failed (<code>): <message>", then the hint of the
//     code on a new line when there is one, retryable as the error says, with the code
//     in the metadata. Deterministic failures (path escape, not found and the like) are
//     not retryable, so the harness does not repeat them;
//   - any other error: "<tool> failed: <message>", an error output with no retryable
//     flag, so the harness may retry it (a tool that runs commands uses withExecSandbox,
//     which marks it not retryable instead).
//
// fn returns the error of a sandbox call as it is; withSandbox does the mapping.
func withSandbox(ctx context.Context, tc toolhub.Context, name string, fn func(env sandboxhub.View) (toolhub.Output, error)) toolhub.Output {
	return runInSandbox(ctx, tc, name, false, fn)
}

// withExecSandbox is withSandbox for the tools that run a command (terminal, run_code).
// An error that is not a *kernel.SandboxError may come after the command started, so the
// command may already have had its side effects: the output is marked not retryable, and
// the harness never re-runs the command on its own (the model can decide to) ("exec
// failures are not retried"). A SandboxError
// keeps the retryable flag its code gives, and a failure to resolve the sandbox (nothing
// ran) is mapped as in withSandbox.
func withExecSandbox(ctx context.Context, tc toolhub.Context, name string, fn func(env sandboxhub.View) (toolhub.Output, error)) toolhub.Output {
	return runInSandbox(ctx, tc, name, true, fn)
}

// runInSandbox is the body of withSandbox and withExecSandbox; sideEffects marks an
// unclassified error of fn not retryable (a failure to resolve the sandbox ran nothing,
// so it keeps the withSandbox mapping).
func runInSandbox(ctx context.Context, tc toolhub.Context, name string, sideEffects bool, fn func(env sandboxhub.View) (toolhub.Output, error)) toolhub.Output {
	env, err := toolkit.SandboxFor(ctx, tc)
	// ran is true when the error came from fn, so the sandbox call itself may have
	// started; a failure to resolve the sandbox ran nothing.
	ran := false
	if err == nil {
		var out toolhub.Output
		if out, err = fn(env); err == nil {
			return out
		}
		ran = true
	}
	if errors.Is(err, toolkit.ErrNoSandbox) {
		return toolkit.Fail(name + ": this agent has no workspace " + emDash + " no sandbox is available")
	}
	var se *kernel.SandboxError
	if errors.As(err, &se) {
		content := name + " failed (" + string(se.Code) + "): " + se.Message
		if hint, ok := hints[se.Code]; ok {
			content += "\n" + hint
		}
		meta := ordered.NewObject()
		meta.Set("code", string(se.Code))
		return toolkit.Failure(content, se.Retryable, meta)
	}
	out := toolhub.Error(name + " failed: " + err.Error())
	if sideEffects && ran {
		out = out.NotRetryable()
	}
	return out
}

// argError is a validation problem the model can fix: "<tool>: <message>", not
// retryable.
func argError(name, message string) toolhub.Output {
	return toolkit.Fail(name + ": " + message)
}

// hasCapability reports whether the sandbox offers a capability.
func hasCapability(env sandboxhub.View, c sandboxhub.Capability) bool {
	return slices.Contains(env.Capabilities(), c)
}

// short is the first 12 characters of a hash. A hash is ASCII
// hex, so cutting bytes is cutting characters.
func short(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}
