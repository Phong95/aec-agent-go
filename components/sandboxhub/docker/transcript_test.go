package docker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/Phong95/aec-agent-go/components/sandboxhub"
	"github.com/Phong95/aec-agent-go/internal/jsonx"
	"github.com/Phong95/aec-agent-go/internal/wiretest"
	"github.com/Phong95/aec-agent-go/kernel"
)

// docker-transcripts.json and docker-view-transcripts.json are recorded by wiregen from
// a reference provider whose cli is replaced by a recorder (no docker daemon is ever
// involved). A scenario is a list of steps on one provider; a step holds the replies
// the recorder gave in order, the docker calls that were made (arguments, stdin,
// timeout, sandbox id) and its outcome. TestTranscriptsMatchGolden and
// TestViewTranscriptsMatchGolden run the same steps through a fake Runner that gives the
// same replies, and the provider must make the same calls and give the same outcome.

type repeat struct {
	Text  string `json:"text"`
	Count int    `json:"count"`
}

// stepArgs are the arguments of every action; an action reads the ones it needs.
type stepArgs struct {
	Spec          *sandboxhub.Spec          `json:"spec"`
	Context       *sandboxhub.CreateContext `json:"context"`
	AgentID       string                    `json:"agentId"`
	Image         string                    `json:"image"`
	SandboxID     string                    `json:"sandboxId"`
	RemoveData    *bool                     `json:"removeData"`
	Agent         *string                   `json:"agent"`
	Path          string                    `json:"path"`
	Content       string                    `json:"content"`
	ContentRepeat *repeat                   `json:"contentRepeat"`
	Opts          json.RawMessage           `json:"opts"`
	Query         json.RawMessage           `json:"query"`
	// Request and PreAborted belong to the exec action (docker-exec-transcripts.json).
	Request    json.RawMessage `json:"request"`
	PreAborted bool            `json:"preAborted"`
}

type outcomeError struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	SandboxID string `json:"sandboxId"`
}

type outcome struct {
	Ok    any           `json:"ok"`
	Error *outcomeError `json:"error"`
}

type step struct {
	Action  string       `json:"action"`
	Args    stepArgs     `json:"args"`
	Replies []reply      `json:"replies"`
	Calls   []callRecord `json:"calls"`
	Outcome outcome      `json:"outcome"`
	// Spawns are the scripted docker clients of an exec step, in the order the exec
	// stream starts them, and Execution is what was recorded of them.
	Spawns    []spawnScript    `json:"spawns"`
	Execution *executionRecord `json:"execution"`
}

type scenario struct {
	Name    string                   `json:"name"`
	Options optionsFixture           `json:"options"`
	Spec    sandboxhub.Spec          `json:"spec"`
	Context sandboxhub.CreateContext `json:"context"`
	Steps   []step                   `json:"steps"`
}

type scenarioFile struct {
	Scenarios []scenario `json:"scenarios"`
}

func loadScenarios(t *testing.T, name string) []scenario {
	t.Helper()
	var f scenarioFile
	if err := jsonx.UnmarshalStrict(wiretest.Load(t, name), &f); err != nil {
		t.Fatalf("decode %s: %v", name, err)
	}
	if len(f.Scenarios) == 0 {
		t.Fatalf("%s has no scenarios", name)
	}
	return f.Scenarios
}

// replayer runs the steps of one scenario on a Go provider over a fake Runner.
type replayer struct {
	t        *testing.T
	sc       scenario
	runner   *fakeRunner
	provider *Provider
	sb       *Sandbox
	views    map[string]sandboxhub.View
	// observed is what the Go exec stream did in the exec step that ran last.
	observed *executionRecord
}

func newReplayer(t *testing.T, sc scenario) *replayer {
	t.Helper()
	runner := newFakeRunner(t)
	opts := sc.Options.options()
	opts.Runner = runner
	p, err := New(opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return &replayer{t: t, sc: sc, runner: runner, provider: p, views: map[string]sandboxhub.View{}}
}

// toAny turns a Go value into the JSON-decoded form the fixture holds, so the two can
// be compared with reflect.DeepEqual.
func toAny(t *testing.T, v any) any {
	t.Helper()
	raw, err := jsonx.Marshal(v)
	if err != nil {
		t.Fatalf("encode %T: %v", v, err)
	}
	var out any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode %s: %v", raw, err)
	}
	return out
}

