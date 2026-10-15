package e2e_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"slices"
	"strings"
	"testing"

	aec "github.com/Phong95/aec-agent-go"
	"github.com/Phong95/aec-agent-go/aecmock"
	"github.com/Phong95/aec-agent-go/components/agent"
	"github.com/Phong95/aec-agent-go/components/agent/profile"
	"github.com/Phong95/aec-agent-go/components/llmhub"
	"github.com/Phong95/aec-agent-go/components/persistence"
	"github.com/Phong95/aec-agent-go/components/skillhub"
	"github.com/Phong95/aec-agent-go/components/skillhub/skill"
	"github.com/Phong95/aec-agent-go/components/toolhub"
	"github.com/Phong95/aec-agent-go/components/toolhub/builtin/clarify"
	"github.com/Phong95/aec-agent-go/kernel"
)

// TestCreateAgent_ClarifyEndToEnd is the tracer of Phase 11: one CreateAgent call builds
// an agent whose model asks the user through the clarify tool; the interaction handler on
// the tool hub receives the question with the right ids and its answer comes back to the
// model as the tool result inside the same turn.
func TestCreateAgent_ClarifyEndToEnd(t *testing.T) {
	ctx := t.Context()

	core, err := aec.NewAgentCore()
	if err != nil {
		t.Fatalf("NewAgentCore: %v", err)
	}
	handler := aecmock.NewScriptedInteraction() // answers "Tower A"
	core.ToolHub().SetInteractionHandler(handler)

	provider := aecmock.NewScriptedProvider("scripted", []aecmock.ScriptedTurn{
		toolTurn(toolCallOf("tc-1", "clarify", `{"question":"Which tower?","choices":["Tower A","Tower B"]}`)),
		{Content: "Done"},
	})
	reg := llmhub.NewRegistry()
	reg.Register("scripted", func(llmhub.ProviderConfig) (llmhub.Provider, error) { return provider, nil })

	a, err := aec.CreateAgent(ctx, aec.AgentConfig{
		Provider: "scripted",
		APIKey:   "k",
		Model:    "m",
		Tools:    []toolhub.Handler{clarify.NewTool()},
	}, aec.WithCore(core), aec.WithProviderRegistry(reg))
	if err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}
	disposed := false
	dispose := func() {
		t.Helper()
		if disposed {
			return
		}
		disposed = true
		if err := a.Dispose(ctx); err != nil {
			t.Errorf("Dispose: %v", err)
		}
	}
	defer dispose()

	session, err := a.StartSession(ctx)
	if err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	res, err := a.Chat(ctx, "Pick one")
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if res.Message.Content != "Done" {
		t.Errorf("reply = %q, want Done", res.Message.Content)
	}

	asked := handler.Asked()
	if len(asked) != 1 {
		t.Fatalf("AskUser was called %d times, want once: %+v", len(asked), asked)
	}
	got := asked[0]
	if got.SessionID != session.ID || got.AgentID != a.ID() || got.Question != "Which tower?" {
		t.Errorf("request = %+v, want session %q agent %q question %q", got, session.ID, a.ID(), "Which tower?")
	}
	if len(got.Choices) != 2 || got.Choices[0] != "Tower A" || got.Choices[1] != "Tower B" {
		t.Errorf("choices = %v, want [Tower A Tower B]", got.Choices)
	}

	// The answer reached the model in the same turn: the second call carries the tool result.
	var toolContent string
	for _, call := range provider.Calls() {
		for _, m := range call.Messages {
			for _, r := range m.ToolResults {
				toolContent = r.Content
			}
		}
	}
	if toolContent != "User's answer: Tower A" {
		t.Errorf("tool result seen by the model = %q, want %q", toolContent, "User's answer: Tower A")
	}

	p := a.Profile()
	if p.Name != "scripted-agent" {
		t.Errorf("profile name = %q, want scripted-agent", p.Name)
	}
	if len(p.Tools) != 1 || p.Tools[0] != "clarify" {
		t.Errorf("profile tools = %v, want [clarify]", p.Tools)
	}
	dispose()
}

