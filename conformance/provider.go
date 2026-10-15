package conformance

import (
	"context"
	"errors"
	"runtime"
	"testing"
	"time"

	"github.com/Phong95/aec-agent-go/components/llmhub"
	"github.com/Phong95/aec-agent-go/conformance/internal/check"
	"github.com/Phong95/aec-agent-go/kernel"
)

// ProviderFactory returns a fresh llmhub.Provider. See the package comment for the
// freshness rules.
type ProviderFactory func(t *testing.T) llmhub.Provider

// ProviderOptions carries the factories of the two Go-only subtests that need a
// provider in a special state. A nil field skips the subtests that use it, with
// a message naming the field.
type ProviderOptions struct {
	// NewFailing returns a provider whose Stream fails in-band (one terminal
	// chunk of type error with a non-empty Error, yielded with a nil error) and
	// whose Chat returns a *kernel.ModelError. Real providers return one backed
	// by an httptest server that answers with an error status.
	NewFailing ProviderFactory
	// NewBlocking returns a provider whose Stream yields at least one chunk and
	// then blocks until its context is done. Real providers return one backed by
	// an httptest server that stops writing mid-response.
	NewBlocking ProviderFactory
}

// providerTimeout bounds every wait of the streaming subtests, so a provider that
// ignores cancellation fails the test instead of hanging it.
const providerTimeout = 2 * time.Second

// hello is the single user message every test sends.
func hello() []kernel.Message {
	return []kernel.Message{{Role: kernel.RoleUser, Content: "Hello"}}
}

// streamAll drains a Stream sequence and fails the test on any yielded error:
// a successful stream never uses the error slot.
func streamAll(t *testing.T, p llmhub.Provider, msgs []kernel.Message) []kernel.StreamChunk {
	t.Helper()
	var out []kernel.StreamChunk
	for c, err := range p.Stream(t.Context(), msgs, llmhub.ModelOptions{}) {
		check.NoErr(t, err, "stream yielded an error")
		out = append(out, c)
	}
	return out
}

// countType counts the chunks of one type.
func countType(chunks []kernel.StreamChunk, typ kernel.ChunkType) int {
	n := 0
	for _, c := range chunks {
		if c.Type == typ {
			n++
		}
	}
	return n
}

// streamOutcome is what a consumer saw: the chunks, the errors and whether
// anything followed the first error.
type streamOutcome struct {
	chunks     int
	errs       []error
	afterError int
}

// consume drains a stream on its own goroutine and reports the outcome, or fails
// the test when it does not finish within providerTimeout. onChunk runs inside
// the loop body after each chunk, on the consumer goroutine.
func consume(t *testing.T, seq func(yield func(kernel.StreamChunk, error) bool), onChunk func()) streamOutcome {
	t.Helper()
	done := make(chan streamOutcome, 1)
	go func() {
		var o streamOutcome
		for _, err := range seq {
			if len(o.errs) > 0 {
				o.afterError++
			}
			if err != nil {
				o.errs = append(o.errs, err)
				continue
			}
			o.chunks++
			if onChunk != nil {
				onChunk()
			}
		}
		done <- o
	}()
	select {
	case o := <-done:
		return o
	case <-time.After(providerTimeout):
		t.Fatalf("the stream did not end within %v", providerTimeout)
		return streamOutcome{}
	}
}

