package host

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/Phong95/aec-agent-go/components/sandboxhub"
	"github.com/Phong95/aec-agent-go/kernel"
)

// WR-02 regressions: AssertSafeAgentID allows ids that name the SAME real folder on
// Windows ("a" and "a.") or on a case-folding file system ("Alice" and "alice"), and
// a reserved device name ("con"). The provider derives the own folder
// <root>/<agentId> from the id, so these ids must be refused or two agents would share
// files and RemoveData of one would delete the other's data.

func TestCheckAgentFolderAppliesTheWindowsNamePolicyOnlyOnWindows(t *testing.T) {
	parent := t.TempDir()
	for _, id := range []string{"a.", "con", "nul.x", "COM1", "LPT²"} {
		if err := checkAgentFolder(parent, id, "host:own:"+id, true, false); err == nil {
			t.Errorf("%q must be refused on Windows", id)
		} else {
			requireCode(t, err, kernel.SandboxCodeInvalidPath)
		}
		if err := checkAgentFolder(parent, id, "host:own:"+id, false, false); err != nil {
			t.Errorf("%q is an ordinary name off Windows: %v", id, err)
		}
	}
	for _, id := range []string{"a", "agent-1", "con-sole", "x.y"} {
		if err := checkAgentFolder(parent, id, "host:own:"+id, true, true); err != nil {
			t.Errorf("%q must be accepted: %v", id, err)
		}
	}
}

func TestCheckAgentFolderRefusesACaseAliasOfAnExistingFolder(t *testing.T) {
	parent := t.TempDir()
	if err := os.Mkdir(filepath.Join(parent, "Alice"), 0o755); err != nil {
		t.Fatal(err)
	}
	// Whether "alice" is the same folder as "Alice" is up to the file system under the
	// test: Windows and macOS usually fold case, Linux does not. The check follows the
	// file system, not the operating system name (IN-203): it refuses the alias where it
	// is one, and accepts the two spellings on a case-sensitive volume.
	_, statErr := os.Stat(filepath.Join(parent, "alice"))
	folds := statErr == nil
	err := checkAgentFolder(parent, "alice", "host:own:alice", false, true)
	if folds {
		if err == nil {
			t.Fatal("an id that differs only in case from an existing folder must be refused")
		}
		requireCode(t, err, kernel.SandboxCodePolicyDenied)
	} else if err != nil {
		t.Errorf("on a case-sensitive volume \"alice\" and \"Alice\" are two folders: %v", err)
	}
	// The same id is fine, a case-sensitive host accepts the alias, and a parent that
	// does not exist yet has nothing to collide with.
	if err := checkAgentFolder(parent, "Alice", "host:own:Alice", false, true); err != nil {
		t.Errorf("the identical id is not an alias: %v", err)
	}
	if err := checkAgentFolder(parent, "alice", "host:own:alice", false, false); err != nil {
		t.Errorf("a case-sensitive host has no alias: %v", err)
	}
	if err := checkAgentFolder(filepath.Join(parent, "missing"), "alice", "host:own:alice", false, true); err != nil {
		t.Errorf("a missing parent has no alias: %v", err)
	}
}

// TestCheckAgentFolderFailsClosedWhenTheParentCannotBeListed is the IN-203 regression:
// a ReadDir error other than "does not exist" used to return nil, so the alias check was
// skipped without a trace. A parent path that cannot be opened at all (an embedded NUL is
// an invalid argument on every operating system) must be refused, not accepted.
func TestCheckAgentFolderFailsClosedWhenTheParentCannotBeListed(t *testing.T) {
	parent := filepath.Join(t.TempDir(), "a\x00b")
	if err := checkAgentFolder(parent, "alice", "host:own:alice", false, true); err == nil {
		t.Fatal("an unreadable parent must fail closed, not skip the alias check")
	} else {
		requireCode(t, err, kernel.SandboxCodeEnvironmentUnavailable)
	}
	// Without a case-folding host the parent is never read, so nothing changes there.
	if err := checkAgentFolder(parent, "alice", "host:own:alice", false, false); err != nil {
		t.Errorf("a case-sensitive host does not read the parent: %v", err)
	}
}

