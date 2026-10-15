package sandboxhub

import (
	"context"
	"time"

	"github.com/Phong95/aec-agent-go/internal/jsonx"
)

// ---------------------------------------------------------------------------
// View is the only thing file/terminal tools ever see (A-59).
//
// One contract for every place an agent can work:
//
//	docker - the shared sandbox or an agent's own container (A-60)
//	host   - an allowlisted folder on the server machine, Claude Code-style (later)
//	remote - a runner on another machine: Windows/Revit box, CI (later)
//
// Tools never import os or os/exec for file or process work: every path and
// every command goes through the sandbox, never the host (threat T-05-08).
// Paths are logical POSIX paths ("src/app.ts", "/workspace/shared/x.csv"); each
// sandbox maps them to its real storage, so tools behave the same on Windows
// hosts, Linux containers and remote runners. Every method fails closed with a
// *kernel.SandboxError.
//
// This file holds the View and Sandbox contracts and their data types.
// ---------------------------------------------------------------------------

// Capability is something a sandbox can do.
type Capability string

// The sandbox capabilities.
const (
	CapabilityFSRead   Capability = "fs.read"
	CapabilityFSWrite  Capability = "fs.write"
	CapabilityFSSearch Capability = "fs.search"
	CapabilityExec     Capability = "exec"
	// CapabilityExecShell is exec with Shell true (sh -c / cmd /c) - separate so
	// sandboxes can offer argv-only exec.
	CapabilityExecShell Capability = "exec.shell"
)

// Operation is an operation class the approval policy reasons about: the
// capabilities "fs.read", "fs.write", "fs.search" and "exec". exec.shell is a
// capability, not an operation.
type Operation string

// The four operation classes.
const (
	OperationFSRead   Operation = "fs.read"
	OperationFSWrite  Operation = "fs.write"
	OperationFSSearch Operation = "fs.search"
	OperationExec     Operation = "exec"
)

// Tenancy says who uses a sandbox: "shared" = several agents use it; "own" =
// one agent only.
type Tenancy string

// The two tenancies.
const (
	TenancyShared Tenancy = "shared"
	TenancyOwn    Tenancy = "own"
)

// OS is the operating system commands run on. Tool-facing paths are POSIX
// regardless.
type OS string

// The operating systems a sandbox may run.
const (
	OSLinux   OS = "linux"
	OSWindows OS = "windows"
	OSDarwin  OS = "darwin"
)

// Traits describes what kind of place a sandbox is - policy (e.g. approvals)
// decides from traits, not from the kind name.
type Traits struct {
	Kind Kind `json:"kind"`
	// Isolated true = cannot reach the host filesystem/processes (docker, most
	// remote). host = false.
	Isolated bool        `json:"isolated"`
	Network  NetworkMode `json:"network"`
	// Persistent: files survive restarts (named volume, real folder).
	Persistent bool `json:"persistent"`
	// Tenancy "shared" = several agents use it; "own" = one agent only.
	Tenancy Tenancy `json:"tenancy"`
	// OS commands run on. Tool-facing paths are POSIX regardless.
	OS OS `json:"os"`
}

// Limits are the hard ceilings the sandbox enforces; per-call values above
// these are clamped.
type Limits struct {
	MaxReadBytes       int `json:"maxReadBytes"`
	MaxWriteBytes      int `json:"maxWriteBytes"`
	MaxListEntries     int `json:"maxListEntries"`
	MaxSearchMatches   int `json:"maxSearchMatches"`
	MaxExecTimeoutMs   int `json:"maxExecTimeoutMs"`
	MaxExecOutputBytes int `json:"maxExecOutputBytes"`
}

// MaxExecTimeout returns MaxExecTimeoutMs as a time.Duration.
func (l Limits) MaxExecTimeout() time.Duration {
	return time.Duration(l.MaxExecTimeoutMs) * time.Millisecond
}

// --- Files -----------------------------------------------------------------

// ReadOptions are the options of View.ReadFile (cancellation is the ctx
// parameter of each View method). Every field is
// optional and zero is meaningful, so each is a pointer.
type ReadOptions struct {
	// MaxBytes is the bytes returned at most (clamped to Limits.MaxReadBytes).
	MaxBytes *int `json:"maxBytes,omitempty"`
	// StartLine is the 1-based inclusive line range start, applied before MaxBytes.
	StartLine *int `json:"startLine,omitempty"`
	EndLine   *int `json:"endLine,omitempty"`
}

