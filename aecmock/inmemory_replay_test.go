package aecmock_test

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Phong95/aec-agent-go/aecmock"
	"github.com/Phong95/aec-agent-go/components/sandboxhub"
	"github.com/Phong95/aec-agent-go/internal/wiretest"
	"github.com/Phong95/aec-agent-go/kernel"
)

// The golden fixtures inmemory-sandbox-files.json and inmemory-sandbox-exec.json are
// scripts: a list of ops, each recorded by wiregen from a reference in-memory sandbox
// with its input fields and its result or error. The replayer below runs the same
// ops through the in-memory sandbox with the same deterministic clock and compares every
// result, error code, error message, timestamp and duration.

type replayError struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	SandboxID string `json:"sandboxId"`
}

type replayOutput struct {
	Stream string `json:"stream"`
	Data   string `json:"data"`
}

type replayOp struct {
	Op       string          `json:"op"`
	Provider string          `json:"provider"`
	Name     string          `json:"name"`
	Options  json.RawMessage `json:"options"`
	Sandbox  string          `json:"sandbox"`
	ID       string          `json:"id"`
	Tenancy  string          `json:"tenancy"`
	AgentID  string          `json:"agentId"`
	Spec     json.RawMessage `json:"spec"`
	View     string          `json:"view"`
	Path     string          `json:"path"`
	Content  string          `json:"content"`
	Opts     json.RawMessage `json:"opts"`
	Query    json.RawMessage `json:"query"`
	Aborted  bool            `json:"aborted"`
	Link     string          `json:"link"`
	Target   string          `json:"target"`
	Request  json.RawMessage `json:"request"`

	RemoveData    *bool `json:"removeData"`
	CancelAfterMs int   `json:"cancelAfterMs"`
	CancelBefore  bool  `json:"cancelBefore"`
	// JSEngineMessage marks a row whose error text is the JS engine's own message,
	// which Go cannot reproduce: only the code and the "Invalid regular expression: "
	// prefix are compared.
	JSEngineMessage bool `json:"jsEngineMessage"`

	Result  json.RawMessage `json:"result"`
	Error   *replayError    `json:"error"`
	Outputs []replayOutput  `json:"outputs"`
}

type replayScript struct {
	Ops []replayOp `json:"ops"`
}

func loadReplayScript(t *testing.T, name string) replayScript {
	t.Helper()
	var s replayScript
	if err := json.Unmarshal(wiretest.Load(t, name), &s); err != nil {
		t.Fatalf("decode %s: %v", name, err)
	}
	if len(s.Ops) == 0 {
		t.Fatalf("%s has no ops", name)
	}
	return s
}

type providerOptions struct {
	Kind         sandboxhub.Kind          `json:"kind"`
	Limits       sandboxhub.LimitsPatch   `json:"limits"`
	Capabilities *[]sandboxhub.Capability `json:"capabilities"`
	FailCreates  int                      `json:"failCreates"`
}

type replayer struct {
	t         *testing.T
	clock     func() int64
	providers map[string]*aecmock.InMemoryProvider
	sandboxes map[string]sandboxhub.Sandbox
	views     map[string]sandboxhub.View

	lastOutputs []replayOutput
}

func newReplayer(t *testing.T) *replayer {
	return &replayer{
		t:         t,
		clock:     wiretest.Clock(1700000000000, 1),
		providers: map[string]*aecmock.InMemoryProvider{},
		sandboxes: map[string]sandboxhub.Sandbox{},
		views:     map[string]sandboxhub.View{},
	}
}

func (r *replayer) provider(name string) *aecmock.InMemoryProvider {
	if name == "" {
		name = "p"
	}
	p, ok := r.providers[name]
	if !ok {
		r.t.Fatalf("script uses provider %q before creating it", name)
	}
	return p
}

func (r *replayer) sandbox(name string) sandboxhub.Sandbox {
	sb, ok := r.sandboxes[name]
	if !ok {
		r.t.Fatalf("script uses sandbox %q before creating it", name)
	}
	return sb
}

func (r *replayer) view(name string) sandboxhub.View {
	v, ok := r.views[name]
	if !ok {
		r.t.Fatalf("script uses view %q before creating it", name)
	}
	return v
}

func decodeInto(t *testing.T, raw json.RawMessage, v any) {
	t.Helper()
	if len(raw) == 0 {
		return
	}
	if err := json.Unmarshal(raw, v); err != nil {
		t.Fatalf("decode %s: %v", raw, err)
	}
}

// opContext is a cancelled context for an aborted op, a live one otherwise.
func opContext(aborted bool) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(context.Background())
	if aborted {
		cancel()
	}
	return ctx, cancel
}

