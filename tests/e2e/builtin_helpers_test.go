package e2e_test

import (
	"testing"

	aec "github.com/Phong95/aec-agent-go"
	"github.com/Phong95/aec-agent-go/aecmock"
	"github.com/Phong95/aec-agent-go/components/llmhub"
	"github.com/Phong95/aec-agent-go/components/sandboxhub"
	"github.com/Phong95/aec-agent-go/components/toolhub"
)

// This file is the shared setup of the root-level tests of the built-in tools (Phase 11):
// an AgentCore with in-memory sandbox providers and a private provider registry, so no
// test touches the process-wide registry of llm.

// workspaceCoreOptions tunes newWorkspaceCore. The zero value is one in-memory sandbox
// provider of kind docker, registered as the shared sandbox of the core.
type workspaceCoreOptions struct {
	// withHost also registers an in-memory provider of kind host and lets an agent pick
	// it for its own sandbox (the hub policy's own kinds become host). The sandbox of an
	// agent with sandbox { own: {} } is then a host folder, which is not isolated, so the
	// hub's default approval policy gates writes and commands there.
	withHost bool
	// hook becomes the authorization hook of the default tool policy.
	hook toolhub.AuthorizationHook
	// interaction becomes the interaction handler of the tool hub.
	interaction toolhub.InteractionHandler
}

// workspaceCore is an AgentCore with its sandbox providers and the recorders a test reads.
type workspaceCore struct {
	core   *aec.AgentCore
	docker *aecmock.InMemoryProvider
	host   *aecmock.InMemoryProvider // nil without withHost
	spans  *spanLog
	log    *eventLog
}

// newWorkspaceCore builds an AgentCore whose shared sandbox is an in-memory provider (the
// pattern of agentcore_sandbox_test.go) with a recording trace sink and event recorder.
// Hand the core to aec.CreateAgent with aec.WithCore, or spawn an agent on it.
func newWorkspaceCore(t *testing.T, o workspaceCoreOptions) *workspaceCore {
	t.Helper()
	core, err := aec.NewAgentCore()
	if err != nil {
		t.Fatalf("NewAgentCore: %v", err)
	}
	w := &workspaceCore{core: core, docker: aecmock.NewInMemoryProvider()}
	if err := core.UseSandboxProvider(w.docker); err != nil {
		t.Fatalf("UseSandboxProvider: %v", err)
	}
	if o.withHost {
		w.host = aecmock.NewInMemoryProvider(aecmock.WithKind(sandboxhub.KindHost))
		if err := core.UseSandboxProvider(w.host); err != nil {
			t.Fatalf("UseSandboxProvider (host): %v", err)
		}
		own, defaultOwn := []sandboxhub.Kind{sandboxhub.KindHost}, sandboxhub.KindHost
		core.SandboxHub().SetPolicy(sandboxhub.PolicyUpdate{OwnKinds: &own, DefaultOwnKind: &defaultOwn})
	}
	if err := core.SandboxHub().ConfigureShared(sandboxhub.Spec{Kind: sandboxhub.KindDocker}); err != nil {
		t.Fatalf("ConfigureShared: %v", err)
	}
	if o.hook != nil {
		core.ToolHub().SetPolicy(toolhub.NewDefaultPolicy(toolhub.DefaultPolicyOptions{AuthorizationHook: o.hook}))
	}
	if o.interaction != nil {
		core.ToolHub().SetInteractionHandler(o.interaction)
	}
	w.spans = &spanLog{}
	core.Trace().AddSink(w.spans)
	w.log = recordEvents(core.Events())
	return w
}

// registryFor is a private provider registry whose factory under the name "scripted"
// returns the given provider, for aec.WithProviderRegistry with Provider "scripted".
func registryFor(p llmhub.Provider) *llmhub.Registry {
	reg := llmhub.NewRegistry()
	reg.Register("scripted", func(llmhub.ProviderConfig) (llmhub.Provider, error) { return p, nil })
	return reg
}
