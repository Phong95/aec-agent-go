package skills

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strconv"
	"strings"

	"github.com/Phong95/aec-agent-go/components/sandboxhub"
	"github.com/Phong95/aec-agent-go/components/skillhub/skill"
	"github.com/Phong95/aec-agent-go/components/toolhub"
	"github.com/Phong95/aec-agent-go/internal/jsstr"
	"github.com/Phong95/aec-agent-go/kernel"
)

// WorkspaceDir is where a skill's files are copied, relative to the agent's
// working directory.
const WorkspaceDir = ".skills"

// hostFolderError is the reason a copy is refused on a sandbox that is not isolated.
const hostFolderError = "the workspace is a host folder, so files are not copied automatically"

// removeTimeoutMs and removeMaxOutputBytes bound the rm -rf of removeDir.
const (
	removeTimeoutMs      = 10_000
	removeMaxOutputBytes = 4_096
)

// CopyToWorkspace copies a skill (SKILL.md plus its files) to
// .skills/<name>/ in the agent's workspace, skipping files that are already up to
// date. SKILL.md is written too, so the folder is the
// whole skill: scripts or instructions that refer to it, or a skill used as the
// starting point for a new one, find it on disk. With replace, the folder is removed
// first so files the skill no longer has do not linger (skill_manage after a
// change).
//
// It copies only into an isolated sandbox: on a host folder writes need the user's
// approval, which a skill load must not bypass, so the result is then an Err. With no
// sandbox for the agent (GetSandbox is nil or fails) the result is the zero value.
// Every write goes through sandboxhub.View; the paths come from the skill name and the
// skill package's file paths, which must be safe relative paths (a skill that holds
// another one is refused with an Err before anything is written, T-10-18).
func CopyToWorkspace(ctx context.Context, s skill.Skill, tc toolhub.Context, replace bool) WorkspaceResult {
	env, res, ok := isolatedSandbox(ctx, tc)
	if !ok {
		return res
	}
	dir, err := skillDir(s.Name)
	if err != nil {
		return WorkspaceResult{Err: err.Error()}
	}
	files := make([]skill.File, 0, len(s.Files)+1)
	files = append(files, skill.File{Path: "SKILL.md", Content: s.Content})
	for _, f := range s.Files {
		if p, ok := skill.NormalizeSkillPath(f.Path); !ok || p != f.Path {
			return WorkspaceResult{Err: `the skill has an unsafe file path "` + f.Path + `"`}
		}
		files = append(files, f)
	}

	if replace {
		if err := removeDir(ctx, env, dir); err != nil {
			return WorkspaceResult{Err: err.Error()}
		}
	}
	for _, f := range files {
		target := dir + "/" + f.Path
		current, err := currentSHA256(ctx, env, target)
		if err != nil {
			return WorkspaceResult{Err: err.Error()}
		}
		if current == sha256Hex(f.Content) {
			continue
		}
		if _, err := env.WriteFile(ctx, target, f.Content, sandboxhub.WriteOptions{}); err != nil {
			return WorkspaceResult{Err: err.Error()}
		}
	}
	return WorkspaceResult{Dir: dir}
}

// RemoveFromWorkspace removes a deleted skill's .skills/<name>/ folder from the
// agent's workspace. A folder that does not exist is a
// success. The result follows the rules of CopyToWorkspace.
func RemoveFromWorkspace(ctx context.Context, name string, tc toolhub.Context) WorkspaceResult {
	env, res, ok := isolatedSandbox(ctx, tc)
	if !ok {
		return res
	}
	dir, err := skillDir(name)
	if err != nil {
		return WorkspaceResult{Err: err.Error()}
	}
	stat, err := env.Stat(ctx, dir)
	if err != nil {
		return WorkspaceResult{Err: err.Error()}
	}
	if stat == nil {
		return WorkspaceResult{Dir: dir}
	}
	if err := removeDir(ctx, env, dir); err != nil {
		return WorkspaceResult{Err: err.Error()}
	}
	return WorkspaceResult{Dir: dir}
}

// skillDir is the workspace folder of a skill. The name must be a plain folder name:
// a skill name from the hub always is (a-z, 0-9 and single hyphens), but a skill
// handed in by another AgentSkills could hold a path, which must never reach the
// sandbox as a different folder.
func skillDir(name string) (string, error) {
	if p, ok := skill.NormalizeSkillPath(name); !ok || p != name || strings.Contains(name, "/") {
		return "", errors.New(`the skill name "` + name + `" is not a safe folder name`)
	}
	return WorkspaceDir + "/" + name, nil
}

// isolatedSandbox resolves the agent's sandbox for a copy. The bool is true only when
// there is an isolated sandbox to use; otherwise the result is what the caller
// returns: the zero value when the agent has no sandbox (skill_view(path) still
// works), an Err for a host folder.
func isolatedSandbox(ctx context.Context, tc toolhub.Context) (sandboxhub.View, WorkspaceResult, bool) {
	if tc.GetSandbox == nil {
		return nil, WorkspaceResult{}, false
	}
	env, err := tc.GetSandbox(ctx)
	if err != nil || env == nil {
		return nil, WorkspaceResult{}, false // no sandbox for this agent
	}
	if !env.Traits().Isolated {
		return nil, WorkspaceResult{Err: hostFolderError}, false
	}
	return env, WorkspaceResult{}, true
}

// removeDir removes a folder with rm -rf (argv, no shell), because sandboxhub.View has
// no delete. Without exec, or on a Windows sandbox, the old files stay: the caller
// still writes the current ones over them. A failing rm is an error that carries its
// stderr, or the exit code when stderr is empty.
func removeDir(ctx context.Context, env sandboxhub.View, dir string) error {
	if !hasCapability(env, sandboxhub.CapabilityExec) || env.Traits().OS == sandboxhub.OSWindows {
		return nil
	}
	res, err := env.Exec(ctx, sandboxhub.ExecRequest{
		Command:        []string{"rm", "-rf", "--", dir},
		TimeoutMs:      removeTimeoutMs,
		MaxOutputBytes: removeMaxOutputBytes,
	}, sandboxhub.ExecOptions{})
	if err != nil {
		return err
	}
	if res.ExitCode == nil || *res.ExitCode != 0 {
		reason := jsstr.Trim(res.Stderr)
		if reason == "" {
			// A killed process has no exit code and is reported as "null".
			code := "null"
			if res.ExitCode != nil {
				code = strconv.Itoa(*res.ExitCode)
			}
			reason = "exit " + code
		}
		return errors.New("could not remove " + dir + ": " + reason)
	}
	return nil
}

// hasCapability reports whether the sandbox offers a capability.
func hasCapability(env sandboxhub.View, c sandboxhub.Capability) bool {
	for _, have := range env.Capabilities() {
		if have == c {
			return true
		}
	}
	return false
}

// currentSHA256 is the hash of the file in the sandbox, or "" when it does not exist
// (a not_found SandboxError). Any other failure is returned.
func currentSHA256(ctx context.Context, env sandboxhub.View, path string) (string, error) {
	one := 1
	got, err := env.ReadFile(ctx, path, sandboxhub.ReadOptions{MaxBytes: &one})
	if err != nil {
		var se *kernel.SandboxError
		if errors.As(err, &se) && se.Code == kernel.SandboxCodeNotFound {
			return "", nil
		}
		return "", err
	}
	return got.SHA256, nil
}

// sha256Hex is the hex SHA-256 of the UTF-8 text.
func sha256Hex(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}
