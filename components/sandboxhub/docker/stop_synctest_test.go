package docker

import (
	"context"
	"testing"
	"testing/synctest"
	"time"

	"github.com/Phong95/aec-agent-go/components/sandboxhub"
)

// The stop sequence (CONTEXT C5, stopGrace): a command that must stop (timeout,
// cancel, output cap) is stopped by CLOSING the helper's stdin, which makes the helper
// kill the command's process group inside the container; the docker client is killed
// only if it is still running stopGrace later. Killing the client alone would leave the
// command running, so the order is the thing under test, and testing/synctest gives the
// 5 seconds a fake clock: nothing here sleeps for real, and there is no docker, no pipe
// and no os/exec inside the bubble (the process is the channel-based fake).

// execResultOrErr is what a background Exec call returned.
type execResultOrErr struct {
	res sandboxhub.ExecResult
	err error
}

// startExec starts v.Exec in a goroutine inside the bubble and returns the channel of
// its outcome, after the fake client has been started and has said AEC-OK.
func startExec(ctx context.Context, t *testing.T, v sandboxhub.View, runner *fakeRunner, req sandboxhub.ExecRequest) (*fakeProcess, <-chan execResultOrErr) {
	t.Helper()
	proc := newFakeProcess()
	runner.onStart = func(context.Context, []string) (Process, error) { return proc, nil }
	out := make(chan execResultOrErr, 1)
	go func() {
		res, err := v.Exec(ctx, req, sandboxhub.ExecOptions{})
		out <- execResultOrErr{res, err}
	}()
	synctest.Wait()
	proc.emitStdout([]byte("AEC-OK\n"))
	synctest.Wait()
	return proc, out
}

func stopRequest(timeoutMs int) sandboxhub.ExecRequest {
	return sandboxhub.ExecRequest{Command: []string{"sleep", "300"}, TimeoutMs: timeoutMs, MaxOutputBytes: 1000}
}

// sleepTo advances the fake clock to start+offset and lets every goroutine settle.
func sleepTo(start time.Time, offset time.Duration) {
	time.Sleep(time.Until(start.Add(offset)))
	synctest.Wait()
}

func wantOutcome(t *testing.T, out <-chan execResultOrErr) sandboxhub.ExecResult {
	t.Helper()
	select {
	case r := <-out:
		if r.err != nil {
			t.Fatalf("Exec failed: %v", r.err)
		}
		return r.res
	default:
		t.Fatal("Exec has not returned")
		return sandboxhub.ExecResult{}
	}
}

// TestStopSequenceTimeout: stdin is closed at the timeout (50 ms), the client is NOT
// killed 4.9 s after that and IS killed at the end of the grace period, and the result
// reports the timeout with no exit code.
func TestStopSequenceTimeout(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		runner, _, v := startedSandbox(t, sandboxhub.TenancyShared)
		start := time.Now()
		proc, out := startExec(context.Background(), t, v, runner, stopRequest(50))

		sleepTo(start, 49*time.Millisecond)
		if proc.stdin.wasClosed() {
			t.Fatal("stdin was closed before the timeout")
		}
		sleepTo(start, 50*time.Millisecond)
		if !proc.stdin.wasClosed() {
			t.Fatal("stdin was not closed at the timeout (50 ms)")
		}
		if got := proc.killCount(); got != 0 {
			t.Fatalf("the client was killed at the timeout (%d kills): the helper must be asked first", got)
		}
		sleepTo(start, 4950*time.Millisecond)
		if got := proc.killCount(); got != 0 {
			t.Fatalf("the client was killed at 4.95 s (%d kills), before the grace period of 5 s after the stop ended", got)
		}
		sleepTo(start, 5049*time.Millisecond)
		if got := proc.killCount(); got != 0 {
			t.Fatalf("the client was killed at 5.049 s (%d kills)", got)
		}
		sleepTo(start, 5051*time.Millisecond)
		if got := proc.killCount(); got != 1 {
			t.Fatalf("the client was killed %d times at 5.051 s, want exactly 1 (stop at 50 ms + %s)", got, stopGrace)
		}
		res := wantOutcome(t, out)
		if !res.TimedOut || res.ExitCode != nil || res.Cancelled || res.Truncated {
			t.Fatalf("result = %+v, want timed out with no exit code", res)
		}
	})
}

