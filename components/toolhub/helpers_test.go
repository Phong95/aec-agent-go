package toolhub_test

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/Phong95/aec-agent-go/components/toolhub"
	"github.com/Phong95/aec-agent-go/kernel"
)

// The schema helpers must build the same value, and so the same bytes, as a
// hand-written literal.
func TestSchemaHelpersMatchLiterals(t *testing.T) {
	cases := map[string]struct {
		got, want toolhub.JSONSchema
		wire      string
	}{
		"object with required": {
			got: toolhub.Object(
				toolhub.Prop("path", toolhub.String("File path")),
				toolhub.Prop("mode", toolhub.Enum("Mode", "a", "b")),
				toolhub.Prop("tags", toolhub.Array(toolhub.String(""), "Tags")),
				toolhub.Prop("n", toolhub.Number("")),
				toolhub.Prop("i", toolhub.Integer("I")),
				toolhub.Prop("ok", toolhub.Boolean("OK")),
			).Require("path"),
			want: toolhub.JSONSchema{
				Type: "object",
				Properties: toolhub.NewProperties(
					toolhub.Property{Name: "path", Schema: toolhub.JSONSchema{Type: "string", Description: "File path"}},
					toolhub.Property{Name: "mode", Schema: toolhub.JSONSchema{Type: "string", Enum: []string{"a", "b"}, Description: "Mode"}},
					toolhub.Property{Name: "tags", Schema: toolhub.JSONSchema{Type: "array", Items: &toolhub.JSONSchema{Type: "string"}, Description: "Tags"}},
					toolhub.Property{Name: "n", Schema: toolhub.JSONSchema{Type: "number"}},
					toolhub.Property{Name: "i", Schema: toolhub.JSONSchema{Type: "integer", Description: "I"}},
					toolhub.Property{Name: "ok", Schema: toolhub.JSONSchema{Type: "boolean", Description: "OK"}},
				),
				Required: []string{"path"},
			},
			wire: `{"type":"object","properties":{"path":{"type":"string","description":"File path"},` +
				`"mode":{"type":"string","enum":["a","b"],"description":"Mode"},` +
				`"tags":{"type":"array","items":{"type":"string"},"description":"Tags"},` +
				`"n":{"type":"number"},"i":{"type":"integer","description":"I"},` +
				`"ok":{"type":"boolean","description":"OK"}},"required":["path"]}`,
		},
		"no arguments": {
			got:  toolhub.Object(),
			want: toolhub.JSONSchema{Type: "object", Properties: toolhub.NewProperties()},
			wire: `{"type":"object","properties":{}}`,
		},
		"empty enum": {
			got:  toolhub.Enum("x"),
			want: toolhub.JSONSchema{Type: "string", Enum: []string{}, Description: "x"},
			wire: `{"type":"string","enum":[],"description":"x"}`,
		},
		"require nothing": {
			got:  toolhub.Object().Require(),
			want: toolhub.JSONSchema{Type: "object", Properties: toolhub.NewProperties()},
			wire: `{"type":"object","properties":{}}`,
		},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			got, err := kernel.EncodeJSON(c.got)
			if err != nil {
				t.Fatal(err)
			}
			want, err := kernel.EncodeJSON(c.want)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != string(want) || string(got) != c.wire {
				t.Fatalf("helpers = %s\nliteral = %s\nwant    = %s", got, want, c.wire)
			}
			if !reflect.DeepEqual(c.got.Clone(), c.want.Clone()) {
				t.Fatalf("helpers value differs from the literal")
			}
		})
	}
}

// Array and Require copy their inputs, so the caller's values stay its own.
func TestSchemaHelpersCopyInputs(t *testing.T) {
	items := toolhub.String("a")
	arr := toolhub.Array(items, "")
	items.Description = "changed"
	if arr.Items.Description != "a" {
		t.Fatal("Array shares its items with the caller")
	}
	names := []string{"x"}
	s := toolhub.Object().Require(names...)
	names[0] = "y"
	if s.Required[0] != "x" {
		t.Fatal("Require shares its names with the caller")
	}
}

