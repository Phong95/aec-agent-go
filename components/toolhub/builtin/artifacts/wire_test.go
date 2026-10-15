package artifacts

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"math"
	"reflect"
	"testing"

	"github.com/Phong95/aec-agent-go/aecmock"
	"github.com/Phong95/aec-agent-go/components/artifacthub/artifact"
	"github.com/Phong95/aec-agent-go/components/sandboxhub"
	"github.com/Phong95/aec-agent-go/components/toolhub"
	"github.com/Phong95/aec-agent-go/internal/wiretest"
	"github.com/Phong95/aec-agent-go/kernel"
)

// builtin-artifact-tools.json is recorded by wiregen from reference artifact tools over a
// scripted ArtifactView (see tools/wiregen/builtin/artifacts.cases.mjs). Each case carries
// its inputs, the result or error the view scripts, the expected output, the
// calls the tool made on the view and the events it emitted. This replay scripts the same
// view in Go, runs the Go tool and compares everything.
//
// The inputs handed to the view are compared as JSON values, not as bytes: artifact.SaveInput
// and artifact.UpdateInput are Phase 5 contracts whose field order is their own (UpdateInput
// writes description and tags before note, the recorded input has note first), and the order of
// the keys of a call between two Go values is not observable.

type wireError struct {
	Kind       string `json:"kind"`
	Code       string `json:"code"`
	Message    string `json:"message"`
	ArtifactID string `json:"artifactId"`
}

type wireSandbox struct {
	Mode  string     `json:"mode"`
	Error *wireError `json:"error"`
}

type wireView struct {
	SessionID *string         `json:"sessionId"`
	Result    json.RawMessage `json:"result"`
	Error     *wireError      `json:"error"`
}

type wireEvent struct {
	Type      string          `json:"type"`
	SessionID string          `json:"sessionId"`
	Payload   json.RawMessage `json:"payload"`
}

type wireCase struct {
	Name    string          `json:"name"`
	Tool    string          `json:"tool"`
	Options json.RawMessage `json:"options"`
	Store   string          `json:"store"`
	Sandbox wireSandbox     `json:"sandbox"`
	Session string          `json:"session"`
	View    wireView        `json:"view"`
	Args    json.RawMessage `json:"args"`
	Output  json.RawMessage `json:"output"`
	Calls   json.RawMessage `json:"calls"`
	Events  []wireEvent     `json:"events"`
}

type wireDefinition struct {
	Tool       string          `json:"tool"`
	Options    json.RawMessage `json:"options"`
	Name       string          `json:"name"`
	Version    string          `json:"version"`
	Definition json.RawMessage `json:"definition"`
}

type wireFixture struct {
	Definitions []wireDefinition `json:"definitions"`
	Cases       []wireCase       `json:"cases"`
}

func loadWireFixture(t *testing.T) wireFixture {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(wiretest.Load(t, "builtin-artifact-tools.json")))
	dec.DisallowUnknownFields()
	var fx wireFixture
	if err := dec.Decode(&fx); err != nil {
		t.Fatalf("decode builtin-artifact-tools: %v", err)
	}
	return fx
}

// optionsFrom rebuilds the Options of a recorded options object. A number is used only
// when it is finite and greater than zero, floored; every other value (a string, null
// for NaN, zero, a negative number) means the default. The Go fields are ints with the
// same positive-or-default rule, so a fractional recorded value is floored here before
// it is handed to the tool.
func optionsFrom(t *testing.T, raw json.RawMessage) Options {
	t.Helper()
	if len(raw) == 0 {
		return Options{}
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("options %s: %v", raw, err)
	}
	num := func(key string) int {
		f, ok := m[key].(float64)
		if !ok || math.IsNaN(f) || math.IsInf(f, 0) || f <= 0 {
			return 0
		}
		return int(math.Floor(f))
	}
	return Options{MaxReadBytes: num("maxReadBytes"), MaxListEntries: num("maxListEntries")}
}

