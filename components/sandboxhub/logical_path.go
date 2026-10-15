package sandboxhub

import (
	"path"
	"regexp"
	"strings"

	"github.com/Phong95/aec-agent-go/internal/jsstr"
	"github.com/Phong95/aec-agent-go/kernel"
)

// ---------------------------------------------------------------------------
// Logical paths - the POSIX paths tools use inside every sandbox.
//
//	/workspace/
//	  shared/              <- every agent of the shared sandbox (collaboration space)
//	  agents/<agentId>/    <- one agent's default working directory
//
// Providers resolve every incoming path through ResolveLogicalPath before
// mapping it to real storage, then re-check the REAL location (after symlinks)
// with IsWithinRoots - both layers are needed to stop escapes.
//
// The golden fixture sandbox-logical-path.json pins the behaviour of these
// functions. It uses package path only, never path/filepath: a
// logical path is POSIX on every operating system, and only the host sandbox
// (sandbox/host) ever maps one to a host path.
// ---------------------------------------------------------------------------

// WorkspaceRoot is the logical root of every sandbox.
const WorkspaceRoot = "/workspace"

// AgentsDir holds one folder per agent of a shared sandbox.
//
// A SharedDir constant is intentionally absent: "will skip it for now, each
// agent has its own folder is enough" (/workspace/shared is left out of the
// layout, so it is outside every agent's roots).
const AgentsDir = WorkspaceRoot + "/agents"

// safeAgentID is the safe agent id pattern. RE2 "$" without (?m) matches at the
// end of the text only, so a trailing "\n" is rejected.
var safeAgentID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

// AssertSafeAgentID returns a policy_denied *kernel.SandboxError unless agentID is
// a plain identifier. Agent ids become folder names, so only plain identifiers
// are accepted: the safeAgentID pattern plus the rule that no id may contain "..".
//
// It is exported because the hub, the host sandbox and the in-memory sandbox live
// in other packages. The error carries no sandbox id.
func AssertSafeAgentID(agentID string) error {
	if !safeAgentID.MatchString(agentID) || strings.Contains(agentID, "..") {
		return kernel.NewSandboxError(
			`Agent id "`+agentID+`" cannot be used as a sandbox folder name`,
			kernel.SandboxCodePolicyDenied, "")
	}
	return nil
}

// AgentLayout returns the default layout for an agent's view of a sandbox:
//   - shared tenancy: works in /workspace/agents/<id>. /workspace/shared is
//     left out for now, so each agent only gets its
//     own folder (other agents' folders are off-limits to file operations -
//     accident prevention, not a security boundary: exec in a shared container
//     can still reach them)
//   - own tenancy: the whole /workspace belongs to the agent; the id is NOT
//     validated
//
// A shared layout with an unsafe id returns the AssertSafeAgentID error. Every
// call returns a fresh Roots slice, so a caller may modify it.
func AgentLayout(tenancy Tenancy, agentID string) (Layout, error) {
	if tenancy == TenancyOwn {
		return Layout{WorkingDir: WorkspaceRoot, Roots: []string{WorkspaceRoot}}, nil
	}
	if err := AssertSafeAgentID(agentID); err != nil {
		return Layout{}, err
	}
	own := AgentsDir + "/" + agentID
	// The roots could also hold /workspace/shared - dropped for now.
	return Layout{WorkingDir: own, Roots: []string{own}}, nil
}

// IsWithinRoots reports whether p is one of the roots or below one of them. The
// root "/" contains every absolute path; any other root contains p when p starts
// with root plus "/".
func IsWithinRoots(p string, roots []string) bool {
	for _, root := range roots {
		prefix := root + "/"
		if root == "/" {
			prefix = "/"
		}
		if p == root || strings.HasPrefix(p, prefix) {
			return true
		}
	}
	return false
}

// isHostPath reports a Windows drive path (a letter, a colon, then a slash or
// the end) or a "//" prefix, in the form after backslashes became slashes.
func isHostPath(slashed string) bool {
	if strings.HasPrefix(slashed, "//") {
		return true
	}
	if len(slashed) < 2 || slashed[1] != ':' {
		return false
	}
	c := slashed[0]
	if (c < 'A' || c > 'Z') && (c < 'a' || c > 'z') {
		return false
	}
	return len(slashed) == 2 || slashed[2] == '/'
}

// ResolveLogicalPath is the validator that makes sure a tool executes in the right
// place: in some cases a tool may run with a wrong sandbox path, and this check
// prevents that. It returns the canonical logical path of input inside roots.
//
// A relative input resolves against workingDir. The input is trimmed with the
// JavaScript whitespace set (jsstr.Trim, never strings.TrimSpace) and Windows
// separators are normalised, so "src\\a.ts" means "src/a.ts". Errors are
// *kernel.SandboxError values carrying sandboxID:
//   - invalid_path: the input contains a NUL byte (checked on the raw input,
//     before trimming).
//   - path_escape: a host path (drive letter, "//" prefix, "\\\\?\\") or a path
//     outside every root after normalisation.
//
// Drive-relative "C:foo", alternate data streams ("a:b"), reserved Windows names
// ("CON", "nul.txt") and a trailing dot pass unchanged. The Windows-name
// hardening belongs to sandbox/host only (CONTEXT A2), because this shared
// function must match its fixtures exactly.
func ResolveLogicalPath(input, workingDir string, roots []string, sandboxID string) (string, error) {
	if strings.Contains(input, "\x00") {
		return "", kernel.NewSandboxError("Path contains a NUL byte", kernel.SandboxCodeInvalidPath, sandboxID)
	}
	// Windows-style separators are normalised so "src\\a.ts" means "src/a.ts".
	slashed := strings.ReplaceAll(jsstr.Trim(input), `\`, "/")
	if isHostPath(slashed) {
		return "", kernel.NewSandboxError(
			`"`+input+`" is a host path `+"\U00002014"+` use a workspace path like "src/app.ts" or "`+WorkspaceRoot+`/shared/file"`,
			kernel.SandboxCodePathEscape, sandboxID)
	}

	var joined string
	switch {
	case slashed == "" || slashed == ".":
		joined = workingDir
	case strings.HasPrefix(slashed, "/"):
		joined = slashed
	default:
		joined = path.Join(workingDir, slashed)
	}
	normalized := path.Clean(joined)
	if len(normalized) > 1 && strings.HasSuffix(normalized, "/") {
		normalized = normalized[:len(normalized)-1]
	}

	if !IsWithinRoots(normalized, roots) {
		return "", kernel.NewSandboxError(
			`"`+input+`" is outside the workspace this agent may use (`+strings.Join(roots, ", ")+`)`,
			kernel.SandboxCodePathEscape, sandboxID)
	}
	return normalized, nil
}
