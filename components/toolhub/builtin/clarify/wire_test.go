package clarify_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/Phong95/aec-agent-go/aecmock"
	"github.com/Phong95/aec-agent-go/components/toolhub"
	"github.com/Phong95/aec-agent-go/components/toolhub/builtin/clarify"
	"github.com/Phong95/aec-agent-go/internal/wiretest"
	"github.com/Phong95/aec-agent-go/kernel"
)

// clarifyFixture is builtin-clarify.json, recorded from the reference ClarifyTool by
// tools/wiregen/builtin/clarify.cases.mjs.
type clarifyFixture struct {
	Tool struct {
		Name       string          `json:"name"`
		Version    string          `json:"version"`
		Definition json.RawMessage `json:"definition"`
	} `json:"tool"`
	Cases []clarifyCase `json:"cases"`
}

type clarifyCase struct {
	Name    string          `json:"name"`
	Args    json.RawMessage `json:"args"`
	Handler string          `json:"handler"`
	Context *struct {
		AgentID *string `json:"agentId"`
	} `json:"context"`
	Output json.RawMessage `json:"output"`
	Asked  json.RawMessage `json:"asked"`
}

func loadClarifyFixture(t *testing.T) clarifyFixture {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(wiretest.Load(t, "builtin-clarify.json")))
	dec.DisallowUnknownFields()
	var fx clarifyFixture
	if err := dec.Decode(&fx); err != nil {
		t.Fatalf("decode builtin-clarify: %v", err)
	}
	return fx
}

// handlerFor rebuilds the interaction handler a recorded case used. The second result is
// false for the case with no handler. A recorded throw of a non-Error value becomes an
// error whose message is the recorded text (the string form of the thrown value), and a
// non-string recorded answer becomes the empty answer, the closest value a Go string can hold.
func handlerFor(t *testing.T, spec string) (*aecmock.ScriptedInteraction, bool) {
	t.Helper()
	switch {
	case spec == "none":
		return nil, false
	case strings.HasPrefix(spec, "answer:"):
		answer := strings.TrimPrefix(spec, "answer:")
		return aecmock.NewScriptedInteraction(aecmock.WithAnswerFunc(func(context.Context, toolhub.ClarifyRequest) (toolhub.ClarifyResponse, error) {
			return toolhub.ClarifyResponse{Answer: answer}, nil
		})), true
	case strings.HasPrefix(spec, "throws:"):
		msg := strings.TrimPrefix(spec, "throws:")
		return aecmock.NewScriptedInteraction(aecmock.WithAnswerFunc(func(context.Context, toolhub.ClarifyRequest) (toolhub.ClarifyResponse, error) {
			return toolhub.ClarifyResponse{}, errors.New(msg)
		})), true
	case strings.HasPrefix(spec, "throwsNonError:"):
		msg := strings.TrimPrefix(spec, "throwsNonError:")
		return aecmock.NewScriptedInteraction(aecmock.WithAnswerFunc(func(context.Context, toolhub.ClarifyRequest) (toolhub.ClarifyResponse, error) {
			return toolhub.ClarifyResponse{}, errors.New(msg)
		})), true
	case spec == "answerNonString":
		return aecmock.NewScriptedInteraction(aecmock.WithAnswerFunc(func(context.Context, toolhub.ClarifyRequest) (toolhub.ClarifyResponse, error) {
			return toolhub.ClarifyResponse{}, nil
		})), true
	default:
		t.Fatalf("unknown handler spec %q", spec)
		return nil, false
	}
}

