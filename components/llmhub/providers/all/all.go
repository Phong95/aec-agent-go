// Package all links every built-in provider of the SDK. Import it for its side
// effect to register anthropic, novita, openai, bedrock and openai-subscription in
// the default llm registry:
//
//	import _ "github.com/Phong95/aec-agent-go/components/llmhub/providers/all"
//
// The root aec package never imports it, so a consumer that does not need a vendor
// SDK never links one. It is the one deliberate blank-import package of the module
// and holds nothing but those imports.
package all

import (
	// Registers the factory "anthropic".
	_ "github.com/Phong95/aec-agent-go/components/llmhub/providers/anthropic"
	// Registers the factory "bedrock".
	_ "github.com/Phong95/aec-agent-go/components/llmhub/providers/bedrock"
	// Registers the factories "novita" and "openai".
	_ "github.com/Phong95/aec-agent-go/components/llmhub/providers/openaicompat"
	// Registers the factory "openai-subscription".
	_ "github.com/Phong95/aec-agent-go/components/llmhub/providers/openaisub"
)
