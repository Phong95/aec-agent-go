// Package bedrock is the AWS Bedrock provider of the SDK: chat, stream, token
// estimate and model listing over the Converse and ConverseStream APIs and
// ListFoundationModels.
//
// It is an opt-in leaf package. Importing it registers the factory "bedrock" in
// the default llm registry from init(), and the llm/all package imports it with the
// other three providers. The root aec package, kernel and llm never import it, so a
// consumer that does not want the AWS SDK does not link it.
//
// Credentials are static: requests are signed with SigV4 from
// llmhub.ProviderConfig.APIKey (the access key id) and SecretKey, and nothing else.
// The default AWS credential chain is not linked (the aws-sdk-go-v2/config module
// is not a dependency), so no profile, instance role or SSO login is ever used.
// AWS_ACCESS_KEY_ID, AWS_SECRET_ACCESS_KEY, AWS_REGION and AWS_ENDPOINT_URL change
// nothing, because the clients are built from explicit options and not from a
// loaded configuration. One variable would change it anyway: the client
// constructors read AWS_BEARER_TOKEN_BEDROCK and silently switch to bearer
// authentication with someone else's token. Every client is therefore built by the
// two helpers in client.go, which clear that token after the SDK has read it.
//
// The AWS clients are reached through three small exported interfaces,
// ConverseAPI, EventStream and ModelsAPI, so a test or a host can replace the
// network with a fake (WithConverseAPI, WithModelsAPI). The AWS output type of a
// stream cannot be built outside the SDK, which is why the seam sits at the event
// stream and not at the output.
package bedrock
