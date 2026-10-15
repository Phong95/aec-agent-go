package docker

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Phong95/aec-agent-go/components/sandboxhub"
	"github.com/Phong95/aec-agent-go/kernel"
)

// docker-exec-transcripts.json is recorded by wiregen from a reference sandbox view
// over a fake child process: per exec step it holds the script the child played (the
// chunks of stdout and stderr and the exit code), the header written to the helper's
// stdin, the live output pieces per stream, the outcome, and whether stdin was ended or
// the client killed. The Go exec stream runs the same script through a fake Process and
// must write the same header and give the same pieces, outcome and stop behaviour.

// spawnScript is what one fake docker client plays: chunks on the two streams, then an
// exit code, at once (closeOn empty) or once the host closed stdin (closeOn "stdinEnd",
// the host stopped the command).
type spawnScript struct {
	Chunks  []chunkScript `json:"chunks"`
	Close   int           `json:"close"`
	CloseOn string        `json:"closeOn"`
}

// chunkScript is one read of one stream: text, raw bytes in hex, or a repeated text.
type chunkScript struct {
	Stream string  `json:"stream"`
	Text   string  `json:"text"`
	Hex    *string `json:"hex"`
	Repeat *repeat `json:"repeat"`
}

func (c chunkScript) data(t *testing.T) []byte {
	t.Helper()
	switch {
	case c.Hex != nil:
		b, err := hex.DecodeString(*c.Hex)
		if err != nil {
			t.Fatalf("chunk hex %q: %v", *c.Hex, err)
		}
		return b
	case c.Repeat != nil:
		return []byte(strings.Repeat(c.Repeat.Text, c.Repeat.Count))
	default:
		return []byte(c.Text)
	}
}

type outputsRecord struct {
	Stdout []string `json:"stdout"`
	Stderr []string `json:"stderr"`
}

// spawnRecord is what was observed of one client: its arguments, the header line written
// to its stdin (raw and parsed), and whether stdin was ended and the client killed.
type spawnRecord struct {
	Args       []string `json:"args"`
	HeaderLine string   `json:"headerLine"`
	Header     any      `json:"header"`
	StdinEnded bool     `json:"stdinEnded"`
	Killed     bool     `json:"killed"`
}

type executionRecord struct {
	Outputs outputsRecord `json:"outputs"`
	Spawns  []spawnRecord `json:"spawns"`
}

// spawnPlayer plays one script on a fake process and joins its goroutine.
type spawnPlayer struct {
	proc *fakeProcess
	args []string
	wg   sync.WaitGroup
}

func startPlayer(t *testing.T, script spawnScript, args []string) *spawnPlayer {
	t.Helper()
	pl := &spawnPlayer{proc: newFakeProcess(), args: slices.Clone(args)}
	pl.wg.Add(1)
	go func() {
		defer pl.wg.Done()
		for _, c := range script.Chunks {
			if c.Stream == "stdout" {
				pl.proc.emitStdout(c.data(t))
			} else {
				pl.proc.emitStderr(c.data(t))
			}
		}
		if script.CloseOn == "stdinEnd" {
			select {
			case <-pl.proc.stdin.closed:
			case <-time.After(replayTimeout):
				t.Errorf("the exec stream never closed stdin of a command it should have stopped")
			}
		}
		code := script.Close
		pl.proc.finish(&code)
	}()
	return pl
}

func (pl *spawnPlayer) record(t *testing.T) spawnRecord {
	t.Helper()
	pl.wg.Wait()
	line := pl.proc.stdin.headerLine()
	var header any
	if err := json.Unmarshal([]byte(line), &header); err != nil {
		t.Errorf("the header line %q is not JSON: %v", line, err)
	}
	return spawnRecord{
		Args: pl.args, HeaderLine: line, Header: header,
		StdinEnded: pl.proc.stdin.wasClosed(), Killed: pl.proc.killCount() > 0,
	}
}

// doExec runs one exec step: the scripted clients are handed out in the order the
// exec stream starts them, and what happened to each is kept for compareExecution.
func (r *replayer) doExec(ctx context.Context, s step) (any, error) {
	var req sandboxhub.ExecRequest
	r.decodeOpts(s.Args.Request, &req)
	execCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	if s.Args.PreAborted {
		cancel()
	}
	r.observed = &executionRecord{Outputs: outputsRecord{Stdout: []string{}, Stderr: []string{}}, Spawns: []spawnRecord{}}
	var players []*spawnPlayer
	queue := slices.Clone(s.Spawns)
	r.runner.mu.Lock()
	r.runner.onStart = func(_ context.Context, args []string) (Process, error) {
		if len(queue) == 0 {
			r.t.Errorf("the exec stream started a docker client the TS code did not start: docker %q", args)
			return nil, errors.New("unscripted docker client")
		}
		pl := startPlayer(r.t, queue[0], args)
		queue = queue[1:]
		players = append(players, pl)
		return pl.proc, nil
	}
	r.runner.mu.Unlock()
	defer func() {
		r.runner.mu.Lock()
		r.runner.onStart = nil
		r.runner.mu.Unlock()
	}()

	res, err := r.views[*s.Args.Agent].Exec(execCtx, req, sandboxhub.ExecOptions{
		OnOutput: func(stream sandboxhub.OutputStream, data string) {
			switch stream {
			case sandboxhub.StreamStdout:
				r.observed.Outputs.Stdout = append(r.observed.Outputs.Stdout, data)
			case sandboxhub.StreamStderr:
				r.observed.Outputs.Stderr = append(r.observed.Outputs.Stderr, data)
			}
		},
	})
	for _, pl := range players {
		r.observed.Spawns = append(r.observed.Spawns, pl.record(r.t))
	}
	if left := len(queue); left != 0 {
		r.t.Errorf("%d scripted docker clients were not started (the Go exec stream started fewer than TS)", left)
	}
	if err != nil {
		return nil, err
	}
	// The elapsed time is not a recorded fact; wiregen blanks it the same way.
	value, _ := toAny(r.t, res).(map[string]any)
	value["durationMs"] = float64(0)
	return value, nil
}

