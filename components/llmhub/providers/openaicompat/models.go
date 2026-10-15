package openaicompat

import (
	"context"
	"encoding/json"

	"github.com/openai/openai-go/v3/option"

	"github.com/Phong95/aec-agent-go/components/llmhub"
	"github.com/Phong95/aec-agent-go/internal/llmtransport"
	"github.com/Phong95/aec-agent-go/kernel"
)

// ListModels lists the models the API key can use. The endpoint is one page (the
// openai SDK's page type has no next page); every entry goes through
// llmhub.MapOpenAICompatibleModel with the exact bytes of the entry and the provider's
// name ("openai" or "novita"), so the metadata, the pricing of an OpenRouter-style
// listing and Raw are the same for every OpenAI-compatible server. The body is read up to
// llmtransport.MaxResponseBody and a list of more than llmtransport.MaxModels
// entries is a provider error, so a hostile server cannot make the list grow
// without a bound.
//
// A failure is a *kernel.ModelError (mapError, so a 401 is "Invalid API key" with
// code auth); a done caller context returns the bare ctx.Err().
func (p *Provider) ListModels(ctx context.Context) ([]llmhub.ModelInfo, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var fail failure
	page, err := p.models.List(ctx, option.WithMiddleware(fail.middleware))
	if err != nil {
		if cerr := ctx.Err(); cerr != nil {
			return nil, cerr
		}
		return nil, mapError(err, &fail)
	}
	if len(page.Data) > llmtransport.MaxModels {
		return nil, kernel.NewModelError("the model list has more entries than the limit of the provider", kernel.ModelCodeProvider, false)
	}
	models := make([]llmhub.ModelInfo, 0, len(page.Data))
	for _, m := range page.Data {
		models = append(models, llmhub.MapOpenAICompatibleModel(json.RawMessage(m.RawJSON()), p.name))
	}
	return models, nil
}
