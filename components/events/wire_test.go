package events_test

import (
	"bytes"
	"encoding/json"
	"reflect"
	"testing"

	"github.com/Phong95/aec-agent-go/components/events"
	"github.com/Phong95/aec-agent-go/internal/jsonx"
	"github.com/Phong95/aec-agent-go/internal/wiretest"
	"github.com/Phong95/aec-agent-go/kernel"
)

// wireEnvelope is the test-local form of an event that keeps the payload bytes
// raw, so the exact recorded payload bytes can be compared.
type wireEnvelope struct {
	Type      string          `json:"type"`
	SessionID string          `json:"sessionId"`
	Payload   json.RawMessage `json:"payload"`
	ID        string          `json:"id"`
	Timestamp int64           `json:"timestamp"`
}

func loadFixtureEvents(t *testing.T) []json.RawMessage {
	t.Helper()
	var elems []json.RawMessage
	if err := jsonx.UnmarshalStrict(wiretest.Load(t, "events-all-22.json"), &elems); err != nil {
		t.Fatalf("decode events-all-22.json: %v", err)
	}
	// Update (A-126): 20 - the two code-job events were removed with scheduled tasks;
	// the file keeps its recorded name.
	if len(elems) != 20 {
		t.Fatalf("events-all-22.json has %d events, want 20", len(elems))
	}
	return elems
}

// TestEventsAll22RoundTrip decodes every recorded event, re-encodes it as an event
// envelope, and re-encodes every payload through its Go struct; all of it must
// equal the recorded bytes.
func TestEventsAll22RoundTrip(t *testing.T) {
	byKind := map[string]sample{}
	for _, s := range samples() {
		byKind[s.kind] = s
	}
	for i, elem := range loadFixtureEvents(t) {
		var env wireEnvelope
		if err := jsonx.UnmarshalStrict(elem, &env); err != nil {
			t.Fatalf("event %d: strict decode: %v", i, err)
		}
		again, err := kernel.EncodeJSON(env)
		if err != nil {
			t.Fatalf("event %d: encode: %v", i, err)
		}
		if !bytes.Equal(again, elem) {
			t.Errorf("event %d (%s) envelope re-encode differs\n got: %s\nwant: %s", i, env.Type, again, elem)
		}

		s, ok := byKind[env.Type]
		if !ok {
			t.Errorf("event %d: no sample for kind %q", i, env.Type)
			continue
		}
		got, err := s.reencode(kernel.Event{Type: env.Type, Payload: env.Payload})
		if err != nil {
			t.Errorf("event %d (%s): PayloadAs: %v", i, s.name, err)
			continue
		}
		if !bytes.Equal(got, env.Payload) {
			t.Errorf("%s payload re-encode differs\n got: %s\nwant: %s", s.name, got, env.Payload)
		}
	}
}

// TestPayloadAsFromDecodedEvents covers the durable-bus replay path: an event
// decoded into kernel.Event carries its payload as map[string]any. Map key
// order is not preserved, so the comparison is semantic.
func TestPayloadAsFromDecodedEvents(t *testing.T) {
	byKind := map[string]sample{}
	for _, s := range samples() {
		byKind[s.kind] = s
	}
	for i, elem := range loadFixtureEvents(t) {
		var e kernel.Event
		if err := jsonx.UnmarshalStrict(elem, &e); err != nil {
			t.Fatalf("event %d: decode: %v", i, err)
		}
		s := byKind[e.Type]
		got, err := s.reencode(e)
		if err != nil {
			t.Errorf("%s: PayloadAs from a decoded map: %v", s.name, err)
			continue
		}
		var env wireEnvelope
		if err := jsonx.UnmarshalStrict(elem, &env); err != nil {
			t.Fatal(err)
		}
		var a, b any
		if err := json.Unmarshal(got, &a); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(env.Payload, &b); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(a, b) {
			t.Errorf("%s: payload from decoded map differs semantically\n got: %s\nwant: %s", s.name, got, env.Payload)
		}
	}
}

// TestDispatcherReproducesGoldenSequence emits the 22 payloads as Go structs through a
// Dispatcher with the same deterministic clock and ID source that wiregen used to
// record the fixture and compares the whole sequence with the fixture byte for byte.
func TestDispatcherReproducesGoldenSequence(t *testing.T) {
	d := events.New(
		events.WithClock(wiretest.Clock(1700000000000, 1)),
		events.WithIDFunc(wiretest.IDs()),
	)
	var out []kernel.Event
	for _, s := range samples() {
		out = append(out, s.emit(d))
	}
	got, err := kernel.EncodeJSON(out)
	if err != nil {
		t.Fatalf("encode events: %v", err)
	}
	wiretest.AssertBytes(t, "events-all-22.json", got)

	// Every emitted event is also in the log, in order.
	var replayed []kernel.Event
	for e, err := range d.Replay(t.Context(), "s1", 0) {
		if err != nil {
			t.Fatal(err)
		}
		replayed = append(replayed, e)
	}
	if !reflect.DeepEqual(replayed, out) {
		t.Fatal("replay differs from the emitted events")
	}
}

