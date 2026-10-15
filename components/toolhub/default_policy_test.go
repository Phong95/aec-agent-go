package toolhub_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/Phong95/aec-agent-go/components/toolhub"
	"github.com/Phong95/aec-agent-go/internal/jsonx"
	"github.com/Phong95/aec-agent-go/internal/wiretest"
	"github.com/Phong95/aec-agent-go/kernel"
)

// policyRow is one row of tool-policy-decisions.json, the recorded fixture of
// policy decisions.
type policyRow struct {
	Name         string             `json:"name"`
	ToolName     string             `json:"toolName"`
	AllowedTools []string           `json:"allowedTools"`
	Definition   toolhub.Definition `json:"definition"`
	ArgsJSON     string             `json:"argsJson"`
	Decision     json.RawMessage    `json:"decision,omitempty"`
	TSThrows     string             `json:"tsThrows,omitempty"`
}

func loadPolicyRows(t *testing.T) []policyRow {
	t.Helper()
	var rows []policyRow
	if err := jsonx.UnmarshalStrict(wiretest.Load(t, "tool-policy-decisions.json"), &rows); err != nil {
		t.Fatalf("decode tool-policy-decisions.json: %v", err)
	}
	return rows
}

// isJSONObject reports whether text is valid JSON whose first non-space byte is
// an opening brace: the only arguments the Go policy evaluates.
func isJSONObject(text string) bool {
	trimmed := strings.TrimLeft(text, " \t\r\n")
	return json.Valid([]byte(text)) && strings.HasPrefix(trimmed, "{")
}

func reasonOf(d toolhub.Decision) string {
	if d.Reason == nil {
		return "<nil>"
	}
	return *d.Reason
}

// policySchemaDivergences are the recorded rows where the schema check deliberately
// differs from the recorded fixture decision ("policy schema types"): the fixture lets an
// "object" property accept null and an array, and denies every value of an "integer"
// property. DefaultPolicy accepts only a JSON object for "object" and a number with no
// fractional part for "integer". Each row holds the DefaultPolicy decision.
var policySchemaDivergences = map[string]string{
	"object-array":              `{"allowed":false,"reason":"Argument \"o\" must be object, got array"}`,
	"object-null":               `{"allowed":false,"reason":"Argument \"o\" must be object, got null"}`,
	"integer-type-never-passes": `{"allowed":true}`,
}

func TestDefaultPolicyMatchesGoldens(t *testing.T) {
	rows := loadPolicyRows(t)
	if len(rows) < 30 {
		t.Fatalf("want at least 30 scenario rows, got %d", len(rows))
	}
	var allowed, denied, throws int
	for _, row := range rows {
		switch {
		case row.TSThrows != "":
			throws++
		case bytes.Contains(row.Decision, []byte(`"allowed":true`)):
			allowed++
		default:
			denied++
		}
	}
	if allowed == 0 || denied == 0 || throws == 0 {
		t.Fatalf("the table must hold allowed, denied and tsThrows rows, got %d, %d, %d", allowed, denied, throws)
	}

	policy := toolhub.NewDefaultPolicy(toolhub.DefaultPolicyOptions{})
	for _, row := range rows {
		t.Run(row.Name, func(t *testing.T) {
			// Built directly from the text, deliberately bypassing NewArguments, so the
			// policy proves it is robust to raw bytes.
			args := kernel.Arguments(row.ArgsJSON)
			before := bytes.Clone(args)

			got := policy.Check(t.Context(), row.ToolName, args, toolhub.Context{SessionID: "s"}, row.Definition, row.AllowedTools)

			if !bytes.Equal(args, before) {
				t.Fatalf("Check modified the argument bytes: %s", args)
			}
			if !isJSONObject(row.ArgsJSON) {
				if got.Allowed || reasonOf(got) != "Arguments must be a JSON object" {
					t.Fatalf("non-object arguments %q: want a denial with %q, got allowed=%v reason=%s",
						row.ArgsJSON, "Arguments must be a JSON object", got.Allowed, reasonOf(got))
				}
				return
			}
			if row.Decision == nil {
				t.Fatalf("row %s holds an object but no TS decision", row.Name)
			}
			encoded, err := kernel.EncodeJSON(got)
			if err != nil {
				t.Fatalf("encode decision: %v", err)
			}
			want := row.Decision
			if goWant, ok := policySchemaDivergences[row.Name]; ok {
				if bytes.Equal(want, []byte(goWant)) {
					t.Fatalf("row %s: the TS decision equals the Go one now; drop it from policySchemaDivergences", row.Name)
				}
				want = []byte(goWant)
			}
			if !bytes.Equal(encoded, want) {
				t.Fatalf("decision differs from the expected one\n got: %s\nwant: %s\n  TS: %s", encoded, want, row.Decision)
			}
		})
	}
}

