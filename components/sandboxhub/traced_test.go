package sandboxhub_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/Phong95/aec-agent-go/components/sandboxhub"
	"github.com/Phong95/aec-agent-go/internal/jsonx"
	"github.com/Phong95/aec-agent-go/internal/wiretest"
	"github.com/Phong95/aec-agent-go/kernel"
)

// tracedFixture mirrors sandbox-traced.json: the recorded tracing of a real
// in-memory view (runs "scoped" and "unscoped") and a stub view whose readFile
// throws a plain Error (run "stub").
type tracedFixture struct {
	Runs []tracedRun `json:"runs"`
}

type tracedRun struct {
	Name  string `json:"name"`
	Scope struct {
		AgentID   string `json:"agentId"`
		SessionID string `json:"sessionId"`
	} `json:"scope"`
	View struct {
		ID           string                  `json:"id"`
		Traits       sandboxhub.Traits       `json:"traits"`
		Capabilities []sandboxhub.Capability `json:"capabilities"`
		Limits       sandboxhub.Limits       `json:"limits"`
		WorkingDir   string                  `json:"workingDir"`
		Roots        []string                `json:"roots"`
	} `json:"view"`
	Calls []tracedCall `json:"calls"`
}

type tracedCall struct {
	Op        string          `json:"op"`
	Input     tracedInput     `json:"input"`
	Result    json.RawMessage `json:"result"`
	Error     *wireErr        `json:"error"`
	Operation string          `json:"operation"`
	Start     map[string]any  `json:"start"`
	End       map[string]any  `json:"end"`
}

type tracedInput struct {
	Path    string                  `json:"path"`
	Content string                  `json:"content"`
	Opts    json.RawMessage         `json:"opts"`
	Query   *sandboxhub.SearchQuery `json:"query"`
	Request *sandboxhub.ExecRequest `json:"request"`
}

func loadTracedFixture(t *testing.T) tracedFixture {
	t.Helper()
	var f tracedFixture
	if err := jsonx.UnmarshalStrict(wiretest.Load(t, "sandbox-traced.json"), &f); err != nil {
		t.Fatalf("decode sandbox-traced.json: %v", err)
	}
	return f
}

// scriptedView is the inner View of the golden test: it replays the recorded
// results and errors of one run, call by call, and checks the operation order.
type scriptedView struct {
	t    *testing.T
	run  *tracedRun
	next int
}

func (v *scriptedView) ID() string                { return v.run.View.ID }
func (v *scriptedView) Traits() sandboxhub.Traits { return v.run.View.Traits }
func (v *scriptedView) Capabilities() []sandboxhub.Capability {
	return slices.Clone(v.run.View.Capabilities)
}
func (v *scriptedView) Limits() sandboxhub.Limits { return v.run.View.Limits }
func (v *scriptedView) WorkingDir() string        { return v.run.View.WorkingDir }
func (v *scriptedView) Roots() []string           { return slices.Clone(v.run.View.Roots) }

// take consumes the next recorded call, which must be op, and returns its error
// (nil when it recorded a result).
func (v *scriptedView) take(op string, out any) error {
	v.t.Helper()
	if v.next >= len(v.run.Calls) {
		v.t.Fatalf("run %s: unexpected %s call", v.run.Name, op)
	}
	call := v.run.Calls[v.next]
	v.next++
	if call.Op != op {
		v.t.Fatalf("run %s call %d: got %s, the fixture has %s", v.run.Name, v.next, op, call.Op)
	}
	if call.Error != nil {
		if call.Error.Code == "" {
			return errors.New(call.Error.Message)
		}
		id := ""
		if call.Error.SandboxID != nil {
			id = *call.Error.SandboxID
		}
		return kernel.NewSandboxError(call.Error.Message, kernel.SandboxErrorCode(call.Error.Code), id)
	}
	if err := jsonx.UnmarshalStrict(call.Result, out); err != nil {
		v.t.Fatalf("run %s call %d: decode the %s result: %v", v.run.Name, v.next, op, err)
	}
	return nil
}

func (v *scriptedView) ReadFile(context.Context, string, sandboxhub.ReadOptions) (sandboxhub.FileContent, error) {
	var out sandboxhub.FileContent
	err := v.take("readFile", &out)
	return out, err
}

func (v *scriptedView) WriteFile(context.Context, string, string, sandboxhub.WriteOptions) (sandboxhub.WriteResult, error) {
	var out sandboxhub.WriteResult
	err := v.take("writeFile", &out)
	return out, err
}

