package openaisub

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"

	"github.com/Phong95/aec-agent-go/components/llmhub"
	"github.com/Phong95/aec-agent-go/internal/llmtransport"
	"github.com/Phong95/aec-agent-go/kernel"
)

// maxModelsBody is how much of the model list is read; a real list is a few
// hundred kilobytes.
const maxModelsBody = 32 << 20

// withClientVersion is modelsURL with the client_version query the Codex models
// endpoint requires (set on any endpoint, so WithEndpoints overrides get it too).
// An unparsable URL is returned as is; the request then fails on it.
func withClientVersion(modelsURL string) string {
	u, err := url.Parse(modelsURL)
	if err != nil {
		return modelsURL
	}
	q := u.Query()
	q.Set("client_version", CodexClientVersion)
	u.RawQuery = q.Encode()
	return u.String()
}

// ListModels lists the models the subscription can use: one GET of the models
// endpoint (with the client_version query) and the models headers (the bearer token, originator aec-agent and the
// account id when there is one; no OpenAI-Beta and no content type). Entries whose
// visibility is "none" are dropped ("these models are hidden from the Codex
// picker"), and every other entry goes through llmhub.MapCodexModel with the exact
// bytes of the entry, so Raw holds the entry as the endpoint sent it.
//
// A list that is missing or is not an array is an empty list. A non-2xx answer is the
// mapHTTPError error (a 401 is "Invalid or expired OAuth token" with code auth;
// Chat returns the same typed error). A body that is not JSON is a provider error.
// A done caller context returns the bare ctx.Err().
func (p *Provider) ListModels(ctx context.Context) ([]llmhub.ModelInfo, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	c := p.snapshot()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, withClientVersion(p.modelsURL), nil)
	if err != nil {
		return nil, mapNetworkError(err)
	}
	req.Header = buildModelsHeaders(c)
	resp, err := p.client.Do(req)
	if err != nil {
		if cerr := ctx.Err(); cerr != nil {
			return nil, cerr
		}
		return nil, mapNetworkError(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, httpFailure(resp)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxModelsBody+1))
	if err != nil {
		if cerr := ctx.Err(); cerr != nil {
			return nil, cerr
		}
		// The transport (llmtransport.NewClient) caps the body at the same size and
		// fails the read with ErrBodyTooLarge, which is the limit, not a network
		// failure.
		if !errors.Is(err, llmtransport.ErrBodyTooLarge) {
			return nil, mapNetworkError(err)
		}
	}
	if err != nil || len(body) > maxModelsBody {
		return nil, kernel.NewModelError("the model list is larger than the limit of the SDK", kernel.ModelCodeProvider, false)
	}
	return decodeModels(body)
}

// decodeModels turns the body of the models endpoint into the listed models. A body
// that is valid JSON but has no models array (not an object, no models member, a
// models member that is not an array) lists nothing.
func decodeModels(body []byte) ([]llmhub.ModelInfo, error) {
	if !json.Valid(body) {
		return nil, kernel.NewModelError("the model list is not valid JSON", kernel.ModelCodeProvider, false)
	}
	models := []llmhub.ModelInfo{}
	var top struct {
		Models json.RawMessage `json:"models"`
	}
	var list []json.RawMessage
	if json.Unmarshal(body, &top) == nil && bytes.HasPrefix(bytes.TrimSpace(top.Models), []byte("[")) {
		// A models member that starts with a bracket is a valid array, so this cannot
		// fail on a body json.Valid accepted; an empty list is the answer if it did.
		if json.Unmarshal(top.Models, &list) != nil {
			list = nil
		}
	}
	for _, raw := range list {
		var item struct {
			Visibility *string `json:"visibility"`
		}
		if json.Unmarshal(raw, &item) == nil && item.Visibility != nil && *item.Visibility == "none" {
			continue
		}
		models = append(models, llmhub.MapCodexModel(raw, providerName))
	}
	return models, nil
}
