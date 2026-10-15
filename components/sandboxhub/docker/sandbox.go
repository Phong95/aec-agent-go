package docker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"regexp"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/Phong95/aec-agent-go/components/sandboxhub"
	"github.com/Phong95/aec-agent-go/internal/jsonx"
	"github.com/Phong95/aec-agent-go/internal/jsstr"
	"github.com/Phong95/aec-agent-go/kernel"
)

// ---------------------------------------------------------------------------
// One Docker sandbox: a long-lived container on a named volume (A-60).
//
//	container  aec-sandbox-<suffix>   `sleep infinity`, every operation is a `docker exec`
//	volume     aec-sandbox-<suffix>   mounted at /workspace - files survive restarts
//
// Hardening: --network none (unless the host config says "full"), memory/cpu/pids
// limits, read-only root filesystem, tmpfs /tmp, all capabilities dropped (CHOWN
// kept for the root-only setup step), no-new-privileges, non-root user, no Docker
// socket, and no environment variables from the server.
// ---------------------------------------------------------------------------

// Compile-time proofs of the sandbox contracts.
var (
	_ sandboxhub.Sandbox          = (*Sandbox)(nil)
	_ sandboxhub.AgentDataRemover = (*Sandbox)(nil)
)

// Sandbox is one Docker sandbox. It is safe for concurrent use:
// its mutable state (disposed, the recovery in flight, the replace step in flight and
// the agent folders created in this container run) sits under one mutex.
//
// A container recovery runs in a goroutine of its own (recover), so that one caller
// giving up cannot fail the others. That goroutine, and the image preparation of
// Provider.EnsureImage, can therefore outlive the call that started them; they end
// when their docker calls end (each is bounded by its own timeout) and Dispose does
// not leave a container behind them (replaceContainer). Create does not return before
// the replace step of its own container has ended, even when its context is cancelled:
// that wait is bounded by two docker calls (see replaceContainer).
type Sandbox struct {
	p      *Provider
	id     string
	spec   sandboxhub.Spec
	cc     sandboxhub.CreateContext
	traits sandboxhub.Traits
	image  string

	containerName string
	volumeName    string
	resources     sandboxhub.ResourceLimits
	// fingerprint is the container settings hash: a different fingerprint means the
	// container is recreated (the volume is kept).
	fingerprint string

	mu         sync.Mutex
	disposed   bool
	recovering *recovery
	// step is non-nil while a replace step (docker rm then docker run) is in flight and
	// is closed when it ends; Dispose waits on it so the container the step creates is
	// removed, not orphaned (replaceContainer).
	step chan struct{}
	// agentDirs are the agent folders already created in this container run.
	agentDirs map[string]struct{}
}

// newSandbox builds a Sandbox and runs no docker command. A network allowlist is
// refused (policy_denied, before any docker call) and so is an image name that starts
// with "-". It maps ids with namesFor (pinned by the fingerprint goldens); the injectivity check of checkSandboxID is made by
// Provider.Create and Provider.RemoveData, the entry points.
func newSandbox(p *Provider, spec sandboxhub.Spec, cc sandboxhub.CreateContext) (*Sandbox, error) {
	network := spec.Network
	if network == "" {
		network = sandboxhub.NetworkNone
	}
	if network == sandboxhub.NetworkAllowlist {
		return nil, kernel.NewSandboxError(
			"Network allowlists are not supported by the Docker sandbox yet \U00002014 use \"none\"",
			kernel.SandboxCodePolicyDenied, cc.ID)
	}
	image := spec.Image
	if image == "" {
		image = p.settings.DefaultImage
	}
	if err := checkImageName(image, cc.ID); err != nil {
		return nil, err
	}
	s := &Sandbox{
		p:    p,
		id:   cc.ID,
		spec: spec,
		cc:   cc,
		traits: sandboxhub.Traits{
			Kind: sandboxhub.KindDocker, Isolated: true, Network: network, Persistent: true, Tenancy: cc.Tenancy, OS: sandboxhub.OSLinux,
		},
		image:     image,
		resources: overlayResources(p.settings.DefaultResources, spec.Limits),
		agentDirs: map[string]struct{}{},
	}
	s.containerName, s.volumeName = p.NamesFor(cc.ID)
	text, err := fingerprintText(fingerprintInput{
		V: 1, Image: s.image, Network: string(network),
		Resources: fingerprintResources{Cpus: s.resources.Cpus, MemoryMb: s.resources.MemoryMb, Pids: s.resources.Pids},
		Tmpfs:     p.settings.TmpfsSize, User: p.settings.User,
	})
	if err != nil {
		return nil, kernel.NewSandboxError(
			fmt.Sprintf("Cannot compute the container settings of \"%s\": %s", cc.ID, err.Error()),
			kernel.SandboxCodeInvalidArgument, cc.ID)
	}
	s.fingerprint = fingerprintOf(text)
	return s, nil
}

