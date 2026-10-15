package docker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"

	"github.com/Phong95/aec-agent-go/components/sandboxhub"
	"github.com/Phong95/aec-agent-go/internal/viewkit"
	"github.com/Phong95/aec-agent-go/kernel"
)

// Compile-time proof of the view contract.
var _ sandboxhub.View = (*view)(nil)

const (
	// rangeReadMaxBytes: line-range reads load the whole file inside the container;
	// above this they are refused.
	rangeReadMaxBytes = 64 * 1024 * 1024
	// searchMaxFileBytes: search skips files bigger than this ...
	searchMaxFileBytes = 8 * 1024 * 1024
	// searchTimeBudgetMs: ... and stops after this much time (the result is flagged
	// truncated).
	searchTimeBudgetMs = 30_000
)

// dockerCapabilities are the capabilities of a Docker sandbox, in a fixed order.
func dockerCapabilities() []sandboxhub.Capability {
	return []sandboxhub.Capability{
		sandboxhub.CapabilityFSRead, sandboxhub.CapabilityFSWrite, sandboxhub.CapabilityFSSearch,
		sandboxhub.CapabilityExec, sandboxhub.CapabilityExecShell, sandboxhub.CapabilityFSStream,
		sandboxhub.CapabilityServices,
	}
}

// view is one agent's view of a Docker Sandbox. Every file
// operation is one helper request; the helper runs inside the container, where
// symlinks are visible, so the host only canonicalises the logical path.
type view struct {
	sb         *Sandbox
	workingDir string
	roots      []string
}

// ID returns the sandbox id.
func (v *view) ID() string { return v.sb.id }

// Traits returns the sandbox traits.
func (v *view) Traits() sandboxhub.Traits { return v.sb.traits }

// Capabilities returns a fresh slice in the declared order.
func (v *view) Capabilities() []sandboxhub.Capability { return dockerCapabilities() }

// Limits returns the provider's per-operation ceilings.
func (v *view) Limits() sandboxhub.Limits { return v.sb.p.settings.Limits }

// WorkingDir returns the logical directory relative paths resolve against.
func (v *view) WorkingDir() string { return v.workingDir }

// Roots returns a fresh copy of the roots.
func (v *view) Roots() []string { return slices.Clone(v.roots) }

// -- Guards --

func (v *view) fail(message string, code kernel.SandboxErrorCode) error {
	return kernel.NewSandboxError(message, code, v.sb.id)
}

// begin is the guard every file operation runs first: the sandbox must not be
// disposed, it must have the capability, and the operation must not be cancelled.
func (v *view) begin(ctx context.Context, capability sandboxhub.Capability) error {
	if err := v.sb.assertUsable(); err != nil {
		return err
	}
	if !slices.Contains(dockerCapabilities(), capability) {
		return v.fail(fmt.Sprintf("This sandbox does not support %s", capability), kernel.SandboxCodeCapabilityUnsupported)
	}
	if ctx.Err() != nil {
		return v.fail("Operation cancelled", kernel.SandboxCodeCancelled)
	}
	return nil
}

// logical is the host-side canonicalisation (traversal, absolute, host paths);
// symlinks are checked in the container.
func (v *view) logical(path string) (string, error) {
	return sandboxhub.ResolveLogicalPath(path, v.workingDir, v.roots, v.sb.id)
}

// decode reads the verbatim result of a helper answer into the Go type, leniently:
// the helper is a separate program that may gain fields, so unknown fields are
// allowed. An answer of the wrong shape is environment_unavailable.
func (v *view) decode(raw json.RawMessage, into any) error {
	if len(raw) == 0 {
		raw = json.RawMessage("null")
	}
	if err := json.Unmarshal(raw, into); err != nil {
		return v.fail("The sandbox returned an unreadable answer: "+err.Error(), kernel.SandboxCodeEnvironmentUnavailable)
	}
	return nil
}

// -- Helper requests: ordered structs in a fixed key order, the op last --

type readRequest struct {
	Path          string   `json:"path"`
	Roots         []string `json:"roots"`
	MaxBytes      int      `json:"maxBytes"`
	StartLine     *int     `json:"startLine,omitempty"`
	EndLine       *int     `json:"endLine,omitempty"`
	RangeMaxBytes int      `json:"rangeMaxBytes"`
	Op            string   `json:"op"`
}

type writeRequest struct {
	Path           string   `json:"path"`
	Roots          []string `json:"roots"`
	Content        string   `json:"content"`
	CreateDirs     bool     `json:"createDirs"`
	ExpectedSha256 string   `json:"expectedSha256,omitempty"`
	Op             string   `json:"op"`
}

type statRequest struct {
	Path  string   `json:"path"`
	Roots []string `json:"roots"`
	Op    string   `json:"op"`
}

