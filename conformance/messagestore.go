package conformance

import (
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"sync"
	"testing"

	"github.com/Phong95/aec-agent-go/components/persistence"
	"github.com/Phong95/aec-agent-go/conformance/internal/check"
	"github.com/Phong95/aec-agent-go/kernel"
)

// MessageStoreFactory returns a persistence.MessageStore. See the package
// comment for the freshness rules of the store suites.
type MessageStoreFactory func(t *testing.T) persistence.MessageStore

// jsonEqual reports whether two JSON-shaped values encode to equal JSON. It
// decodes both encodings into any, so object key order, number spelling and
// insignificant white space do not matter: a JSONB column reorders object keys,
// so a suite must never compare such values byte for byte.
func jsonEqual(a, b any) bool {
	ea, err := kernel.EncodeJSON(a)
	if err != nil {
		return false
	}
	eb, err := kernel.EncodeJSON(b)
	if err != nil {
		return false
	}
	var va, vb any
	if json.Unmarshal(ea, &va) != nil || json.Unmarshal(eb, &vb) != nil {
		return false
	}
	return reflect.DeepEqual(va, vb)
}

// sameSet reports whether got holds exactly the wanted strings, in any order.
func sameSet(got []string, want ...string) bool {
	g := slices.Sorted(slices.Values(got))
	w := slices.Sorted(slices.Values(want))
	return slices.Equal(g, w)
}

// messageFor builds message i of a run: the content carries the run prefix and
// the index, the roles alternate.
func messageFor(prefix string, i int) kernel.Message {
	role := kernel.RoleUser
	if i%2 == 1 {
		role = kernel.RoleAssistant
	}
	return kernel.Message{
		Role:      role,
		Content:   fmt.Sprintf("%s-m%d", prefix, i),
		Timestamp: "2026-01-01T00:00:00.000Z",
	}
}

// messageContents lists the contents of the messages.
func messageContents(msgs []kernel.Message) []string {
	out := make([]string, 0, len(msgs))
	for _, m := range msgs {
		out = append(out, m.Content)
	}
	return out
}

// expectedContents is the contents of messages [from, to) of a run.
func expectedContents(prefix string, from, to int) []string {
	out := []string{}
	for i := from; i < to; i++ {
		out = append(out, fmt.Sprintf("%s-m%d", prefix, i))
	}
	return out
}

func appendMessages(t *testing.T, s persistence.MessageStore, sessionID, prefix string, n int) {
	t.Helper()
	for i := range n {
		check.NoErr(t, s.Append(t.Context(), sessionID, messageFor(prefix, i)), "append %d", i)
	}
}

func getMessages(t *testing.T, s persistence.MessageStore, sessionID string) []kernel.Message {
	t.Helper()
	out, err := s.Get(t.Context(), sessionID)
	check.NoErr(t, err, "get %s", sessionID)
	return out
}

func countMessages(t *testing.T, s persistence.MessageStore, sessionID string) int {
	t.Helper()
	n, err := s.Count(t.Context(), sessionID)
	check.NoErr(t, err, "count %s", sessionID)
	return n
}

// checkRange verifies one GetRange call against the expected window arithmetic:
// the window and the total.
func checkRange(t *testing.T, s persistence.MessageStore, sessionID string, before, limit int, wantContents []string, wantTotal int) {
	t.Helper()
	slice, err := s.GetRange(t.Context(), sessionID, before, limit)
	check.NoErr(t, err, "getRange(%d, %d)", before, limit)
	check.True(t, slices.Equal(messageContents(slice.Messages), wantContents),
		"getRange(%d, %d) returned %v, want %v", before, limit, messageContents(slice.Messages), wantContents)
	check.Equal(t, slice.Total, wantTotal, "total of getRange(%d, %d)", before, limit)
}

// toolMessages are messages whose tool calls and results exercise the JSON-shaped
// parts of a message: nested arguments in a non-sorted key order, markup
// characters and both values of isError.
func toolMessages(prefix string) []kernel.Message {
	yes, no := true, false
	return []kernel.Message{
		{Role: kernel.RoleUser, Content: prefix + "-ask", Timestamp: "2026-01-01T00:00:00.000Z"},
		{
			Role: kernel.RoleAssistant, Content: "",
			ToolCalls: []kernel.ToolCall{
				{ID: prefix + "-call-1", Name: "lookup", Arguments: kernel.Arguments(`{"z":1,"a":{"y":[1,2,3],"b":"<x> & done"},"m":null}`)},
				{ID: prefix + "-call-2", Name: "noop", Arguments: kernel.Arguments(`{}`)},
			},
		},
		{
			Role: kernel.RoleTool, Content: "",
			ToolResults: []kernel.ToolResult{
				{ToolCallID: prefix + "-call-1", Content: "found it", IsError: &no},
				{ToolCallID: prefix + "-call-2", Content: "boom", IsError: &yes},
				{ToolCallID: prefix + "-call-3", Content: "plain"},
			},
		},
	}
}