// FileContent is the result of View.ReadFile.
type FileContent struct {
	// Path is the canonical logical path.
	Path    string `json:"path"`
	Content string `json:"content"`
	// Size of the whole file in bytes.
	Size int `json:"size"`
	// Truncated is true when MaxBytes or the line range cut the content.
	Truncated bool `json:"truncated"`
	// SHA256 (hex) of the whole file - pair with WriteOptions.ExpectedSHA256.
	SHA256 string `json:"sha256"`
}

// WriteOptions are the options of View.WriteFile.
type WriteOptions struct {
	// CreateDirs creates missing parent directories. Default true.
	CreateDirs *bool `json:"createDirs,omitempty"`
	// ExpectedSHA256 is optimistic concurrency: fail with "conflict" unless the
	// current file has this hash.
	ExpectedSHA256 string `json:"expectedSha256,omitempty"`
}

// WriteResult is the result of View.WriteFile.
type WriteResult struct {
	Path  string `json:"path"`
	Bytes int    `json:"bytes"`
	// SHA256 is the hash of what was written - compare with a later ReadFile to
	// verify the write landed.
	SHA256  string `json:"sha256"`
	Created bool   `json:"created"`
}

// EntryType is what a directory entry is.
type EntryType string

// The entry types.
const (
	EntryFile      EntryType = "file"
	EntryDirectory EntryType = "directory"
	EntrySymlink   EntryType = "symlink"
	EntryOther     EntryType = "other"
)

// FileStat is the result of View.Stat.
type FileStat struct {
	Path string    `json:"path"`
	Type EntryType `json:"type"`
	Size int       `json:"size"`
	// ModifiedAt is epoch milliseconds.
	ModifiedAt int64 `json:"modifiedAt"`
}

// ListOptions are the options of View.List.
type ListOptions struct {
	Recursive *bool `json:"recursive,omitempty"`
	// MaxEntries is clamped to Limits.MaxListEntries.
	MaxEntries *int `json:"maxEntries,omitempty"`
	// IncludeHidden includes dot-files. Default false.
	IncludeHidden *bool `json:"includeHidden,omitempty"`
}

// DirEntry is one entry of a ListResult.
type DirEntry struct {
	Path string    `json:"path"`
	Name string    `json:"name"`
	Type EntryType `json:"type"`
	Size int       `json:"size"`
}

// ListResult is the result of View.List. A nil Entries is
// written as [] because the wire field is required.
type ListResult struct {
	Entries   []DirEntry `json:"entries"`
	Truncated bool       `json:"truncated"`
}

// MarshalJSON writes a nil Entries as an empty array.
func (r ListResult) MarshalJSON() ([]byte, error) {
	type plain ListResult
	if r.Entries == nil {
		r.Entries = []DirEntry{}
	}
	return jsonx.Marshal(plain(r))
}

// SearchQuery is the query of View.Search.
type SearchQuery struct {
	Pattern string `json:"pattern"`
	// Path is the directory or file to search. Default: the working directory.
	Path string `json:"path,omitempty"`
	// Literal is a plain-text match (default true). false = regular expression.
	Literal       *bool `json:"literal,omitempty"`
	CaseSensitive *bool `json:"caseSensitive,omitempty"`
	// Glob is a filename filter, e.g. "*.ts".
	Glob string `json:"glob,omitempty"`
	// MaxMatches is clamped to Limits.MaxSearchMatches.
	MaxMatches *int `json:"maxMatches,omitempty"`
}

// SearchMatch is one match of a SearchResult.
type SearchMatch struct {
	Path string `json:"path"`
	// Line is 1-based.
	Line int    `json:"line"`
	Text string `json:"text"`
}

// SearchResult is the result of View.Search. A nil Matches is
// written as [] because the wire field is required.
type SearchResult struct {
	Matches   []SearchMatch `json:"matches"`
	Truncated bool          `json:"truncated"`
}

// MarshalJSON writes a nil Matches as an empty array.
func (r SearchResult) MarshalJSON() ([]byte, error) {
	type plain SearchResult
	if r.Matches == nil {
		r.Matches = []SearchMatch{}
	}
	return jsonx.Marshal(plain(r))
}

// --- Commands --------------------------------------------------------------

// ExecRequest is the request of View.Exec. A nil Command is
// written as [] because the wire field is required.
type ExecRequest struct {
	// Command is argv - no shell parsing unless Shell is true.
	Command []string `json:"command"`
	// Shell runs strings.Join(Command, " ") through the sandbox's shell. Needs the
	// "exec.shell" capability.
	Shell *bool `json:"shell,omitempty"`
	// Cwd is a logical directory. Default: the working directory.
	Cwd string `json:"cwd,omitempty"`
	// Env holds explicit variables only - nothing is inherited from the server process.
	Env   map[string]string `json:"env,omitzero"`
	Stdin string            `json:"stdin,omitempty"`
	// TimeoutMs is required; clamped to Limits.MaxExecTimeoutMs.
	TimeoutMs int `json:"timeoutMs"`
	// MaxOutputBytes is required; clamped to Limits.MaxExecOutputBytes (stdout + stderr).
	MaxOutputBytes int `json:"maxOutputBytes"`
}

