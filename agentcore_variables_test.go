package aec_test

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"

	aec "github.com/Phong95/aec-agent-go"
	"github.com/Phong95/aec-agent-go/components/agent/profile"
	"github.com/Phong95/aec-agent-go/components/secrethub"
	"github.com/Phong95/aec-agent-go/components/secrethub/secret"
	"github.com/Phong95/aec-agent-go/components/toolhub"
	"github.com/Phong95/aec-agent-go/components/toolhub/builtin/secrets"
	"github.com/Phong95/aec-agent-go/components/toolhub/builtin/variables"
	"github.com/Phong95/aec-agent-go/components/toolhub/builtin/web"
	"github.com/Phong95/aec-agent-go/components/variablehub"
	"github.com/Phong95/aec-agent-go/components/variablehub/variable"
	"github.com/Phong95/aec-agent-go/kernel"
)

// A-215: an agent using variables end to end - the "an agent's variables" part of
// tests/unit/variables.test.ts of core/ts. The hub and rules are tested in
// components/variablehub, the terminal / run_code part in builtin/workspace.

func setupVariables(t *testing.T, script secretScript) *secretSetup {
	t.Helper()
	core, err := aec.NewAgentCore()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = core.Dispose(context.WithoutCancel(t.Context())) })
	core.UseSecretHubProvider(secrethub.NewEphemeralSecretHubProvider())
	s := &secretSetup{
		core: core, llm: &secretLLM{script: script}, handler: &secretHandler{approve: true},
		http: &fakeHTTP{respond: func(*http.Request, string) *http.Response { return httpResponse(200, `{"ok":true}`, nil) }},
	}
	tools := append(append([]toolhub.Handler{}, secrets.NewTools()...), variables.NewTools()...)
	tools = append(tools, web.NewHTTPRequestTool(web.HTTPRequestToolOptions{Transport: s.http, Resolver: publicResolver{}}))
	for _, h := range tools {
		if err := core.UseTool(h); err != nil {
			t.Fatal(err)
		}
	}
	core.ToolHub().SetInteractionHandler(s.handler)
	if err := core.UseLLMProvider(s.llm); err != nil {
		t.Fatal(err)
	}
	zero := 0
	p := profile.AgentProfile{
		ID: "agent-a", Name: "Agent A", SystemPrompt: "You are Agent A.", Provider: "fake",
		Tools:      []string{"http_request", "variable", "secret"},
		LoopConfig: &profile.LoopConfig{MaxToolRetries: &zero, RetryBackoffMs: &zero},
	}
	if s.agent, err = core.SpawnAgent(t.Context(), p); err != nil {
		t.Fatal(err)
	}
	return s
}

func createVar(t *testing.T, core *aec.AgentCore, owner, name, value string) {
	t.Helper()
	if _, err := core.VariableHub().Create(t.Context(), variablehub.CreateInput{Definition: variable.Definition{Name: name, Value: value}, OwnerAgentID: owner}); err != nil {
		t.Fatal(err)
	}
}