func TestOwnProviderRefusesAgentIDsThatAliasAnotherFolder(t *testing.T) {
	if !onWindows && !caseInsensitiveHost {
		t.Skip("the folder aliases only exist on Windows and macOS")
	}
	root := t.TempDir()
	p, err := New(Options{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	create := func(id string) error {
		_, err := p.Create(ctx, sandboxhub.Spec{Kind: sandboxhub.KindHost},
			sandboxhub.CreateContext{ID: "host:own:" + id, Tenancy: sandboxhub.TenancyOwn, AgentID: id})
		return err
	}
	if err := create("Alice"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "Alice", "keep.txt"), []byte("alice's data"), 0o600); err != nil {
		t.Fatal(err)
	}
	// "alice" would be the same folder as "Alice": refused, and RemoveData for it must
	// not delete Alice's data either.
	requireCode(t, create("alice"), kernel.SandboxCodePolicyDenied)
	cc := sandboxhub.CreateContext{ID: "host:own:alice", Tenancy: sandboxhub.TenancyOwn, AgentID: "alice"}
	requireCode(t, p.RemoveData(ctx, sandboxhub.Spec{Kind: sandboxhub.KindHost}, cc, ""), kernel.SandboxCodePolicyDenied)
	if data, err := os.ReadFile(filepath.Join(root, "Alice", "keep.txt")); err != nil || string(data) != "alice's data" {
		t.Errorf("Alice's data was lost: %q (%v)", data, err)
	}
	if onWindows {
		for _, id := range []string{"a.", "con", "nul.x"} {
			if err := create(id); err == nil {
				t.Errorf("%q must be refused on Windows", id)
			} else {
				requireCode(t, err, kernel.SandboxCodeInvalidPath)
			}
		}
		if _, err := os.Stat(filepath.Join(root, "con")); err == nil {
			t.Error("a folder for a device name was created")
		}
	}
}

func TestSharedAgentFoldersRefuseACaseAlias(t *testing.T) {
	if !caseInsensitiveHost {
		t.Skip("the folder aliases only exist on Windows and macOS")
	}
	_, sb, _, _ := newSharedViews(t)
	ctx := t.Context()
	if _, err := sb.ForAgent(ctx, "Bob"); err != nil {
		t.Fatal(err)
	}
	_, err := sb.ForAgent(ctx, "bob")
	requireCode(t, err, kernel.SandboxCodePolicyDenied)
	requireCode(t, sb.RemoveAgentData(ctx, "bob"), kernel.SandboxCodePolicyDenied)
	if _, err := os.Stat(filepath.Join(sb.Base(), "agents", "Bob")); err != nil {
		t.Errorf("Bob's folder was removed: %v", err)
	}
}

// TestSharedProviderRemoveDataRefusesACaseAlias is the WR-201 regression: the shared
// Provider.RemoveData is the fourth path that turns an agent id into a folder, and it
// must refuse a case alias of an existing agent folder like ForAgent and
// RemoveAgentData do. Without the check, RemoveData("alice") deleted Alice's files.
func TestSharedProviderRemoveDataRefusesACaseAlias(t *testing.T) {
	if !caseInsensitiveHost {
		t.Skip("the folder aliases only exist on Windows and macOS")
	}
	root := t.TempDir()
	p, err := New(Options{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	spec := sandboxhub.Spec{Kind: sandboxhub.KindHost}
	cc := sandboxhub.CreateContext{ID: "host:shared", Tenancy: sandboxhub.TenancyShared}
	alice := filepath.Join(root, ".shared", "agents", "Alice")
	if err := os.MkdirAll(alice, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(alice, "f.txt"), []byte("alice's data"), 0o600); err != nil {
		t.Fatal(err)
	}
	requireCode(t, p.RemoveData(ctx, spec, cc, "alice"), kernel.SandboxCodePolicyDenied)
	if data, err := os.ReadFile(filepath.Join(alice, "f.txt")); err != nil || string(data) != "alice's data" {
		t.Errorf("Alice's data was lost: %q (%v)", data, err)
	}
	// The identical id still removes its own folder, and a missing one is not an error.
	if err := p.RemoveData(ctx, spec, cc, "Alice"); err != nil {
		t.Errorf("RemoveData of the exact id: %v", err)
	}
	if _, err := os.Stat(alice); err == nil {
		t.Error("the exact id did not remove its folder")
	}
	if err := p.RemoveData(ctx, spec, cc, "nobody"); err != nil {
		t.Errorf("RemoveData of a missing folder: %v", err)
	}
}
