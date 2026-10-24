package workspace

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/Phong95/aec-agent-go/components/sandboxhub"
	"github.com/Phong95/aec-agent-go/components/toolhub"
	"github.com/Phong95/aec-agent-go/internal/toolkit"
	"github.com/Phong95/aec-agent-go/kernel"
)

// TestOptionsDefaults: a zero or negative field means the default, a positive field is
// kept.
func TestOptionsDefaults(t *testing.T) {
	want := resolved{
		maxReadBytes: 32_000, maxDisplayOutputBytes: 30_000, defaultTimeoutMs: 120_000,
		maxTimeoutMs: 600_000, maxListEntries: 500, maxSearchMatches: 100,
	}
	if got := resolve(Options{}); got != want {
		t.Errorf("resolve(zero) = %+v, want %+v", got, want)
	}
	if got := resolve(Options{MaxReadBytes: -1, MaxDisplayOutputBytes: -5, DefaultTimeoutMs: -1, MaxTimeoutMs: -1, MaxListEntries: -1, MaxSearchMatches: -1}); got != want {
		t.Errorf("resolve(negative) = %+v, want the defaults %+v", got, want)
	}
	got := resolve(Options{MaxReadBytes: 1, MaxDisplayOutputBytes: 2, DefaultTimeoutMs: 3, MaxTimeoutMs: 4, MaxListEntries: 5, MaxSearchMatches: 6})
	if got != (resolved{1, 2, 3, 4, 5, 6}) {
		t.Errorf("resolve(set) = %+v", got)
	}
}

// TestWithSandboxMapsEveryError covers the withSandbox mapping: the missing sandbox,
// each SandboxError code with its hint (or none), and an error that is no SandboxError.
func TestWithSandboxMapsEveryError(t *testing.T) {
	view := func(context.Context) (sandboxhub.View, error) { return nil, nil }
	noSandbox := withSandbox(t.Context(), toolhub.Context{GetSandbox: view}, "read_file", func(sandboxhub.View) (toolhub.Output, error) {
		t.Fatal("fn must not run without a sandbox")
		return toolhub.Output{}, nil
	})
	if noSandbox.Content != "read_file: this agent has no workspace \u2014 no sandbox is available" || !notRetryable(noSandbox) {
		t.Errorf("no sandbox = %+v", noSandbox)
	}

	run := func(err error) toolhub.Output {
		env := &loggedView{}
		tc := toolhub.Context{GetSandbox: func(context.Context) (sandboxhub.View, error) { return env, nil }}
		return withSandbox(t.Context(), tc, "x_tool", func(sandboxhub.View) (toolhub.Output, error) { return toolhub.Output{}, err })
	}

	for code, hint := range hints {
		out := run(kernel.NewSandboxError("msg", code, "id"))
		want := "x_tool failed (" + string(code) + "): msg\n" + hint
		if out.Content != want {
			t.Errorf("%s: content = %q, want %q", code, out.Content, want)
		}
	}
	for _, code := range []kernel.SandboxErrorCode{kernel.SandboxCodeTimeout, kernel.SandboxCodeTooLarge, kernel.SandboxCodeInvalidPath, kernel.SandboxCodeCancelled} {
		if _, hinted := hints[code]; hinted {
			t.Fatalf("%s is expected to have no hint", code)
		}
		out := run(kernel.NewSandboxError("msg", code, "id"))
		if out.Content != "x_tool failed ("+string(code)+"): msg" {
			t.Errorf("%s: content = %q, want no hint line", code, out.Content)
		}
	}

	// Retryable follows the error: timeout is retryable, not_found is not.
	if out := run(kernel.NewSandboxError("m", kernel.SandboxCodeTimeout, "")); out.Retryable == nil || !*out.Retryable {
		t.Errorf("timeout must be retryable: %+v", out)
	}
	if out := run(kernel.NewSandboxError("m", kernel.SandboxCodeNotFound, "")); !notRetryable(out) {
		t.Errorf("not_found must not be retryable: %+v", out)
	}

	// A wrapped SandboxError is still found with errors.As.
	wrapped := run(errors.Join(errors.New("context"), kernel.NewSandboxError("deep", kernel.SandboxCodeNotFound, "")))
	if !strings.HasPrefix(wrapped.Content, "x_tool failed (not_found): deep") {
		t.Errorf("wrapped = %q", wrapped.Content)
	}

	// Any other error: no retryable flag at all, so the harness may retry.
	other := run(errors.New("disk exploded"))
	if other.Content != "x_tool failed: disk exploded" || !isError(other) || other.Retryable != nil || other.Metadata.Len() != 0 {
		t.Errorf("other = %+v", other)
	}
}