// overlayResources is { ...defaults, ...definedOnly(limits) }: only a finite number
// above zero replaces a default.
func overlayResources(defaults sandboxhub.ResourceLimits, limits *sandboxhub.LimitsOverride) sandboxhub.ResourceLimits {
	r := defaults
	if limits == nil {
		return r
	}
	if limits.Cpus != nil && *limits.Cpus > 0 && !math.IsInf(*limits.Cpus, 0) {
		r.Cpus = *limits.Cpus
	}
	if limits.MemoryMb != nil && *limits.MemoryMb > 0 {
		r.MemoryMb = *limits.MemoryMb
	}
	if limits.Pids != nil && *limits.Pids > 0 {
		r.Pids = *limits.Pids
	}
	return r
}

// ID returns the sandbox id the hub assigned.
func (s *Sandbox) ID() string { return s.id }

// Traits returns the sandbox traits.
func (s *Sandbox) Traits() sandboxhub.Traits { return s.traits }

// Image returns the container image.
func (s *Sandbox) Image() string { return s.image }

// ContainerName returns the name of the container.
func (s *Sandbox) ContainerName() string { return s.containerName }

// VolumeName returns the name of the volume that holds /workspace.
func (s *Sandbox) VolumeName() string { return s.volumeName }

// runArgs is the docker run argument list of the container, in a fixed order
// (recorded by wiregen in docker-fingerprint.json).
func (s *Sandbox) runArgs() ([]string, error) {
	settings := s.p.settings
	r := s.resources
	cpus, err := jsNumberText(r.Cpus)
	if err != nil {
		return nil, kernel.NewSandboxError(
			fmt.Sprintf("Invalid cpus value %v for \"%s\"", r.Cpus, s.id), kernel.SandboxCodeInvalidArgument, s.id)
	}
	network := "none"
	if s.traits.Network == sandboxhub.NetworkFull {
		network = "bridge"
	}
	memory := strconv.Itoa(r.MemoryMb) + "m"
	return []string{
		"run", "--detach",
		"--name", s.containerName,
		"--hostname", "sandbox",
		"--label", "aec.sandbox=1",
		"--label", "aec.sandbox.id=" + s.id,
		"--label", "aec.sandbox.config=" + s.fingerprint,
		"--network", network,
		"--memory", memory,
		"--memory-swap", memory,
		"--cpus", cpus,
		"--pids-limit", strconv.Itoa(r.Pids),
		"--read-only",
		"--tmpfs", "/tmp:rw,nosuid,nodev,size=" + settings.TmpfsSize,
		"--cap-drop", "ALL",
		"--cap-add", "CHOWN",
		"--security-opt", "no-new-privileges",
		"--init",
		"--user", settings.User,
		"--env", "HOME=/tmp/home",
		"--mount", "type=volume,source=" + s.volumeName + ",target=" + sandboxhub.WorkspaceRoot,
		"--workdir", sandboxhub.WorkspaceRoot,
		"--entrypoint", "sleep",
		s.image,
		"infinity",
	}, nil
}

// dockerCallTimeout is the timeout of the docker calls that manage the container.
const dockerCallTimeout = 120 * time.Second

// containerDown is docker's message for a container that is gone or stopped
// (recoverable by restarting it).
var containerDown = regexp.MustCompile(`(?i)No such container|is not running|is paused|is restarting`)

// recovery is the one container recovery that callers share.
type recovery struct {
	done chan struct{}
	err  error
}

// -- Lifecycle --

// start is Docker up, image present, container running and set up.
func (s *Sandbox) start(ctx context.Context) error {
	if err := s.p.checkDocker(ctx, s.id); err != nil {
		return err
	}
	if err := s.p.EnsureImage(ctx, s.image, s.id); err != nil {
		return err
	}
	return s.ensureContainer(ctx)
}

