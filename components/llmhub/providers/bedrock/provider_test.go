package bedrock

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime/types"

	"github.com/Phong95/aec-agent-go/components/llmhub"
	"github.com/Phong95/aec-agent-go/internal/llmtest"
	"github.com/Phong95/aec-agent-go/internal/llmtransport"
	"github.com/Phong95/aec-agent-go/kernel"
)

// okOutput is a Converse answer that lets a call finish.
func okOutput() *bedrockruntime.ConverseOutput {
	return &bedrockruntime.ConverseOutput{
		Output: &types.ConverseOutputMemberMessage{Value: types.Message{
			Role:    types.ConversationRoleAssistant,
			Content: []types.ContentBlock{&types.ContentBlockMemberText{Value: "ok"}},
		}},
		StopReason: types.StopReasonEndTurn,
		Usage:      &types.TokenUsage{InputTokens: aws.Int32(1), OutputTokens: aws.Int32(1)},
	}
}

// captureConverse returns a fake whose Converse answers okOutput and a function
// that returns the input of the one call it saw.
func captureConverse(t *testing.T) (*fakeAPI, func() *bedrockruntime.ConverseInput) {
	t.Helper()
	api := &fakeAPI{converse: func(context.Context, *bedrockruntime.ConverseInput) (*bedrockruntime.ConverseOutput, error) {
		return okOutput(), nil
	}}
	return api, func() *bedrockruntime.ConverseInput {
		t.Helper()
		api.mu.Lock()
		defer api.mu.Unlock()
		if len(api.converseCalls) != 1 {
			t.Fatalf("Converse was called %d times, want 1", len(api.converseCalls))
		}
		return api.converseCalls[0]
	}
}

// assertNoEmptyTextBlocks checks the empty-content guard: no message
// has an empty content list, and no text block, in a message or in a tool result,
// is empty.
func assertNoEmptyTextBlocks(t *testing.T, msgs []types.Message) {
	t.Helper()
	for i, msg := range msgs {
		if len(msg.Content) == 0 {
			t.Errorf("message %d has an empty content array", i)
		}
		for _, block := range msg.Content {
			switch b := block.(type) {
			case *types.ContentBlockMemberText:
				if b.Value == "" {
					t.Errorf("message %d has an empty text block", i)
				}
			case *types.ContentBlockMemberToolResult:
				for _, tr := range b.Value.Content {
					if text, ok := tr.(*types.ToolResultContentBlockMemberText); ok && text.Value == "" {
						t.Errorf("message %d has an empty text block in a tool result", i)
					}
				}
			}
		}
	}
}