// TestWithExecSandboxIsNotRetried: for a tool that runs a command, an error that is no
// SandboxError may come after the command started, so the output is not retryable and the
// harness never re-runs the command (Go divergence "exec failures are not retried"). A
// SandboxError keeps the retryable flag of its code, and the no-sandbox text is the same.
func TestWithExecSandboxIsNotRetried(t *testing.T) {
	env := &loggedView{}
	tc := toolhub.Context{GetSandbox: func(context.Context) (sandboxhub.View, error) { return env, nil }}
	run := func(err error) toolhub.Output {
		return withExecSandbox(t.Context(), tc, "terminal", func(sandboxhub.View) (toolhub.Output, error) { return toolhub.Output{}, err })
	}
	if out := run(errors.New("pipe broke")); out.Content != "terminal failed: pipe broke" || !notRetryable(out) {
		t.Errorf("other error = %+v, want a not-retryable error", out)
	}
	if out := run(kernel.NewSandboxError("m", kernel.SandboxCodeEnvironmentUnavailable, "")); out.Retryable == nil || !*out.Retryable {
		t.Errorf("environment_unavailable keeps its retryable flag: %+v", out)
	}
	if out := run(kernel.NewSandboxError("m", kernel.SandboxCodeInvalidArgument, "")); !notRetryable(out) {
		t.Errorf("invalid_argument must not be retryable: %+v", out)
	}
	if out := run(nil); isError(out) {
		t.Errorf("a success must pass through: %+v", out)
	}
	// A sandbox that cannot be resolved ran nothing: the withSandbox mapping (no flag).
	unresolved := toolhub.Context{GetSandbox: func(context.Context) (sandboxhub.View, error) { return nil, errors.New("offline") }}
	out := withExecSandbox(t.Context(), unresolved, "run_code", func(sandboxhub.View) (toolhub.Output, error) {
		t.Fatal("fn must not run without a sandbox")
		return toolhub.Output{}, nil
	})
	if out.Content != "run_code failed: offline" || !isError(out) || out.Retryable != nil {
		t.Errorf("unresolved sandbox = %+v", out)
	}
}

// TestInputLimits: the argument limits of terminal and run_code, counted as the tools count
// them (UTF-16 units for a command, UTF-8 bytes for code). The checks run before the
// sandbox is looked up, so a call with no accessor tells a rejected input (the limit
// text) from an accepted one (the no-workspace text).
func TestInputLimits(t *testing.T) {
	bare := toolhub.Context{SessionID: "s", Emit: func(string, any) {}}
	const noWorkspace = "this agent has no workspace"

	quote := func(s string) string { return `"` + s + `"` }
	for _, tc := range []struct {
		name    string
		handler toolhub.Handler
		args    string
		want    string
	}{
		{"command of 10000 characters", NewTerminalTool(Options{}), `{"command":` + quote(strings.Repeat("a", 10_000)) + `}`, noWorkspace},
		{"command of 10001 characters", NewTerminalTool(Options{}), `{"command":` + quote(strings.Repeat("a", 10_001)) + `}`, "command is too long (max 10000 characters)"},
		{"5000 emoji are 10000 units", NewTerminalTool(Options{}), `{"command":` + quote(strings.Repeat("\U0001F600", 5000)) + `}`, noWorkspace},
		{"5001 emoji are 10002 units", NewTerminalTool(Options{}), `{"command":` + quote(strings.Repeat("\U0001F600", 5001)) + `}`, "command is too long"},
		{"code of 200000 bytes", NewRunCodeTool(Options{}), `{"language":"python","code":` + quote(strings.Repeat("a", 200_000)) + `}`, noWorkspace},
		{"code of 200001 bytes", NewRunCodeTool(Options{}), `{"language":"python","code":` + quote(strings.Repeat("a", 200_001)) + `}`, "code is too long (max 200000 bytes)"},
		{"66666 euro signs are 199998 bytes", NewRunCodeTool(Options{}), `{"language":"python","code":` + quote(strings.Repeat("\u20ac", 66_666)) + `}`, noWorkspace},
		{"66667 euro signs are 200001 bytes", NewRunCodeTool(Options{}), `{"language":"python","code":` + quote(strings.Repeat("\u20ac", 66_667)) + `}`, "code is too long"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := execute(t, tc.handler, bare, tc.args)
			if !isError(out) || !strings.Contains(out.Content, tc.want) {
				t.Errorf("content = %.120q, want it to contain %q", out.Content, tc.want)
			}
		})
	}
}

// TestReadFileHugeLineNumber: a line number beyond the int range is clamped, not wrapped
// into a negative range.
func TestReadFileHugeLineNumber(t *testing.T) {
	env, tc := setup(t)
	mustWrite(t, env, "a.txt", "one\ntwo\n")
	out := execute(t, NewReadFileTool(Options{}), tc, `{"path":"a.txt","start_line":1e30,"end_line":1e31}`)
	if isError(out) {
		t.Fatalf("a huge line range failed: %s", out.Content)
	}
	if !strings.Contains(out.Content, "(empty file)") {
		t.Errorf("content = %q, want an empty range", out.Content)
	}
}

// TestSearchLineCutAtSurrogate: a matching line is cut at 300 UTF-16 units; a pair that
// would be split is dropped whole (the jsstr.TruncateUTF16 divergence from JS, which
// leaves a lone surrogate).
func TestSearchLineCutAtSurrogate(t *testing.T) {
	env, tc := setup(t)
	line := "needle " + strings.Repeat("x", 292) + "\U0001F600" + "tail"
	mustWrite(t, env, "p.txt", line+"\n")
	out := execute(t, NewSearchFilesTool(Options{}), tc, `{"pattern":"needle"}`)
	if isError(out) {
		t.Fatalf("search failed: %s", out.Content)
	}
	want := ": " + line[:len("needle ")+292] + "\u2026"
	if !strings.Contains(out.Content, want) {
		t.Errorf("content = %q, want the line cut before the emoji", out.Content)
	}
}

