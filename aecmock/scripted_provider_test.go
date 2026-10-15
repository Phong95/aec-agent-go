package aecmock_test

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/Phong95/aec-agent-go/aecmock"
	"github.com/Phong95/aec-agent-go/components/llmhub"
	"github.com/Phong95/aec-agent-go/kernel"
)

// The scripted provider is a real llmhub.Provider, so it can be registered on an
// AgentCore and on any llmhub.LLMHub.
var _ llmhub.Provider = (*aecmock.ScriptedProvider)(nil)

func textOf(chunks []kernel.StreamChunk) string {
	var s string
	for _, c := range chunks {
		if c.Type == kernel.ChunkText && c.Content != nil {
			s += *c.Content
		}
	}
	return s
}

// The turns are served in order, one per call, whether the call is a Chat or a
// Stream, and the provider reports its name and a version.
func TestScriptedProvider_ServesTurnsInOrder(t *testing.T) {
	p := aecmock.NewScriptedProvider("scripted", []aecmock.ScriptedTurn{
		{Content: "one"},
		{Content: "two words"},
		{ToolCalls: []kernel.ToolCall{call("c1", "t", `{"a":1}`)}},
	})
	ctx := t.Context()
	if p.Name() != "scripted" || p.Version() == "" {
		t.Fatalf("Name = %q, Version = %q", p.Name(), p.Version())
	}
	if err := p.Initialize(ctx); err != nil {
		t.Fatal(err)
	}

	r1, err := p.Chat(ctx, []kernel.Message{{Role: kernel.RoleUser, Content: "hi"}}, llmhub.ModelOptions{Model: "m"})
	if err != nil || r1.Message.Content != "one" || r1.StopReason != kernel.StopEndTurn {
		t.Fatalf("Chat = %+v, %v, want the first turn", r1, err)
	}
	chunks := collect(t, p.Stream(ctx, nil, llmhub.ModelOptions{}))
	if got := textOf(chunks); got != "two words " || chunks[len(chunks)-1].Type != kernel.ChunkDone {
		t.Fatalf("Stream text = %q, last chunk %+v, want the second turn and a done chunk", got, chunks[len(chunks)-1])
	}
	r3, err := p.Chat(ctx, nil, llmhub.ModelOptions{})
	if err != nil || r3.StopReason != kernel.StopToolUse || len(r3.Message.ToolCalls) != 1 {
		t.Fatalf("Chat = %+v, %v, want the tool call turn", r3, err)
	}

	calls := p.Calls()
	if len(calls) != 3 || calls[0].Options.Model != "m" || calls[0].Messages[0].Content != "hi" {
		t.Fatalf("Calls = %+v, want the three calls recorded", calls)
	}
	// The result is a copy.
	calls[0].Messages[0].Content = "changed"
	if p.Calls()[0].Messages[0].Content != "hi" {
		t.Fatal("Calls returned the recorded slice, not a copy")
	}
	if n, err := p.CountTokens(ctx, []kernel.Message{{Content: "12345678"}}); err != nil || n != 2 {
		t.Fatalf("CountTokens = %d, %v, want 2", n, err)
	}
	if err := p.Dispose(ctx); err != nil {
		t.Fatal(err)
	}
}

