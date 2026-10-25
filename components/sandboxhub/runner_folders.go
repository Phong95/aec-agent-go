package sandboxhub

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/Phong95/aec-agent-go/internal/jsstr"
	"github.com/Phong95/aec-agent-go/kernel"
)

// ---------------------------------------------------------------------------
// A-159: the client side of a runner with several folders. Each folder the user chose is
// its own host sandbox view (host.OpenRunnerFolder, where /workspace is that folder);
// this view puts them side by side:
//
//	/workspace/<first>/...   -> the first folder   (the working directory)
//	/workspace/<second>/...  -> the second folder
//
// Every path is resolved here first (against the working folder), routed to the folder it
// names and handed on as that folder's own /workspace path, so each folder still checks
// its paths itself (.., absolute paths, symlinks that lead out -> path_escape). Paths in
// results and error messages are put back under /workspace/<name>. Commands run in the
// folder their cwd names, and only when commands are on for that folder.
// Update (A-160): byte streams are routed the same way, and services (dev servers) start
// in the folder their cwd names (commands on). Their names are unique across the folders;
// Connect reaches only the port of a running service of this runner, never any other
// port on the user's machine. Removing a folder stops its services.
// ---------------------------------------------------------------------------

const maxRunnerFolderSegment = 64

// isFolderNameBad reports the characters runnerFolderName replaces: control characters
// and those that cannot be in a path segment (or a Windows file name).
func isFolderNameBad(r rune) bool {
	if r <= 0x1f || r == 0x7f {
		return true
	}
	return strings.ContainsRune(`/\:*?"<>|`, r)
}

func isSpaceOrDot(r rune) bool { return r == '.' || jsstr.IsSpace(r) }

// RunnerFolderName (A-159) is a folder's segment under /workspace: base with characters
// that cannot be in a path segment (or a Windows file name) replaced, and "-2", "-3" ...
// when taken already has it (compared without case, as on Windows and macOS).
func RunnerFolderName(base string, taken []string) string {
	var b strings.Builder
	bad := false
	for _, r := range base {
		if isFolderNameBad(r) {
			if !bad {
				b.WriteByte('-')
			}
			bad = true
			continue
		}
		bad = false
		b.WriteRune(r)
	}
	name := strings.TrimFunc(b.String(), isSpaceOrDot)
	name = strings.TrimRightFunc(jsstr.TruncateUTF16(name, maxRunnerFolderSegment), isSpaceOrDot)
	if name == "" {
		name = "folder"
	}
	used := make(map[string]bool, len(taken))
	for _, t := range taken {
		used[jsstr.ToLower(t)] = true
	}
	if !used[jsstr.ToLower(name)] {
		return name
	}
	for n := 2; ; n++ {
		suffix := strconv.Itoa(n)
		candidate := jsstr.TruncateUTF16(name, maxRunnerFolderSegment-len(suffix)-1) + "-" + suffix
		if !used[jsstr.ToLower(candidate)] {
			return candidate
		}
	}
}

// RunnerFolderOpener (A-159) opens one folder as a view whose /workspace is that folder
// (host.OpenRunnerFolder). key identifies the folder on this machine (its cleaned
// absolute path, compared without case where the file system is), so the same folder
// is not added twice.
type RunnerFolderOpener func(ctx context.Context, folder string) (view View, key string, err error)

// RunnerFolderSpec (A-159) is one folder the user chose for a runner.
type RunnerFolderSpec struct {
	// Path is the absolute folder path on this machine. Never sent to the server.
	Path string
	// DenyExec refuses commands in this folder. The zero value allows them.
	DenyExec bool
	// Name is its segment under /workspace. "" = the folder's own name (made unique).
	Name string
}

type runnerFolderEntry struct {
	name      string
	path      string
	key       string
	allowExec bool
	view      View
}

