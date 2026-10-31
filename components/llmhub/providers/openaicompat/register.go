package openaicompat

import "github.com/Phong95/aec-agent-go/components/llmhub"

const (
	// novitaBaseURL is the host of the novita registry entry when the config names
	// none.
	novitaBaseURL = "https://api.novita.ai/v3/openai"
	// openaiBaseURL is the host of the openai registry entry when the config names
	// none.
	openaiBaseURL = defaultBaseURL
	// OpenRouterBaseURL (A-177) is the host of the openrouter registry entry when the
	// config names none.
	OpenRouterBaseURL = "https://openrouter.ai/api/v1"
)

// factory returns the registry factory of one name: the provider is built with
// that name, and the base URL defaults to base when cfg.BaseURL is empty (an
// explicit base URL wins). An empty BaseURL means "use the default".
func factory(name, base string) llmhub.Factory {
	return func(cfg llmhub.ProviderConfig) (llmhub.Provider, error) {
		if cfg.BaseURL == "" {
			cfg.BaseURL = base
		}
		p, err := New(cfg, WithName(name))
		if err != nil {
			return nil, err
		}
		return p, nil
	}
}

// openRouterFactory (A-177) is the factory of "openrouter": the provider sends the
// reasoning effort in OpenRouter's reasoning object, and openRouterHeaders.
func openRouterFactory(cfg llmhub.ProviderConfig) (llmhub.Provider, error) {
	if cfg.BaseURL == "" {
		cfg.BaseURL = OpenRouterBaseURL
	}
	p, err := New(cfg, WithName("openrouter"), WithReasoningParam(ReasoningObjectParam), WithHeaders(openRouterHeaders(cfg)))
	if err != nil {
		return nil, err
	}
	return p, nil
}

// openRouterHeaders returns OpenRouter's app attribution headers: cfg.AppURL as
// HTTP-Referer and cfg.AppTitle as X-Title. An empty value sends no header.
func openRouterHeaders(cfg llmhub.ProviderConfig) map[string]string {
	h := map[string]string{}
	if cfg.AppURL != "" {
		h["HTTP-Referer"] = cfg.AppURL
	}
	if cfg.AppTitle != "" {
		h["X-Title"] = cfg.AppTitle
	}
	return h
}

// init registers the factories "novita" and "openai" in the default llm registry,
// the second and third built-in providers, and "openrouter" (A-177). The listing
// order of llmhub.ListProviders comes from the registry's own table, not from the
// order the packages are initialised in.
func init() {
	llmhub.RegisterProvider("novita", factory("novita", novitaBaseURL))
	llmhub.RegisterProvider("openai", factory("openai", openaiBaseURL))
	llmhub.RegisterProvider("openrouter", openRouterFactory)
}