// Health is unavailable once disposed, starting while the container is being
// recovered, and otherwise ready when the container is running.
func (s *Sandbox) Health(ctx context.Context) sandboxhub.Health {
	s.mu.Lock()
	disposed, recovering := s.disposed, s.recovering != nil
	s.mu.Unlock()
	if disposed {
		return sandboxhub.HealthUnavailable
	}
	if recovering {
		return sandboxhub.HealthStarting
	}
	state, err := s.inspect(ctx)
	if err != nil || state == nil || !state.running {
		return sandboxhub.HealthUnavailable
	}
	return sandboxhub.HealthReady
}

// Dispose stops and removes the container. The volume (the files) stays unless
// removeData. docker's own exit codes are ignored: only a docker client that
// cannot run is an error.
//
// A replace step that is already running (docker rm then docker run, started by a
// recovery that outlived the caller's context) is allowed to end first, bounded by ctx,
// so the container it creates is removed here and not left running with no owner. A
// step that has not begun yet sees disposed and starts nothing (replaceContainer).
func (s *Sandbox) Dispose(ctx context.Context, removeData bool) error {
	s.mu.Lock()
	s.disposed = true
	step := s.step
	s.mu.Unlock()
	if step != nil {
		select {
		case <-step:
		case <-ctx.Done():
		}
	}
	if _, err := s.p.runner.Run(ctx, []string{"rm", "--force", s.containerName}, RunOptions{Timeout: removeTimeout}); err != nil {
		return err
	}
	if removeData {
		if _, err := s.p.runner.Run(ctx, []string{"volume", "rm", "--force", s.volumeName}, RunOptions{Timeout: removeTimeout}); err != nil {
			return err
		}
	}
	return nil
}

// isDisposed reports whether Dispose was called.
func (s *Sandbox) isDisposed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.disposed
}

// assertUsable fails with environment_unavailable once the sandbox was disposed.
func (s *Sandbox) assertUsable() error {
	if s.isDisposed() {
		return s.disposedError()
	}
	return nil
}

// disposedError is the "was disposed" error of a sandbox that Dispose has closed.
func (s *Sandbox) disposedError() error {
	return kernel.NewSandboxError(fmt.Sprintf("Sandbox \"%s\" was disposed", s.id), kernel.SandboxCodeEnvironmentUnavailable, s.id)
}

// helper runs one helper file operation: one docker exec of node on the embedded
// helper, the request as one JSON line on stdin. The request is an ordered struct
// whose last field is the op. The container is restarted once if it went down.
func (s *Sandbox) helper(ctx context.Context, request any) (json.RawMessage, error) {
	return s.helperCall(ctx, request, true)
}

// helperCall is helper, with recovery optional: setup must not wait for a recovery
// of which it is part (it would wait for itself forever; the helper has just been
// installed there, so a container that is down is a setup failure).
func (s *Sandbox) helperCall(ctx context.Context, request any, mayRecover bool) (json.RawMessage, error) {
	if err := s.assertUsable(); err != nil {
		return nil, err
	}
	args := []string{"exec", "--interactive", "--user", s.p.settings.User, s.containerName, "node", helperPath}
	input, err := jsonx.Marshal(request)
	if err != nil {
		return nil, kernel.NewSandboxError(
			fmt.Sprintf("Cannot encode the sandbox request: %s", err.Error()), kernel.SandboxCodeInvalidArgument, s.id)
	}
	run := func() (RunResult, error) {
		return s.p.runner.Run(ctx, args, RunOptions{Input: string(input), Timeout: s.p.settings.OperationTimeout, SandboxID: s.id})
	}
	result, err := run()
	if err != nil {
		return nil, err
	}
	if !exitedWith(result, 0) && mayRecover && containerDown.MatchString(result.Stderr) {
		if err := s.recover(ctx); err != nil {
			return nil, err
		}
		if result, err = run(); err != nil {
			return nil, err
		}
	}
	return parseHelperResponse(result, s.id)
}

