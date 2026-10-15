package aec_test

import (
	"context"
	"encoding/json"
	"io"
	"iter"
	"net/http"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"testing"

	aec "github.com/Phong95/aec-agent-go"
	"github.com/Phong95/aec-agent-go/components/agent"
	"github.com/Phong95/aec-agent-go/components/agent/profile"
	"github.com/Phong95/aec-agent-go/components/agent/subagent"
	"github.com/Phong95/aec-agent-go/components/events"
	"github.com/Phong95/aec-agent-go/components/llmhub"
	"github.com/Phong95/aec-agent-go/components/secrethub"
	"github.com/Phong95/aec-agent-go/components/secrethub/secret"
	"github.com/Phong95/aec-agent-go/components/toolhub"
	"github.com/Phong95/aec-agent-go/components/toolhub/builtin/secrets"
	"github.com/Phong95/aec-agent-go/components/toolhub/builtin/subagents"
	"github.com/Phong95/aec-agent-go/components/toolhub/builtin/web"
	"github.com/Phong95/aec-agent-go/kernel"
)

// A-101: an agent using secrets end to end - the "agent secrets" part of
// tests/unit/secrets.test.ts of core/ts. The hub, sealer and rules are tested in
// components/secrethub, the terminal / run_code part in builtin/workspace.

const secretAPIKey = "sk-live-0123456789abcdef"

// secretScript answers model call n (1-based) of the agent.
type secretScript func(msgs []kernel.Message, n int) kernel.ModelResponse

// secretLLM plays the model from a script and records every call.
type secretLLM struct {
	script secretScript
	mu     sync.Mutex
	calls  [][]kernel.Message
}

func (*secretLLM) Name() string    { return "fake" }
func (*secretLLM) Version() string { return "1.0.0" }

func (p *secretLLM) Chat(_ context.Context, msgs []kernel.Message, _ llmhub.ModelOptions) (kernel.ModelResponse, error) {
	p.mu.Lock()
	p.calls = append(p.calls, slices.Clone(msgs))
	n := len(p.calls)
	p.mu.Unlock()
	return p.script(msgs, n), nil
}

func (*secretLLM) Stream(context.Context, []kernel.Message, llmhub.ModelOptions) iter.Seq2[kernel.StreamChunk, error] {
	return func(yield func(kernel.StreamChunk, error) bool) {
		yield(kernel.StreamChunk{Type: kernel.ChunkDone}, nil)
	}
}

func (*secretLLM) CountTokens(context.Context, []kernel.Message) (int, error) { return 10, nil }

func (p *secretLLM) recorded() [][]kernel.Message {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.calls)
}

// everythingSent is every message the model was sent, encoded.
func (p *secretLLM) everythingSent(t *testing.T) string {
	t.Helper()
	raw, err := kernel.EncodeJSON(p.recorded())
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// secretToolCalls is a response that calls tools with the given arguments.
func secretToolCalls(calls ...kernel.ToolCall) kernel.ModelResponse {
	for i := range calls {
		calls[i].ID = "tc-" + string(rune('1'+i))
	}
	return typesToolUse(calls)
}

func secretCall(name string, args map[string]any) kernel.ToolCall {
	raw, _ := json.Marshal(args)
	return kernel.ToolCall{Name: name, Arguments: kernel.Arguments(raw)}
}

// secretHandler approves (or not) and answers RequestSecret with a value ("" = cancel).
type secretHandler struct {
	approve bool
	value   func(r toolhub.SecretRequest) string

	mu        sync.Mutex
	approvals []toolhub.ApprovalRequest
	requests  []toolhub.SecretRequest
}

func (*secretHandler) AskUser(context.Context, toolhub.ClarifyRequest) (toolhub.ClarifyResponse, error) {
	return toolhub.ClarifyResponse{}, nil
}

func (h *secretHandler) RequestApproval(_ context.Context, r toolhub.ApprovalRequest) (toolhub.ApprovalDecision, error) {
	h.mu.Lock()
	h.approvals = append(h.approvals, r)
	h.mu.Unlock()
	return toolhub.ApprovalDecision{Approved: h.approve}, nil
}

func (h *secretHandler) RequestSecret(_ context.Context, r toolhub.SecretRequest) (toolhub.SecretResponse, error) {
	h.mu.Lock()
	h.requests = append(h.requests, r)
	h.mu.Unlock()
	if h.value != nil {
		return toolhub.SecretResponse{Value: h.value(r)}, nil
	}
	return toolhub.SecretResponse{Value: secretAPIKey}, nil
}

func (h *secretHandler) approvalNames() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := []string{}
	for _, a := range h.approvals {
		out = append(out, a.ToolName)
	}
	return out
}

