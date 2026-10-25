package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/Phong95/aec-agent-go/components/agent/agentloop"
	"github.com/Phong95/aec-agent-go/components/sandboxhub"
	"github.com/Phong95/aec-agent-go/internal/nilcheck"
	"github.com/Phong95/aec-agent-go/kernel"
)

// SandboxManager is the per-agent sandbox lifecycle manager.
//
// The shared sandbox is managed by the global sandboxhub.SandboxHub. Own sandboxes are
// managed here: created lazily on first use, cached, and recreated when the agent's
// sandbox selection changes. Their data is deleted here too (Release with
// removeData): the hub only checks own specs against its policy.
//
// It is the agentloop.Sandboxes port of the agent's loop. All methods are safe for
// concurrent use. One own sandbox is created at a time: concurrent GetSandbox calls
// share one in-flight creation that runs on a context detached from the first
// caller, so a caller that gives up does not fail the others (the way the hub
// treats the shared sandbox).
type SandboxManager struct {
	hub       *sandboxhub.SandboxHub
	agentID   string
	selection func() sandboxhub.Selection

	// mu guards own, fingerprint and hasFingerprint, and the orphan flags of the
	// futures. It is never held across a provider call. Lock order: mu, then the
	// hub's lock (Provider), never the other way round.
	mu sync.Mutex
	// own is the running own sandbox, or the creation in flight. A failed creation
	// is cleared, so the next call retries.
	own *ownFuture
	// fingerprint is the JSON text of the spec own was built for; hasFingerprint
	// tells an unset fingerprint from an empty one.
	fingerprint    string
	hasFingerprint bool
	// closed is set by Release and never cleared: a manager whose agent was released
	// creates no sandbox any more, so nothing can be created after the last Release
	// and left running (WR-A02).
	closed bool
	// reconfig is non-nil while a recreation disposes the stale own sandbox; it is
	// closed when that is done. A creator that finds it waits, so two sandboxes with
	// the same id never exist at once (WR-A06).
	reconfig chan struct{}
	// followed maps child sessions (a subagent's run) to the parent session whose
	// runner they use (A-153). Guarded by mu.
	followed map[string]string
}

var (
	_ agentloop.Sandboxes        = (*SandboxManager)(nil)
	_ agentloop.SessionSandboxes = (*SandboxManager)(nil)
)

// ownFuture is the in-flight (or finished) creation of the own sandbox. sb and err
// are written once, under the manager lock, before done is closed, so a reader that
// has received from done may read them without the lock. orphaned and
// orphanRemoveData are guarded by the manager lock.
type ownFuture struct {
	done   chan struct{}
	sb     sandboxhub.Sandbox
	err    error
	cancel context.CancelFunc
	// orphaned is set when a caller stopped waiting for this creation for good
	// (Release or a recreation whose ctx ended), so the goroutine that finishes it
	// must dispose the sandbox itself, with orphanRemoveData.
	orphaned         bool
	orphanRemoveData bool
}

