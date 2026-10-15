package bedrock

import (
	"context"
	"net/http"
	"strings"

	"github.com/Phong95/aec-agent-go/components/llmhub"
	"github.com/Phong95/aec-agent-go/kernel"
)

// Compile-time proof that Provider is an llmhub.Provider and an llmhub.ModelLister.
var (
	_ llmhub.Provider    = (*Provider)(nil)
	_ llmhub.ModelLister = (*Provider)(nil)
)

const (
	// defaultName is the name a provider built without WithName reports.
	defaultName = "bedrock"
	// defaultRegion is the region used when the config names none.
	defaultRegion = "us-east-1"
	// defaultModelSuffix follows the region prefix in the default model id
	// (<regionPrefix>anthropic.claude-sonnet-4-20250514-v1:0).
	defaultModelSuffix = "anthropic.claude-sonnet-4-20250514-v1:0"
)

// options collects what the Option functions set before New builds the clients.
type options struct {
	name          string
	converse      ConverseAPI
	models        ModelsAPI
	baseEndpoint  *string
	httpClient    *http.Client
	retryAttempts int
}

// Option changes how New builds the provider. Options exist for tests and advanced
// use; a plain New(cfg) is what a registry factory calls.
type Option func(*options)

// WithName sets the name the provider reports, which is also the provider name its
// model listing uses (default "bedrock").
func WithName(name string) Option {
	return func(o *options) { o.name = name }
}

// WithConverseAPI replaces the Bedrock runtime with api: Chat and Stream call it
// and no runtime client is built. A test passes a fake; the conformance suite runs
// Bedrock this way.
func WithConverseAPI(api ConverseAPI) Option {
	return func(o *options) { o.converse = api }
}

// WithModelsAPI replaces the Bedrock control plane with api: ListModels calls it
// and no control-plane client is built.
func WithModelsAPI(api ModelsAPI) Option {
	return func(o *options) { o.models = api }
}

// WithBaseEndpoint points both real clients at url instead of the regional AWS
// endpoint. A test passes the address of a loopback server; requests are still
// signed with SigV4 from the configured credentials.
func WithBaseEndpoint(url string) Option {
	return func(o *options) { o.baseEndpoint = &url }
}

// WithHTTPClient makes both real clients send their requests through c instead of
// the SDK's default client. A test passes the client of a loopback upstream. The
// response bodies of c are bounded like those of the default client (see
// newRuntimeClient), but c's own redirect policy applies unchanged.
func WithHTTPClient(c *http.Client) Option {
	return func(o *options) { o.httpClient = c }
}

// WithRetryMaxAttempts sets the number of attempts the SDK makes for a request,
// the first one included (AWS counts attempts, not retries). Without this option
// the SDK default applies (three attempts). Tests that are
// not about retries pass 1.
func WithRetryMaxAttempts(n int) Option {
	return func(o *options) { o.retryAttempts = n }
}

// Provider is the Bedrock llmhub.Provider. It is immutable after New and safe for
// concurrent use.
type Provider struct {
	name         string
	defaultModel string
	regionPrefix string
	converse     ConverseAPI
	models       ModelsAPI
}

// New builds the provider from cfg. cfg.APIKey is the access key id and
// cfg.SecretKey the secret (an empty secret is accepted). An empty cfg.Region
// selects us-east-1 and an empty cfg.Model selects the Claude Sonnet 4 model with
// the region prefix (an empty string means "use the default").
//
// The AWS clients are built by newRuntimeClient and newControlClient, from static
// credentials only; a client that WithConverseAPI or WithModelsAPI replaces is not
// built. New builds a client without any network call.
func New(cfg llmhub.ProviderConfig, opts ...Option) (*Provider, error) {
	var o options
	for _, opt := range opts {
		opt(&o)
	}
	name := o.name
	if name == "" {
		name = defaultName
	}
	region := cfg.Region
	if region == "" {
		region = defaultRegion
	}
	prefix := deriveRegionPrefix(region)
	model := cfg.Model
	if model == "" {
		model = prefix + defaultModelSuffix
	}
	p := &Provider{name: name, defaultModel: model, regionPrefix: prefix, converse: o.converse, models: o.models}
	if p.converse == nil {
		p.converse = runtimeAdapter{client: newRuntimeClient(cfg, region, &o)}
	}
	if p.models == nil {
		p.models = controlAdapter{client: newControlClient(cfg, region, &o)}
	}
	return p, nil
}

// Name returns the name given by WithName, "bedrock" by default.
func (p *Provider) Name() string { return p.name }

// Version returns "1.0.0".
func (*Provider) Version() string { return "1.0.0" }

// Initialize does nothing.
func (*Provider) Initialize(context.Context) error { return nil }

// Dispose does nothing.
func (*Provider) Dispose(context.Context) error { return nil }

// CountTokens returns the shared estimate (llmhub.EstimateTokens), computed without
// calling the API. A done context returns the bare ctx.Err() first, as llmhub.Provider
// requires.
func (*Provider) CountTokens(ctx context.Context, msgs []kernel.Message) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	return llmhub.EstimateTokens(msgs), nil
}

// deriveRegionPrefix is the cross-region inference profile prefix of a region. Raw
// model ids are rejected for on-demand invocation of the newer models, so a prefix
// is added to them (see resolveModelID). The region is compared without regard to
// case (an upper-case US-EAST-1 works too), and no region gets a profile that
// cannot be invoked from there (such as "us." for ca-central-1 or us-gov-west-1):
//   - us-gov- regions get "us-gov." (checked before us-);
//   - us- regions get "us.";
//   - eu- regions get "eu.";
//   - ap- regions get "apac.", the Asia Pacific profile prefix ("ap." is no
//     Bedrock prefix);
//   - every other region (ca-, sa-, me-, af-, il-, mx-, cn- and unknown ones) gets
//     no prefix: being conservative, a raw id that the region may serve on demand
//     is better than a profile that is certainly wrong there.
func deriveRegionPrefix(region string) string {
	region = strings.ToLower(region)
	switch {
	case strings.HasPrefix(region, "us-gov-"):
		return "us-gov."
	case strings.HasPrefix(region, "us-"):
		return "us."
	case strings.HasPrefix(region, "eu-"):
		return "eu."
	case strings.HasPrefix(region, "ap-"):
		return "apac."
	default:
		return ""
	}
}

// geoPrefixes are the geographic prefixes of the Bedrock cross-region inference
// profiles (and the legacy "ap."): an id that starts with one of them already
// names a profile.
var geoPrefixes = []string{"us.", "us-gov.", "eu.", "apac.", "ap.", "jp.", "au.", "ca.", "global."}

// resolveModelID gives a raw model id the provider's region prefix. An ARN, the
// empty string and an id that already starts with a geographic prefix
// (geoPrefixes, without regard to case) are used as they are, so a global., apac.,
// jp. or us-gov. id never gets a second prefix (global.x never becomes
// apac.global.x).
func (p *Provider) resolveModelID(modelID string) string {
	if modelID == "" || strings.HasPrefix(strings.ToLower(modelID), "arn:") || hasGeoPrefix(modelID) {
		return modelID
	}
	return p.regionPrefix + modelID
}

// hasGeoPrefix reports whether id starts with a geographic prefix, ignoring case.
func hasGeoPrefix(id string) bool {
	lower := strings.ToLower(id)
	for _, prefix := range geoPrefixes {
		if strings.HasPrefix(lower, prefix) {
			return true
		}
	}
	return false
}
