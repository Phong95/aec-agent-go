package host

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/Phong95/aec-agent-go/components/sandboxhub"
	"github.com/Phong95/aec-agent-go/kernel"
)

// ---------------------------------------------------------------------------
// Host sandbox - a real folder on the server machine, Claude Code-style.
//
//	logical /workspace/...  ->  <base>/...   (base = <root>/<agentId> for own,
//	                                          <root>/.shared for the shared one)
//
// NOT isolated: file operations are confined to the agent's roots (logical checks
// plus a real-path check after symlinks and junctions, then an os.Root), but
// commands run as the server's user with full access to the machine. That is why
// traits.Isolated is false - the approval policy then asks the user before writes
// and commands.
// ---------------------------------------------------------------------------

// hostCapabilities are the capabilities of a host sandbox, in a fixed order: the
// file side, then commands and shell commands (plan 07-05).
func hostCapabilities() []sandboxhub.Capability {
	return []sandboxhub.Capability{
		sandboxhub.CapabilityFSRead, sandboxhub.CapabilityFSWrite, sandboxhub.CapabilityFSSearch,
		sandboxhub.CapabilityExec, sandboxhub.CapabilityExecShell, sandboxhub.CapabilityFSStream,
		sandboxhub.CapabilityServices,
	}
}

// Sandbox is one host sandbox: the folder Base() that logical
// /workspace maps to, shared by the agent views of ForAgent. It is a
// sandboxhub.Sandbox and a sandboxhub.AgentDataRemover and is safe for concurrent use;
// its only mutable state is the disposed flag, which sits under one mutex.
// Update (A-124 phase 2): and the services map, under the same mutex.
type Sandbox struct {
	id     string
	traits sandboxhub.Traits
	limits sandboxhub.Limits
	caps   []sandboxhub.Capability
	// base is the host folder that logical /workspace maps to.
	base string

	mu       sync.Mutex
	disposed bool
	// services are the services of every agent (A-124 phase 2), keyed
	// "<workingDir>" + newline + "<name>"; they end with the sandbox (Dispose).
	services map[string]*hostService
}

func newSandbox(id string, traits sandboxhub.Traits, limits sandboxhub.Limits, base string) *Sandbox {
	return &Sandbox{id: id, traits: traits, limits: limits, caps: hostCapabilities(), base: base}
}

// ID returns the sandbox id the hub assigned.
func (s *Sandbox) ID() string { return s.id }

// Traits returns the sandbox traits.
func (s *Sandbox) Traits() sandboxhub.Traits { return s.traits }

// Base returns the host folder that logical /workspace maps to.
func (s *Sandbox) Base() string { return s.base }

func (s *Sandbox) isDisposed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.disposed
}

func (s *Sandbox) disposedError() error {
	return kernel.NewSandboxError(fmt.Sprintf("Sandbox \"%s\" was disposed", s.id), kernel.SandboxCodeEnvironmentUnavailable, s.id)
}

// toHost maps a logical path (already checked against the roots) to a host path.
// It is the only place a logical path becomes a host path.
func (s *Sandbox) toHost(logical string) string { return toHostPath(s.base, logical) }

// toHostPath maps a logical path below /workspace to the host folder: the part
// after /workspace is split on "/" and joined onto base with the OS path package.
func toHostPath(base, logical string) string {
	rel := strings.TrimPrefix(strings.TrimPrefix(logical, sandboxhub.WorkspaceRoot), "/")
	if rel == "" {
		return base
	}
	return filepath.Join(append([]string{base}, strings.Split(rel, "/")...)...)
}

// ForAgent returns the view one agent works through: the working directory and
// roots of sandboxhub.AgentLayout, with the working directory created. A disposed
// sandbox fails with environment_unavailable.
func (s *Sandbox) ForAgent(_ context.Context, agentID string) (sandboxhub.View, error) {
	if s.isDisposed() {
		return nil, s.disposedError()
	}
	layout, err := sandboxhub.AgentLayout(s.traits.Tenancy, agentID)
	if err != nil {
		return nil, err
	}
	if err := checkWindowsNames(layout.WorkingDir, layout.WorkingDir, s.id); err != nil {
		return nil, err
	}
	if s.traits.Tenancy != sandboxhub.TenancyOwn {
		// A shared agent's folder is <base>/agents/<id>: no aliasing id may share it.
		if err := checkAgentFolder(s.toHost(sandboxhub.AgentsDir), agentID, s.id, onWindows, caseInsensitiveHost); err != nil {
			return nil, err
		}
	}
	if err := os.MkdirAll(s.toHost(layout.WorkingDir), dirPerm); err != nil { //nolint:gosec // the path is the base joined with a validated agent id
		return nil, mapFSError(err, layout.WorkingDir, s.id)
	}
	return &view{sb: s, workingDir: layout.WorkingDir, roots: layout.Roots}, nil
}

// Health is ready while the base folder exists, unavailable when the sandbox was
// disposed or the folder is gone.
func (s *Sandbox) Health(context.Context) sandboxhub.Health {
	if s.isDisposed() {
		return sandboxhub.HealthUnavailable
	}
	st, err := os.Stat(s.base) //nolint:gosec // base is the provider's configured folder
	if err != nil || !st.IsDir() {
		return sandboxhub.HealthUnavailable
	}
	return sandboxhub.HealthReady
}

// RemoveAgentData deletes one agent's folder and everything below it. The id is
// validated first, besides the checks of the hub and the provider's callers: an id
// like ".." would resolve to the parent folder.
func (s *Sandbox) RemoveAgentData(_ context.Context, agentID string) error {
	if err := sandboxhub.AssertSafeAgentID(agentID); err != nil {
		return err
	}
	if err := checkAgentFolder(s.toHost(sandboxhub.AgentsDir), agentID, s.id, onWindows, caseInsensitiveHost); err != nil {
		return err
	}
	return removeTree(s.toHost(sandboxhub.AgentsDir + "/" + agentID))
}

// Dispose marks the sandbox disposed; every later operation fails with
// environment_unavailable. With removeData it also deletes the base folder.
func (s *Sandbox) Dispose(_ context.Context, removeData bool) error {
	s.mu.Lock()
	s.disposed = true
	s.mu.Unlock()
	// A-124 phase 2: services end with their sandbox.
	s.stopServices() //nolint:contextcheck // services end with the sandbox whatever the caller's context
	if removeData {
		return removeTree(s.base)
	}
	return nil
}
