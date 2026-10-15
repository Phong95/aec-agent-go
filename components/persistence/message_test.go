package persistence_test

import (
	"testing"

	"github.com/Phong95/aec-agent-go/components/persistence"
	"github.com/Phong95/aec-agent-go/kernel"
)

func sampleMessage(t *testing.T) kernel.Message {
	t.Helper()
	args, err := kernel.NewArguments([]byte(`{"b":1,"a":2}`))
	if err != nil {
		t.Fatal(err)
	}
	isErr := true
	return kernel.Message{
		Role:        kernel.RoleAssistant,
		Content:     "c",
		ToolCalls:   []kernel.ToolCall{{ID: "call_1", Name: "read_file", Arguments: args}},
		ToolResults: []kernel.ToolResult{{ToolCallID: "call_1", Content: "r", IsError: &isErr}},
	}
}

// A caller and the store never share memory: mutating a returned message (its
// argument bytes, its IsError pointer, the returned slice) or a message and a
// slice after they were handed to Append or Set changes nothing in the store.
func TestMemoryMessageStoreCopies(t *testing.T) {
	ctx := t.Context()
	store := persistence.NewMemoryMessageStore()

	appended := sampleMessage(t)
	if err := store.Append(ctx, "s", appended); err != nil {
		t.Fatal(err)
	}
	appended.ToolCalls[0].Arguments[2] = 'X'
	*appended.ToolResults[0].IsError = false
	appended.Content = "mutated"

	got, err := store.Get(ctx, "s")
	if err != nil {
		t.Fatal(err)
	}
	if string(got[0].ToolCalls[0].Arguments) != `{"b":1,"a":2}` || !*got[0].ToolResults[0].IsError || got[0].Content != "c" {
		t.Fatalf("Append aliased the message of the caller: %#v", got[0])
	}

	got[0].ToolCalls[0].Arguments[2] = 'Y'
	*got[0].ToolResults[0].IsError = false
	got[0].ToolCalls = nil
	_ = append(got[:0], kernel.Message{Role: kernel.RoleUser})

	again, err := store.Get(ctx, "s")
	if err != nil {
		t.Fatal(err)
	}
	if len(again) != 1 || string(again[0].ToolCalls[0].Arguments) != `{"b":1,"a":2}` || !*again[0].ToolResults[0].IsError {
		t.Fatalf("Get handed out live memory: %#v", again)
	}

	rng, err := store.GetRange(ctx, "s", -1, 5)
	if err != nil {
		t.Fatal(err)
	}
	rng.Messages[0].ToolCalls[0].Arguments[2] = 'Z'
	rng.Messages = nil
	again, _ = store.Get(ctx, "s")
	if string(again[0].ToolCalls[0].Arguments) != `{"b":1,"a":2}` {
		t.Fatalf("GetRange handed out live memory: %s", again[0].ToolCalls[0].Arguments)
	}

	set := []kernel.Message{sampleMessage(t), sampleMessage(t)}
	if err := store.Set(ctx, "t", set); err != nil {
		t.Fatal(err)
	}
	set[0].ToolCalls[0].Arguments[2] = 'Q'
	set[1] = kernel.Message{Role: kernel.RoleUser, Content: "replaced"}
	stored, _ := store.Get(ctx, "t")
	if len(stored) != 2 || string(stored[0].ToolCalls[0].Arguments) != `{"b":1,"a":2}` || stored[1].Content != "c" {
		t.Fatalf("Set aliased the slice of the caller: %#v", stored)
	}
}

// GetRange follows the documented arithmetic at every boundary: before -1, 0, equal to the
// total and above it, with limits of 0, 1, above the total and negative.
func TestGetRangeBoundaries(t *testing.T) {
	ctx := t.Context()
	store := persistence.NewMemoryMessageStore()
	const total = 5
	for i := range total {
		if err := store.Append(ctx, "s", kernel.Message{Role: kernel.RoleUser, Content: string(rune('a' + i))}); err != nil {
			t.Fatal(err)
		}
	}
	// want is the expected arithmetic, spelled out independently of the store.
	want := func(before, limit int) string {
		end := before
		if before < 0 || before > total {
			end = total
		}
		start := max(0, end-limit)
		out := ""
		for i := start; i < end; i++ {
			out += string(rune('a' + i))
		}
		return out
	}
	for _, before := range []int{-1, 0, 3, total, total + 1, 100} {
		for _, limit := range []int{0, 1, 2, total, total + 3, -1, -7} {
			slice, err := store.GetRange(ctx, "s", before, limit)
			if err != nil {
				t.Fatal(err)
			}
			got := ""
			for _, m := range slice.Messages {
				got += m.Content
			}
			if got != want(before, limit) || slice.Total != total {
				t.Errorf("GetRange(before=%d, limit=%d) = %q total %d, want %q total %d", before, limit, got, slice.Total, want(before, limit), total)
			}
			if slice.Messages == nil {
				t.Errorf("GetRange(before=%d, limit=%d) returned a nil slice, want a non-nil one", before, limit)
			}
		}
	}
	unknown, err := store.GetRange(ctx, "nope", -1, 3)
	if err != nil || unknown.Total != 0 || unknown.Messages == nil || len(unknown.Messages) != 0 {
		t.Fatalf("unknown session: %#v %v", unknown, err)
	}
}

// Append is safe under concurrent use and keeps every message (run with -race).
func TestMemoryMessageStoreConcurrentAppend(t *testing.T) {
	ctx := t.Context()
	store := persistence.NewMemoryMessageStore()
	const workers, each = 8, 50
	done := make(chan struct{})
	for range workers {
		go func() {
			defer func() { done <- struct{}{} }()
			for range each {
				if err := store.Append(ctx, "s", kernel.Message{Role: kernel.RoleUser, Content: "x"}); err != nil {
					t.Error(err)
					return
				}
				if _, err := store.Get(ctx, "s"); err != nil {
					t.Error(err)
					return
				}
			}
		}()
	}
	for range workers {
		<-done
	}
	if n, _ := store.Count(ctx, "s"); n != workers*each {
		t.Fatalf("Count = %d, want %d", n, workers*each)
	}
}