// recover brings a stopped or removed container back (the files are on the volume).
// Callers that arrive while a recovery runs share its outcome.
//
// The recovery itself runs detached from every caller's context (a hub-owned
// goroutine on context.WithoutCancel), as the shared recovery cannot be aborted:
// one caller giving up (an aborted agent turn) must not fail the callers that still
// wait, nor stop the recovery between "rm --force" and "run", which would leave no
// container. Every caller, the one that started it included, waits under its OWN
// context and gets "Operation cancelled" when that ends; the docker calls inside the
// recovery are still bounded by their own timeouts.
//
// Dispose does not cancel a recovery in flight, but a recovery never undoes Dispose:
// ensureContainer fails with the disposed error before any docker start or run, and a
// container that a racing run created is removed (replaceContainer). A panic in the
// runner ends the recovery with an error that every waiter receives, as
// sandboxhub.SandboxHub does for a panicking provider; the bookkeeping always runs.
func (s *Sandbox) recover(ctx context.Context) error {
	if err := s.assertUsable(); err != nil {
		return err
	}
	s.mu.Lock()
	r := s.recovering
	if r == nil {
		if ctx.Err() != nil {
			s.mu.Unlock()
			return kernel.NewSandboxError("Operation cancelled", kernel.SandboxCodeCancelled, s.id)
		}
		r = &recovery{done: make(chan struct{})}
		s.recovering = r
		workCtx := context.WithoutCancel(ctx)
		go func() {
			defer func() {
				if p := recover(); p != nil {
					r.err = panicError(s.id, p)
				}
				s.mu.Lock()
				s.recovering = nil
				s.mu.Unlock()
				close(r.done)
			}()
			r.err = s.ensureContainer(workCtx)
		}()
	}
	s.mu.Unlock()
	select {
	case <-r.done:
		return r.err
	case <-ctx.Done():
		return kernel.NewSandboxError("Operation cancelled", kernel.SandboxCodeCancelled, s.id)
	}
}

// -- Container management --

// containerState is what inspect learned about the container.
type containerState struct {
	// config is the aec.sandbox.config label, "" when there is none.
	config  string
	running bool
}

// inspect reads the labels and the running flag of the container; nil means there is
// no such container. The line is "<labels json>|<running>", cut at the LAST "|".
func (s *Sandbox) inspect(ctx context.Context) (*containerState, error) {
	r, err := s.p.runner.Run(ctx,
		[]string{"container", "inspect", "--format", "{{json .Config.Labels}}|{{.State.Running}}", s.containerName},
		RunOptions{Timeout: inspectTimeout, SandboxID: s.id})
	if err != nil {
		return nil, err
	}
	if !exitedWith(r, 0) {
		return nil, nil //nolint:nilnil // no such container is a normal answer, not an error
	}
	line := jsstr.Trim(r.Stdout)
	cut := strings.LastIndex(line, "|")
	labelsText := line
	if cut >= 0 {
		labelsText = line[:cut]
	} else if len(labelsText) > 0 {
		// JS line.slice(0, -1): everything but the last character.
		_, size := utf8.DecodeLastRuneInString(labelsText)
		labelsText = labelsText[:len(labelsText)-size]
	}
	// Unreadable labels (or anything that is not an object) are treated as a foreign
	// container and recreated.
	var config string
	var labels any
	if json.Unmarshal([]byte(labelsText), &labels) == nil {
		if m, ok := labels.(map[string]any); ok {
			config, _ = m["aec.sandbox.config"].(string)
		}
	}
	return &containerState{config: config, running: line[cut+1:] == "true"}, nil
}

// ensureContainer makes the container running with the right settings: reuse one
// whose config label equals the fingerprint (start it when stopped), else replace it.
// The helper is always (re)installed afterwards and the agent folder cache cleared.
//
// A disposed sandbox is never brought back: the check is made before the first docker
// call, before docker start and, under the sandbox lock, before docker rm and docker
// run (replaceContainer), so a recovery that outlived Dispose creates no container and
// no volume. The error is the "was disposed" error.
func (s *Sandbox) ensureContainer(ctx context.Context) error {
	if err := s.assertUsable(); err != nil {
		return err
	}
	state, err := s.inspect(ctx)
	if err != nil {
		return err
	}
	if state != nil && state.config == s.fingerprint {
		if !state.running {
			if err := s.assertUsable(); err != nil {
				return err
			}
			if err := s.docker(ctx, []string{"start", s.containerName}, "start the sandbox container", ""); err != nil {
				return err
			}
		}
	} else if err := s.replaceContainer(ctx, state != nil); err != nil {
		return err
	}
	s.mu.Lock()
	clear(s.agentDirs)
	s.mu.Unlock()
	return s.setup(ctx)
}

