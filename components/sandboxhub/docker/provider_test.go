package docker

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Phong95/aec-agent-go/components/sandboxhub"
	"github.com/Phong95/aec-agent-go/kernel"
)

// The five provider tests that need no Docker ("DockerSandboxProvider (no Docker
// needed)"). The three "SandboxHub data retention" tests run against the in-memory
// provider and live in sandbox/hub_inmemory_test.go. None of these tests talks to a docker daemon: the
// missing-CLI tests run the real CLI runner on a binary that does not exist.

// missingDocker is a path that cannot be a docker binary: it is inside a fresh temp
// folder and nothing is created there.
func missingDocker(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "aec-no-such-docker-binary")
}

func TestDockerSandboxProviderFailsClosedWithARetryableEnvironmentUnavailableWhenTheDockerCLIIsMissing(t *testing.T) {
	provider, err := New(Options{DockerPath: missingDocker(t)})
	if err != nil {
		t.Fatal(err)
	}
	hub := sandboxhub.NewSandboxHub()
	if err := hub.Register(provider); err != nil {
		t.Fatal(err)
	}
	if err := hub.ConfigureShared(sandboxhub.Spec{Kind: sandboxhub.KindDocker}); err != nil {
		t.Fatal(err)
	}

	_, err = hub.Resolve(context.Background(), "a1", sandboxhub.Selection{Mode: sandboxhub.SelectionShared})
	var se *kernel.SandboxError
	if !errors.As(err, &se) {
		t.Fatalf("Resolve error = %v, want a SandboxError", err)
	}
	if se.Code != kernel.SandboxCodeEnvironmentUnavailable {
		t.Errorf("code = %s, want environment_unavailable", se.Code)
	}
	if !se.Retryable {
		t.Errorf("the error is not retryable")
	}
	if !strings.Contains(se.Message, "Docker is not available (is Docker running?)") || !strings.Contains(se.Message, "was not found") {
		t.Errorf("message = %q", se.Message)
	}
}

func TestDockerSandboxProviderReportsDockerAsUnavailableWithoutThrowing(t *testing.T) {
	provider, err := New(Options{DockerPath: missingDocker(t)})
	if err != nil {
		t.Fatal(err)
	}
	status := provider.CheckAvailable(context.Background())
	if status.OK || status.Reason == "" || status.Version != "" {
		t.Errorf("CheckAvailable() = %+v, want not ok with a reason", status)
	}
}