// TestBedrockMapMessagesEmptyContentGuard covers four empty-content cases as named
// subtests. Bedrock's Converse API rejects any content block shaped {text: ""}:
// mapMessages must never emit one, whichever Message field was empty (assistant
// content, tool result content, or an assistant message with neither text nor tool
// calls).
func TestBedrockMapMessagesEmptyContentGuard(t *testing.T) {
	t.Run("drops an assistant message that has empty content and no toolCalls (never sends {text: ''})", func(t *testing.T) {
		api, input := captureConverse(t)
		p := newFakeProvider(t, api)
		if _, err := p.Chat(t.Context(), []kernel.Message{
			{Role: kernel.RoleUser, Content: "hi"},
			{Role: kernel.RoleAssistant, Content: ""},
		}, llmhub.ModelOptions{}); err != nil {
			t.Fatalf("Chat: %v", err)
		}
		in := input()
		assertNoEmptyTextBlocks(t, in.Messages)
		// The empty assistant message must be dropped entirely, not sent as an empty
		// content array (Bedrock also rejects empty content arrays).
		if len(in.Messages) != 1 {
			t.Errorf("got %d messages, want 1", len(in.Messages))
		}
	})

	t.Run("falls back to a placeholder for an empty tool result instead of {text: ''}", func(t *testing.T) {
		api, input := captureConverse(t)
		p := newFakeProvider(t, api)
		f := false
		if _, err := p.Chat(t.Context(), []kernel.Message{{
			Role:        kernel.RoleTool,
			Content:     "",
			ToolResults: []kernel.ToolResult{{ToolCallID: "t1", Content: "", IsError: &f}},
		}}, llmhub.ModelOptions{}); err != nil {
			t.Fatalf("Chat: %v", err)
		}
		in := input()
		assertNoEmptyTextBlocks(t, in.Messages)
		result := in.Messages[0].Content[0].(*types.ContentBlockMemberToolResult).Value
		if text := result.Content[0].(*types.ToolResultContentBlockMemberText).Value; text != "[no output]" {
			t.Errorf("the tool result text is %q, want [no output]", text)
		}
	})

	t.Run("keeps assistant messages that have toolCalls but empty text (no dangling {text: ''})", func(t *testing.T) {
		api, input := captureConverse(t)
		p := newFakeProvider(t, api)
		if _, err := p.Chat(t.Context(), []kernel.Message{
			{Role: kernel.RoleUser, Content: "hi"},
			{Role: kernel.RoleAssistant, Content: "", ToolCalls: []kernel.ToolCall{{ID: "call1", Name: "search", Arguments: kernel.Arguments(`{"q":"x"}`)}}},
		}, llmhub.ModelOptions{}); err != nil {
			t.Fatalf("Chat: %v", err)
		}
		in := input()
		assertNoEmptyTextBlocks(t, in.Messages)
		var assistant *types.Message
		for i := range in.Messages {
			if in.Messages[i].Role == types.ConversationRoleAssistant {
				assistant = &in.Messages[i]
			}
		}
		if assistant == nil || len(assistant.Content) != 1 {
			t.Fatalf("the assistant message is %+v, want one content block", assistant)
		}
		if _, ok := assistant.Content[0].(*types.ContentBlockMemberToolUse); !ok {
			t.Errorf("the block is %T, want a toolUse", assistant.Content[0])
		}
	})

	t.Run("skips empty-content user and system messages instead of emitting {text: ''}", func(t *testing.T) {
		api, input := captureConverse(t)
		p := newFakeProvider(t, api)
		if _, err := p.Chat(t.Context(), []kernel.Message{
			{Role: kernel.RoleSystem, Content: ""},
			{Role: kernel.RoleUser, Content: ""},
			{Role: kernel.RoleUser, Content: "actual question"},
		}, llmhub.ModelOptions{}); err != nil {
			t.Fatalf("Chat: %v", err)
		}
		in := input()
		assertNoEmptyTextBlocks(t, in.Messages)
		if len(in.Messages) != 1 {
			t.Errorf("got %d messages, want 1", len(in.Messages))
		}
		if in.System != nil {
			t.Errorf("system is %v, want absent", in.System)
		}
	})
}