type listRequest struct {
	Path          string   `json:"path"`
	Roots         []string `json:"roots"`
	Recursive     bool     `json:"recursive"`
	IncludeHidden bool     `json:"includeHidden"`
	MaxEntries    int      `json:"maxEntries"`
	Op            string   `json:"op"`
}

type searchRequest struct {
	Path          string   `json:"path"`
	Roots         []string `json:"roots"`
	Pattern       string   `json:"pattern"`
	Literal       bool     `json:"literal"`
	CaseSensitive bool     `json:"caseSensitive"`
	Glob          string   `json:"glob,omitempty"`
	MaxMatches    int      `json:"maxMatches"`
	MaxFileBytes  int      `json:"maxFileBytes"`
	TimeBudgetMs  int      `json:"timeBudgetMs"`
	Op            string   `json:"op"`
}

// -- Files --

// ReadFile reads a file; the helper does the work in the container.
func (v *view) ReadFile(ctx context.Context, path string, opts sandboxhub.ReadOptions) (sandboxhub.FileContent, error) {
	if err := v.begin(ctx, sandboxhub.CapabilityFSRead); err != nil {
		return sandboxhub.FileContent{}, err
	}
	logical, err := v.logical(path)
	if err != nil {
		return sandboxhub.FileContent{}, err
	}
	limit := v.Limits().MaxReadBytes
	raw, err := v.sb.helper(ctx, readRequest{
		Path: logical, Roots: v.roots,
		MaxBytes:  viewkit.ClampOption(opts.MaxBytes, limit, limit),
		StartLine: opts.StartLine, EndLine: opts.EndLine,
		RangeMaxBytes: rangeReadMaxBytes, Op: "read",
	})
	if err != nil {
		return sandboxhub.FileContent{}, err
	}
	var out sandboxhub.FileContent
	err = v.decode(raw, &out)
	return out, err
}

// WriteFile writes a file. A content above the write limit is refused here, before
// any docker call.
func (v *view) WriteFile(ctx context.Context, path, content string, opts sandboxhub.WriteOptions) (sandboxhub.WriteResult, error) {
	if err := v.begin(ctx, sandboxhub.CapabilityFSWrite); err != nil {
		return sandboxhub.WriteResult{}, err
	}
	if size := len(content); size > v.Limits().MaxWriteBytes {
		return sandboxhub.WriteResult{}, v.fail(fmt.Sprintf("Content is %d bytes (limit %d)", size, v.Limits().MaxWriteBytes), kernel.SandboxCodeTooLarge)
	}
	logical, err := v.logical(path)
	if err != nil {
		return sandboxhub.WriteResult{}, err
	}
	raw, err := v.sb.helper(ctx, writeRequest{
		Path: logical, Roots: v.roots, Content: content,
		CreateDirs:     opts.CreateDirs == nil || *opts.CreateDirs,
		ExpectedSha256: opts.ExpectedSHA256, Op: "write",
	})
	if err != nil {
		return sandboxhub.WriteResult{}, err
	}
	var out sandboxhub.WriteResult
	err = v.decode(raw, &out)
	return out, err
}

