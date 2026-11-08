package sandboxhub

import (
	"context"
	"io"
	"slices"
	"strconv"
	"strings"

	"github.com/Phong95/aec-agent-go/components/events"
	"github.com/Phong95/aec-agent-go/kernel"
)

// ---------------------------------------------------------------------------
// A-216: session sandbox folders - a chat that works only in chosen folders of the
// agent's own sandbox (docker or host), the way a runner (A-153 / A-159) scopes a chat
// to folders on the user's machine.
//
//	SandboxHub.SetSessionFolders(agent, session, ["/workspace/api", "/workspace/web"])
//	SandboxManager.GetSessionSandbox(session) -> Scope(agent's view, folders)
//
// Scope wraps the agent's view: its Roots are the folders and its WorkingDir the first
// one, so every path (files, search, streams, exec and service cwd) outside them fails
// with path_escape - the same check (ResolveLogicalPath) and message every sandbox uses.
// A view that implements RootNarrower (docker, host, the in-memory sandbox) is re-rooted
// on the folders too, so its own real-path check refuses a symlink inside a folder that
// leads out of it. A command can still reach beyond the folders (cd .., an absolute
// path): like runner folders and the shared sandbox, this is accident prevention, not a
// security boundary - an own sandbox is the real isolation.
//
// The hub keeps the folders in memory only; the host app persists them per session and
// restores them at startup (like runners).
// ---------------------------------------------------------------------------

// MaxSessionFolders is the most folders one session may be scoped to (the same cap as
// a runner's folders).
const MaxSessionFolders = 20

// RootNarrower is the optional part of a View that re-roots it: WithRoots returns a view
// of the same sandbox (same id, traits, limits and agent) whose working directory and
// roots are the ones given, with every check of the original view - including the real
// path after symlinks - made against those roots. false = it cannot (a root is not
// inside the view's own roots). The returned view has no lifecycle of its own.
type RootNarrower interface {
	WithRoots(workingDir string, roots []string) (View, bool)
}

// NarrowRoots reports whether every root in roots is inside current - the check a
// RootNarrower makes before it re-roots.
func NarrowRoots(roots, current []string) bool {
	if len(roots) == 0 {
		return false
	}
	for _, r := range roots {
		if !IsWithinRoots(r, current) {
			return false
		}
	}
	return true
}

// NormalizeSessionFolders checks the form of a session's folders and returns them
// canonical: logical absolute paths under /workspace (relative ones resolve against
// workingDir), duplicates dropped, order kept (the first is the working directory).
// Errors are *kernel.SandboxError values: policy_denied for none or more than
// MaxSessionFolders, the ResolveLogicalPath errors for a bad path. It does not look
// at the sandbox: SandboxManager.SetSessionFolders also checks each one is a folder of
// the agent's view.
func NormalizeSessionFolders(folders []string, workingDir string, roots []string) ([]string, error) {
	if len(folders) == 0 {
		return nil, kernel.NewSandboxError("Give at least one folder", kernel.SandboxCodePolicyDenied, "")
	}
	if workingDir == "" {
		workingDir = WorkspaceRoot
	}
	if len(roots) == 0 {
		roots = []string{WorkspaceRoot}
	}
	out := make([]string, 0, len(folders))
	for _, f := range folders {
		p, err := ResolveLogicalPath(f, workingDir, roots, "")
		if err != nil {
			return nil, err
		}
		if !slices.Contains(out, p) {
			out = append(out, p)
		}
	}
	if len(out) > MaxSessionFolders {
		return nil, kernel.NewSandboxError(
			"A session can be scoped to at most "+strconv.Itoa(MaxSessionFolders)+" folders", kernel.SandboxCodePolicyDenied, "")
	}
	return out, nil
}

// --- The scoped view ---------------------------------------------------------------

// scopedView is the agent's view limited to the session's folders.
type scopedView struct {
	// inner is the agent's view as it was given (for Roots outside the scope, nothing).
	inner View
	// target gets the calls: inner re-rooted on the folders when it is a RootNarrower,
	// else inner itself (then only the logical check here guards the paths).
	target  View
	folders []string
}

var (
	_ View         = (*scopedView)(nil)
	_ StreamView   = (*scopedView)(nil)
	_ ServicesView = (*scopedView)(nil)
)