// TestProvidersKeepTheResponseAliveOnInvalidToolArguments covers the Bedrock case of
// invalid tool arguments: a tool call whose
// streamed arguments are not valid JSON becomes a call with empty arguments and an
// ArgumentsError, and the stream ends with done.
func TestProvidersKeepTheResponseAliveOnInvalidToolArguments(t *testing.T) {
	t.Run("Bedrock", func(t *testing.T) {
		const badJSON = `{"path": "notes/report.md", "content": "He said "hi""}`
		api := streamOf(eventsOf(
			&types.ConverseStreamOutputMemberMessageStart{Value: types.MessageStartEvent{Role: types.ConversationRoleAssistant}},
			startEvent(0, "tooluse_1", "write_file"),
			deltaEvent(0, badJSON),
			stopEvent(0),
			&types.ConverseStreamOutputMemberMessageStop{Value: types.MessageStopEvent{StopReason: types.StopReasonToolUse}},
		), nil)
		p := newFakeProvider(t, api)
		chunks := drain(t, p, []kernel.Message{{Role: kernel.RoleUser, Content: "write the report"}}, llmhub.ModelOptions{})
		for _, c := range chunks {
			if c.Type == kernel.ChunkError {
				t.Fatalf("the stream carried an error chunk: %s", c.Error)
			}
		}
		if last := chunks[len(chunks)-1]; last.Type != kernel.ChunkDone {
			t.Errorf("the last chunk is %q, want done", last.Type)
		}
		var call *kernel.ToolCall
		for _, c := range chunks {
			if c.Type == kernel.ChunkToolCall {
				call = c.ToolCall
			}
		}
		if call == nil {
			t.Fatal("no tool_call chunk")
		}
		if call.ID != "tooluse_1" || call.Name != "write_file" {
			t.Errorf("the call is %s %s, want tooluse_1 write_file", call.ID, call.Name)
		}
		if len(call.Arguments) != 0 {
			t.Errorf("arguments %s, want none (encoded as {})", call.Arguments)
		}
		if got := call.ArgumentsError; len(got) < 30 || got[:30] != "the arguments are not valid JS" {
			t.Errorf("ArgumentsError %q does not say the arguments are not valid JSON", got)
		}
	})
}

// TestBedrockKeepsOneArgumentBufferPerContentBlockIndex is the regression test
// "keeps one argument buffer per contentBlockIndex" (Bedrock tool calls with
// interleaved content blocks): two tool-call blocks whose deltas
// interleave each keep their own arguments.
func TestBedrockKeepsOneArgumentBufferPerContentBlockIndex(t *testing.T) {
	t.Run("keeps one argument buffer per contentBlockIndex", func(t *testing.T) {
		api := streamOf(eventsOf(
			&types.ConverseStreamOutputMemberMessageStart{Value: types.MessageStartEvent{Role: types.ConversationRoleAssistant}},
			startEvent(1, "t_a", "write_file"),
			startEvent(2, "t_b", "write_file"),
			deltaEvent(1, `{"path": "a.md", `),
			deltaEvent(2, `{"path": "b.md", `),
			deltaEvent(1, `"content": "A"}`),
			deltaEvent(2, `"content": "B"}`),
			stopEvent(1),
			stopEvent(2),
			&types.ConverseStreamOutputMemberMessageStop{Value: types.MessageStopEvent{StopReason: types.StopReasonToolUse}},
		), nil)
		p := newFakeProvider(t, api)
		var calls []string
		for _, c := range drain(t, p, []kernel.Message{{Role: kernel.RoleUser, Content: "write the report"}}, llmhub.ModelOptions{}) {
			if c.Type == kernel.ChunkToolCall {
				b, err := kernel.EncodeJSON(c.ToolCall)
				if err != nil {
					t.Fatal(err)
				}
				calls = append(calls, string(b))
			}
		}
		want := []string{
			`{"id":"t_a","name":"write_file","arguments":{"path":"a.md","content":"A"}}`,
			`{"id":"t_b","name":"write_file","arguments":{"path":"b.md","content":"B"}}`,
		}
		if !slices.Equal(calls, want) {
			t.Errorf("calls differ\n got: %v\nwant: %v", calls, want)
		}
	})
}

// eventsOf is a list of events.
func eventsOf(evs ...types.ConverseStreamOutput) []types.ConverseStreamOutput { return evs }

func startEvent(index int32, id, name string) types.ConverseStreamOutput {
	return &types.ConverseStreamOutputMemberContentBlockStart{Value: types.ContentBlockStartEvent{
		ContentBlockIndex: aws.Int32(index),
		Start:             &types.ContentBlockStartMemberToolUse{Value: types.ToolUseBlockStart{ToolUseId: aws.String(id), Name: aws.String(name)}},
	}}
}

