package conformance

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Phong95/aec-agent-go/components/sandboxhub"
	"github.com/Phong95/aec-agent-go/conformance/internal/check"
	"github.com/Phong95/aec-agent-go/kernel"
)

// SandboxViewFixture is what a conformance run needs from a sandbox
// implementation. View must be an agent view of a
// SHARED-tenancy sandbox (the default layout: working directory
// /workspace/agents/<id>, roots = the agent's own folder).
//
// The hooks carry everything that is specific to one backend, so the suite itself
// never changes per backend and never weakens an assertion. A nil Symlink skips
// the symlink subtests with a message naming the field.
type SandboxViewFixture struct {
	// View is the agent view under test.
	View sandboxhub.View
	// Symlink creates a symlink at a logical path whose target may point anywhere
	// (omit it if the sandbox cannot; the two symlink subtests are then skipped).
	Symlink func(link, target string) error
	// PlantFile puts a file at any logical path, bypassing the agent's roots (for
	// "an outside file exists" checks).
	PlantFile func(path, content string) error
	// MapExec rewrites every exec request the suite sends, so a backend whose
	// commands differ (a Windows host without POSIX tools) can translate them. Nil
	// means the identity. It must leave an empty or unknown command unchanged: the
	// validation subtests send those on purpose.
	MapExec func(req sandboxhub.ExecRequest) sandboxhub.ExecRequest
	// PwdMatches decides whether the stdout of the pwd command names the working
	// directory (a Windows host prints a native path). Nil means
	// strings.TrimSpace(stdout) == workingDir.
	PwdMatches func(stdout, workingDir string) bool
	// SkipExec, when non-empty, skips every exec subtest with this reason (a
	// backend that cannot run commands).
	SkipExec string
	// SkipFollowInside, when non-empty, skips the "follows symlinks that stay inside
	// the roots" subtest with this reason (a host backend that refuses to follow
	// any symlink).
	SkipFollowInside string
}

// SandboxViewFactory returns a fresh fixture for every subtest. The factory
// registers its own cleanup with t.Cleanup.
type SandboxViewFactory func(t *testing.T) SandboxViewFixture

// sandboxViewTimeout bounds every wait of the SandboxView suite, so a backend that
// ignores cancellation or hangs fails the test instead of hanging it.
const sandboxViewTimeout = 15 * time.Second

// sha256Hex is the hex SHA-256 of the UTF-8 text.
func sha256Hex(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}

// expectCode requires err to be a *kernel.SandboxError with the given code.
func expectCode(t *testing.T, err error, code kernel.SandboxErrorCode) {
	t.Helper()
	var se *kernel.SandboxError
	if !errors.As(err, &se) {
		t.Fatalf("expected SandboxError %q, got %v", code, err)
	}
	check.Equal(t, se.Code, code, "SandboxError code (%v)", err)
}