// ---------------------------------------------------------------------------
// TestCreateAgent: the provider cases and the rest of the CreateAgent contract
// ---------------------------------------------------------------------------

// uuidV4 is the shape of crypto.randomUUID and kernel.NewID.
var uuidV4 = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

// factoryLog records the configuration every factory of a mockRegistry received.
type factoryLog struct{ configs []llmhub.ProviderConfig }

// mockRegistry is a private registry whose factory under each name returns an
// aecmock.MockProvider and records the configuration it was given, so no test touches the
// process-wide registry of llm.
func mockRegistry(names ...string) (*llmhub.Registry, *factoryLog) {
	reg := llmhub.NewRegistry()
	log := &factoryLog{}
	for _, name := range names {
		reg.Register(name, func(cfg llmhub.ProviderConfig) (llmhub.Provider, error) {
			log.configs = append(log.configs, cfg)
			return aecmock.NewMockProvider(name, func([]kernel.Message) string { return "ok" }), nil
		})
	}
	return reg, log
}

// createAgent calls CreateAgent and disposes the agent when the test ends (the memory
// defaults start a consolidation scheduler goroutine per agent, and the package runs goleak).
func createAgent(t *testing.T, cfg aec.AgentConfig, opts ...aec.CreateAgentOption) *agent.Agent {
	t.Helper()
	a, err := aec.CreateAgent(t.Context(), cfg, opts...)
	if err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}
	disposeAgent(t, a)
	return a
}

// disposeAgent registers the disposal of an agent as a cleanup of the test.
func disposeAgent(t *testing.T, a *agent.Agent) {
	t.Helper()
	t.Cleanup(func() {
		if err := a.Dispose(context.WithoutCancel(t.Context())); err != nil {
			t.Errorf("Dispose: %v", err)
		}
	})
}

func newCoreOnly(t *testing.T) *aec.AgentCore {
	t.Helper()
	core, err := aec.NewAgentCore()
	if err != nil {
		t.Fatalf("NewAgentCore: %v", err)
	}
	return core
}

