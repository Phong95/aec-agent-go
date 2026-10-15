package agentloop_test

import (
	"testing"

	"github.com/Phong95/aec-agent-go/components/agent/agentloop"
	"github.com/Phong95/aec-agent-go/internal/jsonx"
	"github.com/Phong95/aec-agent-go/internal/wiretest"
	"github.com/Phong95/aec-agent-go/kernel"
)

// TestRepairToolPairsMatchesGolden replays the recorded fixture agentloop-tool-pairs.json:
// each output, and whether the input array itself came back.
func TestRepairToolPairsMatchesGolden(t *testing.T) {
	var f struct {
		Entries []struct {
			Name     string           `json:"name"`
			Messages []kernel.Message `json:"messages"`
			Result   []kernel.Message `json:"result"`
			Same     bool             `json:"same"`
		} `json:"entries"`
	}
	if err := jsonx.UnmarshalStrict(wiretest.Load(t, "agentloop-tool-pairs.json"), &f); err != nil {
		t.Fatalf("decode fixture: %v", err)
	}
	if len(f.Entries) < 9 {
		t.Fatalf("the corpus has %d entries, want at least 9", len(f.Entries))
	}
	for _, e := range f.Entries {
		t.Run(e.Name, func(t *testing.T) {
			in := e.Messages
			got := agentloop.RepairToolPairs(in)
			if g, w := encodeMessages(t, got), encodeMessages(t, e.Result); g != w {
				t.Fatalf("output differs from TS\n got: %s\nwant: %s", g, w)
			}
			if same := sameSlice(got, in); same != e.Same {
				t.Fatalf("returned the input slice = %v, TS = %v", same, e.Same)
			}
		})
	}
}

// T4: a result whose call is gone is dropped (the broken GPT-2 session shape).
func TestRepairToolPairs_DropsOrphanResult(t *testing.T) {
	msgs := []kernel.Message{
		{Role: kernel.RoleUser, Content: "make a landing page"},
		{Role: kernel.RoleAssistant, Content: "## Goal\nsummary"},
		resultMsg("yLO7", "ok"),
		callMsg("dlAa"), resultMsg("dlAa", "ok"),
		{Role: kernel.RoleUser, Content: "hey"},
	}
	out := agentloop.RepairToolPairs(msgs)
	if len(out) != 5 {
		t.Fatalf("%d messages left, want 5 (the orphan result message dropped)", len(out))
	}
	for _, m := range out {
		for _, r := range m.ToolResults {
			if r.ToolCallID == "yLO7" {
				t.Fatal("the orphan result is still there")
			}
		}
	}
	assertWellPaired(t, out)
}

// T5: a call that never got a result is dropped and the message text is kept.
func TestRepairToolPairs_DropsUnansweredCall(t *testing.T) {
	withText := callMsg("x")
	withText.Content = "Writing the file now"
	msgs := []kernel.Message{{Role: kernel.RoleUser, Content: "go"}, withText, {Role: kernel.RoleUser, Content: "still there?"}}
	out := agentloop.RepairToolPairs(msgs)
	if len(out) != 3 {
		t.Fatalf("%d messages, want 3", len(out))
	}
	if want := (kernel.Message{Role: kernel.RoleAssistant, Content: "Writing the file now"}); out[1].Role != want.Role || out[1].Content != want.Content || out[1].ToolCalls != nil || out[1].ToolResults != nil {
		t.Fatalf("message = %+v, want the text alone", out[1])
	}
	// The caller's message keeps its call: repair works on copies.
	if len(withText.ToolCalls) != 1 || len(msgs[1].ToolCalls) != 1 {
		t.Fatal("the input message was changed")
	}
}

// T6: the same slice comes back when everything is paired.
func TestRepairToolPairs_NoChange(t *testing.T) {
	msgs := []kernel.Message{{Role: kernel.RoleUser, Content: "go"}, callMsg("a"), resultMsg("a", "ok")}
	out := agentloop.RepairToolPairs(msgs)
	if !sameSlice(out, msgs) {
		t.Fatal("a repaired copy came back for a paired history")
	}
}

func TestCarriesToolResults(t *testing.T) {
	for name, tc := range map[string]struct {
		m    kernel.Message
		want bool
	}{
		"legacy tool role":    {kernel.Message{Role: kernel.RoleTool}, true},
		"tool results":        {resultMsg("a", "ok"), true},
		"empty result list":   {kernel.Message{Role: kernel.RoleAssistant, ToolResults: []kernel.ToolResult{}}, false},
		"tool calls only":     {callMsg("a"), false},
		"plain user message":  {kernel.Message{Role: kernel.RoleUser, Content: "hi"}, false},
		"assistant with text": {kernel.Message{Role: kernel.RoleAssistant, Content: "hi"}, false},
	} {
		if got := agentloop.CarriesToolResults(tc.m); got != tc.want {
			t.Errorf("%s: CarriesToolResults = %v, want %v", name, got, tc.want)
		}
	}
}
