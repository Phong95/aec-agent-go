package bedrock

import (
	"context"

	"github.com/aws/aws-sdk-go-v2/service/bedrock"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime/types"
)

// ConverseAPI is the part of the Bedrock runtime the provider calls. The real
// implementation wraps a *bedrockruntime.Client (see WithConverseAPI to replace
// it). It is exported, like sandbox/docker.Runner, so a test or a host can supply
// a fake: the conformance suite runs Bedrock through one.
//
// ConverseStream returns an EventStream and not the AWS output, because the output
// type keeps its stream in an unexported field and a fake could not build one. A
// nil EventStream with a nil error means the response had no stream (the provider
// ends such a call with a plain done).
type ConverseAPI interface {
	Converse(ctx context.Context, in *bedrockruntime.ConverseInput) (*bedrockruntime.ConverseOutput, error)
	ConverseStream(ctx context.Context, in *bedrockruntime.ConverseStreamInput) (EventStream, error)
}

// EventStream is the response stream of ConverseStream. It is satisfied by
// *bedrockruntime.ConverseStreamEventStream.
//
// Events delivers the decoded events and is closed when the stream ends or fails;
// Err then reports why it ended (nil for a clean end). Close stops the stream and
// releases the connection; it must be safe to call more than once and
// concurrently, and it must unblock a reader waiting on Events.
type EventStream interface {
	Events() <-chan types.ConverseStreamOutput
	Close() error
	Err() error
}

// ModelsAPI is the control-plane part the provider calls to list models. The real
// implementation wraps a *bedrock.Client (see WithModelsAPI to replace it).
type ModelsAPI interface {
	ListFoundationModels(ctx context.Context, in *bedrock.ListFoundationModelsInput) (*bedrock.ListFoundationModelsOutput, error)
}
