package anthropic

import (
	"context"
	"encoding/json"

	sdk "github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/packages/param"

	"github.com/Phong95/aec-agent-go/components/llmhub"
	"github.com/Phong95/aec-agent-go/internal/llmtransport"
	"github.com/Phong95/aec-agent-go/kernel"
)

// modelsPageSize is the page size of the model list.
const modelsPageSize = 100

// ListModels lists the models the API key can use, one request per page of 100,
// following the cursor until the last page, but at most llmtransport.MaxModelPages
// pages and llmtransport.MaxModels entries: a server that never clears has_more is a
// provider error, not an unbounded list. Every entry goes through
// llmhub.MapAnthropicModel with the exact bytes of the entry, so the metadata and Raw
// equal the catalog entry.
//
// A failure is a *kernel.ModelError (mapError, so a 401 is "Invalid API key" with
// code auth); a done caller context returns the bare ctx.Err().
func (p *Provider) ListModels(ctx context.Context) ([]llmhub.ModelInfo, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	models := []llmhub.ModelInfo{}
	page, err := p.client.Models.List(ctx, sdk.ModelListParams{Limit: param.NewOpt[int64](modelsPageSize)})
	for pages := 1; err == nil && page != nil; pages++ {
		if pages > llmtransport.MaxModelPages {
			return nil, kernel.NewModelError("the model list has more pages than the limit of the provider", kernel.ModelCodeProvider, false)
		}
		for _, info := range page.Data {
			models = append(models, llmhub.MapAnthropicModel(json.RawMessage(info.RawJSON()), providerName))
		}
		if len(models) > llmtransport.MaxModels {
			return nil, kernel.NewModelError("the model list has more entries than the limit of the provider", kernel.ModelCodeProvider, false)
		}
		page, err = page.GetNextPage()
	}
	if err != nil {
		if cerr := ctx.Err(); cerr != nil {
			return nil, cerr
		}
		return nil, mapError(err)
	}
	return models, nil
}
