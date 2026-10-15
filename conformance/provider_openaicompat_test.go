package conformance_test

import (
	"testing"

	"github.com/Phong95/aec-agent-go/components/llmhub"
	"github.com/Phong95/aec-agent-go/components/llmhub/providers/openaicompat"
	"github.com/Phong95/aec-agent-go/conformance"
	"github.com/Phong95/aec-agent-go/internal/llmtest"
)

// The Provider suite against the real OpenAI-compatible provider (Phase 8, LLM-07).
// Every provider talks to a loopback httptest upstream: a factory call starts a
// fresh upstream (closed when the subtest ends), so the break-early subtest's
// goroutine count is not disturbed by a connection an earlier subtest left open, and
// no test reaches api.openai.com or api.novita.ai. The SDK retries are off so a
// failing upstream costs one request.

const (
	openaiCompletion = `{"id":"chatcmpl-1","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"Hello from OpenAI"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`

	openaiHelloChunk  = `data: {"id":"chatcmpl-1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"role":"assistant","content":"Hello "},"finish_reason":null}]}` + "\n\n"
	openaiRestChunk   = `data: {"id":"chatcmpl-1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"content":"from OpenAI"},"finish_reason":null}]}` + "\n\n"
	openaiFinishChunk = `data: {"id":"chatcmpl-1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}` + "\n\n"
	openaiUsageChunk  = `data: {"id":"chatcmpl-1","object":"chat.completion.chunk","created":1,"model":"m","choices":[],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}` + "\n\n"
	openaiDoneLine    = "data: [DONE]\n\n"
)

// openaiProvider builds an OpenAI-compatible provider whose upstream answers with
// respond. The base URL ends in /v1 as the registry default does.
func openaiProvider(t *testing.T, respond func(llmtest.Request) llmtest.Reply) llmhub.Provider {
	t.Helper()
	up := llmtest.NewUpstream(t, respond)
	p, err := openaicompat.New(llmhub.ProviderConfig{APIKey: "k", BaseURL: up.URL + "/v1"},
		openaicompat.WithHTTPClient(up.Client()), openaicompat.WithMaxRetries(0))
	if err != nil {
		t.Fatalf("openaicompat.New: %v", err)
	}
	return p
}

func TestProviderOpenAICompatible(t *testing.T) {
	conformance.Provider(t,
		func(t *testing.T) llmhub.Provider {
			return openaiProvider(t, func(r llmtest.Request) llmtest.Reply {
				if isStreamRequest(r) {
					return llmtest.Reply{
						Header: map[string]string{"Content-Type": "text/event-stream"},
						Chunks: []string{openaiHelloChunk, openaiRestChunk, openaiFinishChunk, openaiUsageChunk, openaiDoneLine},
					}
				}
				return llmtest.Reply{Header: map[string]string{"Content-Type": "application/json"}, Chunks: []string{openaiCompletion}}
			})
		},
		conformance.ProviderOptions{
			// 401 is not retried by the SDK and costs one request, for Chat and Stream.
			NewFailing: func(t *testing.T) llmhub.Provider {
				return openaiProvider(t, func(llmtest.Request) llmtest.Reply {
					return llmtest.Reply{
						Status: 401,
						Header: map[string]string{"Content-Type": "application/json"},
						Chunks: []string{`{"error":{"message":"Incorrect API key provided","type":"invalid_request_error","param":null,"code":"invalid_api_key"}}`},
					}
				})
			},
			// One text delta, then the upstream stops writing until the request ends.
			NewBlocking: func(t *testing.T) llmhub.Provider {
				return openaiProvider(t, func(llmtest.Request) llmtest.Reply {
					return llmtest.Reply{
						Header: map[string]string{"Content-Type": "text/event-stream"},
						Chunks: []string{openaiHelloChunk},
						Hang:   true,
					}
				})
			},
		})
}