// RunnerFolders (A-159) is the folders of one runner as one View. Hand it to
// RunnerHelloFor and ServeRunner. It is safe for concurrent use; create it with
// NewRunnerFolders (or host.OpenRunnerFolders).
type RunnerFolders struct {
	open RunnerFolderOpener

	mu           sync.Mutex
	entries      []*runnerFolderEntry
	listeners    map[int]func([]RunnerFolder)
	nextListener int
	// stopping (A-160) counts the removed folders whose services are still being stopped.
	stopping sync.WaitGroup
}

var (
	_ View         = (*RunnerFolders)(nil)
	_ StreamView   = (*RunnerFolders)(nil)
	_ ServicesView = (*RunnerFolders)(nil)
)

// NewRunnerFolders returns an empty set that opens folders with open.
func NewRunnerFolders(open RunnerFolderOpener) *RunnerFolders {
	return &RunnerFolders{open: open, listeners: map[int]func([]RunnerFolder){}}
}

func rootOf(name string) string { return WorkspaceRoot + "/" + name }

// Folders are the folders as the server sees them, the working one first.
func (f *RunnerFolders) Folders() []RunnerFolder {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.foldersLocked()
}

func (f *RunnerFolders) foldersLocked() []RunnerFolder {
	out := make([]RunnerFolder, len(f.entries))
	for i, e := range f.entries {
		out[i] = RunnerFolder{Name: e.name, AllowExec: e.allowExec}
	}
	return out
}

// Specs are the folders with their paths on this machine (for the app to save).
func (f *RunnerFolders) Specs() []RunnerFolderSpec {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]RunnerFolderSpec, len(f.entries))
	for i, e := range f.entries {
		out[i] = RunnerFolderSpec{Path: e.path, DenyExec: !e.allowExec, Name: e.name}
	}
	return out
}

// ID returns "runner:folders".
func (f *RunnerFolders) ID() string { return "runner:folders" }

// Traits are the first folder's (they are all folders on this machine).
func (f *RunnerFolders) Traits() Traits {
	if e := f.first(); e != nil {
		return e.view.Traits()
	}
	return Traits{}
}

// Limits are the first folder's.
func (f *RunnerFolders) Limits() Limits {
	if e := f.first(); e != nil {
		return e.view.Limits()
	}
	return Limits{}
}

// Capabilities offer commands while any folder allows them.
func (f *RunnerFolders) Capabilities() []Capability {
	f.mu.Lock()
	entries := append([]*runnerFolderEntry(nil), f.entries...)
	f.mu.Unlock()
	caps := []Capability{CapabilityFSRead, CapabilityFSWrite, CapabilityFSSearch}
	exec, shell := false, false
	for _, e := range entries {
		if !e.allowExec {
			continue
		}
		for _, c := range e.view.Capabilities() {
			exec = exec || c == CapabilityExec
			shell = shell || c == CapabilityExecShell
		}
	}
	if exec {
		caps = append(caps, CapabilityExec)
	}
	if shell {
		caps = append(caps, CapabilityExecShell)
	}
	// A-160: every folder streams (each is a host folder), or none is said to.
	stream := len(entries) > 0
	services := false
	for _, e := range entries {
		_, ok := AsStreamView(e.view)
		stream = stream && ok
		_, ok = AsServicesView(e.view)
		services = services || (e.allowExec && ok)
	}
	if stream {
		caps = append(caps, CapabilityFSStream)
	}
	if services {
		caps = append(caps, CapabilityServices)
	}
	return caps
}

// WorkingDir is the first folder, /workspace/<first>.
func (f *RunnerFolders) WorkingDir() string {
	if e := f.first(); e != nil {
		return rootOf(e.name)
	}
	return WorkspaceRoot
}

// Roots are /workspace/<name> of every folder.
func (f *RunnerFolders) Roots() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.entries))
	for i, e := range f.entries {
		out[i] = rootOf(e.name)
	}
	return out
}