func deltaEvent(index int32, input string) types.ConverseStreamOutput {
	return &types.ConverseStreamOutputMemberContentBlockDelta{Value: types.ContentBlockDeltaEvent{
		ContentBlockIndex: aws.Int32(index),
		Delta:             &types.ContentBlockDeltaMemberToolUse{Value: types.ToolUseBlockDelta{Input: aws.String(input)}},
	}}
}

func textEvent(index int32, text string) types.ConverseStreamOutput {
	return &types.ConverseStreamOutputMemberContentBlockDelta{Value: types.ContentBlockDeltaEvent{
		ContentBlockIndex: aws.Int32(index),
		Delta:             &types.ContentBlockDeltaMemberText{Value: text},
	}}
}

func stopEvent(index int32) types.ConverseStreamOutput {
	return &types.ConverseStreamOutputMemberContentBlockStop{Value: types.ContentBlockStopEvent{ContentBlockIndex: aws.Int32(index)}}
}

// TestBedrockDefaults checks what New sets: the name, the version, the region
// default and the default model with the region prefix, and the options.
func TestBedrockDefaults(t *testing.T) {
	cases := []struct {
		region, model, wantModel string
	}{
		{"", "", "us.anthropic.claude-sonnet-4-20250514-v1:0"},
		{"us-east-1", "", "us.anthropic.claude-sonnet-4-20250514-v1:0"},
		{"eu-west-1", "", "eu.anthropic.claude-sonnet-4-20250514-v1:0"},
		// apac. for an ap- region, us-gov. for a
		// us-gov- region, and no prefix where no inference profile prefix is known.
		{"ap-southeast-2", "", "apac.anthropic.claude-sonnet-4-20250514-v1:0"},
		{"us-gov-west-1", "", "us-gov.anthropic.claude-sonnet-4-20250514-v1:0"},
		{"ca-central-1", "", "anthropic.claude-sonnet-4-20250514-v1:0"},
		{"US-EAST-1", "", "us.anthropic.claude-sonnet-4-20250514-v1:0"},
		{"eu-west-1", "anthropic.claude-3-haiku-20240307-v1:0", "eu.anthropic.claude-3-haiku-20240307-v1:0"},
	}
	for _, c := range cases {
		api, input := captureConverse(t)
		cfg := providerConfig()
		cfg.Region = c.region
		cfg.Model = c.model
		p, err := New(cfg, WithConverseAPI(api), WithModelsAPI(fakeModels{}))
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		if _, err := p.Chat(t.Context(), []kernel.Message{{Role: kernel.RoleUser, Content: "x"}}, llmhub.ModelOptions{}); err != nil {
			t.Fatalf("Chat: %v", err)
		}
		if got := aws.ToString(input().ModelId); got != c.wantModel {
			t.Errorf("region %q model %q: the model id is %q, want %q", c.region, c.model, got, c.wantModel)
		}
	}

	p := newFakeProvider(t, &fakeAPI{}, WithModelsAPI(fakeModels{}))
	if p.Name() != "bedrock" || p.Version() != "1.0.0" {
		t.Errorf("name %q version %q, want bedrock 1.0.0", p.Name(), p.Version())
	}
	if named := newFakeProvider(t, &fakeAPI{}, WithName("mine")); named.Name() != "mine" {
		t.Errorf("WithName gave %q", named.Name())
	}
	if err := p.Initialize(t.Context()); err != nil {
		t.Errorf("Initialize: %v", err)
	}
	if err := p.Dispose(t.Context()); err != nil {
		t.Errorf("Dispose: %v", err)
	}
}

// TestBedrockRegistered requires the factory "bedrock" in the default registry and
// that it builds a provider from static credentials (no network call).
func TestBedrockRegistered(t *testing.T) {
	if !slices.Contains(llmhub.ListProviders(), "bedrock") {
		t.Fatalf("bedrock is not registered: %v", llmhub.ListProviders())
	}
	p, err := llmhub.ResolveProvider("bedrock", providerConfig())
	if err != nil {
		t.Fatalf("ResolveProvider: %v", err)
	}
	if p.Name() != "bedrock" {
		t.Errorf("name %q", p.Name())
	}
	if _, ok := p.(llmhub.ModelLister); !ok {
		t.Error("the provider is not an llmhub.ModelLister")
	}
	// An empty secret key is accepted.
	if _, err := llmhub.ResolveProvider("bedrock", llmhub.ProviderConfig{APIKey: "AKIA-TEST"}); err != nil {
		t.Errorf("an empty secret key was refused: %v", err)
	}
}

