package toolhub

import (
	"context"
	"fmt"
	"strings"

	"github.com/Phong95/aec-agent-go/kernel"
)

// FuncToolVersion is the plugin version of a tool built with New or NewTypedTool. It
// matches the version of the built-in tools.
const FuncToolVersion = "1.0.0"

// NewTool returns a Handler from a definition and a function: the shortest way to
// write a tool without declaring a type. fn receives the exact argument bytes the
// model sent; use NewTypedTool to have them decoded into a struct first.
//
// The handler's plugin identity is supplied for you: Name is "tool:" followed by
// the definition name with every "_" written "-" (read_file becomes
// "tool:read-file", the convention of the built-in tools) and Version is
// FuncToolVersion. The ToolHub keys tools by Definition().Name, not by Name.
//
// def is copied, so changing it after the call does not change the tool. A nil
// fn gives a tool whose every call returns an error matching ErrNilHandler.
func NewTool(def Definition, fn func(ctx context.Context, args kernel.Arguments, tc Context) (Output, error)) Handler {
	return &funcTool{def: def.Clone(), fn: fn}
}

// NewTypedTool is New with the arguments decoded into A (kernel.Arguments.Decode:
// unknown keys are ignored, an empty call decodes as {}). A is usually a struct
// with json tags. When the arguments do not decode into A, fn is not called and
// the tool returns an error output the harness does not retry, which tells the
// model what was wrong:
//
//	invalid arguments for <tool name>: <decode error>
//
// The schema in def is still what the policy checks before the call; the decode
// is the tool's own reading of the arguments that passed it. A nil fn gives a
// tool whose every call returns an error matching ErrNilHandler.
func NewTypedTool[A any](def Definition, fn func(ctx context.Context, args A, tc Context) (Output, error)) Handler {
	if fn == nil {
		return NewTool(def, nil)
	}
	name := def.Name
	return NewTool(def, func(ctx context.Context, raw kernel.Arguments, tc Context) (Output, error) {
		var a A
		if err := raw.Decode(&a); err != nil {
			return Errorf("invalid arguments for %s: %v", name, err).NotRetryable(), nil
		}
		return fn(ctx, a, tc)
	})
}

// funcTool is the Handler behind New: a fixed definition and a function.
type funcTool struct {
	def Definition
	fn  func(ctx context.Context, args kernel.Arguments, tc Context) (Output, error)
}

var _ Handler = (*funcTool)(nil)

// Name returns "tool:" plus the definition name, with "_" written "-".
func (t *funcTool) Name() string { return "tool:" + strings.ReplaceAll(t.def.Name, "_", "-") }

// Version returns FuncToolVersion.
func (*funcTool) Version() string { return FuncToolVersion }

// Definition returns a deep copy of the definition, so a caller cannot change it.
func (t *funcTool) Definition() Definition { return t.def.Clone() }

// Execute calls the function, or returns an error matching ErrNilHandler when
// there is none.
func (t *funcTool) Execute(ctx context.Context, args kernel.Arguments, tc Context) (Output, error) {
	if t.fn == nil {
		return Output{}, fmt.Errorf("%w: tool %q was built with a nil function", ErrNilHandler, t.def.Name)
	}
	return t.fn(ctx, args, tc)
}