// Scope (A-216) returns v limited to folders (canonical logical paths, the working one
// first; see NormalizeSessionFolders). Every folder must be inside v's roots, else it
// fails with path_escape. Paths outside the folders fail with path_escape; a command
// runs in the first folder unless its cwd names another one inside them, but it can
// still reach beyond them (see the file comment). Traits, limits, capabilities and id
// stay v's.
func Scope(v View, folders []string) (View, error) {
	if len(folders) == 0 {
		return v, nil
	}
	roots := v.Roots()
	for _, f := range folders {
		if !IsWithinRoots(f, roots) {
			return nil, kernel.NewSandboxError(
				`The folder "`+f+`" is outside the workspace this agent may use (`+strings.Join(roots, ", ")+`)`,
				kernel.SandboxCodePathEscape, v.ID())
		}
	}
	folders = slices.Clone(folders)
	target := v
	if n, ok := v.(RootNarrower); ok {
		if narrowed, ok := n.WithRoots(folders[0], folders); ok {
			target = narrowed
		}
	}
	return &scopedView{inner: v, target: target, folders: folders}, nil
}

func (s *scopedView) ID() string                 { return s.inner.ID() }
func (s *scopedView) Traits() Traits             { return s.inner.Traits() }
func (s *scopedView) Capabilities() []Capability { return s.inner.Capabilities() }
func (s *scopedView) Limits() Limits             { return s.inner.Limits() }
func (s *scopedView) WorkingDir() string         { return s.folders[0] }
func (s *scopedView) Roots() []string            { return slices.Clone(s.folders) }

// resolve is the logical check of every path: inside the folders, canonical.
func (s *scopedView) resolve(p string) (string, error) {
	return ResolveLogicalPath(p, s.folders[0], s.folders, s.inner.ID())
}

func (s *scopedView) ReadFile(ctx context.Context, p string, opts ReadOptions) (FileContent, error) {
	logical, err := s.resolve(p)
	if err != nil {
		return FileContent{}, err
	}
	return s.target.ReadFile(ctx, logical, opts)
}

func (s *scopedView) WriteFile(ctx context.Context, p, content string, opts WriteOptions) (WriteResult, error) {
	logical, err := s.resolve(p)
	if err != nil {
		return WriteResult{}, err
	}
	return s.target.WriteFile(ctx, logical, content, opts)
}

func (s *scopedView) Stat(ctx context.Context, p string) (*FileStat, error) {
	logical, err := s.resolve(p)
	if err != nil {
		return nil, err
	}
	return s.target.Stat(ctx, logical)
}

func (s *scopedView) List(ctx context.Context, p string, opts ListOptions) (ListResult, error) {
	logical, err := s.resolve(p)
	if err != nil {
		return ListResult{}, err
	}
	return s.target.List(ctx, logical, opts)
}

func (s *scopedView) Search(ctx context.Context, q SearchQuery) (SearchResult, error) {
	logical, err := s.resolve(q.Path)
	if err != nil {
		return SearchResult{}, err
	}
	q.Path = logical
	return s.target.Search(ctx, q)
}

func (s *scopedView) Exec(ctx context.Context, req ExecRequest, opts ExecOptions) (ExecResult, error) {
	cwd, err := s.resolve(req.Cwd)
	if err != nil {
		return ExecResult{}, err
	}
	req.Cwd = cwd
	return s.target.Exec(ctx, req, opts)
}

func (s *scopedView) unsupported(c Capability) error {
	return kernel.NewSandboxError("This sandbox does not support "+string(c), kernel.SandboxCodeCapabilityUnsupported, s.inner.ID())
}

func (s *scopedView) ReadStream(ctx context.Context, p string, opts ReadStreamOptions) (*ByteStream, error) {
	sv, ok := AsStreamView(s.target)
	if !ok {
		return nil, s.unsupported(CapabilityFSStream)
	}
	logical, err := s.resolve(p)
	if err != nil {
		return nil, err
	}
	return sv.ReadStream(ctx, logical, opts)
}

func (s *scopedView) WriteStream(ctx context.Context, p string, body io.Reader, opts WriteStreamOptions) (WriteResult, error) {
	sv, ok := AsStreamView(s.target)
	if !ok {
		return WriteResult{}, s.unsupported(CapabilityFSStream)
	}
	logical, err := s.resolve(p)
	if err != nil {
		return WriteResult{}, err
	}
	return sv.WriteStream(ctx, logical, body, opts)
}

// services are the inner view's, not the re-rooted one's: docker and host key an agent's
// services by its view's working directory, so a re-rooted view would see (and count) a
// set of its own. The cwd of a new service is still checked against the folders here.
func (s *scopedView) services() (ServicesView, error) {
	sv, ok := AsServicesView(s.inner)
	if !ok {
		return nil, s.unsupported(CapabilityServices)
	}
	return sv, nil
}

