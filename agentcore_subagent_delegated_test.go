package aec_test

import (
	"context"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/Phong95/aec-agent-go/components/llmhub"
	"github.com/Phong95/aec-agent-go/components/toolhub"
	"github.com/Phong95/aec-agent-go/components/toolhub/builtin/artifacts"
	"github.com/Phong95/aec-agent-go/kernel"
)

// A-137: Definition.Subagent - which tools a subagent may use, and which the parent
// hands to its subagents (left out of its own tool list, so their large output stays
// out of its context). A-129 item 5: a read-only child may save an artifact. The
// "delegated tools" part of tests/unit/subagent-delegated-tools.test.ts of core/ts.

var (
	deepSearch = typeTool{name: "deep_search", risk: toolhub.RiskReadOnly, subagent: toolhub.SubagentPreferred}
	lookTool   = typeTool{name: "look", risk: toolhub.RiskReadOnly}
)

// parentView is the parent's first model call: the tools it is offered and its system
// message. No delegation happens: the child script is never reached.
func parentView(t *testing.T, tools []typeTool, types []string, parentTools []string) (offered []string, system string) {
	t.Helper()
	s := setupTypes(t, typesOptions{
		delegate: nil,
		child: func(context.Context, []kernel.Message, llmhub.ModelOptions) (kernel.ModelResponse, error) {
			return typesReply("x"), nil
		},
		types:       types,
		tools:       tools,
		parentTools: parentTools,
	})
	s.chat(t)
	first := s.llm.recorded()[0]
	return toolNames(first.opts.Tools), systemOf(first.msgs)
}

func TestDelegatedTools_ATypesPreferredToolIsLeftOutAndNamedInTheIndex(t *testing.T) {
	offered, system := parentView(t, []typeTool{deepSearch, lookTool}, []string{"general-purpose"}, nil)
	if slices.Contains(offered, "deep_search") || !slices.Contains(offered, "look") {
		t.Fatalf("offered %v, want look without deep_search", offered)
	}
	if !strings.Contains(system, "deep_search (general-purpose)") {
		t.Fatalf("the index does not name the delegated tool:\n%s", system)
	}
}

func TestDelegatedTools_KeptWhenNoEnabledTypeHasIt(t *testing.T) {
	// researcher has a fixed tool list without deep_search
	offered, system := parentView(t, []typeTool{deepSearch}, []string{"researcher"}, nil)
	if !slices.Contains(offered, "deep_search") {
		t.Fatalf("offered %v, want deep_search", offered)
	}
	if strings.Contains(system, "Work that needs these tools goes to a subagent") {
		t.Fatalf("the index names delegated tools:\n%s", system)
	}
}

func TestDelegatedTools_KeptWhenAPreferredWritingToolMeetsOnlyAReadOnlyType(t *testing.T) {
	build := typeTool{name: "big_build", risk: toolhub.RiskDraft, subagent: toolhub.SubagentPreferred}
	offered, _ := parentView(t, []typeTool{build}, []string{"general-purpose"}, nil)
	if !slices.Contains(offered, "big_build") {
		t.Fatalf("offered %v, want big_build", offered)
	}
}

func TestDelegatedTools_TheChildGetsTheHiddenToolAndSaveArtifact(t *testing.T) {
	var mu sync.Mutex
	var childTools []string
	s := setupTypes(t, typesOptions{
		delegate: []map[string]any{{"subagent": "general-purpose", "task": "search deeply"}},
		child: func(_ context.Context, _ []kernel.Message, opts llmhub.ModelOptions) (kernel.ModelResponse, error) {
			mu.Lock()
			childTools = toolNames(opts.Tools)
			mu.Unlock()
			return typesReply("report"), nil
		},
		tools: []typeTool{deepSearch, {name: "artifact", risk: toolhub.RiskDraft}, {name: "write_file", risk: toolhub.RiskDraft}},
	})
	s.chat(t)
	mu.Lock()
	defer mu.Unlock()
	slices.Sort(childTools)
	// A-129 item 5: the read-only general-purpose child may save a deliverable, but write no other file.
	// A-209: save_artifact is the artifact tool's "save" action.
	if want := []string{"artifact", "deep_search"}; !slices.Equal(childTools, want) {
		t.Fatalf("child tools = %v, want %v", childTools, want)
	}
}

func TestDelegatedTools_AChildNeverGetsAToolMarkedNever(t *testing.T) {
	var mu sync.Mutex
	var childTools []string
	s := setupTypes(t, typesOptions{
		delegate: []map[string]any{{"subagent": "general-purpose", "task": "look"}},
		child: func(_ context.Context, _ []kernel.Message, opts llmhub.ModelOptions) (kernel.ModelResponse, error) {
			mu.Lock()
			childTools = toolNames(opts.Tools)
			mu.Unlock()
			return typesReply("report"), nil
		},
		tools: []typeTool{lookTool, {name: "open_screen", risk: toolhub.RiskReadOnly, subagent: toolhub.SubagentNever}},
	})
	s.chat(t)
	mu.Lock()
	defer mu.Unlock()
	if !slices.Equal(childTools, []string{"look"}) {
		t.Fatalf("child tools = %v, want [look]", childTools)
	}
}

// TestDelegatedTools_AReadOnlyChildMaySaveAndListArtifactsButNotUpdate (A-209): the scope
// of a read-only child judges each call on its own risk and arguments - the artifact
// tool's "save" and "list" run, "update" is refused before the policy.
func TestDelegatedTools_AReadOnlyChildMaySaveAndListArtifactsButNotUpdate(t *testing.T) {
	var mu sync.Mutex
	var results []string
	step := 0
	calls := []string{
		`{"action":"save","name":"a.md","type":"text/markdown","content":"hi"}`,
		`{"action":"update","name":"a.md","content":"bye"}`,
		`{"action":"list"}`,
	}
	s := setupTypes(t, typesOptions{
		delegate: []map[string]any{{"subagent": "general-purpose", "task": "save a report"}},
		// registered below; the agent's tool list is read on every call
		parentTools: []string{"artifact"},
		child: func(_ context.Context, msgs []kernel.Message, _ llmhub.ModelOptions) (kernel.ModelResponse, error) {
			mu.Lock()
			defer mu.Unlock()
			if step > 0 {
				for _, r := range resultsOf(msgs) {
					results = append(results, r.Content)
				}
			}
			if step == len(calls) {
				return typesReply("report"), nil
			}
			step++
			return typesToolUse([]kernel.ToolCall{{ID: "tc-art-" + strconv.Itoa(step), Name: "artifact", Arguments: kernel.Arguments(calls[step-1])}}), nil
		},
	})
	if err := s.core.UseTool(artifacts.NewTool(artifacts.Options{})); err != nil {
		t.Fatal(err)
	}
	s.chat(t)
	mu.Lock()
	defer mu.Unlock()
	if len(results) != 3 {
		t.Fatalf("results = %q, want three", results)
	}
	const denied = "is not available to you as a subagent"
	if strings.Contains(results[0], denied) || !strings.Contains(results[1], denied) || strings.Contains(results[2], denied) {
		t.Errorf("results = %q, want save and list in scope and update denied", results)
	}
}
