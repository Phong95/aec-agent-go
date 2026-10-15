package docker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"sync"
	"testing"

	"github.com/Phong95/aec-agent-go/kernel"
)

// summariseLimit is the length above which a recorded stdin becomes its byte length
// and sha256 (wiregen dockerInputSummary).
const summariseLimit = 1000

// reply is one scripted answer of the docker CLI: a result (code, stdout, stderr; a
// null code is a client killed by a signal) or an error the runner returns (a call that
// fails to run).
type reply struct {
	Code   *int   `json:"code"`
	Stdout string `json:"stdout"`
	Stderr string `json:"stderr"`
	Error  *struct {
		Message string `json:"message"`
		Code    string `json:"code"`
	} `json:"error"`
}

// callRecord is one docker call as wiregen records it: the arguments, the stdin (null,
// the text, or {length, sha256} for a long one), the timeout in ms (null for none) and
// the sandbox id (null for none).
type callRecord struct {
	Args      []string `json:"args"`
	Input     any      `json:"input"`
	TimeoutMs *int64   `json:"timeoutMs"`
	SandboxID *string  `json:"sandboxId"`
}

// fakeRunner replays a reply queue and records the calls. A call the script does not
// cover fails the test: the fixture records exactly the calls expected, so an extra
// call is a behaviour difference.
type fakeRunner struct {
	t     *testing.T
	mu    sync.Mutex
	queue []reply
	calls []callRecord
	// onStart answers Start (the exec stream); nil fails the test on any Start.
	onStart func(ctx context.Context, args []string) (Process, error)
}

func newFakeRunner(t *testing.T) *fakeRunner {
	t.Helper()
	return &fakeRunner{t: t}
}

// script replaces the reply queue and forgets the calls recorded so far.
func (f *fakeRunner) script(replies []reply) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.queue = append([]reply(nil), replies...)
	f.calls = nil
}

func (f *fakeRunner) recorded() []callRecord {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]callRecord(nil), f.calls...)
}

func (f *fakeRunner) remaining() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.queue)
}

func summariseInput(input string) any {
	switch {
	case input == "":
		return nil
	case len(input) > summariseLimit:
		sum := sha256.Sum256([]byte(input))
		return map[string]any{"length": float64(len(input)), "sha256": hex.EncodeToString(sum[:])}
	default:
		return input
	}
}

// Run records the call and answers from the queue.
func (f *fakeRunner) Run(_ context.Context, args []string, opts RunOptions) (RunResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	rec := callRecord{Args: append([]string(nil), args...), Input: summariseInput(opts.Input)}
	if opts.Timeout > 0 {
		ms := opts.Timeout.Milliseconds()
		rec.TimeoutMs = &ms
	}
	if opts.SandboxID != "" {
		id := opts.SandboxID
		rec.SandboxID = &id
	}
	f.calls = append(f.calls, rec)
	if len(f.queue) == 0 {
		f.t.Errorf("unexpected docker call: docker %q", args)
		return RunResult{}, errors.New("unscripted docker call")
	}
	r := f.queue[0]
	f.queue = f.queue[1:]
	if r.Error != nil {
		return RunResult{}, kernel.NewSandboxError(r.Error.Message, kernel.SandboxErrorCode(r.Error.Code), opts.SandboxID)
	}
	return RunResult{Code: r.Code, Stdout: r.Stdout, Stderr: r.Stderr}, nil
}

// Start hands out the scripted client of the exec stream. A test that does not expect
// a client leaves onStart nil and a Start fails it.
func (f *fakeRunner) Start(ctx context.Context, args []string) (Process, error) {
	f.mu.Lock()
	start := f.onStart
	f.mu.Unlock()
	if start == nil {
		f.t.Errorf("unexpected Start of a docker client: docker %q", args)
		return nil, errors.New("no clients in this fake")
	}
	return start(ctx, args)
}

// funcRunner is a Runner made of a function, for tests that need behaviour no queue
// can express (a call that blocks, a call that counts).
type funcRunner struct {
	run func(ctx context.Context, args []string, opts RunOptions) (RunResult, error)
}

func (f funcRunner) Run(ctx context.Context, args []string, opts RunOptions) (RunResult, error) {
	return f.run(ctx, args, opts)
}

func (funcRunner) Start(context.Context, []string) (Process, error) {
	return nil, errors.New("no clients in this fake")
}
