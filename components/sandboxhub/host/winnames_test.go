package host

import (
	"os"
	"testing"

	"github.com/Phong95/aec-agent-go/components/sandboxhub"
	"github.com/Phong95/aec-agent-go/kernel"
)

// windowsNameCases are the segments of the Windows file-name policy (CONTEXT A2).
// invalid is the verdict with windows true; with windows false every name is
// accepted, as on Linux.
var windowsNameCases = []struct {
	segment string
	invalid bool
}{
	// Drive-relative names and alternate data streams.
	{"C:foo", true},
	{"a:b", true},
	{"file.txt:stream", true},
	{"c:", true},
	{":x", true},
	// Reserved device names, in any case, with an extension and with trailing dots or spaces.
	{"CON", true},
	{"con", true},
	{"Con", true},
	{"PRN", true},
	{"AUX", true},
	{"aux", true},
	{"NUL", true},
	{"NUL.txt", true},
	{"nul.tar.gz", true},
	{"CON .txt", true},
	{"con..txt", true},
	{"COM0", true},
	{"COM1", true},
	{"com9", true},
	{"LPT0", true},
	{"LPT9", true},
	{"lpt1.txt", true},
	{"COM\U000000b9", true},
	{"LPT\U000000b2", true},
	{"COM\U000000b3.txt", true},
	{"CONIN$", true},
	{"conout$", true},
	{"CONIN$.txt", true},
	// Trailing dots and spaces alias the name without them.
	{"a.", true},
	{"a ", true},
	{"a...", true},
	{"...", true},
	{"  ", true},
	{"a. .", true},
	{"", true},
	// Characters Windows refuses in a name.
	{"a<b", true},
	{"a>b", true},
	{"a\"b", true},
	{"a|b", true},
	{"a?b", true},
	{"a*b", true},
	{"a\x01b", true},
	{"a\x1fb", true},
	// Ordinary names.
	{"a", false},
	{"readme.md", false},
	{".hidden", false},
	{".git", false},
	{"a.b.c", false},
	{"file.txt", false},
	{"a b", false},
	{"...x", false},
	{".a.", true},
	{"x.", true},
	// Names that look reserved but are not.
	{"COM10", false},
	{"COMM", false},
	{"COM", false},
	{"LPT", false},
	{"LPT10", false},
	{"CONSOLE", false},
	{"CONX", false},
	{"NULL", false},
	{"auxiliary", false},
	{"COM1x", false},
	{"com.txt", false},
	{"CONIN", false},
	{"CONIN$x", false},
	// Navigation, not names.
	{".", false},
	{"..", false},
}

func TestInvalidWindowsNameRejectsTheWindowsNames(t *testing.T) {
	for _, c := range windowsNameCases {
		if got := invalidWindowsName(c.segment, true); got != c.invalid {
			t.Errorf("invalidWindowsName(%q, true) = %v, want %v", c.segment, got, c.invalid)
		}
	}
}

func TestInvalidWindowsNameAcceptsEverythingOnOtherPlatforms(t *testing.T) {
	for _, c := range windowsNameCases {
		if invalidWindowsName(c.segment, false) {
			t.Errorf("invalidWindowsName(%q, false) = true, want false (Linux keeps these names legal)", c.segment)
		}
	}
}

// TestCheckWindowsNamesFollowsThePlatform runs the view-level check: on Windows a
// logical path with a bad segment fails with invalid_path and the message with the
// path as the caller wrote it; everywhere else the same path is accepted.
func TestCheckWindowsNamesFollowsThePlatform(t *testing.T) {
	err := checkWindowsNames("/workspace/a/CON/x.txt", "a/CON/x.txt", "host:test")
	if onWindows {
		requireCode(t, err, kernel.SandboxCodeInvalidPath)
		if want := `"a/CON/x.txt" is not a valid file name on Windows`; err.Error() != want {
			t.Errorf("message = %q, want %q", err.Error(), want)
		}
	} else if err != nil {
		t.Errorf("a non-Windows host must accept the path, got %v", err)
	}
	if err := checkWindowsNames("/workspace/agents/a1/ok/file.txt", "ok/file.txt", "host:test"); err != nil {
		t.Errorf("an ordinary path was rejected: %v", err)
	}
}

// TestWindowsNamesAreRejectedBeforeAnyFileAccess (Windows only): the view refuses
// every Windows alias with invalid_path before it touches the disk, and nothing is
// created.
func TestWindowsNamesAreRejectedBeforeAnyFileAccess(t *testing.T) {
	if !onWindows {
		t.Skip("the Windows name policy only applies on Windows")
	}
	root, _, _, view := newOwnView(t)
	inputs := []string{"C:foo", "a:b", "file.txt:stream", "CON", "con.txt", "NUL.txt", "COM1", "LPT1", "CONIN$", "a.", "a<b", "sub/CON/x.txt", "x/y*z"}
	for _, in := range inputs {
		_, err := view.WriteFile(t.Context(), in, "x", sandboxhub.WriteOptions{})
		requireCode(t, err, kernel.SandboxCodeInvalidPath)
		if want := `"` + in + `" is not a valid file name on Windows`; err.Error() != want {
			t.Errorf("write %q: message %q, want %q", in, err.Error(), want)
		}
		_, err = view.ReadFile(t.Context(), in, sandboxhub.ReadOptions{})
		requireCode(t, err, kernel.SandboxCodeInvalidPath)
		_, err = view.Stat(t.Context(), in)
		requireCode(t, err, kernel.SandboxCodeInvalidPath)
		_, err = view.List(t.Context(), in, sandboxhub.ListOptions{})
		requireCode(t, err, kernel.SandboxCodeInvalidPath)
	}
	entries, err := os.ReadDir(root + `\host-agent`)
	if err != nil || len(entries) != 0 {
		t.Errorf("the agent folder must stay empty, got %v (%v)", entries, err)
	}
}

// TestWindowsNamesAreOrdinaryFilesOnOtherSystems (not Windows): the same names are
// ordinary files, so the Windows policy does not change Linux and macOS behaviour.
func TestWindowsNamesAreOrdinaryFilesOnOtherSystems(t *testing.T) {
	if onWindows {
		t.Skip("these names are not legal on Windows")
	}
	_, _, _, view := newOwnView(t)
	for _, in := range []string{"a:b", "C:foo", "NUL.txt", "CON", "a.", "x?y"} {
		if _, err := view.WriteFile(t.Context(), in, "content of "+in, sandboxhub.WriteOptions{}); err != nil {
			t.Errorf("write %q: %v", in, err)
			continue
		}
		read, err := view.ReadFile(t.Context(), in, sandboxhub.ReadOptions{})
		if err != nil || read.Content != "content of "+in {
			t.Errorf("read %q = %+v (%v)", in, read, err)
		}
	}
}
