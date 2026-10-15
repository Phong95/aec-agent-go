package conformance_test

import (
	"testing"

	"github.com/Phong95/aec-agent-go/aecmock"
	"github.com/Phong95/aec-agent-go/components/llmhub"
	"github.com/Phong95/aec-agent-go/conformance"
	"github.com/Phong95/aec-agent-go/kernel"
)

// echo is the response function of the conformance runner: the last user
// message with an "Echo: " prefix, or "empty".
func echo(msgs []kernel.Message) string {
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == kernel.RoleUser {
			return "Echo: " + msgs[i].Content
		}
	}
	return "Echo: empty"
}

func TestProviderMock(t *testing.T) {
	conformance.Provider(t,
		func(_ *testing.T) llmhub.Provider { return aecmock.NewMockProvider("test-adapter", echo) },
		conformance.ProviderOptions{
			NewFailing: func(_ *testing.T) llmhub.Provider {
				return aecmock.NewMockProvider("test-adapter", echo,
					aecmock.WithFailure(kernel.NewModelError("upstream failed", kernel.ModelCodeProvider, true)))
			},
			NewBlocking: func(_ *testing.T) llmhub.Provider {
				return aecmock.NewMockProvider("test-adapter", echo, aecmock.WithHangAfter(1))
			},
		})
}