// StartService starts the service in the first folder unless its cwd names another
// folder inside the scope.
func (s *scopedView) StartService(ctx context.Context, spec ServiceSpec) (ServiceInfo, error) {
	sv, err := s.services()
	if err != nil {
		return ServiceInfo{}, err
	}
	cwd, err := s.resolve(spec.Cwd)
	if err != nil {
		return ServiceInfo{}, err
	}
	spec.Cwd = cwd
	return sv.StartService(ctx, spec)
}

// StopService, ListServices, ServiceLogs and Connect are the agent's services (one set
// per agent, whatever session started them).
func (s *scopedView) StopService(ctx context.Context, name string) (*ServiceInfo, error) {
	sv, err := s.services()
	if err != nil {
		return nil, err
	}
	return sv.StopService(ctx, name)
}

func (s *scopedView) ListServices(ctx context.Context) ([]ServiceInfo, error) {
	sv, err := s.services()
	if err != nil {
		return nil, err
	}
	return sv.ListServices(ctx)
}

func (s *scopedView) ServiceLogs(ctx context.Context, name string, maxBytes int) (ServiceLogs, error) {
	sv, err := s.services()
	if err != nil {
		return ServiceLogs{}, err
	}
	return sv.ServiceLogs(ctx, name, maxBytes)
}

func (s *scopedView) Connect(ctx context.Context, port int) (io.ReadWriteCloser, error) {
	sv, err := s.services()
	if err != nil {
		return nil, err
	}
	return sv.Connect(ctx, port)
}

// --- Session folders on the hub ------------------------------------------------------

// SetSessionFolders (A-216) scopes one session of one agent to folders of the agent's
// sandbox: the session's workspace tools (SandboxManager.GetSessionSandbox) then work
// only in them. folders are logical paths under /workspace, the working one first; they
// are checked for form only (NormalizeSessionFolders) - SandboxManager.SetSessionFolders
// (Agent.SetSessionFolders) also checks each one is a folder of the agent's sandbox, so
// a host app restoring saved folders at startup may call this directly. It replaces the
// session's folders, emits SandboxSessionFoldersChanged and returns the canonical list.
// A runner attached to the session still wins over its folders.
func (h *SandboxHub) SetSessionFolders(agentID, sessionID string, folders []string) ([]string, error) {
	if err := AssertSafeAgentID(agentID); err != nil {
		return nil, err
	}
	canonical, err := NormalizeSessionFolders(folders, WorkspaceRoot, nil)
	if err != nil {
		return nil, err
	}
	h.mu.Lock()
	previous := h.sessionFolders[runnerKey{agentID, sessionID}]
	h.sessionFolders[runnerKey{agentID, sessionID}] = canonical
	h.mu.Unlock()
	if !slices.Equal(previous, canonical) {
		h.emitSessionFoldersChanged(agentID, sessionID, canonical)
	}
	return slices.Clone(canonical), nil
}

// ClearSessionFolders (A-216) gives the session the agent's whole sandbox back. It
// reports whether the session had folders.
func (h *SandboxHub) ClearSessionFolders(agentID, sessionID string) bool {
	key := runnerKey{agentID, sessionID}
	h.mu.Lock()
	_, had := h.sessionFolders[key]
	delete(h.sessionFolders, key)
	h.mu.Unlock()
	if had {
		h.emitSessionFoldersChanged(agentID, sessionID, nil)
	}
	return had
}

// SessionFolders (A-216) are the folders the session is scoped to, the working one
// first; false = none (the whole sandbox).
func (h *SandboxHub) SessionFolders(agentID, sessionID string) ([]string, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	f, ok := h.sessionFolders[runnerKey{agentID, sessionID}]
	return slices.Clone(f), ok
}

// sessionFolderSessions lists the sessions of agentID that have folders.
func (h *SandboxHub) sessionFolderSessions(agentID string) []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []string
	for k := range h.sessionFolders {
		if k.agentID == agentID {
			out = append(out, k.sessionID)
		}
	}
	return out
}

func (h *SandboxHub) emitSessionFoldersChanged(agentID, sessionID string, folders []string) {
	h.mu.Lock()
	b := h.bus
	h.mu.Unlock()
	if b == nil {
		return
	}
	if folders == nil {
		folders = []string{}
	}
	events.Emit(b, events.SandboxSessionFoldersChanged, sessionID, events.SandboxSessionFoldersChangedPayload{
		AgentID: agentID, SessionID: sessionID, Folders: slices.Clone(folders),
	})
}
