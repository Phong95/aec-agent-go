package sandboxhub_test

import (
	"slices"
	"testing"

	"github.com/Phong95/aec-agent-go/aecmock"
	"github.com/Phong95/aec-agent-go/components/sandboxhub"
	"github.com/Phong95/aec-agent-go/kernel"
)

// The hub cases that need a working view run on aecmock.InMemoryProvider, the
// in-memory provider with real files. The fakeProvider of
// hub_test.go has no files, so these four cases could not run there.

// inMemoryHub is a hub with the in-memory provider registered and the shared
// sandbox configured, like the hubWith / data-retention setups.
func inMemoryHub(t *testing.T) (*sandboxhub.SandboxHub, *aecmock.InMemoryProvider) {
	t.Helper()
	hub := sandboxhub.NewSandboxHub()
	provider := aecmock.NewInMemoryProvider()
	if err := hub.Register(provider); err != nil {
		t.Fatal(err)
	}
	if err := hub.ConfigureShared(sandboxhub.Spec{Kind: sandboxhub.KindDocker}); err != nil {
		t.Fatal(err)
	}
	return hub, provider
}

// TestSandboxHubKeepsFileOpsOutOfEachOthersFoldersAndOutOfWorkspaceShared checks
// that file ops stay out of each other's folders and out of /workspace/shared
// (own folder only).
func TestSandboxHubKeepsFileOpsOutOfEachOthersFoldersAndOutOfWorkspaceShared(t *testing.T) {
	ctx := t.Context()
	hub, _ := inMemoryHub(t)
	t.Cleanup(func() { _ = hub.Dispose(ctx) })

	a, err := hub.Resolve(ctx, "agent-a", sandboxhub.Selection{})
	if err != nil {
		t.Fatal(err)
	}
	b, err := hub.Resolve(ctx, "agent-b", sandboxhub.Selection{})
	if err != nil {
		t.Fatal(err)
	}
	// /workspace/shared was dropped from the default layout - each agent only gets
	// its own folder.
	_, err = a.WriteFile(ctx, "/workspace/shared/handoff.md", "from A", sandboxhub.WriteOptions{})
	expectCode(t, err, kernel.SandboxCodePathEscape)
	_, err = b.ReadFile(ctx, "/workspace/shared/handoff.md", sandboxhub.ReadOptions{})
	expectCode(t, err, kernel.SandboxCodePathEscape)

	if _, err := a.WriteFile(ctx, "private.txt", "A only", sandboxhub.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	_, err = b.ReadFile(ctx, "/workspace/agents/agent-a/private.txt", sandboxhub.ReadOptions{})
	expectCode(t, err, kernel.SandboxCodePathEscape)
}

// TestSandboxHubDeletesTheSharedFolderWhenAnAgentIsRemoved checks "deletes the
// shared folder when an agent is removed" (SandboxHub data retention): with no
// sandbox running the hub asks the provider to remove the folder.
func TestSandboxHubDeletesTheSharedFolderWhenAnAgentIsRemoved(t *testing.T) {
	ctx := t.Context()
	hub, provider := inMemoryHub(t)

	if err := hub.ReleaseAgentData(ctx, "a1"); err != nil {
		t.Fatal(err)
	}
	want := []aecmock.RemovedData{{ID: "docker:shared", AgentID: "a1"}}
	if got := provider.RemovedData(); !slices.Equal(got, want) {
		t.Errorf("RemovedData = %v, want %v", got, want)
	}
}

// TestSandboxHubRemovesTheFolderFromARunningSharedSandboxDirectly checks "removes
// the folder from a running shared sandbox directly": the sandbox deletes the
// folder itself and the provider is not asked.
func TestSandboxHubRemovesTheFolderFromARunningSharedSandboxDirectly(t *testing.T) {
	ctx := t.Context()
	hub, provider := inMemoryHub(t)
	t.Cleanup(func() { _ = hub.Dispose(ctx) })

	view, err := hub.Resolve(ctx, "a1", sandboxhub.Selection{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := view.WriteFile(ctx, "x.txt", "x", sandboxhub.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := hub.ReleaseAgentData(ctx, "a1"); err != nil {
		t.Fatal(err)
	}
	if got := provider.RemovedData(); len(got) != 0 {
		t.Errorf("RemovedData = %v, want none: a running sandbox removes the folder itself", got)
	}
	again, err := hub.Resolve(ctx, "a1", sandboxhub.Selection{})
	if err != nil {
		t.Fatal(err)
	}
	st, err := again.Stat(ctx, "x.txt")
	if err != nil {
		t.Fatal(err)
	}
	if st != nil {
		t.Errorf("x.txt still exists after the agent data was released: %+v", st)
	}
}

// TestSandboxHubKeepsDataWhenTheHubIsDisposed checks "keeps data when the hub is
// disposed (server shutdown)": Dispose stops the sandbox but never removes data.
func TestSandboxHubKeepsDataWhenTheHubIsDisposed(t *testing.T) {
	ctx := t.Context()
	hub, provider := inMemoryHub(t)

	view, err := hub.Resolve(ctx, "a1", sandboxhub.Selection{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := view.WriteFile(ctx, "x.txt", "keep me", sandboxhub.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := hub.Dispose(ctx); err != nil {
		t.Fatal(err)
	}
	if got := provider.RemovedData(); len(got) != 0 {
		t.Errorf("RemovedData = %v, want none", got)
	}
}
