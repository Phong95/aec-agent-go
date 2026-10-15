package bedrock

import (
	"context"
	"encoding/json"

	"github.com/aws/aws-sdk-go-v2/service/bedrock"
	"github.com/aws/aws-sdk-go-v2/service/bedrock/types"

	"github.com/Phong95/aec-agent-go/components/llmhub"
	"github.com/Phong95/aec-agent-go/internal/jsonx"
)

// ListModels lists the foundation models of the region through the control-plane
// client (ListFoundationModels lives on the control plane, not on the runtime
// plane). Every summary that has a model id goes through
// llmhub.MapBedrockFoundationModel with the provider's name; a summary without one is
// dropped.
//
// The AWS SDK exposes no raw payload, so ModelInfo.Raw is built here from the typed
// summary, in the member order of the ListFoundationModels documentation. A failure
// is a *kernel.ModelError (mapError with no model id, so a denied listing names AWS's
// own message and no model); a done caller context returns the bare ctx.Err().
func (p *Provider) ListModels(ctx context.Context) ([]llmhub.ModelInfo, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	out, err := p.models.ListFoundationModels(ctx, &bedrock.ListFoundationModelsInput{})
	if err != nil {
		if cerr := ctx.Err(); cerr != nil {
			return nil, cerr
		}
		return nil, mapError(err, "")
	}
	models := []llmhub.ModelInfo{}
	if out == nil {
		return models, nil
	}
	for _, summary := range out.ModelSummaries {
		raw, err := rawSummary(summary)
		if err != nil {
			continue
		}
		if info, ok := llmhub.MapBedrockFoundationModel(raw, p.name); ok {
			models = append(models, info)
		}
	}
	return models, nil
}

// summaryRaw is the JSON of one model summary, with the members in the order the
// ListFoundationModels documentation shows them (modelArn first, modelLifecycle
// last). The order of the response AWS sent is not visible to the Go SDK, so the
// documented order is the one used. A member the response lacks is absent; an empty
// list the response carries stays an empty list.
type summaryRaw struct {
	ModelArn                   *string       `json:"modelArn,omitempty"`
	ModelID                    *string       `json:"modelId,omitempty"`
	ModelName                  *string       `json:"modelName,omitempty"`
	ProviderName               *string       `json:"providerName,omitempty"`
	InputModalities            []string      `json:"inputModalities,omitzero"`
	OutputModalities           []string      `json:"outputModalities,omitzero"`
	ResponseStreamingSupported *bool         `json:"responseStreamingSupported,omitempty"`
	CustomizationsSupported    []string      `json:"customizationsSupported,omitzero"`
	InferenceTypesSupported    []string      `json:"inferenceTypesSupported,omitzero"`
	ModelLifecycle             *lifecycleRaw `json:"modelLifecycle,omitempty"`
}

// lifecycleRaw is the modelLifecycle member of a summary: its status. The
// lifecycle times are not part of the payload (the Go SDK reads them as times and
// there is nothing the normalised metadata needs from them).
type lifecycleRaw struct {
	Status string `json:"status"`
}

// rawSummary encodes a summary as the Raw payload of its ModelInfo. A list the
// response sent, even an empty one, stays a list: the SDK decodes an empty JSON
// array to an empty non-nil slice, and omitzero keeps it.
func rawSummary(s types.FoundationModelSummary) (json.RawMessage, error) {
	r := summaryRaw{
		ModelArn:                   s.ModelArn,
		ModelID:                    s.ModelId,
		ModelName:                  s.ModelName,
		ProviderName:               s.ProviderName,
		ResponseStreamingSupported: s.ResponseStreamingSupported,
		CustomizationsSupported:    names(s.CustomizationsSupported),
		InferenceTypesSupported:    names(s.InferenceTypesSupported),
		InputModalities:            names(s.InputModalities),
		OutputModalities:           names(s.OutputModalities),
	}
	if s.ModelLifecycle != nil {
		r.ModelLifecycle = &lifecycleRaw{Status: string(s.ModelLifecycle.Status)}
	}
	return jsonx.Marshal(r)
}

// names converts a list of string-typed enum values to strings, keeping nil as nil
// and an empty list as an empty list.
func names[T ~string](in []T) []string {
	if in == nil {
		return nil
	}
	out := make([]string, len(in))
	for i, v := range in {
		out[i] = string(v)
	}
	return out
}
