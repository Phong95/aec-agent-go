package docker

import (
	"context"
	"io"
	"time"

	"github.com/Phong95/aec-agent-go/components/sandboxhub"
)

// Options configures a Provider. Host code only, never from agent profiles. A zero
// value of a string, duration or limit means "use the default".
type Options struct {
	// DockerPath is the docker CLI binary. Default "docker".
	DockerPath string
	// NamePrefix prefixes container and volume names. Default "aec-sandbox"
	// ("aec-sandbox-shared", "aec-sandbox-own-<agentId>").
	NamePrefix string
	// DefaultImage is the image when a spec names none. Default SandboxImage. Every
	// image must provide node (>= 18).
	DefaultImage string
	// User is the uid or uid:gid every command and file operation runs as. Default
	// "1000:1000" (node in the default image).
	User string
	// DefaultResources are the container resources when a spec sets none. Default
	// 2 cpus, 2048 MB, 256 processes.
	DefaultResources sandboxhub.LimitsOverride
	// TmpfsSize is the size of the tmpfs at /tmp (wiped when the container
	// restarts). Default "256m".
	TmpfsSize string
	// Limits overlays DefaultLimits (per-operation ceilings).
	Limits sandboxhub.LimitsPatch
	// BuildDefaultImage builds SandboxImage from the bundled Dockerfile when it is
	// missing. Default true (nil).
	BuildDefaultImage *bool
	// PullImages pulls other missing images. Default true (nil).
	PullImages *bool
	// OperationTimeout is the ceiling for one file operation (read, write, list,
	// search). Default 60 s.
	OperationTimeout time.Duration
	// Runner starts the docker CLI. Nil means the docker binary named by DockerPath
	// over os/exec; tests pass a fake.
	Runner Runner
}

// Settings are the resolved provider settings.
type Settings struct {
	DefaultImage      string
	User              string
	DefaultResources  sandboxhub.ResourceLimits
	TmpfsSize         string
	Limits            sandboxhub.Limits
	BuildDefaultImage bool
	PullImages        bool
	OperationTimeout  time.Duration
	NamePrefix        string
}

// Availability is the outcome of CheckAvailable: OK with the server Version, or not
// OK with the Reason.
type Availability struct {
	OK      bool
	Version string
	Reason  string
}

// RunOptions are the options of Runner.Run (cancellation is the ctx parameter).
type RunOptions struct {
	// Input is written to stdin, then stdin is closed.
	Input string
	// Timeout kills the call after this long (code timeout). Zero means no timeout.
	Timeout time.Duration
	// SandboxID is the sandbox id carried by the errors of the call.
	SandboxID string
}

// RunResult is the outcome of a docker call that ran to completion.
type RunResult struct {
	// Code is the exit code; nil when the client was killed by a signal.
	Code   *int
	Stdout string
	Stderr string
}

// Process is a running docker client with piped stdio, used by the exec stream. Read
// Stdout and Stderr to the end before Wait.
type Process interface {
	Stdin() io.WriteCloser
	Stdout() io.Reader
	Stderr() io.Reader
	// Wait waits for the client to exit. The code is nil when it was killed by a
	// signal; the error is only for a failure of the wait itself.
	Wait() (code *int, err error)
	// Kill kills the docker client (not the command inside the container: the
	// helper kills that when its stdin closes).
	Kill() error
}

// Runner starts docker CLI processes. It is the seam the tests replace with a fake;
// the default runs the docker binary over os/exec. The CLI gets the server's
// environment (it may need DOCKER_HOST, PATH); containers never do - docker exec only
// passes what -e names, and the provider passes nothing from the server.
type Runner interface {
	// Run runs docker to completion. It returns an error only when docker cannot run
	// at all, times out or is cancelled; a failing exit code is a result, not an
	// error.
	Run(ctx context.Context, args []string, opts RunOptions) (RunResult, error)
	// Start starts docker with piped stdio.
	Start(ctx context.Context, args []string) (Process, error)
}
