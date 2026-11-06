package memorysystem

import (
	"context"
	"strings"

	"github.com/Phong95/aec-agent-go/components/toolhub"
	"github.com/Phong95/aec-agent-go/kernel/ordered"
)

// SearchToolName is the memory tool the memory system offers to its agent's model.
const SearchToolName = "memory_search"

// searchArgs are the arguments of a memory_search call.
type searchArgs struct {
	Query string `json:"query"`
}

// searchDefinition is the memory_search definition (a fresh value on every call): a
// read-only tool with one required query.
func searchDefinition() toolhub.Definition {
	return toolhub.Definition{
		Name: SearchToolName,
		Description: "Search your long-term memory of earlier conversations: facts about the user and their projects, " +
			"their preferences, and decisions made before. Use it when the user refers to something from an earlier " +
			"conversation, when their preferences or past decisions may change your answer, or before asking them " +
			"something they may already have told you. Memories can be outdated " + emDash + " if the user says otherwise now, trust the user.",
		Parameters: toolhub.JSONSchema{
			Type: "object",
			Properties: toolhub.NewProperties(
				toolhub.Property{Name: "query", Schema: toolhub.JSONSchema{Type: "string", Description: "What to look for, in a few words or a sentence (e.g. \"preferred code style\", \"deadline of the kart game\")"}},
			),
			Required: []string{"query"},
		},
		Risk: toolhub.RiskReadOnly,
	}
}

// ToolDefinitions returns the tools the memory system adds to the model's tools:
// memory_search while memory and recall are on (recall.maxResults above 0), none
// otherwise. It never fails.
//
// Memory is recalled on demand through this tool, not injected into every turn: a
// block that changes every turn would change the start of the model input and break
// the provider's prompt cache.
func (s *System) ToolDefinitions(context.Context) ([]toolhub.Definition, error) {
	cfg := s.config()
	if !cfg.Enabled || (cfg.Recall != nil && cfg.Recall.MaxResults != nil && *cfg.Recall.MaxResults <= 0) {
		return []toolhub.Definition{}, nil
	}
	return []toolhub.Definition{searchDefinition()}, nil
}

// Tool returns the memory tool registered under name (memory_search), for the loop to
// dispatch. Any other name has none.
func (s *System) Tool(name string) (toolhub.Handler, bool) {
	if name != SearchToolName {
		return nil, false
	}
	return toolhub.NewTypedTool(searchDefinition(), s.search), true
}

// search runs one memory_search call: the recall of RecallForTurn, keyed on the
// model's query instead of the user's message. A failing store is returned as an
// error, which the executor turns into an error result.
func (s *System) search(ctx context.Context, args searchArgs, tc toolhub.Context) (toolhub.Output, error) {
	query := strings.TrimSpace(args.Query)
	if query == "" {
		isErr, retry := true, false
		return toolhub.Output{Content: `memory_search: "query" must be a non-empty string`, IsError: &isErr, Retryable: &retry}, nil
	}
	recalled, err := s.RecallForTurn(ctx, query, tc.SessionID)
	if err != nil {
		return toolhub.Output{}, err
	}
	meta := ordered.NewObject()
	meta.Set("recalledCount", len(recalled.RecordIDs))
	meta.Set("recordIds", recalled.RecordIDs)
	if recalled.Skipped != "" {
		meta.Set("skipped", recalled.Skipped)
		return toolhub.Output{Content: "Memory is switched off for this agent.", Metadata: meta}, nil
	}
	if recalled.Content == nil || *recalled.Content == "" {
		return toolhub.Output{Content: "No memories match \"" + query + "\".", Metadata: meta}, nil
	}
	return toolhub.Output{Content: *recalled.Content, Metadata: meta}, nil
}