// replaceContainer removes the old container (when there is one) and creates the new
// one. Removing and creating is one step as far as cancellation goes: a context that
// ends between the two would leave the sandbox with no container. The context is
// checked before the step, and both calls then run detached (each is still bounded by
// its own docker timeout, so a cancelled Create keeps waiting here for at most two
// docker calls: 2 x 120 s plus the runner's 5 s wait delay each).
//
// The step is also the one place that must not run after Dispose: docker run creates
// the container and, through its --mount, the volume that Dispose(removeData) just
// deleted. beginStep checks disposed under the sandbox lock and registers the step, so
// Dispose either sees the step and waits for it (and then removes what it created) or
// sets disposed first and the step starts nothing. Should Dispose have run while docker
// run was in flight anyway (its context ended before the wait did), the container that
// was just created is removed here, and only that container: by the id that docker run
// printed, never by name. A name is not an identity (the hub always uses the id
// docker:shared, so a successor sandbox that Create built after this Dispose reuses the
// container with the same name and fingerprint, and removing by name would destroy it).
// When docker run reported no usable id the container is left in place and the error
// says so; a failing cleanup is reported as well, not swallowed.
func (s *Sandbox) replaceContainer(ctx context.Context, exists bool) error {
	if ctx.Err() != nil {
		return kernel.NewSandboxError("Operation cancelled", kernel.SandboxCodeCancelled, s.id)
	}
	end, err := s.beginStep()
	if err != nil {
		return err
	}
	defer end()
	stepCtx := context.WithoutCancel(ctx)
	if exists {
		if err := s.docker(stepCtx, []string{"rm", "--force", s.containerName}, "replace the sandbox container", ""); err != nil {
			return err
		}
		// A Dispose that arrived while the old container was being removed: do not
		// create a container only to remove it again.
		if err := s.assertUsable(); err != nil {
			return err
		}
	}
	args, err := s.runArgs()
	if err != nil {
		return err
	}
	created, err := s.dockerOutput(stepCtx, args, "create the sandbox container", "")
	if err != nil {
		return err
	}
	if s.isDisposed() {
		return s.removeCreated(stepCtx, created)
	}
	return nil
}

// containerIDPattern is what docker run --detach prints: the id of the new container,
// 64 hex digits (an older or compatible client may print the 12-digit short form).
var containerIDPattern = regexp.MustCompile(`^[0-9a-f]{12,64}$`)

// removeCreated removes the container that docker run just created, addressed by the id
// it printed (stdout). The volume is not touched, Dispose decides about the data. It
// always returns an error: the "was disposed" error of the sandbox, extended when the
// container could not be removed or could not be identified, so that a container nobody
// owns is recorded and not silently left running.
func (s *Sandbox) removeCreated(ctx context.Context, stdout string) error {
	id := jsstr.Trim(stdout)
	if !containerIDPattern.MatchString(id) {
		return kernel.NewSandboxError(
			fmt.Sprintf("Sandbox \"%s\" was disposed; the container created by the replace step was left in place (%s) because docker run printed no container id: %s",
				s.id, s.containerName, jsstr.TruncateUTF16(strconv.Quote(id), 100)),
			kernel.SandboxCodeEnvironmentUnavailable, s.id)
	}
	r, err := s.p.runner.Run(ctx, []string{"rm", "--force", id}, RunOptions{Timeout: removeTimeout, SandboxID: s.id})
	var detail string
	switch {
	case err != nil:
		detail = err.Error()
	case !exitedWith(r, 0):
		detail = firstLines(r.Stderr)
		if detail == "" {
			detail = "docker exited with " + codeText(r.Code)
		}
	default:
		return s.disposedError()
	}
	return kernel.NewSandboxError(
		fmt.Sprintf("Sandbox \"%s\" was disposed, but the container created by the replace step (%s) could not be removed: %s", s.id, id, detail),
		kernel.SandboxCodeEnvironmentUnavailable, s.id)
}