// WR-01: an approval handler can return reason "", which is written as
// "reason":"". The Reason fields are pointers so that survives; an absent
// reason stays absent.
func TestReasonEmptyVersusAbsent(t *testing.T) {
	enc := func(v any) string {
		t.Helper()
		b, err := kernel.EncodeJSON(v)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	if got, want := enc(events.ToolDeniedPayload{AgentID: "a", Name: "n", Reason: strPtr("")}), `{"agentId":"a","name":"n","reason":""}`; got != want {
		t.Errorf("denied empty: got %s want %s", got, want)
	}
	if got, want := enc(events.ToolDeniedPayload{AgentID: "a", Name: "n"}), `{"agentId":"a","name":"n"}`; got != want {
		t.Errorf("denied absent: got %s want %s", got, want)
	}
	p := events.ToolApprovalResolvedPayload{AgentID: "a", Name: "n", ToolCallID: "c", Approved: true, Reason: strPtr(""), WaitedMs: 1}
	want := `{"agentId":"a","name":"n","toolCallId":"c","approved":true,"reason":"","waitedMs":1}`
	if got := enc(p); got != want {
		t.Errorf("resolved empty: got %s want %s", got, want)
	}
	var back events.ToolApprovalResolvedPayload
	if err := jsonx.UnmarshalStrict([]byte(want), &back); err != nil {
		t.Fatal(err)
	}
	if back.Reason == nil || *back.Reason != "" {
		t.Errorf("explicit empty reason decoded as %v", back.Reason)
	}
	p.Reason = nil
	if got, want := enc(p), `{"agentId":"a","name":"n","toolCallId":"c","approved":true,"waitedMs":1}`; got != want {
		t.Errorf("resolved absent: got %s want %s", got, want)
	}
}

// TestToolApprovalRequestedGatedRoundTrip decodes the recorded approval request a real
// loop emitted for a sandbox-gated call (reason and sandbox present) strictly, and
// requires the envelope and the typed payload to re-encode to the recorded bytes.
// Before Phase 9 the payload had no Reason and Sandbox, so the strict decode rejected
// it.
func TestToolApprovalRequestedGatedRoundTrip(t *testing.T) {
	raw := wiretest.Load(t, "event-tool-approval-requested-gated.json")
	var env wireEnvelope
	if err := jsonx.UnmarshalStrict(raw, &env); err != nil {
		t.Fatalf("strict decode of the envelope: %v", err)
	}
	if env.Type != events.ToolApprovalRequested.Name {
		t.Fatalf("type = %q", env.Type)
	}
	again, err := kernel.EncodeJSON(env)
	if err != nil || !bytes.Equal(again, raw) {
		t.Fatalf("envelope re-encode differs (%v)\n got: %s\nwant: %s", err, again, raw)
	}

	p, err := events.PayloadAs[events.ToolApprovalRequestedPayload](kernel.Event{Type: env.Type, Payload: env.Payload})
	if err != nil {
		t.Fatalf("PayloadAs: %v", err)
	}
	if p.Reason == "" || p.Sandbox == nil || p.Sandbox.ID != "host-1" || p.Sandbox.Kind != "host" || p.Sandbox.Isolated {
		t.Fatalf("payload = %+v, want a reason and the non-isolated host sandbox", p)
	}
	got, err := kernel.EncodeJSON(p)
	if err != nil || !bytes.Equal(got, env.Payload) {
		t.Fatalf("payload re-encode differs (%v)\n got: %s\nwant: %s", err, got, env.Payload)
	}

	// A request without a gate keeps the old shape: no reason and no sandbox key.
	plain := events.ToolApprovalRequestedPayload{AgentID: "a", Name: "n", ToolCallID: "c", Risk: "consequential", Arguments: kernel.Arguments(`{}`)}
	enc, err := kernel.EncodeJSON(plain)
	if err != nil || string(enc) != `{"agentId":"a","name":"n","toolCallId":"c","risk":"consequential","arguments":{}}` {
		t.Fatalf("ungated payload = %s (%v)", enc, err)
	}
}
