package openaisub

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/Phong95/aec-agent-go/internal/llmtransport"
	"github.com/Phong95/aec-agent-go/kernel"
)

// maxErrorBody is how much of an error response body is read. The text becomes an
// error message, so a megabyte is far more than any real error and an endless body
// cannot exhaust memory.
const maxErrorBody = 1 << 20

// mapHTTPError maps a non-2xx response to a typed error. The text is the body, else the status text,
// else "HTTP <status>":
//
//	401  "Invalid or expired OAuth token"  auth            not retryable
//	403  the text                          auth            not retryable
//	429  "Rate limited"                    rate_limit      retryable
//	408  "Request timeout"                 timeout         retryable
//	400  the text                          invalid_request not retryable
//	else the text                          provider        retryable when status >= 500
//
// The body is decoded as UTF-8 as response.text() does (invalid bytes become
// U+FFFD, a leading byte order mark is dropped). A 401 never shows the body, so a
// token the server echoes cannot end up in the message. The text is cut at
// llmtransport.MaxErrorMessage bytes (error mapping review); a 403 is an auth
// failure (the subscription does not cover the model or the account is blocked),
// which a retry cannot cure.
func mapHTTPError(status int, statusText string, body []byte) *kernel.ModelError {
	message := strings.TrimPrefix(strings.ToValidUTF8(string(body), "\ufffd"), "\ufeff")
	if message == "" {
		message = statusText
	}
	if message == "" {
		message = "HTTP " + strconv.Itoa(status)
	}
	message = llmtransport.TruncateMessage(message)
	switch status {
	case 401:
		return kernel.NewModelError("Invalid or expired OAuth token", kernel.ModelCodeAuth, false)
	case 403:
		return kernel.NewModelError(message, kernel.ModelCodeAuth, false)
	case 429:
		return kernel.NewModelError("Rate limited", kernel.ModelCodeRateLimit, true)
	case 408:
		return kernel.NewModelError("Request timeout", kernel.ModelCodeTimeout, true)
	case 400:
		return kernel.NewModelError(message, kernel.ModelCodeInvalidRequest, false)
	default:
		return kernel.NewModelError(message, kernel.ModelCodeProvider, status >= 500)
	}
}

// mapNetworkError maps a transport failure: any failure to reach the endpoint or to
// read the stream is a retryable provider error whose text is the error's own, the
// text of the net/http error. The URL in it is the fixed endpoint, and no header,
// so no credential. A deadline of the transport (the response-header timeout, the
// idle timeout of the stream) gets the code timeout, still retryable, and the text
// is cut at llmtransport.MaxErrorMessage bytes.
//
// The result carries err as its cause (kernel.WithCause), so errors.Is and
// errors.As reach the net/http error.
func mapNetworkError(err error) *kernel.ModelError {
	message := llmtransport.TruncateMessage(err.Error())
	if errors.Is(err, context.DeadlineExceeded) {
		return kernel.WithCause(kernel.NewModelError(message, kernel.ModelCodeTimeout, true), err)
	}
	return kernel.WithCause(kernel.NewModelError(message, kernel.ModelCodeProvider, true), err)
}

// httpFailure reads the (bounded) body of a non-2xx response and maps it. A body
// that cannot be read counts as empty. The error carries the
// response status (StatusCode, when it is 400 or more) and the wait of its
// Retry-After (or retry-after-ms) header.
func httpFailure(resp *http.Response) *kernel.ModelError {
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
	if err != nil {
		body = nil
	}
	me := mapHTTPError(resp.StatusCode, statusText(resp), body)
	if resp.StatusCode >= 400 {
		me.StatusCode = resp.StatusCode
		me.RetryAfter = llmtransport.RetryAfter(resp.Header)
	}
	return me
}

// statusText is the reason phrase of the response status line, without the code.
func statusText(resp *http.Response) string {
	if text, ok := strings.CutPrefix(resp.Status, strconv.Itoa(resp.StatusCode)+" "); ok {
		return text
	}
	return ""
}