func TestOutputHelpersMatchLiterals(t *testing.T) {
	isErr, no := true, false
	cases := map[string]struct {
		got, want toolhub.Output
		wire      string
	}{
		"text":          {toolhub.Text("ok"), toolhub.Output{Content: "ok"}, `{"content":"ok"}`},
		"error":         {toolhub.Error("bad"), toolhub.Output{Content: "bad", IsError: &isErr}, `{"content":"bad","isError":true}`},
		"errorf":        {toolhub.Errorf("bad %d", 7), toolhub.Output{Content: "bad 7", IsError: &isErr}, `{"content":"bad 7","isError":true}`},
		"not retryable": {toolhub.Error("bad").NotRetryable(), toolhub.Output{Content: "bad", IsError: &isErr, Retryable: &no}, `{"content":"bad","isError":true,"retryable":false}`},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			got, _ := kernel.EncodeJSON(c.got)
			want, _ := kernel.EncodeJSON(c.want)
			if string(got) != string(want) || string(got) != c.wire {
				t.Fatalf("helpers = %s\nliteral = %s\nwant    = %s", got, want, c.wire)
			}
		})
	}
}

func TestNewIdentityAndDefinitionCopy(t *testing.T) {
	def := toolhub.Definition{Name: "read_file", Parameters: toolhub.Object(toolhub.Prop("p", toolhub.String("")))}
	h := toolhub.NewTool(def, func(context.Context, kernel.Arguments, toolhub.Context) (toolhub.Output, error) {
		return toolhub.Text("x"), nil
	})
	if h.Name() != "tool:read-file" || h.Version() != toolhub.FuncToolVersion {
		t.Fatalf("identity = %q %q", h.Name(), h.Version())
	}
	def.Name = "changed"
	def.Parameters.Properties.Set("q", toolhub.String(""))
	got := h.Definition()
	if got.Name != "read_file" || got.Parameters.Properties.Len() != 1 {
		t.Fatalf("New shares its definition with the caller: %+v", got)
	}
	got.Parameters.Properties.Set("r", toolhub.String(""))
	if h.Definition().Parameters.Properties.Len() != 1 {
		t.Fatal("Definition returns a shared value")
	}
}

func TestNewTyped(t *testing.T) {
	type args struct {
		N float64 `json:"n"`
	}
	sentinel := errors.New("boom")
	h := toolhub.NewTypedTool(toolhub.Definition{Name: "t"}, func(_ context.Context, a args, _ toolhub.Context) (toolhub.Output, error) {
		if a.N < 0 {
			return toolhub.Output{}, sentinel
		}
		return toolhub.Errorf("n=%v", a.N), nil
	})
	ctx := context.Background()

	out, err := h.Execute(ctx, kernel.Arguments(`{"n":3,"extra":true}`), toolhub.Context{})
	if err != nil || out.Content != "n=3" {
		t.Fatalf("decoded call = %+v, %v", out, err)
	}
	out, err = h.Execute(ctx, nil, toolhub.Context{})
	if err != nil || out.Content != "n=0" {
		t.Fatalf("empty call = %+v, %v", out, err)
	}
	if _, err = h.Execute(ctx, kernel.Arguments(`{"n":-1}`), toolhub.Context{}); !errors.Is(err, sentinel) {
		t.Fatalf("returned error = %v", err)
	}

	out, err = h.Execute(ctx, kernel.Arguments(`{"n":"three"}`), toolhub.Context{})
	if err != nil {
		t.Fatal(err)
	}
	wire, _ := kernel.EncodeJSON(out)
	if !strings.HasPrefix(out.Content, "invalid arguments for t: ") ||
		!strings.HasSuffix(string(wire), `"isError":true,"retryable":false}`) {
		t.Fatalf("decode error output = %s", wire)
	}
}

func TestNewNilFunction(t *testing.T) {
	for name, h := range map[string]toolhub.Handler{
		"NewTool":      toolhub.NewTool(toolhub.Definition{Name: "x"}, nil),
		"NewTypedTool": toolhub.NewTypedTool[struct{}](toolhub.Definition{Name: "x"}, nil),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := h.Execute(context.Background(), nil, toolhub.Context{}); !errors.Is(err, toolhub.ErrNilHandler) {
				t.Fatalf("err = %v, want ErrNilHandler", err)
			}
		})
	}
}
