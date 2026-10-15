package host

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/Phong95/aec-agent-go/components/sandboxhub"
	"github.com/Phong95/aec-agent-go/kernel"
)

// Escape tests: a symlink or junction that leaves the agent's folder is refused
// with path_escape and the symlink message, one that stays inside is followed, and the
// os.Root layer refuses what the real-path check cannot see (threats T-07-14,
// T-07-15, T-07-19). Every test runs in symlink mode on Linux and in symlink or
// junction mode on Windows (makeLink logs which).

const escapeMessageSuffix = `" resolves through a symlink to a location outside the workspace`

// newSharedViews creates a shared host sandbox under a temporary root with the agent
// views a1 and a2 and returns the root, the sandbox and both views.
func newSharedViews(t *testing.T) (string, *Sandbox, sandboxhub.View, sandboxhub.View) {
	t.Helper()
	root := t.TempDir()
	p, err := New(Options{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	sb, err := p.Create(t.Context(), sandboxhub.Spec{Kind: sandboxhub.KindHost},
		sandboxhub.CreateContext{ID: "host:shared", Tenancy: sandboxhub.TenancyShared})
	if err != nil {
		t.Fatal(err)
	}
	a1, err := sb.ForAgent(t.Context(), "a1")
	if err != nil {
		t.Fatal(err)
	}
	a2, err := sb.ForAgent(t.Context(), "a2")
	if err != nil {
		t.Fatal(err)
	}
	return root, sb.(*Sandbox), a1, a2
}

// requireEscape requires the path_escape error with the symlink message for input.
func requireEscape(t *testing.T, err error, input string) {
	t.Helper()
	requireCode(t, err, kernel.SandboxCodePathEscape)
	if want := `"` + input + escapeMessageSuffix; err.Error() != want {
		t.Errorf("message = %q, want %q", err.Error(), want)
	}
}

func TestEscapeLinkToAnotherAgentsFolderIsRefused(t *testing.T) {
	_, sb, a1, a2 := newSharedViews(t)
	ctx := t.Context()
	if _, err := a2.WriteFile(ctx, "secret.txt", "agent two's secret", sandboxhub.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	makeLink(t, filepath.Join(sb.Base(), "agents", "a1", "ln"), filepath.Join(sb.Base(), "agents", "a2"))

	_, err := a1.ReadFile(ctx, "ln/secret.txt", sandboxhub.ReadOptions{})
	requireEscape(t, err, "ln/secret.txt")
	_, err = a1.WriteFile(ctx, "ln/new.txt", "x", sandboxhub.WriteOptions{})
	requireEscape(t, err, "ln/new.txt")
	_, err = a1.List(ctx, "ln", sandboxhub.ListOptions{})
	requireEscape(t, err, "ln")
	_, err = a1.Stat(ctx, "ln")
	requireEscape(t, err, "ln")
	_, err = a1.Search(ctx, sandboxhub.SearchQuery{Pattern: "secret", Path: "ln"})
	requireEscape(t, err, "ln")
	if _, err := os.Stat(filepath.Join(sb.Base(), "agents", "a2", "new.txt")); err == nil {
		t.Error("the refused write created a file in the other agent's folder")
	}
}

func TestEscapeLinkToADirectoryOutsideTheBaseIsRefused(t *testing.T) {
	_, sb, a1, _ := newSharedViews(t)
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("top secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	makeLink(t, filepath.Join(sb.Base(), "agents", "a1", "out"), outside)
	ctx := t.Context()

	read, err := a1.ReadFile(ctx, "out/secret.txt", sandboxhub.ReadOptions{})
	requireEscape(t, err, "out/secret.txt")
	if read.Content != "" {
		t.Errorf("the secret leaked: %q", read.Content)
	}
	_, err = a1.WriteFile(ctx, "out/new.txt", "x", sandboxhub.WriteOptions{})
	requireEscape(t, err, "out/new.txt")
	_, err = a1.WriteFile(ctx, "out/deeper/new.txt", "x", sandboxhub.WriteOptions{})
	requireEscape(t, err, "out/deeper/new.txt")
	_, err = a1.List(ctx, "out", sandboxhub.ListOptions{})
	requireEscape(t, err, "out")
	if entries, _ := os.ReadDir(outside); len(entries) != 1 {
		t.Errorf("the outside folder was changed: %v", entries)
	}
}

// TestEscapeLinkToAFileOutsideIsRefused covers a link whose target is a FILE outside
// (symlink mode only: a junction can only point at a folder).
func TestEscapeLinkToAFileOutsideIsRefused(t *testing.T) {
	if !canSymlink(t) {
		t.Skip("a file link needs os.Symlink, which is not allowed here")
	}
	_, sb, a1, _ := newSharedViews(t)
	outside := filepath.Join(t.TempDir(), "secret.txt")
	if err := os.WriteFile(outside, []byte("top secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(sb.Base(), "agents", "a1", "file-link")); err != nil {
		t.Fatal(err)
	}
	_, err := a1.ReadFile(t.Context(), "file-link", sandboxhub.ReadOptions{})
	requireEscape(t, err, "file-link")
	_, err = a1.WriteFile(t.Context(), "file-link", "overwritten", sandboxhub.WriteOptions{})
	requireEscape(t, err, "file-link")
	if data, _ := os.ReadFile(outside); string(data) != "top secret" {
		t.Errorf("the outside file was overwritten: %q", data)
	}
}

func TestLinkThatStaysInsideIsFollowedForReadWriteListAndStat(t *testing.T) {
	_, sb, a1, _ := newSharedViews(t)
	ctx := t.Context()
	if _, err := a1.WriteFile(ctx, "data/data.txt", "linked data", sandboxhub.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	agent := filepath.Join(sb.Base(), "agents", "a1")
	makeLink(t, filepath.Join(agent, "ln"), filepath.Join(agent, "data"))

	read, err := a1.ReadFile(ctx, "ln/data.txt", sandboxhub.ReadOptions{})
	if err != nil || read.Content != "linked data" {
		t.Fatalf("read through the link = %+v (%v)", read, err)
	}
	if want := "/workspace/agents/a1/ln/data.txt"; read.Path != want {
		t.Errorf("path = %q, want the logical path %q", read.Path, want)
	}
	if _, err := a1.WriteFile(ctx, "ln/via.txt", "through the link", sandboxhub.WriteOptions{}); err != nil {
		t.Fatalf("write through the link: %v", err)
	}
	if data, err := os.ReadFile(filepath.Join(agent, "data", "via.txt")); err != nil || string(data) != "through the link" {
		t.Errorf("the file must land in the link target: %q (%v)", data, err)
	}
	if st, err := a1.Stat(ctx, "ln"); err != nil || st == nil || st.Type != sandboxhub.EntryDirectory {
		t.Errorf("stat follows the link: %+v (%v)", st, err)
	}
	listed, err := a1.List(ctx, "ln", sandboxhub.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range listed.Entries {
		names = append(names, e.Name)
	}
	if !slices.Equal(names, []string{"data.txt", "via.txt"}) {
		t.Errorf("listing through the link = %v", names)
	}
	// The link itself is listed as a symlink (a Windows junction too, like Node) and
	// is never followed by a recursive listing.
	top, err := a1.List(ctx, ".", sandboxhub.ListOptions{Recursive: boolPtr(true)})
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range top.Entries {
		if e.Name == "ln" && e.Type != sandboxhub.EntrySymlink {
			t.Errorf("the link is listed as %q, want symlink", e.Type)
		}
		if e.Path == "/workspace/agents/a1/ln/data.txt" {
			t.Error("a recursive listing followed the link")
		}
	}
	// Search never follows a link either: the file is found once, under data.
	found, err := a1.Search(ctx, sandboxhub.SearchQuery{Pattern: "linked data"})
	if err != nil {
		t.Fatal(err)
	}
	if len(found.Matches) != 1 || found.Matches[0].Path != "/workspace/agents/a1/data/data.txt" {
		t.Errorf("search matches = %+v", found.Matches)
	}
}

func boolPtr(b bool) *bool { return &b }

// TestOsRootLayerRefusesAnEscapingLink drives the os.Root layer directly: a
// root-relative path that crosses a link to the outside gives the escape error, and
// mapFSError turns it into path_escape (the TOCTOU backstop, T-07-15).
func TestOsRootLayerRefusesAnEscapingLink(t *testing.T) {
	realRoot, outside := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("top secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	makeLink(t, filepath.Join(realRoot, "ln"), outside)

	var got []byte
	err := inRoot(realRoot, filepath.Join(realRoot, "ln", "secret.txt"), func(root *os.Root, rel string) error {
		var err error
		got, err = root.ReadFile(rel)
		return err
	})
	if err == nil {
		t.Fatalf("os.Root followed the link and read %q", got)
	}
	if !isPathEscape(err) {
		t.Fatalf("the os.Root escape error was not recognised: %v", err)
	}
	requireEscape(t, mapFSError(err, "ln/secret.txt", "host:test"), "ln/secret.txt")
}

// TestOsRootLayerCatchesALinkSwappedInAfterTheCheck is the race the second layer is
// for: the real-path check passes while the folder is a plain folder, a link to the
// outside is swapped in, and the operation (which uses the earlier result) is
// refused by os.Root and not performed.
func TestOsRootLayerCatchesALinkSwappedInAfterTheCheck(t *testing.T) {
	_, sb, a1, _ := newSharedViews(t)
	ctx := t.Context()
	if _, err := a1.WriteFile(ctx, "d/f.txt", "inside", sandboxhub.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "f.txt"), []byte("outside secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	v := a1.(*view)
	res, err := v.resolve("d/f.txt")
	if err != nil {
		t.Fatalf("the check must pass while d is a plain folder: %v", err)
	}

	// The swap: d becomes a link to the outside folder.
	dir := filepath.Join(sb.Base(), "agents", "a1", "d")
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	makeLink(t, dir, outside)

	var got []byte
	err = inRoot(res.realRoot, res.realPath, func(root *os.Root, rel string) error {
		var err error
		got, err = root.ReadFile(rel)
		return err
	})
	if err == nil {
		t.Fatalf("a link swapped in after the check was followed and read %q", got)
	}
	requireEscape(t, mapFSError(err, "d/f.txt", sb.ID()), "d/f.txt")
	// A write that raced the same way must not touch the outside folder.
	err = inRoot(res.realRoot, res.realPath, func(root *os.Root, rel string) error {
		return root.WriteFile(rel, []byte("overwritten"), 0o600)
	})
	if err == nil || !isPathEscape(err) {
		t.Fatalf("the swapped-in link must refuse a write with the escape error, got %v", err)
	}
	if data, _ := os.ReadFile(filepath.Join(outside, "f.txt")); string(data) != "outside secret" {
		t.Errorf("the outside file was overwritten: %q", data)
	}
}

// TestIsPathEscapeRecognisesTheOsRootError provokes the os.Root escape error with
// paths that leave the root, on every operating system and Go version of the CI
// matrix. If a Go release changes the message, this fails in CI instead of the guard
// failing open (threat T-07-19).
func TestIsPathEscapeRecognisesTheOsRootError(t *testing.T) {
	dir := t.TempDir()
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	if err := os.WriteFile(filepath.Join(dir, "inside.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	escapes := map[string]func() error{
		"dot-dot read":  func() error { _, err := root.ReadFile(filepath.Join("..", "x")); return err },
		"dot-dot stat":  func() error { _, err := root.Stat(filepath.Join("a", "..", "..", "x")); return err },
		"dot-dot write": func() error { return root.WriteFile(filepath.Join("..", "y"), []byte("x"), 0o600) },
		"absolute":      func() error { _, err := root.Stat(filepath.Join(dir, "inside.txt")); return err },
		"open":          func() error { _, err := root.Open(filepath.Join("..", "x")); return err },
	}
	for name, fn := range escapes {
		err := fn()
		if err == nil {
			t.Errorf("%s: the root accepted a path outside it", name)
			continue
		}
		if !isPathEscape(err) {
			t.Errorf("%s: isPathEscape does not recognise %q", name, err)
		}
	}
	// Errors that are not escapes are not recognised as one.
	if _, err := root.ReadFile("missing.txt"); err == nil || isPathEscape(err) {
		t.Errorf("a missing file must not look like an escape: %v", err)
	}
	if isPathEscape(nil) {
		t.Error("nil is not an escape")
	}
}

// TestMapFSErrorMapsTheErrnoTable pins the fsError text and code of each class this
// OS can produce.
func TestMapFSErrorMapsTheErrnoTable(t *testing.T) {
	dir := t.TempDir()
	_, notExist := os.Stat(filepath.Join(dir, "nope"))
	err := mapFSError(notExist, "nope", "host:test")
	requireCode(t, err, kernel.SandboxCodeNotFound)
	if err.Error() != `"nope" does not exist` {
		t.Errorf("not found message = %q", err.Error())
	}
	other := mapFSError(os.ErrInvalid, "x", "host:test")
	requireCode(t, other, kernel.SandboxCodeEnvironmentUnavailable)
	if other.Error() != `"x": invalid argument` {
		t.Errorf("fallback message = %q", other.Error())
	}
	perm := mapFSError(os.ErrPermission, "x", "host:test")
	requireCode(t, perm, kernel.SandboxCodePolicyDenied)
	if perm.Error() != `"x" permission denied` {
		t.Errorf("permission message = %q", perm.Error())
	}
	// A SandboxError passes through unchanged.
	se := kernel.NewSandboxError("already mapped", kernel.SandboxCodeConflict, "host:other")
	var same *kernel.SandboxError
	if !errors.As(mapFSError(se, "x", "host:test"), &same) || same != se {
		t.Error("a SandboxError must be returned unchanged")
	}
}

// TestIsPathEscapeIgnoresTheCallerPath is the WR-01 regression: a *fs.PathError renders
// as "op path: reason", so a file literally named like the os.Root escape message must
// not be taken for an escape. Only the underlying error counts.
func TestIsPathEscapeIgnoresTheCallerPath(t *testing.T) {
	const name = "sub/" + pathEscapesMessage
	dir := t.TempDir()
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()

	// A miss under a path that carries the phrase is a plain not-exist, not an escape.
	_, missErr := root.Lstat(name)
	if missErr == nil {
		t.Fatal("the probe path must not exist")
	}
	if isPathEscape(missErr) {
		t.Errorf("a miss on a path named like the escape message was taken for an escape: %v", missErr)
	}
	// A hand-made error that only carries the phrase in its path is not an escape; one
	// whose underlying error is the phrase is.
	if isPathEscape(&fs.PathError{Op: "statat", Path: name, Err: fs.ErrNotExist}) {
		t.Error("a PathError whose path holds the phrase is not an escape")
	}
	if isPathEscape(errors.New("open " + name + ": nope")) {
		t.Error("a plain error that mentions the phrase is not an escape")
	}
	if !isPathEscape(&fs.PathError{Op: "statat", Path: name, Err: errors.New(pathEscapesMessage)}) {
		t.Error("a PathError whose underlying error is the phrase is an escape")
	}
	if !isPathEscape(fmt.Errorf("wrapped: %w", &fs.PathError{Op: "x", Path: "p", Err: errors.New(pathEscapesMessage)})) {
		t.Error("a wrapped escape error is still an escape")
	}
}

// TestFileNamedLikeTheEscapeMessageCanBeWrittenAndStatted drives the same through the
// view: such a file is created by WriteFile and Stat of an absent one is nil, nil.
func TestFileNamedLikeTheEscapeMessageCanBeWrittenAndStatted(t *testing.T) {
	_, _, a1, _ := newSharedViews(t)
	ctx := t.Context()
	const name = "sub/" + pathEscapesMessage
	if st, err := a1.Stat(ctx, name); err != nil || st != nil {
		t.Fatalf("stat of an absent file named like the escape message = %+v, %v; want nil, nil", st, err)
	}
	w, err := a1.WriteFile(ctx, name, "hello", sandboxhub.WriteOptions{})
	if err != nil {
		t.Fatalf("write of a file named like the escape message: %v", err)
	}
	if !w.Created {
		t.Error("the file must be reported as created")
	}
	read, err := a1.ReadFile(ctx, name, sandboxhub.ReadOptions{})
	if err != nil || read.Content != "hello" {
		t.Fatalf("read back = %+v, %v", read, err)
	}
	if st, err := a1.Stat(ctx, name); err != nil || st == nil || st.Type != sandboxhub.EntryFile {
		t.Errorf("stat of the written file = %+v, %v", st, err)
	}
}
