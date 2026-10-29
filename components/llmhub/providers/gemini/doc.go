// Package gemini is the Google Gemini provider of the SDK (A-162): chat, stream,
// token estimate and model listing over the Gemini REST API
// (generativelanguage.googleapis.com), authenticated with an API key.
//
// It is an opt-in leaf package. Importing it registers the factory "gemini" in the
// default llm registry from init(), and the llm/all package imports it with the
// other providers.
//
// The package is built on the standard library only: a plain net/http request and
// the server-sent-events parser of the openai-subscription provider (sse.go). No
// vendor SDK is linked.
//
// The API key travels in the x-goog-api-key header, never in the URL, so it cannot
// end up in an error text that quotes a URL. ProviderConfig.BaseURL replaces the
// default endpoint (APIURL), as it does for the OpenAI-compatible providers; a
// redirect to another host does not carry the key along.
//
// Function calls carry a thought signature that Gemini 3 wants back with the call in
// later requests. The provider keeps it on kernel.ToolCall.Signature and sends
// SkipSignature for a first call that has none (a call another provider wrote).
//
// There is no overall transport timeout: the caller's context governs the lifetime
// of a request. The client has the response-header timeout of the other providers
// (10 minutes) and an idle timeout between two reads of the stream (5 minutes),
// both from internal/llmtransport (WithRequestTimeout, WithStreamIdleTimeout).
package gemini