// Timeout returns TimeoutMs as a time.Duration.
func (r ExecRequest) Timeout() time.Duration {
	return time.Duration(r.TimeoutMs) * time.Millisecond
}

// MarshalJSON writes a nil Command as an empty array.
func (r ExecRequest) MarshalJSON() ([]byte, error) {
	type plain ExecRequest
	if r.Command == nil {
		r.Command = []string{}
	}
	return jsonx.Marshal(plain(r))
}

// OutputStream names the stream a chunk of live exec output belongs to.
type OutputStream string

// The two output streams of an OnOutput chunk.
const (
	StreamStdout OutputStream = "stdout"
	StreamStderr OutputStream = "stderr"
)

// ExecOptions are the options of View.Exec (cancellation is the ctx parameter).
// It is not wire data.
type ExecOptions struct {
	// OnOutput receives live output as it is produced (long jobs, remote runners).
	OnOutput func(stream OutputStream, data string)
}

// ExecResult is the result of View.Exec.
//
// Timeouts, output overflow and cancellation are reported here (the process is
// killed) rather than returned as an error - the command did run, and its
// partial output matters. Exec returns an error only when the command could not
// be started (bad cwd, capability, env down).
type ExecResult struct {
	// ExitCode is null (nil) when the process was killed (timeout, output cap,
	// cancel). The tag has no omitempty: a nil code is written as null and a 0 as 0.
	ExitCode   *int   `json:"exitCode"`
	Stdout     string `json:"stdout"`
	Stderr     string `json:"stderr"`
	Truncated  bool   `json:"truncated"`
	TimedOut   bool   `json:"timedOut"`
	Cancelled  bool   `json:"cancelled"`
	DurationMs int64  `json:"durationMs"`
}

// View is like a workspace in a sandbox - for the shared sandbox every single
// agent has one sandbox view. Cancellation is the ctx parameter of each method.
type View interface {
	// ID is the stable id, e.g. "docker:shared", "docker:own:<agentId>", "host:my-repo".
	ID() string
	Traits() Traits
	// Capabilities returns a fresh slice in the provider's declared order (a
	// read-only set).
	Capabilities() []Capability
	Limits() Limits
	// WorkingDir is the logical directory relative paths resolve against, e.g.
	// "/workspace/agents/a1".
	WorkingDir() string
	// Roots are the logical roots file operations may touch; anything outside is
	// "path_escape".
	Roots() []string

	ReadFile(ctx context.Context, path string, opts ReadOptions) (FileContent, error)
	WriteFile(ctx context.Context, path, content string, opts WriteOptions) (WriteResult, error)
	// Stat returns nil and no error when the path does not exist.
	Stat(ctx context.Context, path string) (*FileStat, error)
	List(ctx context.Context, path string, opts ListOptions) (ListResult, error)
	// Search runs where the files live (no bulk transfer for remote runners).
	Search(ctx context.Context, q SearchQuery) (SearchResult, error)
	Exec(ctx context.Context, req ExecRequest, opts ExecOptions) (ExecResult, error)
}

// Health is the state of a sandbox.
type Health string

// The health values of a Sandbox.
const (
	HealthReady       Health = "ready"
	HealthStarting    Health = "starting"
	HealthUnavailable Health = "unavailable"
)

// Sandbox is one running (or reconnectable) sandbox, shared by the agents bound
// to it.
type Sandbox interface {
	ID() string
	Traits() Traits
	// ForAgent returns the view one agent works through: it sets the working
	// directory and roots for that agent (shared tenancy: /workspace/agents/<agentId>
	// only - /workspace/shared is left out; own: /workspace) and creates the
	// agent's folder if needed.
	ForAgent(ctx context.Context, agentID string) (View, error)
	Health(ctx context.Context) Health
	// Dispose stops the sandbox. removeData also deletes its files/volume.
	Dispose(ctx context.Context, removeData bool) error
}

// AgentDataRemover is the optional part of Sandbox that removes one agent's data
// from a shared sandbox (agent deleted). A caller checks for it with a type
// assertion.
type AgentDataRemover interface {
	RemoveAgentData(ctx context.Context, agentID string) error
}