// A call past the last turn fails: Chat with an error, Stream with one terminal
// error chunk (the in-band failure contract of llmhub.Provider).
func TestScriptedProvider_Exhaustion(t *testing.T) {
	p := aecmock.NewScriptedProvider("scripted", []aecmock.ScriptedTurn{{Content: "only"}})
	ctx := t.Context()
	if _, err := p.Chat(ctx, nil, llmhub.ModelOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Chat(ctx, nil, llmhub.ModelOptions{}); err == nil {
		t.Fatal("Chat past the last turn succeeded")
	}
	chunks := collect(t, p.Stream(ctx, nil, llmhub.ModelOptions{}))
	if len(chunks) != 1 || chunks[0].Type != kernel.ChunkError || chunks[0].Error == "" {
		t.Fatalf("Stream past the last turn = %+v, want one error chunk", chunks)
	}
}

// A failed turn fails Chat with its error and ends Stream with an error chunk
// carrying its text.
func TestScriptedProvider_FailedTurn(t *testing.T) {
	boom := errors.New("provider down")
	p := aecmock.NewScriptedProvider("scripted", []aecmock.ScriptedTurn{{Err: boom}, {Err: boom}})
	ctx := t.Context()
	if _, err := p.Chat(ctx, nil, llmhub.ModelOptions{}); !errors.Is(err, boom) {
		t.Fatalf("Chat error = %v, want the turn's error", err)
	}
	chunks := collect(t, p.Stream(ctx, nil, llmhub.ModelOptions{}))
	if len(chunks) != 1 || chunks[0].Type != kernel.ChunkError || chunks[0].Error != "provider down" {
		t.Fatalf("Stream = %+v, want one error chunk with the text", chunks)
	}
}

// The hook runs before the call is served, with the zero-based index, for Chat and
// for Stream alike; the call is recorded only once the hook has returned.
func TestScriptedProvider_HookRunsBeforeTheCallIsServed(t *testing.T) {
	var order []string
	var p *aecmock.ScriptedProvider
	p = aecmock.NewScriptedProvider("scripted", []aecmock.ScriptedTurn{{Content: "a"}, {Content: "b"}},
		aecmock.WithCallHook(func(_ context.Context, call int) {
			order = append(order, "hook")
			if got := len(p.Calls()); got != call {
				t.Errorf("call %d: %d calls were recorded before its hook returned, want %d", call, got, call)
			}
			if call != len(order)-1 {
				t.Errorf("hook ran for call %d as the %d-th hook", call, len(order))
			}
		}))
	ctx := t.Context()
	if _, err := p.Chat(ctx, nil, llmhub.ModelOptions{}); err != nil {
		t.Fatal(err)
	}
	collect(t, p.Stream(ctx, nil, llmhub.ModelOptions{}))
	if len(order) != 2 || len(p.Calls()) != 2 {
		t.Fatalf("hooks = %d, calls = %d, want 2 and 2", len(order), len(p.Calls()))
	}
}

// A Stream sequence runs its hook when it is iterated, not when Stream is called.
func TestScriptedProvider_StreamHookRunsAtIteration(t *testing.T) {
	hooks := 0
	p := aecmock.NewScriptedProvider("scripted", []aecmock.ScriptedTurn{{Content: "a"}},
		aecmock.WithCallHook(func(context.Context, int) { hooks++ }))
	seq := p.Stream(t.Context(), nil, llmhub.ModelOptions{})
	if hooks != 0 {
		t.Fatalf("%d hooks ran before the sequence was iterated, want none", hooks)
	}
	collect(t, seq)
	if hooks != 1 {
		t.Fatalf("%d hooks after the iteration, want 1", hooks)
	}
}

// A hook that blocks until the ctx ends gives a provider that stalls: the call
// fails with the ctx error at the instant of the stop and is not recorded.
func TestScriptedProvider_HookBlocksUntilCtxEnds(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		p := aecmock.NewScriptedProvider("scripted", []aecmock.ScriptedTurn{{Content: "never"}},
			aecmock.WithCallHook(func(ctx context.Context, _ int) { <-ctx.Done() }))
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		time.AfterFunc(40*time.Millisecond, cancel)

		start := time.Now()
		_, err := p.Chat(ctx, nil, llmhub.ModelOptions{})
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Chat error = %v, want the ctx error", err)
		}
		if got := time.Since(start); got != 40*time.Millisecond {
			t.Fatalf("Chat returned after %v, want the instant of the stop (40ms)", got)
		}
		if n := len(p.Calls()); n != 0 {
			t.Fatalf("%d calls recorded, want none: the call was never served", n)
		}
	})
}

// Breaking out of the loop stops the sequence at once, and nothing is left running.
func TestScriptedProvider_BreakEarly(t *testing.T) {
	p := aecmock.NewScriptedProvider("scripted", []aecmock.ScriptedTurn{{Content: "a b c d"}})
	seen := 0
	for range p.Stream(t.Context(), nil, llmhub.ModelOptions{}) {
		seen++
		if seen == 2 {
			break
		}
	}
	if seen != 2 {
		t.Fatalf("saw %d chunks before the break, want 2", seen)
	}
	if n := len(p.Calls()); n != 1 {
		t.Fatalf("%d calls recorded, want 1", n)
	}
}