func TestDockerSandboxProviderRejectsNetworkAllowlistsBeforeTouchingDocker(t *testing.T) {
	// A runner that fails the test on any call proves docker is never touched; the
	// binary path is missing as well.
	var calls atomic.Int32
	provider, err := New(Options{Runner: funcRunner{run: func(context.Context, []string, RunOptions) (RunResult, error) {
		calls.Add(1)
		return RunResult{}, errors.New("docker must not be called")
	}}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = provider.Create(context.Background(),
		sandboxhub.Spec{Kind: sandboxhub.KindDocker, Network: sandboxhub.NetworkAllowlist},
		sandboxhub.CreateContext{ID: "docker:own:a1", Tenancy: sandboxhub.TenancyOwn, AgentID: "a1"})
	var se *kernel.SandboxError
	if !errors.As(err, &se) || se.Code != kernel.SandboxCodePolicyDenied {
		t.Fatalf("Create error = %v, want policy_denied", err)
	}
	if calls.Load() != 0 {
		t.Errorf("docker was called %d times", calls.Load())
	}

	// The same through the real runner on a missing binary.
	onMissing, err := New(Options{DockerPath: missingDocker(t)})
	if err != nil {
		t.Fatal(err)
	}
	_, err = onMissing.Create(context.Background(),
		sandboxhub.Spec{Kind: sandboxhub.KindDocker, Network: sandboxhub.NetworkAllowlist},
		sandboxhub.CreateContext{ID: "docker:own:a1", Tenancy: sandboxhub.TenancyOwn, AgentID: "a1"})
	if !errors.As(err, &se) || se.Code != kernel.SandboxCodePolicyDenied {
		t.Fatalf("Create error = %v, want policy_denied", err)
	}
}

func TestDockerSandboxProviderNamesContainersAndVolumesPerSandbox(t *testing.T) {
	provider, err := New(Options{NamePrefix: "aec-sandbox"})
	if err != nil {
		t.Fatal(err)
	}
	if c, _ := provider.NamesFor("docker:shared"); c != "aec-sandbox-shared" {
		t.Errorf("shared container = %q", c)
	}
	if _, v := provider.NamesFor("docker:own:agent-1"); v != "aec-sandbox-own-agent-1" {
		t.Errorf("own volume = %q", v)
	}
	if c, _ := provider.NamesFor("docker:revit-box"); c != "aec-sandbox-env-revit-box" {
		t.Errorf("named container = %q", c)
	}
}

func TestDockerSandboxProviderValidatesHostSettings(t *testing.T) {
	if _, err := New(Options{User: "root"}); err == nil {
		t.Errorf("New accepted the user \"root\"")
	}
	if _, err := New(Options{NamePrefix: "bad name"}); err == nil {
		t.Errorf("New accepted the name prefix \"bad name\"")
	}
}

// -- Runner, image and naming tests --

// TestCliRunnerMapsStartFailures: the real runner reports a missing binary and a
// non-executable one as start failures, and a timeout and a cancel as run failures.
func TestCliRunnerMapsStartFailures(t *testing.T) {
	missing := missingDocker(t)
	_, err := cliRunner{binary: missing}.Run(context.Background(), []string{"version"}, RunOptions{SandboxID: "docker:shared"})
	var se *kernel.SandboxError
	if !errors.As(err, &se) || se.Code != kernel.SandboxCodeEnvironmentUnavailable || se.SandboxID != "docker:shared" {
		t.Fatalf("Run on a missing binary = %v", err)
	}
	if want := "Docker CLI \"" + missing + "\" was not found \U00002014 install Docker to use sandboxes"; se.Message != want {
		t.Errorf("message = %q, want %q", se.Message, want)
	}

	// A bare name that is not on the PATH is exec.ErrNotFound.
	_, err = cliRunner{binary: "aec-no-such-docker-binary"}.Run(context.Background(), nil, RunOptions{})
	if !errors.As(err, &se) || !strings.HasPrefix(se.Message, "Docker CLI \"aec-no-such-docker-binary\" was not found") {
		t.Errorf("Run on a bare missing name = %v", err)
	}

	// A folder is not executable: another start failure (Go reports it as not found on
	// Windows and as a permission error elsewhere; both are environment_unavailable).
	_, err = cliRunner{binary: t.TempDir()}.Run(context.Background(), nil, RunOptions{})
	if !errors.As(err, &se) || se.Code != kernel.SandboxCodeEnvironmentUnavailable ||
		(!strings.HasPrefix(se.Message, "Could not run the Docker CLI: ") && !strings.Contains(se.Message, "was not found")) {
		t.Errorf("Run on a folder = %v", err)
	}

	// A call that is already cancelled starts nothing.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = cliRunner{binary: missing}.Run(ctx, nil, RunOptions{SandboxID: "docker:shared"})
	if !errors.As(err, &se) || se.Code != kernel.SandboxCodeCancelled || se.Message != "Operation cancelled" {
		t.Errorf("Run with a cancelled context = %v", err)
	}
}

// TestProviderSharesOneImagePreparation: concurrent EnsureImage calls for one image
// make one shared preparation and every caller gets its outcome.
func TestProviderSharesOneImagePreparation(t *testing.T) {
	var inspects atomic.Int32
	release := make(chan struct{})
	entered := make(chan struct{}, 1)
	provider, err := New(Options{Runner: funcRunner{run: func(_ context.Context, args []string, _ RunOptions) (RunResult, error) {
		if len(args) > 0 && args[0] == "image" {
			inspects.Add(1)
			entered <- struct{}{}
			<-release
		}
		zero := 0
		return RunResult{Code: &zero}, nil
	}}})
	if err != nil {
		t.Fatal(err)
	}
	const callers = 5
	errs := make(chan error, callers)
	var wg sync.WaitGroup
	for range callers {
		wg.Go(func() { errs <- provider.EnsureImage(context.Background(), "node:22", "") })
	}
	<-entered
	// The other callers are waiting on the shared preparation; give them time to
	// arrive, then let it finish.
	time.Sleep(50 * time.Millisecond)
	close(release)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Errorf("EnsureImage: %v", err)
		}
	}
	if inspects.Load() != 1 {
		t.Errorf("image inspect ran %d times, want 1", inspects.Load())
	}
}

