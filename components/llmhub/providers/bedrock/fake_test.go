package bedrock

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/bedrock"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime/types"
)

// chanStream is an EventStream over a channel: a goroutine feeds the scripted
// events and closes the channel (or, with hang, keeps the channel open until the
// stream is closed). Close unblocks a pending reader, is safe to call twice and
// concurrently, and returns only when the feeding goroutine has ended, so a test
// that forgets nothing leaves no goroutine behind (the SDK documents the same
// requirement for a real reader).
type chanStream struct {
	events chan types.ConverseStreamOutput
	done   chan struct{}
	once   sync.Once
	wg     sync.WaitGroup
	err    error
}

// newChanStream starts a stream that delivers evs, then ends with err (nil for a
// clean end). With hang the stream delivers evs and then stays open until Close.
func newChanStream(evs []types.ConverseStreamOutput, err error, hang bool) *chanStream {
	s := &chanStream{
		events: make(chan types.ConverseStreamOutput),
		done:   make(chan struct{}),
		err:    err,
	}
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

func (s *chanStream) Events() <-chan types.ConverseStreamOutput { return s.events }

func (s *chanStream) Close() error {
	s.once.Do(func() { close(s.done) })
	s.wg.Wait()
	return nil
}

func (s *chanStream) Err() error { return s.err }

// fakeAPI is a scripted ConverseAPI. It records every input.
type fakeAPI struct {
	converse func(context.Context, *bedrockruntime.ConverseInput) (*bedrockruntime.ConverseOutput, error)
	stream   func(context.Context, *bedrockruntime.ConverseStreamInput) (EventStream, error)

	mu            sync.Mutex
	converseCalls []*bedrockruntime.ConverseInput
	streamCalls   []*bedrockruntime.ConverseStreamInput
}

func (f *fakeAPI) Converse(ctx context.Context, in *bedrockruntime.ConverseInput) (*bedrockruntime.ConverseOutput, error) {
	f.mu.Lock()
	f.converseCalls = append(f.converseCalls, in)
	f.mu.Unlock()
	if f.converse == nil {
		return nil, errors.New("fakeAPI: no Converse script")
	}
	return f.converse(ctx, in)
}

func (f *fakeAPI) ConverseStream(ctx context.Context, in *bedrockruntime.ConverseStreamInput) (EventStream, error) {
	f.mu.Lock()
	f.streamCalls = append(f.streamCalls, in)
	f.mu.Unlock()
	if f.stream == nil {
		return nil, errors.New("fakeAPI: no ConverseStream script")
	}
	return f.stream(ctx, in)
}

// streamOf returns a fakeAPI whose ConverseStream delivers evs and then ends with
// err.
func streamOf(evs []types.ConverseStreamOutput, err error) *fakeAPI {
	return &fakeAPI{stream: func(context.Context, *bedrockruntime.ConverseStreamInput) (EventStream, error) {
		return newChanStream(evs, err, false), nil
	}}
}

// fakeModels is a scripted ModelsAPI.
type fakeModels struct {
	list func(context.Context, *bedrock.ListFoundationModelsInput) (*bedrock.ListFoundationModelsOutput, error)
}

func (f fakeModels) ListFoundationModels(ctx context.Context, in *bedrock.ListFoundationModelsInput) (*bedrock.ListFoundationModelsOutput, error) {
	return f.list(ctx, in)
}

// newFakeProvider builds a provider over api for us-east-1.
func newFakeProvider(t *testing.T, api ConverseAPI, opts ...Option) *Provider {
	t.Helper()
	all := append([]Option{WithConverseAPI(api)}, opts...)
	p, err := New(providerConfig(), all...)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return p
}

// fixtureEvent is one SDK-shaped event object of a wiregen transcript: exactly one
// member is set.
type fixtureEvent struct {
	MessageStart      *struct{ Role string } `json:"messageStart"`
	ContentBlockStart *struct {
		ContentBlockIndex *int32 `json:"contentBlockIndex"`
		Start             struct {
			ToolUse *struct {
				ToolUseID *string `json:"toolUseId"`
				Name      *string `json:"name"`
			} `json:"toolUse"`
		} `json:"start"`
	} `json:"contentBlockStart"`
	ContentBlockDelta *struct {
		ContentBlockIndex *int32 `json:"contentBlockIndex"`
		Delta             struct {
			Text             *string                  `json:"text"`
			ToolUse          *struct{ Input *string } `json:"toolUse"`
			ReasoningContent *struct{ Text string }   `json:"reasoningContent"`
		} `json:"delta"`
	} `json:"contentBlockDelta"`
	ContentBlockStop *struct {
		ContentBlockIndex *int32 `json:"contentBlockIndex"`
	} `json:"contentBlockStop"`
	MessageStop *struct {
		StopReason string `json:"stopReason"`
	} `json:"messageStop"`
	Metadata *struct {
		Usage *struct {
			InputTokens  *int32 `json:"inputTokens"`
			OutputTokens *int32 `json:"outputTokens"`
		} `json:"usage"`
	} `json:"metadata"`
}

// eventsFromFixture converts the SDK-shaped JS objects of a transcript into the
// union members the AWS stream delivers.
func eventsFromFixture(t *testing.T, raw []json.RawMessage) []types.ConverseStreamOutput {
	t.Helper()
	out := make([]types.ConverseStreamOutput, 0, len(raw))
	for i, r := range raw {
		var e fixtureEvent
		if err := json.Unmarshal(r, &e); err != nil {
			t.Fatalf("event %d: %v", i, err)
		}
		switch {
		case e.MessageStart != nil:
			out = append(out, &types.ConverseStreamOutputMemberMessageStart{
				Value: types.MessageStartEvent{Role: types.ConversationRole(e.MessageStart.Role)},
			})
		case e.ContentBlockStart != nil:
			ev := types.ContentBlockStartEvent{ContentBlockIndex: e.ContentBlockStart.ContentBlockIndex}
			if tu := e.ContentBlockStart.Start.ToolUse; tu != nil {
				ev.Start = &types.ContentBlockStartMemberToolUse{Value: types.ToolUseBlockStart{ToolUseId: tu.ToolUseID, Name: tu.Name}}
			}
			out = append(out, &types.ConverseStreamOutputMemberContentBlockStart{Value: ev})
		case e.ContentBlockDelta != nil:
			ev := types.ContentBlockDeltaEvent{ContentBlockIndex: e.ContentBlockDelta.ContentBlockIndex}
			d := e.ContentBlockDelta.Delta
			switch {
			case d.Text != nil:
				ev.Delta = &types.ContentBlockDeltaMemberText{Value: *d.Text}
			case d.ToolUse != nil:
				ev.Delta = &types.ContentBlockDeltaMemberToolUse{Value: types.ToolUseBlockDelta{Input: d.ToolUse.Input}}
			case d.ReasoningContent != nil:
				ev.Delta = &types.ContentBlockDeltaMemberReasoningContent{
					Value: &types.ReasoningContentBlockDeltaMemberText{Value: d.ReasoningContent.Text},
				}
			}
			out = append(out, &types.ConverseStreamOutputMemberContentBlockDelta{Value: ev})
		case e.ContentBlockStop != nil:
			out = append(out, &types.ConverseStreamOutputMemberContentBlockStop{
				Value: types.ContentBlockStopEvent{ContentBlockIndex: e.ContentBlockStop.ContentBlockIndex},
			})
		case e.MessageStop != nil:
			out = append(out, &types.ConverseStreamOutputMemberMessageStop{
				Value: types.MessageStopEvent{StopReason: types.StopReason(e.MessageStop.StopReason)},
			})
		case e.Metadata != nil:
			ev := types.ConverseStreamMetadataEvent{}
			if u := e.Metadata.Usage; u != nil {
				ev.Usage = &types.TokenUsage{InputTokens: u.InputTokens, OutputTokens: u.OutputTokens}
			}
			out = append(out, &types.ConverseStreamOutputMemberMetadata{Value: ev})
		default:
			t.Fatalf("event %d has no known member: %s", i, r)
		}
	}
	return out
}
