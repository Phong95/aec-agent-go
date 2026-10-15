package host

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// The tests compare host paths with os.SameFile, never as text: Windows can return
// 8.3 short names and a different case for the same folder (CONTEXT A3, RESEARCH
// F-W4), and a junction target spelled by the link is not the spelling of the
// folder it was created from.

// canSymlink reports whether os.Symlink works here (the symlink-only cases: relative
// targets and link loops, which a junction cannot express).
func canSymlink(t *testing.T) bool {
	t.Helper()
	dir := t.TempDir()
	probe := filepath.Join(dir, "probe-link")
	if err := os.Symlink(dir, probe); err != nil {
		return false
	}
	_ = os.Remove(probe)
	return true
}

// requireResolvesTo requires that got is wantDir (the same folder) followed by the
// missing tail components.
func requireResolvesTo(t *testing.T, got, wantDir string, tail ...string) {
	t.Helper()
	prefix := got
	for i := len(tail) - 1; i >= 0; i-- {
		if filepath.Base(prefix) != tail[i] {
			t.Fatalf("%q does not end with the tail %v", got, tail)
		}
		prefix = filepath.Dir(prefix)
	}
	a, err := os.Stat(prefix)
	if err != nil {
		t.Fatalf("stat %q: %v", prefix, err)
	}
	b, err := os.Stat(wantDir)
	if err != nil {
		t.Fatalf("stat %q: %v", wantDir, err)
	}
	if !os.SameFile(a, b) {
		t.Fatalf("%q (from %q) is not the folder %q", prefix, got, wantDir)
	}
}

func mkdirs(t *testing.T, dirs ...string) {
	t.Helper()
	for _, d := range dirs {
		if err := os.MkdirAll(d, 0o750); err != nil {
			t.Fatal(err)
		}
	}
}

func TestRealPathThroughMissingAppendsAMissingTail(t *testing.T) {
	base := t.TempDir()
	got, err := realPathThroughMissing(filepath.Join(base, "a", "b", "c.txt"))
	if err != nil {
		t.Fatal(err)
	}
	requireResolvesTo(t, got, base, "a", "b", "c.txt")

	// A dot-dot inside a missing tail is applied textually, like path.join.
	got, err = realPathThroughMissing(filepath.Join(base, "missing") + string(filepath.Separator) + ".." + string(filepath.Separator) + "x")
	if err != nil {
		t.Fatal(err)
	}
	requireResolvesTo(t, got, base, "x")
}