func (v *scriptedView) Stat(context.Context, string) (*sandboxhub.FileStat, error) {
	var out *sandboxhub.FileStat
	err := v.take("stat", &out)
	return out, err
}

func (v *scriptedView) List(context.Context, string, sandboxhub.ListOptions) (sandboxhub.ListResult, error) {
	var out sandboxhub.ListResult
	err := v.take("list", &out)
	return out, err
}

func (v *scriptedView) Search(context.Context, sandboxhub.SearchQuery) (sandboxhub.SearchResult, error) {
	var out sandboxhub.SearchResult
	err := v.take("search", &out)
	return out, err
}

func (v *scriptedView) Exec(context.Context, sandboxhub.ExecRequest, sandboxhub.ExecOptions) (sandboxhub.ExecResult, error) {
	var out sandboxhub.ExecResult
	err := v.take("exec", &out)
	return out, err
}

// recordedSpan is one span the test starter recorded.
type recordedSpan struct {
	operation string
	start     map[string]any
	end       map[string]any
	ended     int
}

func (s *recordedSpan) End(attrs map[string]any) {
	s.ended++
	s.end = attrs
}

type spanRecorder struct{ spans []*recordedSpan }

func (r *spanRecorder) starter() sandboxhub.SpanStarter {
	return func(operation string, attrs map[string]any) sandboxhub.SpanEnder {
		s := &recordedSpan{operation: operation, start: attrs}
		r.spans = append(r.spans, s)
		return s
	}
}

