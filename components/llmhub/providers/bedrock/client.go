package bedrock

import (
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/bedrock"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime"

	"github.com/Phong95/aec-agent-go/components/llmhub"
	"github.com/Phong95/aec-agent-go/internal/llmtransport"
)

// This file holds the only two places that build an AWS client. Both build it from
// explicit options and then run an option function that undoes the one environment
// variable the SDK constructors honour regardless of the options.
//
// The constructors (bedrockruntime.New and bedrock.New) call resolveEnvBearerToken,
// which reads AWS_BEARER_TOKEN_BEDROCK and, when it is set, installs a bearer token
// provider and forces the auth scheme to httpBearerAuth, overriding the static
// credentials given in Options. On a server where that variable is set every call
// would be authenticated with someone else's token, or fail with "bearer token with
// HTTP request requires HTTPS" against a plain-http endpoint. The option functions
// passed to New run after that resolver, so clearing the provider and pinning the
// scheme to SigV4 there makes the variable harmless. A source test fails when an AWS
// client is built anywhere else.
//
// New(Options) reads no other variable: the AWS_ENDPOINT_URL family is read only by
// NewFromConfig, and the region and credentials come from the options. The default
// credential chain (the aws-sdk-go-v2/config module) is not a dependency.
//
// The same option function bounds the response bodies of the HTTP client the SDK
// resolved (the AWS default client, or the one a test injected): an error body is
// cut at 1 MiB and any other body that is not an event stream fails at 32 MiB, since
// the SDK deserialises both without a limit. The AWS default client keeps its own
// redirect rules (it follows only 307 and 308 and drops X-Amz-Security-Token on a
// host change, and net/http drops Authorization), and a SigV4 signature is bound to
// the request it signed, so no redirect policy is added here. No request timeout is
// added for Bedrock either.

// newRuntimeClient builds the Bedrock runtime client with static credentials.
func newRuntimeClient(cfg llmhub.ProviderConfig, region string, o *options) *bedrockruntime.Client {
	opts := bedrockruntime.Options{
		Region:           region,
		Credentials:      credentials.NewStaticCredentialsProvider(cfg.APIKey, cfg.SecretKey, ""),
		BaseEndpoint:     o.baseEndpoint,
		RetryMaxAttempts: o.retryAttempts,
	}
	if o.httpClient != nil {
		opts.HTTPClient = o.httpClient
	}
	return bedrockruntime.New(opts, func(ro *bedrockruntime.Options) {
		// AWS_BEARER_TOKEN_BEDROCK must never apply: the bearer provider is cleared
		// after the SDK read the variable and the scheme is pinned to SigV4.
		ro.BearerAuthTokenProvider = nil
		ro.AuthSchemePreference = []string{"sigv4"}
		ro.HTTPClient = llmtransport.BoundBodies(ro.HTTPClient)
	})
}

// newControlClient builds the Bedrock control-plane client (ListFoundationModels
// lives there, not on the runtime plane) with the same static credentials and the
// same bearer-clearing option function.
func newControlClient(cfg llmhub.ProviderConfig, region string, o *options) *bedrock.Client {
	opts := bedrock.Options{
		Region:           region,
		Credentials:      credentials.NewStaticCredentialsProvider(cfg.APIKey, cfg.SecretKey, ""),
		BaseEndpoint:     o.baseEndpoint,
		RetryMaxAttempts: o.retryAttempts,
	}
	if o.httpClient != nil {
		opts.HTTPClient = o.httpClient
	}
	return bedrock.New(opts, func(ro *bedrock.Options) {
		// The control-plane client has the same environment bearer resolver.
		ro.BearerAuthTokenProvider = nil
		ro.AuthSchemePreference = []string{"sigv4"}
		ro.HTTPClient = llmtransport.BoundBodies(ro.HTTPClient)
	})
}