func TestCreateAgent(t *testing.T) {
	// ---- the four provider cases ----

	t.Run("creates an agent with anthropic provider", func(t *testing.T) {
		reg, _ := mockRegistry("anthropic")
		a := createAgent(t, aec.AgentConfig{Provider: "anthropic", APIKey: "test-key", Model: "claude-sonnet-4-20250514"}, aec.WithProviderRegistry(reg))
		if got := a.Profile().Name; got != "anthropic-agent" {
			t.Errorf("profile name = %q, want anthropic-agent", got)
		}
	})

	t.Run("creates an agent with novita provider", func(t *testing.T) {
		reg, _ := mockRegistry("novita")
		a := createAgent(t, aec.AgentConfig{Provider: "novita", APIKey: "test-key", Model: "deepseek/deepseek-v3.2"}, aec.WithProviderRegistry(reg))
		if got := a.Profile().Name; got != "novita-agent" {
			t.Errorf("profile name = %q, want novita-agent", got)
		}
	})

	t.Run("creates an agent with custom system prompt", func(t *testing.T) {
		reg, _ := mockRegistry("anthropic")
		a := createAgent(t, aec.AgentConfig{
			Provider: "anthropic", APIKey: "test-key", Model: "claude-sonnet-4-20250514",
			SystemPrompt: "You are a construction project manager.",
		}, aec.WithProviderRegistry(reg))
		if got := a.Profile().SystemPrompt; got != "You are a construction project manager." {
			t.Errorf("system prompt = %q", got)
		}
	})

	t.Run("throws on unknown provider", func(t *testing.T) {
		reg, _ := mockRegistry("anthropic", "novita")
		a, err := aec.CreateAgent(t.Context(), aec.AgentConfig{Provider: "unknown", APIKey: "x", Model: "y"}, aec.WithProviderRegistry(reg))
		if a != nil {
			disposeAgent(t, a)
			t.Error("CreateAgent returned an agent for an unknown provider")
		}
		if err == nil || !strings.Contains(err.Error(), `Unknown provider "unknown"`) {
			t.Fatalf("error = %v, want one containing Unknown provider \"unknown\"", err)
		}
		if !errors.Is(err, llmhub.ErrUnknownProvider) {
			t.Errorf("error %v does not match llmhub.ErrUnknownProvider", err)
		}
	})

	// ---- defaults and the config rules ----

	t.Run("defaults", func(t *testing.T) {
		a := createAgent(t, aec.AgentConfig{})
		p := a.Profile()
		if !uuidV4.MatchString(p.ID) {
			t.Errorf("id = %q, want a UUID v4", p.ID)
		}
		if p.Name != "agent" || p.SystemPrompt != "You are a helpful assistant." || p.Provider != "" {
			t.Errorf("name %q, prompt %q, provider %q, want agent, the default prompt and no provider", p.Name, p.SystemPrompt, p.Provider)
		}
		if want := skillhub.EssentialSkillNames(); !slices.Equal(p.Skills, want) {
			t.Errorf("skills = %v, want the essential names %v", p.Skills, want)
		}
		if len(p.Tools) != 0 {
			t.Errorf("tools = %v, want none", p.Tools)
		}
		wantCompactor := profile.CompactorConfig{
			ContextWindowTokens: 200_000, Threshold: 0.8, MinTailUserMessages: 4, ProtectFirstN: 2,
			ProactivePruneTokens: 50_000, OldToolResultMinTokens: 2_000, TailTargetRatio: 0.2, ProtectLastN: 10,
			SummarizeMaxOutputTokens: 4_096,
		}
		if p.Compactor != wantCompactor {
			t.Errorf("compactor = %+v, want %+v", p.Compactor, wantCompactor)
		}
		m := p.Memory
		if !m.Enabled || m.UpdateSoul || !m.UpdateUser || m.Consolidation == nil || m.Consolidation.IntervalMinutes == nil || *m.Consolidation.IntervalMinutes != 30 {
			t.Errorf("memory = %+v, want enabled, updateUser, no updateSoul, consolidation every 30 minutes", m)
		}
		if p.InferenceConfig != nil || p.LoopConfig != nil || p.InitialContext != nil || p.LLMModel != "" {
			t.Errorf("profile has unexpected optional parts: %+v", p)
		}
	})

	t.Run("ids are unique", func(t *testing.T) {
		core := newCoreOnly(t)
		a := createAgent(t, aec.AgentConfig{}, aec.WithCore(core))
		b := createAgent(t, aec.AgentConfig{}, aec.WithCore(core))
		if a.ID() == b.ID() {
			t.Errorf("two agents share the id %q", a.ID())
		}
	})

	t.Run("a configured provider is registered on the core", func(t *testing.T) {
		core := newCoreOnly(t)
		reg, log := mockRegistry("anthropic")
		createAgent(t, aec.AgentConfig{Provider: "anthropic", APIKey: "k", Model: "m", BaseURL: "https://gw.example"}, aec.WithCore(core), aec.WithProviderRegistry(reg))
		providers := core.LLMHub().List()
		if len(providers) != 1 || providers[0].Name() != "anthropic" {
			t.Errorf("providers = %v, want the anthropic provider", providers)
		}
		if len(log.configs) != 1 || log.configs[0].APIKey != "k" || log.configs[0].Model != "m" || log.configs[0].BaseURL != "https://gw.example" {
			t.Errorf("factory configs = %+v", log.configs)
		}
	})

	t.Run("a named provider is configured without a key or a model", func(t *testing.T) {
		// CreateAgent always resolves a named provider (the factory decides whether the
		// config is enough), so the profile never names a provider that was not configured;
		// without a name nothing is configured and the profile names none.
		for name, cfg := range map[string]aec.AgentConfig{
			"no key":   {Provider: "anthropic", Model: "m"},
			"no model": {Provider: "anthropic", APIKey: "k"},
		} {
			t.Run(name, func(t *testing.T) {
				core := newCoreOnly(t)
				reg, log := mockRegistry("anthropic")
				a := createAgent(t, cfg, aec.WithCore(core), aec.WithProviderRegistry(reg))
				if providers := core.LLMHub().List(); len(providers) != 1 || providers[0].Name() != "anthropic" {
					t.Errorf("providers = %v, want the anthropic provider", providers)
				}
				if len(log.configs) != 1 || log.configs[0].APIKey != cfg.APIKey || log.configs[0].Model != cfg.Model {
					t.Errorf("factory configs = %+v, want the config passed on as it is", log.configs)
				}
				if p := a.Profile(); p.Name != "anthropic-agent" || p.Provider != "anthropic" {
					t.Errorf("profile name %q provider %q, want anthropic-agent and anthropic", p.Name, p.Provider)
				}
			})
		}
		t.Run("no provider", func(t *testing.T) {
			core := newCoreOnly(t)
			reg, log := mockRegistry("anthropic")
			a := createAgent(t, aec.AgentConfig{APIKey: "k", Model: "m"}, aec.WithCore(core), aec.WithProviderRegistry(reg))
			if n := len(core.LLMHub().List()); n != 0 || len(log.configs) != 0 {
				t.Errorf("%d providers, %d factory calls, want none", n, len(log.configs))
			}
			if p := a.Profile(); p.Name != "agent" || p.Provider != "" {
				t.Errorf("profile name %q provider %q, want agent and no provider", p.Name, p.Provider)
			}
		})
	})

	t.Run("a factory that refuses the config fails CreateAgent", func(t *testing.T) {
		reg := llmhub.NewRegistry()
		errNoKey := errors.New("anthropic: an API key is required")
		reg.Register("anthropic", func(cfg llmhub.ProviderConfig) (llmhub.Provider, error) {
			if cfg.APIKey == "" {
				return nil, errNoKey
			}
			return aecmock.NewMockProvider("anthropic", func([]kernel.Message) string { return "ok" }), nil
		})
		core := newCoreOnly(t)
		a, err := aec.CreateAgent(t.Context(), aec.AgentConfig{Provider: "anthropic", Model: "m"}, aec.WithCore(core), aec.WithProviderRegistry(reg))
		if !errors.Is(err, errNoKey) || a != nil {
			t.Fatalf("CreateAgent = %v, %v, want the factory error and no agent", a, err)
		}
		if n := len(core.ListAgents()); n != 0 {
			t.Errorf("%d agents spawned, want none", n)
		}
	})

	t.Run("no tools registers nothing", func(t *testing.T) {
		// CONTEXT "No auto-registered built-ins": only config.Tools are registered.
		core := newCoreOnly(t)
		a := createAgent(t, aec.AgentConfig{}, aec.WithCore(core))
		if tools := core.ToolHub().List(); len(tools) != 0 {
			t.Errorf("tool hub holds %d tools, want none", len(tools))
		}
		if p := a.Profile(); len(p.Tools) != 0 {
			t.Errorf("profile tools = %v, want none", p.Tools)
		}
		stored, err := core.PersistenceHub().Agents.Load(t.Context(), a.ID())
		if err != nil || stored == nil || len(stored.Tools) != 0 {
			t.Errorf("stored profile = %+v, %v, want an empty tool list", stored, err)
		}
	})

	t.Run("tools are registered and named in the profile in order", func(t *testing.T) {
		core := newCoreOnly(t)
		first := newLoopTool("alpha", sentOK)
		second := newLoopTool("beta", sentOK)
		a := createAgent(t, aec.AgentConfig{Tools: []toolhub.Handler{first, second}}, aec.WithCore(core))
		if p := a.Profile(); !slices.Equal(p.Tools, []string{"alpha", "beta"}) {
			t.Errorf("profile tools = %v, want [alpha beta]", p.Tools)
		}
		if _, ok := core.ToolHub().Get("beta"); !ok {
			t.Error("beta is not registered on the core")
		}
	})

	t.Run("duplicate tool", func(t *testing.T) {
		core := newCoreOnly(t)
		a, err := aec.CreateAgent(t.Context(), aec.AgentConfig{Tools: []toolhub.Handler{clarify.NewTool(), clarify.NewTool()}}, aec.WithCore(core))
		if a != nil {
			disposeAgent(t, a)
			t.Error("CreateAgent returned an agent after a duplicate tool")
		}
		if !errors.Is(err, toolhub.ErrAlreadyRegistered) || err == nil || err.Error() != `Tool "clarify" already registered` {
			t.Fatalf("error = %v, want the hub's duplicate error", err)
		}
		if n := len(core.ListAgents()); n != 0 {
			t.Errorf("%d agents spawned, want none", n)
		}
	})

	t.Run("nil tool", func(t *testing.T) {
		a, err := aec.CreateAgent(t.Context(), aec.AgentConfig{Tools: []toolhub.Handler{nil}})
		if a != nil {
			disposeAgent(t, a)
		}
		if !errors.Is(err, toolhub.ErrNilHandler) {
			t.Fatalf("error = %v, want toolhub.ErrNilHandler", err)
		}
	})

	t.Run("skills are essential then app", func(t *testing.T) {
		core := newCoreOnly(t)
		diary := skill.Package{Content: "---\nname: site-diary\ndescription: Daily diary.\n---\n\nWrite the diary.\n"}
		siteLog := skill.Package{Content: "---\nname: site-log\ndescription: Site log.\n---\n\nWrite the log.\n"}
		a := createAgent(t, aec.AgentConfig{Skills: []skill.Package{diary, siteLog}}, aec.WithCore(core))
		want := append(skillhub.EssentialSkillNames(), "site-diary", "site-log")
		if p := a.Profile(); !slices.Equal(p.Skills, want) {
			t.Errorf("skills = %v, want %v", p.Skills, want)
		}
		sk, found, err := core.SkillHub().Get(t.Context(), "site-diary")
		if err != nil || !found || sk.Source != skill.SourceApp {
			t.Errorf("site-diary = %+v, %v, %v, want an app skill", sk, found, err)
		}
	})

	t.Run("an invalid app skill is an error", func(t *testing.T) {
		a, err := aec.CreateAgent(t.Context(), aec.AgentConfig{Skills: []skill.Package{{Content: "no front matter"}}})
		if a != nil {
			disposeAgent(t, a)
		}
		if err == nil {
			t.Fatal("CreateAgent accepted an invalid skill package")
		}
	})

	t.Run("a skill that is taken is an error", func(t *testing.T) {
		core := newCoreOnly(t)
		taken := skill.Package{Content: "---\nname: plan\ndescription: Another plan.\n---\n\nbody\n"}
		a, err := aec.CreateAgent(t.Context(), aec.AgentConfig{Skills: []skill.Package{taken}}, aec.WithCore(core))
		if a != nil {
			disposeAgent(t, a)
		}
		if err == nil {
			t.Fatal("CreateAgent accepted a skill whose name is an essential skill")
		}
	})

	t.Run("WithCore", func(t *testing.T) {
		core := newCoreOnly(t)
		a := createAgent(t, aec.AgentConfig{Tools: []toolhub.Handler{clarify.NewTool()}}, aec.WithCore(core))
		if got, ok := core.Agent(a.ID()); !ok || got != a {
			t.Errorf("the agent is not registered on the given core: %v, %v", got, ok)
		}
		if _, ok := core.ToolHub().Get("clarify"); !ok {
			t.Error("the tool is not registered on the given core")
		}
	})

	t.Run("nil options keep the defaults", func(t *testing.T) {
		var nilCore *aec.AgentCore
		var nilRegistry *llmhub.Registry
		a := createAgent(t, aec.AgentConfig{}, nil, aec.WithCore(nilCore), aec.WithProviderRegistry(nilRegistry))
		if a == nil {
			t.Fatal("no agent")
		}
	})

	t.Run("the default registry is used without WithProviderRegistry", func(t *testing.T) {
		a, err := aec.CreateAgent(t.Context(), aec.AgentConfig{Provider: "no-such-provider", APIKey: "k", Model: "m"})
		if a != nil {
			disposeAgent(t, a)
		}
		if err == nil || !strings.Contains(err.Error(), `Unknown provider "no-such-provider"`) {
			t.Fatalf("error = %v, want the TS unknown provider text", err)
		}
	})

	t.Run("Persistence", func(t *testing.T) {
		core := newCoreOnly(t)
		hub := persistence.NewPersistenceHub()
		a := createAgent(t, aec.AgentConfig{Persistence: hub}, aec.WithCore(core))
		if core.PersistenceHub() != hub {
			t.Error("the persistence hub of the config did not replace the one of the core")
		}
		stored, err := hub.Agents.Load(t.Context(), a.ID())
		if err != nil || stored == nil || stored.ID != a.ID() {
			t.Errorf("stored profile = %+v, %v, want the agent's profile in the new hub", stored, err)
		}
	})

	t.Run("Bedrock fields reach the provider config", func(t *testing.T) {
		reg, log := mockRegistry("bedrock")
		createAgent(t, aec.AgentConfig{
			Provider: "bedrock", APIKey: "AKIA-ID", SecretKey: "aws-secret", Region: "eu-west-1",
			Model: "anthropic.claude-sonnet-4", BaseURL: "https://bedrock.example",
		}, aec.WithProviderRegistry(reg))
		want := llmhub.ProviderConfig{APIKey: "AKIA-ID", SecretKey: "aws-secret", Region: "eu-west-1", Model: "anthropic.claude-sonnet-4", BaseURL: "https://bedrock.example"}
		if len(log.configs) != 1 || log.configs[0] != want {
			t.Errorf("factory configs = %+v, want %+v", log.configs, want)
		}
	})

	t.Run("save failure returns the agent and the error", func(t *testing.T) {
		boom := errors.New("store is down")
		hub := persistence.NewPersistenceHub(persistence.WithAgents(failingAgents{AgentStore: persistence.NewMemoryAgentStore(), err: boom}))
		core := newCoreOnly(t)
		a, err := aec.CreateAgent(t.Context(), aec.AgentConfig{Persistence: hub}, aec.WithCore(core))
		if a != nil {
			disposeAgent(t, a)
		}
		if !errors.Is(err, boom) {
			t.Fatalf("error = %v, want the save error", err)
		}
		if a == nil {
			t.Fatal("CreateAgent returned no agent with the save error")
		}
		if got, ok := core.Agent(a.ID()); !ok || got != a {
			t.Error("the agent is not registered on the core after a save failure")
		}
	})

	t.Run("redaction", func(t *testing.T) {
		const (
			apiKey    = "sk-SECRET-api-123"
			secretKey = "aws-SECRET-key-456"
			baseURL   = "https://user:pw-SECRET@gw.example/v1/tok-SECRET?k=SECRET#frag-SECRET"
		)
		cfg := aec.AgentConfig{
			Provider: "bedrock", APIKey: apiKey, SecretKey: secretKey, Region: "eu-west-1", Model: "m", BaseURL: baseURL,
			SystemPrompt: "prompt-SECRET", Tools: []toolhub.Handler{clarify.NewTool()},
		}
		holder := struct{ Config aec.AgentConfig }{cfg}

		check := func(t *testing.T, verb, got string) {
			t.Helper()
			if strings.Contains(got, "SECRET") || strings.Contains(got, apiKey) || strings.Contains(got, secretKey) {
				t.Errorf("%s printed a secret: %s", verb, got)
			}
			if !strings.Contains(got, "[REDACTED]") {
				t.Errorf("%s shows no redaction marker: %s", verb, got)
			}
			if !strings.Contains(got, "bedrock") || !strings.Contains(got, "eu-west-1") {
				t.Errorf("%s lost the non-secret fields: %s", verb, got)
			}
		}
		for _, verb := range []string{"%v", "%+v", "%#v", "%s", "%q", "%x", "%d"} {
			check(t, verb, fmt.Sprintf(verb, cfg))
			check(t, verb+" of a pointer", fmt.Sprintf(verb, &cfg))
		}
		check(t, "%+v of a struct holding it", fmt.Sprintf("%+v", holder))
		check(t, "String", cfg.String())
		check(t, "GoString", cfg.GoString())

		// json.Marshal (WR-B02) gives the redacted view too, for the value, a pointer, a struct
		// holding it and a slice of them, and the object is valid JSON.
		for name, v := range map[string]any{"value": cfg, "pointer": &cfg, "holder": holder, "slice": []aec.AgentConfig{cfg}} {
			raw, err := json.Marshal(v)
			if err != nil {
				t.Fatalf("json.Marshal(%s): %v", name, err)
			}
			check(t, "json.Marshal of a "+name, string(raw))
			if !json.Valid(raw) {
				t.Errorf("json.Marshal(%s) is not valid JSON: %s", name, raw)
			}
		}
		raw, _ := json.Marshal(cfg)
		var decoded map[string]any
		if err := json.Unmarshal(raw, &decoded); err != nil || decoded["apiKey"] != "[REDACTED]" || decoded["secretKey"] != "[REDACTED]" || decoded["region"] != "eu-west-1" || decoded["tools"] != float64(1) {
			t.Errorf("json form = %s (%v)", raw, err)
		}

		var buf bytes.Buffer
		slog.New(slog.NewJSONHandler(&buf, nil)).Info("created", "config", cfg)
		check(t, "slog JSON", buf.String())
		buf.Reset()
		slog.New(slog.NewTextHandler(&buf, nil)).Info("created", slog.Any("config", cfg))
		check(t, "slog text", buf.String())

		// An error of CreateAgent never carries the credentials either.
		reg, _ := mockRegistry("other")
		_, err := aec.CreateAgent(t.Context(), cfg, aec.WithProviderRegistry(reg))
		if err == nil || strings.Contains(err.Error(), "SECRET") {
			t.Errorf("error = %v, want an unknown provider error without any secret", err)
		}
	})
}

