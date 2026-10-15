package openaicompat

import "github.com/Phong95/aec-agent-go/components/llmhub"

const (
	// novitaBaseURL is the host of the novita registry entry when the config names
	// none.
	novitaBaseURL = "https://api.novita.ai/v3/openai"
	// openaiBaseURL is the host of the openai registry entry when the config names
	// none.
	openaiBaseURL = defaultBaseURL
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

// init registers the factories "novita" and "openai" in the default llm registry,
// the second and third built-in providers. The listing order of
// llmhub.ListProviders comes from the registry's own table, not from the order the
// packages are initialised in.
func init() {
	llmhub.RegisterProvider("novita", factory("novita", novitaBaseURL))
	llmhub.RegisterProvider("openai", factory("openai", openaiBaseURL))
}