// Add adds a folder (last). It fails with conflict when it is already there, and with
// policy_denied past RunnerMaxFolders.
func (f *RunnerFolders) Add(ctx context.Context, spec RunnerFolderSpec) (RunnerFolder, error) {
	f.mu.Lock()
	full := len(f.entries) >= RunnerMaxFolders
	f.mu.Unlock()
	if full {
		return RunnerFolder{}, kernel.NewSandboxError(fmt.Sprintf("A runner offers at most %d folders", RunnerMaxFolders), kernel.SandboxCodePolicyDenied, "")
	}
	view, key, err := f.open(ctx, spec.Path)
	if err != nil {
		return RunnerFolder{}, err
	}
	f.mu.Lock()
	for _, e := range f.entries {
		if e.key == key {
			f.mu.Unlock()
			return RunnerFolder{}, kernel.NewSandboxError(fmt.Sprintf("%q is already attached as %s", spec.Path, rootOf(e.name)), kernel.SandboxCodeConflict, "")
		}
	}
	if len(f.entries) >= RunnerMaxFolders {
		f.mu.Unlock()
		return RunnerFolder{}, kernel.NewSandboxError(fmt.Sprintf("A runner offers at most %d folders", RunnerMaxFolders), kernel.SandboxCodePolicyDenied, "")
	}
	taken := make([]string, len(f.entries))
	for i, e := range f.entries {
		taken[i] = e.name
	}
	base := spec.Name
	if base == "" {
		base = folderBaseName(spec.Path)
	}
	entry := &runnerFolderEntry{name: RunnerFolderName(base, taken), path: spec.Path, key: key, allowExec: !spec.DenyExec, view: view}
	f.entries = append(f.entries, entry)
	folders, listeners := f.changedLocked()
	f.mu.Unlock()
	notifyFolders(listeners, folders)
	return RunnerFolder{Name: entry.name, AllowExec: entry.allowExec}, nil
}

// Remove removes a folder by name and reports whether it was there. The last one
// cannot be removed (invalid_argument: detach the runner instead).
func (f *RunnerFolders) Remove(name string) (bool, error) {
	f.mu.Lock()
	i := f.indexLocked(name)
	if i < 0 {
		f.mu.Unlock()
		return false, nil
	}
	if len(f.entries) == 1 {
		f.mu.Unlock()
		return false, kernel.NewSandboxError("A runner needs at least one folder - detach it instead", kernel.SandboxCodeInvalidArgument, "")
	}
	removed := f.entries[i]
	f.entries = append(f.entries[:i:i], f.entries[i+1:]...)
	folders, listeners := f.changedLocked()
	f.mu.Unlock()
	notifyFolders(listeners, folders)
	// A-160: its dev servers go with it.
	f.stopping.Add(1)
	go func() {
		defer f.stopping.Done()
		stopAllServices(context.Background(), removed.view)
	}()
	return true, nil
}

// StopServices (A-160) stops every service of every folder (the app detaches or quits),
// and waits for the removed folders' services still being stopped.
func (f *RunnerFolders) StopServices(ctx context.Context) {
	f.mu.Lock()
	entries := append([]*runnerFolderEntry(nil), f.entries...)
	f.mu.Unlock()
	var wg sync.WaitGroup
	for _, e := range entries {
		wg.Add(1)
		go func() {
			defer wg.Done()
			stopAllServices(ctx, e.view)
		}()
	}
	wg.Wait()
	f.stopping.Wait()
}

// SetAllowExec turns commands on or off for one folder and reports whether it is there.
func (f *RunnerFolders) SetAllowExec(name string, allowExec bool) bool {
	f.mu.Lock()
	i := f.indexLocked(name)
	if i < 0 {
		f.mu.Unlock()
		return false
	}
	if f.entries[i].allowExec == allowExec {
		f.mu.Unlock()
		return true
	}
	f.entries[i].allowExec = allowExec
	folders, listeners := f.changedLocked()
	f.mu.Unlock()
	notifyFolders(listeners, folders)
	return true
}

