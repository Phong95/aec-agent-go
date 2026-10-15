package trace

import (
	"bytes"
	"context"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"testing"

	"github.com/Phong95/aec-agent-go/internal/wiretest"
	"github.com/Phong95/aec-agent-go/kernel"
	"github.com/Phong95/aec-agent-go/kernel/ordered"
)

// The five recorded span fixtures, keyed by the scenario step that produces each of
// them.
var spanFixtures = []string{
	"trace-span-start.json",
	"trace-span-end.json",
	"trace-span-child-with-events.json",
	"trace-span-child-error.json",
	"trace-span-zero-duration.json",
}

// encSink encodes every span at callback time (like the wiregen recording sink)
// and remembers the bytes in notification order.
type encSink struct {
	t     *testing.T
	calls []encCall
}

type encCall struct {
	phase string // "start" or "end"
	name  string
	json  []byte
}

func (s *encSink) record(phase string, sp Span) {
	b, err := kernel.EncodeJSON(sp)
	if err != nil {
		s.t.Errorf("encode span %q: %v", sp.Name, err)
		return
	}
	s.calls = append(s.calls, encCall{phase: phase, name: sp.Name, json: b})
}

func (s *encSink) OnSpanStart(sp Span)         { s.record("start", sp) }
func (s *encSink) OnSpanEnd(sp Span)           { s.record("end", sp) }
func (s *encSink) Flush(context.Context) error { return nil }

func (s *encSink) find(phase, name string) []byte {
	for _, c := range s.calls {
		if c.phase == phase && c.name == name {
			return c.json
		}
	}
	s.t.Fatalf("no %s notification for span %q", phase, name)
	return nil
}

// runScenario replays on svc exactly the call sequence of the wiregen trace
// scenario, which sequences Date.now and crypto.randomUUID calls the same way.
// setStepZero is called before the second trace to switch the clock to step 0.
func runScenario(svc *Service, setStepZero func()) {
	t1 := svc.CreateContext("t1")
	run := svc.StartSpan(t1, "agent", KindRun, attrs("agentId", "a1"))
	iter := svc.StartSpan(t1, "iter-1", KindIteration, ordered.Object{})
	iter.AddEvent("tick", ordered.Object{})
	iter.AddEvent("tock", attrs("n", 1))
	iter.End(ordered.Object{})
	compact := svc.StartSpan(t1, "compact", KindCompaction, attrs("a", "v", "b", []any{1, 2}))
	compact.End(attrs("error", "x"))
	run.End(ordered.Object{})

	setStepZero()
	t2 := svc.CreateContext("t2")
	mc := svc.StartSpan(t2, "model", KindModelCall, ordered.Object{})
	mc.End(ordered.Object{})
}

// newScenarioService builds a Service whose clock and IDs equal the patched
// Date.now (start 1700000000000, step 1) and crypto.randomUUID of wiregen. The
// returned function switches the clock to step 0 from the next value on, as the
// recorded scenario does by setting the patched step to 0.
func newScenarioService() (*Service, func()) {
	const start = 1700000000000
	cur := wiretest.Clock(start, 1)
	calls := int64(0)
	clock := func() int64 { calls++; return cur() }
	svc := NewService(WithClock(clock), WithIDFunc(wiretest.IDs()))
	stepZero := func() { cur = wiretest.Clock(start+calls, 0) }
	return svc, stepZero
}

func TestSpanFixturesRoundTrip(t *testing.T) {
	for _, name := range spanFixtures {
		t.Run(name, func(t *testing.T) {
			var sp Span
			wiretest.AssertRoundTrip(t, name, &sp)
		})
	}
}

func TestServiceReproducesGoldenSpans(t *testing.T) {
	svc, stepZero := newScenarioService()
	rec := &encSink{t: t}
	svc.AddSink(rec)
	runScenario(svc, stepZero)

	wiretest.AssertBytes(t, "trace-span-start.json", rec.find("start", "agent"))
	wiretest.AssertBytes(t, "trace-span-end.json", rec.find("end", "agent"))
	wiretest.AssertBytes(t, "trace-span-child-with-events.json", rec.find("end", "iter-1"))
	wiretest.AssertBytes(t, "trace-span-child-error.json", rec.find("end", "compact"))
	wiretest.AssertBytes(t, "trace-span-zero-duration.json", rec.find("end", "model"))
}

func TestConsoleSinkMatchesGolden(t *testing.T) {
	svc, stepZero := newScenarioService()
	var buf bytes.Buffer
	svc.AddSink(NewConsoleSink(&buf))
	runScenario(svc, stepZero)

	want := wiretest.Load(t, "console-sink.txt")
	want = append(want, '\n') // Load strips the single trailing LF; every console line ends in LF
	if !bytes.Equal(buf.Bytes(), want) {
		t.Fatalf("console output differs from console-sink.txt\n got:\n%s\nwant:\n%s", buf.Bytes(), want)
	}
}

