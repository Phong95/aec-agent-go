package grouphub_test

import (
	"encoding/json"
	"os"
	"reflect"
	"slices"
	"testing"

	"github.com/Phong95/aec-agent-go/components/grouphub"
	"github.com/Phong95/aec-agent-go/components/grouphub/group"
	"github.com/Phong95/aec-agent-go/kernel"
)

// A-108 / A-123: the recipient table is shared with the TypeScript SDK
// (core/ts/tests/unit/group-recipients.test.ts reads testdata/recipients.json), so both
// resolve every mode x To x mention x muted combination the same way.
type recipientFixture struct {
	Members []grouphub.MemberRef `json:"members"`
	LeadID  string               `json:"leadId"`
	Cases   []struct {
		Name      string              `json:"name"`
		ReplyMode group.ReplyMode     `json:"replyMode"`
		To        []string            `json:"to"`
		Muted     []string            `json:"muted"`
		Text      string              `json:"text"`
		Want      grouphub.Recipients `json:"want"`
	} `json:"cases"`
}

func loadRecipientFixture(t *testing.T) recipientFixture {
	t.Helper()
	raw, err := os.ReadFile("testdata/recipients.json")
	if err != nil {
		t.Fatal(err)
	}
	var f recipientFixture
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatal(err)
	}
	return f
}

func TestResolveRecipients_SharedFixture(t *testing.T) {
	f := loadRecipientFixture(t)
	if len(f.Cases) < 96 {
		t.Fatalf("the fixture has %d cases, want every reply mode, To list, mention and muted set (>= 96)", len(f.Cases))
	}
	for _, c := range f.Cases {
		t.Run(c.Name, func(t *testing.T) {
			got := grouphub.ResolveRecipients(grouphub.RecipientInput{
				Text: c.Text, To: c.To, Members: f.Members, LeadID: f.LeadID, ReplyMode: c.ReplyMode, Muted: c.Muted,
			})
			if !reflect.DeepEqual(got, c.Want) {
				t.Fatalf("got %+v, want %+v", got, c.Want)
			}
			// The wire form is the one core/ts writes.
			gotJSON, err := kernel.EncodeJSON(got)
			if err != nil {
				t.Fatal(err)
			}
			wantJSON, err := kernel.EncodeJSON(c.Want)
			if err != nil {
				t.Fatal(err)
			}
			if string(gotJSON) != string(wantJSON) {
				t.Fatalf("json %s, want %s", gotJSON, wantJSON)
			}
		})
	}
}

func TestMentionedMembers_CaseInsensitiveNamesAndIdsInGroupOrder(t *testing.T) {
	f := loadRecipientFixture(t)
	if got := grouphub.MentionedMembers("@takeoff then @rfi desk", f.Members); !slices.Equal(got, []string{"rfi", "takeoff"}) {
		t.Fatalf("got %v", got)
	}
	if got := grouphub.MentionedMembers("no mentions here", f.Members); len(got) != 0 {
		t.Fatalf("got %v", got)
	}
	// A longer name wins over a shorter one it starts with, and the boundary is a
	// non-name character.
	members := []grouphub.MemberRef{{ID: "a", Name: "Spec"}, {ID: "b", Name: "Spec Checker"}}
	if got := grouphub.MentionedMembers("ask @spec checker, then @spec-x and @spec.", members); !slices.Equal(got, []string{"a", "b"}) {
		t.Fatalf("got %v", got)
	}
	if got := grouphub.MentionedMembers("@specs only", members); len(got) != 0 {
		t.Fatalf("got %v", got)
	}
}
