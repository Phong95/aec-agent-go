package bedrock

import (
	"context"
	"errors"
	"net/http"

	awshttp "github.com/aws/aws-sdk-go-v2/aws/transport/http"
	"github.com/aws/smithy-go"

	"github.com/Phong95/aec-agent-go/internal/llmtransport"
	"github.com/Phong95/aec-agent-go/kernel"
)

// invalidCredentialErrors are the AWS error names that mean the credentials
// themselves were rejected (unknown key id, bad secret or signature, expired
// session token). AWS answers these with HTTP 403 - the same status as
// AccessDeniedException, which means the credentials are fine but lack
// permission - so they are classified by name and not by status.
var invalidCredentialErrors = map[string]bool{
	"UnrecognizedClientException":  true,
	"InvalidSignatureException":    true,
	"IncompleteSignatureException": true,
	"ExpiredToken":                 true,
	"ExpiredTokenException":        true,
}

// mapError maps an AWS error with the rules below, in order. The name of the AWS
// error is the ErrorCode() of the smithy.APIError in the error chain (typed
// exceptions and the generic one alike), its message is ErrorMessage(), and the HTTP
// status is the one of the *awshttp.ResponseError in the chain. An error with a
// status but no AWS error document (a deserialization failure, an upstream proxy
// page) has no name and the text of the error the response error wraps as its
// message; an error that has neither, such as a connection failure, has status 0, no
// name and its own text.
//
//  1. a credential name (invalidCredentialErrors) gives "Invalid AWS credentials
//     (<detail>)", auth, not retryable;
//  2. status 403 or the name AccessDeniedException gives "AWS Bedrock access
//     denied for model "<modelID>" (<detail>)", auth, not retryable (the target
//     clause is left out when modelID is empty, as ListModels has no model);
//  3. status 429 or ThrottlingException gives "Rate limited", rate_limit, retryable;
//  4. ModelTimeoutException gives "Request timeout", timeout, retryable;
//  5. status 400 or ValidationException gives the message, invalid_request, not
//     retryable;
//  6. anything else gives the message, provider, retryable when the status is 500
//     or more.
//
// Two further rules apply (error mapping review): a failure with no status that is a
// network failure or a transport deadline (llmtransport.IsTransient) is retryable,
// and a deadline is code timeout; and the message is cut at
// llmtransport.MaxErrorMessage bytes.
//
// <detail> is "<name>: <message>" (just the name when the message is empty, just
// the message when there is no name), so the log says what AWS refused. A 403 whose
// body is not an AWS error document (for example an upstream proxy page) is still
// classified by status; its detail is the text of the decoding error, because the
// AWS Go SDK does not keep the body text.
//
// The caller decides whether the call was aborted (its own context is done) before
// it calls mapError; an abort is never mapped here.
//
// The result carries err as its cause (kernel.WithCause), so errors.Is and
// errors.As reach the AWS and transport errors (smithy.APIError, the typed
// exceptions, *awshttp.ResponseError). A response error with a status of 400 or
// more also sets StatusCode, and RetryAfter from the Retry-After header.
func mapError(err error, modelID string) *kernel.ModelError {
	me := kernel.WithCause(classifyError(err, modelID), err)
	var respErr *awshttp.ResponseError
	if errors.As(err, &respErr) {
		if status := respErr.HTTPStatusCode(); status >= 400 {
			me.StatusCode = status
			me.RetryAfter = llmtransport.RetryAfter(responseHeader(respErr))
		}
	}
	return me
}

// responseHeader is the header of the HTTP response of a response error, nil when
// the error has none.
func responseHeader(e *awshttp.ResponseError) http.Header {
	if e == nil || e.ResponseError == nil || e.Response == nil || e.Response.Response == nil {
		return nil
	}
	return e.Response.Header
}

// classifyError builds the code, retryability and text of mapError.
func classifyError(err error, modelID string) *kernel.ModelError {
	status := 0
	var respErr *awshttp.ResponseError
	if errors.As(err, &respErr) {
		status = respErr.HTTPStatusCode()
	}
	named := ""
	msg := err.Error()
	if respErr != nil {
		if inner := respErr.Unwrap(); inner != nil {
			msg = inner.Error()
		}
	}
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) {
		msg = apiErr.ErrorMessage()
		if code := apiErr.ErrorCode(); code != "Error" {
			named = code
		}
	}
	awsDetail := msg
	if named != "" {
		awsDetail = named
		if msg != "" {
			awsDetail = named + ": " + msg
		}
	}

	if named != "" && invalidCredentialErrors[named] {
		return kernel.NewModelError(llmtransport.TruncateMessage("Invalid AWS credentials ("+awsDetail+")"), kernel.ModelCodeAuth, false)
	}
	if status == 403 || named == "AccessDeniedException" {
		// The request was authenticated but not authorized - typically IAM is
		// missing bedrock:InvokeModel* on the model or inference profile, or model
		// access (including its AWS Marketplace subscription) is not granted.
		target := ""
		if modelID != "" {
			target = ` for model "` + modelID + `"`
		}
		return kernel.NewModelError(llmtransport.TruncateMessage("AWS Bedrock access denied"+target+" ("+awsDetail+")"), kernel.ModelCodeAuth, false)
	}
	if status == 429 || named == "ThrottlingException" {
		return kernel.NewModelError("Rate limited", kernel.ModelCodeRateLimit, true)
	}
	if named == "ModelTimeoutException" {
		return kernel.NewModelError("Request timeout", kernel.ModelCodeTimeout, true)
	}
	msg = llmtransport.TruncateMessage(msg)
	if status == 400 || named == "ValidationException" {
		return kernel.NewModelError(msg, kernel.ModelCodeInvalidRequest, false)
	}
	if status == 0 && named == "" {
		switch {
		case errors.Is(err, context.DeadlineExceeded):
			return kernel.NewModelError(msg, kernel.ModelCodeTimeout, true)
		case llmtransport.IsTransient(err):
			return kernel.NewModelError(msg, kernel.ModelCodeProvider, true)
		}
	}
	return kernel.NewModelError(msg, kernel.ModelCodeProvider, status >= 500)
}
