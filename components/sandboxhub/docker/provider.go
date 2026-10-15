package docker

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Phong95/aec-agent-go/components/sandboxhub"
	"github.com/Phong95/aec-agent-go/internal/jsstr"
	"github.com/Phong95/aec-agent-go/kernel"
)

// Compile-time proofs of the sandbox contracts.
var (
	_ sandboxhub.Provider            = (*Provider)(nil)
	_ sandboxhub.ProviderDataRemover = (*Provider)(nil)
)

// DefaultLimits returns the per-operation ceilings of a Docker sandbox, a fresh value
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

// Defaults of the provider settings.
const (
	defaultUser             = "1000:1000"
	defaultTmpfsSize        = "256m"
	defaultNamePrefix       = "aec-sandbox"
	defaultOperationTimeout = 60 * time.Second
)

var (
	// userPattern is the /^\d+(:\d+)?$/ check of the container user. RE2 "$" without
	// (?m) matches at the end of the text only.
	userPattern = regexp.MustCompile(`^\d+(:\d+)?$`)
	// namePrefixPattern is the /^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,63}$/ check.
	namePrefixPattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,63}$`)
)

// Provider creates Docker sandboxes: kind docker, plugin
// name sandbox:docker, version 1.0.0.
type Provider struct {
	settings Settings
	runner   Runner

	// images holds one preparation per image: concurrent callers
	// share it, a failure is removed so the next call retries.
	mu     sync.Mutex
	images map[string]*imageFuture
}

// New returns a provider with the defaults for every option left zero. The user must
// be a numeric "uid" or "uid:gid" and the name prefix must match namePrefixPattern,
// otherwise New fails. A default image that starts with "-" is refused too (docker
// would read it as a flag).
func New(opts Options) (*Provider, error) {
	s := Settings{
		DefaultImage:      firstNonEmpty(opts.DefaultImage, SandboxImage),
		User:              firstNonEmpty(opts.User, defaultUser),
		DefaultResources:  resolveResources(opts.DefaultResources),
		TmpfsSize:         firstNonEmpty(opts.TmpfsSize, defaultTmpfsSize),
		Limits:            DefaultLimits().Apply(opts.Limits),
		BuildDefaultImage: boolOrTrue(opts.BuildDefaultImage),
		PullImages:        boolOrTrue(opts.PullImages),
		OperationTimeout:  opts.OperationTimeout,
		NamePrefix:        firstNonEmpty(opts.NamePrefix, defaultNamePrefix),
	}
	if s.OperationTimeout == 0 {
		s.OperationTimeout = defaultOperationTimeout
	}
	if !userPattern.MatchString(s.User) {
		return nil, fmt.Errorf("DockerSandboxProvider: user must be numeric \"uid\" or \"uid:gid\", got \"%s\"", s.User)
	}
	if !namePrefixPattern.MatchString(s.NamePrefix) {
		return nil, fmt.Errorf("DockerSandboxProvider: invalid namePrefix \"%s\"", s.NamePrefix)
	}
	if strings.HasPrefix(s.DefaultImage, "-") {
		return nil, fmt.Errorf("DockerSandboxProvider: default image \"%s\" must not start with \"-\"", s.DefaultImage)
	}
	runner := opts.Runner
	if runner == nil {
		runner = cliRunner{binary: firstNonEmpty(opts.DockerPath, "docker")}
	}
	return &Provider{settings: s, runner: runner, images: map[string]*imageFuture{}}, nil
}

// resolveResources is { ...DEFAULT_RESOURCES, ...options.defaultResources }: every
// field that is set replaces the default, even a zero.
func resolveResources(o sandboxhub.LimitsOverride) sandboxhub.ResourceLimits {
	r := sandboxhub.ResourceLimits{Cpus: 2, MemoryMb: 2048, Pids: 256}
	if o.Cpus != nil {
		r.Cpus = *o.Cpus
	}
	if o.MemoryMb != nil {
		r.MemoryMb = *o.MemoryMb
	}
	if o.Pids != nil {
		r.Pids = *o.Pids
	}
	return r
}

func firstNonEmpty(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}

func boolOrTrue(b *bool) bool {
	return b == nil || *b
}

// Name returns the plugin name, sandbox:docker.
func (*Provider) Name() string { return "sandbox:docker" }

// Version returns the plugin version, 1.0.0.
func (*Provider) Version() string { return "1.0.0" }

// Kind returns docker.
func (*Provider) Kind() sandboxhub.Kind { return sandboxhub.KindDocker }

// Settings returns the resolved settings.
func (p *Provider) Settings() Settings { return p.settings }

// NamesFor returns the container and volume names for a sandbox id
// ("docker:shared" gives "aec-sandbox-shared"); both are the same string.
func (p *Provider) NamesFor(sandboxID string) (container, volume string) {
	return namesFor(p.settings.NamePrefix, sandboxID)
}

// checkImageName refuses an image name that starts with "-": the image is a
// positional docker argument, so docker would read such a name as a flag.
func checkImageName(image, sandboxID string) error {
	if strings.HasPrefix(image, "-") {
		return kernel.NewSandboxError(
			fmt.Sprintf("Image \"%s\" cannot be used: an image name must not start with \"-\"", image),
			kernel.SandboxCodePolicyDenied, sandboxID)
	}
	return nil
}

// Timeouts of the provider's docker calls (image build and pull, and the calls of
// RemoveData and CheckAvailable).
const (
	imageBuildTimeout  = 30 * time.Minute
	imagePullTimeout   = 20 * time.Minute
	inspectTimeout     = 30 * time.Second
	removeTimeout      = 60 * time.Second
	removeFilesTimeout = 120 * time.Second
	versionTimeout     = 20 * time.Second
)

// Create creates or reconnects to the sandbox of a spec: Docker must be
// up, the image present, and the container running with the helper installed. A
// spec of another kind is provider_unavailable, a network allowlist is policy_denied
// and so is a sandbox or agent id that shares its container and volume name with
// another id (checkSandboxID), all before any docker call.
//
// Create waits for the replace step of its container (docker rm then docker run) even
// when ctx is cancelled meanwhile, because splitting that step would leave no container;
// it then returns "Operation cancelled". That wait is bounded by two docker calls (each
// 120 s plus the runner's 5 s wait delay), not by ctx.
func (p *Provider) Create(ctx context.Context, spec sandboxhub.Spec, cc sandboxhub.CreateContext) (sandboxhub.Sandbox, error) {
	if spec.Kind != sandboxhub.KindDocker {
		return nil, kernel.NewSandboxError(
			fmt.Sprintf("DockerSandboxProvider cannot create \"%s\" sandboxes", spec.Kind), kernel.SandboxCodeProviderUnavailable, cc.ID)
	}
	if err := checkSandboxID(cc); err != nil {
		return nil, err
	}
	sb, err := newSandbox(p, spec, cc)
	if err != nil {
		return nil, err
	}
	if err := sb.start(ctx); err != nil {
		return nil, err
	}
	return sb, nil
}

// RemoveData deletes the stored data of a sandbox that is not running in this
// process: its whole volume (own tenancy, or no agent id) or, with an
// agent id and shared tenancy, one agent's folder in it. A volume or image that does
// not exist means nothing is stored and is not an error.
func (p *Provider) RemoveData(ctx context.Context, spec sandboxhub.Spec, cc sandboxhub.CreateContext, agentID string) error {
	// A volume is only removed through a name no other sandbox id maps to.
	if err := checkSandboxID(cc); err != nil {
		return err
	}
	container, volume := p.NamesFor(cc.ID)
	if cc.Tenancy == sandboxhub.TenancyOwn || agentID == "" {
		if _, err := p.runner.Run(ctx, []string{"rm", "--force", container}, RunOptions{Timeout: removeTimeout, SandboxID: cc.ID}); err != nil {
			return err
		}
		_, err := p.runner.Run(ctx, []string{"volume", "rm", "--force", volume}, RunOptions{Timeout: removeTimeout, SandboxID: cc.ID})
		return err
	}
	if err := sandboxhub.AssertSafeAgentID(agentID); err != nil {
		return err
	}
	exists, err := p.runner.Run(ctx, []string{"volume", "inspect", volume}, RunOptions{Timeout: inspectTimeout, SandboxID: cc.ID})
	if err != nil {
		return err
	}
	if !exitedWith(exists, 0) {
		return nil // never created - nothing stored
	}
	image := firstNonEmpty(spec.Image, p.settings.DefaultImage)
	if err := checkImageName(image, cc.ID); err != nil {
		return err
	}
	present, err := p.runner.Run(ctx, []string{"image", "inspect", "--format", "{{.Id}}", image}, RunOptions{Timeout: inspectTimeout})
	if err != nil {
		return err
	}
	if !exitedWith(present, 0) {
		return nil
	}
	r, err := p.runner.Run(ctx, []string{
		"run", "--rm",
		"--network", "none",
		"--user", p.settings.User,
		"--cap-drop", "ALL",
		"--security-opt", "no-new-privileges",
		"--mount", "type=volume,source=" + volume + ",target=" + sandboxhub.WorkspaceRoot,
		"--entrypoint", "sh",
		image,
		"-c", `chmod -R u+rwX -- "$1" 2>/dev/null; rm -rf -- "$1"`,
		"sh", sandboxhub.AgentsDir + "/" + agentID,
	}, RunOptions{Timeout: removeFilesTimeout, SandboxID: cc.ID})
	if err != nil {
		return err
	}
	if !exitedWith(r, 0) {
		return kernel.NewSandboxError(
			"Could not remove the agent's sandbox files: "+jsstr.TruncateUTF16(jsstr.Trim(r.Stderr), 300),
			kernel.SandboxCodeEnvironmentUnavailable, cc.ID)
	}
	return nil
}

// CheckAvailable reports whether Docker is reachable, for
// startup logs and health checks. It never fails: every problem is a Reason.
func (p *Provider) CheckAvailable(ctx context.Context) Availability {
	r, err := p.runner.Run(ctx, []string{"version", "--format", "{{.Server.Version}}"}, RunOptions{Timeout: versionTimeout})
	if err != nil {
		return Availability{Reason: err.Error()}
	}
	if version := jsstr.Trim(r.Stdout); exitedWith(r, 0) && version != "" {
		return Availability{OK: true, Version: version}
	}
	first, _, _ := strings.Cut(jsstr.Trim(r.Stderr), "\n")
	if first == "" {
		first = "docker exited with " + codeText(r.Code)
	}
	return Availability{Reason: first}
}

// checkDocker fails with a retryable environment_unavailable when Docker is not running.
func (p *Provider) checkDocker(ctx context.Context, sandboxID string) error {
	if status := p.CheckAvailable(ctx); !status.OK {
		return kernel.NewSandboxError(
			"Docker is not available (is Docker running?): "+status.Reason, kernel.SandboxCodeEnvironmentUnavailable, sandboxID)
	}
	return nil
}

// imageFuture is the result of one image preparation that callers share.
type imageFuture struct {
	done chan struct{}
	err  error
}

// EnsureImage makes sure an image exists locally: the default image
// is built from the bundled Dockerfile, anything else is pulled. Call it at server
// startup to avoid a slow first tool call. One preparation per image is shared by
// concurrent callers and a success is remembered; a failure is forgotten, so the next
// call tries again. An image name that starts with "-" is refused before any docker
// call.
func (p *Provider) EnsureImage(ctx context.Context, image, sandboxID string) error {
	if err := checkImageName(image, sandboxID); err != nil {
		return err
	}
	p.mu.Lock()
	if f, ok := p.images[image]; ok {
		p.mu.Unlock()
		select {
		case <-f.done:
			return f.err
		case <-ctx.Done():
			return kernel.NewSandboxError("Operation cancelled", kernel.SandboxCodeCancelled, sandboxID)
		}
	}
	if ctx.Err() != nil {
		p.mu.Unlock()
		return kernel.NewSandboxError("Operation cancelled", kernel.SandboxCodeCancelled, sandboxID)
	}
	f := &imageFuture{done: make(chan struct{})}
	p.images[image] = f
	p.mu.Unlock()

	// The preparation is shared, so it must not run on this caller's context: if this caller gave up, the build or pull
	// that other callers wait for would be cancelled and they would all get "Operation
	// cancelled". It runs on a detached context in its own goroutine, bounded by the
	// timeouts of its docker calls, and every caller waits under its own context.
	workCtx := context.WithoutCancel(ctx)
	go func() {
		// A panic in the runner becomes the error every waiter gets, and the
		// bookkeeping (forget a failure, release the waiters) always runs.
		defer func() {
			if r := recover(); r != nil {
				f.err = panicError(sandboxID, r)
			}
			if f.err != nil {
				p.mu.Lock()
				if p.images[image] == f {
					delete(p.images, image) // retried on the next call
				}
				p.mu.Unlock()
			}
			close(f.done)
		}()
		f.err = p.prepareImage(workCtx, image, sandboxID)
	}()
	select {
	case <-f.done:
		return f.err
	case <-ctx.Done():
		return kernel.NewSandboxError("Operation cancelled", kernel.SandboxCodeCancelled, sandboxID)
	}
}

func (p *Provider) prepareImage(ctx context.Context, image, sandboxID string) error {
	present, err := p.runner.Run(ctx, []string{"image", "inspect", "--format", "{{.Id}}", image}, RunOptions{Timeout: inspectTimeout, SandboxID: sandboxID})
	if err != nil {
		return err
	}
	if exitedWith(present, 0) {
		return nil
	}
	if image == SandboxImage && p.settings.BuildDefaultImage {
		r, err := p.runner.Run(ctx, []string{"build", "--tag", image, "-"}, RunOptions{Input: dockerfileSource, Timeout: imageBuildTimeout, SandboxID: sandboxID})
		if err != nil {
			return err
		}
		if !exitedWith(r, 0) {
			return kernel.NewSandboxError(
				fmt.Sprintf("Building %s failed: %s", image, tailUTF16(jsstr.Trim(r.Stderr), 500)), kernel.SandboxCodeEnvironmentUnavailable, sandboxID)
		}
		return nil
	}
	if p.settings.PullImages {
		r, err := p.runner.Run(ctx, []string{"pull", image}, RunOptions{Timeout: imagePullTimeout, SandboxID: sandboxID})
		if err != nil {
			return err
		}
		if !exitedWith(r, 0) {
			return kernel.NewSandboxError(
				fmt.Sprintf("Pulling %s failed: %s", image, tailUTF16(jsstr.Trim(r.Stderr), 500)), kernel.SandboxCodeEnvironmentUnavailable, sandboxID)
		}
		return nil
	}
	return kernel.NewSandboxError(fmt.Sprintf("Image \"%s\" is not available locally", image), kernel.SandboxCodeEnvironmentUnavailable, sandboxID)
}

// exitedWith reports whether the call ended with exactly this exit code (a nil code,
// a kill by signal, is never equal).
//
//nolint:unparam // every caller checks 0 today; the code stays explicit so call sites read as explicit exit-code checks
func exitedWith(r RunResult, code int) bool { return r.Code != nil && *r.Code == code }

// codeText is the exit code the way a JS template prints it: null for a signal.
func codeText(code *int) string {
	if code == nil {
		return "null"
	}
	return strconv.Itoa(*code)
}

// tailUTF16 is s.slice(-n): the last n UTF-16 code units. JS may cut a surrogate pair
// in half; a Go string cannot hold half a pair, so a pair that would be split is
// dropped whole.
func tailUTF16(s string, n int) string {
	units := 0
	for _, r := range s {
		units++
		if r >= 0x10000 {
			units++
		}
	}
	skip := units - n
	if skip <= 0 {
		return s
	}
	for i, r := range s {
		if skip <= 0 {
			return s[i:]
		}
		skip--
		if r >= 0x10000 {
			skip--
		}
	}
	return ""
}