// scenarioSchema is a small definition for the hook-stage tests.
func scenarioDefinition() toolhub.Definition {
	return toolhub.Definition{
		Name:        "t",
		Description: "d",
		Parameters: toolhub.JSONSchema{
			Type:       "object",
			Properties: toolhub.NewProperties(toolhub.Property{Name: "s", Schema: toolhub.JSONSchema{Type: "string"}}),
			Required:   []string{"s"},
		},
	}
}

func TestDefaultPolicyHookStage(t *testing.T) {
	def := scenarioDefinition()
	valid := kernel.Arguments(`{"s":"a"}`)

	t.Run("a deny decision is returned unchanged", func(t *testing.T) {
		var calls atomic.Int32
		p := toolhub.NewDefaultPolicy(toolhub.DefaultPolicyOptions{AuthorizationHook: func(context.Context, string, kernel.Arguments, toolhub.Context) (toolhub.Decision, error) {
			calls.Add(1)
			return toolhub.Deny("Denied by test hook"), nil
		}})
		got := p.Check(t.Context(), "t", valid, toolhub.Context{}, def, nil)
		if got.Allowed || reasonOf(got) != "Denied by test hook" || calls.Load() != 1 {
			t.Fatalf("got allowed=%v reason=%s calls=%d", got.Allowed, reasonOf(got), calls.Load())
		}
	})

	t.Run("an empty deny reason is kept", func(t *testing.T) {
		p := toolhub.NewDefaultPolicy(toolhub.DefaultPolicyOptions{AuthorizationHook: func(context.Context, string, kernel.Arguments, toolhub.Context) (toolhub.Decision, error) {
			return toolhub.Deny(""), nil
		}})
		got := p.Check(t.Context(), "t", valid, toolhub.Context{}, def, nil)
		if got.Allowed || got.Reason == nil || *got.Reason != "" {
			t.Fatalf("want a denial with an explicit empty reason, got %+v", got)
		}
	})

	t.Run("an allowing hook allows", func(t *testing.T) {
		p := toolhub.NewDefaultPolicy(toolhub.DefaultPolicyOptions{AuthorizationHook: func(context.Context, string, kernel.Arguments, toolhub.Context) (toolhub.Decision, error) {
			return toolhub.Allow(), nil
		}})
		got := p.Check(t.Context(), "t", valid, toolhub.Context{}, def, nil)
		if !got.Allowed || got.Reason != nil {
			t.Fatalf("want a plain allow, got %+v", got)
		}
	})

	t.Run("the hook is skipped when the allowlist denies", func(t *testing.T) {
		var calls atomic.Int32
		p := toolhub.NewDefaultPolicy(toolhub.DefaultPolicyOptions{AuthorizationHook: func(context.Context, string, kernel.Arguments, toolhub.Context) (toolhub.Decision, error) {
			calls.Add(1)
			return toolhub.Allow(), nil
		}})
		got := p.Check(t.Context(), "t", valid, toolhub.Context{}, def, []string{"other"})
		if got.Allowed || calls.Load() != 0 {
			t.Fatalf("allowed=%v hook calls=%d", got.Allowed, calls.Load())
		}
	})

	t.Run("the hook is skipped when the schema denies", func(t *testing.T) {
		var calls atomic.Int32
		p := toolhub.NewDefaultPolicy(toolhub.DefaultPolicyOptions{AuthorizationHook: func(context.Context, string, kernel.Arguments, toolhub.Context) (toolhub.Decision, error) {
			calls.Add(1)
			return toolhub.Allow(), nil
		}})
		got := p.Check(t.Context(), "t", kernel.Arguments(`{}`), toolhub.Context{}, def, nil)
		if got.Allowed || calls.Load() != 0 {
			t.Fatalf("allowed=%v hook calls=%d", got.Allowed, calls.Load())
		}
	})

	t.Run("a hook error is a denial", func(t *testing.T) {
		p := toolhub.NewDefaultPolicy(toolhub.DefaultPolicyOptions{AuthorizationHook: func(context.Context, string, kernel.Arguments, toolhub.Context) (toolhub.Decision, error) {
			return toolhub.Allow(), errors.New("boom")
		}})
		got := p.Check(t.Context(), "t", valid, toolhub.Context{}, def, nil)
		if got.Allowed || reasonOf(got) != "Authorization hook failed: boom" {
			t.Fatalf("got allowed=%v reason=%s", got.Allowed, reasonOf(got))
		}
	})

	t.Run("a hook panic is a denial", func(t *testing.T) {
		p := toolhub.NewDefaultPolicy(toolhub.DefaultPolicyOptions{AuthorizationHook: func(context.Context, string, kernel.Arguments, toolhub.Context) (toolhub.Decision, error) {
			panic("kaboom")
		}})
		got := p.Check(t.Context(), "t", valid, toolhub.Context{}, def, nil)
		if got.Allowed || !strings.HasPrefix(reasonOf(got), "Authorization hook failed: ") || !strings.Contains(reasonOf(got), "kaboom") {
			t.Fatalf("got allowed=%v reason=%s", got.Allowed, reasonOf(got))
		}
	})

	t.Run("a hook that overwrites its arguments does not change the caller's bytes", func(t *testing.T) {
		p := toolhub.NewDefaultPolicy(toolhub.DefaultPolicyOptions{AuthorizationHook: func(_ context.Context, _ string, args kernel.Arguments, _ toolhub.Context) (toolhub.Decision, error) {
			for i := range args {
				args[i] = 'X'
			}
			return toolhub.Allow(), nil
		}})
		args := bytes.Clone(valid)
		got := p.Check(t.Context(), "t", args, toolhub.Context{}, def, nil)
		if !got.Allowed {
			t.Fatalf("want allow, got %s", reasonOf(got))
		}
		if !bytes.Equal(args, valid) {
			t.Fatalf("hook changed the caller's bytes: %s", args)
		}
	})

	t.Run("the zero value is a usable policy", func(t *testing.T) {
		var p toolhub.DefaultPolicy
		if got := p.Check(t.Context(), "t", valid, toolhub.Context{}, def, nil); !got.Allowed {
			t.Fatalf("zero policy denied: %s", reasonOf(got))
		}
	})
}

