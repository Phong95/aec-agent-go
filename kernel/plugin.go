package kernel

import (
	"context"

	"github.com/Phong95/aec-agent-go/internal/nilcheck"
)

// Plugin lifecycle - every plugin implements this.
//
// A plugin has a name and a version, and may have an initialize hook and a
// dispose hook. Go has no optional methods, so the contract is split: Plugin
// carries the two required members, and Initializer and Disposer are the two
// optional hooks. A plugin opts in to a hook by implementing the matching
// interface.
//
// There is no plugin registry here: AgentCore calls the hooks itself (through
// Initialize and Dispose below). Real
// registration (the Use* methods) arrives with the packages that own each
// contract and with AgentCore in Phase 10.
// Update: registration lives on AgentCore (UsePersistence, UseLLMProvider, UseTool,
// UseSkill, UseSandboxProvider, UseJobRunner, UseSubagentType, UseSecretHubProvider,
// UseSubagentRunner, UseChannelPlatform, UseChannelTurnRunner); there is no Kernel
// interface in Go - AgentCore is the API.

// Plugin is the required identity of every plugin.
type Plugin interface {
	Name() string
	Version() string
}

// Initializer is the optional initialize hook of a plugin.
type Initializer interface {
	Initialize(ctx context.Context) error
}

// Disposer is the optional dispose hook of a plugin.
type Disposer interface {
	Dispose(ctx context.Context) error
}

// Initialize calls p's Initialize hook if, and only if, p implements
// Initializer: a plugin without the hook is a no-op that returns nil. It takes any (not Plugin) so that a value
// implementing only one hook works. A nil p, including a typed nil pointer, is a
// no-op.
func Initialize(ctx context.Context, p any) error {
	if nilcheck.IsNil(p) {
		return nil
	}
	if h, ok := p.(Initializer); ok {
		return h.Initialize(ctx)
	}
	return nil
}

// Dispose calls p's Dispose hook if, and only if, p implements Disposer. It
// treats a plugin without the hook and a nil p like Initialize does.
func Dispose(ctx context.Context, p any) error {
	if nilcheck.IsNil(p) {
		return nil
	}
	if h, ok := p.(Disposer); ok {
		return h.Dispose(ctx)
	}
	return nil
}
