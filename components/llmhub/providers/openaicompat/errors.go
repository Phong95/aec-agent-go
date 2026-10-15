package openaicompat

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	sdk "github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"github.com/openai/openai-go/v3/packages/ssestream"

	"github.com/Phong95/aec-agent-go/internal/llmtransport"
	"github.com/Phong95/aec-agent-go/kernel"
)

// malformedEventText is the error text for a server-sent event whose data is not
// JSON.
const malformedEventText = "Error reading response: malformed server-sent event JSON."

// failure remembers the last error response of one call: its status and its body.
// The Go SDK builds its *Error from the "error" member of the body, and when that
// member is not an object (a plain string or an array, which several
// OpenAI-compatible servers send) it returns a decoding error that has lost the
// status, so a 401 could no longer be told from a 500. The middleware sees every
// response before the SDK parses it, and keeps the final one: a call passes
// failure.middleware as a request option and hands the failure to mapError.
//
// One failure belongs to one call; it is not shared between goroutines.
type failure struct {
	status int
	body   string
	// header is the header of that response, read for its Retry-After.
	header http.Header
}

// middleware records the response of every attempt. Each attempt overwrites the
// previous one, so after the call it holds the last attempt (zero status when that
// attempt had no error status). The body of an error response is read and put back
// for the SDK, which reads it again. The client of the provider (llmtransport)
// already cuts an error body at 1 MiB, and the read here is capped the same way, so
// a custom transport cannot make this middleware read without a bound.
func (f *failure) middleware(req *http.Request, next option.MiddlewareNext) (*http.Response, error) {
	res, err := next(req)
	f.status, f.body, f.header = 0, "", nil
	if err == nil && res != nil && res.StatusCode >= 400 && res.Body != nil {
		b, readErr := io.ReadAll(io.LimitReader(res.Body, llmtransport.MaxErrorBody))
		_ = res.Body.Close()
		res.Body = io.NopCloser(bytes.NewReader(b))
		if readErr == nil {
			f.status, f.body, f.header = res.StatusCode, string(b), res.Header
		}
	}
	return res, err
}

// mapError maps an SDK or transport error to a *kernel.ModelError. An error the API
// answered with a status gets this code, retryability and text:
//
//	401  "Invalid API key"   auth           not retryable
//	403  the API text        auth           not retryable
//	429  "Rate limited"      rate_limit     retryable
//	408  "Request timeout"   timeout        retryable
//	400  the API text        invalid_request not retryable
//	else the API text        provider       retryable when status >= 500
//
// A 403 (the key is valid but may not use the model, the project or the region) is
// an auth failure, not a provider failure: retrying it cannot help.
//
// The API text is built from the status and the response body (statusText), and
// never from the SDK Error() string: that one embeds the request URL with its query,
// which can hold a credential, and it dereferences the request. A failure before any
// status (the connection was refused or reset) is the text "Connection error."
// (provider) and a request that timed out is "Request timed out." (timeout). Both
// are retryable (llmtransport.IsTransient: a refused or reset connection, a body cut
// short, a transport deadline).
//
// Every text that comes from the vendor is cut at llmtransport.MaxErrorMessage
// bytes.
//
// An error event in the middle of a stream (*ssestream.StreamError) is an API
// error without a status: its text is read from the error object of the event
// (streamErrorText), provider, not retryable.
//
// The status and the body come from fail, the last response the call saw, whatever
// shape the body has; an *sdk.Error that fail did not see (fail is nil) supplies its
// own status and the body it kept in its response.
//
// The caller decides whether the call was aborted (its own context is done) before
// it calls mapError; an abort is never mapped here.
//
// The result carries err as its cause (kernel.WithCause), so errors.Is and
// errors.As reach the SDK and transport errors; their text still never enters the
// message. An error response also sets StatusCode, and RetryAfter from the
// Retry-After (or retry-after-ms) header of that response.
func mapError(err error, fail *failure) *kernel.ModelError {
	var apiErr *sdk.Error
	status, body := 0, ""
	var header http.Header
	switch {
	case fail != nil && fail.status >= 400:
		status, body, header = fail.status, fail.body, fail.header
	case errors.As(err, &apiErr):
		status, body = apiErr.StatusCode, bodyOf(apiErr)
		if apiErr.Response != nil {
			header = apiErr.Response.Header
		}
	}
	if status != 0 {
		me := kernel.WithCause(statusError(status, body), err)
		if status >= 400 {
			me.StatusCode = status
			me.RetryAfter = llmtransport.RetryAfter(header)
		}
		return me
	}
	return kernel.WithCause(transportError(err), err)
}

// transportError maps a failure that has no error status (a stream error event, a
// malformed event, a deadline, a network failure, anything else).
func transportError(err error) *kernel.ModelError {
	var streamErr *ssestream.StreamError
	if errors.As(err, &streamErr) {
		return kernel.NewModelError(llmtransport.TruncateMessage(streamErrorText(streamErr)), kernel.ModelCodeProvider, false)
	}
	var syntaxErr *json.SyntaxError
	switch {
	case errors.As(err, &syntaxErr):
		return kernel.NewModelError(malformedEventText, kernel.ModelCodeProvider, false)
	case errors.Is(err, context.DeadlineExceeded):
		return kernel.NewModelError("Request timed out.", kernel.ModelCodeTimeout, true)
	case llmtransport.IsTransient(err):
		// *url.Error and *net.OpError: their text carries the URL or the addresses.
		return kernel.NewModelError("Connection error.", kernel.ModelCodeProvider, true)
	}
	return kernel.NewModelError(llmtransport.TruncateMessage(err.Error()), kernel.ModelCodeProvider, false)
}