// MessageStore runs the conformance suite for persistence.MessageStore
// implementations.
//
// It is written for server/go's Postgres stores: it encodes the behaviour of the
// in-memory store that the Postgres store shares - chronological order inside one
// session, the GetRange window arithmetic, replacement by Set, isolation between
// sessions - and nothing that only the in-memory store does. Every session id and
// message content carries a per-run prefix, JSON-shaped values are compared by
// decoding (never byte for byte), and no subtest assumes an empty store. The
// Go-only subtests prove that messages are copied in and out and that concurrent
// appends keep every message.
func MessageStore(t *testing.T, newStore MessageStoreFactory) {
	t.Helper()

	// -- get() / count() --

	t.Run("get() returns an empty list for an unknown session", func(t *testing.T) {
		store := newStore(t)
		check.NoErr(t, kernel.Initialize(t.Context(), store))
		sid := "sess-" + runPrefix()

		got := getMessages(t, store, sid)

		check.True(t, got != nil, "get of an unknown session must be a non-nil empty slice")
		check.Len(t, got, 0)
		check.Equal(t, countMessages(t, store, sid), 0)
	})

	t.Run("append() keeps chronological order", func(t *testing.T) {
		store := newStore(t)
		check.NoErr(t, kernel.Initialize(t.Context(), store))
		p := runPrefix()
		sid := "sess-" + p

		appendMessages(t, store, sid, p, 4)

		check.True(t, slices.Equal(messageContents(getMessages(t, store, sid)), expectedContents(p, 0, 4)),
			"messages are not in append order: %v", messageContents(getMessages(t, store, sid)))
	})

	t.Run("count() reflects the appended messages", func(t *testing.T) {
		store := newStore(t)
		check.NoErr(t, kernel.Initialize(t.Context(), store))
		p := runPrefix()
		sid := "sess-" + p

		appendMessages(t, store, sid, p, 3)

		check.Equal(t, countMessages(t, store, sid), 3)
	})

	// -- getRange() --

	t.Run("getRange(-1, n) returns the last n messages and the total", func(t *testing.T) {
		store := newStore(t)
		check.NoErr(t, kernel.Initialize(t.Context(), store))
		p := runPrefix()
		sid := "sess-" + p
		appendMessages(t, store, sid, p, 5)

		checkRange(t, store, sid, -1, 2, expectedContents(p, 3, 5), 5)
	})

	t.Run("getRange(before, n) returns the window that ends before the index", func(t *testing.T) {
		store := newStore(t)
		check.NoErr(t, kernel.Initialize(t.Context(), store))
		p := runPrefix()
		sid := "sess-" + p
		appendMessages(t, store, sid, p, 5)

		checkRange(t, store, sid, 3, 2, expectedContents(p, 1, 3), 5)
		checkRange(t, store, sid, 1, 4, expectedContents(p, 0, 1), 5)
	})

	t.Run("getRange() with before equal to the total returns the last window", func(t *testing.T) {
		store := newStore(t)
		check.NoErr(t, kernel.Initialize(t.Context(), store))
		p := runPrefix()
		sid := "sess-" + p
		appendMessages(t, store, sid, p, 5)

		checkRange(t, store, sid, 5, 2, expectedContents(p, 3, 5), 5)
	})

	t.Run("getRange() with before above the total behaves like -1", func(t *testing.T) {
		store := newStore(t)
		check.NoErr(t, kernel.Initialize(t.Context(), store))
		p := runPrefix()
		sid := "sess-" + p
		appendMessages(t, store, sid, p, 5)

		checkRange(t, store, sid, 99, 2, expectedContents(p, 3, 5), 5)
	})

	t.Run("getRange() with a limit above the list returns everything before the index", func(t *testing.T) {
		store := newStore(t)
		check.NoErr(t, kernel.Initialize(t.Context(), store))
		p := runPrefix()
		sid := "sess-" + p
		appendMessages(t, store, sid, p, 5)

		checkRange(t, store, sid, 3, 10, expectedContents(p, 0, 3), 5)
		checkRange(t, store, sid, -1, 10, expectedContents(p, 0, 5), 5)
	})

	t.Run("getRange() with a limit of 0 returns no messages and the total", func(t *testing.T) {
		store := newStore(t)
		check.NoErr(t, kernel.Initialize(t.Context(), store))
		p := runPrefix()
		sid := "sess-" + p
		appendMessages(t, store, sid, p, 5)

		checkRange(t, store, sid, -1, 0, []string{}, 5)
		checkRange(t, store, sid, 2, 0, []string{}, 5)
	})

	t.Run("getRange() of an unknown session returns an empty window and a total of 0", func(t *testing.T) {
		store := newStore(t)
		check.NoErr(t, kernel.Initialize(t.Context(), store))
		sid := "sess-" + runPrefix()

		slice, err := store.GetRange(t.Context(), sid, -1, 5)
		check.NoErr(t, err)
		check.Len(t, slice.Messages, 0)
		check.Equal(t, slice.Total, 0)
	})

	// -- set() / clear() --

	t.Run("set() replaces the whole list", func(t *testing.T) {
		store := newStore(t)
		check.NoErr(t, kernel.Initialize(t.Context(), store))
		p := runPrefix()
		sid := "sess-" + p
		appendMessages(t, store, sid, p, 3)

		replacement := []kernel.Message{messageFor(p+"-new", 0), messageFor(p+"-new", 1)}
		check.NoErr(t, store.Set(t.Context(), sid, replacement))

		check.True(t, slices.Equal(messageContents(getMessages(t, store, sid)), expectedContents(p+"-new", 0, 2)),
			"set did not replace the list: %v", messageContents(getMessages(t, store, sid)))
		check.Equal(t, countMessages(t, store, sid), 2)

		// Appending after a set continues after the replacement.
		check.NoErr(t, store.Append(t.Context(), sid, messageFor(p+"-new", 2)))
		check.True(t, slices.Equal(messageContents(getMessages(t, store, sid)), expectedContents(p+"-new", 0, 3)),
			"append after set is out of order: %v", messageContents(getMessages(t, store, sid)))
	})

	t.Run("set() with an empty list leaves an empty session", func(t *testing.T) {
		store := newStore(t)
		check.NoErr(t, kernel.Initialize(t.Context(), store))
		p := runPrefix()
		sid := "sess-" + p
		appendMessages(t, store, sid, p, 3)

		check.NoErr(t, store.Set(t.Context(), sid, []kernel.Message{}))

		check.Len(t, getMessages(t, store, sid), 0)
		check.Equal(t, countMessages(t, store, sid), 0)
	})

	t.Run("clear() empties the session", func(t *testing.T) {
		store := newStore(t)
		check.NoErr(t, kernel.Initialize(t.Context(), store))
		p := runPrefix()
		sid := "sess-" + p
		appendMessages(t, store, sid, p, 3)

		check.NoErr(t, store.Clear(t.Context(), sid))

		check.Len(t, getMessages(t, store, sid), 0)
		check.Equal(t, countMessages(t, store, sid), 0)
		// Clearing an unknown session is not an error.
		check.NoErr(t, store.Clear(t.Context(), "sess-unknown-"+kernel.NewID()))
	})

	// -- Isolation --

	t.Run("two sessions never leak into each other", func(t *testing.T) {
		store := newStore(t)
		check.NoErr(t, kernel.Initialize(t.Context(), store))
		p := runPrefix()
		sidA, sidB := "sess-"+p+"-A", "sess-"+p+"-B"

		appendMessages(t, store, sidA, p+"-a", 3)
		appendMessages(t, store, sidB, p+"-b", 2)

		check.True(t, slices.Equal(messageContents(getMessages(t, store, sidA)), expectedContents(p+"-a", 0, 3)), "session A holds %v", messageContents(getMessages(t, store, sidA)))
		check.True(t, slices.Equal(messageContents(getMessages(t, store, sidB)), expectedContents(p+"-b", 0, 2)), "session B holds %v", messageContents(getMessages(t, store, sidB)))
		check.Equal(t, countMessages(t, store, sidA), 3)
		check.Equal(t, countMessages(t, store, sidB), 2)

		check.NoErr(t, store.Clear(t.Context(), sidA))
		check.Equal(t, countMessages(t, store, sidA), 0)
		check.Equal(t, countMessages(t, store, sidB), 2, "clearing session A changed session B")
	})

	// -- JSON-shaped parts --

	t.Run("tool calls and tool results survive a round trip", func(t *testing.T) {
		store := newStore(t)
		check.NoErr(t, kernel.Initialize(t.Context(), store))
		p := runPrefix()
		sid := "sess-" + p
		want := toolMessages(p)

		for _, m := range want {
			check.NoErr(t, store.Append(t.Context(), sid, m))
		}

		got := getMessages(t, store, sid)
		check.Len(t, got, len(want))
		for i := range min(len(got), len(want)) {
			check.True(t, jsonEqual(got[i], want[i]), "message %d differs: got %+v, want %+v", i, got[i], want[i])
		}

		// The same messages stored with Set read back the same way.
		check.NoErr(t, store.Set(t.Context(), sid, want))
		got = getMessages(t, store, sid)
		check.Len(t, got, len(want))
		for i := range min(len(got), len(want)) {
			check.True(t, jsonEqual(got[i], want[i]), "message %d differs after set: got %+v, want %+v", i, got[i], want[i])
		}
	})

	// -- Go-only guarantees --

	t.Run("mutating returned or given messages does not change the store (Go-only)", func(t *testing.T) {
		store := newStore(t)
		check.NoErr(t, kernel.Initialize(t.Context(), store))
		p := runPrefix()
		sid := "sess-" + p
		want := toolMessages(p)

		// The messages handed to Set and Append are the caller's again.
		given := toolMessages(p)
		check.NoErr(t, store.Set(t.Context(), sid, given[:2]))
		check.NoErr(t, store.Append(t.Context(), sid, given[2]))
		for i := range given {
			given[i].Content = "changed after write"
			for j := range given[i].ToolCalls {
				given[i].ToolCalls[j].Name = "changed after write"
				if len(given[i].ToolCalls[j].Arguments) > 2 {
					given[i].ToolCalls[j].Arguments[2] = 'X'
				}
			}
			for j := range given[i].ToolResults {
				given[i].ToolResults[j].Content = "changed after write"
				if given[i].ToolResults[j].IsError != nil {
					*given[i].ToolResults[j].IsError = !*given[i].ToolResults[j].IsError
				}
			}
		}

		verify := func(label string) {
			t.Helper()
			got := getMessages(t, store, sid)
			check.Len(t, got, len(want), "%s: message count", label)
			for i := range min(len(got), len(want)) {
				check.True(t, jsonEqual(got[i], want[i]), "%s: message %d is %+v, want %+v", label, i, got[i], want[i])
			}
			check.Equal(t, countMessages(t, store, sid), len(want), "%s: count", label)
		}
		verify("after mutating the messages given to Set and Append")

		// Change every mutable part of what Get returns, and its order.
		got := getMessages(t, store, sid)
		for i := range got {
			got[i].Content = "changed after get"
			for j := range got[i].ToolCalls {
				got[i].ToolCalls[j].Name = "changed after get"
				if len(got[i].ToolCalls[j].Arguments) > 2 {
					got[i].ToolCalls[j].Arguments[2] = 'X'
				}
			}
			for j := range got[i].ToolResults {
				got[i].ToolResults[j].Content = "changed after get"
				if got[i].ToolResults[j].IsError != nil {
					*got[i].ToolResults[j].IsError = !*got[i].ToolResults[j].IsError
				}
			}
		}
		slices.Reverse(got)
		verify("after mutating the messages returned by Get")

		// Change every mutable part of what GetRange returns.
		slice, err := store.GetRange(t.Context(), sid, -1, 10)
		check.NoErr(t, err)
		for i := range slice.Messages {
			slice.Messages[i].Content = "changed after getRange"
			for j := range slice.Messages[i].ToolCalls {
				if len(slice.Messages[i].ToolCalls[j].Arguments) > 2 {
					slice.Messages[i].ToolCalls[j].Arguments[2] = 'X'
				}
			}
			for j := range slice.Messages[i].ToolResults {
				if slice.Messages[i].ToolResults[j].IsError != nil {
					*slice.Messages[i].ToolResults[j].IsError = !*slice.Messages[i].ToolResults[j].IsError
				}
			}
		}
		verify("after mutating the messages returned by GetRange")
	})

	t.Run("concurrent Append on one session keeps every message (Go-only)", func(t *testing.T) {
		const writers, perWriter = 8, 5
		store := newStore(t)
		check.NoErr(t, kernel.Initialize(t.Context(), store))
		p := runPrefix()
		sid := "sess-" + p

		var wg sync.WaitGroup
		for w := range writers {
			wg.Go(func() {
				for i := range perWriter {
					m := kernel.Message{Role: kernel.RoleUser, Content: fmt.Sprintf("%s-w%d-i%d", p, w, i)}
					if err := store.Append(t.Context(), sid, m); err != nil {
						t.Errorf("append from writer %d: %v", w, err)
					}
				}
			})
		}
		wg.Wait()

		got := getMessages(t, store, sid)
		check.Len(t, got, writers*perWriter, "stored messages")
		check.Equal(t, countMessages(t, store, sid), writers*perWriter, "count")
		// Every message is stored once, and one writer's messages keep their order.
		seen := map[string]int{}
		next := make([]int, writers)
		for _, m := range got {
			seen[m.Content]++
			var w, i int
			if _, err := fmt.Sscanf(m.Content, p+"-w%d-i%d", &w, &i); err != nil {
				t.Errorf("unexpected message content %q", m.Content)
				continue
			}
			check.Equal(t, i, next[w], "writer %d wrote out of order", w)
			next[w] = i + 1
		}
		var repeated []string
		for c, n := range seen {
			if n != 1 {
				repeated = append(repeated, c)
			}
		}
		check.Len(t, repeated, 0, "messages stored more than once")
	})
}
