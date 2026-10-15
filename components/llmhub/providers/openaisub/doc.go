// Package openaisub is the ChatGPT-subscription provider of the SDK: chat, stream,
// token estimate and model listing over the internal Responses API that ChatGPT's own Codex client uses, reachable
// with a subscription OAuth bearer token instead of an API key.
//
// It is an opt-in leaf package. Importing it registers the factory
// "openai-subscription" in the default llm registry from init(), and the llm/all
// package imports it with the other three providers.
//
// The package is built on the standard library only: a plain net/http request and
// its own server-sent-events parser (sse.go). The Responses endpoint is not part of
// the public OpenAI API, so no vendor SDK covers it.
//
// The two endpoints are fixed constants (ResponsesURL and ModelsURL). ProviderConfig
// BaseURL is ignored, and it must stay ignored: honouring it would let a
// stray configuration send the bearer token and the account id to another host.
// Only the WithEndpoints option, which exists for tests, changes them. A redirect to
// another host does not carry the token or the account id along.
//
// The OAuth tokens are swapped at run time only through UpdateTokens, which
// implements llmhub.TokenUpdater. The OAuth flow itself (PKCE, refresh before expiry)
// stays in the server: this package only holds the pair it is given.
//
// There is no overall transport timeout: the caller's context governs the lifetime
// of a request (a stream can legitimately stay open for minutes). The client does
// have the response-header timeout of the other providers (10 minutes) and an idle
// timeout between two reads of the stream (5 minutes), both from
// internal/llmtransport (WithRequestTimeout, WithStreamIdleTimeout).
//
// The endpoint is unofficial and has never been exercised against the live service
// (WINDOWS.md #5). What is proven here is the behaviour against recorded fixtures:
// the request bodies and headers equal the recorded ones, and the chunks equal the
// expected chunks for recorded Responses transcripts. A guarded live test arrives
// with plan 08-07.
package openaisub
