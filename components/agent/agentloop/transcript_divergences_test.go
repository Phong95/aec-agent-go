package agentloop_test

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/Phong95/aec-agent-go/components/events"
	"github.com/Phong95/aec-agent-go/kernel"
	"github.com/Phong95/aec-agent-go/kernel/ordered"
)

// applyGoDivergences turns the recorded transcript of fixture name into the
// transcript the loop is expected to produce. Each divergence below is a behaviour
// in which the loop differs from the recording; it rewrites only the parts of the
// transcript that behaviour is about, and every other byte stays the recorded one.
// RISK-UNSET is handled in replay itself (see there).
func applyGoDivergences(t *testing.T, name string, want *tTranscript) {
	t.Helper()
	// Stop rule (run side): the recording ends the turn only on end_turn, so a
	// max_tokens or an error response loops until the cap. The loop ends the turn on
	// every response that does not run tools: these runs end after their first
	// response. The run span then also carries that stop reason (A-88).
	if runStopFixtures[name] {
		endTurnAtFirstResponse(t, want)
	}
	// Unknown tool: the recording retries it with backoff (two ToolRetry, then
	// ToolCompleted{attempt 2}). The loop answers it at once: ToolCompleted{attempt 0}.
	if unknownToolFixtures[name] {
		noRetryOfUnknownTool(t, want)
	}
	// Stored working set: the recording stores the compacted working set with the
	// system message in front, so from the next turn on the model input has two
	// system messages and the second compaction protects both. The loop stores it
	// without the system message.
	if name == "agentloop-run-second-compaction" {
		secondCompactionHasOneSystemMessage(t, want)
	}
	// Stream usage: the recording never puts the usage on ModelResponded in a stream.
	// The loop does when the provider's done chunk reported one, as Run does.
	streamUsageOnModelResponded(t, want)
	// Run usage: the recording seeds a run's usage total with the session's current
	// usage. The loop counts only the run's own model calls, and feeds the compactor
	// the context window size the latest response reported.
	runUsageWithoutSeed(t, want)
}

// runStopFixtures are the Run fixtures whose responses stop with max_tokens or
// error (stop rule).
var runStopFixtures = map[string]bool{
	"agentloop-run-max-tokens-cap": true,
	"agentloop-run-error-stop-cap": true,
}

// unknownToolFixtures are the fixtures that call a tool that does not exist.
var unknownToolFixtures = map[string]bool{
	"agentloop-run-unknown-tool":    true,
	"agentloop-stream-unknown-tool": true,
}

// decodeObject decodes a JSON object (a payload or a result) for editing.
func decodeObject(t *testing.T, raw []byte) ordered.Object {
	t.Helper()
	var o ordered.Object
	if err := o.UnmarshalJSON(raw); err != nil {
		t.Fatalf("decode %s: %v", raw, err)
	}
	return o
}

