package conformance

import (
	"bytes"
	"strings"
	"testing"

	"github.com/Phong95/aec-agent-go/components/toolhub"
	"github.com/Phong95/aec-agent-go/conformance/internal/check"
	"github.com/Phong95/aec-agent-go/kernel"
)

// ToolPolicyFactory returns a fresh toolhub.Policy.
type ToolPolicyFactory func(t *testing.T) toolhub.Policy

// toolPolicyContext is the test tool context: a session and a no-op Emit.
func toolPolicyContext() toolhub.Context {
	return toolhub.Context{
		SessionID: "test-session",
		Emit:      func(string, any) {},
	}
}

// toolPolicyDefinition is the mock tool definition the policy is checked against.
func toolPolicyDefinition() toolhub.Definition {
	return toolhub.Definition{
		Name:        "mock_tool",
		Description: "A mock tool for policy testing",
		Parameters: toolhub.JSONSchema{
			Type: "object",
			Properties: toolhub.NewProperties(
				toolhub.Property{Name: "query", Schema: toolhub.JSONSchema{Type: "string", Description: "Search query"}},
				toolhub.Property{Name: "limit", Schema: toolhub.JSONSchema{Type: "number", Description: "Max results"}},
				toolhub.Property{Name: "verbose", Schema: toolhub.JSONSchema{Type: "boolean", Description: "Verbose output"}},
				toolhub.Property{Name: "category", Schema: toolhub.JSONSchema{
					Type:        "string",
					Description: "Category filter",
					Enum:        []string{"alpha", "beta", "gamma"},
				}},
			),
			Required: []string{"query"},
		},
	}
}

// toolPolicyAllowed is the allowlist the policy under test is built with.
func toolPolicyAllowed() []string {
	return []string{"mock_tool", "other_tool"}
}

// argsJSON builds Arguments from JSON text, the way a provider boundary does.
func argsJSON(t *testing.T, text string) kernel.Arguments {
	t.Helper()
	args, err := kernel.NewArguments([]byte(text))
	check.NoErr(t, err, "building arguments from %s", text)
	return args
}

