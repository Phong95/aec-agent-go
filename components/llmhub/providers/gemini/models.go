package gemini

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"slices"

	"github.com/Phong95/aec-agent-go/components/llmhub"
	"github.com/Phong95/aec-agent-go/internal/llmtransport"
	"github.com/Phong95/aec-agent-go/kernel"
)

// maxModelsBody is how much of one page of the model list is read.
const maxModelsBody = 32 << 20

// maxModelPages bounds the pages ListModels follows, so a server that keeps sending
// a next page token cannot keep it busy forever.
const maxModelPages = 100

// ListModels lists the models that can generate content: GET models with
// pageSize=1000, following nextPageToken until the list ends. Entries whose
// supportedGenerationMethods do not include generateContent (embedding and other
// models) are dropped; every other entry goes through llmhub.MapGeminiModel with its
// exact bytes, so Raw holds the entry as the API sent it.
//
// A non-2xx answer is the mapHTTPError error, a body that is not JSON a provider
// error. A done caller context returns the bare ctx.Err().
func (p *Provider) ListModels(ctx context.Context) ([]llmhub.ModelInfo, error) {
	models := []llmhub.ModelInfo{}
	token := ""
	for range maxModelPages {
		page, next, err := p.listPage(ctx, token)
		if err != nil {
			return nil, err
		}
		models = append(models, page...)
		if next == "" {
			return models, nil
		}
		token = next
	}
	return models, nil
}

// listPage fetches one page of the model list and returns its models and the next
// page token.
func (p *Provider) listPage(ctx context.Context, token string) ([]llmhub.ModelInfo, string, error) {
	if err := ctx.Err(); err != nil {
		return nil, "", err
	}
	q := url.Values{}
	q.Set("pageSize", "1000")
	if token != "" {
		q.Set("pageToken", token)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.baseURL+"/models?"+q.Encode(), nil)
	if err != nil {
		return nil, "", mapNetworkError(err)
	}
	req.Header = p.headers(false)
	resp, err := p.client.Do(req)
	if err != nil {
		if cerr := ctx.Err(); cerr != nil {
			return nil, "", cerr
		}
		return nil, "", mapNetworkError(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, "", httpFailure(resp)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxModelsBody+1))
	if err != nil {
		if cerr := ctx.Err(); cerr != nil {
			return nil, "", cerr
		}
		if !errors.Is(err, llmtransport.ErrBodyTooLarge) {
			return nil, "", mapNetworkError(err)
		}
	}
	if err != nil || len(body) > maxModelsBody {
		return nil, "", kernel.NewModelError("the model list is larger than the limit of the SDK", kernel.ModelCodeProvider, false)
	}
	return decodeModels(body)
}

// decodeModels turns one page of the model list into the listed models and the next
// page token. A body that is valid JSON but has no models array lists nothing.
func decodeModels(body []byte) ([]llmhub.ModelInfo, string, error) {
	if !json.Valid(body) {
		return nil, "", kernel.NewModelError("the model list is not valid JSON", kernel.ModelCodeProvider, false)
	}
	// Each member is decoded on its own, so a member of another type (a models member
	// that is not an array, a token that is not a string) reads as absent.
	var page struct {
		Models        json.RawMessage `json:"models"`
		NextPageToken json.RawMessage `json:"nextPageToken"`
	}
	var (
		list  []json.RawMessage
		token string
	)
	if json.Unmarshal(body, &page) == nil {
		if json.Unmarshal(page.Models, &list) != nil {
			list = nil
		}
		if json.Unmarshal(page.NextPageToken, &token) != nil {
			token = ""
		}
	}
	models := []llmhub.ModelInfo{}
	for _, raw := range list {
		var entry struct {
			Methods []any `json:"supportedGenerationMethods"`
		}
		if json.Unmarshal(raw, &entry) == nil && entry.Methods != nil && !slices.Contains(entry.Methods, any("generateContent")) {
			continue
		}
		models = append(models, llmhub.MapGeminiModel(raw, providerName))
	}
	return models, token, nil
}