// OnChange calls fn with the new list after every Add, Remove or SetAllowExec. It
// returns the function that removes fn.
func (f *RunnerFolders) OnChange(fn func([]RunnerFolder)) (unsubscribe func()) {
	f.mu.Lock()
	id := f.nextListener
	f.nextListener++
	f.listeners[id] = fn
	f.mu.Unlock()
	return func() {
		f.mu.Lock()
		delete(f.listeners, id)
		f.mu.Unlock()
	}
}

// ReadFile reads a file of the folder the path names.
func (f *RunnerFolders) ReadFile(ctx context.Context, path string, opts ReadOptions) (FileContent, error) {
	at, err := f.route(path)
	if err != nil {
		return FileContent{}, err
	}
	out, err := at.entry.view.ReadFile(ctx, at.inner, opts)
	if err != nil {
		return FileContent{}, at.wrap(err)
	}
	out.Path = at.outer(out.Path)
	return out, nil
}

// WriteFile writes a file of the folder the path names.
func (f *RunnerFolders) WriteFile(ctx context.Context, path, content string, opts WriteOptions) (WriteResult, error) {
	at, err := f.route(path)
	if err != nil {
		return WriteResult{}, err
	}
	out, err := at.entry.view.WriteFile(ctx, at.inner, content, opts)
	if err != nil {
		return WriteResult{}, at.wrap(err)
	}
	out.Path = at.outer(out.Path)
	return out, nil
}

// Stat stats a path of the folder it names.
func (f *RunnerFolders) Stat(ctx context.Context, path string) (*FileStat, error) {
	at, err := f.route(path)
	if err != nil {
		return nil, err
	}
	out, err := at.entry.view.Stat(ctx, at.inner)
	if err != nil {
		return nil, at.wrap(err)
	}
	if out != nil {
		s := *out
		s.Path = at.outer(s.Path)
		out = &s
	}
	return out, nil
}

// List lists a directory of the folder it names.
func (f *RunnerFolders) List(ctx context.Context, path string, opts ListOptions) (ListResult, error) {
	at, err := f.route(path)
	if err != nil {
		return ListResult{}, err
	}
	out, err := at.entry.view.List(ctx, at.inner, opts)
	if err != nil {
		return ListResult{}, at.wrap(err)
	}
	for i := range out.Entries {
		out.Entries[i].Path = at.outer(out.Entries[i].Path)
	}
	return out, nil
}

// Search searches the folder q.Path names (default: the working folder).
func (f *RunnerFolders) Search(ctx context.Context, q SearchQuery) (SearchResult, error) {
	where := q.Path
	if where == "" {
		where = f.WorkingDir()
	}
	at, err := f.route(where)
	if err != nil {
		return SearchResult{}, err
	}
	q.Path = at.inner
	out, err := at.entry.view.Search(ctx, q)
	if err != nil {
		return SearchResult{}, at.wrap(err)
	}
	for i := range out.Matches {
		out.Matches[i].Path = at.outer(out.Matches[i].Path)
	}
	return out, nil
}

// Exec runs a command in the folder req.Cwd names (default: the working folder), only
// when commands are on for it (policy_denied otherwise).
func (f *RunnerFolders) Exec(ctx context.Context, req ExecRequest, opts ExecOptions) (ExecResult, error) {
	cwd := req.Cwd
	if cwd == "" {
		cwd = f.WorkingDir()
	}
	at, err := f.route(cwd)
	if err != nil {
		return ExecResult{}, err
	}
	f.mu.Lock()
	allowExec := at.entry.allowExec
	f.mu.Unlock()
	if !allowExec {
		return ExecResult{}, kernel.NewSandboxError(fmt.Sprintf("Commands are turned off for the folder %q in the app", at.entry.name), kernel.SandboxCodePolicyDenied, f.ID())
	}
	req.Cwd = at.inner
	out, err := at.entry.view.Exec(ctx, req, opts)
	if err != nil {
		return ExecResult{}, at.wrap(err)
	}
	return out, nil
}

