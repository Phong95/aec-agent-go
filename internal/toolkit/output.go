package toolkit

import (
	"github.com/Phong95/aec-agent-go/components/toolhub"
	"github.com/Phong95/aec-agent-go/kernel/ordered"
)

// Fail is the fail helper: an error output the harness must not retry (isError
// true, retryable false). Argument and validation problems the model can fix by itself
// are reported this way, as a tool-level output and not as a returned error.
func Fail(content string) toolhub.Output {
	return Failure(content, false, ordered.Object{})
}

// Failure is an error output with an explicit retryable flag and metadata. The zero
// Object is left out of the encoded output, as an absent metadata is.
func Failure(content string, retryable bool, meta ordered.Object) toolhub.Output {
	out := toolhub.Error(content)
	out.Retryable = &retryable
	out.Metadata = meta
	return out
}

// Result is a successful output: no isError and no retryable flag. The zero Object is
// left out of the encoded output.
func Result(content string, meta ordered.Object) toolhub.Output {
	out := toolhub.Text(content)
	out.Metadata = meta
	return out
}