// beginStep registers a replace step unless the sandbox was disposed, under the lock
// that Dispose takes to set disposed. Only one step runs at a time: ensureContainer is
// reached through the single-flight recovery or through Create, before the sandbox is
// published. The returned function ends the step and releases Dispose.
func (s *Sandbox) beginStep() (end func(), err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.disposed {
		return nil, s.disposedError()
	}
	done := make(chan struct{})
	s.step = done
	return func() {
		s.mu.Lock()
		s.step = nil
		close(done)
		s.mu.Unlock()
	}, nil
}

// panicError is the error that waiters of a shared goroutine receive when the runner
// panicked in it (a test double or a custom runner): the panic must not kill the
// process, and the goroutine's bookkeeping must still run. The goroutine stack at the
// panic is kept in the message (it is the only record of where a programming error in a
// custom runner came from); it must be called from the deferred recover.
func panicError(sandboxID string, p any) error {
	return kernel.NewSandboxError(fmt.Sprintf("docker: panic in shared work: %v\n%s", p, debug.Stack()), kernel.SandboxCodeEnvironmentUnavailable, sandboxID)
}

// mkdirRequest is the helper request that creates folders; the op is last.
type mkdirRequest struct {
	Paths []string `json:"paths"`
	Roots []string `json:"roots"`
	Op    string   `json:"op"`
}

// setupScript installs the helper (root-owned, so the sandbox user cannot replace it),
// gives the sandbox user /workspace and a HOME. The lines are joined with "\n".
var setupScript = strings.Join([]string{
	"set -e",
	"mkdir -p /tmp/.aec /tmp/home",
	"cat > /tmp/.aec/helper.js",
	"chown 0:0 /tmp/.aec /tmp/.aec/helper.js",
	"chmod 755 /tmp/.aec",
	"chmod 644 /tmp/.aec/helper.js",
	`chown "$1" /tmp/home /workspace`,
}, "\n")

// setup installs the helper, then creates the layout folders as the sandbox user.
func (s *Sandbox) setup(ctx context.Context) error {
	if err := s.docker(ctx,
		[]string{"exec", "--interactive", "--user", "0", s.containerName, "sh", "-c", setupScript, "sh", s.p.settings.User},
		"set up the sandbox container", helperSource); err != nil {
		return err
	}
	// Own tenancy uses /workspace directly; the shared layout folders only exist in
	// shared sandboxes. (There is no /workspace/shared; the agents folder is enough.)
	paths := []string{sandboxhub.WorkspaceRoot}
	if s.traits.Tenancy == sandboxhub.TenancyShared {
		paths = []string{sandboxhub.AgentsDir}
	}
	if _, err := s.helperCall(ctx, mkdirRequest{Paths: paths, Roots: []string{sandboxhub.WorkspaceRoot}, Op: "mkdir"}, false); err != nil {
		var se *kernel.SandboxError
		if errors.As(err, &se) && se.Code == kernel.SandboxCodeEnvironmentUnavailable {
			return kernel.NewSandboxError(
				fmt.Sprintf("Image \"%s\" cannot run the sandbox helper \U00002014 sandbox images must include node (>= 18). %s", s.image, se.Message),
				kernel.SandboxCodeEnvironmentUnavailable, s.id)
		}
		return err
	}
	return nil
}

// docker runs one docker command of the container lifecycle; a failing exit code is
// environment_unavailable "Could not <what>: <first lines of stderr>".
func (s *Sandbox) docker(ctx context.Context, args []string, what, input string) error {
	_, err := s.dockerOutput(ctx, args, what, input)
	return err
}

// dockerOutput is docker that also returns stdout (docker run prints the new container's
// id there).
func (s *Sandbox) dockerOutput(ctx context.Context, args []string, what, input string) (string, error) {
	r, err := s.p.runner.Run(ctx, args, RunOptions{Input: input, Timeout: dockerCallTimeout, SandboxID: s.id})
	if err != nil {
		return "", err
	}
	if !exitedWith(r, 0) {
		detail := firstLines(r.Stderr)
		if detail == "" {
			detail = "docker exited with " + codeText(r.Code)
		}
		return "", kernel.NewSandboxError(fmt.Sprintf("Could not %s: %s", what, detail), kernel.SandboxCodeEnvironmentUnavailable, s.id)
	}
	return r.Stdout, nil
}

