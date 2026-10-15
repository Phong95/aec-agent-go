package host

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"testing/iotest"

	"github.com/Phong95/aec-agent-go/components/sandboxhub"
	"github.com/Phong95/aec-agent-go/kernel"
)

func intPtr(v int) *int { return &v }

// TestHostSandboxDisposeAndHealth checks the disposed state: every later operation
// fails with environment_unavailable and the disposed message, health follows, and the
// data stays unless removeData is set.
func TestHostSandboxDisposeAndHealth(t *testing.T) {
	root, _, sb, view := newOwnView(t)
	ctx := t.Context()
	if sb.Health(ctx) != sandboxhub.HealthReady {
		t.Errorf("health = %q, want ready", sb.Health(ctx))
	}
	if _, err := view.WriteFile(ctx, "keep.txt", "k", sandboxhub.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := sb.Dispose(ctx, false); err != nil {
		t.Fatal(err)
	}
	if sb.Health(ctx) != sandboxhub.HealthUnavailable {
		t.Errorf("health after dispose = %q", sb.Health(ctx))
	}
	if _, err := os.Stat(filepath.Join(root, "host-agent", "keep.txt")); err != nil {
		t.Errorf("Dispose without removeData must keep the files: %v", err)
	}
	want := `Sandbox "host:own:host-agent" was disposed`
	_, err := view.ReadFile(ctx, "keep.txt", sandboxhub.ReadOptions{})
	requireCode(t, err, kernel.SandboxCodeEnvironmentUnavailable)
	if err.Error() != want {
		t.Errorf("message = %q, want %q", err.Error(), want)
	}
	_, err = sb.ForAgent(ctx, ownAgent)
	requireCode(t, err, kernel.SandboxCodeEnvironmentUnavailable)
	_, err = view.Exec(ctx, sandboxhub.ExecRequest{Command: []string{"echo"}, TimeoutMs: 1, MaxOutputBytes: 1}, sandboxhub.ExecOptions{})
	requireCode(t, err, kernel.SandboxCodeEnvironmentUnavailable)

	// A sandbox whose folder was deleted from outside is unavailable too.
	_, _, sb2, _ := newOwnView(t)
	if err := os.RemoveAll(sb2.Base()); err != nil {
		t.Fatal(err)
	}
	if sb2.Health(ctx) != sandboxhub.HealthUnavailable {
		t.Errorf("health of a missing folder = %q", sb2.Health(ctx))
	}
}

// TestHostSandboxReadFileTooLarge uses a sparse file just above the 64 MiB cap, so
// nothing is read: the size check comes first.
func TestHostSandboxReadFileTooLarge(t *testing.T) {
	root, _, _, view := newOwnView(t)
	f, err := os.Create(filepath.Join(root, "host-agent", "huge.bin"))
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(maxFileBytes + 1); err != nil {
		_ = f.Close()
		t.Skipf("cannot create a sparse file of 64 MiB: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	_, err = view.ReadFile(t.Context(), "huge.bin", sandboxhub.ReadOptions{})
	requireCode(t, err, kernel.SandboxCodeTooLarge)
	if want := "\"huge.bin\" is 67108865 bytes \U00002014 too large to read"; err.Error() != want {
		t.Errorf("message = %q, want %q", err.Error(), want)
	}
	_, err = view.ReadFile(t.Context(), ".", sandboxhub.ReadOptions{})
	requireCode(t, err, kernel.SandboxCodeInvalidPath)
}

// TestReadAtMostBoundsTheRead: the whole-file read is bounded, so a file that grew past the
// limit after the size check is refused instead of read without bound. A reader of exactly
// the limit is kept, one byte more is over and nothing is returned.
func TestReadAtMostBoundsTheRead(t *testing.T) {
	raw, over, err := readAtMost(strings.NewReader("abcd"), 4)
	if err != nil || over || string(raw) != "abcd" {
		t.Errorf("at the limit: raw %q over %v err %v", raw, over, err)
	}
	raw, over, err = readAtMost(strings.NewReader("abcde"), 4)
	if err != nil || !over || raw != nil {
		t.Errorf("one byte over: raw %q over %v err %v", raw, over, err)
	}
	// An endless reader stops at limit+1 bytes.
	raw, over, err = readAtMost(endless{}, 1<<20)
	if err != nil || !over || raw != nil {
		t.Errorf("endless: raw len %d over %v err %v", len(raw), over, err)
	}
	if _, _, err := readAtMost(iotest.ErrReader(errors.New("disk")), 4); err == nil {
		t.Error("a read error must be returned")
	}
}

type endless struct{}

func (endless) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 'x'
	}
	return len(p), nil
}

// TestHostSandboxSearchSkipsTheRecordedSet pins the search filters: .git and
// node_modules, binary files, files above 8 MiB; a single file base is scanned
// alone, also inside a skipped folder, and keeps its logical path.
func TestHostSandboxSearchSkipsTheRecordedSet(t *testing.T) {
	root, _, _, view := newOwnView(t)
	ctx := t.Context()
	agent := filepath.Join(root, "host-agent")
	files := map[string]string{
		"src/a.txt":                    "needle a\n",
		"src/.git/config":              "needle git\n",
		"src/node_modules/pkg/i.js":    "needle module\n",
		"src/bin.dat":                  "needle\x00binary\n",
		"src/.hidden-file":             "needle hidden\n",
		"src/nested/deep/b.txt":        "x\nneedle b\n",
		"src/nested/deep/crlf.txt":     "needle crlf\r\nsecond\r\n",
		"src/nested/deep/bad-utf8.txt": "needle \xff\xfe\n",
	}
	for p, content := range files {
		full := filepath.Join(agent, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(full), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	big := make([]byte, searchMaxFileBytes+1)
	copy(big, "needle big\n")
	if err := os.WriteFile(filepath.Join(agent, "src", "big.txt"), big, 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := view.Search(ctx, sandboxhub.SearchQuery{Pattern: "needle", Path: "src"})
	if err != nil {
		t.Fatal(err)
	}
	var paths []string
	for _, m := range got.Matches {
		paths = append(paths, m.Path)
	}
	want := []string{
		"/workspace/src/.hidden-file", "/workspace/src/a.txt", "/workspace/src/nested/deep/b.txt",
		"/workspace/src/nested/deep/bad-utf8.txt", "/workspace/src/nested/deep/crlf.txt",
	}
	if !slices.Equal(paths, want) {
		t.Errorf("search paths = %v\nwant %v", paths, want)
	}
	for _, m := range got.Matches {
		if m.Path == "/workspace/src/nested/deep/bad-utf8.txt" && m.Text != "needle \U0000fffd\U0000fffd" {
			t.Errorf("invalid UTF-8 must decode lossily, got %q", m.Text)
		}
		if m.Path == "/workspace/src/nested/deep/crlf.txt" && m.Text != "needle crlf\r" {
			t.Errorf("a CR stays in the line text (only LF splits), got %q", m.Text)
		}
	}
	one, err := view.Search(ctx, sandboxhub.SearchQuery{Pattern: "needle", Path: "src/.git/config"})
	if err != nil || len(one.Matches) != 1 || one.Matches[0].Path != "/workspace/src/.git/config" {
		t.Errorf("single-file search = %+v (%v)", one, err)
	}
	if bin, err := view.Search(ctx, sandboxhub.SearchQuery{Pattern: "needle", Path: "src/bin.dat"}); err != nil || len(bin.Matches) != 0 {
		t.Errorf("a binary file must be skipped: %+v (%v)", bin, err)
	}
	// A pattern that RE2 cannot compile, or that means something else in RE2, is invalid_argument.
	for _, pattern := range []string{"(?i)needle", "(?=a)", `\Qneedle\E`} {
		_, err := view.Search(ctx, sandboxhub.SearchQuery{Pattern: pattern, Literal: boolPtr(false)})
		requireCode(t, err, kernel.SandboxCodeInvalidArgument)
	}
}

// TestHostSandboxListRules pins the list rules: hidden entries do
// not count towards the cap, sizes come from the file, directories have size 0 and
// entries are sorted with the JS locale order.
func TestHostSandboxListRules(t *testing.T) {
	_, _, _, view := newOwnView(t)
	ctx := t.Context()
	for _, p := range []string{"l/b.txt", "l/.hidden1", "l/.hidden2", "l/a.txt", "l/c.txt", "l/sub/x.txt"} {
		if _, err := view.WriteFile(ctx, p, "12345", sandboxhub.WriteOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	capped, err := view.List(ctx, "l", sandboxhub.ListOptions{MaxEntries: intPtr(2)})
	if err != nil {
		t.Fatal(err)
	}
	if len(capped.Entries) != 2 || !capped.Truncated || capped.Entries[0].Name != "a.txt" || capped.Entries[1].Name != "b.txt" {
		t.Errorf("capped listing = %+v", capped)
	}
	if capped.Entries[0].Size != 5 || capped.Entries[0].Path != "/workspace/l/a.txt" || capped.Entries[0].Type != sandboxhub.EntryFile {
		t.Errorf("entry details = %+v", capped.Entries[0])
	}
	all, err := view.List(ctx, "l", sandboxhub.ListOptions{IncludeHidden: boolPtr(true), Recursive: boolPtr(true)})
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range all.Entries {
		names = append(names, e.Name)
	}
	if want := []string{".hidden1", ".hidden2", "a.txt", "b.txt", "c.txt", "sub", "x.txt"}; !slices.Equal(names, want) {
		t.Errorf("recursive listing = %v, want %v", names, want)
	}
	if sub := all.Entries[5]; sub.Type != sandboxhub.EntryDirectory || sub.Size != 0 {
		t.Errorf("a directory entry = %+v", sub)
	}
}

// TestHostSandboxWriteRules pins the write rules that need a
// real file system: an existing directory, a file used as a parent directory, and a
// missing parent with createDirs false.
func TestHostSandboxWriteRules(t *testing.T) {
	_, _, _, view := newOwnView(t)
	ctx := t.Context()
	if _, err := view.WriteFile(ctx, "a/b.txt", "x", sandboxhub.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := view.WriteFile(ctx, "a", "x", sandboxhub.WriteOptions{}); err == nil {
		t.Error("writing onto a directory must fail")
	} else {
		requireCode(t, err, kernel.SandboxCodeInvalidPath)
		if err.Error() != `"a" is a directory` {
			t.Errorf("message = %q", err.Error())
		}
	}
	// A file where the parent directory should be: the parent is not a directory.
	if _, err := view.WriteFile(ctx, "a/b.txt/c.txt", "x", sandboxhub.WriteOptions{CreateDirs: boolPtr(false)}); err == nil {
		t.Error("writing below a file must fail")
	} else {
		var se *kernel.SandboxError
		if !errors.As(err, &se) || (se.Code != kernel.SandboxCodeInvalidPath && se.Code != kernel.SandboxCodeNotFound) {
			t.Errorf("error = %v, want invalid_path or not_found", err)
		}
	}
	again, err := view.WriteFile(ctx, "a/b.txt", "y", sandboxhub.WriteOptions{})
	if err != nil || again.Created {
		t.Errorf("an overwrite = %+v (%v), want created false", again, err)
	}
}
