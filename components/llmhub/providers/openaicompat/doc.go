// Package openaicompat is the OpenAI-compatible provider of the SDK: chat, stream,
// token estimate and model listing over the Chat Completions API of any server
// that speaks it, configured by a base URL and an API key.
//
// It is an opt-in leaf package. Importing it registers three factories in the
// default llm registry from init(): "openai" (default base URL
// https://api.openai.com/v1), "novita" (default base URL
// https://api.novita.ai/v3/openai) and "openrouter" (default base URL
// https://openrouter.ai/api/v1, A-177), built-ins built from this one provider. The
// llm/all package imports it with the other providers. The root aec package, kernel and
// llm never import it, so a consumer that does not want the vendor SDK does not link
// it.
//
// The package reads no environment variable. The vendor SDK has no public switch
// to turn its environment defaults off, and its client constructor
// (openai.NewClient) applies them: OPENAI_API_KEY, OPENAI_BASE_URL, and also
// OPENAI_ORG_ID and OPENAI_PROJECT_ID, which it would send as the
// OpenAI-Organization and OpenAI-Project headers even when the key is explicit.
// So the package never calls that constructor: it builds the chat completion and
// model services directly from the explicit options of llmhub.ProviderConfig
// (openai.NewChatCompletionService, openai.NewModelService), and a source test
// fails on any call of the client constructor in non-test code.
//
// The vendor SDK does the transport: connection, retries (the SDK default of two),
// the auth header, SSE decoding and error typing. The request body is not the
// SDK's: the provider builds the exact JSON it puts on the wire (fixed key order,
// null content for an assistant message with no text) and hands it to the SDK with
// option.WithRequestBody, so a recorded request golden compares byte for byte. The
// SDK's chat completion accumulator is not used: the provider keeps one buffer per
// tool-call index itself, because the accumulator would hide the per-delta progress
// chunks.
package openaicompat
