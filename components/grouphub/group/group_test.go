package group_test

import (
	"encoding/json"
	"errors"
	"reflect"
	"testing"

	"github.com/Phong95/aec-agent-go/components/grouphub/group"
	"github.com/Phong95/aec-agent-go/kernel"
)

func TestPlan_JSONIsTheWireShapeOfCoreTS(t *testing.T) {
	cases := []struct {
		plan group.Plan
		want string
	}{
		{group.Plan{Kind: group.PlanSingle, AgentID: "spec"}, `{"kind":"single","agentId":"spec"}`},
		{group.Plan{Kind: group.PlanRound, AgentIDs: []string{"a", "b"}, Summarize: true}, `{"kind":"round","agentIds":["a","b"],"summarize":true}`},
		{group.Plan{Kind: group.PlanRound, AgentIDs: []string{"a"}}, `{"kind":"round","agentIds":["a"],"summarize":false}`},
		{group.Plan{Kind: group.PlanLead, LeadID: "rfi", Helpers: []string{"spec"}}, `{"kind":"lead","leadId":"rfi","helpers":["spec"]}`},
		{group.Plan{Kind: group.PlanLead, LeadID: "rfi"}, `{"kind":"lead","leadId":"rfi","helpers":[]}`},
		{group.Plan{Kind: group.PlanNone, Reason: "nobody"}, `{"kind":"none","reason":"nobody"}`},
	}
	for _, c := range cases {
		raw, err := kernel.EncodeJSON(c.plan)
		if err != nil {
			t.Fatal(err)
		}
		if string(raw) != c.want {
			t.Fatalf("encode %+v = %s, want %s", c.plan, raw, c.want)
		}
		var back group.Plan
		if err := json.Unmarshal(raw, &back); err != nil {
			t.Fatal(err)
		}
		again, _ := kernel.EncodeJSON(back)
		if string(again) != c.want {
			t.Fatalf("round trip of %s = %s", c.want, again)
		}
	}
}

func TestMessage_JSONOmitsWhatIsAbsentAndKeepsAnEmptyToList(t *testing.T) {
	user := group.Message{ID: "m1", SessionID: "s", GroupID: "g", Seq: 1, Kind: group.KindUser, Text: "hi", To: []string{}, CreatedAt: "c", UpdatedAt: "u"}
	raw, err := kernel.EncodeJSON(user)
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"id":"m1","sessionId":"s","groupId":"g","seq":1,"kind":"user","text":"hi","to":[],"createdAt":"c","updatedAt":"u"}`; string(raw) != want {
		t.Fatalf("got %s, want %s", raw, want)
	}
	note := group.Message{ID: "m2", SessionID: "s", GroupID: "g", Seq: 2, Kind: group.KindEvent, Text: "n", CreatedAt: "c", UpdatedAt: "u"}
	raw, _ = kernel.EncodeJSON(note)
	if want := `{"id":"m2","sessionId":"s","groupId":"g","seq":2,"kind":"event","text":"n","createdAt":"c","updatedAt":"u"}`; string(raw) != want {
		t.Fatalf("got %s, want %s", raw, want)
	}
}

func TestResolveLimits_DefaultsAndCaps(t *testing.T) {
	if got := group.ResolveLimits(group.Limits{}); got != group.DefaultLimits() {
		t.Fatalf("empty = %+v", got)
	}
	got := group.ResolveLimits(group.Limits{MaxMembersPerRound: 99, MaxTokensPerTurn: -1, MaxElapsedMsPerTurn: 1000, MemberTimeoutMs: 99 * 60_000})
	want := group.Limits{MaxMembersPerRound: 8, MaxTokensPerTurn: 400_000, MaxElapsedMsPerTurn: 1000, MemberTimeoutMs: 30 * 60_000}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

func TestError_IsAndName(t *testing.T) {
	var err error = group.NewError("busy", group.CodeBusy)
	if !errors.Is(err, &group.Error{}) || !errors.Is(err, &group.Error{Code: group.CodeBusy}) || errors.Is(err, &group.Error{Code: group.CodeNotFound}) {
		t.Fatal("errors.Is does not match by code")
	}
	var sdk *kernel.SDKError
	if !errors.As(err, &sdk) || sdk.Retryable {
		t.Fatal("not an unretryable SDKError")
	}
	var gerr *group.Error
	if !errors.As(err, &gerr) || gerr.Name() != "GroupError" {
		t.Fatalf("not a GroupError: %v", err)
	}
}
