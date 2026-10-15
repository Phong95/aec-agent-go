package workspace

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/Phong95/aec-agent-go/aecmock"
	"github.com/Phong95/aec-agent-go/components/sandboxhub"
	"github.com/Phong95/aec-agent-go/components/toolhub"
	"github.com/Phong95/aec-agent-go/kernel"
)

// This file holds the 18 unit cases of the workspace tools: the tools against the
// in-memory sandbox view, with tool-level assertions. The recorded corpus (wire_test.go)
// compares the same tools byte for byte over many more cases; these cases keep readable
// test titles and a narrative.

func sha256Hex(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}

// setup builds a SandboxHub with the in-memory provider as the shared
// sandbox, and the view agent a1 gets from it.
func setup(t *testing.T) (sandboxhub.View, toolhub.Context) {
	t.Helper()
	hub := sandboxhub.NewSandboxHub()
	if err := hub.Register(aecmock.NewInMemoryProvider()); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if err := hub.ConfigureShared(sandboxhub.Spec{Kind: sandboxhub.KindDocker}); err != nil {
		t.Fatalf("ConfigureShared: %v", err)
	}
	env, err := hub.Resolve(t.Context(), "a1", sandboxhub.Selection{})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	t.Cleanup(func() {
		if err := hub.Dispose(context.WithoutCancel(t.Context())); err != nil {
			t.Errorf("Dispose: %v", err)
		}
	})
	return env, contextFor(env)
}

// contextFor is a tool context whose sandbox accessor returns env.
func contextFor(env sandboxhub.View) toolhub.Context {
	return toolhub.Context{
		SessionID:  "s1",
		AgentID:    "a1",
		Emit:       func(string, any) {},
		GetSandbox: func(context.Context) (sandboxhub.View, error) { return env, nil },
	}
}

// execute runs a handler with the JSON arguments and fails the test on a returned error:
// a workspace tool reports every problem as an output.
func execute(t *testing.T, h toolhub.Handler, tc toolhub.Context, args string) toolhub.Output {
	t.Helper()
	a, err := kernel.NewArguments([]byte(args))
	if err != nil {
		t.Fatalf("arguments %s: %v", args, err)
	}
	out, err := h.Execute(t.Context(), a, tc)
	if err != nil {
		t.Fatalf("Execute returned an error (every failure must be an output): %v", err)
	}
	return out
}

func isError(o toolhub.Output) bool { return o.IsError != nil && *o.IsError }

// notRetryable reports an error output that is marked not retryable.
func notRetryable(o toolhub.Output) bool { return isError(o) && o.Retryable != nil && !*o.Retryable }

// metaValue reads a metadata key, failing the test when it is absent.
func metaValue(t *testing.T, o toolhub.Output, key string) any {
	t.Helper()
	v, ok := o.Metadata.Get(key)
	if !ok {
		t.Fatalf("metadata has no %q: %+v", key, o.Metadata)
	}
	return v
}

func mustWrite(t *testing.T, env sandboxhub.View, path, content string) {
	t.Helper()
	if _, err := env.WriteFile(t.Context(), path, content, sandboxhub.WriteOptions{}); err != nil {
		t.Fatalf("WriteFile(%s): %v", path, err)
	}
}

func mustRead(t *testing.T, env sandboxhub.View, path string) string {
	t.Helper()
	fc, err := env.ReadFile(t.Context(), path, sandboxhub.ReadOptions{})
	if err != nil {
		t.Fatalf("ReadFile(%s): %v", path, err)
	}
	return fc.Content
}

// lyingView is a "lying" view: every read reports a sha256 of zeros, so a write
// does not read back as written.
type lyingView struct{ sandboxhub.View }

func (v lyingView) ReadFile(ctx context.Context, path string, opts sandboxhub.ReadOptions) (sandboxhub.FileContent, error) {
	fc, err := v.View.ReadFile(ctx, path, opts)
	fc.SHA256 = strings.Repeat("0", 64)
	return fc, err
}

// racingView is a "racing" view: someone else writes between edit_file's read and
// its write.
type racingView struct{ sandboxhub.View }

func (v racingView) ReadFile(ctx context.Context, path string, opts sandboxhub.ReadOptions) (sandboxhub.FileContent, error) {
	fc, err := v.View.ReadFile(ctx, path, opts)
	if err == nil && fc.Content == "v1 value" {
		if _, err := v.WriteFile(ctx, path, "v2 value by someone else", sandboxhub.WriteOptions{}); err != nil {
			return sandboxhub.FileContent{}, err
		}
	}
	return fc, err
}

