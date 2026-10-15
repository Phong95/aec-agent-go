package conformance

import (
	"slices"
	"testing"

	"github.com/Phong95/aec-agent-go/components/channelplatformhub/channel"
	"github.com/Phong95/aec-agent-go/components/persistence"
	"github.com/Phong95/aec-agent-go/conformance/internal/check"
	"github.com/Phong95/aec-agent-go/kernel"
)

// ChannelStoreFactory returns a persistence.ChannelStore. See the package comment for
// the freshness rules of the store suites.
type ChannelStoreFactory func(t *testing.T) persistence.ChannelStore

// channelFixture builds a channel with a unique id (and agent, unless agentID is set).
func channelFixture(agentID, createdAt string) channel.Channel {
	if agentID == "" {
		agentID = uniqueAgentID()
	}
	if createdAt == "" {
		createdAt = "2026-10-07T00:00:00.000Z"
	}
	return channel.Channel{
		ID: kernel.NewID(), AgentID: agentID, Platform: "telegram", Name: "Support bot",
		Credentials: map[string]string{"botToken": "123:secret"},
		Access:      channel.DefaultAccess(),
		Settings:    channel.DefaultSettings(),
		Enabled:     true, CreatedAt: createdAt, UpdatedAt: createdAt,
	}
}

func channelIDs(cs []channel.Channel) []string {
	out := make([]string, len(cs))
	for i, c := range cs {
		out[i] = c.ID
	}
	return out
}

// ChannelStore runs the conformance suite for persistence.ChannelStore implementations
// (chat-app channels, A-100), the testChannelStoreConformance suite of core/ts. It
// checks the invariants the agent's channel manager relies on: channels round-trip
// with every field (credentials, access, account), save replaces, lists are scoped per
// agent and ordered oldest first, records are copied in and out, and deletes work by id
// and by agent.
func ChannelStore(t *testing.T, newStore ChannelStoreFactory) {
	t.Helper()

	fresh := func(t *testing.T) persistence.ChannelStore {
		t.Helper()
		s := newStore(t)
		check.NoErr(t, kernel.Initialize(t.Context(), s))
		return s
	}
	save := func(t *testing.T, s persistence.ChannelStore, c channel.Channel) {
		t.Helper()
		check.NoErr(t, s.Save(t.Context(), c), "save %s", c.ID)
	}
	get := func(t *testing.T, s persistence.ChannelStore, id string) *channel.Channel {
		t.Helper()
		got, err := s.Get(t.Context(), id)
		check.NoErr(t, err, "get %s", id)
		return got
	}
	list := func(t *testing.T, s persistence.ChannelStore, agentID string) []channel.Channel {
		t.Helper()
		out, err := s.ListByAgent(t.Context(), agentID)
		check.NoErr(t, err, "list %s", agentID)
		return out
	}

	t.Run("save() + get() round-trip every field", func(t *testing.T) {
		s := fresh(t)
		full := channelFixture("", "")
		full.Credentials = map[string]string{"botToken": "123:abc", "extra": "x"}
		full.Access = channel.Access{DmPolicy: channel.DmAllowlist, AllowFrom: []string{"1", "2"}, GroupPolicy: channel.GroupAllowlist, Groups: []string{"-100"}, RequireMention: false}
		full.Settings = channel.Settings{Streaming: false, ToolProgress: false}
		full.Enabled = false
		full.Account = &channel.AccountInfo{ID: "42", Username: "my_bot", DisplayName: "My Bot"}
		// Phase 3: the home chat (/sethome).
		full.Home = &channel.Home{ChatID: "-100", ThreadID: "7", Title: "Site team", SetBy: "1", SetAt: "2026-01-02T00:00:00.000Z"}
		bare := channelFixture("", "")
		save(t, s, full)
		save(t, s, bare)

		for _, want := range []channel.Channel{full, bare} {
			got := get(t, s, want.ID)
			if got == nil {
				t.Fatalf("channel %s is absent", want.ID)
			}
			check.DeepEqual(t, *got, want, "round-trip of %s", want.ID)
		}
	})

	t.Run("get() returns nil for an unknown id", func(t *testing.T) {
		s := fresh(t)
		check.True(t, get(t, s, kernel.NewID()) == nil, "unknown id must be nil")
	})

	t.Run("save() replaces an existing channel, including dropping its account", func(t *testing.T) {
		s := fresh(t)
		original := channelFixture("", "")
		original.Account = &channel.AccountInfo{ID: "1", Username: "old_bot"}
		save(t, s, original)
		changed := original.Clone()
		changed.Account = nil
		changed.Name = "Renamed"
		changed.Credentials = map[string]string{"botToken": "new"}
		changed.Access.DmPolicy = channel.DmOpen
		changed.Settings = channel.Settings{Streaming: false, ToolProgress: true}
		save(t, s, changed)

		got := get(t, s, original.ID)
		if got == nil {
			t.Fatal("channel is absent")
		}
		check.DeepEqual(t, *got, changed, "replaced channel")
		check.Len(t, list(t, s, original.AgentID), 1)
	})

	t.Run("listByAgent() returns only that agent's channels, oldest created first", func(t *testing.T) {
		s := fresh(t)
		agentID := uniqueAgentID()
		second := channelFixture(agentID, "2026-01-02T00:00:00.000Z")
		first := channelFixture(agentID, "2026-01-01T00:00:00.000Z")
		save(t, s, second)
		save(t, s, first)
		save(t, s, channelFixture("", ""))

		check.True(t, slices.Equal(channelIDs(list(t, s, agentID)), []string{first.ID, second.ID}), "order")
		check.Len(t, list(t, s, uniqueAgentID()), 0)
	})

	t.Run("records are copied in and out", func(t *testing.T) {
		s := fresh(t)
		original := channelFixture("", "")
		save(t, s, original)
		// Mutating the value passed to Save must not reach the store.
		original.Credentials["botToken"] = "mutated after save"

		got := get(t, s, original.ID)
		check.Equal(t, got.Credentials["botToken"], "123:secret", "the store kept the caller's map")
		got.Access.AllowFrom = append(got.Access.AllowFrom, "intruder")
		got.Credentials["botToken"] = "changed"

		again := get(t, s, original.ID)
		check.Equal(t, again.Credentials["botToken"], "123:secret", "a returned map is shared")
		check.Len(t, again.Access.AllowFrom, 0, "a returned slice is shared")
	})

	t.Run("delete() removes one channel", func(t *testing.T) {
		s := fresh(t)
		agentID := uniqueAgentID()
		keep := channelFixture(agentID, "")
		gone := channelFixture(agentID, "")
		save(t, s, keep)
		save(t, s, gone)

		check.NoErr(t, s.Delete(t.Context(), gone.ID))

		check.True(t, get(t, s, gone.ID) == nil, "deleted channel is still there")
		check.True(t, slices.Equal(channelIDs(list(t, s, agentID)), []string{keep.ID}), "the other channel must stay")
	})

	t.Run("deleteByAgent() removes every channel of one agent only", func(t *testing.T) {
		s := fresh(t)
		agentID := uniqueAgentID()
		save(t, s, channelFixture(agentID, ""))
		save(t, s, channelFixture(agentID, ""))
		other := channelFixture("", "")
		save(t, s, other)

		check.NoErr(t, s.DeleteByAgent(t.Context(), agentID))

		check.Len(t, list(t, s, agentID), 0)
		check.True(t, get(t, s, other.ID) != nil, "another agent's channel was deleted")
	})
}