// statusError maps an error response to its code, retryability and text.
func statusError(status int, body string) *kernel.ModelError {
	switch status {
	case 401:
		return kernel.NewModelError("Invalid API key", kernel.ModelCodeAuth, false)
	case 403:
		return kernel.NewModelError(statusText(status, body), kernel.ModelCodeAuth, false)
	case 429:
		return kernel.NewModelError("Rate limited", kernel.ModelCodeRateLimit, true)
	case 408:
		return kernel.NewModelError("Request timeout", kernel.ModelCodeTimeout, true)
	case 400:
		return kernel.NewModelError(statusText(status, body), kernel.ModelCodeInvalidRequest, false)
	default:
		return kernel.NewModelError(statusText(status, body), kernel.ModelCodeProvider, status >= 500)
	}
}

// bodyOf returns the response body of an API error. The SDK reads the body to build
// the error and puts it back into the response so that the response can be dumped;
// it is read here and put back again, so the error stays usable. When the response
// is not there (a synthetic error), the body is rebuilt from the error object the
// SDK kept, wrapped in the envelope it was unwrapped from; an error with no object
// has an empty body.
func bodyOf(e *sdk.Error) string {
	if e.Response != nil && e.Response.Body != nil {
		b, err := io.ReadAll(io.LimitReader(e.Response.Body, llmtransport.MaxErrorBody))
		e.Response.Body = io.NopCloser(bytes.NewReader(b))
		if err == nil {
			return string(b)
		}
	}
	if raw := e.RawJSON(); raw != "" {
		return `{"error":` + raw + `}`
	}
	return ""
}

// jsonWhitespace is what JSON.parse skips around a value.
const jsonWhitespace = " \t\r\n"

// statusText builds the text of an error response from the status and the body,
// following the OpenAI client libraries' error message rule:
//
//   - the body is parsed as JSON when it can be. A JSON object or array whose
//     "error" member is missing or null is wrapped, so the whole body is the error
//     value; an object with another "error" member has that member as the error
//     value; every other JSON value (a string, a number, true, false, null) has no
//     error value;
//   - the text is the "message" of the error value when it is truthy (a string as
//     it is, anything else as compact JSON), else the whole error value as compact
//     JSON when it is truthy, else the body itself when it is not JSON or is JSON
//     that is null, false, 0 or "" (a truthy JSON body without an error value has
//     no text);
//   - the message is "<status> <text>", or "<status> status code (no body)" when
//     there is no text.
//
// Compact keeps the bytes of the value and removes only whitespace, where
// JSON.stringify would also re-spell escapes and numbers.
func statusText(status int, body string) string {
	trimmed := strings.Trim(body, jsonWhitespace)
	var errValue []byte
	fallback := body
	if json.Valid([]byte(trimmed)) {
		value := decode([]byte(trimmed))
		if truthy(value) {
			fallback = ""
		}
		switch v := value.(type) {
		case map[string]any:
			errValue = []byte(trimmed)
			if _, has := v["error"]; has && v["error"] != nil {
				var members map[string]json.RawMessage
				if err := json.Unmarshal([]byte(trimmed), &members); err == nil {
					errValue = members["error"]
				}
			}
		case []any:
			errValue = []byte(trimmed)
		}
	}
	text := errorText(errValue)
	if text == "" {
		text = fallback
	}
	text = llmtransport.TruncateMessage(text)
	if text == "" {
		return fmt.Sprintf("%d status code (no body)", status)
	}
	return fmt.Sprintf("%d %s", status, text)
}

// streamErrorText is the text of an error event inside a stream: an API error
// with no status, built from the "error" value of the event, so the text is the
// message of that value, or the value as compact JSON, or "(no status code or
// body)" when there is neither.
func streamErrorText(e *ssestream.StreamError) string {
	data := bytes.Trim(e.Event.Data, jsonWhitespace)
	var raw map[string]json.RawMessage
	if json.Unmarshal(data, &raw) == nil {
		if text := errorText(raw["error"]); text != "" {
			return text
		}
	}
	return "(no status code or body)"
}

// errorText is the msg of APIError.makeMessage for an error value given as raw
// JSON (nil when there is none): its "message" when that is truthy, else the value
// as compact JSON when the value is truthy, else empty.
func errorText(raw []byte) string {
	if len(raw) == 0 {
		return ""
	}
	value := decode(raw)
	if members, ok := value.(map[string]any); ok {
		if msg, has := members["message"]; has && truthy(msg) {
			if s, isString := msg.(string); isString {
				return s
			}
			var parts map[string]json.RawMessage
			if json.Unmarshal(raw, &parts) == nil {
				return compact(parts["message"])
			}
		}
	}
	if truthy(value) {
		return compact(raw)
	}
	return ""
}

// decode reads valid JSON with numbers kept as json.Number, so that truthy can test
// them. It returns nil for anything that does not decode.
func decode(raw []byte) any {
	var v any
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&v); err != nil {
		return nil
	}
	return v
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
