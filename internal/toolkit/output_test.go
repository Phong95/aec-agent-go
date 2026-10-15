package toolkit_test

import (
	"testing"

	"github.com/Phong95/aec-agent-go/internal/toolkit"
	"github.com/Phong95/aec-agent-go/kernel"
	"github.com/Phong95/aec-agent-go/kernel/ordered"
)

func encode(t *testing.T, v any) string {
	t.Helper()
	b, err := kernel.EncodeJSON(v)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	return string(b)
}

// TestOutputs pins the wire form of the three builders: the key order content, isError,
// retryable, metadata (the ToolOutput wire order), and an absent metadata left out.
func TestOutputs(t *testing.T) {
	meta := ordered.NewObject()
	meta.Set("code", "not_found")
	meta.Set("n", 2)

	for _, tc := range []struct {
		name string
		got  any
		want string
	}{
		{"Fail", toolkit.Fail("bad input"), `{"content":"bad input","isError":true,"retryable":false}`},
		{"Failure not retryable", toolkit.Failure("nope", false, ordered.Object{}), `{"content":"nope","isError":true,"retryable":false}`},
		{"Failure retryable", toolkit.Failure("try again", true, ordered.Object{}), `{"content":"try again","isError":true,"retryable":true}`},
		{"Failure with metadata", toolkit.Failure("x", true, meta), `{"content":"x","isError":true,"retryable":true,"metadata":{"code":"not_found","n":2}}`},
		{"Failure with an empty metadata object", toolkit.Failure("x", false, ordered.NewObject()), `{"content":"x","isError":true,"retryable":false,"metadata":{}}`},
		{"Result", toolkit.Result("ok", ordered.Object{}), `{"content":"ok"}`},
		{"Result with metadata", toolkit.Result("ok", meta), `{"content":"ok","metadata":{"code":"not_found","n":2}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := encode(t, tc.got); got != tc.want {
				t.Errorf("encoded = %s, want %s", got, tc.want)
			}
		})
	}
}

// TestFailIsNotRetryableAndAnError checks the pointers, not only the encoding.
func TestFailIsNotRetryableAndAnError(t *testing.T) {
	o := toolkit.Fail("x")
	if o.IsError == nil || !*o.IsError {
		t.Error("Fail must set isError true")
	}
	if o.Retryable == nil || *o.Retryable {
		t.Error("Fail must set retryable false")
	}
	r := toolkit.Result("x", ordered.Object{})
	if r.IsError != nil || r.Retryable != nil {
		t.Error("Result must set neither isError nor retryable")
	}
}