// plainHandler has no RequestSecret: it can't take secret values.
type plainHandler struct{}

func (plainHandler) AskUser(context.Context, toolhub.ClarifyRequest) (toolhub.ClarifyResponse, error) {
	return toolhub.ClarifyResponse{}, nil
}

func (plainHandler) RequestApproval(context.Context, toolhub.ApprovalRequest) (toolhub.ApprovalDecision, error) {
	return toolhub.ApprovalDecision{Approved: true}, nil
}

// fakeHTTP records the requests and answers with respond.
type fakeHTTP struct {
	respond func(r *http.Request, body string) *http.Response
	mu      sync.Mutex
	reqs    []recordedRequest
}

type recordedRequest struct {
	method, url, body string
	header            http.Header
}

func (f *fakeHTTP) RoundTrip(r *http.Request) (*http.Response, error) {
	body := ""
	if r.Body != nil {
		b, _ := io.ReadAll(r.Body)
		body = string(b)
	}
	f.mu.Lock()
	f.reqs = append(f.reqs, recordedRequest{method: r.Method, url: r.URL.String(), body: body, header: r.Header.Clone()})
	f.mu.Unlock()
	resp := f.respond(r, body)
	resp.Request = r
	return resp, nil
}

func (f *fakeHTTP) recorded() []recordedRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.reqs)
}

func httpResponse(status int, body string, header map[string]string) *http.Response {
	h := http.Header{}
	for k, v := range header {
		h.Set(k, v)
	}
	return &http.Response{StatusCode: status, Header: h, Body: io.NopCloser(strings.NewReader(body))}
}

// publicResolver resolves every host to a public address.
type publicResolver struct{}

func (publicResolver) LookupNetIP(context.Context, string, string) ([]netip.Addr, error) {
	return []netip.Addr{netip.MustParseAddr("93.184.216.34")}, nil
}

type secretSetup struct {
	core    *aec.AgentCore
	agent   *agent.Agent
	llm     *secretLLM
	handler *secretHandler
	http    *fakeHTTP
	mu      sync.Mutex
	events  []kernel.Event
}

type secretOptions struct {
	script  secretScript
	respond func(r *http.Request, body string) *http.Response
	value   func(r toolhub.SecretRequest) string
	edit    func(p *profile.AgentProfile)
	noKeys  bool
}

func setupSecrets(t *testing.T, o secretOptions) *secretSetup {
	t.Helper()
	core, err := aec.NewAgentCore()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = core.Dispose(context.WithoutCancel(t.Context())) })
	if !o.noKeys {
		core.UseSecretHubProvider(secrethub.NewEphemeralSecretHubProvider())
	}
	respond := o.respond
	if respond == nil {
		respond = func(*http.Request, string) *http.Response { return httpResponse(200, "ok", nil) }
	}
	s := &secretSetup{core: core, llm: &secretLLM{script: o.script}, handler: &secretHandler{approve: true, value: o.value}, http: &fakeHTTP{respond: respond}}
	for _, h := range secrets.NewTools() {
		if err := core.UseTool(h); err != nil {
			t.Fatal(err)
		}
	}
	for _, h := range []toolhub.Handler{web.NewHTTPRequestTool(web.HTTPRequestToolOptions{Transport: s.http, Resolver: publicResolver{}}), subagents.NewSpawnSubagentTool()} {
		if err := core.UseTool(h); err != nil {
			t.Fatal(err)
		}
	}
	core.ToolHub().SetInteractionHandler(s.handler)
	if err := core.UseLLMProvider(s.llm); err != nil {
		t.Fatal(err)
	}
	core.Events().OnAny(func(e kernel.Event) {
		s.mu.Lock()
		s.events = append(s.events, e)
		s.mu.Unlock()
	})
	zero := 0
	p := profile.AgentProfile{
		ID: "agent-a", Name: "Agent A", SystemPrompt: "You are Agent A.", Provider: "fake",
		Tools:      []string{"http_request", "list_secrets", "manage_secret", "spawn_subagent"},
		LoopConfig: &profile.LoopConfig{MaxToolRetries: &zero, RetryBackoffMs: &zero},
	}
	if o.edit != nil {
		o.edit(&p)
	}
	if s.agent, err = core.SpawnAgent(t.Context(), p); err != nil {
		t.Fatal(err)
	}
	return s
}