// handlerFor builds the tool a recorded case ran. A tool name the Go package does not
// know fails the test, so a tool added to the recorder cannot be forgotten here.
func handlerFor(t *testing.T, name string, o Options) toolhub.Handler {
	t.Helper()
	for _, h := range NewTools(o) {
		if h.Definition().Name == name {
			return h
		}
	}
	t.Fatalf("the fixture holds an entry for the tool %q, which NewTools does not build", name)
	return nil
}

// errorFor rebuilds the error a recorded case scripts: a real ArtifactError or
// SandboxError with the recorded code and message (the retryable flag follows from the
// code), a plain error, or a plain string, which is an error here too.
func errorFor(e wireError) error {
	switch e.Kind {
	case "artifact":
		return kernel.NewArtifactError(e.Message, kernel.ArtifactErrorCode(e.Code), e.ArtifactID)
	case "sandbox":
		return kernel.NewSandboxError(e.Message, kernel.SandboxErrorCode(e.Code), "docker:shared")
	default:
		return errors.New(e.Message)
	}
}

// valueOf encodes a Go value with the SDK encoder and decodes it back into a generic
// JSON value, so two values can be compared without the order of their keys.
func valueOf(t *testing.T, v any) any {
	t.Helper()
	raw, err := kernel.EncodeJSON(v)
	if err != nil {
		t.Fatalf("encode %T: %v", v, err)
	}
	var out any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode %s: %v", raw, err)
	}
	return out
}

// stubView is the scripted artifact.View of a recorded case. It returns the scripted
// result (decoded into the type of the operation) or the scripted error, and records
// every call as the generic value of the recorded call: the operation, the input, the
// options of a read and the id of the sandbox it was given.
type stubView struct {
	t         *testing.T
	sessionID string
	result    json.RawMessage
	err       error
	calls     []map[string]any
}

var _ artifact.View = (*stubView)(nil)

func (v *stubView) AgentID() string   { return "a1" }
func (v *stubView) SessionID() string { return v.sessionID }

func (v *stubView) record(op string, input any, sb sandboxhub.View, opts any) {
	call := map[string]any{"op": op, "input": valueOf(v.t, input)}
	if opts != nil {
		call["opts"] = valueOf(v.t, opts)
	}
	if sb != nil {
		call["sandboxId"] = sb.ID()
	}
	v.calls = append(v.calls, call)
}

func (v *stubView) decode(into any) error {
	if v.err != nil {
		return v.err
	}
	if len(v.result) == 0 {
		return errors.New("the case scripts no result")
	}
	return json.Unmarshal(v.result, into)
}

func (v *stubView) Save(_ context.Context, in artifact.SaveInput, sb sandboxhub.View) (artifact.Write, error) {
	v.record("save", in, sb, nil)
	var w artifact.Write
	err := v.decode(&w)
	return w, err
}

func (v *stubView) Update(_ context.Context, in artifact.UpdateInput, sb sandboxhub.View) (artifact.Write, error) {
	v.record("update", in, sb, nil)
	var w artifact.Write
	err := v.decode(&w)
	return w, err
}

func (v *stubView) Read(_ context.Context, ref artifact.Ref, sb sandboxhub.View, o artifact.ReadOptions) (artifact.Read, error) {
	v.record("read", ref, sb, o)
	var r artifact.Read
	err := v.decode(&r)
	return r, err
}

func (v *stubView) List(_ context.Context, q artifact.Query) ([]artifact.Artifact, error) {
	v.record("list", q, nil, nil)
	var l []artifact.Artifact
	err := v.decode(&l)
	return l, err
}

func (*stubView) Get(context.Context, artifact.Ref) (artifact.Artifact, error) {
	return artifact.Artifact{}, errors.New("get is not used by the tools")
}

func (*stubView) Versions(context.Context, artifact.Ref) ([]artifact.Version, error) {
	return nil, errors.New("versions is not used by the tools")
}

func (*stubView) Delete(context.Context, artifact.Ref) error {
	return errors.New("delete is not used by the tools")
}