// TestBedrockCountTokens requires the shared estimate and the bare ctx.Err() for a
// done context.
func TestBedrockCountTokens(t *testing.T) {
	p := newFakeProvider(t, &fakeAPI{})
	msgs := []kernel.Message{{Role: kernel.RoleUser, Content: "hello world"}}
	got, err := p.CountTokens(t.Context(), msgs)
	if err != nil || got != llmhub.EstimateTokens(msgs) {
		t.Errorf("CountTokens = %d, %v, want %d", got, err, llmhub.EstimateTokens(msgs))
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := p.CountTokens(ctx, msgs); !errors.Is(err, context.Canceled) {
		t.Errorf("a done context gave %v, want context.Canceled", err)
	}
}

// TestBedrockContextHandling checks the llmhub.Provider cancellation rules on the
// injected seam: a done context makes no call, and an error that arrives with a done
// context is the bare ctx.Err(), never a model failure.
func TestBedrockContextHandling(t *testing.T) {
	t.Run("a done context makes no call", func(t *testing.T) {
		api := &fakeAPI{}
		p := newFakeProvider(t, api)
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		if _, err := p.Chat(ctx, nil, llmhub.ModelOptions{}); !errors.Is(err, context.Canceled) {
			t.Errorf("Chat gave %v, want context.Canceled", err)
		}
		var yields int
		for c, err := range p.Stream(ctx, nil, llmhub.ModelOptions{}) {
			yields++
			if !errors.Is(err, context.Canceled) || c.Type != "" {
				t.Errorf("yield %d: chunk %v error %v", yields, c, err)
			}
		}
		if yields != 1 {
			t.Errorf("Stream yielded %d times, want 1", yields)
		}
		if _, err := p.ListModels(ctx); !errors.Is(err, context.Canceled) {
			t.Errorf("ListModels gave %v, want context.Canceled", err)
		}
		if len(api.converseCalls)+len(api.streamCalls) != 0 {
			t.Error("the API was called with a done context")
		}
	})

	t.Run("an error that arrives with a done context is the bare ctx.Err()", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		api := &fakeAPI{
			converse: func(context.Context, *bedrockruntime.ConverseInput) (*bedrockruntime.ConverseOutput, error) {
				cancel()
				return nil, awsError("ThrottlingException", "slow", 429)
			},
			stream: func(context.Context, *bedrockruntime.ConverseStreamInput) (EventStream, error) {
				cancel()
				return nil, awsError("ThrottlingException", "slow", 429)
			},
		}
		p := newFakeProvider(t, api)
		if _, err := p.Chat(ctx, nil, llmhub.ModelOptions{}); !errors.Is(err, context.Canceled) {
			t.Errorf("Chat gave %v, want context.Canceled", err)
		}
		ctx2, cancel2 := context.WithCancel(t.Context())
		api.stream = func(context.Context, *bedrockruntime.ConverseStreamInput) (EventStream, error) {
			cancel2()
			return nil, awsError("ThrottlingException", "slow", 429)
		}
		for c, err := range p.Stream(ctx2, nil, llmhub.ModelOptions{}) {
			if !errors.Is(err, context.Canceled) || c.Type != "" {
				t.Errorf("chunk %v error %v, want only context.Canceled", c, err)
			}
		}
	})

	t.Run("cancelling in the middle of a stream ends it at once and closes the stream", func(t *testing.T) {
		var stream *chanStream
		api := &fakeAPI{stream: func(context.Context, *bedrockruntime.ConverseStreamInput) (EventStream, error) {
			stream = newChanStream(eventsOf(textEvent(0, "first")), nil, true)
			return stream, nil
		}}
		p := newFakeProvider(t, api)
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		var got []kernel.StreamChunk
		var last error
		deadline := time.AfterFunc(5*time.Second, cancel)
		defer deadline.Stop()
		for c, err := range p.Stream(ctx, nil, llmhub.ModelOptions{}) {
			if err != nil {
				last = err
				continue
			}
			got = append(got, c)
			cancel()
		}
		if len(got) != 1 || !errors.Is(last, context.Canceled) {
			t.Errorf("chunks %v, last error %v, want one chunk then context.Canceled", got, last)
		}
		select {
		case <-stream.done:
		default:
			t.Error("the stream was not closed")
		}
	})

	t.Run("breaking out of the loop closes the stream", func(t *testing.T) {
		var closed atomic.Bool
		api := &fakeAPI{stream: func(context.Context, *bedrockruntime.ConverseStreamInput) (EventStream, error) {
			s := newChanStream(eventsOf(textEvent(0, "a"), textEvent(0, "b")), nil, false)
			return closeSpy{chanStream: s, closed: &closed}, nil
		}}
		p := newFakeProvider(t, api)
		for range p.Stream(t.Context(), nil, llmhub.ModelOptions{}) {
			break
		}
		if !closed.Load() {
			t.Error("the stream was not closed after break")
		}
	})
}