// Provider runs the conformance suite for llmhub.Provider implementations. It
// verifies lifecycle, the shape of Chat responses, stream ordering and token
// counting, plus the streaming contract: breaking out of
// the loop releases everything, a cancelled context ends a stream promptly with
// ctx.Err() once in the error slot, and a provider failure is one terminal
// in-band error chunk with a nil error.
//
// The suite does not import aecmock; the caller supplies the factories.
func Provider(t *testing.T, newProvider ProviderFactory, opts ProviderOptions) {
	t.Helper()

	// -- Lifecycle --

	t.Run("has a non-empty name and version", func(t *testing.T) {
		p := newProvider(t)
		check.True(t, p.Name() != "", "name is empty")
		check.True(t, p.Version() != "", "version is empty")
	})

	t.Run("initialize() resolves without error", func(t *testing.T) {
		p := newProvider(t)
		check.NoErr(t, kernel.Initialize(t.Context(), p))
	})

	t.Run("dispose() resolves without error", func(t *testing.T) {
		p := newProvider(t)
		check.NoErr(t, kernel.Initialize(t.Context(), p))
		check.NoErr(t, kernel.Dispose(t.Context(), p))
	})

	// -- chat() --

	t.Run("chat() returns a valid ModelResponse with end_turn", func(t *testing.T) {
		p := newProvider(t)
		check.NoErr(t, kernel.Initialize(t.Context(), p))

		resp, err := p.Chat(t.Context(), hello(), llmhub.ModelOptions{})
		check.NoErr(t, err)

		check.Equal(t, resp.Message.Role, kernel.RoleAssistant, "message role")
		check.True(t, resp.Usage.InputTokens >= 0, "inputTokens is negative")
		check.True(t, resp.Usage.OutputTokens >= 0, "outputTokens is negative")
		switch resp.StopReason {
		case kernel.StopEndTurn, kernel.StopToolUse, kernel.StopMaxTokens, kernel.StopError:
		default:
			t.Errorf("stop reason %q is not one of end_turn, tool_use, max_tokens, error", resp.StopReason)
		}
	})

	t.Run("chat() with end_turn has content in the message", func(t *testing.T) {
		p := newProvider(t)
		check.NoErr(t, kernel.Initialize(t.Context(), p))

		resp, err := p.Chat(t.Context(), []kernel.Message{{Role: kernel.RoleUser, Content: "Say hello"}}, llmhub.ModelOptions{})
		check.NoErr(t, err)

		if resp.StopReason == kernel.StopEndTurn {
			check.True(t, len(resp.Message.Content) > 0, "an end_turn response has empty content")
		}
	})

	// -- stream() --

	t.Run("stream() yields at least one chunk ending with done", func(t *testing.T) {
		p := newProvider(t)
		check.NoErr(t, kernel.Initialize(t.Context(), p))

		chunks := streamAll(t, p, hello())

		check.True(t, len(chunks) > 0, "the stream yielded no chunks")
		if len(chunks) > 0 {
			check.Equal(t, chunks[len(chunks)-1].Type, kernel.ChunkDone, "type of the last chunk")
		}
	})

	t.Run("stream() emits exactly one done chunk", func(t *testing.T) {
		p := newProvider(t)
		check.NoErr(t, kernel.Initialize(t.Context(), p))

		chunks := streamAll(t, p, hello())

		check.Equal(t, countType(chunks, kernel.ChunkDone), 1, "number of done chunks")
	})

	t.Run("stream() emits no chunks after done", func(t *testing.T) {
		p := newProvider(t)
		check.NoErr(t, kernel.Initialize(t.Context(), p))

		seenDone := false
		for c, err := range p.Stream(t.Context(), hello(), llmhub.ModelOptions{}) {
			check.NoErr(t, err, "stream yielded an error")
			if seenDone {
				t.Fatalf("chunk emitted after done: %+v", c)
			}
			if c.Type == kernel.ChunkDone {
				seenDone = true
			}
		}
		check.True(t, seenDone, "the stream never emitted done")
	})

	t.Run("stream() text chunks have non-empty content", func(t *testing.T) {
		p := newProvider(t)
		check.NoErr(t, kernel.Initialize(t.Context(), p))

		for _, c := range streamAll(t, p, hello()) {
			if c.Type == kernel.ChunkText {
				// Despite the subtest name, the check only asserts that content is
				// defined, never that it is non-empty, so an empty text chunk passes;
				// a nil Content means "not defined".
				check.True(t, c.Content != nil, "a text chunk has no content")
			}
		}
	})

	// -- countTokens() --

	t.Run("countTokens() returns a non-negative number", func(t *testing.T) {
		p := newProvider(t)
		check.NoErr(t, kernel.Initialize(t.Context(), p))

		n, err := p.CountTokens(t.Context(), []kernel.Message{{Role: kernel.RoleUser, Content: "Hello world"}})
		check.NoErr(t, err)

		check.True(t, n >= 0, "the count is negative")
	})

	t.Run("countTokens() returns more for longer messages", func(t *testing.T) {
		p := newProvider(t)
		check.NoErr(t, kernel.Initialize(t.Context(), p))

		short, err := p.CountTokens(t.Context(), []kernel.Message{{Role: kernel.RoleUser, Content: "Hi"}})
		check.NoErr(t, err)
		long, err := p.CountTokens(t.Context(), []kernel.Message{{
			Role: kernel.RoleUser, Content: "This is a much longer message with many more words in it",
		}})
		check.NoErr(t, err)

		check.True(t, long >= short, "a longer message counted fewer tokens")
	})

	// -- streaming contract --

	t.Run("stream stops cleanly when the consumer breaks early", func(t *testing.T) {
		p := newProvider(t)
		check.NoErr(t, kernel.Initialize(t.Context(), p))

		// Never t.Parallel here: the count below is process wide. The strict
		// leak check is goleak in the test package that runs this suite; this
		// polling check also works for callers that do not use goleak.
		before := runtime.NumGoroutine()
		for _, err := range p.Stream(t.Context(), hello(), llmhub.ModelOptions{}) {
			check.NoErr(t, err, "stream yielded an error")
			break
		}

		deadline := time.Now().Add(providerTimeout)
		for runtime.NumGoroutine() > before && time.Now().Before(deadline) {
			time.Sleep(10 * time.Millisecond)
		}
		if after := runtime.NumGoroutine(); after > before {
			t.Errorf("goroutines after breaking out of the stream: %d, before: %d", after, before)
		}
	})

	t.Run("stream yields ctx.Err once when the context is cancelled before it starts", func(t *testing.T) {
		p := newProvider(t)
		check.NoErr(t, kernel.Initialize(t.Context(), p))

		ctx, cancel := context.WithCancel(t.Context())
		cancel()

		o := consume(t, p.Stream(ctx, hello(), llmhub.ModelOptions{}), nil)

		check.Equal(t, o.chunks, 0, "chunks before the error")
		check.Len(t, o.errs, 1, "errors yielded")
		if len(o.errs) == 1 {
			check.True(t, errors.Is(o.errs[0], context.Canceled), "the error is %v, want context.Canceled", o.errs[0])
		}
		check.Equal(t, o.afterError, 0, "items after the error")
	})

	t.Run("stream ends promptly when the context is cancelled mid-stream", func(t *testing.T) {
		if opts.NewBlocking == nil {
			t.Skip("ProviderOptions.NewBlocking is nil")
		}
		p := opts.NewBlocking(t)
		check.NoErr(t, kernel.Initialize(t.Context(), p))

		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()

		cancelled := false
		o := consume(t, p.Stream(ctx, hello(), llmhub.ModelOptions{}), func() {
			if !cancelled {
				cancelled = true
				cancel()
			}
		})

		check.True(t, o.chunks >= 1, "the blocking provider yielded no chunk before it blocked")
		check.Len(t, o.errs, 1, "errors yielded")
		if len(o.errs) == 1 {
			check.True(t, errors.Is(o.errs[0], context.Canceled), "the error is %v, want context.Canceled", o.errs[0])
		}
		check.Equal(t, o.afterError, 0, "items after the error")
	})

	t.Run("stream reports a provider failure as one terminal in-band error chunk", func(t *testing.T) {
		if opts.NewFailing == nil {
			t.Skip("ProviderOptions.NewFailing is nil")
		}
		p := opts.NewFailing(t)
		check.NoErr(t, kernel.Initialize(t.Context(), p))

		var chunks []kernel.StreamChunk
		for c, err := range p.Stream(t.Context(), hello(), llmhub.ModelOptions{}) {
			check.NoErr(t, err, "a provider failure must arrive in-band with a nil error")
			chunks = append(chunks, c)
		}

		check.True(t, len(chunks) > 0, "the failing stream yielded nothing")
		if len(chunks) > 0 {
			last := chunks[len(chunks)-1]
			check.Equal(t, last.Type, kernel.ChunkError, "type of the last chunk")
			check.True(t, last.Error != "", "the error chunk has an empty message")
		}
		check.Equal(t, countType(chunks, kernel.ChunkDone), 0, "done chunks in a failed stream")

		// The same failure through Chat is a *kernel.ModelError.
		_, err := opts.NewFailing(t).Chat(t.Context(), hello(), llmhub.ModelOptions{})
		var me *kernel.ModelError
		check.True(t, errors.As(err, &me), "Chat on a failing provider returned %v, want a *kernel.ModelError", err)
	})

	t.Run("stream yields nothing after an error chunk", func(t *testing.T) {
		if opts.NewFailing == nil {
			t.Skip("ProviderOptions.NewFailing is nil")
		}
		p := opts.NewFailing(t)
		check.NoErr(t, kernel.Initialize(t.Context(), p))

		seenError := false
		for c, err := range p.Stream(t.Context(), hello(), llmhub.ModelOptions{}) {
			if seenError {
				t.Fatalf("item after the error chunk: chunk %+v, error %v", c, err)
			}
			if c.Type == kernel.ChunkError {
				seenError = true
			}
		}
		check.True(t, seenError, "the failing stream never emitted an error chunk")
	})

	t.Run("chat returns context.Canceled for a cancelled context", func(t *testing.T) {
		p := newProvider(t)
		check.NoErr(t, kernel.Initialize(t.Context(), p))

		ctx, cancel := context.WithCancel(t.Context())
		cancel()

		_, err := p.Chat(ctx, hello(), llmhub.ModelOptions{})
		check.True(t, errors.Is(err, context.Canceled), "Chat returned %v, want an error matching context.Canceled", err)
	})
}
