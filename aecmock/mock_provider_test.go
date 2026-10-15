package aecmock_test

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Phong95/aec-agent-go/aecmock"
	"github.com/Phong95/aec-agent-go/components/llmhub"
	"github.com/Phong95/aec-agent-go/internal/jsonx"
	"github.com/Phong95/aec-agent-go/internal/wiretest"
	"github.com/Phong95/aec-agent-go/kernel"
)

// mockProviderFixtures lists every fixture the MockProvider tests read.
var mockProviderFixtures = []string{"mock-provider-io.json"}

// providerFixture mirrors mock-provider-io.json: the recorded output of the mock
// provider. Chat and the chunks stay raw so the comparison is on
// the exact bytes.
type providerFixture struct {
	Messages      []kernel.Message  `json:"messages"`
	Chat          json.RawMessage   `json:"chat"`
	Stream        []json.RawMessage `json:"stream"`
	CountTokens   float64           `json:"countTokens"`
	EmptyMessages []kernel.Message  `json:"emptyMessages"`
	EmptyStream   []json.RawMessage `json:"emptyStream"`
}

func loadProviderFixture(t *testing.T) providerFixture {
	t.Helper()
	var f providerFixture
	if err := jsonx.UnmarshalStrict(wiretest.Load(t, "mock-provider-io.json"), &f); err != nil {
		t.Fatalf("decode mock-provider-io.json: %v", err)
	}
	return f
}

// echo is the response function of the conformance runner.
func echo(msgs []kernel.Message) string {
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == kernel.RoleUser {
			return "Echo: " + msgs[i].Content
		}
	}
	return "Echo: empty"
}

// collect drains a stream, failing the test on any yielded error.
func collect(t *testing.T, seq func(yield func(kernel.StreamChunk, error) bool)) []kernel.StreamChunk {
	t.Helper()
	var out []kernel.StreamChunk
	for c, err := range seq {
		if err != nil {
			t.Fatalf("unexpected error in the stream: %v", err)
		}
		out = append(out, c)
	}
	return out
}

// encodeChunks encodes each chunk with the SDK encoder.
func encodeChunks(t *testing.T, chunks []kernel.StreamChunk) []string {
	t.Helper()
	out := make([]string, len(chunks))
	for i, c := range chunks {
		b, err := kernel.EncodeJSON(c)
		if err != nil {
			t.Fatalf("encode chunk %d: %v", i, err)
		}
		out[i] = string(b)
	}
	return out
}

func rawStrings(raws []json.RawMessage) []string {
	out := make([]string, len(raws))
	for i, r := range raws {
		out[i] = string(r)
	}
	return out
}