func (s *secretSetup) chat(t *testing.T) string {
	t.Helper()
	session, err := s.agent.StartSession(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.agent.Chat(t.Context(), "go", agent.WithChatSession(session.ID)); err != nil {
		t.Fatal(err)
	}
	return session.ID
}

func (s *secretSetup) create(t *testing.T, in secrethub.CreateInput) {
	t.Helper()
	if _, err := s.core.SecretHub().Create(t.Context(), in); err != nil {
		t.Fatal(err)
	}
}

func (s *secretSetup) recordedEvents() []kernel.Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.events)
}

// lastResults is the content of the tool results of model call n (1-based).
func (s *secretSetup) results(t *testing.T, n int) []string {
	t.Helper()
	calls := s.llm.recorded()
	if len(calls) < n {
		t.Fatalf("only %d model calls", len(calls))
	}
	out := []string{}
	for _, r := range resultsOf(calls[n-1]) {
		out = append(out, r.Content)
	}
	return out
}

func def(name string, hosts ...string) secret.Definition {
	return secret.Definition{Name: name, AllowedHosts: hosts}
}

func TestSecrets_HTTPRequestPutsTheValueInTheRequestAndTheModelNeverSeesIt(t *testing.T) {
	s := setupSecrets(t, secretOptions{
		respond: func(r *http.Request, _ string) *http.Response {
			return httpResponse(200, `{"ok":true,"echo":"`+r.Header.Get("Authorization")+`"}`, map[string]string{"content-type": "application/json"})
		},
		script: func(_ []kernel.Message, n int) kernel.ModelResponse {
			if n == 1 {
				return secretToolCalls(secretCall("http_request", map[string]any{"url": "https://api.example.com/v1/forecast", "headers": map[string]any{"Authorization": "Bearer {{secret:WEATHER_KEY}}"}}))
			}
			return typesReply("done")
		},
	})
	desc := "forecasts"
	s.create(t, secrethub.CreateInput{Definition: secret.Definition{Name: "WEATHER_KEY", Description: &desc, AllowedHosts: []string{"api.example.com"}}, Value: secretAPIKey, OwnerAgentID: "agent-a"})
	sessionID := s.chat(t)

	reqs := s.http.recorded()
	if len(reqs) != 1 || reqs[0].header.Get("Authorization") != "Bearer "+secretAPIKey {
		t.Fatalf("requests = %+v", reqs)
	}
	if len(s.handler.approvalNames()) != 0 {
		t.Fatalf("GET asked for approval: %v", s.handler.approvalNames())
	}
	system := systemOf(s.llm.recorded()[0])
	if !strings.Contains(system, "<available_secrets>") || !strings.Contains(system, "WEATHER_KEY — forecasts") {
		t.Fatalf("system = %q", system)
	}
	if res := s.results(t, 2); len(res) != 1 || !strings.Contains(res[0], "Bearer [secret:WEATHER_KEY]") {
		t.Fatalf("result = %v", res)
	}
	if strings.Contains(s.llm.everythingSent(t), secretAPIKey) {
		t.Fatal("the model saw the value")
	}
	stored, err := s.core.PersistenceHub().Messages.Get(t.Context(), sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if raw, _ := kernel.EncodeJSON(stored); strings.Contains(string(raw), secretAPIKey) {
		t.Fatal("the conversation holds the value")
	}
	evs := s.recordedEvents()
	if raw, _ := kernel.EncodeJSON(evs); strings.Contains(string(raw), secretAPIKey) {
		t.Fatal("an event holds the value")
	}
	used := slices.ContainsFunc(evs, func(e kernel.Event) bool {
		if e.Type != events.SecretUsed.Name {
			return false
		}
		p, _ := events.PayloadAs[events.SecretUsedPayload](e)
		return p.Host == "api.example.com" && p.AgentID == "agent-a" && e.SessionID == sessionID
	})
	if !used {
		t.Fatal("no secret:used event")
	}
}

func TestSecrets_RefusesAHostSchemeOrSecretTheAgentMayNotUse(t *testing.T) {
	calls := []map[string]any{
		{"url": "https://evil.example.net/x", "headers": map[string]any{"Authorization": "{{secret:K}}"}},
		{"url": "http://api.example.com/x", "headers": map[string]any{"Authorization": "{{secret:K}}"}},
		{"url": "https://{{secret:K}}.example.com/x"},
		{"url": "https://api.example.com/x", "headers": map[string]any{"Authorization": "{{secret:NOT_MINE}}"}},
	}
	s := setupSecrets(t, secretOptions{script: func(_ []kernel.Message, n int) kernel.ModelResponse {
		if n == 1 {
			tcs := []kernel.ToolCall{}
			for _, a := range calls {
				tcs = append(tcs, secretCall("http_request", a))
			}
			return secretToolCalls(tcs...)
		}
		return typesReply("done")
	}})
	s.create(t, secrethub.CreateInput{Definition: def("K", "api.example.com"), Value: secretAPIKey, OwnerAgentID: "agent-a"})
	s.create(t, secrethub.CreateInput{Definition: def("NOT_MINE", "api.example.com"), Value: secretAPIKey + "x", OwnerAgentID: "agent-b"})
	s.chat(t)
	res := s.results(t, 2)
	wants := []string{"may not be sent to evil.example.net", "only sent over https", "part of the host name", "no secret named NOT_MINE"}
	for i, want := range wants {
		if !strings.Contains(res[i], want) {
			t.Errorf("result %d = %q, want %q", i, res[i], want)
		}
	}
	if n := len(s.http.recorded()); n != 0 {
		t.Fatalf("%d requests were sent", n)
	}
}

func TestSecrets_AGlobalSecretWorksOnlyOnceTheAgentEnablesIt(t *testing.T) {
	call := secretToolCalls(secretCall("http_request", map[string]any{"url": "https://api.example.com/x", "headers": map[string]any{"X-Key": "{{secret:SHARED}}"}}))
	s := setupSecrets(t, secretOptions{script: func(_ []kernel.Message, n int) kernel.ModelResponse {
		if n%2 == 1 {
			return call
		}
		return typesReply("done")
	}})
	s.create(t, secrethub.CreateInput{Definition: def("SHARED", "api.example.com"), Value: secretAPIKey})
	s.chat(t)
	if n := len(s.http.recorded()); n != 0 {
		t.Fatalf("%d requests before the secret was enabled", n)
	}
	s.agent.UpdateSecrets(&secret.Config{Use: []string{"SHARED"}})
	s.chat(t)
	if n := len(s.http.recorded()); n != 1 {
		t.Fatalf("%d requests after the secret was enabled", n)
	}
	// Deleting the global secret drops it from the profile (the last one: the config goes).
	if err := s.core.SecretHub().Delete(t.Context(), "", "SHARED"); err != nil {
		t.Fatal(err)
	}
	if s.agent.Profile().Secrets != nil {
		t.Fatalf("secrets = %+v, want nil", s.agent.Profile().Secrets)
	}
	stored, err := s.core.PersistenceHub().Agents.Load(t.Context(), "agent-a")
	if err != nil || stored == nil || stored.Secrets != nil {
		t.Fatalf("the saved profile keeps the secret: %+v, %v", stored, err)
	}
}

func TestSecrets_POSTAsksForApprovalGETDoesNot(t *testing.T) {
	s := setupSecrets(t, secretOptions{
		respond: func(*http.Request, string) *http.Response { return httpResponse(201, "created", nil) },
		script: func(_ []kernel.Message, n int) kernel.ModelResponse {
			if n == 1 {
				return secretToolCalls(secretCall("http_request", map[string]any{"method": "POST", "url": "https://api.example.com/items", "json": map[string]any{"a": 1}}))
			}
			return typesReply("done")
		},
	})
	s.chat(t)
	if got := s.handler.approvalNames(); !slices.Equal(got, []string{"http_request"}) {
		t.Fatalf("approvals = %v", got)
	}
	reqs := s.http.recorded()
	if len(reqs) != 1 || reqs[0].method != "POST" || reqs[0].body != `{"a":1}` || reqs[0].header.Get("Content-Type") != "application/json" {
		t.Fatalf("requests = %+v", reqs)
	}
}

func TestSecrets_DoesNotFollowARedirectToAnotherHostOnceASecretIsInTheRequest(t *testing.T) {
	s := setupSecrets(t, secretOptions{
		respond: func(r *http.Request, _ string) *http.Response {
			if r.URL.Hostname() == "api.example.com" {
				return httpResponse(302, "", map[string]string{"location": "https://other.example.org/steal"})
			}
			return httpResponse(200, "stolen", nil)
		},
		script: func(_ []kernel.Message, n int) kernel.ModelResponse {
			if n == 1 {
				return secretToolCalls(secretCall("http_request", map[string]any{"url": "https://api.example.com/x", "headers": map[string]any{"A": "{{secret:K}}"}}))
			}
			return typesReply("done")
		},
	})
	s.create(t, secrethub.CreateInput{Definition: def("K", "api.example.com"), Value: secretAPIKey, OwnerAgentID: "agent-a"})
	s.chat(t)
	reqs := s.http.recorded()
	if len(reqs) != 1 || !strings.Contains(reqs[0].url, "api.example.com") {
		t.Fatalf("requests = %+v", reqs)
	}
	if res := s.results(t, 2); !strings.Contains(res[0], "HTTP 302") {
		t.Fatalf("result = %v", res)
	}
}

func TestSecrets_ManageSecretCreateAsksForApprovalThenForTheValue(t *testing.T) {
	s := setupSecrets(t, secretOptions{script: func(_ []kernel.Message, n int) kernel.ModelResponse {
		if n == 1 {
			return secretToolCalls(secretCall("manage_secret", map[string]any{"action": "create", "name": "CRM_TOKEN", "description": "CRM", "allowed_hosts": []string{"crm.example.com"}}))
		}
		return typesReply("done")
	}})
	sessionID := s.chat(t)
	if got := s.handler.approvalNames(); !slices.Equal(got, []string{"manage_secret"}) {
		t.Fatalf("approvals = %v", got)
	}
	s.handler.mu.Lock()
	reqs := slices.Clone(s.handler.requests)
	s.handler.mu.Unlock()
	if len(reqs) != 1 || reqs[0].Name != "CRM_TOKEN" || reqs[0].Description != "CRM" || !slices.Equal(reqs[0].AllowedHosts, []string{"crm.example.com"}) ||
		!slices.Equal(reqs[0].AllowedTools, []string{"http_request"}) || reqs[0].AgentID != "agent-a" || reqs[0].SessionID != sessionID {
		t.Fatalf("secret requests = %+v", reqs)
	}
	info, ok, err := s.core.SecretHub().Get(t.Context(), "agent-a", "CRM_TOKEN")
	if err != nil || !ok || info.CreatedBy != secret.CreatorAgent || !slices.Equal(info.AllowedHosts, []string{"crm.example.com"}) {
		t.Fatalf("stored = %+v, %v, %v", info, ok, err)
	}
	if res := s.results(t, 2); !strings.Contains(res[0], "Stored secret CRM_TOKEN") {
		t.Fatalf("result = %v", res)
	}
	if strings.Contains(s.llm.everythingSent(t), secretAPIKey) {
		t.Fatal("the model saw the value")
	}
	stored, _ := s.core.PersistenceHub().Messages.Get(t.Context(), sessionID)
	if raw, _ := kernel.EncodeJSON(stored); strings.Contains(string(raw), secretAPIKey) {
		t.Fatal("the conversation holds the value")
	}
}

func TestSecrets_ManageSecretCancelledOrNoSecretInput(t *testing.T) {
	create := func(_ []kernel.Message, n int) kernel.ModelResponse {
		if n == 1 {
			return secretToolCalls(secretCall("manage_secret", map[string]any{"action": "create", "name": "K"}))
		}
		return typesReply("done")
	}
	s := setupSecrets(t, secretOptions{script: create, value: func(toolhub.SecretRequest) string { return "" }})
	s.chat(t)
	if res := s.results(t, 2); !strings.Contains(res[0], "cancelled") {
		t.Fatalf("result = %v", res)
	}
	if _, ok, _ := s.core.SecretHub().Get(t.Context(), "agent-a", "K"); ok {
		t.Fatal("a cancelled entry stored a secret")
	}

	u := setupSecrets(t, secretOptions{script: create})
	u.core.ToolHub().SetInteractionHandler(plainHandler{})
	u.chat(t)
	if res := u.results(t, 2); !strings.Contains(res[0], "no one can enter a secret value here") {
		t.Fatalf("result = %v", res)
	}
}

func TestSecrets_ManageSecretCannotTouchAGlobalSecretAndListShowsMetadataOnly(t *testing.T) {
	s := setupSecrets(t, secretOptions{
		edit: func(p *profile.AgentProfile) { p.Secrets = &secret.Config{Use: []string{"SHARED"}} },
		script: func(_ []kernel.Message, n int) kernel.ModelResponse {
			if n == 1 {
				return secretToolCalls(secretCall("manage_secret", map[string]any{"action": "delete", "name": "SHARED"}), secretCall("list_secrets", map[string]any{}))
			}
			return typesReply("done")
		},
	})
	desc := "shared key"
	s.create(t, secrethub.CreateInput{Definition: secret.Definition{Name: "SHARED", Description: &desc, AllowedHosts: []string{"a.example.com"}}, Value: secretAPIKey})
	s.chat(t)
	res := s.results(t, 2)
	if !strings.Contains(res[0], "you have no secret named SHARED") {
		t.Fatalf("delete = %q", res[0])
	}
	if !strings.Contains(res[1], "SHARED (global) · shared key · tools: http_request · hosts: a.example.com") || strings.Contains(res[1], secretAPIKey) {
		t.Fatalf("list = %q", res[1])
	}
	if _, ok, _ := s.core.SecretHub().Get(t.Context(), "", "SHARED"); !ok {
		t.Fatal("the global secret was deleted")
	}
}

func TestSecrets_RemovingTheAgentDeletesItsOwnSecretsButKeepsGlobalOnes(t *testing.T) {
	s := setupSecrets(t, secretOptions{script: func([]kernel.Message, int) kernel.ModelResponse { return typesReply("ok") }})
	s.create(t, secrethub.CreateInput{Definition: def("OWN"), Value: secretAPIKey, OwnerAgentID: "agent-a"})
	s.create(t, secrethub.CreateInput{Definition: def("GLOBAL"), Value: secretAPIKey})
	if err := s.core.RemoveAgent(t.Context(), "agent-a"); err != nil {
		t.Fatal(err)
	}
	if own, _ := s.core.SecretHub().List(t.Context(), "agent-a"); len(own) != 0 {
		t.Fatalf("own secrets = %+v", own)
	}
	all, _ := s.core.SecretHub().ListAll(t.Context())
	if len(all) != 1 || all[0].Name != "GLOBAL" {
		t.Fatalf("all = %+v", all)
	}
}

func TestSecrets_SubagentTypesNeverGetManageSecretAndSeeTheParentsSecrets(t *testing.T) {
	s := setupSecrets(t, secretOptions{
		edit: func(p *profile.AgentProfile) {
			p.Subagents = &subagent.Config{Allow: []subagent.TargetConfig{}, Types: []string{"general-purpose"}}
		},
		script: func(msgs []kernel.Message, _ int) kernel.ModelResponse {
			system := systemOf(msgs)
			if strings.Contains(system, "You are Agent A.") {
				if len(resultsOf(msgs)) > 0 {
					return typesReply("parent done")
				}
				return secretToolCalls(secretCall("spawn_subagent", map[string]any{"subagent": "general-purpose", "task": "look"}))
			}
			manage, key := "no-manage", "no key"
			if strings.Contains(system, "manage_secret") {
				manage = "manage"
			}
			if strings.Contains(system, "CHILD_KEY") {
				key = "sees key name"
			}
			return typesReply("child tools: " + manage + "; " + key)
		},
	})
	s.create(t, secrethub.CreateInput{Definition: def("CHILD_KEY", "a.example.com"), Value: secretAPIKey, OwnerAgentID: "agent-a"})
	s.chat(t)
	calls := s.llm.recorded()
	last := resultsOf(calls[len(calls)-1])
	if len(last) != 1 || !strings.Contains(last[0].Content, "no-manage") || !strings.Contains(last[0].Content, "sees key name") {
		t.Fatalf("child reply = %+v", last)
	}
}
