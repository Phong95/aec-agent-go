package aecmock_test

import (
	"context"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"go.uber.org/goleak"

	"github.com/Phong95/aec-agent-go/aecmock"
	"github.com/Phong95/aec-agent-go/components/sandboxhub"
	"github.com/Phong95/aec-agent-go/internal/wiretest"
)

// TestInMemoryExecMatchesGolden replays inmemory-sandbox-exec.json, the recorded rows of
// the mini-exec run with real timers: echo, pwd through a symlinked cwd, printenv,
// cat, rm, the sleep operand, unknown programs, request validation, the
// timeout, the output cap and cancellation. Every row's result fields, error code
// and message, OnOutput pieces and clock-derived duration must match.
func TestInMemoryExecMatchesGolden(t *testing.T) {
	script := loadReplayScript(t, "inmemory-sandbox-exec.json")
	execs := 0
	for _, op := range script.Ops {
		if op.Op == "exec" {
			execs++
		}
	}
	if execs < 120 {
		t.Fatalf("the script has only %d exec rows", execs)
	}
	replay(t, script)
}

// execFixture is a shared sandbox with one agent view and a deterministic clock.
func execFixture(t *testing.T, opts ...aecmock.InMemoryOption) (*aecmock.InMemorySandbox, sandboxhub.View) {
	t.Helper()
	opts = append([]aecmock.InMemoryOption{aecmock.WithClock(wiretest.Clock(1700000000000, 1))}, opts...)
	provider := aecmock.NewInMemoryProvider(opts...)
	env, err := provider.Create(context.Background(), sandboxhub.Spec{Kind: sandboxhub.KindDocker},
		sandboxhub.CreateContext{ID: "docker:shared", Tenancy: sandboxhub.TenancyShared})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	view, err := env.ForAgent(context.Background(), "a1")
	if err != nil {
		t.Fatalf("ForAgent: %v", err)
	}
	sb, ok := provider.Sandbox("docker:shared")
	if !ok {
		t.Fatal("the provider does not know the sandbox it created")
	}
	return sb, view
}

func execRequest(command ...string) sandboxhub.ExecRequest {
	return sandboxhub.ExecRequest{Command: command, TimeoutMs: 5000, MaxOutputBytes: 1000}
}

// TestInMemoryExecCancelledContext: an already-cancelled context is not an error. Nothing runs and the result says
// cancelled, with a nil exit code and no output; the two clock reads still happen.
func TestInMemoryExecCancelledContext(t *testing.T) {
	_, view := execFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var pieces []string
	res, err := view.Exec(ctx, execRequest("echo", "never"), sandboxhub.ExecOptions{
		OnOutput: func(_ sandboxhub.OutputStream, data string) { pieces = append(pieces, data) },
	})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if !res.Cancelled || res.ExitCode != nil || res.Stdout != "" || res.TimedOut || res.Truncated || len(pieces) != 0 {
		t.Errorf("result = %+v pieces = %q, want a cancelled result with no output", res, pieces)
	}
	if res.DurationMs != 1 {
		t.Errorf("DurationMs = %d, want 1 (two clock ticks)", res.DurationMs)
	}

	// A nil context behaves like a background context.
	var nilCtx context.Context //nolint:staticcheck // the nil context is the case under test
	res, err = view.Exec(nilCtx, execRequest("echo", "ok"), sandboxhub.ExecOptions{})
	if err != nil || res.Stdout != "ok\n" {
		t.Errorf("nil context: %+v, %v", res, err)
	}
}