// --- A-160: byte streams ---------------------------------------------------------

// ReadStream reads a file of the folder the path names as raw bytes.
func (f *RunnerFolders) ReadStream(ctx context.Context, path string, opts ReadStreamOptions) (*ByteStream, error) {
	at, err := f.route(path)
	if err != nil {
		return nil, err
	}
	sv, ok := AsStreamView(at.entry.view)
	if !ok {
		return nil, kernel.NewSandboxError("This folder cannot stream files", kernel.SandboxCodeCapabilityUnsupported, f.ID())
	}
	out, err := sv.ReadStream(ctx, at.inner, opts)
	if err != nil {
		return nil, at.wrap(err)
	}
	out.Path = at.outer(out.Path)
	return out, nil
}

// WriteStream writes raw bytes to a file of the folder the path names.
func (f *RunnerFolders) WriteStream(ctx context.Context, path string, body io.Reader, opts WriteStreamOptions) (WriteResult, error) {
	at, err := f.route(path)
	if err != nil {
		return WriteResult{}, err
	}
	sv, ok := AsStreamView(at.entry.view)
	if !ok {
		return WriteResult{}, kernel.NewSandboxError("This folder cannot stream files", kernel.SandboxCodeCapabilityUnsupported, f.ID())
	}
	out, err := sv.WriteStream(ctx, at.inner, body, opts)
	if err != nil {
		return WriteResult{}, at.wrap(err)
	}
	out.Path = at.outer(out.Path)
	return out, nil
}

// --- A-160: services --------------------------------------------------------------

// StartService starts a service in the folder spec.Cwd names (default: the working
// folder), only when commands are on for it. Names are unique across the folders:
// stop, logs and links find a service by name.
func (f *RunnerFolders) StartService(ctx context.Context, spec ServiceSpec) (ServiceInfo, error) {
	cwd := spec.Cwd
	if cwd == "" {
		cwd = f.WorkingDir()
	}
	at, err := f.route(cwd)
	if err != nil {
		return ServiceInfo{}, err
	}
	f.mu.Lock()
	allowExec := at.entry.allowExec
	entries := append([]*runnerFolderEntry(nil), f.entries...)
	f.mu.Unlock()
	if !allowExec {
		return ServiceInfo{}, kernel.NewSandboxError(fmt.Sprintf("Commands are turned off for the folder %q in the app", at.entry.name), kernel.SandboxCodePolicyDenied, f.ID())
	}
	svc, ok := AsServicesView(at.entry.view)
	if !ok {
		return ServiceInfo{}, kernel.NewSandboxError("This folder cannot run services", kernel.SandboxCodeCapabilityUnsupported, f.ID())
	}
	for _, other := range entries {
		osv, ok := AsServicesView(other.view)
		if other == at.entry || !ok {
			continue
		}
		list, err := osv.ListServices(ctx)
		if err != nil {
			return ServiceInfo{}, err
		}
		for _, s := range list {
			if s.Name == spec.Name && s.Status != ServiceExited {
				return ServiceInfo{}, kernel.NewSandboxError(
					fmt.Sprintf("A service named %q is already running in %q - stop it first", spec.Name, other.name),
					kernel.SandboxCodeConflict, f.ID())
			}
		}
	}
	spec.Cwd = at.inner
	info, err := svc.StartService(ctx, spec)
	if err != nil {
		return ServiceInfo{}, at.wrap(err)
	}
	info.Cwd = at.outer(info.Cwd)
	return info, nil
}

