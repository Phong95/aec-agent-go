package anthropic

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	sdk "github.com/anthropics/anthropic-sdk-go"

	"github.com/Phong95/aec-agent-go/internal/jsonx"
	"github.com/Phong95/aec-agent-go/internal/llmtransport"
	"github.com/Phong95/aec-agent-go/kernel"
)

// mapError maps an SDK error onto a *kernel.ModelError. An error the API answered
// with a status gets this code, retryability and text:
//
//	401  "Invalid API key"   auth           not retryable
//	403  the API text        auth           not retryable
//	429  "Rate limited"      rate_limit     retryable
//	408  "Request timeout"   timeout        retryable
//	400  the API text        invalid_request not retryable
//	else the API text        provider       retryable when status >= 500
//
// A 403 (permission_error: the key is valid but may not use the model or the
// endpoint) is an auth failure, not a provider failure: retrying it cannot help,
// and the caller must fix the credentials.
//
// The API text is built from the status and the response body ("<status> <body>",
// see apiText), and never from the SDK
// Error() string: that one embeds the request URL with its query, which can hold
// a credential, and it dereferences the request. A failure before any status (the
// connection was refused or reset) is the text "Connection error." (provider) and
// a request that timed out is "Request timed out." (timeout). Both are retryable
// (llmtransport.IsTransient: a refused or reset connection, a body cut short, a
// transport deadline), since a network
// failure is exactly what a retry is for. Anything else keeps its own text (for
// example the SDK's refusal to send a non-streaming request that may take over ten
// minutes).
//
// Every text that comes from the vendor is cut at llmtransport.MaxErrorMessage
// bytes, so an HTML error page does not end up whole in a log line.
//
// An error event in the middle of a stream arrives as an API error with the
// status of the stream response (200), which is reported as an error without a
// status: its text is the event JSON alone, provider, not retryable.
//
// The caller decides whether the call was aborted (its own context is done) before
// it calls mapError; an abort is never mapped here.
//
// The result carries err as its cause (kernel.WithCause), so errors.Is and
// errors.As reach the SDK and transport errors; their text still never enters the
// message. An API error with a status of 400 or more also sets StatusCode, and
// RetryAfter from the Retry-After (or retry-after-ms) header of the response.
func mapError(err error) *kernel.ModelError {
	me := kernel.WithCause(classifyError(err), err)
	var apiErr *sdk.Error
	if errors.As(err, &apiErr) && apiErr.StatusCode >= 400 {
		me.StatusCode = apiErr.StatusCode
		if apiErr.Response != nil {
			me.RetryAfter = llmtransport.RetryAfter(apiErr.Response.Header)
		}
	}
	return me
}

// classifyError builds the code, retryability and text of mapError.
func classifyError(err error) *kernel.ModelError {
	var apiErr *sdk.Error
	if errors.As(err, &apiErr) {
		status := apiErr.StatusCode
		if status < 400 {
			return kernel.NewModelError(apiText(0, apiErr.RawJSON()), kernel.ModelCodeProvider, false)
		}
		switch status {
		case 401:
			return kernel.NewModelError("Invalid API key", kernel.ModelCodeAuth, false)
		case 403:
			return kernel.NewModelError(apiText(status, apiErr.RawJSON()), kernel.ModelCodeAuth, false)
		case 429:
			return kernel.NewModelError("Rate limited", kernel.ModelCodeRateLimit, true)
		case 408:
			return kernel.NewModelError("Request timeout", kernel.ModelCodeTimeout, true)
		case 400:
			return kernel.NewModelError(apiText(status, apiErr.RawJSON()), kernel.ModelCodeInvalidRequest, false)
		default:
			return kernel.NewModelError(apiText(status, apiErr.RawJSON()), kernel.ModelCodeProvider, status >= 500)
		}
	}
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return kernel.NewModelError("Request timed out.", kernel.ModelCodeTimeout, true)
	case llmtransport.IsTransient(err):
		// *url.Error and *net.OpError: their text carries the URL or the addresses.
		return kernel.NewModelError("Connection error.", kernel.ModelCodeProvider, true)
	}
	return kernel.NewModelError(llmtransport.TruncateMessage(err.Error()), kernel.ModelCodeProvider, false)
}

// jsonWhitespace is what JSON.parse skips around a value.
const jsonWhitespace = " \t\r\n"

// apiText builds the message of an API error from the response body. With a status it is "<status> <text>", and "<status>
// status code (no body)" when there is no text; without one (status 0, an error
// event inside a stream) it is the text alone, or "(no status code or body)".
//
// The text is, in this order: the top-level "message" of a JSON body when it is
// truthy (a string as is, anything else as compact JSON); else a truthy JSON body
// as compact JSON; else the body text as it came (a body that is not JSON, or JSON
// that is null, false, 0 or ""). Compact keeps the bytes of the body and removes
// only whitespace, so escapes and numbers keep their original spelling.
func apiText(status int, raw string) string {
	text := llmtransport.TruncateMessage(describeBody(status, raw))
	switch {
	case status != 0 && text != "":
		return fmt.Sprintf("%d %s", status, text)
	case status != 0:
		return fmt.Sprintf("%d status code (no body)", status)
	case text != "":
		return text
	}
	return "(no status code or body)"
}

// describeBody returns the text part of apiText.
func describeBody(status int, raw string) string {
	if status == 0 {
		// A streamed event: the data without the line break the SDK appends.
		raw = strings.TrimSuffix(raw, "\n")
	}
	trimmed := strings.Trim(raw, jsonWhitespace)
	if !json.Valid([]byte(trimmed)) {
		if status == 0 && raw != "" {
			// The event data is kept as a string and printed as a JSON string.
			if quoted, err := jsonx.Marshal(raw); err == nil {
				return string(quoted)
			}
		}
		return raw
	}
	var value any
	dec := json.NewDecoder(strings.NewReader(trimmed))
	dec.UseNumber()
	if err := dec.Decode(&value); err != nil || !truthy(value) {
		return raw
	}
	if _, isObject := value.(map[string]any); isObject {
		var members map[string]json.RawMessage
		if err := json.Unmarshal([]byte(trimmed), &members); err == nil {
			if msgRaw, ok := members["message"]; ok {
				var msg any
				mdec := json.NewDecoder(bytes.NewReader(msgRaw))
				mdec.UseNumber()
				if err := mdec.Decode(&msg); err == nil && truthy(msg) {
					if s, isString := msg.(string); isString {
						return s
					}
					return compact([]byte(msgRaw))
				}
			}
		}
	}
	return compact([]byte(trimmed))
}

// compact removes the insignificant whitespace of valid JSON.
func compact(b []byte) string {
	var buf bytes.Buffer
	if err := json.Compact(&buf, b); err != nil {
		return string(b)
	}
	return buf.String()
}

// truthy is the JS truthiness of a decoded JSON value: null, false, 0 and "" are
// falsy, everything else (an empty array or object too) is truthy.
func truthy(v any) bool {
	switch x := v.(type) {
	case nil:
		return false
	case bool:
		return x
	case string:
		return x != ""
	case json.Number:
		f, err := x.Float64()
		return err != nil || f != 0
	}
	return true
}
