package host

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/Phong95/aec-agent-go/kernel"
)

// onWindows is true when this binary runs on Windows. The Windows file-name policy
// of invalidWindowsName is applied to every logical path only there; on Linux and
// macOS the same names are ordinary files (CONTEXT A2).
const onWindows = runtime.GOOS == "windows"

// windowsReservedBases are the device names Windows reserves, in upper case. The
// numbered devices (COM0-COM9 and LPT0-LPT9, with the superscript digits too) are
// handled in reservedDeviceName.
var windowsReservedBases = map[string]bool{
	"CON": true, "PRN": true, "AUX": true, "NUL": true, "CONIN$": true, "CONOUT$": true,
}

// invalidWindowsName reports whether one path segment is a name that Windows
// treats specially or refuses, so the host sandbox rejects it before any file
// access (CONTEXT A2, threat T-07-16):
//
//   - a colon (a drive-relative "C:foo" or an alternate data stream "a:b" or
//     "file.txt:stream") or any of the characters < > " | ? * or an ASCII control
//     character
//   - a trailing dot or space, which Windows strips, so "a." and "a " alias "a"
//     and a name made only of dots aliases the directory itself
//   - a reserved device name (CON, PRN, AUX, NUL, COM0-COM9, LPT0-LPT9, CONIN$,
//     CONOUT$), also with an extension ("NUL.txt") or trailing dots and spaces
//     before the extension ("CON .txt"), in any case
//
// The platform is a parameter so the same table runs on every operating system in
// CI: with windows false every name is accepted, as on Linux. A segment of "." or
// ".." is navigation, not a name (the logical path is normalised before this is
// called), and is accepted.
func invalidWindowsName(segment string, windows bool) bool {
	if !windows || segment == "." || segment == ".." {
		return false
	}
	if strings.ContainsAny(segment, ":<>\"|?*") {
		return true
	}
	for i := range len(segment) {
		if segment[i] < 0x20 {
			return true
		}
	}
	if segment == "" || strings.HasSuffix(segment, ".") || strings.HasSuffix(segment, " ") {
		return true
	}
	return reservedDeviceName(segment)
}

// reservedDeviceName reports whether the base name of the segment (the text before
// the first dot, trailing dots and spaces removed) is a reserved device name.
func reservedDeviceName(segment string) bool {
	base, _, _ := strings.Cut(segment, ".")
	base = strings.ToUpper(strings.TrimRight(base, ". "))
	if windowsReservedBases[base] {
		return true
	}
	for _, prefix := range []string{"COM", "LPT"} {
		digit, ok := strings.CutPrefix(base, prefix)
		if !ok {
			continue
		}
		switch digit {
		case "0", "1", "2", "3", "4", "5", "6", "7", "8", "9",
			"\U000000b9", "\U000000b2", "\U000000b3": // the superscript digits are reserved too
			return true
		}
	}
	return false
}

// checkWindowsNames applies invalidWindowsName to every segment of a logical path
// when running on Windows and fails with invalid_path before any file access. label
// is the path as the caller wrote it. On every other operating system it accepts
// everything.
func checkWindowsNames(logical, label, sandboxID string) error {
	if !onWindows {
		return nil
	}
	for _, segment := range strings.Split(logical, "/") {
		if segment != "" && invalidWindowsName(segment, true) {
			return kernel.NewSandboxError(`"`+label+`" is not a valid file name on Windows`, kernel.SandboxCodeInvalidPath, sandboxID)
		}
	}
	return nil
}

// caseInsensitiveHost is true on the operating systems whose default file systems
// fold case (Windows and macOS): two agent ids that differ only in case can name the
// same folder there. It only says that the check is worth making. On macOS a volume
// may be case-sensitive (APFS can be formatted that way), so checkAgentFolder asks the
// file system itself before it refuses an id.
var caseInsensitiveHost = runtime.GOOS == "windows" || runtime.GOOS == "darwin"

// checkAgentFolder guards the folder a host sandbox derives from an agent id (the
// own sandbox <root>/<agentId> and the shared <base>/agents/<agentId>), which
// AssertSafeAgentID alone does not make unique on the real file system:
//
//   - on Windows a name with a trailing dot or space ("a."), a reserved device name
//     ("con", "nul.x") or an invalid character is refused with invalid_path, because
//     "a." names the folder "a" and "con" is a device (threat T-07-16; the same
//     table that guards every other path segment);
//   - on a case-folding host an id that differs only in case from a folder that
//     already exists in parent ("alice" next to "Alice") is refused with
//     policy_denied, because both ids would share one folder and an
//     agent's files and RemoveData would reach the other's (an isolation hole that
//     would otherwise open). The refusal is made only when the file system confirms
//     the alias (the two spellings are the same folder), so a case-sensitive volume,
//     where "alice" and "Alice" are two folders, keeps accepting both.
//
// parent is the host folder that holds the agent folders and need not exist. The
// platform is a parameter so the table runs on every operating system in CI. A parent
// that cannot be listed for any reason other than "does not exist" (permission denied,
// an I/O error) fails closed with the mapped file system error: the alias check must
// never be skipped without a trace. The existence check and the later MkdirAll are not
// atomic: two creations of aliasing ids in the same instant can both pass, which only
// a host serialising its sandbox creation can exclude.
func checkAgentFolder(parent, agentID, sandboxID string, windows, foldsCase bool) error {
	if windows && invalidWindowsName(agentID, true) {
		return kernel.NewSandboxError(`"`+agentID+`" is not a valid file name on Windows`, kernel.SandboxCodeInvalidPath, sandboxID)
	}
	if !foldsCase {
		return nil
	}
	entries, err := os.ReadDir(parent)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil // a folder that does not exist yet has no alias
		}
		return mapFSError(err, agentID, sandboxID)
	}
	for _, e := range entries {
		name := e.Name()
		if name == agentID || !strings.EqualFold(name, agentID) {
			continue
		}
		same, err := sameFolder(filepath.Join(parent, name), filepath.Join(parent, agentID))
		if err != nil {
			return mapFSError(err, agentID, sandboxID)
		}
		if same {
			return kernel.NewSandboxError(
				`Agent id "`+agentID+`" cannot be used as a sandbox folder name: it differs only in case from the existing folder "`+name+`"`,
				kernel.SandboxCodePolicyDenied, sandboxID)
		}
	}
	return nil
}

// sameFolder reports whether the existing folder at existing and the path at probe are
// one file on disk: on a case-folding volume the probe, spelled differently, resolves to
// the folder; on a case-sensitive volume it does not exist. A path that is missing is
// "not the same" (a folder removed in the meantime has nothing to collide with).
func sameFolder(existing, probe string) (bool, error) {
	a, err := os.Stat(existing)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	b, err := os.Stat(probe)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	return os.SameFile(a, b), nil
}