func TestVariablesOwnWinsEnabledGlobalsAndIndex(t *testing.T) {
	s := setupVariables(t, func([]kernel.Message, int) kernel.ModelResponse { return typesReply("ok") })
	ctx := t.Context()
	if _, err := s.core.VariableHub().Create(ctx, variablehub.CreateInput{Definition: variable.Definition{Name: "REGION", Value: "eu-west-1", Description: "Where we deploy"}}); err != nil {
		t.Fatal(err)
	}
	createVar(t, s.core, "", "HIDDEN", "not enabled")
	createVar(t, s.core, "", "API_BASE", "https://global.example.com")
	s.agent.UpdateVariables(&variable.Config{Use: []string{"REGION", "API_BASE"}})
	if _, err := s.agent.Variables().Create(ctx, variable.Definition{Name: "API_BASE", Value: "https://own.example.com"}); err != nil {
		t.Fatal(err)
	}
	list, _ := s.agent.Variables().List(ctx)
	var got []string
	for _, v := range list {
		got = append(got, v.Name+"="+v.Value+":"+string(v.Scope))
	}
	if !slices.Equal(got, []string{"API_BASE=https://own.example.com:agent", "REGION=eu-west-1:global"}) {
		t.Fatalf("list = %v", got)
	}
	values, err := s.agent.Variables().Resolve(ctx, []string{"API_BASE", "REGION"})
	if err != nil || values["API_BASE"] != "https://own.example.com" || values["REGION"] != "eu-west-1" {
		t.Fatalf("resolve = %v, %v", values, err)
	}
	if _, err := s.agent.Variables().Resolve(ctx, []string{"HIDDEN"}); err == nil || !strings.Contains(err.Error(), "no variable named HIDDEN") {
		t.Fatalf("hidden = %v", err)
	}
	s.chat(t)
	system := s.llm.recorded()[0][0].Content
	if !strings.Contains(system, "<available_variables>") || !strings.Contains(system, `REGION = "eu-west-1" `+"—"+` Where we deploy (global)`) {
		t.Fatalf("system prompt has no variable index:\n%s", system)
	}
}

func TestVariablesFillHTTPRequestBeforeSecrets(t *testing.T) {
	s := setupVariables(t, func(_ []kernel.Message, n int) kernel.ModelResponse {
		if n == 1 {
			return secretToolCalls(secretCall("http_request", map[string]any{
				"method": "POST", "url": "{{var:API_BASE}}/items?region={{var:REGION}}",
				"headers": map[string]any{"Authorization": "{{var:AUTH}}"}, "body": "env={{var:REGION}}",
			}))
		}
		return typesReply("done")
	})
	ctx := t.Context()
	if _, err := s.core.SecretHub().Create(ctx, secrethub.CreateInput{Definition: secret.Definition{Name: "API_KEY", AllowedHosts: []string{"api.example.com"}}, Value: secretAPIKey}); err != nil {
		t.Fatal(err)
	}
	s.agent.UpdateSecrets(&secret.Config{Use: []string{"API_KEY"}})
	createVar(t, s.core, "", "API_BASE", "https://api.example.com/v1")
	createVar(t, s.core, "", "REGION", "eu")
	createVar(t, s.core, "", "AUTH", "Bearer {{secret:API_KEY}}")
	s.agent.UpdateVariables(&variable.Config{Use: []string{"API_BASE", "REGION", "AUTH"}})
	s.chat(t)
	reqs := s.http.recorded()
	if len(reqs) != 1 || reqs[0].url != "https://api.example.com/v1/items?region=eu" || reqs[0].body != "env=eu" {
		t.Fatalf("requests = %+v", reqs)
	}
	if got := reqs[0].header.Get("Authorization"); got != "Bearer "+secretAPIKey {
		t.Fatalf("authorization = %q", got)
	}
	if strings.Contains(s.llm.everythingSent(t), secretAPIKey) {
		t.Fatal("the model saw the secret that came through the variable")
	}
}

func TestVariablesUnknownNameFailsTheCall(t *testing.T) {
	s := setupVariables(t, func(_ []kernel.Message, n int) kernel.ModelResponse {
		if n == 1 {
			return secretToolCalls(secretCall("http_request", map[string]any{"method": "GET", "url": "{{var:NOPE}}/x"}))
		}
		return typesReply("done")
	})
	s.chat(t)
	if len(s.http.recorded()) != 0 {
		t.Fatal("a request was sent")
	}
	if res := lastToolResults(s.llm.recorded()[1]); len(res) == 0 || !strings.Contains(res[0].Content, "no variable named NOPE") {
		t.Fatalf("result = %+v", res)
	}
}