// SandboxView runs the conformance suite for sandboxhub.View implementations
// (A-59). Every kind - in-memory,
// docker (A-60), host, remote - must pass it unchanged: a backend adapts to the
// suite only through the documented SandboxViewFixture hooks. Each subtest gets
// a fresh fixture from the factory. The subtests named "(Go-only)" prove the
// context contract.
func SandboxView(t *testing.T, factory SandboxViewFactory) {
	t.Helper()

	// run starts one subtest on a fresh fixture. exec marks the subtests that run
	// commands, which SkipExec skips.
	run := func(name string, exec bool, body func(t *testing.T, fx SandboxViewFixture)) {
		t.Run(name, func(t *testing.T) {
			fx := factory(t)
			if fx.View == nil {
				t.Fatal("the fixture has no View")
			}
			if exec && fx.SkipExec != "" {
				t.Skip(fx.SkipExec)
			}
			body(t, fx)
		})
	}

	// -- Shape --

	run("exposes id, traits, limits and a POSIX working directory inside its roots", false, func(t *testing.T, fx SandboxViewFixture) {
		v := fx.View
		check.True(t, v.ID() != "", "the id is empty")
		check.True(t, slices.Contains([]sandboxhub.Kind{sandboxhub.KindDocker, sandboxhub.KindHost, sandboxhub.KindRemote}, v.Traits().Kind),
			"kind %q is not docker, host or remote", v.Traits().Kind)
		check.Equal(t, v.Traits().Tenancy, sandboxhub.TenancyShared)
		wd := v.WorkingDir()
		check.True(t, strings.HasPrefix(wd, "/"), "working directory %q is not absolute POSIX", wd)
		inside := false
		for _, r := range v.Roots() {
			if wd == r || strings.HasPrefix(wd, r+"/") {
				inside = true
			}
		}
		check.True(t, inside, "working directory %q is not inside the roots %v", wd, v.Roots())
		l := v.Limits()
		for name, value := range map[string]int{
			"maxReadBytes": l.MaxReadBytes, "maxWriteBytes": l.MaxWriteBytes, "maxListEntries": l.MaxListEntries,
			"maxSearchMatches": l.MaxSearchMatches, "maxExecTimeoutMs": l.MaxExecTimeoutMs,
			"maxExecOutputBytes": l.MaxExecOutputBytes,
		} {
			check.True(t, value > 0, "limit %s is %d, want > 0", name, value)
		}
	})

	// -- Files --

	run("writes and reads back a file, with matching sha256 (read-after-write)", false, func(t *testing.T, fx SandboxViewFixture) {
		v := fx.View
		ctx := t.Context()
		written, err := v.WriteFile(ctx, "notes/plan.md", "# Plan\nline 2\n", sandboxhub.WriteOptions{})
		check.NoErr(t, err)
		check.True(t, written.Created, "the first write must report created")
		check.Equal(t, written.Path, v.WorkingDir()+"/notes/plan.md")
		check.Equal(t, written.SHA256, sha256Hex("# Plan\nline 2\n"))

		read, err := v.ReadFile(ctx, "notes/plan.md", sandboxhub.ReadOptions{})
		check.NoErr(t, err)
		check.Equal(t, read.Content, "# Plan\nline 2\n")
		check.Equal(t, read.SHA256, written.SHA256)
		check.False(t, read.Truncated, "a full read must not be truncated")

		again, err := v.WriteFile(ctx, "notes/plan.md", "v2", sandboxhub.WriteOptions{})
		check.NoErr(t, err)
		check.False(t, again.Created, "an overwrite must not report created")
	})

	run("reads a line range and caps bytes, flagging truncation", false, func(t *testing.T, fx SandboxViewFixture) {
		v := fx.View
		ctx := t.Context()
		_, err := v.WriteFile(ctx, "lines.txt", "a\nb\nc\nd\n", sandboxhub.WriteOptions{})
		check.NoErr(t, err)
		rng, err := v.ReadFile(ctx, "lines.txt", sandboxhub.ReadOptions{StartLine: intp(2), EndLine: intp(3)})
		check.NoErr(t, err)
		check.Equal(t, rng.Content, "b\nc")
		check.True(t, rng.Truncated, "a line range that cuts the file must be flagged truncated")
		capped, err := v.ReadFile(ctx, "lines.txt", sandboxhub.ReadOptions{MaxBytes: intp(3)})
		check.NoErr(t, err)
		check.Equal(t, capped.Content, "a\nb")
		check.True(t, capped.Truncated, "a byte cap that cuts the file must be flagged truncated")
		check.Equal(t, capped.Size, 8)
	})

	// /workspace/shared is no longer part of the default layout (AgentLayout in
	// sandbox/logical_path.go) - an agent only gets its own folder. If the shared
	// folder comes back, turn this into a collaboration test again.
	run("keeps file ops out of /workspace/shared (not in the agent's roots)", false, func(t *testing.T, fx SandboxViewFixture) {
		ctx := t.Context()
		_, err := fx.View.WriteFile(ctx, "/workspace/shared/export.csv", "id,status\n", sandboxhub.WriteOptions{})
		expectCode(t, err, kernel.SandboxCodePathEscape)
		_, err = fx.View.ReadFile(ctx, "../../shared/export.csv", sandboxhub.ReadOptions{})
		expectCode(t, err, kernel.SandboxCodePathEscape)
	})

	run("rejects writes above the size limit and stale expectedSha256 (conflict)", false, func(t *testing.T, fx SandboxViewFixture) {
		v := fx.View
		ctx := t.Context()
		_, err := v.WriteFile(ctx, "big.txt", strings.Repeat("x", v.Limits().MaxWriteBytes+1), sandboxhub.WriteOptions{})
		expectCode(t, err, kernel.SandboxCodeTooLarge)
		first, err := v.WriteFile(ctx, "doc.txt", "one", sandboxhub.WriteOptions{})
		check.NoErr(t, err)
		_, err = v.WriteFile(ctx, "doc.txt", "two", sandboxhub.WriteOptions{})
		check.NoErr(t, err)
		_, err = v.WriteFile(ctx, "doc.txt", "three", sandboxhub.WriteOptions{ExpectedSHA256: first.SHA256})
		expectCode(t, err, kernel.SandboxCodeConflict)
		ok, err := v.WriteFile(ctx, "doc.txt", "three", sandboxhub.WriteOptions{ExpectedSHA256: sha256Hex("two")})
		check.NoErr(t, err)
		check.Equal(t, ok.SHA256, sha256Hex("three"))
	})

	run("createDirs: false refuses to create missing parents", false, func(t *testing.T, fx SandboxViewFixture) {
		_, err := fx.View.WriteFile(t.Context(), "missing/dir/file.txt", "x", sandboxhub.WriteOptions{CreateDirs: boolp(false)})
		expectCode(t, err, kernel.SandboxCodeNotFound)
	})

	run("stat returns type/size, or null for a missing path", false, func(t *testing.T, fx SandboxViewFixture) {
		v := fx.View
		ctx := t.Context()
		_, err := v.WriteFile(ctx, "s/file.txt", "hello", sandboxhub.WriteOptions{})
		check.NoErr(t, err)
		file, err := v.Stat(ctx, "s/file.txt")
		check.NoErr(t, err)
		if file == nil {
			t.Fatal("stat of an existing file returned nil")
		}
		check.Equal(t, file.Type, sandboxhub.EntryFile)
		check.Equal(t, file.Size, 5)
		dir, err := v.Stat(ctx, "s")
		check.NoErr(t, err)
		if dir == nil {
			t.Fatal("stat of an existing directory returned nil")
		}
		check.Equal(t, dir.Type, sandboxhub.EntryDirectory)
		missing, err := v.Stat(ctx, "nope.txt")
		check.NoErr(t, err)
		check.True(t, missing == nil, "stat of a missing path must return nil, got %+v", missing)
	})

	run("reports not_found and invalid_path", false, func(t *testing.T, fx SandboxViewFixture) {
		v := fx.View
		ctx := t.Context()
		_, err := v.ReadFile(ctx, "nope.txt", sandboxhub.ReadOptions{})
		expectCode(t, err, kernel.SandboxCodeNotFound)
		_, err = v.WriteFile(ctx, "d/f.txt", "x", sandboxhub.WriteOptions{})
		check.NoErr(t, err)
		_, err = v.ReadFile(ctx, "d", sandboxhub.ReadOptions{})
		expectCode(t, err, kernel.SandboxCodeInvalidPath)
		_, err = v.List(ctx, "d/f.txt", sandboxhub.ListOptions{})
		expectCode(t, err, kernel.SandboxCodeInvalidPath)
	})

	run("lists a directory (flat, recursive, hidden, capped)", false, func(t *testing.T, fx SandboxViewFixture) {
		v := fx.View
		ctx := t.Context()
		for p, content := range map[string]string{"l/a.txt": "1", "l/b/c.txt": "2", "l/.hidden": "3"} {
			_, err := v.WriteFile(ctx, p, content, sandboxhub.WriteOptions{})
			check.NoErr(t, err)
		}
		flat, err := v.List(ctx, "l", sandboxhub.ListOptions{})
		check.NoErr(t, err)
		names := entryNames(flat.Entries)
		slices.Sort(names)
		check.DeepEqual(t, names, []string{"a.txt", "b"})
		deep, err := v.List(ctx, "l", sandboxhub.ListOptions{Recursive: boolp(true)})
		check.NoErr(t, err)
		paths := make([]string, 0, len(deep.Entries))
		for _, e := range deep.Entries {
			paths = append(paths, e.Path)
		}
		check.True(t, slices.Contains(paths, v.WorkingDir()+"/l/b/c.txt"), "recursive listing %v lacks l/b/c.txt", paths)
		hidden, err := v.List(ctx, "l", sandboxhub.ListOptions{IncludeHidden: boolp(true)})
		check.NoErr(t, err)
		check.True(t, slices.Contains(entryNames(hidden.Entries), ".hidden"), "listing %v lacks .hidden", entryNames(hidden.Entries))
		capped, err := v.List(ctx, "l", sandboxhub.ListOptions{Recursive: boolp(true), MaxEntries: intp(1)})
		check.NoErr(t, err)
		check.Len(t, capped.Entries, 1)
		check.True(t, capped.Truncated, "a capped listing must be flagged truncated")
	})

	run("searches file contents (literal, case-insensitive, regex, glob, capped)", false, func(t *testing.T, fx SandboxViewFixture) {
		v := fx.View
		ctx := t.Context()
		_, err := v.WriteFile(ctx, "src/a.ts", "const Rebar = 1;\nrebar spacing\n", sandboxhub.WriteOptions{})
		check.NoErr(t, err)
		_, err = v.WriteFile(ctx, "src/b.md", "Rebar notes\n", sandboxhub.WriteOptions{})
		check.NoErr(t, err)
		wd := v.WorkingDir()

		literal, err := v.Search(ctx, sandboxhub.SearchQuery{Pattern: "rebar", Path: "src"})
		check.NoErr(t, err)
		check.DeepEqual(t, literal.Matches, []sandboxhub.SearchMatch{{Path: wd + "/src/a.ts", Line: 2, Text: "rebar spacing"}})
		ci, err := v.Search(ctx, sandboxhub.SearchQuery{Pattern: "rebar", Path: "src", CaseSensitive: boolp(false)})
		check.NoErr(t, err)
		check.Len(t, ci.Matches, 3)
		regex, err := v.Search(ctx, sandboxhub.SearchQuery{Pattern: `^const \w+`, Path: "src", Literal: boolp(false)})
		check.NoErr(t, err)
		check.Len(t, regex.Matches, 1)
		glob, err := v.Search(ctx, sandboxhub.SearchQuery{Pattern: "Rebar", Path: "src", Glob: "*.md"})
		check.NoErr(t, err)
		globPaths := make([]string, 0, len(glob.Matches))
		for _, m := range glob.Matches {
			globPaths = append(globPaths, m.Path)
		}
		check.DeepEqual(t, globPaths, []string{wd + "/src/b.md"})
		capped, err := v.Search(ctx, sandboxhub.SearchQuery{Pattern: "rebar", Path: "src", CaseSensitive: boolp(false), MaxMatches: intp(1)})
		check.NoErr(t, err)
		check.True(t, capped.Truncated, "a capped search must be flagged truncated")
		check.Len(t, capped.Matches, 1)
		_, err = v.Search(ctx, sandboxhub.SearchQuery{Pattern: "(", Literal: boolp(false)})
		expectCode(t, err, kernel.SandboxCodeInvalidArgument)
	})

	// -- Path containment --

	for _, p := range []string{
		"../../../etc/passwd",
		"/etc/passwd",
		"/workspace/agents/some-other-agent/secret.txt",
		"../other-agent/file.txt",
		"C:/Windows/win.ini",
		"C:\\Windows\\win.ini",
	} {
		run("blocks path escape: "+p, false, func(t *testing.T, fx SandboxViewFixture) {
			ctx := t.Context()
			_, err := fx.View.ReadFile(ctx, p, sandboxhub.ReadOptions{})
			expectCode(t, err, kernel.SandboxCodePathEscape)
			_, err = fx.View.WriteFile(ctx, p, "x", sandboxhub.WriteOptions{})
			expectCode(t, err, kernel.SandboxCodePathEscape)
			_, err = fx.View.List(ctx, p, sandboxhub.ListOptions{})
			expectCode(t, err, kernel.SandboxCodePathEscape)
		})
	}

	run("rejects NUL bytes in paths", false, func(t *testing.T, fx SandboxViewFixture) {
		_, err := fx.View.ReadFile(t.Context(), "a\x00b", sandboxhub.ReadOptions{})
		expectCode(t, err, kernel.SandboxCodeInvalidPath)
	})

	run("blocks symlinks that point outside the roots", false, func(t *testing.T, fx SandboxViewFixture) {
		if fx.Symlink == nil {
			t.Skip("SandboxViewFixture.Symlink is nil: the backend cannot create symlinks")
		}
		v := fx.View
		ctx := t.Context()
		if fx.PlantFile != nil {
			check.NoErr(t, fx.PlantFile("/workspace/agents/victim/secret.txt", "top secret"))
		}
		check.NoErr(t, fx.Symlink(v.WorkingDir()+"/escape", "/workspace/agents/victim"))
		_, err := v.ReadFile(ctx, "escape/secret.txt", sandboxhub.ReadOptions{})
		expectCode(t, err, kernel.SandboxCodePathEscape)
		_, err = v.WriteFile(ctx, "escape/new.txt", "x", sandboxhub.WriteOptions{})
		expectCode(t, err, kernel.SandboxCodePathEscape)
		check.NoErr(t, fx.Symlink(v.WorkingDir()+"/to-etc", "/etc"))
		_, err = v.List(ctx, "to-etc", sandboxhub.ListOptions{})
		expectCode(t, err, kernel.SandboxCodePathEscape)
	})

	run("follows symlinks that stay inside the roots", false, func(t *testing.T, fx SandboxViewFixture) {
		if fx.Symlink == nil {
			t.Skip("SandboxViewFixture.Symlink is nil: the backend cannot create symlinks")
		}
		if fx.SkipFollowInside != "" {
			t.Skip(fx.SkipFollowInside)
		}
		v := fx.View
		ctx := t.Context()
		_, err := v.WriteFile(ctx, v.WorkingDir()+"/data/data.txt", "linked data", sandboxhub.WriteOptions{})
		check.NoErr(t, err)
		check.NoErr(t, fx.Symlink(v.WorkingDir()+"/link-to-data", v.WorkingDir()+"/data"))
		read, err := v.ReadFile(ctx, "link-to-data/data.txt", sandboxhub.ReadOptions{})
		check.NoErr(t, err)
		check.Equal(t, read.Content, "linked data")
	})

	run("fails fs operations with an already-aborted signal (cancelled)", false, func(t *testing.T, fx SandboxViewFixture) {
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		_, err := fx.View.ReadFile(ctx, "x.txt", sandboxhub.ReadOptions{})
		expectCode(t, err, kernel.SandboxCodeCancelled)
	})

	// -- Commands --

	// execReq is a request with the generous limits every exec test uses.
	execReq := func(command ...string) sandboxhub.ExecRequest {
		return sandboxhub.ExecRequest{Command: command, TimeoutMs: 5_000, MaxOutputBytes: 10_000}
	}

	run("runs argv commands in the working directory", true, func(t *testing.T, fx SandboxViewFixture) {
		echo, err := runExec(t.Context(), t, fx, execReq("echo", "hello", "world"), sandboxhub.ExecOptions{})
		check.NoErr(t, err)
		check.True(t, exitedWith(echo, 0), "echo exit code %v, want 0", echo.ExitCode)
		check.Equal(t, echo.Stdout, "hello world\n")
		check.False(t, echo.TimedOut, "echo must not time out")
		check.False(t, echo.Cancelled, "echo must not be cancelled")
		check.False(t, echo.Truncated, "echo must not be truncated")
		pwd, err := runExec(t.Context(), t, fx, execReq("pwd"), sandboxhub.ExecOptions{})
		check.NoErr(t, err)
		matches := fx.PwdMatches
		if matches == nil {
			matches = func(stdout, workingDir string) bool { return strings.TrimSpace(stdout) == workingDir }
		}
		check.True(t, matches(pwd.Stdout, fx.View.WorkingDir()), "pwd printed %q, working directory is %q", pwd.Stdout, fx.View.WorkingDir())
	})

	run("reports non-zero exit codes without throwing", true, func(t *testing.T, fx SandboxViewFixture) {
		r, err := runExec(t.Context(), t, fx, execReq("false"), sandboxhub.ExecOptions{})
		check.NoErr(t, err)
		check.True(t, exitedWith(r, 1), "false exit code %v, want 1", r.ExitCode)
	})

	run("runs in a relative cwd and rejects cwd escapes", true, func(t *testing.T, fx SandboxViewFixture) {
		ctx := t.Context()
		_, err := fx.View.WriteFile(ctx, "proj/readme.txt", "hi", sandboxhub.WriteOptions{})
		check.NoErr(t, err)
		req := execReq("cat", "readme.txt")
		req.Cwd = "proj"
		r, err := runExec(ctx, t, fx, req, sandboxhub.ExecOptions{})
		check.NoErr(t, err)
		check.True(t, exitedWith(r, 0), "cat exit code %v, want 0", r.ExitCode)
		check.Equal(t, r.Stdout, "hi")
		escape := execReq("pwd")
		escape.Cwd = "/etc"
		_, err = runExec(ctx, t, fx, escape, sandboxhub.ExecOptions{})
		expectCode(t, err, kernel.SandboxCodePathEscape)
	})

	run("does not inherit the server's environment variables", true, func(t *testing.T, fx SandboxViewFixture) {
		// t.Setenv makes this subtest non-parallel, which the process-wide variable needs.
		t.Setenv("AEC_CONFORMANCE_SECRET", "leaked")
		hidden, err := runExec(t.Context(), t, fx, execReq("printenv", "AEC_CONFORMANCE_SECRET"), sandboxhub.ExecOptions{})
		check.NoErr(t, err)
		check.False(t, strings.Contains(hidden.Stdout, "leaked"), "the server's variable leaked into the command: %q", hidden.Stdout)
		req := execReq("printenv", "GREETING")
		req.Env = map[string]string{"GREETING": "hi"}
		passed, err := runExec(t.Context(), t, fx, req, sandboxhub.ExecOptions{})
		check.NoErr(t, err)
		check.Equal(t, strings.TrimSpace(passed.Stdout), "hi")
	})

	run("kills a command at its timeout", true, func(t *testing.T, fx SandboxViewFixture) {
		req := execReq("sleep", "10")
		req.TimeoutMs = 200
		r, err := runExec(t.Context(), t, fx, req, sandboxhub.ExecOptions{})
		check.NoErr(t, err)
		check.True(t, r.TimedOut, "the command must be reported as timed out")
		check.True(t, r.ExitCode == nil, "a killed command has no exit code, got %v", r.ExitCode)
		check.True(t, r.DurationMs < 5_000, "the command ran %d ms, want < 5000", r.DurationMs)
	})

	run("kills a command that exceeds the output cap", true, func(t *testing.T, fx SandboxViewFixture) {
		req := execReq("yes")
		req.TimeoutMs = 10_000
		req.MaxOutputBytes = 1_000
		r, err := runExec(t.Context(), t, fx, req, sandboxhub.ExecOptions{})
		check.NoErr(t, err)
		check.True(t, r.Truncated, "the command must be reported as truncated")
		check.True(t, r.ExitCode == nil, "a killed command has no exit code, got %v", r.ExitCode)
		check.True(t, len(r.Stdout)+len(r.Stderr) <= 1_000, "output is %d bytes, want <= 1000", len(r.Stdout)+len(r.Stderr))
	})

	run("cancels a running command via AbortSignal and streams output", true, func(t *testing.T, fx SandboxViewFixture) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		timer := time.AfterFunc(100*time.Millisecond, cancel)
		defer timer.Stop()
		r, err := runExec(ctx, t, fx, execReq("sleep", "10"), sandboxhub.ExecOptions{})
		check.NoErr(t, err)
		check.True(t, r.Cancelled, "the command must be reported as cancelled")
		check.True(t, r.ExitCode == nil, "a killed command has no exit code, got %v", r.ExitCode)

		var out outputCollector
		_, err = runExec(t.Context(), t, fx, execReq("echo", "streamed"), sandboxhub.ExecOptions{OnOutput: out.collect})
		check.NoErr(t, err)
		check.True(t, strings.Contains(out.joined(), "streamed"), "the live output %q lacks the echoed text", out.joined())
	})

	run("validates exec requests", true, func(t *testing.T, fx SandboxViewFixture) {
		ctx := t.Context()
		_, err := runExec(ctx, t, fx, sandboxhub.ExecRequest{Command: []string{}, TimeoutMs: 1_000, MaxOutputBytes: 1_000}, sandboxhub.ExecOptions{})
		expectCode(t, err, kernel.SandboxCodeInvalidArgument)
		_, err = runExec(ctx, t, fx, sandboxhub.ExecRequest{Command: []string{"echo"}, TimeoutMs: 0, MaxOutputBytes: 1_000}, sandboxhub.ExecOptions{})
		expectCode(t, err, kernel.SandboxCodeInvalidArgument)
		if !slices.Contains(fx.View.Capabilities(), sandboxhub.CapabilityExecShell) {
			_, err = runExec(ctx, t, fx,
				sandboxhub.ExecRequest{Command: []string{"echo hi"}, Shell: boolp(true), TimeoutMs: 1_000, MaxOutputBytes: 1_000},
				sandboxhub.ExecOptions{})
			expectCode(t, err, kernel.SandboxCodeCapabilityUnsupported)
		}
	})

	// -- Go-only: the context contract --

	run("cancelled context fails file operations (Go-only)", false, func(t *testing.T, fx SandboxViewFixture) {
		v := fx.View
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		_, err := v.ReadFile(ctx, "x.txt", sandboxhub.ReadOptions{})
		expectCode(t, err, kernel.SandboxCodeCancelled)
		_, err = v.WriteFile(ctx, "x.txt", "x", sandboxhub.WriteOptions{})
		expectCode(t, err, kernel.SandboxCodeCancelled)
		_, err = v.Stat(ctx, "x.txt")
		expectCode(t, err, kernel.SandboxCodeCancelled)
		_, err = v.List(ctx, ".", sandboxhub.ListOptions{})
		expectCode(t, err, kernel.SandboxCodeCancelled)
		_, err = v.Search(ctx, sandboxhub.SearchQuery{Pattern: "x"})
		expectCode(t, err, kernel.SandboxCodeCancelled)
	})

	run("cancelled context cancels exec (Go-only)", true, func(t *testing.T, fx SandboxViewFixture) {
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		r, err := runExec(ctx, t, fx, execReq("sleep", "10"), sandboxhub.ExecOptions{})
		if err != nil {
			// A backend may refuse to start the command, with the cancelled error.
			expectCode(t, err, kernel.SandboxCodeCancelled)
			return
		}
		check.True(t, r.Cancelled, "a command started with a cancelled context must report cancelled")
		check.True(t, r.ExitCode == nil, "a cancelled command has no exit code, got %v", r.ExitCode)
	})

	run("concurrent writes to different files (Go-only)", false, func(t *testing.T, fx SandboxViewFixture) {
		const n = 16
		v := fx.View
		ctx := t.Context()
		var wg sync.WaitGroup
		errs := make([]error, n)
		for i := range n {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_, errs[i] = v.WriteFile(ctx, fmt.Sprintf("concurrent/file-%02d.txt", i), fmt.Sprintf("content %d", i), sandboxhub.WriteOptions{})
			}()
		}
		done := make(chan struct{})
		go func() {
			wg.Wait()
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(sandboxViewTimeout):
			t.Fatalf("the %d concurrent writes did not finish within %v", n, sandboxViewTimeout)
		}
		for i := range n {
			check.NoErr(t, errs[i], "write %d", i)
		}
		for i := range n {
			read, err := v.ReadFile(ctx, fmt.Sprintf("concurrent/file-%02d.txt", i), sandboxhub.ReadOptions{})
			check.NoErr(t, err, "read %d", i)
			check.Equal(t, read.Content, fmt.Sprintf("content %d", i))
		}
	})

	run("exec leaves the request untouched (Go-only)", true, func(t *testing.T, fx SandboxViewFixture) {
		req := mapExecRequest(fx, execReq("printenv", "GREETING"))
		req.Env = map[string]string{"GREETING": "hi"}
		wantCommand := slices.Clone(req.Command)
		wantEnv := maps.Clone(req.Env)
		_, err := execBounded(t.Context(), t, fx.View, req, sandboxhub.ExecOptions{})
		check.NoErr(t, err)
		check.True(t, slices.Equal(req.Command, wantCommand), "Exec changed Command to %v, was %v", req.Command, wantCommand)
		check.True(t, maps.Equal(req.Env, wantEnv), "Exec changed Env to %v, was %v", req.Env, wantEnv)
	})

	run("capabilities and roots are copies (Go-only)", false, func(t *testing.T, fx SandboxViewFixture) {
		v := fx.View
		caps, roots := v.Capabilities(), v.Roots()
		wantCaps, wantRoots := slices.Clone(caps), slices.Clone(roots)
		for i := range caps {
			caps[i] = "tampered"
		}
		for i := range roots {
			roots[i] = "/tampered"
		}
		check.True(t, slices.Equal(v.Capabilities(), wantCaps), "Capabilities() changed to %v, was %v", v.Capabilities(), wantCaps)
		check.True(t, slices.Equal(v.Roots(), wantRoots), "Roots() changed to %v, was %v", v.Roots(), wantRoots)
	})
}

