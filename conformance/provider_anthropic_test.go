package conformance_test

import (
	"bytes"
	"testing"

	"github.com/Phong95/aec-agent-go/components/llmhub"
	"github.com/Phong95/aec-agent-go/components/llmhub/providers/anthropic"
	"github.com/Phong95/aec-agent-go/conformance"
	"github.com/Phong95/aec-agent-go/internal/llmtest"
)

// The Provider suite against the real Anthropic provider (Phase 8, LLM-07). Every
// provider talks to a loopback httptest upstream: a factory call starts a fresh
// upstream (closed when the subtest ends), so the break-early subtest's goroutine
// count is not disturbed by a connection an earlier subtest left open, and no test
// reaches api.anthropic.com. The SDK retries are off so a failing upstream costs one
// request.

const (
	anthropicMessage = `{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"text","text":"Hello from Anthropic"}],"model":"claude-test","stop_reason":"end_turn","stop_sequence":null,"usage":{"input_tokens":10,"output_tokens":5}}`

	anthropicStartEvent = "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"type\":\"message\",\"role\":\"assistant\",\"content\":[],\"model\":\"claude-test\",\"stop_reason\":null,\"stop_sequence\":null,\"usage\":{\"input_tokens\":10,\"output_tokens\":1}}}\n\n"
	anthropicTextStart  = "event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n"
	anthropicTextHello  = "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"Hello \"}}\n\n"
	anthropicTextRest   = "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"from Anthropic\"}}\n\n"
	anthropicTextStop   = "event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n"
	anthropicDelta      = "event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\",\"stop_sequence\":null},\"usage\":{\"output_tokens\":5}}\n\n"
	anthropicStop       = "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
)

// anthropicProvider builds an Anthropic provider whose upstream answers with
// respond.
func anthropicProvider(t *testing.T, respond func(llmtest.Request) llmtest.Reply) llmhub.Provider {
	t.Helper()
	up := llmtest.NewUpstream(t, respond)
	p, err := anthropic.New(llmhub.ProviderConfig{APIKey: "k", BaseURL: up.URL},
		anthropic.WithHTTPClient(up.Client()), anthropic.WithMaxRetries(0))
	if err != nil {
		t.Fatalf("anthropic.New: %v", err)
	}
	return p
}

// isStreamRequest reports whether the request asks for an event stream.
func isStreamRequest(r llmtest.Request) bool {
	return bytes.Contains(r.Body, []byte(`"stream":true`))
}

func TestProviderAnthropic(t *testing.T) {
	conformance.Provider(t,
		func(t *testing.T) llmhub.Provider {
			return anthropicProvider(t, func(r llmtest.Request) llmtest.Reply {
				if isStreamRequest(r) {
					return llmtest.Reply{
						Header: map[string]string{"Content-Type": "text/event-stream"},
						Chunks: []string{anthropicStartEvent, anthropicTextStart, anthropicTextHello, anthropicTextRest, anthropicTextStop, anthropicDelta, anthropicStop},
					}
				}
				return llmtest.Reply{Header: map[string]string{"Content-Type": "application/json"}, Chunks: []string{anthropicMessage}}
			})
		},
		conformance.ProviderOptions{
			// 401 is not retried by the SDK and costs one request, for Chat and Stream.
			NewFailing: func(t *testing.T) llmhub.Provider {
				return anthropicProvider(t, func(llmtest.Request) llmtest.Reply {
					return llmtest.Reply{
						Status: 401,
						Header: map[string]string{"Content-Type": "application/json"},
						Chunks: []string{`{"type":"error","error":{"type":"authentication_error","message":"invalid x-api-key"}}`},
					}
				})
			},
			// One text delta, then the upstream stops writing until the request ends.
			NewBlocking: func(t *testing.T) llmhub.Provider {
				return anthropicProvider(t, func(llmtest.Request) llmtest.Reply {
					return llmtest.Reply{
						Header: map[string]string{"Content-Type": "text/event-stream"},
						Chunks: []string{anthropicStartEvent, anthropicTextStart, anthropicTextHello},
						Hang:   true,
					}
				})
			},
		})
}
