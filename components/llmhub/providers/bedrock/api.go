package bedrock

import (
	"context"

	"github.com/aws/aws-sdk-go-v2/service/bedrock"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime"
)

// runtimeAdapter makes a *bedrockruntime.Client a ConverseAPI.
type runtimeAdapter struct {
	client *bedrockruntime.Client
}

func (a runtimeAdapter) Converse(ctx context.Context, in *bedrockruntime.ConverseInput) (*bedrockruntime.ConverseOutput, error) {
	return a.client.Converse(ctx, in)
}

func (a runtimeAdapter) ConverseStream(ctx context.Context, in *bedrockruntime.ConverseStreamInput) (EventStream, error) {
	out, err := a.client.ConverseStream(ctx, in)
	if err != nil {
		return nil, err
	}
	es := out.GetStream()
	if es == nil {
		// Return an untyped nil: a nil *ConverseStreamEventStream inside the
		// interface would not compare equal to nil.
		return nil, nil
	}
	return es, nil
}

// controlAdapter makes a *bedrock.Client a ModelsAPI.
type controlAdapter struct {
	client *bedrock.Client
}

func (a controlAdapter) ListFoundationModels(ctx context.Context, in *bedrock.ListFoundationModelsInput) (*bedrock.ListFoundationModelsOutput, error) {
	return a.client.ListFoundationModels(ctx, in)
}