// TestEnsureImageWaiterStopsWithItsContext: a caller that waits for another caller's
// preparation returns "Operation cancelled" when its own context ends.
func TestEnsureImageWaiterStopsWithItsContext(t *testing.T) {
	release := make(chan struct{})
	entered := make(chan struct{}, 1)
	provider, err := New(Options{Runner: funcRunner{run: func(context.Context, []string, RunOptions) (RunResult, error) {
		entered <- struct{}{}
		<-release
		zero := 0
		return RunResult{Code: &zero}, nil
	}}})
	if err != nil {
		t.Fatal(err)
	}
	first := make(chan error, 1)
	go func() { first <- provider.EnsureImage(context.Background(), "node:22", "docker:shared") }()
	<-entered

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err = provider.EnsureImage(ctx, "node:22", "docker:shared")
	var se *kernel.SandboxError
	if !errors.As(err, &se) || se.Code != kernel.SandboxCodeCancelled {
		t.Errorf("a waiter with a cancelled context got %v, want cancelled", err)
	}
	close(release)
	if err := <-first; err != nil {
		t.Errorf("the first caller: %v", err)
	}
}

// TestRecoveryIsShared: operations that find the container down while a recovery runs
// wait for it and share its outcome (one container start, not one per operation).
func TestRecoveryIsShared(t *testing.T) {
	var (
		mu       sync.Mutex
		inspects int
		runs     int
	)
	release := make(chan struct{})
	entered := make(chan struct{}, 8)
	zero, one := 0, 1
	runner := funcRunner{run: func(_ context.Context, args []string, opts RunOptions) (RunResult, error) {
		switch {
		case len(args) >= 2 && args[0] == "container" && args[1] == "inspect":
			mu.Lock()
			inspects++
			n := inspects
			mu.Unlock()
			if n == 1 {
				// the first recovery: hold it until the others joined
				entered <- struct{}{}
				<-release
			}
			return RunResult{Code: &one, Stderr: "Error: No such container"}, nil
		case len(args) > 0 && args[0] == "run":
			mu.Lock()
			runs++
			mu.Unlock()
		case len(args) > 0 && args[0] == "exec" && strings.Contains(opts.Input, `"op":"stat"`):
			return RunResult{Code: &zero, Stdout: `{"ok":true,"result":null}` + "\n"}, nil
		}
		return RunResult{Code: &zero, Stdout: `{"ok":true,"result":{}}` + "\n"}, nil
	}}
	// The first stat of each caller finds the container down (a flag flips once the
	// recovery ran), the retry succeeds.
	var recovered atomic.Bool
	wrapped := funcRunner{run: func(ctx context.Context, args []string, opts RunOptions) (RunResult, error) {
		if len(args) > 0 && args[0] == "exec" && strings.Contains(opts.Input, `"op":"stat"`) && !recovered.Load() {
			return RunResult{Code: &one, Stderr: "Error response from daemon: No such container: x"}, nil
		}
		if len(args) > 0 && args[0] == "run" {
			recovered.Store(true)
		}
		return runner.run(ctx, args, opts)
	}}
	provider, err := New(Options{Runner: wrapped})
	if err != nil {
		t.Fatal(err)
	}
	sb, err := newSandbox(provider, sandboxhub.Spec{Kind: sandboxhub.KindDocker}, sandboxhub.CreateContext{ID: "docker:own:a1", Tenancy: sandboxhub.TenancyOwn, AgentID: "a1"})
	if err != nil {
		t.Fatal(err)
	}
	v, err := sb.ForAgent(context.Background(), "a1")
	if err != nil {
		t.Fatal(err)
	}

	const callers = 4
	errs := make(chan error, callers)
	var wg sync.WaitGroup
	for range callers {
		wg.Go(func() {
			_, err := v.Stat(context.Background(), "x.txt")
			errs <- err
		})
	}
	<-entered
	if h := sb.Health(context.Background()); h != sandboxhub.HealthStarting {
		t.Errorf("Health while recovering = %s, want starting", h)
	}
	time.Sleep(50 * time.Millisecond) // let the other callers join the recovery
	close(release)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Errorf("Stat: %v", err)
		}
	}
	mu.Lock()
	gotRuns := runs
	mu.Unlock()
	if gotRuns != 1 {
		t.Errorf("docker run was called %d times, want 1 (the recovery is shared)", gotRuns)
	}
	if h := sb.Health(context.Background()); h == sandboxhub.HealthStarting {
		t.Errorf("Health after the recovery = %s", h)
	}
}