// TestInMemoryExecFirstKillWins: the timer, the context and the output cap each set
// exactly one flag, and the first one to fire wins.
func TestInMemoryExecFirstKillWins(t *testing.T) {
	_, view := execFixture(t)

	res, err := view.Exec(context.Background(), sandboxhub.ExecRequest{Command: []string{"sleep", "10"}, TimeoutMs: 30, MaxOutputBytes: 100}, sandboxhub.ExecOptions{})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if !res.TimedOut || res.Cancelled || res.Truncated || res.ExitCode != nil {
		t.Errorf("timeout: %+v", res)
	}

	res, err = view.Exec(context.Background(), execRequest("yes"), sandboxhub.ExecOptions{})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if !res.Truncated || res.TimedOut || res.Cancelled || res.ExitCode != nil || len(res.Stdout) != 1000 {
		t.Errorf("cap: truncated=%v timedOut=%v cancelled=%v exit=%v len=%d", res.Truncated, res.TimedOut, res.Cancelled, res.ExitCode, len(res.Stdout))
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	res, err = view.Exec(ctx, sandboxhub.ExecRequest{Command: []string{"sleep", "10"}, TimeoutMs: 5000, MaxOutputBytes: 100}, sandboxhub.ExecOptions{})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if !res.Cancelled || res.TimedOut || res.Truncated || res.ExitCode != nil {
		t.Errorf("cancel: %+v", res)
	}
}

// TestInMemoryExecLeavesNoGoroutines runs the three ways a command is killed over and
// over (timeout, output cap, cancel) plus a command that ends on its own, and proves
// that no goroutine, timer callback or context registration outlives an Exec call.
func TestInMemoryExecLeavesNoGoroutines(t *testing.T) {
	defer goleak.VerifyNone(t)
	_, view := execFixture(t)
	for range 20 {
		if res, err := view.Exec(context.Background(), sandboxhub.ExecRequest{Command: []string{"sleep", "10"}, TimeoutMs: 5, MaxOutputBytes: 10}, sandboxhub.ExecOptions{}); err != nil || !res.TimedOut {
			t.Fatalf("timeout run: %+v, %v", res, err)
		}
		if res, err := view.Exec(context.Background(), execRequest("yes"), sandboxhub.ExecOptions{}); err != nil || !res.Truncated {
			t.Fatalf("cap run: %+v, %v", res, err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		go func() {
			time.Sleep(2 * time.Millisecond)
			cancel()
		}()
		res, err := view.Exec(ctx, execRequest("sleep", "10"), sandboxhub.ExecOptions{})
		cancel()
		if err != nil || !res.Cancelled {
			t.Fatalf("cancel run: %+v, %v", res, err)
		}
		if res, err := view.Exec(context.Background(), execRequest("echo", "done"), sandboxhub.ExecOptions{}); err != nil || res.Stdout != "done\n" {
			t.Fatalf("plain run: %+v, %v", res, err)
		}
	}
}

// TestInMemoryExecConcurrentCalls runs commands from several goroutines on one view
// (the mini-exec runs in each caller's goroutine and holds no lock while it runs).
func TestInMemoryExecConcurrentCalls(t *testing.T) {
	sb, view := execFixture(t)
	if err := sb.PlantFile("/workspace/agents/a1/f.txt", "shared\n"); err != nil {
		t.Fatalf("PlantFile: %v", err)
	}
	var wg sync.WaitGroup
	for i := range 12 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			commands := [][]string{{"cat", "f.txt"}, {"echo", "x"}, {"pwd"}, {"sleep", "0.001"}, {"printenv", "A"}}
			req := execRequest(commands[i%len(commands)]...)
			req.Env = map[string]string{"A": "1"}
			if _, err := view.Exec(context.Background(), req, sandboxhub.ExecOptions{}); err != nil {
				t.Errorf("Exec %v: %v", req.Command, err)
			}
		}()
	}
	wg.Wait()
}

// TestInMemoryExecDoesNotMutateTheRequest: Exec never changes the request it was
// given, its argv slice or its environment map.
func TestInMemoryExecDoesNotMutateTheRequest(t *testing.T) {
	sb, view := execFixture(t)
	if err := sb.PlantFile("/workspace/agents/a1/f.txt", "x\n"); err != nil {
		t.Fatalf("PlantFile: %v", err)
	}
	for _, command := range [][]string{
		{"echo", "a", "b"}, {"cat", "f.txt"}, {"rm", "-f", "f.txt"}, {"printenv", "A"}, {"sleep", "0.001"}, {"nosuch", "arg"},
	} {
		shell := false
		req := sandboxhub.ExecRequest{
			Command: command, Shell: &shell, Cwd: ".", Env: map[string]string{"A": "1"}, Stdin: "in",
			TimeoutMs: 5000, MaxOutputBytes: 1000,
		}
		want := sandboxhub.ExecRequest{
			Command: append([]string(nil), command...), Shell: &shell, Cwd: ".", Env: map[string]string{"A": "1"}, Stdin: "in",
			TimeoutMs: 5000, MaxOutputBytes: 1000,
		}
		if _, err := view.Exec(context.Background(), req, sandboxhub.ExecOptions{}); err != nil {
			t.Fatalf("Exec %v: %v", command, err)
		}
		if !reflect.DeepEqual(req, want) {
			t.Errorf("Exec changed the request for %v: %+v", command, req)
		}
	}
}

// TestInMemoryExecOnOutputPieces: OnOutput receives each non-empty piece, in order,
// and the cut piece of a truncated run; the callback runs outside the sandbox lock,
// so it may use the view.
func TestInMemoryExecOnOutputPieces(t *testing.T) {
	sb, view := execFixture(t)
	if err := sb.PlantFile("/workspace/agents/a1/f.txt", "file\n"); err != nil {
		t.Fatalf("PlantFile: %v", err)
	}
	var got []string
	res, err := view.Exec(context.Background(), execRequest("cat", "f.txt", "missing", "f.txt"), sandboxhub.ExecOptions{
		OnOutput: func(stream sandboxhub.OutputStream, data string) {
			got = append(got, string(stream)+":"+data)
			// Calling back into the view from the callback must not deadlock.
			if _, err := view.Stat(context.Background(), "f.txt"); err != nil {
				t.Errorf("Stat from the callback: %v", err)
			}
		},
	})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	want := []string{"stdout:file\n", "stderr:cat: missing: not_found\n", "stdout:file\n"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("pieces = %q, want %q", got, want)
	}
	if res.ExitCode == nil || *res.ExitCode != 1 || !strings.Contains(res.Stderr, "not_found") {
		t.Errorf("result = %+v", res)
	}
}