// do runs one step and returns the value and the error of its action.
func (r *replayer) do(ctx context.Context, s step) (any, error) {
	a := s.Args
	spec, cc := r.sc.Spec, r.sc.Context
	if a.Spec != nil {
		spec = *a.Spec
	}
	if a.Context != nil {
		cc = *a.Context
	}
	switch s.Action {
	case "create":
		sb, err := r.provider.Create(ctx, spec, cc)
		if err != nil {
			return nil, err
		}
		r.sb = sb.(*Sandbox)
		return map[string]any{
			"id": r.sb.ID(), "image": r.sb.Image(), "containerName": r.sb.ContainerName(), "volumeName": r.sb.VolumeName(),
			"traits": toAny(r.t, r.sb.Traits()),
		}, nil
	case "removeData":
		return nil, r.provider.RemoveData(ctx, spec, cc, a.AgentID)
	case "checkAvailable":
		av := r.provider.CheckAvailable(ctx)
		if av.OK {
			return map[string]any{"ok": true, "version": av.Version}, nil
		}
		return map[string]any{"ok": false, "reason": av.Reason}, nil
	case "ensureImage":
		return nil, r.provider.EnsureImage(ctx, a.Image, a.SandboxID)
	case "dispose":
		return nil, r.sb.Dispose(ctx, a.RemoveData != nil && *a.RemoveData)
	case "health":
		return string(r.sb.Health(ctx)), nil
	case "forAgent":
		v, err := r.sb.ForAgent(ctx, *a.Agent)
		if err != nil {
			return nil, err
		}
		r.views[*a.Agent] = v
		return map[string]any{"workingDir": v.WorkingDir(), "roots": toAny(r.t, v.Roots())}, nil
	case "removeAgentData":
		return nil, r.sb.RemoveAgentData(ctx, *a.Agent)
	case "readFile":
		var opts sandboxhub.ReadOptions
		r.decodeOpts(a.Opts, &opts)
		out, err := r.views[*a.Agent].ReadFile(ctx, a.Path, opts)
		return out, err
	case "writeFile":
		var opts sandboxhub.WriteOptions
		r.decodeOpts(a.Opts, &opts)
		content := a.Content
		if a.ContentRepeat != nil {
			content = strings.Repeat(a.ContentRepeat.Text, a.ContentRepeat.Count)
		}
		out, err := r.views[*a.Agent].WriteFile(ctx, a.Path, content, opts)
		return out, err
	case "stat":
		out, err := r.views[*a.Agent].Stat(ctx, a.Path)
		return out, err
	case "list":
		var opts sandboxhub.ListOptions
		r.decodeOpts(a.Opts, &opts)
		out, err := r.views[*a.Agent].List(ctx, a.Path, opts)
		return out, err
	case "exec":
		return r.doExec(ctx, s)
	case "search":
		var q sandboxhub.SearchQuery
		if err := jsonx.UnmarshalStrict(a.Query, &q); err != nil {
			r.t.Fatalf("decode search query %s: %v", a.Query, err)
		}
		out, err := r.views[*a.Agent].Search(ctx, q)
		return out, err
	default:
		r.t.Fatalf("unknown step action %q", s.Action)
		return nil, nil
	}
}

func (r *replayer) decodeOpts(raw json.RawMessage, into any) {
	r.t.Helper()
	if len(raw) == 0 || string(raw) == "null" {
		return
	}
	if err := jsonx.UnmarshalStrict(raw, into); err != nil {
		r.t.Fatalf("decode options %s: %v", raw, err)
	}
}

// sameInput compares a recorded stdin: a text that is a JSON object (a helper
// request) is compared as parsed JSON, because the helper ignores key order; anything
// else (a script, a Dockerfile, the length and sha256 of a long input) must be equal.
func sameInput(got, want any) bool {
	if reflect.DeepEqual(got, want) {
		return true
	}
	gs, gok := got.(string)
	ws, wok := want.(string)
	if !gok || !wok || !strings.HasPrefix(ws, "{") {
		return false
	}
	var g, w any
	if json.Unmarshal([]byte(gs), &g) != nil || json.Unmarshal([]byte(ws), &w) != nil {
		return false
	}
	return reflect.DeepEqual(g, w)
}