// cancelledRunErr is what the real runner returns when its context ends.
func cancelledRunErr(id string) error {
	return kernel.NewSandboxError("Operation cancelled", kernel.SandboxCodeCancelled, id)
}

// TestEnsureImageDoesNotRunOnTheFirstCallersContext is the WR-03 regression for the
// shared image preparation: the first caller gives up while the preparation runs, and
// a caller that still waits must get the outcome, not "Operation cancelled" (the shared
// preparation cannot be aborted by one caller).
func TestEnsureImageDoesNotRunOnTheFirstCallersContext(t *testing.T) {
	release := make(chan struct{})
	entered := make(chan struct{}, 1)
	provider, err := New(Options{Runner: funcRunner{run: func(ctx context.Context, _ []string, _ RunOptions) (RunResult, error) {
		entered <- struct{}{}
		<-release
		if ctx.Err() != nil {
			return RunResult{}, cancelledRunErr("docker:shared")
		}
		zero := 0
		return RunResult{Code: &zero}, nil
	}}})
	if err != nil {
		t.Fatal(err)
	}
	firstCtx, cancelFirst := context.WithCancel(context.Background())
	first := make(chan error, 1)
	go func() { first <- provider.EnsureImage(firstCtx, "node:22", "docker:shared") }()
	<-entered

	second := make(chan error, 1)
	go func() { second <- provider.EnsureImage(context.Background(), "node:22", "docker:shared") }()
	time.Sleep(50 * time.Millisecond) // let the second caller join the shared preparation
	cancelFirst()

	// The first caller is released at once, with its own cancellation.
	var se *kernel.SandboxError
	if err := <-first; !errors.As(err, &se) || se.Code != kernel.SandboxCodeCancelled {
		t.Errorf("the first caller got %v, want cancelled", err)
	}
	close(release)
	if err := <-second; err != nil {
		t.Errorf("a waiter with a live context got %v, want the preparation's success", err)
	}
	// The success is remembered: no second preparation.
	if err := provider.EnsureImage(context.Background(), "node:22", "docker:shared"); err != nil {
		t.Errorf("the remembered success: %v", err)
	}
}