func TestRealPathThroughMissingReturnsAnExistingPathAsIs(t *testing.T) {
	base := t.TempDir()
	mkdirs(t, filepath.Join(base, "d"))
	if err := os.WriteFile(filepath.Join(base, "d", "f.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := realPathThroughMissing(filepath.Join(base, "d", "f.txt"))
	if err != nil {
		t.Fatal(err)
	}
	requireResolvesTo(t, got, filepath.Join(base, "d", "f.txt"))
	if got != filepath.Clean(got) {
		t.Errorf("%q is not clean", got)
	}
	// A regular file used as a directory is a missing tail (realpath fails and the
	// walk peels), not an error: the operation fails later with the real reason.
	got, err = realPathThroughMissing(filepath.Join(base, "d", "f.txt", "child"))
	if err != nil {
		t.Fatalf("a file used as a directory must not fail the walk: %v", err)
	}
	requireResolvesTo(t, got, filepath.Join(base, "d", "f.txt"), "child")
}

func TestRealPathThroughMissingFollowsALinkInsideTheRoot(t *testing.T) {
	base := t.TempDir()
	mkdirs(t, filepath.Join(base, "dir"))
	makeLink(t, filepath.Join(base, "ln"), filepath.Join(base, "dir"))
	got, err := realPathThroughMissing(filepath.Join(base, "ln", "f.txt"))
	if err != nil {
		t.Fatal(err)
	}
	requireResolvesTo(t, got, filepath.Join(base, "dir"), "f.txt")
	if !hostWithin(got, base) {
		t.Errorf("%q must be inside %q", got, base)
	}
	// The link itself resolves to its target.
	got, err = realPathThroughMissing(filepath.Join(base, "ln"))
	if err != nil {
		t.Fatal(err)
	}
	requireResolvesTo(t, got, filepath.Join(base, "dir"))
}

func TestRealPathThroughMissingResolvesALinkToOutside(t *testing.T) {
	base, outside := t.TempDir(), t.TempDir()
	mkdirs(t, filepath.Join(base, "in"))
	makeLink(t, filepath.Join(base, "in", "ln"), outside)
	got, err := realPathThroughMissing(filepath.Join(base, "in", "ln", "x", "y.txt"))
	if err != nil {
		t.Fatal(err)
	}
	requireResolvesTo(t, got, outside, "x", "y.txt")
	if hostWithin(got, filepath.Join(base, "in")) {
		t.Errorf("%q must be outside %q", got, filepath.Join(base, "in"))
	}
}

func TestRealPathThroughMissingFollowsADanglingLink(t *testing.T) {
	base := t.TempDir()
	target := filepath.Join(base, "gone")
	mkdirs(t, target)
	makeLink(t, filepath.Join(base, "ln"), target)
	if err := os.Remove(target); err != nil {
		t.Fatal(err)
	}
	// The link now points at a folder that does not exist: a write through it would
	// create the target, so the walk follows it to where the write would land.
	got, err := realPathThroughMissing(filepath.Join(base, "ln", "f.txt"))
	if err != nil {
		t.Fatal(err)
	}
	requireResolvesTo(t, got, base, "gone", "f.txt")
}

func TestRealPathThroughMissingFollowsRelativeSymlinkTargets(t *testing.T) {
	if !canSymlink(t) {
		t.Skip("relative link targets need os.Symlink, which is not allowed here")
	}
	base := t.TempDir()
	mkdirs(t, filepath.Join(base, "a"), filepath.Join(base, "b"), filepath.Join(base, "c", "d"))
	// Relative to the directory that holds the link, with a dot-dot.
	if err := os.Symlink(filepath.Join("..", "b"), filepath.Join(base, "a", "up")); err != nil {
		t.Fatal(err)
	}
	got, err := realPathThroughMissing(filepath.Join(base, "a", "up", "f.txt"))
	if err != nil {
		t.Fatal(err)
	}
	requireResolvesTo(t, got, filepath.Join(base, "b"), "f.txt")
	// A dot-dot AFTER a link in a target applies to the resolved folder, as the
	// operating system does: c/d/.. is c, even though a lexical clean of the target
	// "hop/.." (which filepath.Join would give: ".") does not know that hop is a link.
	if err := os.Symlink(filepath.Join(base, "c", "d"), filepath.Join(base, "hop")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("hop"+string(filepath.Separator)+"..", filepath.Join(base, "viahop")); err != nil {
		t.Fatal(err)
	}
	got, err = realPathThroughMissing(filepath.Join(base, "viahop", "f.txt"))
	if err != nil {
		t.Fatal(err)
	}
	requireResolvesTo(t, got, filepath.Join(base, "c"), "f.txt")
}

func TestRealPathThroughMissingStopsFollowingAfter40Hops(t *testing.T) {
	if !canSymlink(t) {
		t.Skip("a long chain of links needs os.Symlink, which is not allowed here")
	}
	base := t.TempDir()
	mkdirs(t, filepath.Join(base, "dir"))
	// l00 -> dir, l01 -> l00, ..., l40 -> l39: resolving l39 takes 40 hops, l40 takes 41.
	name := func(i int) string { return fmt.Sprintf("l%02d", i) }
	for i := range 41 {
		to := filepath.Join(base, "dir")
		if i > 0 {
			to = filepath.Join(base, name(i-1))
		}
		if err := os.Symlink(to, filepath.Join(base, name(i))); err != nil {
			t.Fatal(err)
		}
	}
	got, err := realPathThroughMissing(filepath.Join(base, name(39), "f.txt"))
	if err != nil {
		t.Fatal(err)
	}
	requireResolvesTo(t, got, filepath.Join(base, "dir"), "f.txt") // 40 hops still resolve
	got, err = realPathThroughMissing(filepath.Join(base, name(40), "f.txt"))
	if err != nil {
		t.Fatalf("a chain over the limit must not fail the walk: %v", err)
	}
	// The 41st hop (the link l00) is not followed: the walk stops there and appends
	// the rest, and the operation then fails in the os.Root layer.
	if filepath.Base(filepath.Dir(got)) != name(0) || filepath.Base(got) != "f.txt" {
		t.Errorf("past the hop limit the walk must stop at the link %s, got %q", name(0), got)
	}
}

func TestRealPathThroughMissingSurvivesALinkLoop(t *testing.T) {
	if !canSymlink(t) {
		t.Skip("a link loop needs os.Symlink, which is not allowed here")
	}
	base := t.TempDir()
	if err := os.Symlink(filepath.Join(base, "pong"), filepath.Join(base, "ping")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(base, "ping"), filepath.Join(base, "pong")); err != nil {
		t.Fatal(err)
	}
	got, err := realPathThroughMissing(filepath.Join(base, "ping", "x"))
	if err != nil {
		t.Fatalf("a loop must not fail or hang the walk: %v", err)
	}
	if !hostWithin(got, base) {
		t.Errorf("a looping path stays where the walk stopped, inside %q, got %q", base, got)
	}
}

func TestWithinTable(t *testing.T) {
	cases := []struct {
		name          string
		child, parent string
		sep           string
		fold          bool
		want          bool
	}{
		{"equal", "/a/b", "/a/b", "/", false, true},
		{"below", "/a/b/c", "/a/b", "/", false, true},
		{"sibling with the same prefix", "/a/bc", "/a/b", "/", false, false},
		{"parent of the parent", "/a", "/a/b", "/", false, false},
		{"parent with a trailing separator", "/a/b/c", "/a/b/", "/", false, true},
		{"root parent", "/x", "/", "/", false, true},
		{"case matters on Linux", "/A/B/c", "/a/b", "/", false, false},
		{"case folds on Windows", `C:\Users\Me\Agent\x.txt`, `c:\users\me\AGENT`, `\`, true, true},
		{"windows sibling prefix", `C:\a\bc`, `C:\a\b`, `\`, true, false},
		{"windows drive differs", `D:\a\b`, `C:\a\b`, `\`, true, false},
		{"windows equal ignoring case", `C:\A\B`, `c:\a\b`, `\`, true, true},
		{"windows drive root", `C:\x`, `C:\`, `\`, true, true},
	}
	for _, c := range cases {
		if got := within(c.child, c.parent, c.sep, c.fold); got != c.want {
			t.Errorf("%s: within(%q, %q, %q, %v) = %v, want %v", c.name, c.child, c.parent, c.sep, c.fold, got, c.want)
		}
	}
	base := t.TempDir()
	if !hostWithin(filepath.Join(base, "x"), base) || hostWithin(base+"x", base) {
		t.Error("hostWithin must use the OS separator and be exact about the prefix")
	}
}
