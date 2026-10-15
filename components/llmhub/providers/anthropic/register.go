package anthropic

import "github.com/Phong95/aec-agent-go/components/llmhub"

// init registers the factory in the default llm registry under "anthropic", the
// first built-in of the registry. The listing order of llmhub.ListProviders comes
// from the registry's own table, not from the order packages are initialised in.
func init() {
	llmhub.RegisterProvider(providerName, func(cfg llmhub.ProviderConfig) (llmhub.Provider, error) {
		p, err := New(cfg)
		if err != nil {
			return nil, err
		}
		return p, nil
	})
}
