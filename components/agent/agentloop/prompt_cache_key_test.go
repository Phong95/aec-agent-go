package agentloop_test

import (
	"testing"

	"github.com/Phong95/aec-agent-go/aecmock"
	"github.com/Phong95/aec-agent-go/components/agent/agentloop"
	"github.com/Phong95/aec-agent-go/components/llmhub"
	"github.com/Phong95/aec-agent-go/components/toolhub"
	"github.com/Phong95/aec-agent-go/kernel"
)

// TestPromptCacheKeyIsTheSessionID (A-188): every model call of a run carries the
// session id as its prompt cache key, unless the caller set one.
func TestPromptCacheKeyIsTheSessionID(t *testing.T) {
	work := constTool("work", toolhub.Output{Content: "done"})
	rg := newRig(t, rigOptions{handlers: []toolhub.Handler{work}},
		toolTurn(callOf("c1", "work", `{}`)),
		aecmock.ScriptedTurn{Content: "finished"},
	)
	if _, err := rg.run(t, "go"); err != nil {
		t.Fatalf("run: %v", err)
	}
	calls := rg.models.Calls()
	if len(calls) != 2 {
		t.Fatalf("%d model calls, want 2", len(calls))
	}
	for i, c := range calls {
		if c.Options.PromptCacheKey != "s-1" {
			t.Errorf("call %d: prompt cache key %q, want the session id s-1", i, c.Options.PromptCacheKey)
		}
	}
}

func TestPromptCacheKeyOfTheCallerWins(t *testing.T) {
	rg := newRig(t, rigOptions{}, aecmock.ScriptedTurn{Content: "hi"})
	if _, err := rg.loop.Run(t.Context(), "go", "s-1", llmhub.ModelOptions{PromptCacheKey: "mine"}, agentloop.RunOptions{}); err != nil {
		t.Fatalf("run: %v", err)
	}
	if got := rg.models.Calls()[0].Options.PromptCacheKey; got != "mine" {
		t.Errorf("prompt cache key %q, want the caller's", got)
	}
}

// TestModelCallSpanCarriesTheCacheCounts (A-195): the model-call span ends with the
// cache counts the provider reported (run and stream), and without them when none were.
func TestModelCallSpanCarriesTheCacheCounts(t *testing.T) {
	read := 700
	cached := aecmock.ScriptedTurn{Content: "hi", Usage: &kernel.TokenUsage{InputTokens: 900, OutputTokens: 3, CacheReadTokens: &read}}
	for _, mode := range []string{"run", "stream"} {
		t.Run(mode, func(t *testing.T) {
			rg := newRig(t, rigOptions{}, cached)
			var err error
			if mode == "run" {
				_, err = rg.run(t, "go")
			} else {
				_, err = rg.stream(t, "go")
			}
			if err != nil {
				t.Fatalf("%s: %v", mode, err)
			}
			attrs, ok := rg.sink.endAttrs("model-call")
			if !ok {
				t.Fatal("no model-call span")
			}
			if v, _ := attrs.Get("cacheReadTokens"); v != 700 {
				t.Errorf("cacheReadTokens = %v, want 700 (keys %v)", v, attrs.Keys())
			}
			if attrs.Has("cacheWriteTokens") {
				t.Error("cacheWriteTokens was not reported, want it left out")
			}
		})
	}

	rg := newRig(t, rigOptions{}, aecmock.ScriptedTurn{Content: "hi", Usage: &kernel.TokenUsage{InputTokens: 9, OutputTokens: 3}})
	if _, err := rg.run(t, "go"); err != nil {
		t.Fatal(err)
	}
	if attrs, _ := rg.sink.endAttrs("model-call"); attrs.Has("cacheReadTokens") {
		t.Error("no cache count was reported, want none on the span")
	}
}