func TestVariableToolAsksBeforeChanges(t *testing.T) {
	calls := []map[string]any{
		{"action": "create", "name": "API_BASE", "value": "https://api.example.com", "description": ""},
		{"action": "list", "name": "", "value": "", "description": ""},
		{"action": "update", "name": "API_BASE", "value": "", "description": "The API"},
		{"action": "create", "name": "API_BASE", "value": "x"},
		{"action": "delete", "name": "API_BASE"},
	}
	s := setupVariables(t, func(_ []kernel.Message, n int) kernel.ModelResponse {
		if n <= len(calls) {
			return secretToolCalls(secretCall("variable", calls[n-1]))
		}
		return typesReply("done")
	})
	s.chat(t)
	rec := s.llm.recorded()
	var results []string
	for _, c := range rec[1:] {
		if r := lastToolResults(c); len(r) > 0 {
			results = append(results, r[0].Content)
		}
	}
	if len(results) != 5 {
		t.Fatalf("results = %v", results)
	}
	if !strings.Contains(results[0], `Stored variable API_BASE = "https://api.example.com" (yours)`) ||
		!strings.Contains(results[1], "API_BASE") ||
		!strings.Contains(results[2], "The API") || !strings.Contains(results[2], "https://api.example.com") ||
		!strings.Contains(results[3], "already have a variable") ||
		!strings.Contains(results[4], "Deleted variable API_BASE") {
		t.Fatalf("results = %v", results)
	}
	var actions []string
	s.handler.mu.Lock()
	for _, a := range s.handler.approvals {
		var args map[string]any
		_ = json.Unmarshal(a.Arguments, &args)
		action, _ := args["action"].(string)
		actions = append(actions, action)
	}
	s.handler.mu.Unlock()
	if !slices.Equal(actions, []string{"create", "update", "create", "delete"}) {
		t.Fatalf("approvals = %v", actions)
	}
	if own, _ := s.core.VariableHub().List(t.Context(), "agent-a"); len(own) != 0 {
		t.Fatalf("left = %+v", own)
	}
}

func TestVariablesDeletedGlobalLeavesProfilesAndOwnGoWithTheAgent(t *testing.T) {
	s := setupVariables(t, func([]kernel.Message, int) kernel.ModelResponse { return typesReply("ok") })
	ctx := t.Context()
	createVar(t, s.core, "", "A", "1")
	createVar(t, s.core, "", "B", "2")
	s.agent.UpdateVariables(&variable.Config{Use: []string{"A", "B"}})
	createVar(t, s.core, "agent-a", "OWN", "x")
	if err := s.core.VariableHub().Delete(ctx, "", "A"); err != nil {
		t.Fatal(err)
	}
	if v := s.agent.Profile().Variables; v == nil || !slices.Equal(v.Use, []string{"B"}) {
		t.Fatalf("variables = %+v", v)
	}
	if err := s.core.VariableHub().Delete(ctx, "", "B"); err != nil {
		t.Fatal(err)
	}
	if v := s.agent.Profile().Variables; v != nil {
		t.Fatalf("variables = %+v", v)
	}
	if err := s.core.RemoveAgent(ctx, "agent-a"); err != nil {
		t.Fatal(err)
	}
	if own, _ := s.core.VariableHub().List(ctx, "agent-a"); len(own) != 0 {
		t.Fatalf("own left = %+v", own)
	}
}

func TestVariablesRefuseASecretValueThroughTheCore(t *testing.T) {
	s := setupVariables(t, func([]kernel.Message, int) kernel.ModelResponse { return typesReply("ok") })
	ctx := t.Context()
	if _, err := s.core.SecretHub().Create(ctx, secrethub.CreateInput{Definition: secret.Definition{Name: "API_KEY", AllowedHosts: []string{"api.example.com"}}, Value: secretAPIKey}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.core.VariableHub().Create(ctx, variablehub.CreateInput{Definition: variable.Definition{Name: "LEAK", Value: secretAPIKey}}); err == nil {
		t.Fatal("a secret's value became a variable")
	}
}