func TestDefaultPolicyIdentity(t *testing.T) {
	p := toolhub.NewDefaultPolicy(toolhub.DefaultPolicyOptions{})
	if p.Name() != "policy:default" || p.Version() != "1.0.0" {
		t.Fatalf("identity = %s %s", p.Name(), p.Version())
	}
	if err := kernel.Initialize(t.Context(), p); err != nil {
		t.Fatalf("initialize: %v", err)
	}
	if err := kernel.Dispose(t.Context(), p); err != nil {
		t.Fatalf("dispose: %v", err)
	}
}

// TestDefaultPolicySchemaIntegerAndObject pins the Go schema types: "integer" is a JSON
// number with no fractional part (exact on the decimal text, any exponent), and "object" is
// a JSON object only (not null, not an array).
func TestDefaultPolicySchemaIntegerAndObject(t *testing.T) {
	def := toolhub.Definition{
		Name: "t",
		Parameters: toolhub.JSONSchema{
			Type: "object",
			Properties: toolhub.NewProperties(
				toolhub.Property{Name: "i", Schema: toolhub.JSONSchema{Type: "integer"}},
				toolhub.Property{Name: "o", Schema: toolhub.JSONSchema{Type: "object"}},
			),
		},
	}
	p := toolhub.NewDefaultPolicy(toolhub.DefaultPolicyOptions{})
	cases := []struct {
		args   string
		allow  bool
		reason string
	}{
		{`{"i":0}`, true, ""},
		{`{"i":-0}`, true, ""},
		{`{"i":-7}`, true, ""},
		{`{"i":1.0}`, true, ""},
		{`{"i":1.500e1}`, true, ""},
		{`{"i":1e3}`, true, ""},
		{`{"i":1E+3}`, true, ""},
		{`{"i":1e400}`, true, ""},
		{`{"i":0.0e-99999999999999999999}`, true, ""},
		{`{"i":12345678901234567890123}`, true, ""},
		{`{"i":1.5}`, false, `Argument "i" must be integer, got number`},
		{`{"i":1e-3}`, false, `Argument "i" must be integer, got number`},
		{`{"i":15e-1}`, false, `Argument "i" must be integer, got number`},
		{`{"i":1.0000000000000000000001}`, false, `Argument "i" must be integer, got number`},
		{`{"i":5e-99999999999999999999}`, false, `Argument "i" must be integer, got number`},
		{`{"i":"1"}`, false, `Argument "i" must be integer, got string`},
		{`{"i":null}`, false, `Argument "i" must be integer, got object`},
		{`{"o":{}}`, true, ""},
		{`{"o":{"a":[1]}}`, true, ""},
		{`{"o":null}`, false, `Argument "o" must be object, got null`},
		{`{"o":[]}`, false, `Argument "o" must be object, got array`},
		{`{"o":"x"}`, false, `Argument "o" must be object, got string`},
		{`{"o":1}`, false, `Argument "o" must be object, got number`},
	}
	for _, c := range cases {
		got := p.Check(t.Context(), "t", kernel.Arguments(c.args), toolhub.Context{}, def, nil)
		if got.Allowed != c.allow || (!c.allow && reasonOf(got) != c.reason) {
			t.Errorf("%s: got allowed=%v reason=%s, want allowed=%v reason=%s", c.args, got.Allowed, reasonOf(got), c.allow, c.reason)
		}
	}
}