func (r *replayer) compareCalls(label string, got, want []callRecord) {
	r.t.Helper()
	if len(got) != len(want) {
		r.t.Errorf("%s: %d docker calls, TS made %d\n got: %v\nwant: %v", label, len(got), len(want), callArgs(got), callArgs(want))
		return
	}
	for i := range want {
		g, w := got[i], want[i]
		if !reflect.DeepEqual(g.Args, w.Args) {
			r.t.Errorf("%s: call %d arguments\n got: %q\nwant: %q", label, i, g.Args, w.Args)
		}
		if !sameInput(g.Input, w.Input) {
			r.t.Errorf("%s: call %d (docker %s) stdin\n got: %v\nwant: %v", label, i, firstArg(w.Args), shorten(g.Input), shorten(w.Input))
		}
		if !reflect.DeepEqual(g.TimeoutMs, w.TimeoutMs) {
			r.t.Errorf("%s: call %d (docker %s) timeout = %v ms, TS used %v ms", label, i, firstArg(w.Args), deref(g.TimeoutMs), deref(w.TimeoutMs))
		}
		if !reflect.DeepEqual(g.SandboxID, w.SandboxID) {
			r.t.Errorf("%s: call %d (docker %s) sandbox id = %v, TS used %v", label, i, firstArg(w.Args), deref(g.SandboxID), deref(w.SandboxID))
		}
	}
}

func callArgs(calls []callRecord) [][]string {
	out := make([][]string, len(calls))
	for i, c := range calls {
		out[i] = c.Args
	}
	return out
}

func deref[T any](p *T) any {
	if p == nil {
		return nil
	}
	return *p
}

func shorten(v any) any {
	if s, ok := v.(string); ok && len(s) > 200 {
		return s[:200] + "..."
	}
	return v
}

func (r *replayer) compareOutcome(label string, value any, err error, want outcome) {
	r.t.Helper()
	if want.Error != nil {
		if err == nil {
			r.t.Errorf("%s: no error, TS failed with {%s %q}", label, want.Error.Code, want.Error.Message)
			return
		}
		var se *kernel.SandboxError
		if want.Error.Code == "" {
			// A plain error (no code): only the message is comparable.
			if err.Error() != want.Error.Message {
				r.t.Errorf("%s: error %q, TS threw %q", label, err.Error(), want.Error.Message)
			}
			return
		}
		if !errors.As(err, &se) {
			r.t.Errorf("%s: error %v is not a SandboxError, TS gave {%s %q}", label, err, want.Error.Code, want.Error.Message)
			return
		}
		if string(se.Code) != want.Error.Code || se.Message != want.Error.Message || se.SandboxID != want.Error.SandboxID {
			r.t.Errorf("%s: error {%s %q %q}\nTS gave  {%s %q %q}", label, se.Code, se.Message, se.SandboxID, want.Error.Code, want.Error.Message, want.Error.SandboxID)
		}
		return
	}
	if err != nil {
		r.t.Errorf("%s: unexpected error %v, TS succeeded with %v", label, err, shorten(fmtAny(want.Ok)))
		return
	}
	if got := toAny(r.t, value); !reflect.DeepEqual(got, want.Ok) {
		r.t.Errorf("%s: value\n got: %v\nwant: %v", label, shorten(fmtAny(got)), shorten(fmtAny(want.Ok)))
	}
}

func fmtAny(v any) any {
	raw, err := json.Marshal(v) //nolint:errchkjson,forbidigo // a diagnostic text, not wire data
	if err != nil {
		return v
	}
	return string(raw)
}

// run replays every step of the scenario and compares calls and outcomes.
func (r *replayer) run() {
	r.t.Helper()
	for i, s := range r.sc.Steps {
		label := fmt.Sprintf("%s step %d (%s)", r.sc.Name, i, s.Action)
		r.runner.script(s.Replies)
		value, err := r.do(context.Background(), s)
		r.compareCalls(label, r.runner.recorded(), s.Calls)
		r.compareOutcome(label, value, err, s.Outcome)
		if s.Execution != nil {
			r.compareExecution(label, s.Execution)
		}
		if left := r.runner.remaining(); left != 0 {
			r.t.Errorf("%s: %d scripted replies were not used (the Go provider made fewer calls)", label, left)
		}
	}
}

func TestTranscriptsMatchGolden(t *testing.T) {
	for _, sc := range loadScenarios(t, "docker-transcripts.json") {
		t.Run(sc.Name, func(t *testing.T) {
			newReplayer(t, sc).run()
		})
	}
}
