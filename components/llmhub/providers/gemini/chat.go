package gemini

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/Phong95/aec-agent-go/components/llmhub"
	"github.com/Phong95/aec-agent-go/internal/llmtransport"
	"github.com/Phong95/aec-agent-go/kernel"
)

// maxResponseBody is how much of a generateContent response is read; a real one is
// far smaller (a function call with a whole file as arguments is a few megabytes).
const maxResponseBody = 32 << 20

// Chat sends one generateContent request and maps the response: the text of every
// part that is not a thought, the function calls (with their signatures), the usage
// (zero when none was reported) and the stop reason (mapStopReason).
//
// A non-2xx answer is the mapHTTPError error, a transport failure the
// mapNetworkError error and a body that is not JSON a provider error. When the
// caller's context is done the bare ctx.Err() is returned instead (an aborted call
// is not a model failure).
func (p *Provider) Chat(ctx context.Context, msgs []kernel.Message, opts llmhub.ModelOptions) (kernel.ModelResponse, error) {
	if err := ctx.Err(); err != nil {
		return kernel.ModelResponse{}, err
	}
	body, err := buildBody(msgs, opts)
	if err != nil {
		return kernel.ModelResponse{}, kernel.WithCause(kernel.NewModelError(err.Error(), kernel.ModelCodeProvider, false), err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.modelURL(opts.Model, "generateContent"), bytes.NewReader(body))
	if err != nil {
		return kernel.ModelResponse{}, mapNetworkError(err)
	}
	req.Header = p.headers(true)
	resp, err := p.client.Do(req)
	if err != nil {
		if cerr := ctx.Err(); cerr != nil {
			return kernel.ModelResponse{}, cerr
		}
		return kernel.ModelResponse{}, mapNetworkError(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return kernel.ModelResponse{}, httpFailure(resp)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBody+1))
	if err != nil {
		if cerr := ctx.Err(); cerr != nil {
			return kernel.ModelResponse{}, cerr
		}
		if !errors.Is(err, llmtransport.ErrBodyTooLarge) {
			return kernel.ModelResponse{}, mapNetworkError(err)
		}
	}
	if err != nil || len(raw) > maxResponseBody {
		return kernel.ModelResponse{}, kernel.NewModelError("the Gemini response is larger than the limit of the SDK", kernel.ModelCodeProvider, false)
	}
	var parsed responseBody
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return kernel.ModelResponse{}, kernel.WithCause(kernel.NewModelError("the Gemini response is not valid JSON", kernel.ModelCodeProvider, false), err)
	}

	var (
		text  strings.Builder
		calls []kernel.ToolCall
	)
	for _, part := range parsed.parts() {
		switch {
		case part.Thought:
		case part.FunctionCall != nil:
			calls = append(calls, toolCall(part))
		case part.Text != nil:
			text.WriteString(*part.Text)
		}
	}
	out := kernel.ModelResponse{
		Message:    kernel.Message{Role: kernel.RoleAssistant, Content: text.String(), ToolCalls: calls},
		StopReason: mapStopReason(parsed.finishReason(), len(calls) > 0),
	}
	if u := parsed.usage(); u != nil {
		out.Usage = *u
	}
	return out, nil
}