func TestMockProviderMatchesGolden(t *testing.T) {
	f := loadProviderFixture(t)
	ctx := t.Context()

	t.Run("chat", func(t *testing.T) {
		p := aecmock.NewMockProvider("test-adapter", echo)
		got, err := p.Chat(ctx, f.Messages, llmhub.ModelOptions{})
		if err != nil {
			t.Fatalf("Chat: %v", err)
		}
		b, err := kernel.EncodeJSON(got)
		if err != nil {
			t.Fatalf("encode response: %v", err)
		}
		if string(b) != string(f.Chat) {
			t.Errorf("chat bytes:\n got %s\nwant %s", b, f.Chat)
		}
	})

	t.Run("stream", func(t *testing.T) {
		p := aecmock.NewMockProvider("test-adapter", echo)
		got := encodeChunks(t, collect(t, p.Stream(ctx, f.Messages, llmhub.ModelOptions{})))
		if !slices.Equal(got, rawStrings(f.Stream)) {
			t.Errorf("stream bytes:\n got %v\nwant %v", got, rawStrings(f.Stream))
		}
	})

	t.Run("empty stream", func(t *testing.T) {
		p := aecmock.NewMockProvider("test-adapter", func([]kernel.Message) string { return "" })
		got := encodeChunks(t, collect(t, p.Stream(ctx, f.EmptyMessages, llmhub.ModelOptions{})))
		if !slices.Equal(got, rawStrings(f.EmptyStream)) {
			t.Errorf("empty stream bytes:\n got %v\nwant %v", got, rawStrings(f.EmptyStream))
		}
	})

	t.Run("nil respond is the empty string", func(t *testing.T) {
		p := aecmock.NewMockProvider("test-adapter", nil)
		got := encodeChunks(t, collect(t, p.Stream(ctx, f.EmptyMessages, llmhub.ModelOptions{})))
		if !slices.Equal(got, rawStrings(f.EmptyStream)) {
			t.Errorf("nil respond stream:\n got %v\nwant %v", got, rawStrings(f.EmptyStream))
		}
	})

	t.Run("countTokens is the ceiling of the TS count", func(t *testing.T) {
		if f.CountTokens == math.Trunc(f.CountTokens) {
			t.Fatalf("fixture shape changed: the TS count %v is an integer", f.CountTokens)
		}
		p := aecmock.NewMockProvider("test-adapter", echo)
		got, err := p.CountTokens(ctx, f.Messages)
		if err != nil {
			t.Fatalf("CountTokens: %v", err)
		}
		if want := int(math.Ceil(f.CountTokens)); got != want {
			t.Errorf("CountTokens = %d, want %d (TS %v)", got, want, f.CountTokens)
		}
	})

	t.Run("countTokens counts UTF-16 code units", func(t *testing.T) {
		// Eight emoji runes are 16 code units (4 tokens), not 8 runes (2 tokens).
		content := strings.Repeat(string(rune(0x1f600)), 8)
		p := aecmock.NewMockProvider("test-adapter", echo)
		got, err := p.CountTokens(ctx, []kernel.Message{{Role: kernel.RoleUser, Content: content}})
		if err != nil {
			t.Fatalf("CountTokens: %v", err)
		}
		if got != 4 {
			t.Errorf("CountTokens = %d, want 4", got)
		}
	})

	t.Run("identity", func(t *testing.T) {
		p := aecmock.NewMockProvider("test-adapter", echo)
		if p.Name() != "test-adapter" || p.Version() != "1.0.0" {
			t.Errorf("identity = %q %q", p.Name(), p.Version())
		}
		if err := kernel.Initialize(ctx, p); err != nil {
			t.Errorf("Initialize: %v", err)
		}
		if err := kernel.Dispose(ctx, p); err != nil {
			t.Errorf("Dispose: %v", err)
		}
	})
}

