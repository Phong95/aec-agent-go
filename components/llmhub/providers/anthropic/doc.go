// Package anthropic is the Anthropic provider of the SDK: chat, stream, token
// estimate and model listing over the Messages API.
//
// It is an opt-in leaf package. Importing it registers the factory "anthropic" in
// the default llm registry from init(), and the llm/all package imports it with
// the other three providers. The root aec package, kernel and llm never import it,
// so a consumer that does not want the vendor SDK does not link it.
//
// The package reads no environment variable: the client is built with
// option.WithoutEnvironmentDefaults and the explicit options of llmhub.ProviderConfig,
// so ANTHROPIC_API_KEY, ANTHROPIC_BASE_URL and ANTHROPIC_AUTH_TOKEN never change
// what is sent.
//
// The vendor SDK does the transport: connection, retries (the SDK default of two),
// the auth headers, SSE decoding and error typing. The request body is not the
// SDK's: the provider builds the exact JSON it puts on the wire (fixed key order,
// string content where the content is a single text) and hands it to the SDK with
// option.WithRequestBody, so a recorded request golden compares byte for byte.
package anthropic