// TestClarifyMatchesGolden replays builtin-clarify.json: the tool's name, version and
// definition, and for every recorded case the output bytes and the request the handler
// received.
func TestClarifyMatchesGolden(t *testing.T) {
	fx := loadClarifyFixture(t)
	if len(fx.Cases) < 40 {
		t.Fatalf("the corpus holds %d cases, want at least 40", len(fx.Cases))
	}

	handler := clarify.NewTool()
	if handler.Name() != fx.Tool.Name || handler.Version() != fx.Tool.Version {
		t.Errorf("tool = %s %s, want the TS %s %s", handler.Name(), handler.Version(), fx.Tool.Name, fx.Tool.Version)
	}
	def, err := kernel.EncodeJSON(handler.Definition())
	if err != nil {
		t.Fatal(err)
	}
	if string(def) != string(fx.Tool.Definition) {
		t.Errorf("definition differs from TS\n got: %s\nwant: %s", def, fx.Tool.Definition)
	}

	for _, c := range fx.Cases {
		t.Run(c.Name, func(t *testing.T) {
			args, err := kernel.NewArguments(c.Args)
			if err != nil {
				t.Fatalf("arguments: %v", err)
			}
			tc := toolhub.Context{SessionID: "s1", AgentID: "a1", Emit: func(string, any) {}}
			if c.Context != nil && c.Context.AgentID == nil {
				tc.AgentID = ""
			}
			interaction, has := handlerFor(t, c.Handler)
			if has {
				tc.Interaction = interaction
			}

			out, err := handler.Execute(t.Context(), args, tc)
			if err != nil {
				t.Fatalf("Execute returned an error (every failure must be an output): %v", err)
			}
			got, err := kernel.EncodeJSON(out)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != string(c.Output) {
				t.Errorf("output differs from TS\n got: %s\nwant: %s", got, c.Output)
			}

			asked := []toolhub.ClarifyRequest{}
			if has {
				asked = interaction.Asked()
			}
			gotAsked, err := kernel.EncodeJSON(asked)
			if err != nil {
				t.Fatal(err)
			}
			if string(gotAsked) != string(c.Asked) {
				t.Errorf("requests differ from TS\n got: %s\nwant: %s", gotAsked, c.Asked)
			}
		})
	}
}

// TestClarifyCorpusCoversEveryBranch keeps the recorder honest: it fails when the corpus
// stops exercising a handler kind or an argument class the tool branches on.
func TestClarifyCorpusCoversEveryBranch(t *testing.T) {
	fx := loadClarifyFixture(t)
	kinds := map[string]int{}
	classes := map[string]int{}
	for _, c := range fx.Cases {
		kind, _, _ := strings.Cut(c.Handler, ":")
		kinds[kind]++
		out := string(c.Output)
		for class, marker := range map[string]string{
			"answered":          `"answered":true`,
			"unanswered":        `"answered":false`,
			"handler failed":    "clarify failed:",
			"unavailable":       "clarify is unavailable:",
			"question missing":  `must be a non-empty string`,
			"question too long": "question is too long",
		} {
			if strings.Contains(out, marker) {
				classes[class]++
			}
		}
		if strings.Contains(string(c.Asked), `"choices"`) {
			classes["asked with choices"]++
		}
		if strings.Contains(string(c.Asked), `"choices":["1","2","3","4"]`) {
			classes["choices limited to four"]++
		}
		if strings.Contains(string(c.Args), `"choices":[1,`) || strings.Contains(string(c.Args), `"choices":"A"`) {
			classes["choices of other types"]++
		}
		if c.Context != nil {
			classes["no agent id"]++
		}
		if len(c.Asked) > 2 && !strings.Contains(string(c.Asked), `"choices"`) {
			classes["asked without choices"]++
		}
	}
	for _, kind := range []string{"answer", "throws", "throwsNonError", "answerNonString", "none"} {
		if kinds[kind] == 0 {
			t.Errorf("no recorded case uses the handler kind %q", kind)
		}
	}
	for _, class := range []string{
		"answered", "unanswered", "handler failed", "unavailable", "question missing", "question too long",
		"asked with choices", "asked without choices", "choices limited to four", "choices of other types", "no agent id",
	} {
		if classes[class] == 0 {
			t.Errorf("no recorded case covers %q", class)
		}
	}
}
