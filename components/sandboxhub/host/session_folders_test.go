package host

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/Phong95/aec-agent-go/components/sandboxhub"
)

// A-216: a host view scoped to session folders is re-rooted on them (WithRoots), so a
// link inside a folder that leads to another folder of the same agent is refused.
func TestScopedHostViewRefusesLinkOutOfTheFolders(t *testing.T) {
	_, sb, a1, _ := newSharedViews(t)
	ctx := t.Context()
	agentDir := filepath.Join(sb.Base(), "agents", "a1")
	for _, d := range []string{"api", "other"} {
		if err := os.MkdirAll(filepath.Join(agentDir, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(agentDir, "other", "secret.txt"), []byte("s"), 0o600); err != nil {
		t.Fatal(err)
	}
	makeLink(t, filepath.Join(agentDir, "api", "ln"), filepath.Join(agentDir, "other"))

	// Without the scope the link is inside the agent's folder, so it is allowed.
	if _, err := a1.ReadFile(ctx, "api/ln/secret.txt", sandboxhub.ReadOptions{}); err != nil {
		t.Fatalf("unscoped read: %v", err)
	}
	scoped, err := sandboxhub.Scope(a1, []string{"/workspace/agents/a1/api"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = scoped.ReadFile(ctx, "ln/secret.txt", sandboxhub.ReadOptions{})
	requireEscape(t, err, "/workspace/agents/a1/api/ln/secret.txt")
	_, err = scoped.WriteFile(ctx, "ln/new.txt", "x", sandboxhub.WriteOptions{})
	requireEscape(t, err, "/workspace/agents/a1/api/ln/new.txt")
	if _, err := os.Stat(filepath.Join(agentDir, "other", "new.txt")); err == nil {
		t.Error("the refused write created a file outside the folders")
	}
	if _, err := scoped.WriteFile(ctx, "ok.txt", "x", sandboxhub.WriteOptions{}); err != nil {
		t.Fatalf("write inside: %v", err)
	}
}