func TestWorkspaceTools(t *testing.T) {
	// ---- workspace tool definitions ----

	t.Run("declares a sandbox operation and a risk on every tool", func(t *testing.T) {
		tools := NewTools(Options{})
		var names []string
		byName := map[string]toolhub.Definition{}
		for _, h := range tools {
			def := h.Definition()
			names = append(names, def.Name)
			byName[def.Name] = def
			if def.SandboxOperation == "" || def.Risk == "" {
				t.Errorf("%s declares risk %q and sandbox operation %q, want both", def.Name, def.Risk, def.SandboxOperation)
			}
		}
		if !slices.Equal(names, ToolNames()) {
			t.Errorf("tool names = %v, want %v", names, ToolNames())
		}
		if d := byName["read_file"]; d.Risk != toolhub.RiskReadOnly || d.SandboxOperation != sandboxhub.OperationFSRead {
			t.Errorf("read_file = risk %q op %q", d.Risk, d.SandboxOperation)
		}
		if d := byName["write_file"]; d.Risk != toolhub.RiskDraft || d.SandboxOperation != sandboxhub.OperationFSWrite {
			t.Errorf("write_file = risk %q op %q", d.Risk, d.SandboxOperation)
		}
		if d := byName["terminal"]; d.SandboxOperation != sandboxhub.OperationExec {
			t.Errorf("terminal op = %q", d.SandboxOperation)
		}
	})

	t.Run("fails without retry when the agent has no sandbox accessor", func(t *testing.T) {
		out := execute(t, NewReadFileTool(Options{}), toolhub.Context{SessionID: "s", Emit: func(string, any) {}}, `{"path":"a.txt"}`)
		if !notRetryable(out) {
			t.Fatalf("output = %+v, want an error that is not retryable", out)
		}
	})

	// ---- read_file ----

	t.Run("returns numbered lines with size, and line ranges", func(t *testing.T) {
		env, tc := setup(t)
		mustWrite(t, env, "notes.md", "alpha\nbeta\ngamma\n")
		all := execute(t, NewReadFileTool(Options{}), tc, `{"path":"notes.md"}`)
		if isError(all) {
			t.Fatalf("read failed: %s", all.Content)
		}
		if !strings.Contains(all.Content, "/workspace/agents/a1/notes.md (17 B)") {
			t.Errorf("content lacks the path and size: %q", all.Content)
		}
		if !strings.Contains(all.Content, "1\talpha\n2\tbeta\n3\tgamma") {
			t.Errorf("content lacks the numbered lines: %q", all.Content)
		}
		if got := metaValue(t, all, "sha256"); got != sha256Hex("alpha\nbeta\ngamma\n") {
			t.Errorf("sha256 = %v", got)
		}
		if got := metaValue(t, all, "truncated"); got != false {
			t.Errorf("truncated = %v, want false", got)
		}

		rng := execute(t, NewReadFileTool(Options{}), tc, `{"path":"notes.md","start_line":2,"end_line":2}`)
		if !strings.Contains(rng.Content, "2\tbeta") || strings.Contains(rng.Content, "alpha") {
			t.Errorf("range content = %q, want line 2 only", rng.Content)
		}
	})

	t.Run("caps what the model sees and says how to read more", func(t *testing.T) {
		env, tc := setup(t)
		mustWrite(t, env, "big.txt", strings.Repeat("x", 500))
		out := execute(t, NewReadFileTool(Options{MaxReadBytes: 100}), tc, `{"path":"big.txt"}`)
		if !strings.Contains(out.Content, "truncated") {
			t.Errorf("content = %q, want a truncation note", out.Content)
		}
		if metaValue(t, out, "truncated") != true || metaValue(t, out, "size") != 500 {
			t.Errorf("metadata = %+v, want truncated true and size 500", out.Metadata)
		}
	})

	t.Run("maps sandbox errors to non-retryable results with a hint", func(t *testing.T) {
		_, tc := setup(t)
		missing := execute(t, NewReadFileTool(Options{}), tc, `{"path":"nope.txt"}`)
		if !notRetryable(missing) || metaValue(t, missing, "code") != "not_found" {
			t.Errorf("missing = %+v", missing)
		}
		escape := execute(t, NewReadFileTool(Options{}), tc, `{"path":"/etc/passwd"}`)
		if !notRetryable(escape) || metaValue(t, escape, "code") != "path_escape" {
			t.Errorf("escape = %+v", escape)
		}
		if !strings.Contains(escape.Content, "Only paths inside your workspace") {
			t.Errorf("escape content = %q, want the path hint", escape.Content)
		}
	})

	t.Run("validates arguments", func(t *testing.T) {
		_, tc := setup(t)
		if out := execute(t, NewReadFileTool(Options{}), tc, `{"path":""}`); !notRetryable(out) {
			t.Errorf("empty path = %+v", out)
		}
		if out := execute(t, NewReadFileTool(Options{}), tc, `{"path":"a","start_line":3,"end_line":1}`); !isError(out) {
			t.Errorf("end before start = %+v", out)
		}
	})

	// ---- write_file ----

	t.Run("creates, then overwrites, verifying each write by read-back", func(t *testing.T) {
		env, tc := setup(t)
		created := execute(t, NewWriteFileTool(Options{}), tc, `{"path":"out/report.md","content":"# Report\n"}`)
		if !regexp.MustCompile(`^Created /workspace/agents/a1/out/report\.md .* verified by read-back`).MatchString(created.Content) {
			t.Errorf("created = %q", created.Content)
		}
		if metaValue(t, created, "verified") != true || metaValue(t, created, "sha256") != sha256Hex("# Report\n") {
			t.Errorf("created metadata = %+v", created.Metadata)
		}
		again := execute(t, NewWriteFileTool(Options{}), tc, `{"path":"out/report.md","content":"v2"}`)
		if !strings.HasPrefix(again.Content, "Overwrote") {
			t.Errorf("again = %q, want it to start with Overwrote", again.Content)
		}
		if got := mustRead(t, env, "out/report.md"); got != "v2" {
			t.Errorf("file = %q, want v2", got)
		}
	})

	t.Run("reports a write that does not read back as written", func(t *testing.T) {
		env, _ := setup(t)
		out := execute(t, NewWriteFileTool(Options{}), contextFor(lyingView{env}), `{"path":"x.txt","content":"data"}`)
		if !notRetryable(out) || metaValue(t, out, "verified") != false {
			t.Errorf("output = %+v, want a non-retryable error with verified false", out)
		}
	})

	// ---- edit_file ----

	t.Run("replaces a unique match and shows the edited region", func(t *testing.T) {
		env, tc := setup(t)
		mustWrite(t, env, "spec.md", "Rebar: 12mm\nSpacing: 200\nCover: 40\n")
		out := execute(t, NewEditFileTool(Options{}), tc, `{"path":"spec.md","old_text":"Spacing: 200","new_text":"Spacing: 150"}`)
		if isError(out) {
			t.Fatalf("edit failed: %s", out.Content)
		}
		if !strings.Contains(out.Content, "replaced 1 occurrence (line 2)") || !strings.Contains(out.Content, "2\tSpacing: 150") {
			t.Errorf("content = %q", out.Content)
		}
		if got := mustRead(t, env, "spec.md"); got != "Rebar: 12mm\nSpacing: 150\nCover: 40\n" {
			t.Errorf("file = %q", got)
		}
	})

	t.Run("refuses ambiguous and missing matches, and supports replace_all", func(t *testing.T) {
		env, tc := setup(t)
		mustWrite(t, env, "a.txt", "TODO one\nTODO two\n")
		ambiguous := execute(t, NewEditFileTool(Options{}), tc, `{"path":"a.txt","old_text":"TODO","new_text":"DONE"}`)
		if !notRetryable(ambiguous) || !strings.Contains(ambiguous.Content, "matches 2 places") {
			t.Errorf("ambiguous = %+v", ambiguous)
		}
		missing := execute(t, NewEditFileTool(Options{}), tc, `{"path":"a.txt","old_text":"nothing","new_text":"x"}`)
		if !strings.Contains(missing.Content, "was not found") {
			t.Errorf("missing = %q", missing.Content)
		}
		all := execute(t, NewEditFileTool(Options{}), tc, `{"path":"a.txt","old_text":"TODO","new_text":"DONE","replace_all":true}`)
		if !strings.Contains(all.Content, "replaced 2 occurrences") {
			t.Errorf("replace_all = %q", all.Content)
		}
		if got := mustRead(t, env, "a.txt"); got != "DONE one\nDONE two\n" {
			t.Errorf("file = %q", got)
		}
	})

	t.Run("does not overwrite a file that changed after it was read (conflict)", func(t *testing.T) {
		env, _ := setup(t)
		mustWrite(t, env, "race.txt", "v1 value")
		out := execute(t, NewEditFileTool(Options{}), contextFor(racingView{env}), `{"path":"race.txt","old_text":"v1","new_text":"v9"}`)
		if !isError(out) || metaValue(t, out, "code") != "conflict" {
			t.Errorf("output = %+v, want an error with code conflict", out)
		}
		if got := mustRead(t, env, "race.txt"); got != "v2 value by someone else" {
			t.Errorf("file = %q, want the other writer's content", got)
		}
	})

	// ---- list_files and search_files ----

	t.Run("lists relative to the folder, recursively", func(t *testing.T) {
		env, tc := setup(t)
		mustWrite(t, env, "src/a.ts", "export const a = 1;\n")
		mustWrite(t, env, "src/lib/b.ts", "export const b = 2;\n")
		flat := execute(t, NewListFilesTool(Options{}), tc, `{}`)
		if !strings.Contains(flat.Content, "src/") {
			t.Errorf("flat = %q", flat.Content)
		}
		deep := execute(t, NewListFilesTool(Options{}), tc, `{"path":"src","recursive":true}`)
		if !strings.Contains(deep.Content, "a.ts  (20 B)") || !strings.Contains(deep.Content, "lib/b.ts") {
			t.Errorf("deep = %q", deep.Content)
		}
	})

	t.Run("searches with literal, regex and glob, and reports no matches", func(t *testing.T) {
		env, tc := setup(t)
		mustWrite(t, env, "src/a.ts", "const Beam = 1;\n")
		mustWrite(t, env, "docs/b.md", "beam sizes\n")
		literal := execute(t, NewSearchFilesTool(Options{}), tc, `{"pattern":"beam","case_sensitive":false}`)
		if !strings.Contains(literal.Content, "2 matches") || !strings.Contains(literal.Content, "/workspace/agents/a1/src/a.ts:1: const Beam = 1;") {
			t.Errorf("literal = %q", literal.Content)
		}
		regex := execute(t, NewSearchFilesTool(Options{}), tc, `{"pattern":"^const \\w+","regex":true}`)
		if metaValue(t, regex, "count") != 1 {
			t.Errorf("regex = %+v", regex)
		}
		glob := execute(t, NewSearchFilesTool(Options{}), tc, `{"pattern":"beam","glob":"*.md"}`)
		if metaValue(t, glob, "count") != 1 {
			t.Errorf("glob = %+v", glob)
		}
		none := execute(t, NewSearchFilesTool(Options{}), tc, `{"pattern":"column"}`)
		if !strings.HasPrefix(none.Content, "No matches") {
			t.Errorf("none = %q", none.Content)
		}
	})

	// ---- terminal and run_code ----

	t.Run("runs a command and reports exit code and output", func(t *testing.T) {
		env, tc := setup(t)
		mustWrite(t, env, "hello.txt", "hi there")
		out := execute(t, NewTerminalTool(Options{}), tc, `{"command":"cat hello.txt"}`)
		if isError(out) {
			t.Fatalf("command failed: %s", out.Content)
		}
		for _, want := range []string{"$ cat hello.txt", "exit code 0", "stdout:\nhi there"} {
			if !strings.Contains(out.Content, want) {
				t.Errorf("content %q lacks %q", out.Content, want)
			}
		}
	})

	t.Run("reports failures and timeouts as non-retryable errors (no automatic re-run)", func(t *testing.T) {
		_, tc := setup(t)
		failed := execute(t, NewTerminalTool(Options{}), tc, `{"command":"false"}`)
		if !notRetryable(failed) || metaValue(t, failed, "exitCode") != 1 {
			t.Errorf("failed = %+v", failed)
		}
		slow := execute(t, NewTerminalTool(Options{}), tc, `{"command":"sleep 5","timeout_seconds":0.2}`)
		if !notRetryable(slow) || metaValue(t, slow, "timedOut") != true {
			t.Errorf("slow = %+v", slow)
		}
		if !strings.Contains(slow.Content, "timed out") {
			t.Errorf("slow content = %q", slow.Content)
		}
	})

	t.Run("refuses shell syntax where the sandbox has no shell", func(t *testing.T) {
		_, tc := setup(t) // the in-memory sandbox is argv-only
		out := execute(t, NewTerminalTool(Options{}), tc, `{"command":"echo hi | cat"}`)
		if !notRetryable(out) || !strings.Contains(out.Content, "without a shell") {
			t.Errorf("output = %+v", out)
		}
	})

	t.Run("run_code passes the program on stdin and explains a missing interpreter", func(t *testing.T) {
		_, tc := setup(t)
		out := execute(t, NewRunCodeTool(Options{}), tc, `{"language":"python","code":"print(1)"}`)
		if !strings.Contains(out.Content, "python3 is not installed") {
			t.Errorf("content = %q", out.Content)
		}
		if bad := execute(t, NewRunCodeTool(Options{}), tc, `{"language":"cobol","code":"x"}`); !notRetryable(bad) {
			t.Errorf("unknown language = %+v", bad)
		}
	})

	t.Run("splits simple command lines into argv", func(t *testing.T) {
		for _, tc := range []struct {
			line string
			want []string // nil means the line needs a shell
		}{
			{`git commit -m "first draft" --quiet`, []string{"git", "commit", "-m", "first draft", "--quiet"}},
			{`echo 'a b' c\ d`, []string{"echo", "a b", "c d"}},
			{"ls | wc", nil},
			{"echo $HOME", nil},
			{`echo "unterminated`, nil},
		} {
			if got := splitArgv(tc.line); !slices.Equal(got, tc.want) {
				t.Errorf("splitArgv(%q) = %q, want %q", tc.line, got, tc.want)
			}
		}
	})
}