func (r *replayer) compareExecution(label string, want *executionRecord) {
	r.t.Helper()
	got := r.observed
	if !reflect.DeepEqual(got.Outputs, want.Outputs) {
		r.t.Errorf("%s: live output pieces\n got: %q\nwant: %q", label, got.Outputs, want.Outputs)
	}
	if len(got.Spawns) != len(want.Spawns) {
		r.t.Errorf("%s: %d docker clients started, TS started %d", label, len(got.Spawns), len(want.Spawns))
		return
	}
	for i, w := range want.Spawns {
		g := got.Spawns[i]
		if !reflect.DeepEqual(g.Args, w.Args) {
			r.t.Errorf("%s: client %d arguments\n got: %q\nwant: %q", label, i, g.Args, w.Args)
		}
		if g.HeaderLine != w.HeaderLine {
			r.t.Errorf("%s: client %d header line\n got: %s\nwant: %s", label, i, g.HeaderLine, w.HeaderLine)
		}
		if !reflect.DeepEqual(g.Header, w.Header) {
			r.t.Errorf("%s: client %d header\n got: %v\nwant: %v", label, i, g.Header, w.Header)
		}
		if g.StdinEnded != w.StdinEnded {
			r.t.Errorf("%s: client %d stdin ended = %v, TS ended it = %v", label, i, g.StdinEnded, w.StdinEnded)
		}
		if g.Killed != w.Killed {
			r.t.Errorf("%s: client %d killed = %v, TS killed it = %v", label, i, g.Killed, w.Killed)
		}
	}
}

// TestExecTranscriptsMatchGolden replays every recorded exec scenario.
func TestExecTranscriptsMatchGolden(t *testing.T) {
	for _, sc := range loadScenarios(t, "docker-exec-transcripts.json") {
		t.Run(sc.Name, func(t *testing.T) {
			newReplayer(t, sc).run()
		})
	}
}

// execStarted is a started sandbox plus the exec view of agent a1 and the fake runner
// that hands out the clients a test queues.
func execStarted(t *testing.T) (*fakeRunner, sandboxhub.View) {
	t.Helper()
	runner, _, v := startedSandbox(t, sandboxhub.TenancyShared)
	return runner, v
}

func execRequest() sandboxhub.ExecRequest {
	return sandboxhub.ExecRequest{Command: []string{"echo", "hi"}, TimeoutMs: 5000, MaxOutputBytes: 100000}
}

// TestExecStartFailureCarriesTheSandboxID: the client that cannot be started (the docker
// binary is missing) fails with the sandbox's id on the error.
func TestExecStartFailureCarriesTheSandboxID(t *testing.T) {
	runner, v := execStarted(t)
	runner.onStart = func(context.Context, []string) (Process, error) {
		return nil, kernel.NewSandboxError("Docker CLI \"docker\" was not found", kernel.SandboxCodeEnvironmentUnavailable, "")
	}
	_, err := v.Exec(context.Background(), execRequest(), sandboxhub.ExecOptions{})
	var se *kernel.SandboxError
	if !errors.As(err, &se) || se.Code != kernel.SandboxCodeEnvironmentUnavailable || se.SandboxID != "docker:shared" {
		t.Fatalf("Exec error = %v, want environment_unavailable for docker:shared", err)
	}
}

