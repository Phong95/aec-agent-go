package conformance_test

import (
	"testing"

	"github.com/Phong95/aec-agent-go/components/llmhub"
	"github.com/Phong95/aec-agent-go/components/llmhub/providers/openaisub"
	"github.com/Phong95/aec-agent-go/conformance"
	"github.com/Phong95/aec-agent-go/internal/llmtest"
)

// The Provider suite against the real ChatGPT-subscription provider (Phase 8, LLM-07).
// The provider talks to a loopback httptest upstream: a factory call starts a fresh
// upstream (closed when the subtest ends), so the break-early subtest's goroutine
// count is not disturbed by a connection an earlier subtest left open, and no test
// reaches chatgpt.com. The subscription endpoints are fixed constants; the
// openaisub.WithEndpoints option, which exists for tests, points them at the upstream.

const (
	subscriptionResponsesPath = "/backend-api/codex/responses"
	subscriptionModelsPath    = "/backend-api/codex/models"

	subscriptionHello = "data: {\"type\":\"response.output_text.delta\",\"delta\":\"Hello \"}\n\n"
	subscriptionRest  = "data: {\"type\":\"response.output_text.delta\",\"delta\":\"from the subscription\"}\n\n"
	subscriptionDone  = "data: {\"type\":\"response.completed\",\"response\":{\"usage\":{\"input_tokens\":10,\"output_tokens\":5}}}\n\n"
)

// subscriptionProvider builds a subscription provider whose upstream answers with
// respond.
func subscriptionProvider(t *testing.T, respond func(llmtest.Request) llmtest.Reply) llmhub.Provider {
	t.Helper()
	up := llmtest.NewUpstream(t, respond)
	p, err := openaisub.New(llmhub.ProviderConfig{OAuthAccessToken: "tok", ChatgptAccountID: "acct"},
		openaisub.WithEndpoints(up.URL+subscriptionResponsesPath, up.URL+subscriptionModelsPath),
		openaisub.WithHTTPClient(up.Client()))
	if err != nil {
		t.Fatalf("openaisub.New: %v", err)
	}
	return p
}

func TestProviderSubscription(t *testing.T) {
	conformance.Provider(t,
		func(t *testing.T) llmhub.Provider {
			// Chat collects this same stream, so it returns end_turn with content.
			return subscriptionProvider(t, func(llmtest.Request) llmtest.Reply {
				return llmtest.Reply{
					Header: map[string]string{"Content-Type": "text/event-stream"},
					Chunks: []string{subscriptionHello, subscriptionRest, subscriptionDone},
				}
			})
		},
		conformance.ProviderOptions{
			// A 401 is not retried by the provider and costs one request, for Chat and
			// Stream: one in-band error chunk, and a *kernel.ModelError from Chat.
			NewFailing: func(t *testing.T) llmhub.Provider {
				return subscriptionProvider(t, func(llmtest.Request) llmtest.Reply {
					return llmtest.Reply{Status: 401, Header: map[string]string{"Content-Type": "application/json"}, Chunks: []string{`{"detail":"token expired"}`}}
				})
			},
			// One text delta, then the upstream stops writing until the request ends.
			NewBlocking: func(t *testing.T) llmhub.Provider {
				return subscriptionProvider(t, func(llmtest.Request) llmtest.Reply {
					return llmtest.Reply{
						Header: map[string]string{"Content-Type": "text/event-stream"},
						Chunks: []string{subscriptionHello},
						Hang:   true,
					}
				})
			},
		})
}
