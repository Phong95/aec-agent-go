package conformance_test

import (
	"context"
	"sync"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime/types"
	"github.com/aws/smithy-go"

	"github.com/Phong95/aec-agent-go/components/llmhub"
	"github.com/Phong95/aec-agent-go/components/llmhub/providers/bedrock"
	"github.com/Phong95/aec-agent-go/conformance"
)

// The Provider suite against the real Bedrock provider (Phase 8, LLM-04 and LLM-07)
// on an INJECTED client: bedrock.WithConverseAPI replaces the AWS runtime with a fake
// that returns the answers below, so no request is made to AWS and no credential is
// used. (The wire path of the real client, with the eventstream framing, the error
// documents and cancellation, is covered in the llm/bedrock package against an
// httptest server.) The exported seam follows sandbox/docker.Runner: this runner is
// the reason the seam is exported, so that go test ./conformance/... covers Bedrock.

// converseFake is a bedrock.ConverseAPI with scripted answers.
type converseFake struct {
	converse func(context.Context) (*bedrockruntime.ConverseOutput, error)
	stream   func(context.Context) (bedrock.EventStream, error)
}

func (f converseFake) Converse(ctx context.Context, _ *bedrockruntime.ConverseInput) (*bedrockruntime.ConverseOutput, error) {
	return f.converse(ctx)
}

func (f converseFake) ConverseStream(ctx context.Context, _ *bedrockruntime.ConverseStreamInput) (bedrock.EventStream, error) {
	return f.stream(ctx)
}

// eventChannel is a bedrock.EventStream over a channel. A goroutine delivers the
// events and then closes the channel; with hang it keeps the channel open until
// Close, as a stream that stops in the middle does. Close unblocks a pending reader,
// may be called twice and concurrently, and returns when the goroutine has ended.
type eventChannel struct {
	events chan types.ConverseStreamOutput
	done   chan struct{}
	once   sync.Once
	wg     sync.WaitGroup
}

func newEventChannel(evs []types.ConverseStreamOutput, hang bool) *eventChannel {
	s := &eventChannel{events: make(chan types.ConverseStreamOutput), done: make(chan struct{})}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		for _, ev := range evs {
			select {
			case s.events <- ev:
			case <-s.done:
				return
			}
		}
		if hang {
			<-s.done
			return
		}
		close(s.events)
	}()
	return s
}

func (s *eventChannel) Events() <-chan types.ConverseStreamOutput { return s.events }

func (s *eventChannel) Close() error {
	s.once.Do(func() { close(s.done) })
	s.wg.Wait()
	return nil
}

func (*eventChannel) Err() error { return nil }

// textDelta is a contentBlockDelta event with text.
func textDelta(text string) types.ConverseStreamOutput {
	return &types.ConverseStreamOutputMemberContentBlockDelta{Value: types.ContentBlockDeltaEvent{
		ContentBlockIndex: aws.Int32(0),
		Delta:             &types.ContentBlockDeltaMemberText{Value: text},
	}}
}

// bedrockProvider builds a Bedrock provider over the fake. It reads no environment
// variable and makes no request: the runtime is the fake, and the control-plane
// client New builds is never called.
func bedrockProvider(t *testing.T, fake converseFake) llmhub.Provider {
	t.Helper()
	p, err := bedrock.New(
		llmhub.ProviderConfig{APIKey: "AKIA-TEST", SecretKey: "secret-test", Region: "us-east-1"},
		bedrock.WithConverseAPI(fake),
	)
	if err != nil {
		t.Fatalf("bedrock.New: %v", err)
	}
	return p
}

func TestProviderBedrock(t *testing.T) {
	conformance.Provider(t,
		func(t *testing.T) llmhub.Provider {
			return bedrockProvider(t, converseFake{
				// The answer of Converse: one assistant text message, end_turn, usage.
				converse: func(context.Context) (*bedrockruntime.ConverseOutput, error) {
					return &bedrockruntime.ConverseOutput{
						Output: &types.ConverseOutputMemberMessage{Value: types.Message{
							Role:    types.ConversationRoleAssistant,
							Content: []types.ContentBlock{&types.ContentBlockMemberText{Value: "Hello from Bedrock"}},
						}},
						StopReason: types.StopReasonEndTurn,
						Usage:      &types.TokenUsage{InputTokens: aws.Int32(10), OutputTokens: aws.Int32(5), TotalTokens: aws.Int32(15)},
					}, nil
				},
				// The stream: non-empty text deltas, a metadata event with usage, then
				// the end of the channel.
				stream: func(context.Context) (bedrock.EventStream, error) {
					return newEventChannel([]types.ConverseStreamOutput{
						textDelta("Hello "),
						textDelta("from Bedrock"),
						&types.ConverseStreamOutputMemberMetadata{Value: types.ConverseStreamMetadataEvent{
							Usage: &types.TokenUsage{InputTokens: aws.Int32(10), OutputTokens: aws.Int32(5), TotalTokens: aws.Int32(15)},
						}},
					}, false), nil
				},
			})
		},
		conformance.ProviderOptions{
			// AccessDeniedException: an auth ModelError from Chat and one in-band error
			// chunk from Stream, with no retry (the call is made once).
			NewFailing: func(t *testing.T) llmhub.Provider {
				denied := &smithy.GenericAPIError{Code: "AccessDeniedException", Message: "not authorized to perform bedrock:InvokeModel", Fault: smithy.FaultClient}
				return bedrockProvider(t, converseFake{
					converse: func(context.Context) (*bedrockruntime.ConverseOutput, error) { return nil, denied },
					stream:   func(context.Context) (bedrock.EventStream, error) { return nil, denied },
				})
			},
			// One text delta, then the stream stays open until it is closed or the
			// caller's context ends.
			NewBlocking: func(t *testing.T) llmhub.Provider {
				return bedrockProvider(t, converseFake{
					converse: func(ctx context.Context) (*bedrockruntime.ConverseOutput, error) {
						<-ctx.Done()
						return nil, ctx.Err()
					},
					stream: func(context.Context) (bedrock.EventStream, error) {
						return newEventChannel([]types.ConverseStreamOutput{textDelta("Hello ")}, true), nil
					},
				})
			},
		})
}