// StopService stops a service by name; nil when no folder has one of that name.
func (f *RunnerFolders) StopService(ctx context.Context, name string) (*ServiceInfo, error) {
	entry, svc, err := f.findService(ctx, name)
	if err != nil || entry == nil {
		return nil, err
	}
	info, err := svc.StopService(ctx, name)
	if err != nil || info == nil {
		return info, err
	}
	out := *info
	out.Cwd = outerOf(entry, out.Cwd)
	return &out, nil
}

// ListServices lists the services of every folder, sorted by name.
func (f *RunnerFolders) ListServices(ctx context.Context) ([]ServiceInfo, error) {
	f.mu.Lock()
	entries := append([]*runnerFolderEntry(nil), f.entries...)
	f.mu.Unlock()
	all := []ServiceInfo{}
	for _, e := range entries {
		svc, ok := AsServicesView(e.view)
		if !ok {
			continue
		}
		list, err := svc.ListServices(ctx)
		if err != nil {
			return nil, err
		}
		for _, s := range list {
			s.Cwd = outerOf(e, s.Cwd)
			all = append(all, s)
		}
	}
	sort.SliceStable(all, func(i, j int) bool { return all[i].Name < all[j].Name })
	return all, nil
}

// ServiceLogs returns the end of a service's output (not_found when no folder has it).
func (f *RunnerFolders) ServiceLogs(ctx context.Context, name string, maxBytes int) (ServiceLogs, error) {
	entry, svc, err := f.findService(ctx, name)
	if err != nil {
		return ServiceLogs{}, err
	}
	if entry == nil {
		return ServiceLogs{}, kernel.NewSandboxError(fmt.Sprintf("There is no service named %q", name), kernel.SandboxCodeNotFound, f.ID())
	}
	return svc.ServiceLogs(ctx, name, maxBytes)
}

// Connect reaches only the port of a running service of this runner: never another port
// on the user's machine (not_found).
func (f *RunnerFolders) Connect(ctx context.Context, port int) (io.ReadWriteCloser, error) {
	f.mu.Lock()
	entries := append([]*runnerFolderEntry(nil), f.entries...)
	f.mu.Unlock()
	for _, e := range entries {
		svc, ok := AsServicesView(e.view)
		if !ok {
			continue
		}
		list, err := svc.ListServices(ctx)
		if err != nil {
			return nil, err
		}
		for _, s := range list {
			if s.Port == port && s.Status != ServiceExited {
				return svc.Connect(ctx, port)
			}
		}
	}
	return nil, kernel.NewSandboxError(fmt.Sprintf("No service of this runner listens on port %d", port), kernel.SandboxCodeNotFound, f.ID())
}

// findService is the folder that has a service of this name (a running one first);
// nil when none has.
func (f *RunnerFolders) findService(ctx context.Context, name string) (*runnerFolderEntry, ServicesView, error) {
	f.mu.Lock()
	entries := append([]*runnerFolderEntry(nil), f.entries...)
	f.mu.Unlock()
	var exited *runnerFolderEntry
	var exitedView ServicesView
	for _, e := range entries {
		svc, ok := AsServicesView(e.view)
		if !ok {
			continue
		}
		list, err := svc.ListServices(ctx)
		if err != nil {
			return nil, nil, err
		}
		for _, s := range list {
			if s.Name != name {
				continue
			}
			if s.Status != ServiceExited {
				return e, svc, nil
			}
			if exited == nil {
				exited, exitedView = e, svc
			}
		}
	}
	return exited, exitedView, nil
}

// outerOf puts a path a folder reports (its own /workspace) under /workspace/<name>.
func outerOf(e *runnerFolderEntry, innerPath string) string {
	root := rootOf(e.name)
	switch {
	case innerPath == WorkspaceRoot:
		return root
	case strings.HasPrefix(innerPath, WorkspaceRoot+"/"):
		return root + innerPath[len(WorkspaceRoot):]
	default:
		return innerPath
	}
}

