package aecmock_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/Phong95/aec-agent-go/aecmock"
	"github.com/Phong95/aec-agent-go/components/sandboxhub"
	"github.com/Phong95/aec-agent-go/internal/wiretest"
	"github.com/Phong95/aec-agent-go/kernel"
)

// requireJSON encodes got with the wire encoder and requires the bytes to equal
// the fixture bytes of the same key.
func requireJSON(t *testing.T, fixture map[string]json.RawMessage, key string, got any) {
	t.Helper()
	want, ok := fixture[key]
	if !ok {
		t.Fatalf("fixture has no key %q", key)
	}
	enc, err := kernel.EncodeJSON(got)
	if err != nil {
		t.Fatalf("encode %s: %v", key, err)
	}
	if string(enc) != string(want) {
		t.Errorf("%s differs from the real TS value\n got: %s\nwant: %s", key, enc, want)
	}
}

// TestInMemoryReplaysSandboxWire replays the scenario of sandbox-wire.json through
// the in-memory sandbox with the same injected clock the fixture was recorded with
// and requires every value to be the recorded bytes, timestamps included: modifiedAt
// 1700000000004 (two ticks at reset, one for the agent folder, one for notes, one
// for the file) and durationMs 1 (two ticks per exec).
func TestInMemoryReplaysSandboxWire(t *testing.T) {
	var fixture map[string]json.RawMessage
	if err := json.Unmarshal(wiretest.Load(t, "sandbox-wire.json"), &fixture); err != nil {
		t.Fatalf("decode sandbox-wire.json: %v", err)
	}

	ctx := context.Background()
	provider := aecmock.NewInMemoryProvider(aecmock.WithClock(wiretest.Clock(1700000000000, 1)))
	env, err := provider.Create(ctx, sandboxhub.Spec{Kind: sandboxhub.KindDocker}, sandboxhub.CreateContext{ID: "docker:shared", Tenancy: sandboxhub.TenancyShared})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	view, err := env.ForAgent(ctx, "agent-1")
	if err != nil {
		t.Fatalf("ForAgent: %v", err)
	}

	write, err := view.WriteFile(ctx, "notes/hello.md", "hello <&> world\nsecond line\n", sandboxhub.WriteOptions{})
	if err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	read, err := view.ReadFile(ctx, "notes/hello.md", sandboxhub.ReadOptions{})
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	stat, err := view.Stat(ctx, "notes/hello.md")
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	list, err := view.List(ctx, "notes", sandboxhub.ListOptions{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	search, err := view.Search(ctx, sandboxhub.SearchQuery{Pattern: "world"})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	exec, err := view.Exec(ctx, sandboxhub.ExecRequest{Command: []string{"echo", "<&>"}, TimeoutMs: 5000, MaxOutputBytes: 1000}, sandboxhub.ExecOptions{})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}

	requireJSON(t, fixture, "traits", view.Traits())
	requireJSON(t, fixture, "limits", view.Limits())
	requireJSON(t, fixture, "capabilities", view.Capabilities())
	requireJSON(t, fixture, "writeResult", write)
	requireJSON(t, fixture, "fileContent", read)
	requireJSON(t, fixture, "fileStat", stat)
	requireJSON(t, fixture, "listResult", list)
	requireJSON(t, fixture, "searchResult", search)
	requireJSON(t, fixture, "execResult", exec)
}

// TestInMemoryFileOpsMatchGolden replays inmemory-sandbox-files.json: every file
// operation, symlink rule, listing and search order, data removal and dispose step
// of the in-memory sandbox, with its recorded error code and message and every
// clock-derived timestamp.
func TestInMemoryFileOpsMatchGolden(t *testing.T) {
	script := loadReplayScript(t, "inmemory-sandbox-files.json")
	if len(script.Ops) < 300 {
		t.Fatalf("the script has only %d ops", len(script.Ops))
	}
	replay(t, script)
}

// inMemoryFixtures is every golden fixture owned by the in-memory sandbox.
var inMemoryFixtures = []string{
	"inmemory-sandbox-exec.json",
	"inmemory-sandbox-files.json",
}

// TestInMemoryFixturesCovered fails when a fixture with the inmemory- prefix has no
// entry above, and when an entry has no file.
func TestInMemoryFixturesCovered(t *testing.T) {
	entries, err := os.ReadDir(wiretest.Dir(t))
	if err != nil {
		t.Fatalf("read fixture dir: %v", err)
	}
	present := map[string]bool{}
	for _, e := range entries {
		present[e.Name()] = true
		if strings.HasPrefix(e.Name(), "inmemory-") && !slices.Contains(inMemoryFixtures, e.Name()) {
			t.Errorf("fixture %s has no entry in inMemoryFixtures", e.Name())
		}
	}
	for _, f := range inMemoryFixtures {
		if !present[f] {
			t.Errorf("inMemoryFixtures entry %s has no fixture file", f)
		}
	}
}

// TestInMemoryConcurrentWritesAreSafe writes different files from many goroutines
// through two views of one sandbox; under -race it proves the single sandbox mutex
// covers the store and the injected clock.
func TestInMemoryConcurrentWritesAreSafe(t *testing.T) {
	ctx := context.Background()
	provider := aecmock.NewInMemoryProvider(aecmock.WithClock(wiretest.Clock(1700000000000, 1)))
	env, err := provider.Create(ctx, sandboxhub.Spec{Kind: sandboxhub.KindDocker}, sandboxhub.CreateContext{ID: "docker:shared", Tenancy: sandboxhub.TenancyShared})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	var views []sandboxhub.View
	for _, id := range []string{"a1", "a2"} {
		v, err := env.ForAgent(ctx, id)
		if err != nil {
			t.Fatalf("ForAgent: %v", err)
		}
		views = append(views, v)
	}
	const perView = 25
	var wg sync.WaitGroup
	for vi, v := range views {
		for i := range perView {
			wg.Add(1)
			go func() {
				defer wg.Done()
				name := fmt.Sprintf("dir%d/file%d.txt", i%3, i)
				if _, err := v.WriteFile(ctx, name, fmt.Sprintf("content %d %d\n", vi, i), sandboxhub.WriteOptions{}); err != nil {
					t.Errorf("WriteFile %s: %v", name, err)
					return
				}
				if _, err := v.Stat(ctx, name); err != nil {
					t.Errorf("Stat %s: %v", name, err)
				}
				if _, err := v.Search(ctx, sandboxhub.SearchQuery{Pattern: "content"}); err != nil {
					t.Errorf("Search: %v", err)
				}
				if _, err := v.List(ctx, ".", sandboxhub.ListOptions{Recursive: ptrBool(true)}); err != nil {
					t.Errorf("List: %v", err)
				}
			}()
		}
	}
	wg.Wait()
	for vi, v := range views {
		res, err := v.Search(ctx, sandboxhub.SearchQuery{Pattern: fmt.Sprintf("content %d ", vi)})
		if err != nil {
			t.Fatalf("Search: %v", err)
		}
		if len(res.Matches) != perView {
			t.Errorf("view %d found %d matches, want %d", vi, len(res.Matches), perView)
		}
	}
}

func ptrBool(b bool) *bool { return &b }

// TestInMemoryReturnsCopies proves that changing a returned slice or value does not
// change the sandbox: capabilities, roots, list entries, search matches, the
// provider's removal log and the limits are all copies.
func TestInMemoryReturnsCopies(t *testing.T) {
	ctx := context.Background()
	provider := aecmock.NewInMemoryProvider()
	env, err := provider.Create(ctx, sandboxhub.Spec{Kind: sandboxhub.KindDocker}, sandboxhub.CreateContext{ID: "docker:shared", Tenancy: sandboxhub.TenancyShared})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	view, err := env.ForAgent(ctx, "a1")
	if err != nil {
		t.Fatalf("ForAgent: %v", err)
	}

	caps := view.Capabilities()
	caps[0] = "hacked"
	if got := view.Capabilities(); got[0] != sandboxhub.CapabilityFSRead {
		t.Errorf("Capabilities is not a copy: %v", got)
	}
	roots := view.Roots()
	roots[0] = "/etc"
	if got := view.Roots(); got[0] != "/workspace/agents/a1" {
		t.Errorf("Roots is not a copy: %v", got)
	}

	if _, err := view.WriteFile(ctx, "a.txt", "needle\n", sandboxhub.WriteOptions{}); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	list, err := view.List(ctx, ".", sandboxhub.ListOptions{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	list.Entries[0].Name = "changed"
	again, _ := view.List(ctx, ".", sandboxhub.ListOptions{})
	if again.Entries[0].Name != "a.txt" {
		t.Errorf("List entries are not a copy: %+v", again.Entries)
	}
	search, err := view.Search(ctx, sandboxhub.SearchQuery{Pattern: "needle"})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	search.Matches[0].Text = "changed"
	searchAgain, _ := view.Search(ctx, sandboxhub.SearchQuery{Pattern: "needle"})
	if searchAgain.Matches[0].Text != "needle" {
		t.Errorf("Search matches are not a copy: %+v", searchAgain.Matches)
	}

	if err := provider.RemoveData(ctx, sandboxhub.Spec{}, sandboxhub.CreateContext{ID: "docker:shared", Tenancy: sandboxhub.TenancyShared}, "a1"); err != nil {
		t.Fatalf("RemoveData: %v", err)
	}
	removed := provider.RemovedData()
	removed[0].ID = "changed"
	if got := provider.RemovedData(); got[0].ID != "docker:shared" {
		t.Errorf("RemovedData is not a copy: %+v", got)
	}
}

// --- The assertions of the retired Phase 6 FileView, on the in-memory view -------
//
// The Phase 6 FileView of this package was a minimal ReadFile/WriteFile/Stat
// double with the sandbox messages and codes. It is gone (NewInMemoryView replaces it);
// every behavioural assertion of its tests lives on here, on the real in-memory
// sandbox.

func sha(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}

// newView returns a view of a fresh shared in-memory sandbox.
func newView(t *testing.T, id, agentID string) sandboxhub.View {
	t.Helper()
	v, err := aecmock.NewInMemoryView(id, agentID)
	if err != nil {
		t.Fatalf("NewInMemoryView(%q, %q): %v", id, agentID, err)
	}
	return v
}

// requireSandboxError fails unless err is a *kernel.SandboxError with this code,
// message and sandbox id.
func requireSandboxError(t *testing.T, err error, code kernel.SandboxErrorCode, message, id string) {
	t.Helper()
	var se *kernel.SandboxError
	if !errors.As(err, &se) {
		t.Fatalf("error = %v, want a *kernel.SandboxError", err)
	}
	if se.Code != code || se.Message != message || se.SandboxID != id {
		t.Fatalf("SandboxError = {%s %q %q}, want {%s %q %q}", se.Code, se.Message, se.SandboxID, code, message, id)
	}
}

func TestInMemoryViewReadWriteStat(t *testing.T) {
	ctx := t.Context()
	v := newView(t, "docker:shared", "a1")
	if v.ID() != "docker:shared" || v.WorkingDir() != "/workspace/agents/a1" {
		t.Fatalf("ID %q WorkingDir %q", v.ID(), v.WorkingDir())
	}
	if roots := v.Roots(); len(roots) != 1 || roots[0] != "/workspace/agents/a1" {
		t.Fatalf("Roots = %v", roots)
	}

	// Stat of an absent path is nil with no error.
	if st, err := v.Stat(ctx, "artifacts/a.md"); st != nil || err != nil {
		t.Fatalf("Stat absent = %v, %v; want nil, nil", st, err)
	}

	w, err := v.WriteFile(ctx, "artifacts/a.md", "h\u00e9llo", sandboxhub.WriteOptions{})
	if err != nil {
		t.Fatal(err)
	}
	want := sandboxhub.WriteResult{Path: "/workspace/agents/a1/artifacts/a.md", Bytes: 6, SHA256: sha("h\u00e9llo"), Created: true}
	if w != want {
		t.Fatalf("WriteFile = %+v, want %+v", w, want)
	}
	again, err := v.WriteFile(ctx, "/workspace/agents/a1/artifacts/a.md", "second", sandboxhub.WriteOptions{})
	if err != nil || again.Created {
		t.Fatalf("overwrite = %+v, %v; want Created false", again, err)
	}

	got, err := v.ReadFile(ctx, "artifacts/a.md", sandboxhub.ReadOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if got.Path != want.Path || got.Content != "second" || got.Size != 6 || got.Truncated || got.SHA256 != sha("second") {
		t.Fatalf("ReadFile = %+v", got)
	}

	// MaxBytes cuts the content, never the reported size or hash.
	one := 3
	cut, err := v.ReadFile(ctx, "artifacts/a.md", sandboxhub.ReadOptions{MaxBytes: &one})
	if err != nil {
		t.Fatal(err)
	}
	if cut.Content != "sec" || !cut.Truncated || cut.Size != 6 || cut.SHA256 != sha("second") {
		t.Fatalf("ReadFile maxBytes 3 = %+v", cut)
	}
	// A zero maxBytes means the default limit, as the clamp does.
	zero := 0
	whole, err := v.ReadFile(ctx, "artifacts/a.md", sandboxhub.ReadOptions{MaxBytes: &zero})
	if err != nil || whole.Content != "second" || whole.Truncated {
		t.Fatalf("ReadFile maxBytes 0 = %+v, %v", whole, err)
	}
	// A cut inside a character decodes to one replacement character.
	if _, err := v.WriteFile(ctx, "u.txt", "a\u00e9", sandboxhub.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	two := 2
	split, err := v.ReadFile(ctx, "u.txt", sandboxhub.ReadOptions{MaxBytes: &two})
	if err != nil || split.Content != "a\uFFFD" || !split.Truncated {
		t.Fatalf("ReadFile split character = %+v, %v", split, err)
	}

	// An empty file is a file with an empty content and the empty-string hash.
	if _, err := v.WriteFile(ctx, "empty.txt", "", sandboxhub.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	empty, err := v.ReadFile(ctx, "empty.txt", sandboxhub.ReadOptions{})
	if err != nil || empty.Content != "" || empty.Size != 0 || empty.SHA256 != sha("") {
		t.Fatalf("ReadFile empty = %+v, %v", empty, err)
	}

	st, err := v.Stat(ctx, "artifacts/a.md")
	if err != nil || st == nil || st.Type != sandboxhub.EntryFile || st.Size != 6 || st.Path != want.Path {
		t.Fatalf("Stat file = %+v, %v", st, err)
	}
	dir, err := v.Stat(ctx, "artifacts")
	if err != nil || dir == nil || dir.Type != sandboxhub.EntryDirectory || dir.Size != 0 {
		t.Fatalf("Stat directory = %+v, %v", dir, err)
	}

	// Line ranges apply before maxBytes.
	if _, err := v.WriteFile(ctx, "lines.txt", "one\ntwo\nthree", sandboxhub.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	from, to := 2, 2
	ranged, err := v.ReadFile(ctx, "lines.txt", sandboxhub.ReadOptions{StartLine: &from, EndLine: &to})
	if err != nil || ranged.Content != "two" || !ranged.Truncated || ranged.Size != 13 {
		t.Fatalf("ReadFile range = %+v, %v", ranged, err)
	}
}

func TestInMemoryViewWriteOptions(t *testing.T) {
	ctx := t.Context()
	v := newView(t, "docker:shared", "a1")
	if _, err := v.WriteFile(ctx, "a.txt", "one", sandboxhub.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	_, err := v.WriteFile(ctx, "a.txt", "two", sandboxhub.WriteOptions{ExpectedSHA256: sha("other")})
	requireSandboxError(t, err, kernel.SandboxCodeConflict, `"a.txt" changed since it was read (hash mismatch)`, "docker:shared")
	if _, err := v.WriteFile(ctx, "a.txt", "two", sandboxhub.WriteOptions{ExpectedSHA256: sha("one")}); err != nil {
		t.Fatalf("matching expectedSha256: %v", err)
	}
	no := false
	_, err = v.WriteFile(ctx, "deep/er/b.txt", "x", sandboxhub.WriteOptions{CreateDirs: &no})
	requireSandboxError(t, err, kernel.SandboxCodeNotFound, `Directory "/workspace/agents/a1/deep/er" does not exist`, "docker:shared")
	_, err = v.WriteFile(ctx, "big.txt", strings.Repeat("x", 1_000_001), sandboxhub.WriteOptions{})
	requireSandboxError(t, err, kernel.SandboxCodeTooLarge, "Content is 1000001 bytes (limit 1000000)", "docker:shared")
}

func TestInMemoryViewErrors(t *testing.T) {
	ctx := t.Context()
	v := newView(t, "docker:shared", "a1")
	if _, err := v.WriteFile(ctx, "out/x.md", "x", sandboxhub.WriteOptions{}); err != nil {
		t.Fatal(err)
	}

	// not_found names the input path, not the resolved one.
	_, err := v.ReadFile(ctx, "nope.md", sandboxhub.ReadOptions{})
	requireSandboxError(t, err, kernel.SandboxCodeNotFound, `"nope.md" does not exist`, "docker:shared")

	// A directory is invalid_path.
	_, err = v.ReadFile(ctx, "out", sandboxhub.ReadOptions{})
	requireSandboxError(t, err, kernel.SandboxCodeInvalidPath, `"out" is a directory, not a file`, "docker:shared")
	_, err = v.WriteFile(ctx, "out", "x", sandboxhub.WriteOptions{})
	requireSandboxError(t, err, kernel.SandboxCodeInvalidPath, `"out" is a directory`, "docker:shared")

	// A path outside the roots is path_escape, however it is spelled.
	roots := "/workspace/agents/a1"
	for _, p := range []string{"../a2/secret.md", "/etc/passwd", "/workspace/agents/a2/x", "out/../../a2/x"} {
		_, err = v.ReadFile(ctx, p, sandboxhub.ReadOptions{})
		requireSandboxError(t, err, kernel.SandboxCodePathEscape, `"`+p+`" is outside the workspace this agent may use (`+roots+`)`, "docker:shared")
		_, err = v.WriteFile(ctx, p, "x", sandboxhub.WriteOptions{})
		requireSandboxError(t, err, kernel.SandboxCodePathEscape, `"`+p+`" is outside the workspace this agent may use (`+roots+`)`, "docker:shared")
		_, err = v.Stat(ctx, p)
		requireSandboxError(t, err, kernel.SandboxCodePathEscape, `"`+p+`" is outside the workspace this agent may use (`+roots+`)`, "docker:shared")
	}
	for _, p := range []string{`C:\Users\x`, "c:/x", "//server/share"} {
		_, err = v.ReadFile(ctx, p, sandboxhub.ReadOptions{})
		requireSandboxError(t, err, kernel.SandboxCodePathEscape,
			"\""+p+"\" is a host path \u2014 use a workspace path like \"src/app.ts\" or \"/workspace/shared/file\"", "docker:shared")
	}
	_, err = v.ReadFile(ctx, "a\x00b", sandboxhub.ReadOptions{})
	requireSandboxError(t, err, kernel.SandboxCodeInvalidPath, "Path contains a NUL byte", "docker:shared")

	// The operations the Phase 6 FileView refused (capability_unsupported) are
	// offered by the real in-memory view: listing, search and exec work.
	if list, err := v.List(ctx, "out", sandboxhub.ListOptions{}); err != nil || len(list.Entries) != 1 {
		t.Fatalf("List = %+v, %v", list, err)
	}
	if res, err := v.Search(ctx, sandboxhub.SearchQuery{Pattern: "x"}); err != nil || len(res.Matches) != 1 {
		t.Fatalf("Search = %+v, %v", res, err)
	}
	req := sandboxhub.ExecRequest{Command: []string{"true"}, TimeoutMs: 1000, MaxOutputBytes: 1000}
	if res, err := v.Exec(ctx, req, sandboxhub.ExecOptions{}); err != nil || res.ExitCode == nil || *res.ExitCode != 0 {
		t.Fatalf("Exec = %+v, %v", res, err)
	}

	// A cancelled context stops the call before it touches anything.
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	_, err = v.ReadFile(cancelled, "out/x.md", sandboxhub.ReadOptions{})
	requireSandboxError(t, err, kernel.SandboxCodeCancelled, "Operation cancelled", "docker:shared")
}

// TestInMemoryViewRemove covers what FileView.Remove did through the exec rm of the
// in-memory sandbox (the way the tests remove a file): a file goes, its parent
// directory stays, and a missing path, a directory without -r and an escape fail
// with exit code 1 and a stderr line.
func TestInMemoryViewRemove(t *testing.T) {
	ctx := t.Context()
	v := newView(t, "docker:shared", "a1")
	if _, err := v.WriteFile(ctx, "out/x.md", "x", sandboxhub.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	rm := func(args ...string) sandboxhub.ExecResult {
		t.Helper()
		req := sandboxhub.ExecRequest{Command: append([]string{"rm"}, args...), TimeoutMs: 5000, MaxOutputBytes: 1000}
		res, err := v.Exec(ctx, req, sandboxhub.ExecOptions{})
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	if res := rm("out/x.md"); res.ExitCode == nil || *res.ExitCode != 0 || res.Stderr != "" {
		t.Fatalf("rm out/x.md = %+v", res)
	}
	_, err := v.ReadFile(ctx, "out/x.md", sandboxhub.ReadOptions{})
	requireSandboxError(t, err, kernel.SandboxCodeNotFound, `"out/x.md" does not exist`, "docker:shared")
	// The parent directory stays.
	if st, err := v.Stat(ctx, "out"); err != nil || st == nil || st.Type != sandboxhub.EntryDirectory {
		t.Fatalf("Stat out = %+v, %v", st, err)
	}
	for _, tc := range []struct{ arg, stderr string }{
		{"out/x.md", "rm: out/x.md: No such file or directory\n"},
		{"out", "rm: out: is a directory\n"},
		{"../x", "rm: ../x: path_escape\n"},
	} {
		if res := rm(tc.arg); res.ExitCode == nil || *res.ExitCode != 1 || res.Stderr != tc.stderr {
			t.Fatalf("rm %s = %+v, want exit 1 and stderr %q", tc.arg, res, tc.stderr)
		}
	}
}

func TestInMemoryViewTraitsAndConcurrency(t *testing.T) {
	v := newView(t, "docker:shared", "a1")
	if tr := v.Traits(); tr.Kind != sandboxhub.KindDocker || tr.Tenancy != sandboxhub.TenancyShared || tr.OS != sandboxhub.OSLinux || !tr.Isolated {
		t.Fatalf("Traits = %+v", tr)
	}
	caps := v.Capabilities()
	caps[0] = "mutated"
	if v.Capabilities()[0] != sandboxhub.CapabilityFSRead {
		t.Fatal("Capabilities must return a fresh slice")
	}
	roots := v.Roots()
	roots[0] = "/mutated"
	if v.Roots()[0] != "/workspace/agents/a1" {
		t.Fatal("Roots must return a fresh slice")
	}
	if v.Limits().MaxReadBytes != 1_000_000 {
		t.Fatalf("Limits = %+v", v.Limits())
	}

	// Concurrent writers and readers of one path are race free (go test -race).
	done := make(chan struct{})
	for range 8 {
		go func() {
			defer func() { done <- struct{}{} }()
			for range 50 {
				_, _ = v.WriteFile(t.Context(), "c.txt", "x", sandboxhub.WriteOptions{})
				_, _ = v.ReadFile(t.Context(), "c.txt", sandboxhub.ReadOptions{})
				_, _ = v.Stat(t.Context(), "c.txt")
			}
		}()
	}
	for range 8 {
		<-done
	}
}
