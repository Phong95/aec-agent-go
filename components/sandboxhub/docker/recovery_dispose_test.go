package docker

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Phong95/aec-agent-go/components/sandboxhub"
	"github.com/Phong95/aec-agent-go/kernel"
)

// Review iteration 2 regressions for the detached shared work (WR-202, IN-204): the
// container recovery and the image preparation run in goroutines that outlive the call
// that started them, so Dispose and a panicking runner have to be handled there.

// waitUntil polls cond until it holds or five seconds pass.
func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// callLog is a goroutine-safe list of the docker verbs a runner saw.
type callLog struct {
	mu    sync.Mutex
	verbs []string
}

func (l *callLog) add(verb string) {
	l.mu.Lock()
	l.verbs = append(l.verbs, verb)
	l.mu.Unlock()
}

func (l *callLog) all() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.verbs...)
}

func (l *callLog) count(verb string) int {
	n := 0
	for _, v := range l.all() {
		if v == verb {
			n++
		}
	}
	return n
}

// verbOf names a docker call: "inspect" for a container inspect, otherwise the first
// argument ("run", "rm", "exec", "volume", ...).
func verbOf(args []string) string {
	if len(args) >= 2 && args[0] == "container" && args[1] == "inspect" {
		return "inspect"
	}
	if len(args) > 0 {
		return args[0]
	}
	return ""
}

func isDisposedError(err error) bool {
	var se *kernel.SandboxError
	return errors.As(err, &se) && se.Code == kernel.SandboxCodeEnvironmentUnavailable && strings.Contains(se.Message, "was disposed")
}