// intp and boolp make the optional-field pointers of the option structs.
func intp(v int) *int    { return &v }
func boolp(v bool) *bool { return &v }

// entryNames lists the names of directory entries.
func entryNames(entries []sandboxhub.DirEntry) []string {
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name)
	}
	return names
}

// exitedWith reports whether the command exited with the given code.
func exitedWith(r sandboxhub.ExecResult, code int) bool {
	return r.ExitCode != nil && *r.ExitCode == code
}

// mapExecRequest applies the fixture's MapExec hook (nil is the identity).
func mapExecRequest(fx SandboxViewFixture, req sandboxhub.ExecRequest) sandboxhub.ExecRequest {
	if fx.MapExec == nil {
		return req
	}
	return fx.MapExec(req)
}

// execBounded runs one Exec on its own goroutine and fails the test when it does
// not return within sandboxViewTimeout, so a backend that ignores cancellation or
// a timeout fails the suite instead of hanging it.
func execBounded(ctx context.Context, t *testing.T, v sandboxhub.View, req sandboxhub.ExecRequest, opts sandboxhub.ExecOptions) (sandboxhub.ExecResult, error) {
	t.Helper()
	type outcome struct {
		res sandboxhub.ExecResult
		err error
	}
	done := make(chan outcome, 1)
	go func() {
		res, err := v.Exec(ctx, req, opts)
		done <- outcome{res, err}
	}()
	select {
	case o := <-done:
		return o.res, o.err
	case <-time.After(sandboxViewTimeout):
		t.Fatalf("Exec of %v did not return within %v", req.Command, sandboxViewTimeout)
		return sandboxhub.ExecResult{}, nil
	}
}

// runExec is execBounded after the fixture's MapExec hook.
func runExec(ctx context.Context, t *testing.T, fx SandboxViewFixture, req sandboxhub.ExecRequest, opts sandboxhub.ExecOptions) (sandboxhub.ExecResult, error) {
	t.Helper()
	return execBounded(ctx, t, fx.View, mapExecRequest(fx, req), opts)
}

// outputCollector gathers the live output pieces of OnOutput. A backend may call
// OnOutput from the goroutines that copy the process streams, so it is locked.
type outputCollector struct {
	mu     sync.Mutex
	pieces []string
}

func (c *outputCollector) collect(_ sandboxhub.OutputStream, data string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.pieces = append(c.pieces, data)
}

func (c *outputCollector) joined() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return strings.Join(c.pieces, "")
}
