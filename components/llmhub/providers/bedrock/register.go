package bedrock

import "github.com/Phong95/aec-agent-go/components/llmhub"

// init registers the factory "bedrock" in the default llm registry, the fourth
// built-in of the registry. The listing order of llmhub.ListProviders comes from
// the registry's own table, not from the order the packages are initialised in.
func init() {
	llmhub.RegisterProvider("bedrock", func(cfg llmhub.ProviderConfig) (llmhub.Provider, error) {
		p, err := New(cfg)
		if err != nil {
			return nil, err
		}
		return p, nil
	})
}
