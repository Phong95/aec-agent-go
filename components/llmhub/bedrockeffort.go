package llmhub

import (
	"regexp"
	"slices"
	"strings"
)

// A-176: the reasoning effort of Claude models served through Bedrock.
//
// Converse has no effort field of its own: the level goes to the model as
// additionalModelRequestFields.output_config.effort, the same body field the
// direct Anthropic API takes (A-164). ListFoundationModels does not say which
// models accept it, so the known Claude models are listed here, from a live
// check (2026-10-30, us-east-1). Fable 5 / 5.1 could not be checked (the
// account's data retention mode is not available for them) and are not listed.
// The same table as core/ts bedrock-effort.ts.

// BedrockEffortSupport is the effort levels a Bedrock Claude model accepts, and the
// beta flag it needs for them.
type BedrockEffortSupport struct {
	// Efforts are the accepted levels, lowest first.
	Efforts []string `json:"efforts"`
	// Beta is the anthropic_beta flag the model needs for output_config.effort; empty
	// when it needs none.
	Beta string `json:"beta,omitempty"`
}

var (
	bedrockAllLevels = []string{"low", "medium", "high", "xhigh", "max"}
	bedrockNoXhigh   = []string{"low", "medium", "high", "max"}
)

// bedrockKnownModels is keyed by the model name after "anthropic.", without the date
// and version suffix.
var bedrockKnownModels = map[string]BedrockEffortSupport{
	"claude-opus-5-5":   {Efforts: bedrockAllLevels},
	"claude-sonnet-5-5": {Efforts: bedrockAllLevels},
	"claude-haiku-5-5":  {Efforts: bedrockAllLevels},
	"claude-opus-5":     {Efforts: bedrockAllLevels},
	"claude-sonnet-5":   {Efforts: bedrockAllLevels},
	"claude-opus-4-8":   {Efforts: bedrockAllLevels},
	"claude-opus-4-7":   {Efforts: bedrockAllLevels},
	"claude-opus-4-6":   {Efforts: bedrockNoXhigh},
	"claude-sonnet-4-6": {Efforts: bedrockNoXhigh},
	// Rejected ("Extra inputs are not permitted") without the beta flag.
	"claude-opus-4-5": {Efforts: []string{"low", "medium", "high"}, Beta: "effort-2025-11-24"},
	// Sonnet 4.5 and Haiku 4.5 reject output_config.effort, with or without the flag.
}

// bedrockGeoPrefixes are the geographic prefixes of the cross-region inference
// profiles (and the legacy "ap.").
var bedrockGeoPrefixes = []string{"us.", "us-gov.", "eu.", "apac.", "ap.", "jp.", "au.", "ca.", "global."}

// bedrockModelSuffix is what may follow a known name: a release date, then a version
// ("-20251101-v1:0", "-v1").
var bedrockModelSuffix = regexp.MustCompile(`^(-\d{8})?(-v\d+(:\d+)?)?$`)

// bedrockAnthropicName is the Anthropic model name of a Bedrock model id
// ("claude-opus-5-5"); ok is false when the id is not an Anthropic model. A geographic
// prefix is skipped, and so is an ARN up to its last "/" (an application inference
// profile's ARN names no model, so it is not one).
func bedrockAnthropicName(modelID string) (string, bool) {
	id := strings.ToLower(modelID)
	if strings.HasPrefix(id, "arn:") {
		id = id[strings.LastIndex(id, "/")+1:]
	}
	for _, prefix := range bedrockGeoPrefixes {
		if strings.HasPrefix(id, prefix) {
			id = id[len(prefix):]
			break
		}
	}
	return strings.CutPrefix(id, "anthropic.")
}

// IsBedrockAnthropicModel reports whether a Bedrock model id names an Anthropic
// (Claude) model.
func IsBedrockAnthropicModel(modelID string) bool {
	_, ok := bedrockAnthropicName(modelID)
	return ok
}

// LookupBedrockEffort is the effort support of a known Bedrock Claude model; ok is
// false for any other model. The Efforts slice is a copy.
func LookupBedrockEffort(modelID string) (BedrockEffortSupport, bool) {
	name, ok := bedrockAnthropicName(modelID)
	if !ok {
		return BedrockEffortSupport{}, false
	}
	for key, support := range bedrockKnownModels {
		if rest, found := strings.CutPrefix(name, key); found && bedrockModelSuffix.MatchString(rest) {
			return BedrockEffortSupport{Efforts: slices.Clone(support.Efforts), Beta: support.Beta}, true
		}
	}
	return BedrockEffortSupport{}, false
}