// wait returns the outcome of the future once it is done, or ctx.Err() when ctx ends
// first. A finished future wins over a cancelled ctx.
func (f *ownFuture) wait(ctx context.Context) (sandboxhub.Sandbox, error) {
	select {
	case <-f.done:
		return f.result()
	default:
	}
	select {
	case <-f.done:
		return f.result()
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// result returns the outcome of a finished future.
func (f *ownFuture) result() (sandboxhub.Sandbox, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.sb, nil
}

// NewSandboxManager returns the manager of the agent agentID over the hub. selection
// returns the agent's CURRENT sandbox selection and is called on every GetSandbox,
// so the profile's sandbox is read each time; it must be safe to call from any
// goroutine. The zero Selection means the shared sandbox. A nil selection means the
// shared sandbox always.
func NewSandboxManager(hub *sandboxhub.SandboxHub, agentID string, selection func() sandboxhub.Selection) *SandboxManager {
	if selection == nil {
		selection = func() sandboxhub.Selection { return sandboxhub.Selection{} }
	}
	return &SandboxManager{hub: hub, agentID: agentID, selection: selection, followed: map[string]string{}}
}

// GetSessionSandbox (A-153) is GetSandbox for one session: a session the user attached
// a runner to (a folder on their machine) gets that folder - also while the runner is
// offline, so its calls fail rather than land in another sandbox. Every other session
// gets GetSandbox. Only workspace tools ask per session.
func (m *SandboxManager) GetSessionSandbox(ctx context.Context, sessionID string) (sandboxhub.View, error) {
	if sb, ok := m.hub.Runner(m.agentID, m.sessionOf(sessionID)); ok {
		return sb.ForAgent(ctx, m.agentID)
	}
	return m.GetSandbox(ctx)
}

// FollowSession (A-153): a subagent's run (child session) works where its parent
// session does - in the parent's runner folder when it has one. It returns the
// function that ends it (run over).
func (m *SandboxManager) FollowSession(childSessionID, parentSessionID string) func() {
	m.mu.Lock()
	m.followed[childSessionID] = parentSessionID
	m.mu.Unlock()
	return func() {
		m.mu.Lock()
		if m.followed[childSessionID] == parentSessionID {
			delete(m.followed, childSessionID)
		}
		m.mu.Unlock()
	}
}

// DescribeRunner (A-153) is the session's runner folder for /status; "" = the session
// has none.
func (m *SandboxManager) DescribeRunner(sessionID string) string {
	info, ok := m.hub.RunnerInfo(m.agentID, m.sessionOf(sessionID))
	if !ok {
		return ""
	}
	offline := ""
	if !info.Connected {
		offline = " (offline)"
	}
	// A-159: several folders.
	if len(info.Folders) > 1 {
		names := make([]string, len(info.Folders))
		for i, f := range info.Folders {
			names[i] = `"` + f.Name + `"`
		}
		return "the folders " + strings.Join(names, ", ") + " on the user's computer" + offline
	}
	return `the folder "` + info.FolderName + `" on the user's computer` + offline
}

// RunnerFoldersIndex (A-159) is the session's runner folders for the system message
// (<workspace_folders>), so the model knows where each one is; false = the session has
// no runner.
func (m *SandboxManager) RunnerFoldersIndex(sessionID string) (string, bool) {
	sb, ok := m.hub.Runner(m.agentID, m.sessionOf(sessionID))
	if !ok {
		return "", false
	}
	folders := sb.Folders()
	lines := []string{"<workspace_folders>"}
	if sb.Layout() == sandboxhub.RunnerLayoutFlat {
		var folder sandboxhub.RunnerFolder
		if len(folders) > 0 {
			folder = folders[0]
		}
		off := ""
		if !folder.AllowExec {
			off = " (commands are off)"
		}
		lines = append(lines, `The workspace tools work in the folder "`+folder.Name+`" on the user's computer, at /workspace`+off+".")
	} else {
		lines = append(lines, "The workspace tools work in these folders on the user's computer:")
		for i, f := range folders {
			var notes []string
			if i == 0 {
				notes = append(notes, "the working directory: relative paths and commands start here")
			}
			if !f.AllowExec {
				notes = append(notes, "commands are off")
			}
			line := "- /workspace/" + f.Name
			if len(notes) > 0 {
				line += " (" + strings.Join(notes, "; ") + ")"
			}
			lines = append(lines, line)
		}
		if len(folders) > 1 {
			lines = append(lines, "Use the full path (/workspace/<folder>/...) for files in the other folders, and the terminal's cwd to run a command in one.")
		}
	}
	if !sb.Connected() {
		lines = append(lines, "The app is offline: these tools fail until it reconnects.")
	}
	lines = append(lines, "</workspace_folders>")
	return strings.Join(lines, "\n"), true
}

// sessionOf is the session whose runner sessionID uses: its parent's while it follows one.
func (m *SandboxManager) sessionOf(sessionID string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if parent, ok := m.followed[sessionID]; ok {
		return parent
	}
	return sessionID
}

// GetSandbox returns the agent's sandbox view, creating the sandbox on first use.
// It fails closed with a *kernel.SandboxError: "none" is disabled,
// an unconfigured shared sandbox is not_configured, a kind without a provider is
// provider_unavailable and a selection the policy refuses is policy_denied. When
// ctx ends while the sandbox is being created it returns the ctx error and the
// creation carries on for the other callers.
func (m *SandboxManager) GetSandbox(ctx context.Context) (sandboxhub.View, error) {
	if m.isClosed() {
		return nil, errSandboxReleased("")
	}
	sel := m.selection()
	switch sel.Mode {
	case sandboxhub.SelectionNone:
		return nil, kernel.NewSandboxError(`This agent has no sandbox (sandbox: "none")`, kernel.SandboxCodeDisabled, "")
	case "", sandboxhub.SelectionShared:
		// The shared sandbox is managed by the hub.
		return m.hub.Resolve(ctx, m.agentID, sandboxhub.Selection{Mode: sandboxhub.SelectionShared})
	case sandboxhub.SelectionOwn:
		// Own sandbox - managed here (a nil Own means the zero OwnConfig).
		var own sandboxhub.OwnConfig
		if sel.Own != nil {
			own = *sel.Own
		}
		return m.resolveOwn(ctx, own)
	default:
		return nil, kernel.NewSandboxError("Unrecognised sandbox selection", kernel.SandboxCodePolicyDenied, "")
	}
}

// isClosed says whether Release was called on the manager.
func (m *SandboxManager) isClosed() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.closed
}

// errSandboxReleased is the error of a manager that Release closed: its agent was
// disposed, so it must not bring a sandbox (or the data folder of a deleted agent)
// back to life.
func errSandboxReleased(id string) error {
	return kernel.NewSandboxError("This agent was disposed; its sandbox is no longer available", kernel.SandboxCodeDisabled, id)
}

// RequiresApproval says whether op needs the user's approval in a sandbox with
// these traits (host folders: writes and commands do by default).
func (m *SandboxManager) RequiresApproval(traits sandboxhub.Traits, op sandboxhub.Operation) bool {
	return m.hub.RequiresApproval(traits, op)
}

// Validate checks a new selection against the hub policy before it is saved on the
// profile. It returns a *kernel.SandboxError when the selection is
// refused and creates nothing.
func (m *SandboxManager) Validate(sel sandboxhub.Selection) error {
	return m.hub.ValidateSelection(m.agentID, sel)
}

// Release stops this agent's own sandbox, if any. removeData also
// deletes its files, including cold data: a volume or folder left from an earlier
// run, or from a selection since switched away from. The running sandbox is
// disposed with removeData; then, for removeData only, the data of every other kind
// the policy allows is removed through its provider, when the provider can. Every
// removal is attempted and the first failure is returned.
//
// A creation still in flight is cancelled and awaited. When ctx ends first, Release
// returns the ctx error and the goroutine that finishes the creation disposes the
// sandbox, so none is leaked. A recreation that is disposing the stale sandbox is
// awaited too.
//
// Release closes the manager for good: GetSandbox fails after it (a
// *kernel.SandboxError with SandboxCodeDisabled), so a tool call that was in flight
// when the agent was disposed cannot create a sandbox that nothing would release. A
// second Release has nothing to stop but is not a no-op: with removeData it still
// removes the cold data, so a Release that failed (Docker was down) or that kept the
// data can be retried as RemoveAgent does.
func (m *SandboxManager) Release(ctx context.Context, removeData bool) error {
	m.mu.Lock()
	m.closed = true
	running := m.own
	m.own = nil
	m.fingerprint, m.hasFingerprint = "", false
	reconfig := m.reconfig
	m.mu.Unlock()

	if reconfig != nil {
		// A recreation is disposing the stale sandbox of the same id: its data must not
		// be removed under its feet.
		select {
		case <-reconfig:
		case <-ctx.Done():
			return ctx.Err()
		}
	}

	var managed sandboxhub.Sandbox
	if running != nil {
		running.cancel()
		sb, err := m.settle(ctx, running, removeData)
		if err != nil {
			return err
		}
		managed = sb
	}
	if managed != nil {
		if err := managed.Dispose(ctx, removeData); err != nil {
			return err
		}
	}
	if !removeData {
		return nil
	}

	// Not running in this process: remove the cold own-sandbox data through the
	// provider, for every kind the agent may have used (it can switch kinds; each
	// keeps its own files).
	if err := sandboxhub.AssertSafeAgentID(m.agentID); err != nil {
		return err
	}
	var failures []error
	for _, kind := range m.hub.AllowedOwnKinds() {
		if managed != nil && managed.Traits().Kind == kind {
			continue
		}
		provider, ok := m.hub.Provider(kind)
		if !ok {
			continue
		}
		remover, ok := provider.(sandboxhub.ProviderDataRemover)
		if !ok || nilcheck.IsNil(provider) {
			continue
		}
		cc := sandboxhub.CreateContext{ID: ownSandboxID(kind, m.agentID), Tenancy: sandboxhub.TenancyOwn, AgentID: m.agentID}
		if err := remover.RemoveData(ctx, sandboxhub.Spec{Kind: kind}, cc, ""); err != nil {
			failures = append(failures, err)
		}
	}
	if len(failures) > 0 {
		return failures[0]
	}
	return nil
}

// settle waits for a future that Release or a recreation took out of the manager. It
// returns the sandbox, or nil when the creation failed, and
// an error only when ctx ended first: the future is then orphaned, so the goroutine
// that finishes the creation disposes the sandbox with removeData instead of
// leaking it.
func (m *SandboxManager) settle(ctx context.Context, f *ownFuture, removeData bool) (sandboxhub.Sandbox, error) {
	select {
	case <-f.done:
	default:
		select {
		case <-f.done:
		case <-ctx.Done():
			m.mu.Lock()
			select {
			case <-f.done:
				// Finished meanwhile: take it after all.
			default:
				f.orphaned, f.orphanRemoveData = true, removeData
				m.mu.Unlock()
				return nil, ctx.Err()
			}
			m.mu.Unlock()
		}
	}
	// sb is set only by a successful creation, so a failed one is nil here.
	return f.sb, nil
}

// --- Own sandbox lifecycle ---

// resolveOwn returns the view of the own sandbox for config, creating the
// sandbox, or recreating it when the spec changed.
func (m *SandboxManager) resolveOwn(ctx context.Context, config sandboxhub.OwnConfig) (sandboxhub.View, error) {
	// The agent id becomes part of a path or a container name inside the provider; the
	// built-in providers check it themselves, a custom one need not (WR-A03).
	if err := sandboxhub.AssertSafeAgentID(m.agentID); err != nil {
		return nil, err
	}
	spec, err := m.hub.BuildOwnSpec(config)
	if err != nil {
		return nil, err
	}
	raw, err := kernel.EncodeJSON(spec)
	if err != nil {
		return nil, fmt.Errorf("agent: fingerprint of the own sandbox spec: %w", err)
	}
	fingerprint := string(raw)
	sandboxID := ownSandboxID(spec.Kind, m.agentID)

	m.mu.Lock()
	// A recreation that is disposing the stale sandbox owns the id until it is done:
	// wait for it, then look again (WR-A06).
	for m.reconfig != nil && !m.closed {
		wait := m.reconfig
		m.mu.Unlock()
		select {
		case <-wait:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		m.mu.Lock()
	}
	if m.closed {
		m.mu.Unlock()
		return nil, errSandboxReleased(sandboxID)
	}

	// Config changed: dispose the old sandbox (its files are kept), build a new one.
	var stale *ownFuture
	if m.hasFingerprint && m.fingerprint != fingerprint {
		stale, m.own = m.own, nil
	}
	m.fingerprint, m.hasFingerprint = fingerprint, true
	if stale != nil {
		reconfig := make(chan struct{})
		m.reconfig = reconfig
		m.mu.Unlock()
		old, err := m.settle(ctx, stale, false)
		if err == nil && old != nil {
			// The old sandbox's own failure is ignored.
			_ = old.Dispose(ctx, false)
		}
		m.mu.Lock()
		m.reconfig = nil
		close(reconfig)
		if err != nil {
			m.mu.Unlock()
			return nil, err
		}
		if m.closed {
			// Release ran while the stale sandbox was being disposed.
			m.mu.Unlock()
			return nil, errSandboxReleased(sandboxID)
		}
	}

	// m.mu is held here.
	fut := m.own
	if fut == nil {
		provider, ok := m.hub.Provider(spec.Kind)
		if !ok {
			m.mu.Unlock()
			return nil, kernel.NewSandboxError(
				`No sandbox provider is registered for "`+string(spec.Kind)+`"`,
				kernel.SandboxCodeProviderUnavailable, sandboxID)
		}
		if err := ctx.Err(); err != nil {
			m.mu.Unlock()
			return nil, err
		}
		// The creation outlives the caller that started it, so it runs on a context
		// that keeps the caller's values but not its cancellation. Release cancels it.
		createCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
		fut = &ownFuture{done: make(chan struct{}), cancel: cancel}
		m.own = fut
		go m.create(createCtx, fut, provider, spec, sandboxID)
	}
	m.mu.Unlock()

	managed, err := fut.wait(ctx)
	if err != nil {
		return nil, err
	}
	view, err := managed.ForAgent(ctx, m.agentID)
	if err != nil {
		return nil, kernel.AsSandboxError(err, sandboxID)
	}
	return view, nil
}

// create runs provider.Create and publishes the outcome on fut. A failed creation is
// not cached: the next call retries (for example Docker Desktop was still
// starting). The cache is cleared only when it still holds this future (Release or a
// recreation may have replaced it), before done is closed, so a waiter that sees the
// error can already retry. A panic in the provider becomes an error, because this
// goroutine has no caller to recover it.
func (m *SandboxManager) create(ctx context.Context, fut *ownFuture, p sandboxhub.Provider, spec sandboxhub.Spec, id string) {
	sb, err := safeCreateOwn(ctx, p, spec, sandboxhub.CreateContext{ID: id, Tenancy: sandboxhub.TenancyOwn, AgentID: m.agentID})

	var orphan sandboxhub.Sandbox
	var orphanRemoveData bool
	m.mu.Lock()
	if err != nil {
		if m.own == fut {
			m.own = nil
		}
		fut.err = kernel.AsSandboxError(err, id)
	} else {
		fut.sb = sb
		if fut.orphaned {
			orphan, orphanRemoveData = sb, fut.orphanRemoveData
		}
	}
	// Closed under the lock: settle checks done and sets orphaned under the same
	// lock, so exactly one of the two disposes the sandbox.
	close(fut.done)
	m.mu.Unlock()

	if err != nil {
		fut.cancel()
	}
	if orphan != nil {
		_ = orphan.Dispose(context.WithoutCancel(ctx), orphanRemoveData)
	}
}

func safeCreateOwn(ctx context.Context, p sandboxhub.Provider, spec sandboxhub.Spec, cc sandboxhub.CreateContext) (sb sandboxhub.Sandbox, err error) {
	defer func() {
		if r := recover(); r != nil {
			sb, err = nil, fmt.Errorf("provider panicked: %v", r)
		}
	}()
	sb, err = p.Create(ctx, spec, cc)
	if err == nil && nilcheck.IsNil(sb) {
		return nil, errors.New("provider returned a nil sandbox")
	}
	return sb, err
}

// ownSandboxID is the id of an agent's own sandbox, <kind>:own:<agentId>.
func ownSandboxID(kind sandboxhub.Kind, agentID string) string {
	return string(kind) + ":own:" + agentID
}
