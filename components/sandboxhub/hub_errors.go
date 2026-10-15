package sandboxhub

import "errors"

// The sentinel errors of the SandboxHub. The errors it returns satisfy errors.Is against
// these, while their Error() text is the hub's fixed message. Every other hub
// failure is a *kernel.SandboxError with its code and message; only these four
// plain errors are sentinels.
var (
	// ErrProviderAlreadyRegistered: a provider of that kind is already registered.
	ErrProviderAlreadyRegistered = errors.New("sandbox: provider already registered")
	// ErrSharedRunning: ConfigureShared after the shared sandbox exists or is being created.
	ErrSharedRunning = errors.New("sandbox: shared sandbox already running")
	// ErrOwnSelection: Resolve was given an own selection, which SandboxManager handles.
	ErrOwnSelection = errors.New("sandbox: own selection is managed by SandboxManager")
	// ErrNilProvider: Register was given a nil provider .
	ErrNilProvider = errors.New("sandbox: nil provider")
)

// hubError carries a fixed error text unchanged and unwraps to a sentinel. It is a
// type of its own, not fmt.Errorf or errors.New: the texts are exact, and the
// golden fixtures compare them (the same convention as the llm hub).
type hubError struct {
	msg      string
	sentinel error
}

func (e *hubError) Error() string { return e.msg }

func (e *hubError) Unwrap() error { return e.sentinel }