// wireForm encodes attrs the way the wire does and decodes them into plain JSON
// values, so numbers compare as float64 and key order does not matter (Go span
// attributes encode with sorted keys).
func wireForm(t *testing.T, attrs map[string]any) map[string]any {
	t.Helper()
	b, err := jsonx.Marshal(attrs)
	if err != nil {
		t.Fatalf("encode attributes: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("decode attributes %s: %v", b, err)
	}
	return out
}

func decodeOpts(t *testing.T, raw json.RawMessage, out any) {
	t.Helper()
	if len(raw) == 0 {
		return
	}
	if err := jsonx.UnmarshalStrict(raw, out); err != nil {
		t.Fatalf("decode opts %s: %v", raw, err)
	}
}

func TestTracedViewMatchesGolden(t *testing.T) {
	f := loadTracedFixture(t)
	if len(f.Runs) != 3 {
		t.Fatalf("the fixture has %d runs, want 3", len(f.Runs))
	}
	ctx := context.Background()
	for i := range f.Runs {
		run := &f.Runs[i]
		t.Run(run.Name, func(t *testing.T) {
			inner := &scriptedView{t: t, run: run}
			rec := &spanRecorder{}
			tv := sandboxhub.WithTracing(inner, rec.starter(), sandboxhub.TraceScope{AgentID: run.Scope.AgentID, SessionID: run.Scope.SessionID})

			for n, call := range run.Calls {
				name := fmt.Sprintf("%s call %d (%s)", run.Name, n, call.Op)
				before := len(rec.spans)
				in := call.Input
				var result any
				var err error
				switch call.Op {
				case "readFile":
					var o sandboxhub.ReadOptions
					decodeOpts(t, in.Opts, &o)
					result, err = tv.ReadFile(ctx, in.Path, o)
				case "writeFile":
					var o sandboxhub.WriteOptions
					decodeOpts(t, in.Opts, &o)
					result, err = tv.WriteFile(ctx, in.Path, in.Content, o)
				case "stat":
					result, err = tv.Stat(ctx, in.Path)
				case "list":
					var o sandboxhub.ListOptions
					decodeOpts(t, in.Opts, &o)
					result, err = tv.List(ctx, in.Path, o)
				case "search":
					result, err = tv.Search(ctx, *in.Query)
				case "exec":
					result, err = tv.Exec(ctx, *in.Request, sandboxhub.ExecOptions{})
				default:
					t.Fatalf("%s: unknown op", name)
				}

				if got := len(rec.spans) - before; got != 1 {
					t.Fatalf("%s: %d spans, want exactly 1", name, got)
				}
				span := rec.spans[before]
				if span.operation != call.Operation {
					t.Errorf("%s: operation %q, want %q", name, span.operation, call.Operation)
				}
				if span.ended != 1 {
					t.Errorf("%s: span ended %d times, want once", name, span.ended)
				}
				wantStart, wantEnd := q5GoSizes(t, call.Op, in.Content, call.Start, call.End, call.Result)
				if !reflect.DeepEqual(wireForm(t, span.start), wantStart) {
					t.Errorf("%s: start attributes\n got %v\nwant %v", name, wireForm(t, span.start), wantStart)
				}
				if !reflect.DeepEqual(wireForm(t, span.end), wantEnd) {
					t.Errorf("%s: end attributes\n got %v\nwant %v", name, wireForm(t, span.end), wantEnd)
				}

				if call.Error != nil {
					assertErrorMatches(t, name, err, call.Error)
					continue
				}
				if err != nil {
					t.Fatalf("%s: unexpected error %v", name, err)
				}
				// The result passes through unchanged.
				got, mErr := jsonx.Marshal(result)
				if mErr != nil || string(got) != string(call.Result) {
					t.Errorf("%s: result changed\n got %s\nwant %s", name, got, call.Result)
				}
			}
			if inner.next != len(run.Calls) {
				t.Errorf("only %d of %d recorded calls reached the inner view", inner.next, len(run.Calls))
			}
		})
	}
}

// q5GoSizes is the expected form of the recorded attributes: the fixture records
// the fs.write start "bytes" and the exec end "stdoutBytes" and "stderrBytes" as
// UTF-16 lengths, WithTracing as byte lengths. The values are recomputed from the
// recorded content and result; every other attribute is the recorded one. The maps
// are copies.
func q5GoSizes(t *testing.T, op, content string, start, end map[string]any, result json.RawMessage) (map[string]any, map[string]any) {
	t.Helper()
	start, end = maps.Clone(start), maps.Clone(end)
	switch op {
	case "writeFile":
		if _, ok := start["bytes"]; ok {
			start["bytes"] = float64(len(content))
		}
	case "exec":
		if _, ok := end["stdoutBytes"]; !ok || len(result) == 0 || string(result) == "null" {
			break
		}
		var r struct {
			Stdout string `json:"stdout"`
			Stderr string `json:"stderr"`
		}
		if err := json.Unmarshal(result, &r); err != nil {
			t.Fatalf("decode exec result: %v", err)
		}
		end["stdoutBytes"] = float64(len(r.Stdout))
		end["stderrBytes"] = float64(len(r.Stderr))
	}
	return start, end
}

// TestTracedViewSizeAttributesCountBytes pins the byte-count size attributes on
// their own: a write of "a" + a 4-byte emoji + "b" is 6 bytes (the fixture records 4
// UTF-16 units), and exec output "caf" + e-acute + newline is 6 bytes (the fixture
// records 5 units).
func TestTracedViewSizeAttributesCountBytes(t *testing.T) {
	f := loadTracedFixture(t)
	var checkedWrite, checkedExec bool
	for _, call := range f.Runs[0].Calls {
		switch {
		case call.Op == "writeFile" && call.Input.Content == "a\U0001F600b":
			checkedWrite = true
			if call.Start["bytes"] != float64(4) {
				t.Errorf("TS write start bytes = %v, want 4 UTF-16 units", call.Start["bytes"])
			}
		case call.Op == "exec" && call.Input.Request != nil && len(call.Input.Request.Command) == 2 && call.Input.Request.Command[1] == "caf\U000000e9":
			checkedExec = true
			if call.End["stdoutBytes"] != float64(5) { // "caf" + e-acute + newline is 6 bytes, 5 units
				t.Errorf("TS stdoutBytes = %v, want 5 UTF-16 units", call.End["stdoutBytes"])
			}
		}
	}
	if !checkedWrite || !checkedExec {
		t.Fatalf("the emoji write (%v) or the accented exec (%v) is missing from the fixture", checkedWrite, checkedExec)
	}

	ctx := context.Background()
	rec := &spanRecorder{}
	tv := sandboxhub.WithTracing(newInertView(t, nil), rec.starter(), sandboxhub.TraceScope{})
	if _, err := tv.WriteFile(ctx, "x", "a\U0001F600b", sandboxhub.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := tv.Exec(ctx, sandboxhub.ExecRequest{Command: []string{"echo"}, TimeoutMs: 1000, MaxOutputBytes: 1000}, sandboxhub.ExecOptions{}); err != nil {
		t.Fatal(err)
	}
	if got := rec.spans[0].start["bytes"]; got != 6 {
		t.Errorf("Go write start bytes = %v, want 6 (bytes)", got)
	}
	// the inert view prints "topsecret-output" and "topsecret-error"
	if got := rec.spans[1].end["stdoutBytes"]; got != len("topsecret-output") {
		t.Errorf("Go stdoutBytes = %v, want %d", got, len("topsecret-output"))
	}
	if got := rec.spans[1].end["stderrBytes"]; got != len("topsecret-error") {
		t.Errorf("Go stderrBytes = %v, want %d", got, len("topsecret-error"))
	}
}

func TestTracedViewDelegatesShape(t *testing.T) {
	f := loadTracedFixture(t)
	run := &f.Runs[0]
	tv := sandboxhub.WithTracing(&scriptedView{t: t, run: run}, (&spanRecorder{}).starter(), sandboxhub.TraceScope{})

	if tv.ID() != run.View.ID || tv.Traits() != run.View.Traits || tv.Limits() != run.View.Limits || tv.WorkingDir() != run.View.WorkingDir {
		t.Errorf("scalar accessors differ: %q %+v %+v %q", tv.ID(), tv.Traits(), tv.Limits(), tv.WorkingDir())
	}
	if !slices.Equal(tv.Capabilities(), run.View.Capabilities) || !slices.Equal(tv.Roots(), run.View.Roots) {
		t.Errorf("capabilities %v roots %v differ", tv.Capabilities(), tv.Roots())
	}
	// Fresh copies: changing a returned slice does not reach the next call.
	caps := tv.Capabilities()
	caps[0] = "mutated"
	roots := tv.Roots()
	roots[0] = "mutated"
	if !slices.Equal(tv.Capabilities(), run.View.Capabilities) || !slices.Equal(tv.Roots(), run.View.Roots) {
		t.Error("Capabilities or Roots share their slice with an earlier result")
	}
}

// inertView is a View whose operations return what the test sets.
type inertView struct {
	scriptedView
	err error
}

func (v *inertView) ReadFile(context.Context, string, sandboxhub.ReadOptions) (sandboxhub.FileContent, error) {
	return sandboxhub.FileContent{}, v.err
}

func (v *inertView) WriteFile(context.Context, string, string, sandboxhub.WriteOptions) (sandboxhub.WriteResult, error) {
	return sandboxhub.WriteResult{Bytes: 1, Created: true}, v.err
}

func (v *inertView) Exec(_ context.Context, _ sandboxhub.ExecRequest, _ sandboxhub.ExecOptions) (sandboxhub.ExecResult, error) {
	code := 0
	return sandboxhub.ExecResult{ExitCode: &code, Stdout: "topsecret-output", Stderr: "topsecret-error"}, v.err
}

func newInertView(t *testing.T, err error) *inertView {
	t.Helper()
	f := loadTracedFixture(t)
	return &inertView{scriptedView: scriptedView{t: t, run: &f.Runs[0]}, err: err}
}

func TestTracedViewReturnsTheInnerErrorUnchanged(t *testing.T) {
	ctx := context.Background()

	sbErr := kernel.NewSandboxError("not here", kernel.SandboxCodeNotFound, "docker:shared")
	rec := &spanRecorder{}
	tv := sandboxhub.WithTracing(newInertView(t, sbErr), rec.starter(), sandboxhub.TraceScope{})
	_, err := tv.ReadFile(ctx, "x", sandboxhub.ReadOptions{})
	if err != error(sbErr) { //nolint:errorlint // identity, not errors.Is: the error must be returned unchanged
		t.Errorf("the SandboxError was replaced: %v", err)
	}
	if got := rec.spans[0].end; got["error"] != "not_found" || got["message"] != "not here" || len(got) != 2 {
		t.Errorf("end attributes of a SandboxError = %v", got)
	}

	plain := errors.New("disk on fire")
	rec = &spanRecorder{}
	tv = sandboxhub.WithTracing(newInertView(t, plain), rec.starter(), sandboxhub.TraceScope{})
	_, err = tv.WriteFile(ctx, "x", "y", sandboxhub.WriteOptions{})
	if err != plain { //nolint:errorlint // identity, not errors.Is: the error must be returned unchanged
		t.Errorf("the plain error was replaced: %v", err)
	}
	if got := rec.spans[0].end; got["error"] != "error" || got["message"] != "disk on fire" {
		t.Errorf("end attributes of a plain error = %v", got)
	}

	// A wrapped SandboxError still gives its code.
	wrapped := errors.Join(errors.New("context"), sbErr)
	rec = &spanRecorder{}
	tv = sandboxhub.WithTracing(newInertView(t, wrapped), rec.starter(), sandboxhub.TraceScope{})
	if _, err := tv.ReadFile(ctx, "x", sandboxhub.ReadOptions{}); err != wrapped { //nolint:errorlint // identity, not errors.Is: the error must be returned unchanged
		t.Errorf("the wrapped error was replaced: %v", err)
	}
	if got := rec.spans[0].end["error"]; got != "not_found" {
		t.Errorf("code of a wrapped SandboxError = %v, want not_found", got)
	}
}

// TestTracedViewNeverRecordsContentArgumentsOrOutput: spans carry paths, a
// program name and counts, never what may hold secrets.
func TestTracedViewNeverRecordsContentArgumentsOrOutput(t *testing.T) {
	ctx := context.Background()
	rec := &spanRecorder{}
	tv := sandboxhub.WithTracing(newInertView(t, nil), rec.starter(), sandboxhub.TraceScope{AgentID: "a1", SessionID: "s1"})

	if _, err := tv.WriteFile(ctx, "notes/x.txt", "topsecret-content", sandboxhub.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	shell := true
	for _, req := range []sandboxhub.ExecRequest{
		{Command: []string{"curl", "--header", "Authorization: topsecret-arg"}, TimeoutMs: 1000, MaxOutputBytes: 1000, Env: map[string]string{"TOKEN": "topsecret-env"}, Stdin: "topsecret-stdin"},
		{Command: []string{"echo", "topsecret-arg"}, Shell: &shell, TimeoutMs: 1000, MaxOutputBytes: 1000},
	} {
		if _, err := tv.Exec(ctx, req, sandboxhub.ExecOptions{}); err != nil {
			t.Fatal(err)
		}
	}

	if len(rec.spans) != 3 {
		t.Fatalf("%d spans, want 3", len(rec.spans))
	}
	for _, s := range rec.spans {
		all := string(mustMarshal(t, s.start)) + string(mustMarshal(t, s.end))
		if strings.Contains(all, "topsecret") {
			t.Errorf("span %s leaked content, arguments, environment or output: %s", s.operation, all)
		}
	}
	if got := rec.spans[1].start["command"]; got != "curl" {
		t.Errorf("exec command attribute = %v, want the program name", got)
	}
	if got := rec.spans[2].start["command"]; got != "shell" {
		t.Errorf("shell exec command attribute = %v, want shell", got)
	}
}

func mustMarshal(t *testing.T, v any) []byte {
	t.Helper()
	b, err := jsonx.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TestTracedViewToleratesAbsentStarterAndSpan: a nil starter returns the inner
// view and a starter that returns no span just records nothing.
func TestTracedViewToleratesAbsentStarterAndSpan(t *testing.T) {
	ctx := context.Background()
	inner := newInertView(t, nil)
	if got := sandboxhub.WithTracing(inner, nil, sandboxhub.TraceScope{}); got != sandboxhub.View(inner) {
		t.Error("a nil starter must return the inner view unchanged")
	}
	tv := sandboxhub.WithTracing(inner, func(string, map[string]any) sandboxhub.SpanEnder { return nil }, sandboxhub.TraceScope{})
	if _, err := tv.WriteFile(ctx, "x", "y", sandboxhub.WriteOptions{}); err != nil {
		t.Errorf("a nil span broke the call: %v", err)
	}
}

// TestTracedViewMakesFreshAttributeMaps: a starter may keep or change its map
// without affecting later spans.
func TestTracedViewMakesFreshAttributeMaps(t *testing.T) {
	ctx := context.Background()
	var kept []map[string]any
	tv := sandboxhub.WithTracing(newInertView(t, nil), func(_ string, attrs map[string]any) sandboxhub.SpanEnder {
		kept = append(kept, attrs)
		if len(kept) == 1 {
			attrs["sandboxId"] = "tampered"
		}
		return &recordedSpan{}
	}, sandboxhub.TraceScope{})
	for range 2 {
		if _, err := tv.WriteFile(ctx, "x", "y", sandboxhub.WriteOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	if len(kept) != 2 || kept[0]["path"] != "x" {
		t.Fatalf("kept = %v", kept)
	}
	kept[0]["path"] = "other"
	if kept[1]["path"] != "x" {
		t.Error("two spans share one attribute map")
	}
	if kept[1]["sandboxId"] == "tampered" {
		t.Error("the base attributes are shared between spans")
	}
}