// encodeRaw encodes an edited object back to wire bytes.
func encodeRaw(t *testing.T, o ordered.Object) json.RawMessage {
	t.Helper()
	b, err := kernel.EncodeJSON(o)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// intOf reads a number of a decoded object (absent is zero).
func intOf(t *testing.T, o ordered.Object, key string) int {
	t.Helper()
	v, ok := o.Get(key)
	if !ok {
		return 0
	}
	switch n := v.(type) {
	case json.Number:
		i, err := n.Int64()
		if err != nil {
			t.Fatalf("%s = %v: %v", key, n, err)
		}
		return int(i)
	case int:
		return n
	default:
		t.Fatalf("%s is a %T", key, v)
		return 0
	}
}

// objectOf reads a nested object of a decoded object.
func objectOf(t *testing.T, o ordered.Object, key string) ordered.Object {
	t.Helper()
	v, ok := o.Get(key)
	if !ok {
		t.Fatalf("no %s among %v", key, o.Keys())
	}
	n, ok := v.(ordered.Object)
	if !ok {
		t.Fatalf("%s is a %T", key, v)
	}
	return n
}

// usageAttrs is an {inputTokens, outputTokens} pair.
type usageAttrs struct{ in, out int }

// modelCallUsages returns, in order, the usage of every model-call span that ended
// with a response (an aborted one has no stopReason), and whether it reported one.
func modelCallUsages(t *testing.T, want *tTranscript) (usages []usageAttrs, reported []bool) {
	t.Helper()
	for _, sp := range want.Spans {
		if sp.Phase != "end" || sp.Name != "model-call" || !sp.Attributes.Has("stopReason") {
			continue
		}
		usages = append(usages, usageAttrs{intOf(t, sp.Attributes, "inputTokens"), intOf(t, sp.Attributes, "outputTokens")})
		reported = append(reported, sp.Attributes.Has("inputTokens"))
	}
	return usages, reported
}

// totalUsageObject is a run total as the loop writes it.
func totalUsageObject(u usageAttrs) ordered.Object {
	o := ordered.NewObject()
	o.Set("inputTokens", u.in)
	o.Set("outputTokens", u.out)
	o.Set("cacheReadTokens", 0)
	o.Set("cacheWriteTokens", 0)
	return o
}

// endTurnAtFirstResponse is the expected transcript of a Run whose first response
// ends the turn: everything up to that response, then the completion.
func endTurnAtFirstResponse(t *testing.T, want *tTranscript) {
	t.Helper()
	usages, _ := modelCallUsages(t, want)
	first := usages[0]

	// Events: up to the first ModelResponded, then RunCompleted.
	// A-104: the ContextUsage event that follows the first ModelResponded is kept too.
	var evs []tEvent
	for i, e := range want.Events {
		evs = append(evs, e)
		if e.Type == events.ModelResponded.Name {
			if i+1 < len(want.Events) && want.Events[i+1].Type == events.ContextUsage.Name {
				evs = append(evs, want.Events[i+1])
			}
			break
		}
	}
	done := ordered.NewObject()
	done.Set("agentId", "test-agent")
	done.Set("iterations", 1)
	done.Set("totalUsage", totalUsageObject(first))
	evs = append(evs, tEvent{Type: events.RunCompleted.Name, SessionID: evs[0].SessionID, Payload: encodeRaw(t, done)})
	want.Events = evs

	// Spans: up to the first model-call end, then the iteration end and the run end.
	var iterEnd, runEnd tSpan
	for _, sp := range want.Spans {
		if sp.Phase == "end" && sp.Name == "iteration-1" {
			iterEnd = sp
		}
		if sp.Phase == "end" && sp.Name == "agent-run" {
			runEnd = sp
		}
	}
	var spans []tSpan
	var stop any
	for _, sp := range want.Spans {
		spans = append(spans, sp)
		if sp.Phase == "end" && sp.Name == "model-call" {
			stop, _ = sp.Attributes.Get("stopReason")
			break
		}
	}
	runAttrs := runEnd.Attributes.Clone()
	runAttrs.Set("iterations", 1)
	runAttrs.Set("totalUsage", totalUsageObject(first))
	runAttrs.Delete("maxIterationsReached")
	// A-88: a turn that ends with max_tokens or error puts the stop reason on the
	// run span.
	if stop == string(kernel.StopMaxTokens) || stop == string(kernel.StopError) {
		runAttrs.Set("stopReason", stop)
	}
	runEnd.Attributes = runAttrs
	want.Spans = append(spans, iterEnd, runEnd)

	// Messages: the user message and the first answer, which is also the result.
	want.Messages = want.Messages[:2]
	res := ordered.NewObject()
	msg := ordered.NewObject()
	msg.Set("role", string(want.Messages[1].Role))
	msg.Set("content", want.Messages[1].Content)
	res.Set("message", msg)
	res.Set("usage", totalUsageObject(first))
	res.Set("iterations", 1)
	want.Results = []json.RawMessage{encodeRaw(t, res)}
}

// noRetryOfUnknownTool drops the retries of an unknown tool and makes its
// completion the first attempt.
func noRetryOfUnknownTool(t *testing.T, want *tTranscript) {
	t.Helper()
	var evs []tEvent
	for _, e := range want.Events {
		switch e.Type {
		case events.ToolRetry.Name:
			continue
		case events.ToolCompleted.Name:
			p := decodeObject(t, e.Payload)
			p.Set("attempt", 0)
			e.Payload = encodeRaw(t, p)
		}
		evs = append(evs, e)
	}
	want.Events = evs
}

// secondCompactionHasOneSystemMessage is the third run of
// agentloop-run-second-compaction as the loop runs it. The working set the first
// compaction stored is [First question!, SUMMARY, Second question., Second answer]
// without the system message, so the third run prepares 6 messages (the recording
// has 7, two of them system), the second compaction protects the system message and
// "First question!" (the recording: both system messages) and summarises only the
// rest, and the model input keeps the user's first question where the recording has
// the duplicate.
func secondCompactionHasOneSystemMessage(t *testing.T, want *tTranscript) {
	t.Helper()
	compactions := 0
	for i, sp := range want.Spans {
		switch {
		case sp.Name == "compaction" && sp.Phase == "start":
			compactions++
			if compactions == 3 {
				a := sp.Attributes.Clone()
				a.Set("messageCountBefore", 6)
				want.Spans[i].Attributes = a
			}
		case sp.Name == "compaction" && sp.Phase == "end" && compactions == 3:
			a := sp.Attributes.Clone()
			a.Set("messageCountBefore", 6)
			// The summarised middle no longer holds "First question!\n" (16 units).
			a.Set("summarySourceLength", 43)
			want.Spans[i].Attributes = a
		case sp.Name == "context-assembly" && sp.Phase == "end" && compactions == 3:
			a := sp.Attributes.Clone()
			a.Set("preparedMessageCount", 6)
			a.Set("systemMessageIndex", 0)
			v, _ := a.Get("messages")
			msgs := append([]any(nil), v.([]any)...)
			first := ordered.NewObject()
			first.Set("role", "user")
			first.Set("content", "First question!")
			first.Set("timestamp", "2026-01-01T00:00:00.000Z")
			first.Set("estimatedTokens", 4)
			msgs[1] = first // recorded: the duplicate system message, 6 tokens
			a.Set("messages", msgs)
			est := objectOf(t, a, "estimatedTokens").Clone()
			est.Set("messages", 17)
			est.Set("total", 17)
			a.Set("estimatedTokens", est)
			want.Spans[i].Attributes = a
		case sp.Name == "summarize" && compactions == 3:
			// The second summary call summarises 3 messages, not the recorded 4 (no
			// duplicate system message in its middle).
			a := sp.Attributes.Clone()
			a.Set("inputMessageCount", 3)
			want.Spans[i].Attributes = a
		}
	}
	if compactions != 3 || len(want.SummaryRequests) != 2 {
		t.Fatalf("%d compaction spans and %d summary requests, the fixture has 3 and 2", compactions, len(want.SummaryRequests))
	}
	req := want.SummaryRequests[1]
	req.Messages = []tMessage{{Role: kernel.RoleUser, Content: "[ASSISTANT] SUMMARY-TEXT\n\n[USER] Second question.\n\n[ASSISTANT] Second answer"}}
	want.SummaryRequests[1] = req
}

// streamUsageOnModelResponded puts the reported usage on every ModelResponded
// event that lacks one (the stream side), right before hasToolCalls as the
// payload declares it. The usage of the n-th response is the one its model-call
// span ended with.
func streamUsageOnModelResponded(t *testing.T, want *tTranscript) {
	t.Helper()
	usages, reported := modelCallUsages(t, want)
	n := 0
	for i, e := range want.Events {
		if e.Type != events.ModelResponded.Name {
			continue
		}
		p := decodeObject(t, e.Payload)
		if !p.Has("usage") && reported[n] {
			u := ordered.NewObject()
			u.Set("inputTokens", usages[n].in)
			u.Set("outputTokens", usages[n].out)
			out := ordered.NewObject()
			for k, v := range p.All() {
				if k == "hasToolCalls" {
					out.Set("usage", u)
				}
				out.Set(k, v)
			}
			want.Events[i].Payload = encodeRaw(t, out)
		}
		n++
	}
}

// runUsageWithoutSeed removes the session's seed from every run total: the
// run's result or done chunk, the agent-run span end and the RunCompleted or
// MaxIterationsReached event. The seed of a run is the usage of the latest response
// before it started. The compaction span records the compactor's input, which is
// the latest response's usage too (recorded: the seeded running total).
func runUsageWithoutSeed(t *testing.T, want *tTranscript) {
	t.Helper()
	var seeds []usageAttrs
	var last usageAttrs
	for i, sp := range want.Spans {
		switch {
		case sp.Name == "agent-run" && sp.Phase == "start":
			seeds = append(seeds, last)
		case sp.Name == "model-call" && sp.Phase == "end" && sp.Attributes.Has("inputTokens"):
			last = usageAttrs{intOf(t, sp.Attributes, "inputTokens"), intOf(t, sp.Attributes, "outputTokens")}
		case sp.Name == "compaction":
			a := sp.Attributes.Clone()
			a.Set("inputTokens", last.in)
			a.Set("outputTokens", last.out)
			want.Spans[i].Attributes = a
		case sp.Name == "agent-run" && sp.Phase == "end":
			a := sp.Attributes.Clone()
			a.Set("totalUsage", minusSeed(t, objectOf(t, a, "totalUsage"), seeds[len(seeds)-1]))
			want.Spans[i].Attributes = a
		}
	}

	run := -1
	for i, e := range want.Events {
		switch e.Type {
		case events.UserMessage.Name:
			run++
		case events.RunCompleted.Name, events.MaxIterationsReached.Name:
			p := decodeObject(t, e.Payload)
			p.Set("totalUsage", minusSeed(t, objectOf(t, p, "totalUsage"), seeds[run]))
			want.Events[i].Payload = encodeRaw(t, p)
		}
	}

	for i, raw := range want.Results {
		r := decodeObject(t, raw)
		switch {
		case r.Has("usage"):
			r.Set("usage", minusSeed(t, objectOf(t, r, "usage"), seeds[i]))
		case r.Has("chunks"):
			v, _ := r.Get("chunks")
			chunks, _ := v.([]any)
			for j, c := range chunks {
				co, ok := c.(ordered.Object)
				if !ok || !co.Has("usage") {
					continue
				}
				if tp, _ := co.Get("type"); tp != string(kernel.ChunkDone) {
					continue
				}
				co = co.Clone()
				co.Set("usage", minusSeed(t, objectOf(t, co, "usage"), seeds[i]))
				chunks[j] = co
			}
			r.Set("chunks", chunks)
		default:
			continue // a run that threw
		}
		want.Results[i] = encodeRaw(t, r)
	}
}

// minusSeed is a run total without the seed, input and output only.
func minusSeed(t *testing.T, total ordered.Object, seed usageAttrs) ordered.Object {
	t.Helper()
	out := total.Clone()
	out.Set("inputTokens", intOf(t, total, "inputTokens")-seed.in)
	out.Set("outputTokens", intOf(t, total, "outputTokens")-seed.out)
	return out
}

// firstDifference names the first element where two transcripts differ, so a
// failure points at the event, span, message or result to look at.
func firstDifference(t *testing.T, got, want tTranscript) string {
	t.Helper()
	enc := func(v any) string {
		b, err := kernel.EncodeJSON(v)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	sections := []struct {
		name      string
		got, want []string
	}{
		{"results", encAll(enc, got.Results), encAll(enc, want.Results)},
		{"events", encAll(enc, got.Events), encAll(enc, want.Events)},
		{"spans", encAll(enc, got.Spans), encAll(enc, want.Spans)},
		{"messages", encAll(enc, got.Messages), encAll(enc, want.Messages)},
		{"approvalRequests", encAll(enc, got.ApprovalRequests), encAll(enc, want.ApprovalRequests)},
		{"summaryRequests", encAll(enc, got.SummaryRequests), encAll(enc, want.SummaryRequests)},
	}
	for _, sec := range sections {
		for i := range max(len(sec.got), len(sec.want)) {
			var g, w string
			if i < len(sec.got) {
				g = sec.got[i]
			}
			if i < len(sec.want) {
				w = sec.want[i]
			}
			if g != w {
				return fmt.Sprintf("first difference at %s[%d]\n  got: %s\n want: %s", sec.name, i, g, w)
			}
		}
	}
	return "no element differs"
}

func encAll[E any](enc func(any) string, xs []E) []string {
	out := make([]string, len(xs))
	for i, x := range xs {
		out[i] = enc(x)
	}
	return out
}