// closeSpy records that Close was called.
type closeSpy struct {
	*chanStream
	closed *atomic.Bool
}

func (s closeSpy) Close() error {
	s.closed.Store(true)
	return s.chanStream.Close()
}

// TestBedrockRequestBuildFailure checks that arguments that are not a JSON object
// fail the call with a provider ModelError (Chat) or one error chunk (Stream)
// instead of panicking or sending a broken request.
func TestBedrockRequestBuildFailure(t *testing.T) {
	msgs := []kernel.Message{{Role: kernel.RoleAssistant, ToolCalls: []kernel.ToolCall{{ID: "c1", Name: "n", Arguments: kernel.Arguments("[1")}}}}
	api := &fakeAPI{}
	p := newFakeProvider(t, api)
	_, err := p.Chat(t.Context(), msgs, llmhub.ModelOptions{})
	if me := failureOf(t, err); me.Code != kernel.ModelCodeProvider || me.Retryable {
		t.Errorf("Chat error %+v, want a provider error that is not retryable", me)
	}
	chunks := drain(t, p, msgs, llmhub.ModelOptions{})
	if len(chunks) != 1 || chunks[0].Type != kernel.ChunkError || chunks[0].Error == "" {
		t.Errorf("Stream chunks %v, want one error chunk", chunks)
	}
	if len(api.converseCalls)+len(api.streamCalls) != 0 {
		t.Error("the API was called for a request that could not be built")
	}
}