func viewInfo(v sandboxhub.View) map[string]any {
	return map[string]any{
		"id": v.ID(), "traits": v.Traits(), "workingDir": v.WorkingDir(), "roots": v.Roots(),
		"capabilities": v.Capabilities(), "limits": v.Limits(),
	}
}

// run executes one op and returns its Go result (nil for the ops that return
// nothing) and error.
func (r *replayer) run(op replayOp) (any, error) {
	t := r.t
	switch op.Op {
	case "provider":
		var po providerOptions
		decodeInto(t, op.Options, &po)
		opts := []aecmock.InMemoryOption{aecmock.WithClock(r.clock)}
		if po.Kind != "" {
			opts = append(opts, aecmock.WithKind(po.Kind))
		}
		opts = append(opts, aecmock.WithLimits(po.Limits), aecmock.WithFailCreates(po.FailCreates))
		if po.Capabilities != nil {
			opts = append(opts, aecmock.WithCapabilities(*po.Capabilities...))
		}
		p := aecmock.NewInMemoryProvider(opts...)
		r.providers[op.Name] = p
		return map[string]any{"name": p.Name(), "version": p.Version(), "kind": p.Kind()}, nil
	case "create":
		spec := sandboxhub.Spec{Kind: sandboxhub.KindDocker}
		decodeInto(t, op.Spec, &spec)
		cc := sandboxhub.CreateContext{ID: op.ID, Tenancy: sandboxhub.Tenancy(op.Tenancy), AgentID: op.AgentID}
		env, err := r.provider(op.Provider).Create(context.Background(), spec, cc)
		if err != nil {
			return nil, err
		}
		r.sandboxes[op.Sandbox] = env
		return map[string]any{"id": env.ID(), "traits": env.Traits()}, nil
	case "forAgent":
		v, err := r.sandbox(op.Sandbox).ForAgent(context.Background(), op.AgentID)
		if err != nil {
			return nil, err
		}
		r.views[op.View] = v
		return viewInfo(v), nil
	case "symlink":
		return nil, r.sandbox(op.Sandbox).(*aecmock.InMemorySandbox).Symlink(op.Link, op.Target)
	case "plantFile":
		return nil, r.sandbox(op.Sandbox).(*aecmock.InMemorySandbox).PlantFile(op.Path, op.Content)
	case "readFile":
		var o sandboxhub.ReadOptions
		decodeInto(t, op.Opts, &o)
		ctx, cancel := opContext(op.Aborted)
		defer cancel()
		return r.view(op.View).ReadFile(ctx, op.Path, o)
	case "writeFile":
		var o sandboxhub.WriteOptions
		decodeInto(t, op.Opts, &o)
		ctx, cancel := opContext(op.Aborted)
		defer cancel()
		return r.view(op.View).WriteFile(ctx, op.Path, op.Content, o)
	case "stat":
		ctx, cancel := opContext(op.Aborted)
		defer cancel()
		return r.view(op.View).Stat(ctx, op.Path)
	case "list":
		var o sandboxhub.ListOptions
		decodeInto(t, op.Opts, &o)
		ctx, cancel := opContext(op.Aborted)
		defer cancel()
		return r.view(op.View).List(ctx, op.Path, o)
	case "search":
		var q sandboxhub.SearchQuery
		decodeInto(t, op.Query, &q)
		ctx, cancel := opContext(op.Aborted)
		defer cancel()
		return r.view(op.View).Search(ctx, q)
	case "exec":
		return r.runExec(op)
	case "removeAgentData":
		return nil, r.sandbox(op.Sandbox).(sandboxhub.AgentDataRemover).RemoveAgentData(context.Background(), op.AgentID)
	case "dispose":
		return nil, r.sandbox(op.Sandbox).Dispose(context.Background(), op.RemoveData != nil && *op.RemoveData)
	case "health":
		return r.sandbox(op.Sandbox).Health(context.Background()), nil
	case "disposed":
		return r.sandbox(op.Sandbox).(*aecmock.InMemorySandbox).Disposed(), nil
	case "removeData":
		p := r.provider(op.Provider)
		cc := sandboxhub.CreateContext{ID: op.ID, Tenancy: sandboxhub.Tenancy(op.Tenancy)}
		if err := p.RemoveData(context.Background(), sandboxhub.Spec{Kind: sandboxhub.KindDocker}, cc, op.AgentID); err != nil {
			return nil, err
		}
		removed := []map[string]any{}
		for _, rd := range p.RemovedData() {
			m := map[string]any{"id": rd.ID}
			if rd.AgentID != "" {
				m["agentId"] = rd.AgentID
			}
			removed = append(removed, m)
		}
		_, has := p.Sandbox(op.ID)
		return map[string]any{"removedData": removed, "has": has}, nil
	case "createCount":
		return r.provider(op.Provider).CreateCount(), nil
	default:
		t.Fatalf("script has an unknown op %q", op.Op)
		return nil, nil
	}
}

