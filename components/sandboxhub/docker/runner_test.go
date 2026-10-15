package docker

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Phong95/aec-agent-go/kernel"
)

// runHelper is the program the real-runner tests execute instead of docker (the test
// binary itself, selected by AEC_DOCKER_HELPER); it returns the exit code.
func runHelper(mode string) int {
	switch mode {
	case "cat":
		_, _ = io.Copy(os.Stdout, os.Stdin)
		fmt.Fprint(os.Stderr, "from stderr")
		return 0
	case "exit3":
		fmt.Fprint(os.Stderr, "boom")
		return 3
	case "sleep":
		time.Sleep(time.Minute)
		return 0
	case "probe":
		fmt.Print(os.Getenv("AEC_DOCKER_PROBE") + "|" + strings.Join(os.Args[1:], "|"))
		return 0
	default:
		return 99
	}
}

// helperRunner is a real cliRunner whose "docker" is this test binary in helper mode.
func helperRunner(t *testing.T, mode string) cliRunner {
	t.Helper()
	t.Setenv("AEC_DOCKER_HELPER", mode)
	return cliRunner{binary: os.Args[0]}
}

func TestCliRunnerRunsToCompletion(t *testing.T) {
	r := helperRunner(t, "cat")
	got, err := r.Run(context.Background(), []string{"x"}, RunOptions{Input: "hello é\n", Timeout: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	if got.Code == nil || *got.Code != 0 || got.Stdout != "hello é\n" || got.Stderr != "from stderr" {
		t.Errorf("Run = %+v", got)
	}

	// A failing exit code is a result, not an error.
	r = helperRunner(t, "exit3")
	got, err = r.Run(context.Background(), nil, RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if got.Code == nil || *got.Code != 3 || got.Stderr != "boom" {
		t.Errorf("Run = %+v", got)
	}
}

// TestCliRunnerGivesTheCLITheServerEnvironment: the docker CLI gets the server's
// environment (it may need DOCKER_HOST and PATH); the arguments arrive as given.
func TestCliRunnerGivesTheCLITheServerEnvironment(t *testing.T) {
	t.Setenv("AEC_DOCKER_PROBE", "server-value")
	r := helperRunner(t, "probe")
	got, err := r.Run(context.Background(), []string{"run", "--env", "A B", ""}, RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if want := "server-value|run|--env|A B|"; got.Stdout != want {
		t.Errorf("stdout = %q, want %q", got.Stdout, want)
	}
}

func TestCliRunnerTimeout(t *testing.T) {
	r := helperRunner(t, "sleep")
	start := time.Now()
	_, err := r.Run(context.Background(), []string{"sleep"}, RunOptions{Timeout: 200 * time.Millisecond, SandboxID: "docker:shared"})
	var se *kernel.SandboxError
	if !errors.As(err, &se) || se.Code != kernel.SandboxCodeTimeout || se.SandboxID != "docker:shared" {
		t.Fatalf("Run = %v, want a timeout", err)
	}
	if se.Message != "docker sleep timed out after 200 ms" {
		t.Errorf("message = %q", se.Message)
	}
	if !se.Retryable {
		t.Errorf("a timeout is retryable")
	}
	if elapsed := time.Since(start); elapsed > 30*time.Second {
		t.Errorf("the timeout took %s", elapsed)
	}
}

func TestCliRunnerCancel(t *testing.T) {
	r := helperRunner(t, "sleep")
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(200*time.Millisecond, cancel)
	_, err := r.Run(ctx, []string{"sleep"}, RunOptions{Timeout: time.Minute, SandboxID: "docker:shared"})
	var se *kernel.SandboxError
	if !errors.As(err, &se) || se.Code != kernel.SandboxCodeCancelled || se.Message != "Operation cancelled" {
		t.Errorf("Run = %v, want cancelled", err)
	}
}

// TestCaptureStopsAtTheCap: both streams share the 64 MiB cap and a write is never
// refused, so the pipe keeps draining.
func TestCaptureStopsAtTheCap(t *testing.T) {
	var c capture
	out := captureWriter{c: &c, into: func(c *capture) *[]byte { return &c.out }}
	errW := captureWriter{c: &c, into: func(c *capture) *[]byte { return &c.err }}
	chunk := bytes.Repeat([]byte("x"), 1<<20)
	for range 70 {
		if n, err := out.Write(chunk); n != len(chunk) || err != nil {
			t.Fatalf("Write = %d, %v", n, err)
		}
	}
	if n, err := errW.Write([]byte("late")); n != 4 || err != nil {
		t.Fatalf("Write = %d, %v", n, err)
	}
	if len(c.out) != maxCaptureBytes || len(c.err) != 0 {
		t.Errorf("captured %d and %d bytes, want %d and 0", len(c.out), len(c.err), maxCaptureBytes)
	}
}

func TestCliRunnerStart(t *testing.T) {
	r := helperRunner(t, "cat")
	p, err := r.Start(context.Background(), []string{"x"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(p.Stdin(), "line\n"); err != nil {
		t.Fatal(err)
	}
	if err := p.Stdin().Close(); err != nil {
		t.Fatal(err)
	}
	out, err := io.ReadAll(p.Stdout())
	if err != nil {
		t.Fatal(err)
	}
	errText, err := io.ReadAll(p.Stderr())
	if err != nil {
		t.Fatal(err)
	}
	code, err := p.Wait()
	if err != nil || code == nil || *code != 0 || string(out) != "line\n" || string(errText) != "from stderr" {
		t.Errorf("Start/Wait = %q %q %v %v", out, errText, code, err)
	}

	// Kill ends a client that would run for a minute.
	r = helperRunner(t, "sleep")
	p, err = r.Start(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Kill(); err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, p.Stdout())
	_, _ = io.Copy(io.Discard, p.Stderr())
	if _, err := p.Wait(); err != nil {
		t.Errorf("Wait after Kill: %v", err)
	}

	// A binary that does not exist: the same mapping as Run.
	_, err = cliRunner{binary: missingDocker(t)}.Start(context.Background(), nil)
	var se *kernel.SandboxError
	if !errors.As(err, &se) || se.Code != kernel.SandboxCodeEnvironmentUnavailable {
		t.Errorf("Start on a missing binary = %v", err)
	}
}
