package host_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/Phong95/aec-agent-go/components/sandboxhub"
	"github.com/Phong95/aec-agent-go/components/sandboxhub/host"
	"github.com/Phong95/aec-agent-go/internal/wiretest"
	"github.com/Phong95/aec-agent-go/kernel"
)

// host-sandbox-ops.json is a script recorded by wiregen from a reference
// host sandbox provider over a temporary folder: a list of ops, each with its input
// fields and its result or error. TestHostOpsMatchGolden runs the same ops on a Go host
// sandbox over t.TempDir() and compares every result, error code, error message and
// sandbox id. The script holds only operations whose outcome does not depend on the
// operating system: stat results have no modifiedAt, traits have no os, and views
// record no capabilities (they depend on the machine, and exec arrived in plan 07-05).

type hostRecordError struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	SandboxID string `json:"sandboxId"`
}

type hostOp struct {
	Op            string          `json:"op"`
	Provider      string          `json:"provider"`
	Name          string          `json:"name"`
	Options       json.RawMessage `json:"options"`
	Root          string          `json:"root"`
	Sandbox       string          `json:"sandbox"`
	ID            string          `json:"id"`
	Tenancy       string          `json:"tenancy"`
	AgentID       string          `json:"agentId"`
	RemoveAgentID string          `json:"removeAgentId"`
	Spec          json.RawMessage `json:"spec"`
	View          string          `json:"view"`
	Path          string          `json:"path"`
	Content       string          `json:"content"`
	Opts          json.RawMessage `json:"opts"`
	Query         json.RawMessage `json:"query"`
	Aborted       bool            `json:"aborted"`
	RemoveData    *bool           `json:"removeData"`
	// JSEngineMessage marks a row whose error text is the JS engine's own regular
	// expression message, which Go cannot reproduce: only the code and the
	// "Invalid regular expression: " prefix are compared.
	JSEngineMessage bool `json:"jsEngineMessage"`

	Result json.RawMessage  `json:"result"`
	Error  *hostRecordError `json:"error"`
}

type hostScript struct {
	Ops []hostOp `json:"ops"`
}

type hostProviderOptions struct {
	Limits sandboxhub.LimitsPatch `json:"limits"`
}

type hostReplayer struct {
	t         *testing.T
	root      string
	providers map[string]*host.Provider
	sandboxes map[string]sandboxhub.Sandbox
	views     map[string]sandboxhub.View
}

func decodeRaw(t *testing.T, raw json.RawMessage, v any) {
	t.Helper()
	if len(raw) == 0 {
		return
	}
	if err := json.Unmarshal(raw, v); err != nil {
		t.Fatalf("decode %s: %v", raw, err)
	}
}

func opContext(aborted bool) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(context.Background())
	if aborted {
		cancel()
	}
	return ctx, cancel
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

// traitsOf is the traits the script records: everything but the os.
func traitsOf(tr sandboxhub.Traits) map[string]any {
	return map[string]any{
		"kind": tr.Kind, "isolated": tr.Isolated, "network": tr.Network, "persistent": tr.Persistent, "tenancy": tr.Tenancy,
	}
}

func (r *hostReplayer) provider(name string) *host.Provider {
	if name == "" {
		name = "p"
	}
	p, ok := r.providers[name]
	if !ok {
		r.t.Fatalf("script uses provider %q before creating it", name)
	}
	return p
}

func (r *hostReplayer) sandbox(name string) sandboxhub.Sandbox {
	sb, ok := r.sandboxes[name]
	if !ok {
		r.t.Fatalf("script uses sandbox %q before creating it", name)
	}
	return sb
}

func (r *hostReplayer) view(name string) sandboxhub.View {
	v, ok := r.views[name]
	if !ok {
		r.t.Fatalf("script uses view %q before creating it", name)
	}
	return v
}