// TestArtifactToolsMatchGolden replays builtin-artifact-tools.json: the name, version and
// definition of every tool and option variant, and for every case the output bytes, the
// calls the tool made on the view and the events it emitted.
func TestArtifactToolsMatchGolden(t *testing.T) {
	fx := loadWireFixture(t)
	if len(fx.Cases) < 30 {
		t.Fatalf("the corpus holds %d cases, want at least 30", len(fx.Cases))
	}

	for _, d := range fx.Definitions {
		h := handlerFor(t, d.Tool, optionsFrom(t, d.Options))
		if h.Name() != d.Name || h.Version() != d.Version {
			t.Errorf("%s: tool = %s %s, want the TS %s %s", d.Tool, h.Name(), h.Version(), d.Name, d.Version)
		}
		got, err := kernel.EncodeJSON(h.Definition())
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != string(d.Definition) {
			t.Errorf("%s with options %s: definition differs from TS\n got: %s\nwant: %s", d.Tool, d.Options, got, d.Definition)
		}
	}

	for _, c := range fx.Cases {
		t.Run(c.Name, func(t *testing.T) {
			h := handlerFor(t, c.Tool, optionsFrom(t, c.Options))

			view := &stubView{t: t, result: c.View.Result}
			if c.View.SessionID != nil {
				view.sessionID = *c.View.SessionID
			}
			if c.View.Error != nil {
				view.err = errorFor(*c.View.Error)
			}
			var emitted []wireEvent
			tc := toolhub.Context{
				SessionID: c.Session,
				AgentID:   "a1",
				Emit: func(eventType string, payload any) {
					raw, err := kernel.EncodeJSON(payload)
					if err != nil {
						t.Errorf("encode the payload of %s: %v", eventType, err)
					}
					emitted = append(emitted, wireEvent{Type: eventType, Payload: raw})
				},
			}
			if c.Store == "view" {
				tc.GetArtifacts = func() artifact.View { return view }
			}
			switch c.Sandbox.Mode {
			case "view":
				env, err := aecmock.NewInMemoryView("sb-test", "a1")
				if err != nil {
					t.Fatal(err)
				}
				tc.GetSandbox = func(context.Context) (sandboxhub.View, error) { return env, nil }
			case "throws":
				failure := errorFor(*c.Sandbox.Error)
				tc.GetSandbox = func(context.Context) (sandboxhub.View, error) { return nil, failure }
			}

			args, err := kernel.NewArguments(c.Args)
			if err != nil {
				t.Fatalf("arguments: %v", err)
			}
			out, err := h.Execute(t.Context(), args, tc)
			if err != nil {
				t.Fatalf("Execute returned an error (every failure must be an output): %v", err)
			}
			got, err := kernel.EncodeJSON(out)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != string(c.Output) {
				t.Errorf("output differs from TS\n got: %s\nwant: %s", got, c.Output)
			}

			var wantCalls any
			if err := json.Unmarshal(c.Calls, &wantCalls); err != nil {
				t.Fatalf("calls: %v", err)
			}
			gotCalls := make([]any, 0, len(view.calls))
			for _, call := range view.calls {
				gotCalls = append(gotCalls, call)
			}
			if !reflect.DeepEqual(gotCalls, wantCalls) {
				t.Errorf("view calls differ from TS\n got: %v\nwant: %v", gotCalls, wantCalls)
			}

			if len(emitted) != len(c.Events) {
				t.Fatalf("emitted %d events, TS emitted %d", len(emitted), len(c.Events))
			}
			for i, e := range emitted {
				// The tool context carries the session id; the loop adds it to the event
				// when it forwards the emit, so only the type and the payload are the tool's.
				if e.Type != c.Events[i].Type || string(e.Payload) != string(c.Events[i].Payload) {
					t.Errorf("event %d differs from TS\n got: %s %s\nwant: %s %s", i, e.Type, e.Payload, c.Events[i].Type, c.Events[i].Payload)
				}
			}
		})
	}
}