// TestBedrockToolResultStatus checks IsError: true gives status error, false and
// absent give success.
func TestBedrockToolResultStatus(t *testing.T) {
	api, input := captureConverse(t)
	p := newFakeProvider(t, api)
	yes, no := true, false
	_, err := p.Chat(t.Context(), []kernel.Message{{
		Role: kernel.RoleAssistant,
		ToolResults: []kernel.ToolResult{
			{ToolCallID: "a", Content: "x", IsError: &yes},
			{ToolCallID: "b", Content: "x", IsError: &no},
			{ToolCallID: "c", Content: "x"},
		},
	}}, llmhub.ModelOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var statuses []types.ToolResultStatus
	for _, b := range input().Messages[0].Content {
		statuses = append(statuses, b.(*types.ContentBlockMemberToolResult).Value.Status)
	}
	want := []types.ToolResultStatus{types.ToolResultStatusError, types.ToolResultStatusSuccess, types.ToolResultStatusSuccess}
	if !slices.Equal(statuses, want) {
		t.Errorf("statuses %v, want %v", statuses, want)
	}
}

// TestBedrockMapErrorFallbacks covers what the golden table cannot say: an error
// that is not an AWS error at all, and a nil-safe unnamed error. A network failure
// and a transport deadline are retryable in Go (error mapping review), and a long
// message is cut at llmtransport.MaxErrorMessage.
func TestBedrockMapErrorFallbacks(t *testing.T) {
	me := mapError(errors.New("connection reset"), "m")
	if me.Code != kernel.ModelCodeProvider || me.Retryable || me.Message != "connection reset" {
		t.Errorf("a plain error gave %+v", me)
	}
	// A timeout from the HTTP client has no AWS name and no status.
	me = mapError(context.DeadlineExceeded, "m")
	if me.Code != kernel.ModelCodeTimeout || !me.Retryable || me.Message != context.DeadlineExceeded.Error() {
		t.Errorf("a deadline error gave %+v", me)
	}
	// A refused or reset connection (a net.Error) and a body cut short.
	for _, err := range []error{
		&net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")},
		fmt.Errorf("read: %w", io.ErrUnexpectedEOF),
	} {
		if me := mapError(err, "m"); me.Code != kernel.ModelCodeProvider || !me.Retryable {
			t.Errorf("%v gave %+v, want a retryable provider error", err, me)
		}
	}
	long := strings.Repeat("x", 3*llmtransport.MaxErrorMessage)
	if me := mapError(errors.New(long), "m"); len(me.Message) > llmtransport.MaxErrorMessage {
		t.Errorf("a long message kept %d bytes", len(me.Message))
	}
}

// TestBedrockNoModelsAPICallWithoutListing makes sure New never calls the network:
// building a provider on the real clients makes no request (the loopback-only
// client would fail the test if it did, and no upstream exists).
func TestBedrockNewMakesNoRequest(t *testing.T) {
	up := llmtest.NewUpstream(t, func(llmtest.Request) llmtest.Reply { return llmtest.Reply{Status: 500} })
	p := realProvider(t, up, providerConfig())
	if p == nil {
		t.Fatal("nil provider")
	}
	if n := len(up.Requests()); n != 0 {
		t.Errorf("New made %d requests", n)
	}
}

// TestBedrockStopReasons pins the stop-reason table: a guardrail or a filter is an error, the token limits are
// max_tokens, and every normal or unknown end is end_turn.
func TestBedrockStopReasons(t *testing.T) {
	cases := map[string]kernel.StopReason{
		"end_turn":                      kernel.StopEndTurn,
		"stop_sequence":                 kernel.StopEndTurn,
		"":                              kernel.StopEndTurn,
		"a_reason_added_later":          kernel.StopEndTurn,
		"tool_use":                      kernel.StopToolUse,
		"max_tokens":                    kernel.StopMaxTokens,
		"model_context_window_exceeded": kernel.StopMaxTokens,
		"guardrail_intervened":          kernel.StopError,
		"content_filtered":              kernel.StopError,
	}
	for reason, want := range cases {
		if got := mapStopReason(reason); got != want {
			t.Errorf("mapStopReason(%q) = %q, want %q", reason, got, want)
		}
	}
}

// TestBedrockStreamEndings covers the terminal events: messageStop alone or metadata alone ends the stream with done, a stream that
// closes before both ends with one error chunk.
func TestBedrockStreamEndings(t *testing.T) {
	stop := &types.ConverseStreamOutputMemberMessageStop{Value: types.MessageStopEvent{StopReason: types.StopReasonEndTurn}}
	meta := &types.ConverseStreamOutputMemberMetadata{Value: types.ConverseStreamMetadataEvent{}}
	cases := []struct {
		name string
		evs  []types.ConverseStreamOutput
		last kernel.StreamChunk
	}{
		{"message stop", eventsOf(textEvent(0, "a"), stop), kernel.StreamChunk{Type: kernel.ChunkDone}},
		{"metadata", eventsOf(textEvent(0, "a"), meta), kernel.StreamChunk{Type: kernel.ChunkDone}},
		{"cut", eventsOf(textEvent(0, "a")), kernel.StreamChunk{Type: kernel.ChunkError, Error: errTruncated}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			chunks := drain(t, newFakeProvider(t, streamOf(c.evs, nil)), []kernel.Message{{Role: kernel.RoleUser, Content: "x"}}, llmhub.ModelOptions{})
			last := chunks[len(chunks)-1]
			if len(chunks) != 2 || last.Type != c.last.Type || last.Error != c.last.Error {
				t.Errorf("chunks %+v, want text then %+v", chunks, c.last)
			}
		})
	}
}