// runExec runs an exec op, cancelling the context after the recorded delay (or
// before the call), and returns the result with the pieces the OnOutput callback
// received in the Outputs field of a copy of the op.
func (r *replayer) runExec(op replayOp) (any, error) {
	var req sandboxhub.ExecRequest
	decodeInto(r.t, op.Request, &req)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if op.CancelBefore {
		cancel()
	}
	if op.CancelAfterMs > 0 {
		timer := time.AfterFunc(time.Duration(op.CancelAfterMs)*time.Millisecond, cancel)
		defer timer.Stop()
	}
	var mu sync.Mutex
	var outputs []replayOutput
	res, err := r.view(op.View).Exec(ctx, req, sandboxhub.ExecOptions{OnOutput: func(stream sandboxhub.OutputStream, data string) {
		mu.Lock()
		defer mu.Unlock()
		outputs = append(outputs, replayOutput{Stream: string(stream), Data: data})
	}})
	mu.Lock()
	r.lastOutputs = outputs
	mu.Unlock()
	return res, err
}

// toGeneric encodes v with the wire encoder and decodes it again, so Go values and
// the recorded JSON compare as plain data.
func toGeneric(t *testing.T, v any) any {
	t.Helper()
	enc, err := kernel.EncodeJSON(v)
	if err != nil {
		t.Fatalf("encode %T: %v", v, err)
	}
	var out any
	if err := json.Unmarshal(enc, &out); err != nil {
		t.Fatalf("decode %s: %v", enc, err)
	}
	return out
}

// replay runs every op of the script and compares it with the record.
func replay(t *testing.T, script replayScript) {
	t.Helper()
	r := newReplayer(t)
	var errorRows, okRows int
	for i, op := range script.Ops {
		label := op.Op
		if op.Path != "" {
			label += " " + op.Path
		}
		got, err := r.run(op)
		if op.Error != nil {
			errorRows++
			requireReplayError(t, i, label, err, op)
			continue
		}
		okRows++
		if err != nil {
			t.Errorf("op %d (%s): unexpected error %v", i, label, err)
			continue
		}
		var want any
		if err := json.Unmarshal(op.Result, &want); err != nil {
			t.Fatalf("op %d (%s): bad recorded result %s", i, label, op.Result)
		}
		if g := toGeneric(t, got); !reflect.DeepEqual(g, want) {
			t.Errorf("op %d (%s) differs from the real TS result\n got: %s\nwant: %s", i, label, mustJSON(t, g), op.Result)
		}
		if op.Op == "exec" {
			requireOutputs(t, i, label, r.lastOutputs, op.Outputs)
		}
	}
	t.Logf("replayed %d ops (%d results, %d errors)", len(script.Ops), okRows, errorRows)
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(b)
}

func requireOutputs(t *testing.T, i int, label string, got, want []replayOutput) {
	t.Helper()
	if len(got) == 0 && len(want) == 0 {
		return
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("op %d (%s): OnOutput pieces differ\n got: %q\nwant: %q", i, label, got, want)
	}
}

func requireReplayError(t *testing.T, i int, label string, err error, op replayOp) {
	t.Helper()
	want := op.Error
	if err == nil {
		t.Errorf("op %d (%s): want the TS error %s %q, got none", i, label, want.Code, want.Message)
		return
	}
	var se *kernel.SandboxError
	isSandbox := errors.As(err, &se)
	if want.Code == "" {
		if isSandbox {
			t.Errorf("op %d (%s): want a plain error %q, got SandboxError %s", i, label, want.Message, se.Code)
		} else if err.Error() != want.Message {
			t.Errorf("op %d (%s): error %q, want %q", i, label, err.Error(), want.Message)
		}
		return
	}
	if !isSandbox {
		t.Errorf("op %d (%s): error %v is not a SandboxError; want %s %q", i, label, err, want.Code, want.Message)
		return
	}
	if string(se.Code) != want.Code {
		t.Errorf("op %d (%s): code %s, want %s (%q)", i, label, se.Code, want.Code, want.Message)
	}
	if se.SandboxID != want.SandboxID {
		t.Errorf("op %d (%s): sandbox id %q, want %q", i, label, se.SandboxID, want.SandboxID)
	}
	if op.JSEngineMessage {
		if !strings.HasPrefix(se.Message, "Invalid regular expression: ") {
			t.Errorf("op %d (%s): message %q lacks the prefix of %q", i, label, se.Message, want.Message)
		}
		return
	}
	if se.Message != want.Message {
		t.Errorf("op %d (%s): message %q, want %q", i, label, se.Message, want.Message)
	}
}