// AgentConfig.LLM is a provider built in code: it is registered as it is and names the
// profile's provider, Model names the profile's model, and the Profile hook runs last.
func TestCreateAgent_LLMAndProfileHook(t *testing.T) {
	ctx := t.Context()

	t.Run("a provider of LLM is used as it is and the hook changes the profile", func(t *testing.T) {
		provider := aecmock.NewScriptedProvider("custom", []aecmock.ScriptedTurn{{Content: "from custom"}})
		maxIterations := 3
		a, err := aec.CreateAgent(ctx, aec.AgentConfig{
			LLM:   provider,
			Model: "custom-model",
			Profile: func(p *profile.AgentProfile) {
				p.Name = "site-agent"
				p.LoopConfig = &profile.LoopConfig{MaxIterations: &maxIterations}
			},
		})
		if err != nil {
			t.Fatalf("CreateAgent: %v", err)
		}
		defer func() {
			if err := a.Dispose(ctx); err != nil {
				t.Errorf("Dispose: %v", err)
			}
		}()
		p := a.Profile()
		if p.Provider != "custom" || p.LLMModel != "custom-model" || p.Name != "site-agent" {
			t.Fatalf("profile provider/model/name = %q/%q/%q, want custom/custom-model/site-agent", p.Provider, p.LLMModel, p.Name)
		}
		if p.LoopConfig == nil || p.LoopConfig.MaxIterations == nil || *p.LoopConfig.MaxIterations != 3 {
			t.Fatalf("loop config = %+v, want the hook's", p.LoopConfig)
		}
		if _, err := a.StartSession(ctx); err != nil {
			t.Fatal(err)
		}
		res, err := a.Chat(ctx, "hi")
		if err != nil {
			t.Fatalf("Chat: %v", err)
		}
		if res.Message.Content != "from custom" {
			t.Fatalf("reply = %q, want from custom", res.Message.Content)
		}
	})

	t.Run("a Provider that does not name LLM is an error", func(t *testing.T) {
		provider := aecmock.NewScriptedProvider("custom", nil)
		_, err := aec.CreateAgent(ctx, aec.AgentConfig{LLM: provider, Provider: "anthropic"})
		if err == nil || !strings.Contains(err.Error(), "does not name") {
			t.Fatalf("CreateAgent = %v, want the mismatch error", err)
		}
	})
}