// stopAllServices (A-160) stops a folder's running services; failures are ignored (the
// folder is going away).
func stopAllServices(ctx context.Context, view View) {
	svc, ok := AsServicesView(view)
	if !ok {
		return
	}
	list, err := svc.ListServices(ctx)
	if err != nil {
		return
	}
	var wg sync.WaitGroup
	for _, s := range list {
		if s.Status == ServiceExited {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = svc.StopService(ctx, s.Name)
		}()
	}
	wg.Wait()
}

// --- Internals --------------------------------------------------------------------

func (f *RunnerFolders) first() *runnerFolderEntry {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.entries) == 0 {
		return nil
	}
	return f.entries[0]
}

func (f *RunnerFolders) indexLocked(name string) int {
	for i, e := range f.entries {
		if e.name == name {
			return i
		}
	}
	return -1
}

func (f *RunnerFolders) changedLocked() ([]RunnerFolder, []func([]RunnerFolder)) {
	listeners := make([]func([]RunnerFolder), 0, len(f.listeners))
	for _, l := range f.listeners {
		listeners = append(listeners, l)
	}
	return f.foldersLocked(), listeners
}

// notifyFolders calls each listener with its own copy; a listener's panic is its own.
func notifyFolders(listeners []func([]RunnerFolder), folders []RunnerFolder) {
	for _, l := range listeners {
		func() {
			defer func() { _ = recover() }()
			l(append([]RunnerFolder(nil), folders...))
		}()
	}
}

// folderRoute is where one logical path goes: the folder, its own /workspace path, and
// how results and errors are put back under /workspace/<name>.
type folderRoute struct {
	entry   *runnerFolderEntry
	logical string
	inner   string
	root    string
	id      string
}

func (r folderRoute) outer(innerPath string) string {
	switch {
	case innerPath == WorkspaceRoot:
		return r.root
	case strings.HasPrefix(innerPath, WorkspaceRoot+"/"):
		return r.root + innerPath[len(WorkspaceRoot):]
	default:
		return innerPath
	}
}

// wrap names this folder's paths in the folder's own error messages.
func (r folderRoute) wrap(err error) error {
	var se *kernel.SandboxError
	if !errors.As(err, &se) {
		return err
	}
	message := strings.ReplaceAll(se.Message, `"`+r.inner+`"`, `"`+r.logical+`"`)
	return kernel.NewSandboxError(message, se.Code, r.id)
}

// route resolves a logical path against the working folder and finds the folder it is in.
func (f *RunnerFolders) route(input string) (folderRoute, error) {
	f.mu.Lock()
	if len(f.entries) == 0 {
		f.mu.Unlock()
		return folderRoute{}, kernel.NewSandboxError("The runner has no folder", kernel.SandboxCodeNotConfigured, f.ID())
	}
	entries := append([]*runnerFolderEntry(nil), f.entries...)
	f.mu.Unlock()
	roots := make([]string, len(entries))
	for i, e := range entries {
		roots[i] = rootOf(e.name)
	}
	logical, err := ResolveLogicalPath(input, roots[0], roots, f.ID())
	if err != nil {
		return folderRoute{}, err
	}
	for i, e := range entries {
		root := roots[i]
		if logical == root || strings.HasPrefix(logical, root+"/") {
			return folderRoute{entry: e, logical: logical, inner: WorkspaceRoot + logical[len(root):], root: root, id: f.ID()}, nil
		}
	}
	// ResolveLogicalPath keeps every path inside the roots.
	return folderRoute{}, kernel.NewSandboxError(fmt.Sprintf("%q is outside the folders of this runner", input), kernel.SandboxCodePathEscape, f.ID())
}

// folderBaseName is the last segment of a folder path (Windows or POSIX).
func folderBaseName(folder string) string {
	trimmed := strings.TrimRight(folder, `\/`)
	if i := strings.LastIndexAny(trimmed, `\/`); i >= 0 {
		trimmed = trimmed[i+1:]
	}
	if trimmed == "" {
		return folder
	}
	return trimmed
}