// TestStopSequenceNoKillWhenStreamsClose: when the helper does what it should - the
// command dies and the client exits right after stdin closed - the client is never
// killed, not even after the grace period has long passed.
func TestStopSequenceNoKillWhenStreamsClose(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		runner, _, v := startedSandbox(t, sandboxhub.TenancyShared)
		start := time.Now()
		proc := newFakeProcess()
		proc.closeStreamsOnStdinClose(137)
		runner.onStart = func(context.Context, []string) (Process, error) { return proc, nil }
		out := make(chan execResultOrErr, 1)
		go func() {
			res, err := v.Exec(context.Background(), stopRequest(50), sandboxhub.ExecOptions{})
			out <- execResultOrErr{res, err}
		}()
		synctest.Wait()
		proc.emitStdout([]byte("AEC-OK\n"))
		synctest.Wait()

		sleepTo(start, 50*time.Millisecond)
		if !proc.stdin.wasClosed() {
			t.Fatal("stdin was not closed at the timeout")
		}
		res := wantOutcome(t, out)
		if !res.TimedOut || res.ExitCode != nil {
			t.Fatalf("result = %+v, want timed out with no exit code", res)
		}
		sleepTo(start, 30*time.Second)
		if got := proc.killCount(); got != 0 {
			t.Fatalf("the client was killed %d times although its streams closed after stdin did", got)
		}
	})
}

// TestStopSequenceCancel: a cancelled context follows the same sequence as a timeout:
// stdin closed at the cancel, the client killed only stopGrace later - or never when the
// streams close first.
func TestStopSequenceCancel(t *testing.T) {
	t.Run("the client is killed only after the grace period", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			runner, _, v := startedSandbox(t, sandboxhub.TenancyShared)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			start := time.Now()
			proc, out := startExec(ctx, t, v, runner, stopRequest(60_000))

			sleepTo(start, 200*time.Millisecond)
			if proc.stdin.wasClosed() {
				t.Fatal("stdin was closed before the cancel")
			}
			cancel()
			synctest.Wait()
			if !proc.stdin.wasClosed() {
				t.Fatal("stdin was not closed at the cancel")
			}
			cancelledAt := time.Now()
			sleepTo(cancelledAt, 4900*time.Millisecond)
			if got := proc.killCount(); got != 0 {
				t.Fatalf("the client was killed %d times 4.9 s after the cancel", got)
			}
			sleepTo(cancelledAt, 5010*time.Millisecond)
			if got := proc.killCount(); got != 1 {
				t.Fatalf("the client was killed %d times 5.01 s after the cancel, want exactly 1", got)
			}
			res := wantOutcome(t, out)
			if !res.Cancelled || res.ExitCode != nil || res.TimedOut {
				t.Fatalf("result = %+v, want cancelled with no exit code", res)
			}
		})
	})
	t.Run("the client is never killed when its streams close after stdin", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			runner, _, v := startedSandbox(t, sandboxhub.TenancyShared)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			proc := newFakeProcess()
			proc.closeStreamsOnStdinClose(137)
			runner.onStart = func(context.Context, []string) (Process, error) { return proc, nil }
			out := make(chan execResultOrErr, 1)
			go func() {
				res, err := v.Exec(ctx, stopRequest(60_000), sandboxhub.ExecOptions{})
				out <- execResultOrErr{res, err}
			}()
			synctest.Wait()
			proc.emitStdout([]byte("AEC-OK\n"))
			synctest.Wait()

			cancel()
			synctest.Wait()
			res := wantOutcome(t, out)
			if !res.Cancelled || res.ExitCode != nil {
				t.Fatalf("result = %+v, want cancelled with no exit code", res)
			}
			time.Sleep(30 * time.Second)
			if got := proc.killCount(); got != 0 {
				t.Fatalf("the client was killed %d times although its streams closed after stdin did", got)
			}
		})
	})
}

// TestStopSequenceOutputCap: the output cap starts the same sequence. The command's
// output is cut at the cap, stdin is closed at once, and the client is killed only when
// it keeps running for the grace period.
func TestStopSequenceOutputCap(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		runner, _, v := startedSandbox(t, sandboxhub.TenancyShared)
		req := stopRequest(60_000)
		req.MaxOutputBytes = 4
		proc, out := startExec(context.Background(), t, v, runner, req)

		proc.emitStdout([]byte("abcdef"))
		synctest.Wait()
		if !proc.stdin.wasClosed() {
			t.Fatal("stdin was not closed when the output hit the cap")
		}
		capped := time.Now()
		sleepTo(capped, 4990*time.Millisecond)
		if got := proc.killCount(); got != 0 {
			t.Fatalf("the client was killed %d times before the grace period ended", got)
		}
		sleepTo(capped, 5010*time.Millisecond)
		if got := proc.killCount(); got != 1 {
			t.Fatalf("the client was killed %d times after the grace period, want exactly 1", got)
		}
		res := wantOutcome(t, out)
		if !res.Truncated || res.Stdout != "abcd" || res.ExitCode != nil {
			t.Fatalf("result = %+v, want truncated stdout abcd with no exit code", res)
		}
	})
}