// TestARecoveryThatOutlivesDisposeCreatesNoContainer is the WR-202 regression. The
// recovery runs on a detached context, so an aborted agent turn no longer stops it; the
// usual next step is Dispose, and the recovery used to go on to "docker run", which
// recreated the container (and, through --mount, the volume that Dispose(removeData)
// had just deleted) with nobody left to remove it.
//
// Sequence: a recovery starts and is held at its first docker call, the turn is aborted,
// the sandbox is disposed with removeData, the recovery is released. It must issue no
// "run" and no "start" after Dispose, and the caller that still waits gets the "was
// disposed" error.
func TestARecoveryThatOutlivesDisposeCreatesNoContainer(t *testing.T) {
	var (
		log   callLog
		first atomic.Bool
	)
	first.Store(true)
	release := make(chan struct{})
	entered := make(chan struct{}, 1)
	zero, one := 0, 1
	provider, err := New(Options{Runner: funcRunner{run: func(_ context.Context, args []string, opts RunOptions) (RunResult, error) {
		verb := verbOf(args)
		log.add(verb)
		switch {
		case verb == "exec" && strings.Contains(opts.Input, `"op":"stat"`):
			return RunResult{Code: &one, Stderr: "Error response from daemon: No such container: x"}, nil
		case verb == "inspect":
			if first.CompareAndSwap(true, false) {
				entered <- struct{}{}
				<-release
			}
			return RunResult{Code: &one, Stderr: "Error: No such container"}, nil
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

	turnCtx, abortTurn := context.WithCancel(context.Background())
	turnErr := make(chan error, 1)
	go func() { _, err := v.Stat(turnCtx, "x.txt"); turnErr <- err }()
	<-entered // the recovery runs and is held at its first docker call
	waiterErr := make(chan error, 1)
	go func() { _, err := v.Stat(context.Background(), "x.txt"); waiterErr <- err }()
	time.Sleep(50 * time.Millisecond) // let the second caller join the recovery
	abortTurn()

	var se *kernel.SandboxError
	if err := <-turnErr; !errors.As(err, &se) || se.Code != kernel.SandboxCodeCancelled {
		t.Errorf("the aborted turn got %v, want cancelled", err)
	}
	if err := sb.Dispose(context.Background(), true); err != nil {
		t.Fatalf("Dispose: %v", err)
	}
	close(release) // the recovery goes on after Dispose

	if err := <-waiterErr; !isDisposedError(err) {
		t.Errorf("the caller that waited got %v, want the \"was disposed\" error", err)
	}
	waitUntil(t, "the recovery to end", func() bool {
		sb.mu.Lock()
		defer sb.mu.Unlock()
		return sb.recovering == nil
	})
	if n := log.count("run"); n != 0 {
		t.Errorf("docker run was issued %d time(s) after Dispose; the recovery recreated the container and volume", n)
	}
	if n := log.count("start"); n != 0 {
		t.Errorf("docker start was issued %d time(s) after Dispose", n)
	}
}

// createdID is what docker run --detach prints: the full id of the container it created.
const createdID = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

// TestDisposeDuringTheReplaceStepRemovesTheContainerItCreated covers the window the
// check cannot close: docker run is already in flight when Dispose arrives. Dispose
// must wait for the step (bounded by its context) and the container the step created
// must be removed, and only that container: no volume is removed when removeData is
// false, and nothing runs after the cleanup but Dispose's own rm.
func TestDisposeDuringTheReplaceStepRemovesTheContainerItCreated(t *testing.T) {
	var (
		mu    sync.Mutex
		calls [][]string
	)
	runEntered := make(chan struct{}, 1)
	releaseRun := make(chan struct{})
	zero, one := 0, 1
	provider, err := New(Options{Runner: funcRunner{run: func(_ context.Context, args []string, _ RunOptions) (RunResult, error) {
		mu.Lock()
		calls = append(calls, append([]string(nil), args...))
		mu.Unlock()
		switch verbOf(args) {
		case "inspect":
			return RunResult{Code: &one, Stderr: "Error: No such container"}, nil
		case "run":
			runEntered <- struct{}{}
			<-releaseRun
			return RunResult{Code: &zero, Stdout: createdID + "\n"}, nil
		}
		return RunResult{Code: &zero, Stdout: `{"ok":true,"result":{}}` + "\n"}, nil
	}}})
	if err != nil {
		t.Fatal(err)
	}
	sb, err := newSandbox(provider, sandboxhub.Spec{Kind: sandboxhub.KindDocker}, sandboxhub.CreateContext{ID: "docker:shared", Tenancy: sandboxhub.TenancyShared})
	if err != nil {
		t.Fatal(err)
	}

	ensureErr := make(chan error, 1)
	go func() { ensureErr <- sb.ensureContainer(context.Background()) }()
	<-runEntered
	disposeErr := make(chan error, 1)
	go func() { disposeErr <- sb.Dispose(context.Background(), false) }()
	waitUntil(t, "Dispose to mark the sandbox disposed", sb.isDisposed)
	select {
	case err := <-disposeErr:
		t.Fatalf("Dispose returned (%v) while the replace step was still running", err)
	case <-time.After(50 * time.Millisecond):
	}
	close(releaseRun)

	if err := <-ensureErr; !isDisposedError(err) {
		t.Errorf("ensureContainer = %v, want the \"was disposed\" error", err)
	}
	if err := <-disposeErr; err != nil {
		t.Errorf("Dispose: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	runAt := -1
	for i, a := range calls {
		if verbOf(a) == "run" {
			runAt = i
		}
	}
	if runAt < 0 {
		t.Fatal("docker run was never issued; the test did not reach the window")
	}
	after := calls[runAt+1:]
	if len(after) != 2 {
		t.Fatalf("calls after docker run = %q, want the step's rm and Dispose's rm", after)
	}
	// The step removes the container it created by the id docker run printed (WR-301),
	// then Dispose removes the sandbox's container by name, as it always does.
	for i, want := range []string{createdID, sb.containerName} {
		a := after[i]
		if len(a) != 3 || a[0] != "rm" || a[1] != "--force" || a[2] != want {
			t.Errorf("call %d after docker run = %q, want rm --force %s", i, a, want)
		}
	}
}

// replaceStepRunner drives one replace step on a sandbox that Dispose has already
// passed: docker run is held, Dispose runs with a context that has ended (so it does not
// wait for the step), then run is released with the given stdout. It returns the calls
// made after docker run and the error of ensureContainer.
func replaceStepRunner(t *testing.T, runStdout string, rmByID func(id string) RunResult) (after [][]string, ensureErr error) {
	t.Helper()
	var (
		mu    sync.Mutex
		calls [][]string
	)
	runEntered := make(chan struct{}, 1)
	releaseRun := make(chan struct{})
	zero, one := 0, 1
	var sb *Sandbox
	provider, err := New(Options{Runner: funcRunner{run: func(_ context.Context, args []string, _ RunOptions) (RunResult, error) {
		mu.Lock()
		calls = append(calls, append([]string(nil), args...))
		mu.Unlock()
		switch verbOf(args) {
		case "inspect":
			return RunResult{Code: &one, Stderr: "Error: No such container"}, nil
		case "run":
			runEntered <- struct{}{}
			<-releaseRun
			return RunResult{Code: &zero, Stdout: runStdout}, nil
		case "rm":
			if rmByID != nil && args[len(args)-1] != sb.containerName {
				return rmByID(args[len(args)-1]), nil
			}
		}
		return RunResult{Code: &zero, Stdout: `{"ok":true,"result":{}}` + "\n"}, nil
	}}})
	if err != nil {
		t.Fatal(err)
	}
	sb, err = newSandbox(provider, sandboxhub.Spec{Kind: sandboxhub.KindDocker}, sandboxhub.CreateContext{ID: "docker:shared", Tenancy: sandboxhub.TenancyShared})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- sb.ensureContainer(context.Background()) }()
	<-runEntered
	ended, cancel := context.WithCancel(context.Background())
	cancel()
	if err := sb.Dispose(ended, false); err != nil {
		t.Fatalf("Dispose: %v", err)
	}
	mu.Lock()
	seen := len(calls) // docker run and Dispose's own calls; the step's cleanup comes after
	mu.Unlock()
	close(releaseRun)
	ensureErr = <-done
	mu.Lock()
	defer mu.Unlock()
	return append([][]string(nil), calls[seen:]...), ensureErr
}

// TestTheReplaceStepRemovesItsContainerByIDNotByName is the WR-301 regression. The step
// that lost the race with Dispose cleans up after docker run. A container name is not an
// identity: the hub always uses the id docker:shared, so a successor sandbox that was
// created after this Dispose reuses the container of the same name and fingerprint, and
// removing by name would destroy the successor's container. The cleanup must address
// the id that docker run printed.
func TestTheReplaceStepRemovesItsContainerByIDNotByName(t *testing.T) {
	after, err := replaceStepRunner(t, createdID+"\n", nil)
	if !isDisposedError(err) {
		t.Errorf("ensureContainer = %v, want the \"was disposed\" error", err)
	}
	// Dispose already ran (before the release); the step's cleanup is the only call left.
	if len(after) != 1 || len(after[0]) != 3 || after[0][0] != "rm" || after[0][1] != "--force" || after[0][2] != createdID {
		t.Fatalf("calls after docker run = %q, want exactly rm --force %s", after, createdID)
	}
}

// TestTheReplaceStepKeepsWhatItCannotIdentify: when docker run printed no usable id
// (empty, or not a hex id) the step must not fall back to the name. It leaves the
// container and says so.
func TestTheReplaceStepKeepsWhatItCannotIdentify(t *testing.T) {
	for _, stdout := range []string{"", "\n", "aec-sandbox-shared\n", "not an id", createdID + "\n" + createdID + "\n", "0123\n"} {
		after, err := replaceStepRunner(t, stdout, nil)
		if len(after) != 0 {
			t.Errorf("stdout %q: calls after docker run = %q, want none (no removal by name)", stdout, after)
		}
		var se *kernel.SandboxError
		if !errors.As(err, &se) || se.Code != kernel.SandboxCodeEnvironmentUnavailable ||
			!strings.Contains(se.Message, "was disposed") || !strings.Contains(se.Message, "no container id") {
			t.Errorf("stdout %q: ensureContainer = %v, want a disposed error that records the missing container id", stdout, err)
		}
	}
}

// TestTheReplaceStepReportsACleanupThatFails is the IN-303 regression: a refused
// cleanup rm used to be swallowed, leaving a running container nobody owns and nothing
// that said so.
func TestTheReplaceStepReportsACleanupThatFails(t *testing.T) {
	one := 1
	_, err := replaceStepRunner(t, createdID+"\n", func(string) RunResult {
		return RunResult{Code: &one, Stderr: "Error response from daemon: daemon refused"}
	})
	var se *kernel.SandboxError
	if !errors.As(err, &se) || se.Code != kernel.SandboxCodeEnvironmentUnavailable ||
		!strings.Contains(se.Message, "was disposed") || !strings.Contains(se.Message, "daemon refused") || !strings.Contains(se.Message, createdID) {
		t.Errorf("ensureContainer = %v, want a disposed error that names the container and the daemon's refusal", err)
	}
}

// TestDisposeBetweenTheRemoveAndTheRunStopsTheStep is the other IN-303 regression:
// when Dispose arrives while the old container is being removed, the step used to go on
// to create a container only to remove it again.
func TestDisposeBetweenTheRemoveAndTheRunStopsTheStep(t *testing.T) {
	var (
		log callLog
		sb  *Sandbox
	)
	zero := 0
	provider, err := New(Options{Runner: funcRunner{run: func(_ context.Context, args []string, _ RunOptions) (RunResult, error) {
		log.add(verbOf(args))
		switch verbOf(args) {
		case "inspect":
			return RunResult{Code: &zero, Stdout: `{}|false` + "\n"}, nil // a foreign container: replace it
		case "rm":
			sb.mu.Lock() // Dispose has run: it sets this under the lock, then waits for the step
			sb.disposed = true
			sb.mu.Unlock()
		}
		return RunResult{Code: &zero, Stdout: `{"ok":true,"result":{}}` + "\n"}, nil
	}}})
	if err != nil {
		t.Fatal(err)
	}
	sb, err = newSandbox(provider, sandboxhub.Spec{Kind: sandboxhub.KindDocker}, sandboxhub.CreateContext{ID: "docker:shared", Tenancy: sandboxhub.TenancyShared})
	if err != nil {
		t.Fatal(err)
	}
	if err := sb.ensureContainer(context.Background()); !isDisposedError(err) {
		t.Errorf("ensureContainer = %v, want the \"was disposed\" error", err)
	}
	if n := log.count("run"); n != 0 {
		t.Errorf("docker run was issued %d time(s) after Dispose had marked the sandbox disposed", n)
	}
}

// TestAPanicInTheRunnerIsAnErrorForEveryWaiter is the IN-204 regression: the recovery
// goroutine had no recover, so a panic in a Runner killed the process (hub.safeCreate
// only guards Create itself) and, had it been caught, would have left sb.recovering set
// and done open. Every waiter now gets the error and the next call starts afresh.
func TestAPanicInTheRunnerIsAnErrorForEveryWaiter(t *testing.T) {
	var first atomic.Bool
	first.Store(true)
	release := make(chan struct{})
	entered := make(chan struct{}, 1)
	zero := 0
	provider, err := New(Options{Runner: funcRunner{run: func(_ context.Context, args []string, _ RunOptions) (RunResult, error) {
		if verbOf(args) == "inspect" && first.CompareAndSwap(true, false) {
			entered <- struct{}{}
			<-release
			panic("runner exploded")
		}
		if verbOf(args) == "inspect" {
			return RunResult{Code: &zero, Stdout: `{}|false` + "\n"}, nil // a foreign container: replace it
		}
		return RunResult{Code: &zero, Stdout: `{"ok":true,"result":{}}` + "\n"}, nil
	}}})
	if err != nil {
		t.Fatal(err)
	}
	sb, err := newSandbox(provider, sandboxhub.Spec{Kind: sandboxhub.KindDocker}, sandboxhub.CreateContext{ID: "docker:shared", Tenancy: sandboxhub.TenancyShared})
	if err != nil {
		t.Fatal(err)
	}

	errs := make(chan error, 2)
	go func() { errs <- sb.recover(context.Background()) }()
	<-entered
	go func() { errs <- sb.recover(context.Background()) }()
	time.Sleep(50 * time.Millisecond) // let the second caller join
	close(release)
	for i := range 2 {
		err := <-errs
		var se *kernel.SandboxError
		if !errors.As(err, &se) || se.Code != kernel.SandboxCodeEnvironmentUnavailable || !strings.Contains(se.Message, "runner exploded") {
			t.Errorf("waiter %d got %v, want an environment_unavailable error naming the panic", i, err)
		} else if !strings.Contains(se.Message, "goroutine") || !strings.Contains(se.Message, "recovery_dispose_test.go") {
			// IN-302: the stack of the panic is kept, down to the runner that raised it.
			t.Errorf("waiter %d: the error carries no stack of the panic: %q", i, se.Message)
		}
	}
	// The bookkeeping ran: the recovery is gone and a new one works.
	sb.mu.Lock()
	stuck := sb.recovering != nil
	sb.mu.Unlock()
	if stuck {
		t.Fatal("sb.recovering is still set after the panic")
	}
	if err := sb.recover(context.Background()); err != nil {
		t.Errorf("the recovery after a panic: %v", err)
	}
}

// TestAPanicInThePreparationIsAnErrorAndIsForgotten is the same guard for EnsureImage:
// the waiters get the error, the failure is not remembered, and the next call retries.
func TestAPanicInThePreparationIsAnErrorAndIsForgotten(t *testing.T) {
	var calls atomic.Int32
	zero := 0
	provider, err := New(Options{Runner: funcRunner{run: func(context.Context, []string, RunOptions) (RunResult, error) {
		if calls.Add(1) == 1 {
			panic("prepare exploded")
		}
		return RunResult{Code: &zero}, nil
	}}})
	if err != nil {
		t.Fatal(err)
	}
	err = provider.EnsureImage(context.Background(), "node:22", "docker:shared")
	var se *kernel.SandboxError
	if !errors.As(err, &se) || se.Code != kernel.SandboxCodeEnvironmentUnavailable || !strings.Contains(se.Message, "prepare exploded") {
		t.Fatalf("EnsureImage = %v, want an environment_unavailable error naming the panic", err)
	}
	if err := provider.EnsureImage(context.Background(), "node:22", "docker:shared"); err != nil {
		t.Errorf("the retry after a panic: %v", err)
	}
}