// -- Helper answers --

// parseHelperResponse reads the last line of stdout as the helper's answer,
// {"ok":true,"result":...} or {"ok":false,"code":...,"message":...}. No JSON line means
// docker or node failed before the helper could answer. The result is returned
// verbatim and decoded by the caller, leniently: the helper is a separate program that
// may gain fields.
func parseHelperResponse(r RunResult, sandboxID string) (json.RawMessage, error) {
	lines := strings.Split(jsstr.Trim(r.Stdout), "\n")
	line := lines[len(lines)-1]
	var response map[string]json.RawMessage
	if json.Unmarshal([]byte(line), &response) != nil || response == nil || response["ok"] == nil {
		detail := firstLines(r.Stderr)
		if detail == "" {
			detail = "docker exited with " + codeText(r.Code)
		}
		return nil, kernel.NewSandboxError("The sandbox did not answer: "+detail, kernel.SandboxCodeEnvironmentUnavailable, sandboxID)
	}
	if !truthy(response["ok"]) {
		var code, message string
		_ = json.Unmarshal(response["code"], &code)
		_ = json.Unmarshal(response["message"], &message)
		return nil, kernel.NewSandboxError(message, kernel.SandboxErrorCode(code), sandboxID)
	}
	return response["result"], nil
}

// truthy is the JavaScript truthiness of a JSON value (used on the helper's ok field).
func truthy(raw json.RawMessage) bool {
	var v any
	if json.Unmarshal(raw, &v) != nil {
		return false
	}
	switch x := v.(type) {
	case nil:
		return false
	case bool:
		return x
	case float64:
		return x != 0
	case string:
		return x != ""
	default:
		return true
	}
}

// firstLines is the first three lines of text on one line, at most 500 characters.
func firstLines(text string) string {
	lines := strings.Split(jsstr.Trim(text), "\n")
	if len(lines) > 3 {
		lines = lines[:3]
	}
	return jsstr.TruncateUTF16(strings.Join(lines, " "), 500)
}

// removeRequest is the helper request that deletes a path; the op is last.
type removeRequest struct {
	Path  string   `json:"path"`
	Roots []string `json:"roots"`
	Op    string   `json:"op"`
}

// ForAgent returns the view one agent works through: the working directory and roots
// of sandboxhub.AgentLayout. In a shared sandbox the agent's folder is created once per
// container run (the helper creates it as the sandbox user and may not leave the
// agents folder). A disposed sandbox fails with environment_unavailable.
func (s *Sandbox) ForAgent(ctx context.Context, agentID string) (sandboxhub.View, error) {
	if err := s.assertUsable(); err != nil {
		return nil, err
	}
	layout, err := sandboxhub.AgentLayout(s.traits.Tenancy, agentID)
	if err != nil {
		return nil, err
	}
	if s.traits.Tenancy == sandboxhub.TenancyShared && !s.hasAgentDir(agentID) {
		req := mkdirRequest{Paths: []string{layout.WorkingDir}, Roots: []string{sandboxhub.AgentsDir}, Op: "mkdir"}
		if _, err := s.helper(ctx, req); err != nil {
			return nil, err
		}
		s.markAgentDir(agentID)
	}
	return &view{sb: s, workingDir: layout.WorkingDir, roots: layout.Roots}, nil
}

// RemoveAgentData deletes one agent's folder in a shared sandbox (the agent was
// deleted) and forgets that the folder was created.
func (s *Sandbox) RemoveAgentData(ctx context.Context, agentID string) error {
	if err := sandboxhub.AssertSafeAgentID(agentID); err != nil {
		return err
	}
	if err := s.assertUsable(); err != nil {
		return err
	}
	req := removeRequest{Path: sandboxhub.AgentsDir + "/" + agentID, Roots: []string{sandboxhub.AgentsDir}, Op: "remove"}
	if _, err := s.helper(ctx, req); err != nil {
		return err
	}
	s.mu.Lock()
	delete(s.agentDirs, agentID)
	s.mu.Unlock()
	return nil
}

func (s *Sandbox) hasAgentDir(agentID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.agentDirs[agentID]
	return ok
}

func (s *Sandbox) markAgentDir(agentID string) {
	s.mu.Lock()
	s.agentDirs[agentID] = struct{}{}
	s.mu.Unlock()
}