// run executes one op and returns its Go result and error.
func (r *hostReplayer) run(op hostOp) (any, error) {
	t := r.t
	switch op.Op {
	case "provider":
		var po hostProviderOptions
		decodeRaw(t, op.Options, &po)
		p, err := host.New(host.Options{Root: r.root, Limits: po.Limits})
		if err != nil {
			return nil, err
		}
		r.providers[op.Name] = p
		return map[string]any{"name": p.Name(), "version": p.Version(), "kind": p.Kind()}, nil
	case "providerRoot":
		if _, err := host.New(host.Options{Root: op.Root}); err != nil {
			return nil, err
		}
		return "constructed", nil
	case "create":
		spec := sandboxhub.Spec{Kind: sandboxhub.KindHost}
		decodeRaw(t, op.Spec, &spec)
		cc := sandboxhub.CreateContext{ID: op.ID, Tenancy: sandboxhub.Tenancy(op.Tenancy), AgentID: op.AgentID}
		sb, err := r.provider(op.Provider).Create(context.Background(), spec, cc)
		if err != nil {
			return nil, err
		}
		r.sandboxes[op.Sandbox] = sb
		return map[string]any{"id": sb.ID(), "traits": traitsOf(sb.Traits())}, nil
	case "forAgent":
		v, err := r.sandbox(op.Sandbox).ForAgent(context.Background(), op.AgentID)
		if err != nil {
			return nil, err
		}
		r.views[op.View] = v
		return map[string]any{
			"id": v.ID(), "traits": traitsOf(v.Traits()), "workingDir": v.WorkingDir(), "roots": v.Roots(), "limits": v.Limits(),
		}, nil
	case "readFile":
		var o sandboxhub.ReadOptions
		decodeRaw(t, op.Opts, &o)
		ctx, cancel := opContext(op.Aborted)
		defer cancel()
		return r.view(op.View).ReadFile(ctx, op.Path, o)
	case "writeFile":
		var o sandboxhub.WriteOptions
		decodeRaw(t, op.Opts, &o)
		ctx, cancel := opContext(op.Aborted)
		defer cancel()
		return r.view(op.View).WriteFile(ctx, op.Path, op.Content, o)
	case "stat":
		ctx, cancel := opContext(op.Aborted)
		defer cancel()
		st, err := r.view(op.View).Stat(ctx, op.Path)
		if err != nil || st == nil {
			return nil, err
		}
		return map[string]any{"path": st.Path, "type": st.Type, "size": st.Size}, nil
	case "list":
		var o sandboxhub.ListOptions
		decodeRaw(t, op.Opts, &o)
		ctx, cancel := opContext(op.Aborted)
		defer cancel()
		return r.view(op.View).List(ctx, op.Path, o)
	case "search":
		var q sandboxhub.SearchQuery
		decodeRaw(t, op.Query, &q)
		ctx, cancel := opContext(op.Aborted)
		defer cancel()
		return r.view(op.View).Search(ctx, q)
	case "exists":
		base := r.sandbox(op.Sandbox).(*host.Sandbox).Base()
		target := base
		if op.Path != "" {
			target = filepath.Join(append([]string{base}, strings.Split(op.Path, "/")...)...)
		}
		_, err := os.Stat(target)
		return err == nil, nil
	case "removeAgentData":
		return nil, r.sandbox(op.Sandbox).(sandboxhub.AgentDataRemover).RemoveAgentData(context.Background(), op.AgentID)
	case "dispose":
		return nil, r.sandbox(op.Sandbox).Dispose(context.Background(), op.RemoveData != nil && *op.RemoveData)
	case "health":
		return r.sandbox(op.Sandbox).Health(context.Background()), nil
	case "removeData":
		cc := sandboxhub.CreateContext{ID: op.ID, Tenancy: sandboxhub.Tenancy(op.Tenancy), AgentID: op.AgentID}
		return nil, r.provider(op.Provider).RemoveData(context.Background(), sandboxhub.Spec{Kind: sandboxhub.KindHost}, cc, op.RemoveAgentID)
	default:
		t.Fatalf("script has an unknown op %q", op.Op)
		return nil, nil
	}
}

func requireRecordedError(t *testing.T, i int, label string, err error, op hostOp) {
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

// TestHostOpsMatchGolden replays every row of host-sandbox-ops.json on a Go host sandbox
// over a temporary root.
func TestHostOpsMatchGolden(t *testing.T) {
	var script hostScript
	if err := json.Unmarshal(wiretest.Load(t, "host-sandbox-ops.json"), &script); err != nil {
		t.Fatalf("decode host-sandbox-ops.json: %v", err)
	}
	if len(script.Ops) == 0 {
		t.Fatal("host-sandbox-ops.json has no ops")
	}
	r := &hostReplayer{
		t: t, root: t.TempDir(),
		providers: map[string]*host.Provider{}, sandboxes: map[string]sandboxhub.Sandbox{}, views: map[string]sandboxhub.View{},
	}
	var okRows, errorRows int
	for i, op := range script.Ops {
		label := op.Op
		if op.Path != "" {
			label += " " + op.Path
		}
		got, err := r.run(op)
		if op.Error != nil {
			errorRows++
			requireRecordedError(t, i, label, err, op)
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
			t.Errorf("op %d (%s) differs from the real TS result\n got: %v\nwant: %s", i, label, g, op.Result)
		}
	}
	t.Logf("replayed %d ops (%d results, %d errors)", len(script.Ops), okRows, errorRows)
	if errorRows == 0 || okRows == 0 {
		t.Errorf("the script must hold results and errors, got %d and %d", okRows, errorRows)
	}
}

// hostFixtures is every golden fixture owned by package host.
var hostFixtures = []string{
	"host-sandbox-ops.json",
}

// TestHostFixturesCovered fails when a fixture with the prefix host- has no entry
// above, and when an entry has no file, so a fixture added to wiregen cannot go
// unchecked.
func TestHostFixturesCovered(t *testing.T) {
	entries, err := os.ReadDir(wiretest.Dir(t))
	if err != nil {
		t.Fatalf("read fixture dir: %v", err)
	}
	present := map[string]bool{}
	for _, e := range entries {
		present[e.Name()] = true
		if strings.HasPrefix(e.Name(), "host-") && !slices.Contains(hostFixtures, e.Name()) {
			t.Errorf("fixture %s has no entry in hostFixtures", e.Name())
		}
	}
	for _, f := range hostFixtures {
		if !present[f] {
			t.Errorf("hostFixtures entry %s has no fixture file", f)
		}
	}
}