// ToolPolicy runs the conformance suite for toolhub.Policy implementations. It
// verifies allowlist enforcement, schema validation, authorization hooks and
// fail-closed behaviour in 18 tests. newPolicy returns
// a policy with no authorization hook; newPolicyWithDenyHook returns one whose
// hook denies every call with a reason (for example "Denied by test hook").
//
// It adds three Go-only subtests: arguments that are not a JSON object and
// arguments that are not valid JSON must be denied, never panic, and Check must
// not modify the argument bytes it is given.
func ToolPolicy(t *testing.T, newPolicy, newPolicyWithDenyHook ToolPolicyFactory) {
	t.Helper()

	// run checks one call against def and the allowed list and returns the decision.
	run := func(t *testing.T, p toolhub.Policy, name, args string, allowed []string) toolhub.Decision {
		t.Helper()
		return p.Check(t.Context(), name, argsJSON(t, args), toolPolicyContext(), toolPolicyDefinition(), allowed)
	}
	mustDeny := func(t *testing.T, d toolhub.Decision, reasonHas string) {
		t.Helper()
		check.False(t, d.Allowed, "the call must be denied")
		if d.Reason == nil {
			t.Errorf("a denial must carry a reason")
			return
		}
		if reasonHas != "" {
			check.True(t, strings.Contains(*d.Reason, reasonHas), "reason %q does not contain %q", *d.Reason, reasonHas)
		}
	}

	// -- Plugin lifecycle --

	t.Run("has a non-empty name and version", func(t *testing.T) {
		p := newPolicy(t)
		check.True(t, p.Name() != "", "name is empty")
		check.True(t, p.Version() != "", "version is empty")
	})

	t.Run("initialize() resolves without error", func(t *testing.T) {
		p := newPolicy(t)
		check.NoErr(t, kernel.Initialize(t.Context(), p))
	})

	t.Run("dispose() resolves without error", func(t *testing.T) {
		p := newPolicy(t)
		check.NoErr(t, kernel.Initialize(t.Context(), p))
		check.NoErr(t, kernel.Dispose(t.Context(), p))
	})

	// -- Allowlist enforcement --

	t.Run("allows a tool that is in the allowed list", func(t *testing.T) {
		d := run(t, newPolicy(t), "mock_tool", `{"query":"test"}`, toolPolicyAllowed())
		check.True(t, d.Allowed, "decision %+v", d)
	})

	t.Run("denies a tool that is NOT in the allowed list", func(t *testing.T) {
		d := run(t, newPolicy(t), "forbidden_tool", `{"query":"test"}`, toolPolicyAllowed())
		mustDeny(t, d, "")
	})

	t.Run("allows any tool when the allowed list is empty (no restriction)", func(t *testing.T) {
		d := run(t, newPolicy(t), "any_tool", `{"query":"test"}`, []string{})
		check.True(t, d.Allowed, "decision %+v", d)
	})

	// -- Schema validation --

	t.Run("allows valid arguments matching the schema", func(t *testing.T) {
		d := run(t, newPolicy(t), "mock_tool", `{"query":"find rebar"}`, toolPolicyAllowed())
		check.True(t, d.Allowed, "decision %+v", d)
	})

	t.Run("denies when a required argument is missing", func(t *testing.T) {
		d := run(t, newPolicy(t), "mock_tool", `{"limit":10}`, toolPolicyAllowed())
		mustDeny(t, d, "query")
	})

	t.Run("denies when an argument has the wrong type", func(t *testing.T) {
		d := run(t, newPolicy(t), "mock_tool", `{"query":12345}`, toolPolicyAllowed())
		mustDeny(t, d, "query")
	})

	t.Run("denies when a number argument receives a string", func(t *testing.T) {
		d := run(t, newPolicy(t), "mock_tool", `{"query":"test","limit":"not-a-number"}`, toolPolicyAllowed())
		mustDeny(t, d, "limit")
	})

	t.Run("denies when a boolean argument receives a string", func(t *testing.T) {
		d := run(t, newPolicy(t), "mock_tool", `{"query":"test","verbose":"yes"}`, toolPolicyAllowed())
		mustDeny(t, d, "verbose")
	})

	t.Run("denies when an enum argument receives an invalid value", func(t *testing.T) {
		d := run(t, newPolicy(t), "mock_tool", `{"query":"test","category":"invalid"}`, toolPolicyAllowed())
		mustDeny(t, d, "category")
	})

	t.Run("allows valid enum values", func(t *testing.T) {
		d := run(t, newPolicy(t), "mock_tool", `{"query":"test","category":"beta"}`, toolPolicyAllowed())
		check.True(t, d.Allowed, "decision %+v", d)
	})

	t.Run("allows optional arguments to be omitted", func(t *testing.T) {
		d := run(t, newPolicy(t), "mock_tool", `{"query":"test"}`, toolPolicyAllowed())
		check.True(t, d.Allowed, "decision %+v", d)
	})

	// -- Authorization hook --

	t.Run("denies when the authorization hook returns deny", func(t *testing.T) {
		d := run(t, newPolicyWithDenyHook(t), "mock_tool", `{"query":"test"}`, toolPolicyAllowed())
		mustDeny(t, d, "")
	})

	// -- Context immutability --

	t.Run("policy check does not mutate the context object", func(t *testing.T) {
		p := newPolicy(t)
		tc := toolPolicyContext()
		original := tc.SessionID
		p.Check(t.Context(), "mock_tool", argsJSON(t, `{"query":"test"}`), tc, toolPolicyDefinition(), toolPolicyAllowed())
		check.Equal(t, tc.SessionID, original)
	})

	// -- Fail-closed behavior --

	t.Run("returns a decision object (never throws) for allowlist denial", func(t *testing.T) {
		d := run(t, newPolicy(t), "nonexistent_tool", `{}`, toolPolicyAllowed())
		check.False(t, d.Allowed, "the call must be denied")
	})

	t.Run("returns a decision object (never throws) for schema denial", func(t *testing.T) {
		d := run(t, newPolicy(t), "mock_tool", `{}`, toolPolicyAllowed())
		check.False(t, d.Allowed, "the call must be denied")
	})

	// -- Go-only: arguments the policy must refuse and must not change --

	t.Run("denies non-object arguments", func(t *testing.T) {
		p := newPolicy(t)
		// kernel.NewArguments would reject these; a caller can still build the
		// bytes directly, and the policy is the last gate before the handler.
		for _, raw := range []string{`[1]`, `"text"`, `42`, `true`} {
			d := p.Check(t.Context(), "mock_tool", kernel.Arguments(raw), toolPolicyContext(), toolPolicyDefinition(), toolPolicyAllowed())
			check.False(t, d.Allowed, "arguments %s must be denied", raw)
		}
	})

	t.Run("denies arguments that are not valid JSON", func(t *testing.T) {
		p := newPolicy(t)
		for _, raw := range []string{`{"query":`, `{"query":"x"`, `not json`} {
			d := p.Check(t.Context(), "mock_tool", kernel.Arguments(raw), toolPolicyContext(), toolPolicyDefinition(), toolPolicyAllowed())
			check.False(t, d.Allowed, "arguments %s must be denied", raw)
		}
	})

	t.Run("check does not modify the argument bytes", func(t *testing.T) {
		args := argsJSON(t, `{"query":"test","zeta":1,"alpha":"a<b"}`)
		want := bytes.Clone(args)
		for _, p := range []toolhub.Policy{newPolicy(t), newPolicyWithDenyHook(t)} {
			p.Check(t.Context(), "mock_tool", args, toolPolicyContext(), toolPolicyDefinition(), toolPolicyAllowed())
			check.True(t, bytes.Equal(args, want), "Check changed the argument bytes: %s", args)
		}
	})
}