// TestDefinitionsAreFresh: Definition returns a value the caller may change without
// touching the next one.
func TestDefinitionsAreFresh(t *testing.T) {
	for _, h := range NewTools(Options{}) {
		d := h.Definition()
		d.Name = "changed"
		d.Parameters.Required = append(d.Parameters.Required, "extra")
		if again := h.Definition(); again.Name == "changed" || len(again.Parameters.Required) == len(d.Parameters.Required) {
			t.Errorf("%s: Definition shares state between calls", h.Name())
		}
	}
}

// zeroExecLimitsView is a third-party sandboxhub.View that never set its exec limits: both stay
// at the Go zero value. It records the exec request it receives.
type zeroExecLimitsView struct {
	sandboxhub.View
	got sandboxhub.ExecRequest
}

func (v *zeroExecLimitsView) Limits() sandboxhub.Limits {
	return sandboxhub.Limits{MaxReadBytes: 100000, MaxWriteBytes: 100000, MaxListEntries: 500, MaxSearchMatches: 100}
}

func (v *zeroExecLimitsView) Exec(_ context.Context, req sandboxhub.ExecRequest, _ sandboxhub.ExecOptions) (sandboxhub.ExecResult, error) {
	v.got = req
	zero := 0
	return sandboxhub.ExecResult{ExitCode: &zero, Stdout: "ok\n"}, nil
}

// TestTimeoutFromZeroSandboxLimit (review WR-B05): a sandbox that leaves MaxExecTimeoutMs and
// MaxExecOutputBytes at their zero value declares no cap. terminal and run_code then use the
// tool's own bounds instead of a 1 ms timeout (a spurious "timed out after 0 s") and a zero
// output limit.
func TestTimeoutFromZeroSandboxLimit(t *testing.T) {
	o := resolve(Options{})
	view := &zeroExecLimitsView{View: newEnvironment(t, sandboxSpec{Mode: "isolated"}).plain}
	args := func(raw string) toolkit.Args {
		a, err := kernel.NewArguments([]byte(raw))
		if err != nil {
			t.Fatal(err)
		}
		return toolkit.DecodeArgs(a)
	}
	if got := timeoutFrom(args(`{}`), o, view); got != o.defaultTimeoutMs {
		t.Errorf("default timeout with a zero sandbox limit = %d ms, want %d", got, o.defaultTimeoutMs)
	}
	if got := timeoutFrom(args(`{"timeout_seconds":30}`), o, view); got != 30000 {
		t.Errorf("30 s with a zero sandbox limit = %d ms, want 30000", got)
	}
	if got := timeoutFrom(args(`{"timeout_seconds":100000}`), o, view); got != o.maxTimeoutMs {
		t.Errorf("a huge request is still bounded by the tool maximum: %d ms, want %d", got, o.maxTimeoutMs)
	}
	// A real sandbox limit still wins when it is lower.
	limited := &limitedView{max: 5000}
	if got := timeoutFrom(args(`{"timeout_seconds":30}`), o, limited); got != 5000 {
		t.Errorf("a positive sandbox limit must still cap the timeout: %d ms, want 5000", got)
	}

	tc := toolhub.Context{GetSandbox: func(context.Context) (sandboxhub.View, error) { return view, nil }}
	for name, h := range map[string]toolhub.Handler{"terminal": NewTerminalTool(Options{}), "run_code": NewRunCodeTool(Options{})} {
		raw := `{"command":"echo ok"}`
		if name == "run_code" {
			raw = `{"language":"shell","code":"echo ok"}`
		}
		a, err := kernel.NewArguments([]byte(raw))
		if err != nil {
			t.Fatal(err)
		}
		view.got = sandboxhub.ExecRequest{}
		out, err := h.Execute(t.Context(), a, tc)
		if err != nil {
			t.Fatal(err)
		}
		if out.IsError != nil && *out.IsError {
			t.Errorf("%s: unexpected error output %q", name, out.Content)
		}
		if view.got.TimeoutMs != o.defaultTimeoutMs {
			t.Errorf("%s: exec timeout = %d ms, want the tool default %d (not 1 ms)", name, view.got.TimeoutMs, o.defaultTimeoutMs)
		}
		if view.got.MaxOutputBytes != o.maxDisplayOutputBytes {
			t.Errorf("%s: exec output limit = %d, want the tool display limit %d", name, view.got.MaxOutputBytes, o.maxDisplayOutputBytes)
		}
	}
}

// limitedView is a View with a positive exec timeout limit.
type limitedView struct {
	sandboxhub.View
	max int
}

func (v *limitedView) Limits() sandboxhub.Limits {
	return sandboxhub.Limits{MaxExecTimeoutMs: v.max, MaxExecOutputBytes: 1000}
}