// TestRecoveryDoesNotRunOnTheFirstCallersContext is the WR-03 regression for the
// shared container recovery: the caller that started it gives up, and the other
// caller must still see the container come back (docker rm then run complete).
func TestRecoveryDoesNotRunOnTheFirstCallersContext(t *testing.T) {
	var (
		mu    sync.Mutex
		runs  int
		first = true
	)
	release := make(chan struct{})
	entered := make(chan struct{}, 1)
	zero, one := 0, 1
	var recovered atomic.Bool
	provider, err := New(Options{Runner: funcRunner{run: func(ctx context.Context, args []string, opts RunOptions) (RunResult, error) {
		switch {
		case len(args) > 0 && args[0] == "exec" && strings.Contains(opts.Input, `"op":"stat"`):
			if !recovered.Load() {
				return RunResult{Code: &one, Stderr: "Error response from daemon: No such container: x"}, nil
			}
			return RunResult{Code: &zero, Stdout: `{"ok":true,"result":null}` + "\n"}, nil
		case len(args) >= 2 && args[0] == "container" && args[1] == "inspect":
			mu.Lock()
			hold := first
			first = false
			mu.Unlock()
			if hold {
				entered <- struct{}{}
				<-release
			}
			if ctx.Err() != nil {
				return RunResult{}, cancelledRunErr("docker:own:a1")
			}
			return RunResult{Code: &one, Stderr: "Error: No such container"}, nil
		case len(args) > 0 && args[0] == "run":
			if ctx.Err() != nil {
				return RunResult{}, cancelledRunErr("docker:own:a1")
			}
			mu.Lock()
			runs++
			mu.Unlock()
			recovered.Store(true)
		}
		if ctx.Err() != nil {
			return RunResult{}, cancelledRunErr("docker:own:a1")
		}
		return RunResult{Code: &zero, Stdout: `{"ok":true,"result":{}}` + "\n"}, nil
	}}})
	if err != nil {
		t.Fatal(err)
	}
	sb, err := newSandbox(provider, sandboxhub.Spec{Kind: sandboxhub.KindDocker}, sandboxhub.CreateContext{ID: "docker:own:a1", Tenancy: sandboxhub.TenancyOwn, AgentID: "a1"})
	if err != nil {
		t.Fatal(err)
	}
	v, err := sb.ForAgent(context.Background(), "a1")
	if err != nil {
		t.Fatal(err)
	}

	firstCtx, cancelFirst := context.WithCancel(context.Background())
	firstErr := make(chan error, 1)
	go func() { _, err := v.Stat(firstCtx, "x.txt"); firstErr <- err }()
	<-entered // the first caller started the recovery and it is held at its first docker call

	secondErr := make(chan error, 1)
	go func() { _, err := v.Stat(context.Background(), "x.txt"); secondErr <- err }()
	time.Sleep(50 * time.Millisecond) // let the second caller join the recovery
	cancelFirst()

	var se *kernel.SandboxError
	if err := <-firstErr; !errors.As(err, &se) || se.Code != kernel.SandboxCodeCancelled {
		t.Errorf("the first caller got %v, want cancelled", err)
	}
	close(release)
	if err := <-secondErr; err != nil {
		t.Errorf("the second caller got %v: the recovery was aborted by the first caller's context", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if runs != 1 {
		t.Errorf("docker run was called %d times, want 1", runs)
	}
}

// TestReplacingTheContainerIsNotSplitByACancellation is the WR-03 regression for the
// rm to run step: a context that ends right after "docker rm --force" must not stop
// "docker run", which would leave the sandbox with no container at all.
func TestReplacingTheContainerIsNotSplitByACancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	zero := 0
	var runCtxErr error
	var ranRun bool
	provider, err := New(Options{Runner: funcRunner{run: func(callCtx context.Context, args []string, _ RunOptions) (RunResult, error) {
		switch {
		case len(args) >= 2 && args[0] == "container" && args[1] == "inspect":
			return RunResult{Code: &zero, Stdout: `{}|false` + "\n"}, nil // a foreign container: replace it
		case len(args) > 0 && args[0] == "rm":
			cancel() // the caller gives up between rm and run
		case len(args) > 0 && args[0] == "run":
			ranRun, runCtxErr = true, callCtx.Err()
		default:
			if callCtx.Err() != nil {
				return RunResult{}, cancelledRunErr("docker:shared")
			}
		}
		return RunResult{Code: &zero}, nil
	}}})
	if err != nil {
		t.Fatal(err)
	}
	sb, err := newSandbox(provider, sandboxhub.Spec{Kind: sandboxhub.KindDocker}, sandboxhub.CreateContext{ID: "docker:shared", Tenancy: sandboxhub.TenancyShared})
	if err != nil {
		t.Fatal(err)
	}
	_ = sb.ensureContainer(ctx) // the later setup steps may report the cancellation; run must already have happened
	if !ranRun {
		t.Fatal("docker run was not called after docker rm: the sandbox was left without a container")
	}
	if runCtxErr != nil {
		t.Errorf("docker run got a cancelled context (%v)", runCtxErr)
	}
	// A context that is already cancelled starts neither step.
	ranRun = false
	cancelled, stop := context.WithCancel(context.Background())
	stop()
	var se *kernel.SandboxError
	if err := sb.ensureContainer(cancelled); !errors.As(err, &se) || se.Code != kernel.SandboxCodeCancelled {
		t.Errorf("ensureContainer with a cancelled context = %v, want cancelled", err)
	}
	if ranRun {
		t.Error("docker run started although the context was already cancelled")
	}
}

// TestCreateRefusesIDsThatShareAContainerAndVolumeName is the WR-05 regression: namesFor
// keeps a fixed mapping (every character outside [A-Za-z0-9_.-] becomes "-"), which is
// not injective, so two own sandboxes could share one container and one /workspace volume.
// Create and RemoveData refuse such ids before any docker call; the ids that map to a
// name of their own keep their names.
func TestCreateRefusesIDsThatShareAContainerAndVolumeName(t *testing.T) {
	provider, err := New(Options{Runner: funcRunner{run: func(_ context.Context, args []string, _ RunOptions) (RunResult, error) {
		t.Errorf("docker was called for a refused id: docker %q", args)
		return RunResult{}, errors.New("unexpected docker call")
	}}})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	own := func(agent string) sandboxhub.CreateContext {
		return sandboxhub.CreateContext{ID: "docker:own:" + agent, Tenancy: sandboxhub.TenancyOwn, AgentID: agent}
	}
	refused := []sandboxhub.CreateContext{
		own("a/b"), own("a b"), own("a\nb"), own("../x"), own(""), own("a\U0001f600b"),
		{ID: "docker:env:x", Tenancy: sandboxhub.TenancyShared},
		{ID: "docker:weird name!", Tenancy: sandboxhub.TenancyShared},
		// the id is plain but the agent id is not
		{ID: "docker:own:a-b", Tenancy: sandboxhub.TenancyOwn, AgentID: "a/b"},
		// without the "docker:" prefix, TrimPrefix is a no-op and the id would share the
		// container and volume of its prefixed twin (IN-202)
		{ID: "shared", Tenancy: sandboxhub.TenancyShared},
		{ID: "own:x", Tenancy: sandboxhub.TenancyOwn, AgentID: "x"},
		{ID: "x", Tenancy: sandboxhub.TenancyShared},
		{ID: "", Tenancy: sandboxhub.TenancyShared},
	}
	for _, cc := range refused {
		_, err := provider.Create(ctx, sandboxhub.Spec{Kind: sandboxhub.KindDocker}, cc)
		var se *kernel.SandboxError
		if !errors.As(err, &se) || se.Code != kernel.SandboxCodePolicyDenied || se.SandboxID != cc.ID {
			t.Errorf("Create(%q, agent %q) = %v, want policy_denied for the id", cc.ID, cc.AgentID, err)
		}
		err = provider.RemoveData(ctx, sandboxhub.Spec{Kind: sandboxhub.KindDocker}, cc, "")
		if !errors.As(err, &se) || se.Code != kernel.SandboxCodePolicyDenied {
			t.Errorf("RemoveData(%q) = %v, want policy_denied", cc.ID, err)
		}
	}
	// What is refused is exactly what collided: the plain ids all have distinct names.
	names := map[string]string{}
	for _, id := range []string{"docker:shared", "docker:own:a-b", "docker:own:a.b", "docker:own:a_b", "docker:revit-box", "docker:env-x", "docker:own-a-b"} {
		if err := checkSandboxID(sandboxhub.CreateContext{ID: id, Tenancy: sandboxhub.TenancyShared}); err != nil {
			t.Errorf("checkSandboxID(%q) = %v, want accepted", id, err)
		}
		c, v := provider.NamesFor(id)
		if c != v {
			t.Errorf("%q: container %q and volume %q differ", id, c, v)
		}
		if other, dup := names[c]; dup {
			t.Errorf("%q and %q share the name %q", id, other, c)
		}
		names[c] = id
	}
	if c, _ := provider.NamesFor("docker:own:agent-1"); c != "aec-sandbox-own-agent-1" {
		t.Errorf("an accepted id changed its name: %q", c)
	}
}