// TestExecNilExitCodeStaysNil: a client that ends without an exit code (a signal) and
// was not stopped by the host gives a result with no exit code.
func TestExecNilExitCodeStaysNil(t *testing.T) {
	runner, v := execStarted(t)
	runner.onStart = func(context.Context, []string) (Process, error) {
		p := newFakeProcess()
		go func() {
			p.emitStdout([]byte("AEC-OK\nbye\n"))
			p.finish(nil)
		}()
		return p, nil
	}
	res, err := v.Exec(context.Background(), execRequest(), sandboxhub.ExecOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if res.ExitCode != nil || res.Stdout != "bye\n" {
		t.Fatalf("result = %+v, want exit code nil and stdout bye", res)
	}
}

// TestExecTimeoutClosesStdinAfterTheHeader: the timeout closes the helper stdin, and
// the header is always written first (a stop never closes stdin before the write).
func TestExecTimeoutClosesStdinAfterTheHeader(t *testing.T) {
	runner, v := execStarted(t)
	var proc *fakeProcess
	runner.onStart = func(context.Context, []string) (Process, error) {
		proc = newFakeProcess()
		proc.closeStreamsOnStdinClose(137)
		return proc, nil
	}
	req := execRequest()
	req.TimeoutMs = 1
	res, err := v.Exec(context.Background(), req, sandboxhub.ExecOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !res.TimedOut || res.ExitCode != nil {
		t.Fatalf("result = %+v, want timed out with no exit code", res)
	}
	if !strings.HasPrefix(proc.stdin.written(), `{"command":["echo","hi"]`) || !proc.stdin.wasClosed() {
		t.Fatalf("the header was not written before stdin closed: %q (closed %v)", proc.stdin.written(), proc.stdin.wasClosed())
	}
}

// TestExecOnOutputOrder: the pieces of one stream arrive in order.
func TestExecOnOutputOrder(t *testing.T) {
	runner, v := execStarted(t)
	runner.onStart = func(context.Context, []string) (Process, error) {
		p := newFakeProcess()
		go func() {
			p.emitStdout([]byte("AEC-OK\n"))
			for _, piece := range []string{"a", "b", "c"} {
				p.emitStdout([]byte(piece))
			}
			p.finish(new(int))
		}()
		return p, nil
	}
	var got []string
	res, err := v.Exec(context.Background(), execRequest(), sandboxhub.ExecOptions{
		OnOutput: func(_ sandboxhub.OutputStream, data string) { got = append(got, data) },
	})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got, []string{"a", "b", "c"}) || res.Stdout != "abc" || res.ExitCode == nil || *res.ExitCode != 0 {
		t.Fatalf("pieces %q, result %+v", got, res)
	}
}

// TestExecOnOutputRunsOutsideTheStreamLock is the WR-04 regression: a callback that
// blocks (a slow SSE client, a full channel) must not keep the cancellation from
// stopping the command. The helper's stdin has to be closed while the callback is still
// blocked, and the pieces still arrive in order.
func TestExecOnOutputRunsOutsideTheStreamLock(t *testing.T) {
	runner, v := execStarted(t)
	var proc *fakeProcess
	runner.onStart = func(context.Context, []string) (Process, error) {
		proc = newFakeProcess()
		proc.closeStreamsOnStdinClose(137)
		p := proc
		go func() {
			p.emitStdout([]byte("AEC-OK\nfirst"))
			p.emitStdout([]byte("second"))
		}()
		return p, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	entered := make(chan struct{})
	release := make(chan struct{})
	var mu sync.Mutex
	var got []string
	done := make(chan sandboxhub.ExecResult, 1)
	go func() {
		res, err := v.Exec(ctx, execRequest(), sandboxhub.ExecOptions{OnOutput: func(_ sandboxhub.OutputStream, data string) {
			mu.Lock()
			got = append(got, data)
			first := len(got) == 1
			mu.Unlock()
			if first {
				close(entered)
				<-release // a consumer that does not return
			}
		}})
		if err != nil {
			t.Error(err)
		}
		done <- res
	}()
	select {
	case <-entered:
	case <-time.After(replayTimeout):
		t.Fatal("OnOutput was never called")
	}
	cancel()
	// The stop must go through although the callback is still blocked.
	deadline := time.After(replayTimeout)
	for !proc.stdin.wasClosed() {
		select {
		case <-deadline:
			close(release)
			t.Fatal("the cancellation could not stop the command while OnOutput was blocked (the stream mutex is held across the callback)")
		case <-time.After(5 * time.Millisecond):
		}
	}
	close(release)
	res := <-done
	if !res.Cancelled || res.ExitCode != nil {
		t.Errorf("result = %+v, want cancelled with no exit code", res)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(got) == 0 || got[0] != "first" {
		t.Errorf("pieces = %q, want them in order starting with first", got)
	}
}

// TestHelperTimeoutMs: a timeout the helper's JS timer cannot hold
// (setTimeout(kill, timeoutMs + 1000) above 2^31-1 ms fires after 1 ms) is capped at the
// largest one it can; any smaller value is sent as it is.
func TestHelperTimeoutMs(t *testing.T) {
	for _, tc := range []struct{ in, want int }{
		{1, 1},
		{600_000, 600_000},
		{maxHelperTimeoutMs, maxHelperTimeoutMs},
		{maxHelperTimeoutMs + 1, maxHelperTimeoutMs},
		{3_000_000_000, maxHelperTimeoutMs},
	} {
		if got := helperTimeoutMs(tc.in); got != tc.want {
			t.Errorf("helperTimeoutMs(%d) = %d, want %d", tc.in, got, tc.want)
		}
	}
	if maxHelperTimeoutMs+1000 != 2147483647 {
		t.Fatal("the cap plus the helper's 1000 ms grace must be the largest JS timer delay")
	}
}