// TestBedrockToolResultsOfAToolMessage covers a tool message: its
// results are sent, its own content is not (a user message keeps its text, see the
// role-user-with-results golden).
func TestBedrockToolResultsOfAToolMessage(t *testing.T) {
	_, mapped, err := mapMessages([]kernel.Message{
		{Role: kernel.RoleTool, Content: "ignored", ToolResults: []kernel.ToolResult{{ToolCallID: "c1", Content: "r1"}}},
	}, llmhub.ModelOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(mapped) != 1 || mapped[0].Role != types.ConversationRoleUser || len(mapped[0].Content) != 1 {
		t.Fatalf("mapped %+v, want one user message with one block", mapped)
	}
	if _, ok := mapped[0].Content[0].(*types.ContentBlockMemberToolResult); !ok {
		t.Errorf("the block is %T, want a tool result", mapped[0].Content[0])
	}
}

// TestBedrockStreamDoneStopReason serves real eventstream frames and requires the done
// chunk to carry the stop reason of messageStop, mapped like Chat maps it (A-88), and
// the request to send llmhub.DefaultMaxOutputTokens when ModelOptions.MaxTokens is nil.
func TestBedrockStreamDoneStopReason(t *testing.T) {
	cases := map[string]kernel.StopReason{
		"end_turn":             kernel.StopEndTurn,
		"tool_use":             kernel.StopToolUse,
		"max_tokens":           kernel.StopMaxTokens,
		"stop_sequence":        kernel.StopEndTurn,
		"guardrail_intervened": kernel.StopError,
		"content_filtered":     kernel.StopError,
	}
	for reported, want := range cases {
		t.Run(reported, func(t *testing.T) {
			up := llmtest.NewUpstream(t, func(llmtest.Request) llmtest.Reply {
				return streamReply(
					frame(t, "messageStart", `{"role":"assistant"}`),
					frame(t, "contentBlockDelta", `{"contentBlockIndex":0,"delta":{"text":"hi"}}`),
					frame(t, "contentBlockStop", `{"contentBlockIndex":0}`),
					frame(t, "messageStop", `{"stopReason":"`+reported+`"}`),
					frame(t, "metadata", `{"usage":{"inputTokens":3,"outputTokens":4,"totalTokens":7},"metrics":{"latencyMs":5}}`),
				)
			})
			p := realProvider(t, up, providerConfig())
			chunks := drain(t, p, []kernel.Message{{Role: kernel.RoleUser, Content: "go"}}, llmhub.ModelOptions{})
			done := chunks[len(chunks)-1]
			if done.Type != kernel.ChunkDone || done.StopReason != want {
				t.Fatalf("last chunk = %+v, want done with stop reason %q", done, want)
			}
			if done.Usage == nil || done.Usage.OutputTokens != 4 {
				t.Fatalf("done usage = %+v, want the metadata usage", done.Usage)
			}
			if body := string(up.Requests()[0].Body); !strings.Contains(body, `"inferenceConfig":{"maxTokens":16384}`) {
				t.Fatalf("request body %s lacks the default maxTokens 16384", body)
			}
		})
	}
}
