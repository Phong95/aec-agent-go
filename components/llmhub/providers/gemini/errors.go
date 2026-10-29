package gemini

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/Phong95/aec-agent-go/internal/llmtransport"
	"github.com/Phong95/aec-agent-go/kernel"
)

// maxErrorBody is how much of an error response body is read.
const maxErrorBody = 1 << 20

// mapHTTPError maps a non-2xx response to a typed error. The text is the error
// body's message, else the body, else the status text, else "HTTP <status>":
//
//	401, or a 400 whose details say API_KEY_INVALID  "Invalid Gemini API key (<text>)"  auth  not retryable
//	403       the text           auth             not retryable
//	429       "Rate limited"     rate_limit       retryable
//	408, 504  "Request timeout"  timeout          retryable
//	400, 404  the text           invalid_request  not retryable
//	else      the text           provider         retryable when status >= 500
//
// The API key travels in a header, never in the URL, so no text here can carry it.
// The text is cut at llmtransport.MaxErrorMessage bytes.
func mapHTTPError(status int, statusText string, body []byte) *kernel.ModelError {
	text := strings.TrimPrefix(strings.ToValidUTF8(string(body), "\ufffd"), "\ufeff")
	var parsed struct {
		Error *apiError `json:"error"`
	}
	invalidKey := false
	if json.Unmarshal(body, &parsed) == nil && parsed.Error != nil {
		if parsed.Error.Message != "" {
			text = parsed.Error.Message
		}
		for _, d := range parsed.Error.Details {
			if d.Reason == "API_KEY_INVALID" {
				invalidKey = true
			}
		}
	}
	if text == "" {
		text = statusText
	}
	if text == "" {
		text = "HTTP " + strconv.Itoa(status)
	}
	text = llmtransport.TruncateMessage(text)
	switch {
	case status == 401 || invalidKey:
		return kernel.NewModelError("Invalid Gemini API key ("+text+")", kernel.ModelCodeAuth, false)
	case status == 403:
		return kernel.NewModelError(text, kernel.ModelCodeAuth, false)
	case status == 429:
		return kernel.NewModelError("Rate limited", kernel.ModelCodeRateLimit, true)
	case status == 408 || status == 504:
		return kernel.NewModelError("Request timeout", kernel.ModelCodeTimeout, true)
	case status == 400 || status == 404:
		return kernel.NewModelError(text, kernel.ModelCodeInvalidRequest, false)
	default:
		return kernel.NewModelError(text, kernel.ModelCodeProvider, status >= 500)
	}
}

// mapNetworkError maps a transport failure: a retryable provider error whose text
// is the error's own, or a retryable timeout for a deadline of the transport (the
// response-header timeout, the idle timeout of the stream). The result carries err
// as its cause (kernel.WithCause).
func mapNetworkError(err error) *kernel.ModelError {
	message := llmtransport.TruncateMessage(err.Error())
	if errors.Is(err, context.DeadlineExceeded) {
		return kernel.WithCause(kernel.NewModelError(message, kernel.ModelCodeTimeout, true), err)
	}
	return kernel.WithCause(kernel.NewModelError(message, kernel.ModelCodeProvider, true), err)
}

// httpFailure reads the (bounded) body of a non-2xx response and maps it. A body
// that cannot be read counts as empty. The error carries the response status and
// the wait of its Retry-After header.
func httpFailure(resp *http.Response) *kernel.ModelError {
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
	if err != nil {
		body = nil
	}
	text, _ := strings.CutPrefix(resp.Status, strconv.Itoa(resp.StatusCode)+" ")
	me := mapHTTPError(resp.StatusCode, text, body)
	if resp.StatusCode >= 400 {
		me.StatusCode = resp.StatusCode
		me.RetryAfter = llmtransport.RetryAfter(resp.Header)
	}
	return me
}