// A ctx that is done yields its error once, in the error slot, and the sequence ends;
// Chat returns the bare ctx error.
func TestScriptedProvider_CtxCancel(t *testing.T) {
	p := aecmock.NewScriptedProvider("scripted", []aecmock.ScriptedTurn{{Content: "a b c d"}, {Content: "never"}})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	yields, got := 0, error(nil)
	chunks := 0
	for c, err := range p.Stream(ctx, nil, llmhub.ModelOptions{}) {
		yields++
		if err != nil {
			got = err
			continue
		}
		if c.Type == kernel.ChunkText {
			chunks++
			if chunks == 2 {
				cancel()
			}
		}
	}
	if !errors.Is(got, context.Canceled) || chunks != 2 || yields != 3 {
		t.Fatalf("yields = %d, text chunks = %d, error = %v, want 2 chunks and one ctx error", yields, chunks, got)
	}
	if _, err := p.Chat(ctx, nil, llmhub.ModelOptions{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("Chat with a done ctx = %v, want the ctx error", err)
	}
	if _, err := p.CountTokens(ctx, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("CountTokens with a done ctx = %v, want the ctx error", err)
	}
}

// WithHangAfterChunks yields that many chunks and then blocks until the ctx is done.
func TestScriptedProvider_HangAfterChunks(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		hello, lo := "Hel", "lo"
		p := aecmock.NewScriptedProvider("scripted", []aecmock.ScriptedTurn{{Chunks: []kernel.StreamChunk{
			{Type: kernel.ChunkText, Content: &hello}, {Type: kernel.ChunkText, Content: &lo}, {Type: kernel.ChunkDone},
		}}}, aecmock.WithHangAfterChunks(2))
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		time.AfterFunc(25*time.Millisecond, cancel)

		start := time.Now()
		var got []kernel.StreamChunk
		var last error
		for c, err := range p.Stream(ctx, nil, llmhub.ModelOptions{}) {
			if err != nil {
				last = err
				continue
			}
			got = append(got, c)
		}
		if textOf(got) != "Hello" || len(got) != 2 || !errors.Is(last, context.Canceled) {
			t.Fatalf("chunks = %+v, error = %v, want the two text chunks and the ctx error", got, last)
		}
		if elapsed := time.Since(start); elapsed != 25*time.Millisecond {
			t.Fatalf("the stream ended after %v, want the instant of the stop (25ms)", elapsed)
		}
	})
}

// WithScriptedOptions reaches the turn logic: the summary call is answered with the
// text and keeps the script.
func TestScriptedProvider_SummaryReply(t *testing.T) {
	p := aecmock.NewScriptedProvider("scripted", []aecmock.ScriptedTurn{{Content: "main"}},
		aecmock.WithScriptedOptions(aecmock.WithSummaryReply("SUMMARY-TEXT")))
	ctx := t.Context()
	sum, err := p.Chat(ctx, nil, llmhub.ModelOptions{SystemPrompt: "summarize"})
	if err != nil || sum.Message.Content != "SUMMARY-TEXT" {
		t.Fatalf("summary Chat = %+v, %v", sum, err)
	}
	main, err := p.Chat(ctx, nil, llmhub.ModelOptions{})
	if err != nil || main.Message.Content != "main" {
		t.Fatalf("main Chat = %+v, %v, want the script untouched by the summary call", main, err)
	}
}

// Concurrent callers are safe: every call gets its own index and its own turn.
func TestScriptedProvider_ConcurrentCalls(t *testing.T) {
	const n = 16
	turns := make([]aecmock.ScriptedTurn, n)
	for i := range turns {
		turns[i] = aecmock.ScriptedTurn{Content: "x"}
	}
	var mu sync.Mutex
	var indexes []int
	p := aecmock.NewScriptedProvider("scripted", turns, aecmock.WithCallHook(func(_ context.Context, call int) {
		mu.Lock()
		defer mu.Unlock()
		indexes = append(indexes, call)
	}))
	var wg sync.WaitGroup
	for range n {
		wg.Go(func() {
			if _, err := p.Chat(t.Context(), nil, llmhub.ModelOptions{}); err != nil {
				t.Errorf("Chat: %v", err)
			}
		})
	}
	wg.Wait()
	slices.Sort(indexes)
	for i, got := range indexes {
		if got != i {
			t.Fatalf("hook indexes = %v, want 0..%d each once", indexes, n-1)
		}
	}
	if len(p.Calls()) != n {
		t.Fatalf("%d calls recorded, want %d", len(p.Calls()), n)
	}
}
