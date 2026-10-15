package host

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"

	"github.com/Phong95/aec-agent-go/components/sandboxhub"
	"github.com/Phong95/aec-agent-go/kernel"
)

// Compile-time proofs of the sandbox contracts.
var (
	_ sandboxhub.Provider            = (*Provider)(nil)
	_ sandboxhub.ProviderDataRemover = (*Provider)(nil)
	_ sandboxhub.Sandbox             = (*Sandbox)(nil)
	_ sandboxhub.AgentDataRemover    = (*Sandbox)(nil)
	_ sandboxhub.View                = (*view)(nil)
)

// Permissions of the folders and files a host sandbox creates. They are the usual
// defaults (0o777 and 0o666 minus the umask): a host sandbox holds
// plain files that people open from the machine, and it is not isolated.
const (
	dirPerm  os.FileMode = 0o755
	filePerm os.FileMode = 0o644
)

// DefaultLimits returns the per-operation ceilings of a host sandbox, a fresh value
// each call. Options.Limits overrides them.
func DefaultLimits() sandboxhub.Limits {
	return sandboxhub.Limits{
		MaxReadBytes:       2_000_000,
		MaxWriteBytes:      5_000_000,
		MaxListEntries:     5_000,
		MaxSearchMatches:   500,
		MaxExecTimeoutMs:   10 * 60_000,
		MaxExecOutputBytes: 2_000_000,
	}
}

// Provider creates host sandboxes: real folders on the
// server machine, not isolated.
//
//	own sandbox      <root>/<agentId>             the whole /workspace
//	shared sandbox   <root>/.shared/agents/<id>   one folder per agent
//
// Files are plain files - they survive restarts and can be opened from the
// machine. Commands run as the server's user, so the default approval policy asks
// the user before every write and command in a host sandbox. It is a
// sandboxhub.Provider, a sandboxhub.ProviderDataRemover and a kernel.Plugin named
// sandbox:host, and it is safe for concurrent use (it has no mutable state).
type Provider struct {
	root   string
	limits sandboxhub.Limits
}

// New returns a provider for the given root. The root must be non-empty and
// absolute, otherwise the error text names the given root; it is
// cleaned like path.resolve.
func New(opts Options) (*Provider, error) {
	if opts.Root == "" || !filepath.IsAbs(opts.Root) {
		return nil, fmt.Errorf("HostSandboxProvider needs an absolute root folder (got \"%s\")", opts.Root)
	}
	return &Provider{root: filepath.Clean(opts.Root), limits: DefaultLimits().Apply(opts.Limits)}, nil
}

// Name returns the plugin name, sandbox:host.
func (*Provider) Name() string { return "sandbox:host" }

// Version returns the plugin version, 1.0.0.
func (*Provider) Version() string { return "1.0.0" }

// Kind returns host.
func (*Provider) Kind() sandboxhub.Kind { return sandboxhub.KindHost }

// Root returns the absolute, cleaned folder that holds every host sandbox.
func (p *Provider) Root() string { return p.root }

// Create creates the host folder of a sandbox if it does not exist yet and returns
// the sandbox. A spec of another kind is provider_unavailable; an own sandbox needs
// cc.AgentID, which must be a safe agent id.
func (p *Provider) Create(_ context.Context, spec sandboxhub.Spec, cc sandboxhub.CreateContext) (sandboxhub.Sandbox, error) {
	if spec.Kind != sandboxhub.KindHost {
		return nil, kernel.NewSandboxError(
			fmt.Sprintf("HostSandboxProvider cannot create \"%s\" sandboxes", spec.Kind), kernel.SandboxCodeProviderUnavailable, cc.ID)
	}
	base, err := p.baseFor(cc)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(base, dirPerm); err != nil { //nolint:gosec // base is the provider's configured root joined with a validated agent id, never profile data
		return nil, kernel.NewSandboxError(
			fmt.Sprintf("Cannot create host folder \"%s\": %s", base, err.Error()), kernel.SandboxCodeEnvironmentUnavailable, cc.ID)
	}
	traits := sandboxhub.Traits{
		Kind:     sandboxhub.KindHost,
		Isolated: false,
		// Commands run on the server machine with its network.
		Network:    sandboxhub.NetworkFull,
		Persistent: true,
		Tenancy:    cc.Tenancy,
		OS:         hostOS(),
	}
	return newSandbox(cc.ID, traits, p.limits, base), nil
}

// hostOS maps runtime.GOOS to the OS trait: windows, darwin or linux.
func hostOS() sandboxhub.OS {
	switch runtime.GOOS {
	case "windows":
		return sandboxhub.OSWindows
	case "darwin":
		return sandboxhub.OSDarwin
	default:
		return sandboxhub.OSLinux
	}
}

// RemoveData deletes the stored data of a sandbox that is not running in this
// process: with a non-empty agentID and shared tenancy that agent's folder (the id
// is validated first), otherwise the whole sandbox folder. A folder that does not
// exist is not an error.
func (p *Provider) RemoveData(_ context.Context, _ sandboxhub.Spec, cc sandboxhub.CreateContext, agentID string) error {
	if agentID != "" && cc.Tenancy == sandboxhub.TenancyShared {
		if err := sandboxhub.AssertSafeAgentID(agentID); err != nil {
			return err
		}
		base, err := p.baseFor(cc)
		if err != nil {
			return err
		}
		// The agent folder must not be a case alias of another agent's folder, or this
		// would delete that agent's files (the same guard as Sandbox.ForAgent and
		// Sandbox.RemoveAgentData; the shared branch of baseFor has none).
		if err := checkAgentFolder(toHostPath(base, sandboxhub.AgentsDir), agentID, cc.ID, onWindows, caseInsensitiveHost); err != nil {
			return err
		}
		return removeTree(toHostPath(base, sandboxhub.AgentsDir+"/"+agentID))
	}
	base, err := p.baseFor(cc)
	if err != nil {
		return err
	}
	return removeTree(base)
}

// baseFor is the host folder that logical /workspace maps to for this sandbox.
// An own sandbox is <root>/<agentId> and needs cc.AgentID; the shared one is
// <root>/.shared. Agent ids never start with "." (AssertSafeAgentID), so the shared
// folder cannot collide with an own folder. An own agent id is also checked for the
// file system aliases AssertSafeAgentID allows (checkAgentFolder).
func (p *Provider) baseFor(cc sandboxhub.CreateContext) (string, error) {
	if cc.Tenancy == sandboxhub.TenancyOwn {
		if cc.AgentID == "" {
			return "", kernel.NewSandboxError("An own host sandbox needs an agent id", kernel.SandboxCodeInvalidArgument, cc.ID)
		}
		if err := sandboxhub.AssertSafeAgentID(cc.AgentID); err != nil {
			return "", err
		}
		// The id becomes a folder name that must be unique on the real file system.
		if err := checkAgentFolder(p.root, cc.AgentID, cc.ID, onWindows, caseInsensitiveHost); err != nil {
			return "", err
		}
		return filepath.Join(p.root, cc.AgentID), nil
	}
	return filepath.Join(p.root, ".shared"), nil
}

// removeTree deletes a folder tree like fs.rm({ recursive, force }): a missing path
// is not an error. The path comes from the provider (its root and a validated agent
// id), never from a profile. A link inside the tree is removed as a link: os.RemoveAll
// does not follow symlinks or Windows junctions, which the tests prove.
func removeTree(p string) error {
	return os.RemoveAll(p) //nolint:gosec // p is the provider's root joined with a validated agent id, never profile data
}