func TestMockProviderScripts(t *testing.T) {
	ctx := t.Context()
	msgs := []kernel.Message{{Role: kernel.RoleUser, Content: "Hello"}}

	t.Run("WithStream yields the chunks verbatim", func(t *testing.T) {
		text := "partial"
		want := []kernel.StreamChunk{{Type: kernel.ChunkText, Content: &text}, {Type: kernel.ChunkDone, Aborted: true}}
		p := aecmock.NewMockProvider("p", echo, aecmock.WithStream(func([]kernel.Message) []kernel.StreamChunk { return want }))
		got := encodeChunks(t, collect(t, p.Stream(ctx, msgs, llmhub.ModelOptions{})))
		if !slices.Equal(got, encodeChunks(t, want)) {
			t.Errorf("stream = %v, want %v", got, encodeChunks(t, want))
		}
	})

	t.Run("WithResponse returns the scripted response", func(t *testing.T) {
		want := kernel.ModelResponse{
			Message:    kernel.Message{Role: kernel.RoleAssistant, Content: "scripted"},
			Usage:      kernel.TokenUsage{InputTokens: 1, OutputTokens: 2},
			StopReason: kernel.StopMaxTokens,
		}
		p := aecmock.NewMockProvider("p", echo, aecmock.WithResponse(func([]kernel.Message) kernel.ModelResponse { return want }))
		got, err := p.Chat(ctx, msgs, llmhub.ModelOptions{})
		if err != nil {
			t.Fatalf("Chat: %v", err)
		}
		if got.Message.Content != "scripted" || got.StopReason != kernel.StopMaxTokens || got.Usage != want.Usage {
			t.Errorf("response = %+v, want %+v", got, want)
		}
	})

	t.Run("WithFailure is in-band for Stream and an error for Chat", func(t *testing.T) {
		failure := kernel.NewModelError("upstream failed", kernel.ModelCodeProvider, true)
		p := aecmock.NewMockProvider("p", echo, aecmock.WithFailure(failure))

		var items int
		for c, err := range p.Stream(ctx, msgs, llmhub.ModelOptions{}) {
			items++
			if err != nil {
				t.Fatalf("a provider failure must not use the error slot: %v", err)
			}
			if c.Type != kernel.ChunkError || c.Error != "upstream failed" {
				t.Errorf("chunk = %+v, want an error chunk with the failure message", c)
			}
		}
		if items != 1 {
			t.Errorf("got %d items, want exactly 1", items)
		}

		_, err := p.Chat(ctx, msgs, llmhub.ModelOptions{})
		var me *kernel.ModelError
		if !errors.As(err, &me) || me != failure {
			t.Errorf("Chat error = %v, want the scripted *ModelError", err)
		}
	})

	t.Run("WithHangAfter yields n chunks then blocks until cancelled", func(t *testing.T) {
		p := aecmock.NewMockProvider("p", echo, aecmock.WithHangAfter(2))
		cctx, cancel := context.WithCancel(ctx)
		defer cancel()

		type outcome struct {
			chunks int
			errs   []error
		}
		done := make(chan outcome, 1)
		go func() {
			var o outcome
			for _, err := range p.Stream(cctx, msgs, llmhub.ModelOptions{}) {
				if err != nil {
					o.errs = append(o.errs, err)
					continue
				}
				o.chunks++
				if o.chunks == 2 {
					cancel()
				}
			}
			done <- o
		}()
		select {
		case o := <-done:
			if o.chunks != 2 {
				t.Errorf("got %d chunks before the hang, want 2", o.chunks)
			}
			if len(o.errs) != 1 || !errors.Is(o.errs[0], context.Canceled) {
				t.Errorf("errors = %v, want exactly one context.Canceled", o.errs)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("the stream did not end after the context was cancelled")
		}
	})

	t.Run("WithHangAfter beyond the chunk count hangs after the last chunk", func(t *testing.T) {
		p := aecmock.NewMockProvider("p", func([]kernel.Message) string { return "" }, aecmock.WithHangAfter(10))
		cctx, cancel := context.WithCancel(ctx)
		defer cancel()
		var chunks int
		var gotErr error
		for c, err := range p.Stream(cctx, msgs, llmhub.ModelOptions{}) {
			if err != nil {
				gotErr = err
				continue
			}
			chunks++
			if c.Type == kernel.ChunkDone {
				cancel()
			}
		}
		if chunks != 2 || !errors.Is(gotErr, context.Canceled) {
			t.Errorf("chunks = %d, error = %v; want 2 chunks and context.Canceled", chunks, gotErr)
		}
	})
}

func TestMockProviderCancelled(t *testing.T) {
	p := aecmock.NewMockProvider("p", echo)
	msgs := []kernel.Message{{Role: kernel.RoleUser, Content: "Hello"}}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	t.Run("stream yields ctx.Err once and ends", func(t *testing.T) {
		var items int
		for c, err := range p.Stream(ctx, msgs, llmhub.ModelOptions{}) {
			items++
			if !errors.Is(err, context.Canceled) {
				t.Errorf("error = %v, want context.Canceled", err)
			}
			if c.Type != "" || c.Content != nil {
				t.Errorf("chunk = %+v, want the zero chunk", c)
			}
		}
		if items != 1 {
			t.Errorf("got %d items, want exactly 1", items)
		}
	})

	t.Run("stream cancelled mid-stream", func(t *testing.T) {
		cctx, ccancel := context.WithCancel(t.Context())
		defer ccancel()
		var chunks int
		var errs []error
		for _, err := range p.Stream(cctx, msgs, llmhub.ModelOptions{}) {
			if err != nil {
				errs = append(errs, err)
				continue
			}
			chunks++
			ccancel()
		}
		if chunks != 1 || len(errs) != 1 || !errors.Is(errs[0], context.Canceled) {
			t.Errorf("chunks = %d, errors = %v; want 1 chunk then one context.Canceled", chunks, errs)
		}
	})

	t.Run("chat returns the bare ctx.Err", func(t *testing.T) {
		_, err := p.Chat(ctx, msgs, llmhub.ModelOptions{})
		if err != context.Canceled { //nolint:errorlint // the contract is the bare, unwrapped ctx.Err()
			t.Errorf("Chat error = %v, want context.Canceled itself", err)
		}
	})

	t.Run("countTokens returns the bare ctx.Err", func(t *testing.T) {
		_, err := p.CountTokens(ctx, msgs)
		if err != context.Canceled { //nolint:errorlint // the contract is the bare, unwrapped ctx.Err()
			t.Errorf("CountTokens error = %v, want context.Canceled itself", err)
		}
	})
}

func TestMockProviderBreakEarly(t *testing.T) {
	p := aecmock.NewMockProvider("p", echo)
	msgs := []kernel.Message{{Role: kernel.RoleUser, Content: "one two three four"}}

	// Drive the iterator by hand so a yield counter can prove it is never called
	// again after it returned false (a range loop would panic instead).
	yields := 0
	p.Stream(t.Context(), msgs, llmhub.ModelOptions{})(func(kernel.StreamChunk, error) bool {
		yields++
		return false
	})
	if yields != 1 {
		t.Errorf("yield called %d times, want 1", yields)
	}

	// The same through a range loop, which the runtime polices.
	n := 0
	for range p.Stream(t.Context(), msgs, llmhub.ModelOptions{}) {
		n++
		break
	}
	if n != 1 {
		t.Errorf("range body ran %d times, want 1", n)
	}

	// A hung stream whose consumer stops after the first chunk returns at once.
	hung := aecmock.NewMockProvider("p", echo, aecmock.WithHangAfter(1))
	done := make(chan struct{})
	go func() {
		defer close(done)
		for range hung.Stream(t.Context(), msgs, llmhub.ModelOptions{}) {
			break
		}
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("breaking out of a hung stream blocked")
	}
}

func TestMockProviderCallsConcurrent(t *testing.T) {
	p := aecmock.NewMockProvider("p", echo)
	ctx := t.Context()
	maxTokens := 10

	const workers = 8
	var wg sync.WaitGroup
	for i := range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			msgs := []kernel.Message{{Role: kernel.RoleUser, Content: strings.Repeat("x", i)}}
			opts := llmhub.ModelOptions{Model: "m", MaxTokens: &maxTokens, StopSequences: []string{"END"}}
			if i%2 == 0 {
				_, _ = p.Chat(ctx, msgs, opts)
			} else {
				for _, err := range p.Stream(ctx, msgs, opts) {
					if err != nil {
						t.Errorf("stream error: %v", err)
					}
				}
			}
			_ = p.Calls()
		}()
	}
	wg.Wait()

	calls := p.Calls()
	if len(calls) != workers {
		t.Fatalf("recorded %d calls, want %d", len(calls), workers)
	}

	// The result is a copy: changing it does not change what is recorded.
	calls[0].Messages[0].Content = "changed"
	calls[0].Options.StopSequences[0] = "changed"
	again := p.Calls()
	if again[0].Messages[0].Content == "changed" || again[0].Options.StopSequences[0] == "changed" {
		t.Error("Calls returned a slice that aliases the recorded state")
	}
}

// TestMockProviderFixturesCovered fails when a mock-provider fixture has no entry
// above and when an entry has no file.
func TestMockProviderFixturesCovered(t *testing.T) {
	entries, err := os.ReadDir(wiretest.Dir(t))
	if err != nil {
		t.Fatalf("read fixture dir: %v", err)
	}
	present := map[string]bool{}
	for _, e := range entries {
		present[e.Name()] = true
		if strings.HasPrefix(e.Name(), "mock-provider") && !slices.Contains(mockProviderFixtures, e.Name()) {
			t.Errorf("fixture %s has no entry in mockProviderFixtures", e.Name())
		}
	}
	for _, f := range mockProviderFixtures {
		if !present[f] {
			t.Errorf("mockProviderFixtures entry %s has no fixture file", f)
		}
	}
}