func TestSpansReachCustomAndConsoleSink(t *testing.T) {
	svc, stepZero := newScenarioService()
	custom := &recSink{}
	var buf bytes.Buffer
	svc.AddSink(custom)
	svc.AddSink(NewConsoleSink(&buf))
	runScenario(svc, stepZero)

	// 4 spans: run, iteration, compaction, model-call.
	if len(custom.starts) != 4 || len(custom.ends) != 4 {
		t.Fatalf("custom sink got %d starts and %d ends, want 4 and 4", len(custom.starts), len(custom.ends))
	}
	lines := bytes.Count(buf.Bytes(), []byte{'\n'})
	if lines != len(custom.starts)+len(custom.ends) {
		t.Fatalf("console wrote %d lines, want one per notification (%d)", lines, len(custom.starts)+len(custom.ends))
	}
	// Both sinks received the same spans in the same order.
	var names []string
	for _, sp := range custom.starts {
		names = append(names, sp.Name)
	}
	if !reflect.DeepEqual(names, []string{"agent", "iter-1", "compact", "model"}) {
		t.Fatalf("custom sink start order = %v", names)
	}
	for _, name := range names {
		if !bytes.Contains(buf.Bytes(), []byte(" "+name+" ")) && !bytes.Contains(buf.Bytes(), []byte(" "+name+"\n")) {
			t.Fatalf("console output has no line for span %q:\n%s", name, buf.Bytes())
		}
	}
}

func TestSpanKindsMatchGolden(t *testing.T) {
	data, err := os.ReadFile(filepath.Join(wiretest.Dir(t), "span-kinds.json"))
	if err != nil {
		t.Fatal(err)
	}
	var fixture []string
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	got := []string{
		string(KindRun), string(KindIteration), string(KindContextAssembly), string(KindCompaction),
		string(KindMemoryRecall), string(KindModelCall), string(KindToolDispatch), string(KindSkillDispatch),
		string(KindSandboxOp),
	}
	sort.Strings(got)
	want := append([]string(nil), fixture...)
	sort.Strings(want)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Go SpanKind set = %v, TS set = %v", got, want)
	}
	if len(fixture) != 9 {
		t.Fatalf("span-kinds.json has %d entries, want 9", len(fixture))
	}
}

func TestConsoleSinkFormat(t *testing.T) {
	var buf bytes.Buffer
	cs := NewConsoleSink(&buf)
	end := int64(0)
	cs.OnSpanStart(Span{
		TraceID: "t", SpanID: "abcdefghijkl", ParentSpanID: "12345678-9", Name: "n", Kind: KindToolDispatch,
		Attributes: attrs("z", 1, "a", nil, "m", "<x>"),
	})
	cs.OnSpanStart(Span{SpanID: "abc", Name: "short", Kind: KindSandboxOp})
	cs.OnSpanEnd(Span{Name: "n", Kind: KindSkillDispatch, Status: StatusError, DurationMs: &end})
	cs.OnSpanEnd(Span{Name: "open", Kind: KindContextAssembly, Status: StatusOK})
	want := "[trace] \u25b6 TOOL n span=abcdefgh parent=12345678 {z=1, a=null, m=\"<x>\"}\n" +
		"[trace] \u25b6 sandbox-op short span=abc\n" +
		"[trace] \u25c0 SKILL n 0ms ERROR\n" +
		"[trace] \u25c0 CTX open\n"
	if buf.String() != want {
		t.Fatalf("console output\n got: %q\nwant: %q", buf.String(), want)
	}
	if err := cs.Flush(t.Context()); err != nil {
		t.Fatalf("Flush = %v", err)
	}
}

func TestConsoleSinkDefaultsToStdout(t *testing.T) {
	if cs := NewConsoleSink(nil); cs.w != os.Stdout {
		t.Fatal("NewConsoleSink(nil) must write to os.Stdout")
	}
}

// WR-02: JSON.stringify writes NaN and Infinity as null and -0 as 0, so a span
// with such an attribute must still encode, and the console shows the same.
func TestNonFiniteAttributesEncodeLikeJS(t *testing.T) {
	nonFinite := attrs("nan", math.NaN(), "inf", math.Inf(1), "nz", math.Copysign(0, -1), "ok", 1.5)
	sp := Span{
		TraceID: "t", SpanID: "s", Name: "n", Kind: KindRun, Status: StatusOK, Attributes: nonFinite,
		Events: []SpanEvent{{Name: "e", Timestamp: 1, Attributes: attrs("x", math.NaN())}},
	}
	b, err := kernel.EncodeJSON(sp)
	if err != nil {
		t.Fatalf("EncodeJSON must not fail on NaN/Inf attributes: %v", err)
	}
	want := `{"traceId":"t","spanId":"s","name":"n","kind":"run","startTime":0,"status":"ok","attributes":{"nan":null,"inf":null,"nz":0,"ok":1.5},"events":[{"name":"e","timestamp":1,"attributes":{"x":null}}]}`
	if string(b) != want {
		t.Fatalf("got  %s\nwant %s", b, want)
	}

	var buf bytes.Buffer
	NewConsoleSink(&buf).OnSpanStart(Span{SpanID: "s", Name: "n", Kind: KindRun, Attributes: nonFinite})
	wantLine := "[trace] \u25b6 RUN n span=s {nan=null, inf=null, nz=0, ok=1.5}\n"
	if buf.String() != wantLine {
		t.Fatalf("console\n got: %q\nwant: %q", buf.String(), wantLine)
	}
}