// Stat returns nil and no error when the path does not exist.
func (v *view) Stat(ctx context.Context, path string) (*sandboxhub.FileStat, error) {
	if err := v.begin(ctx, sandboxhub.CapabilityFSRead); err != nil {
		return nil, err
	}
	logical, err := v.logical(path)
	if err != nil {
		return nil, err
	}
	raw, err := v.sb.helper(ctx, statRequest{Path: logical, Roots: v.roots, Op: "stat"})
	if err != nil {
		return nil, err
	}
	var out *sandboxhub.FileStat
	if err := v.decode(raw, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// List lists a directory.
func (v *view) List(ctx context.Context, path string, opts sandboxhub.ListOptions) (sandboxhub.ListResult, error) {
	if err := v.begin(ctx, sandboxhub.CapabilityFSRead); err != nil {
		return sandboxhub.ListResult{}, err
	}
	logical, err := v.logical(path)
	if err != nil {
		return sandboxhub.ListResult{}, err
	}
	limit := v.Limits().MaxListEntries
	raw, err := v.sb.helper(ctx, listRequest{
		Path: logical, Roots: v.roots,
		Recursive:     opts.Recursive != nil && *opts.Recursive,
		IncludeHidden: opts.IncludeHidden != nil && *opts.IncludeHidden,
		MaxEntries:    viewkit.ClampOption(opts.MaxEntries, limit, limit), Op: "list",
	})
	if err != nil {
		return sandboxhub.ListResult{}, err
	}
	var out sandboxhub.ListResult
	err = v.decode(raw, &out)
	return out, err
}

// Search runs inside the container, in the helper, so the pattern is a JavaScript
// regular expression.
func (v *view) Search(ctx context.Context, q sandboxhub.SearchQuery) (sandboxhub.SearchResult, error) {
	if err := v.begin(ctx, sandboxhub.CapabilityFSSearch); err != nil {
		return sandboxhub.SearchResult{}, err
	}
	if q.Pattern == "" {
		return sandboxhub.SearchResult{}, v.fail("Search pattern must be a non-empty string", kernel.SandboxCodeInvalidArgument)
	}
	where := q.Path
	if where == "" {
		where = v.workingDir
	}
	logical, err := v.logical(where)
	if err != nil {
		return sandboxhub.SearchResult{}, err
	}
	limit := v.Limits().MaxSearchMatches
	raw, err := v.sb.helper(ctx, searchRequest{
		Path: logical, Roots: v.roots, Pattern: q.Pattern,
		Literal:       q.Literal == nil || *q.Literal,
		CaseSensitive: q.CaseSensitive == nil || *q.CaseSensitive,
		Glob:          q.Glob, MaxMatches: viewkit.ClampOption(q.MaxMatches, limit, limit),
		MaxFileBytes: searchMaxFileBytes, TimeBudgetMs: searchTimeBudgetMs, Op: "search",
	})
	if err != nil {
		return sandboxhub.SearchResult{}, err
	}
	var out sandboxhub.SearchResult
	err = v.decode(raw, &out)
	return out, err
}

// -- Commands --

// Exec runs one command through the helper's exec stream: the request is validated,
// the header is built and sent, and a container that is found down
// is recovered once and the command retried. A timeout, a cancel and an output cap are
// reported in the result, not as an error; the command that never started is an error.
func (v *view) Exec(ctx context.Context, req sandboxhub.ExecRequest, opts sandboxhub.ExecOptions) (sandboxhub.ExecResult, error) {
	if err := v.sb.assertUsable(); err != nil {
		return sandboxhub.ExecResult{}, err
	}
	// Both capabilities exist on a Docker sandbox; the checks stay so the failure texts
	// (and a future capability change) follow the capability list.
	if !slices.Contains(dockerCapabilities(), sandboxhub.CapabilityExec) {
		return sandboxhub.ExecResult{}, v.fail("This sandbox does not support exec", kernel.SandboxCodeCapabilityUnsupported)
	}
	shell := req.Shell != nil && *req.Shell
	if shell && !slices.Contains(dockerCapabilities(), sandboxhub.CapabilityExecShell) {
		return sandboxhub.ExecResult{}, v.fail("This sandbox does not support shell commands", kernel.SandboxCodeCapabilityUnsupported)
	}
	if len(req.Command) == 0 || req.Command[0] == "" {
		return sandboxhub.ExecResult{}, v.fail("command must be a non-empty argv array of strings", kernel.SandboxCodeInvalidArgument)
	}
	if req.TimeoutMs <= 0 || req.MaxOutputBytes <= 0 {
		return sandboxhub.ExecResult{}, v.fail("timeoutMs and maxOutputBytes must be positive", kernel.SandboxCodeInvalidArgument)
	}
	// A Go map has no insertion order, so the names are checked in sorted order and
	// the first bad one is reported.
	for _, name := range slices.Sorted(maps.Keys(req.Env)) {
		if !validEnvName(name) {
			return sandboxhub.ExecResult{}, v.fail(fmt.Sprintf("Invalid environment variable \"%s\"", name), kernel.SandboxCodeInvalidArgument)
		}
	}
	cwd := req.Cwd
	if cwd == "" {
		cwd = v.workingDir
	}
	logical, err := v.logical(cwd)
	if err != nil {
		return sandboxhub.ExecResult{}, err
	}
	env := req.Env
	if env == nil {
		env = map[string]string{}
	}
	limits := v.Limits()
	header := execHeader{
		Command: req.Command, Shell: shell, Cwd: logical, Roots: v.roots, Env: env, Stdin: req.Stdin,
		TimeoutMs:      helperTimeoutMs(viewkit.ExecTimeoutMs(req.TimeoutMs, limits.MaxExecTimeoutMs)),
		MaxOutputBytes: min(req.MaxOutputBytes, limits.MaxExecOutputBytes),
	}
	result, err := v.runExec(ctx, header, opts)
	if !errors.Is(err, errContainerDown) {
		return result, err
	}
	if err := v.sb.recover(ctx); err != nil {
		return sandboxhub.ExecResult{}, err
	}
	result, err = v.runExec(ctx, header, opts)
	if errors.Is(err, errContainerDown) {
		return sandboxhub.ExecResult{}, v.containerNotRunning(err)
	}
	return result, err
}
