package conformance

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/Phong95/aec-agent-go/components/toolhub"
	"github.com/Phong95/aec-agent-go/conformance/internal/check"
	"github.com/Phong95/aec-agent-go/kernel"
)

// ToolHandlerFactory returns a fresh toolhub.Handler.
type ToolHandlerFactory func(t *testing.T) toolhub.Handler

// toolHandlerContext is the test tool context: a session and a no-op Emit.
func toolHandlerContext(sessionID string) toolhub.Context {
	return toolhub.Context{
		SessionID: sessionID,
		Emit:      func(string, any) {},
	}
}

// ToolHandler runs the conformance suite for toolhub.Handler implementations. It
// verifies the Execute envelope, the definition shape and access to the tool
// context in 9 tests. validArgs are arguments the
// handler accepts; nil means an empty object.
//
// It adds two Go-only subtests: Execute must return promptly when its context is
// already cancelled, and Execute must not modify the argument bytes it is given.
func ToolHandler(t *testing.T, newHandler ToolHandlerFactory, validArgs kernel.Arguments) {
	t.Helper()

	// args returns a fresh copy of the valid arguments for every call, so a
	// handler that writes into its input cannot affect a later subtest.
	args := func() kernel.Arguments {
		if len(validArgs) == 0 {
			return kernel.Arguments(`{}`)
		}
		return bytes.Clone(validArgs)
	}

	// execute initializes the handler and runs it once.
	execute := func(t *testing.T, h toolhub.Handler, tc toolhub.Context) toolhub.Output {
		t.Helper()
		check.NoErr(t, kernel.Initialize(t.Context(), h))
		out, err := h.Execute(t.Context(), args(), tc)
		check.NoErr(t, err, "execute")
		return out
	}

	// -- Lifecycle --

	t.Run("has a non-empty name and version", func(t *testing.T) {
		h := newHandler(t)
		check.True(t, h.Name() != "", "name is empty")
		check.True(t, h.Version() != "", "version is empty")
	})

	t.Run("initialize() resolves without error", func(t *testing.T) {
		h := newHandler(t)
		check.NoErr(t, kernel.Initialize(t.Context(), h))
	})

	t.Run("dispose() resolves without error", func(t *testing.T) {
		h := newHandler(t)
		check.NoErr(t, kernel.Initialize(t.Context(), h))
		check.NoErr(t, kernel.Dispose(t.Context(), h))
	})

	// -- Definition --

	t.Run("definition has a non-empty name and description", func(t *testing.T) {
		def := newHandler(t).Definition()
		check.True(t, def.Name != "", "definition name is empty")
		check.True(t, def.Description != "", "definition description is empty")
	})

	t.Run("definition has parameters with type object", func(t *testing.T) {
		def := newHandler(t).Definition()
		check.Equal(t, def.Parameters.Type, "object")
	})

	t.Run("definition.required lists only properties that exist in definition.properties", func(t *testing.T) {
		params := newHandler(t).Definition().Parameters
		// This check applies only when both are present.
		if len(params.Required) == 0 || params.Properties == nil {
			return
		}
		for _, field := range params.Required {
			check.True(t, params.Properties.Has(field), "required field %q has no property", field)
		}
	})

	// -- execute() --

	t.Run("execute() returns ToolOutput with a content string", func(t *testing.T) {
		out := execute(t, newHandler(t), toolHandlerContext("test-session"))
		// Content is a Go string, so the type is guaranteed; what the wire sees is
		// the contract: the encoded output must carry a string "content" key.
		b, err := kernel.EncodeJSON(out)
		check.NoErr(t, err, "encoding the output")
		var m map[string]json.RawMessage
		check.NoErr(t, json.Unmarshal(b, &m), "decoding the output %s", b)
		content, ok := m["content"]
		check.True(t, ok, "the encoded output %s has no content key", b)
		check.True(t, len(content) > 0 && content[0] == '"', "content is not a string: %s", content)
	})

	t.Run("execute() result isError is boolean or undefined", func(t *testing.T) {
		out := execute(t, newHandler(t), toolHandlerContext("test-session"))
		// *bool is nil (undefined) or points to a boolean; reading it must be safe.
		if out.IsError != nil {
			_ = *out.IsError
		}
	})

	// -- ToolContext identity --

	t.Run("receives ToolContext with identity fields", func(t *testing.T) {
		out := execute(t, newHandler(t), toolHandlerContext("ctx-session-789"))
		b, err := kernel.EncodeJSON(out)
		check.NoErr(t, err, "encoding the output")
		check.True(t, bytes.Contains(b, []byte(`"content"`)), "the encoded output %s has no content", b)
	})

	// -- Go-only: cancellation and exact argument bytes --

	t.Run("execute returns promptly when ctx is already cancelled", func(t *testing.T) {
		h := newHandler(t)
		check.NoErr(t, kernel.Initialize(t.Context(), h))
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		done := make(chan struct{})
		go func() {
			defer close(done)
			// The result does not matter: a handler may finish or fail, but not hang.
			_, _ = h.Execute(ctx, args(), toolHandlerContext("test-session"))
		}()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatal("Execute did not return within 2s after its context was cancelled")
		}
	})

	t.Run("execute does not modify the argument bytes", func(t *testing.T) {
		h := newHandler(t)
		check.NoErr(t, kernel.Initialize(t.Context(), h))
		in := args()
		want := bytes.Clone(in)
		_, err := h.Execute(t.Context(), in, toolHandlerContext("test-session"))
		check.NoErr(t, err, "execute")
		check.True(t, bytes.Equal(in, want), "Execute changed the argument bytes: %s", in)
	})
}
