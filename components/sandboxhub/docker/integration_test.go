package docker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Phong95/aec-agent-go/components/sandboxhub"
	"github.com/Phong95/aec-agent-go/kernel"
)

// Real-daemon tests (guarded, see guard_test.go): the 9 isolation tests and the 5
// lifecycle tests. The 3 workspace-tool tests stay with the workspace tools
// (Phase 11) and the shared SandboxView suite runs from the conformance package. Every
// test gets its own aec-test-<hex> prefix, so it never sees, reuses or removes a
// container of another run or of the user.

const (
	// serverSecret is set in the test process environment: nothing may carry it into a
	// container.
	serverSecretEnv = "AEC_TEST_SERVER_SECRET"
	// eventuallyWait is how long a check on the container's processes may take to
	// become true.
	eventuallyWait = 5 * time.Second
	// createWait bounds the creation of a sandbox (an image pull may be part of it).
	createWait = 10 * time.Minute
)

// dockerEnv is one test's world: its prefix and a hub with a Docker provider under it.
type dockerEnv struct {
	prefix, image string
	provider      *Provider
	hub           *sandboxhub.SandboxHub
}

// newTestProvider builds a provider under the test prefix. It never builds the default
// sandbox image (BuildDefaultImage false): the tests name their own image.
func newTestProvider(t *testing.T, prefix, image string) *Provider {
	t.Helper()
	build := false
	p, err := New(Options{NamePrefix: prefix, DefaultImage: image, BuildDefaultImage: &build})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return p
}

// newTestHub returns a hub with the provider registered, the shared sandbox on
// the test image and the policy maxLimits cpus 1, memory 512 MB, pids 128.
func newTestHub(t *testing.T, p *Provider, image string) *sandboxhub.SandboxHub {
	t.Helper()
	hub := sandboxhub.NewSandboxHub()
	if err := hub.Register(p); err != nil {
		t.Fatal(err)
	}
	if err := hub.ConfigureShared(sandboxhub.Spec{Kind: sandboxhub.KindDocker, Image: image}); err != nil {
		t.Fatal(err)
	}
	cpus, memory, pids := 1.0, 512, 128
	hub.SetPolicy(sandboxhub.PolicyUpdate{MaxLimits: &sandboxhub.LimitsOverride{Cpus: &cpus, MemoryMb: &memory, Pids: &pids}})
	return hub
}

// disposeHub disposes a hub when the test ends (the container goes, the volume stays;
// the prefix cleanup removes both).
func disposeHub(t *testing.T, hub *sandboxhub.SandboxHub) {
	t.Helper()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), dockerCallWait)
		defer cancel()
		if err := hub.Dispose(ctx); err != nil {
			t.Logf("dispose the hub: %v", err)
		}
	})
}

// newDockerEnv is requireDocker, newPrefix and a hub; the test body resolves agents on it.
func newDockerEnv(t *testing.T) *dockerEnv {
	t.Helper()
	image := requireDocker(t)
	prefix := newPrefix(t)
	p := newTestProvider(t, prefix, image)
	hub := newTestHub(t, p, image)
	disposeHub(t, hub)
	return &dockerEnv{prefix: prefix, image: image, provider: p, hub: hub}
}

// container is the name of the shared sandbox's container.
func (e *dockerEnv) container() string { return e.prefix + "-shared" }

// resolve is hub.resolve(agentId) with the creation allowed to take a while.
func (e *dockerEnv) resolve(t *testing.T, agent string) sandboxhub.View {
	t.Helper()
	return resolveOn(t, e.hub, agent)
}

func resolveOn(t *testing.T, hub *sandboxhub.SandboxHub, agent string) sandboxhub.View {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), createWait)
	defer cancel()
	v, err := hub.Resolve(ctx, agent, sandboxhub.Selection{})
	if err != nil {
		t.Fatalf("resolve %s: %v", agent, err)
	}
	return v
}

// wantCode requires err to be a SandboxError with the given code.
func wantCode(t *testing.T, err error, code kernel.SandboxErrorCode) {
	t.Helper()
	var se *kernel.SandboxError
	if !errors.As(err, &se) {
		t.Fatalf("error = %v, want a SandboxError %q", err, code)
	}
	if se.Code != code {
		t.Fatalf("error code = %q (%s), want %q", se.Code, se.Message, code)
	}
}

// run runs a command with a 20 s timeout and 200000 bytes of output.
func run(t *testing.T, v sandboxhub.View, command ...string) sandboxhub.ExecResult {
	t.Helper()
	return runWith(t, v, sandboxhub.ExecRequest{Command: command, TimeoutMs: 20_000, MaxOutputBytes: 200_000})
}

func runWith(t *testing.T, v sandboxhub.View, req sandboxhub.ExecRequest) sandboxhub.ExecResult {
	t.Helper()
	res, err := v.Exec(context.Background(), req, sandboxhub.ExecOptions{})
	if err != nil {
		t.Fatalf("exec %v: %v", req.Command, err)
	}
	return res
}

// notZero reports whether the exit code is not 0: a null code (killed) counts.
func notZero(code *int) bool { return code == nil || *code != 0 }

// processScript prints the command line of every process of the container (slim images
// have no ps, so /proc is read).
const processScript = `const fs=require('fs');console.log(fs.readdirSync('/proc').filter(p=>/^[0-9]+$/.test(p))` +
	`.map(p=>{try{return fs.readFileSync('/proc/'+p+'/cmdline','utf8').split('\0').join(' ')}catch(e){return ''}}).join('\n'))`

func processList(t *testing.T, v sandboxhub.View) string {
	t.Helper()
	return run(t, v, "node", "-e", processScript).Stdout
}

// eventually polls check every 200 ms for eventuallyWait and fails the test when it
// never holds, rather than hanging.
func eventually(t *testing.T, what string, check func() bool) {
	t.Helper()
	until := time.Now().Add(eventuallyWait)
	for time.Now().Before(until) {
		if check() {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	if !check() {
		t.Fatalf("still true after %s: %s", eventuallyWait, what)
	}
}

// hostConfig is `docker container inspect` of the test's own container, HostConfig.
func hostConfig(t *testing.T, prefix, container string) map[string]any {
	t.Helper()
	out, err := ownedDocker(t, prefix, container, "container", "inspect", "--format", "{{json .HostConfig}}")
	if err != nil {
		t.Fatalf("inspect %s: %v", container, err)
	}
	var cfg map[string]any
	if err := json.Unmarshal([]byte(out), &cfg); err != nil {
		t.Fatalf("decode HostConfig %q: %v", out, err)
	}
	return cfg
}

// -- Tracer --

func TestDockerIntegrationTracerWriteReadExec(t *testing.T) {
	image := requireDocker(t)
	prefix := newPrefix(t)
	p := newTestProvider(t, prefix, image)
	ctx, cancel := context.WithTimeout(context.Background(), createWait)
	defer cancel()
	sb, err := p.Create(ctx, sandboxhub.Spec{Kind: sandboxhub.KindDocker, Image: image},
		sandboxhub.CreateContext{ID: "docker:shared", Tenancy: sandboxhub.TenancyShared})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	disposed := false
	t.Cleanup(func() {
		if !disposed {
			_ = sb.Dispose(context.Background(), true)
		}
	})
	if got, want := sb.(*Sandbox).ContainerName(), prefix+"-shared"; got != want {
		t.Fatalf("container name = %q, want %q", got, want)
	}
	v, err := sb.ForAgent(ctx, "tracer")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := v.WriteFile(ctx, "hello.txt", "hi", sandboxhub.WriteOptions{}); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	read, err := v.ReadFile(ctx, "hello.txt", sandboxhub.ReadOptions{})
	if err != nil || read.Content != "hi" {
		t.Fatalf("ReadFile = %+v, %v; want content hi", read, err)
	}
	res := run(t, v, "echo", "hello")
	if res.ExitCode == nil || *res.ExitCode != 0 || res.Stdout != "hello\n" {
		t.Fatalf("exec echo = %+v, want exit 0 and hello", res)
	}
	disposed = true
	if err := sb.Dispose(ctx, true); err != nil {
		t.Fatalf("Dispose: %v", err)
	}
}

// -- Isolation --

func TestDockerIntegrationKillsTheProcessTreeAtTheTimeout(t *testing.T) {
	e := newDockerEnv(t)
	v := e.resolve(t, "hardening")
	res := runWith(t, v, sandboxhub.ExecRequest{Command: []string{"sh", "-c", "sleep 301 & sleep 302; echo never"}, TimeoutMs: 500, MaxOutputBytes: 1_000})
	if !res.TimedOut || res.ExitCode != nil {
		t.Fatalf("result = %+v, want timed out with no exit code", res)
	}
	if strings.Contains(res.Stdout, "never") {
		t.Fatalf("the command ran on after the timeout: %q", res.Stdout)
	}
	sleeping := regexp.MustCompile(`sleep 30[12]`)
	eventually(t, "sleep 301 or 302 is still running in the container", func() bool { return !sleeping.MatchString(processList(t, v)) })
}

func TestDockerIntegrationKillsTheCommandWhenCancelled(t *testing.T) {
	e := newDockerEnv(t)
	v := e.resolve(t, "hardening")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var cancelledAt atomic.Int64
	timer := time.AfterFunc(500*time.Millisecond, func() {
		cancelledAt.Store(time.Now().UnixNano())
		cancel()
	})
	defer timer.Stop()
	res, err := v.Exec(ctx, sandboxhub.ExecRequest{Command: []string{"sleep", "303"}, TimeoutMs: 30_000, MaxOutputBytes: 1_000}, sandboxhub.ExecOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Cancelled || res.ExitCode != nil {
		t.Fatalf("result = %+v, want cancelled with no exit code", res)
	}
	// The stop closes the helper stdin first, so the exec ends long before the docker
	// client would be killed (stopGrace); the order itself is proven on fake time by
	// TestStopSequenceCancel.
	if took := time.Since(time.Unix(0, cancelledAt.Load())); took > stopGrace-time.Second {
		t.Fatalf("Exec returned %s after the cancel, close to the %s grace period: the helper was not asked to stop first", took, stopGrace)
	}
	eventually(t, "sleep 303 is still running in the container", func() bool { return !strings.Contains(processList(t, v), "sleep 303") })
}

func TestDockerIntegrationStopsACommandFloodingOutputAtTheCap(t *testing.T) {
	e := newDockerEnv(t)
	v := e.resolve(t, "hardening")
	res := runWith(t, v, sandboxhub.ExecRequest{Command: []string{"sh", "-c", "yes flood"}, TimeoutMs: 20_000, MaxOutputBytes: 5_000})
	if !res.Truncated || len(res.Stdout) > 5_000 {
		t.Fatalf("truncated = %v, stdout %d bytes; want truncated and at most 5000", res.Truncated, len(res.Stdout))
	}
	eventually(t, "yes flood is still running in the container", func() bool { return !strings.Contains(processList(t, v), "yes flood") })
}

func TestDockerIntegrationHasNoNetwork(t *testing.T) {
	e := newDockerEnv(t)
	v := e.resolve(t, "hardening")
	connect := run(t, v, "node", "-e",
		"require('net').connect({host:'1.1.1.1',port:443,timeout:3000})"+
			".on('connect',()=>{console.log('CONNECTED');process.exit(0)})"+
			".on('error',(err)=>{console.log('ERR '+err.code);process.exit(3)})"+
			".on('timeout',()=>{console.log('TIMEOUT');process.exit(4)})")
	if strings.Contains(connect.Stdout, "CONNECTED") || !notZero(connect.ExitCode) {
		t.Fatalf("a connection was made: %+v", connect)
	}
	if dns := run(t, v, "getent", "hosts", "example.com"); !notZero(dns.ExitCode) {
		t.Fatalf("a name was resolved: %+v", dns)
	}
	interfaces := run(t, v, "ls", "/sys/class/net")
	if got := strings.Fields(interfaces.Stdout); !slices.Equal(got, []string{"lo"}) {
		t.Fatalf("network interfaces = %q, want only lo", got)
	}
}

func TestDockerIntegrationExposesNoServerSecretsNoDockerSocketAndNoHostEnvironment(t *testing.T) {
	secret := "server-secret-" + randomHex(t, 6)
	t.Setenv(serverSecretEnv, secret)
	e := newDockerEnv(t)
	v := e.resolve(t, "hardening")
	env := run(t, v, "env")
	if strings.Contains(env.Stdout, secret) {
		t.Fatalf("the server secret is in the container environment")
	}
	if regexp.MustCompile(`DATABASE_URL|API_KEY|AWS_`).MatchString(env.Stdout) {
		t.Fatalf("a server variable is in the container environment: %q", env.Stdout)
	}
	if init := run(t, v, "cat", "/proc/1/environ"); strings.Contains(init.Stdout, secret) {
		t.Fatalf("the server secret is in the init environment")
	}
	if socket := run(t, v, "sh", "-c", "test -e /var/run/docker.sock || test -e /run/docker.sock"); !notZero(socket.ExitCode) {
		t.Fatalf("a Docker socket exists in the container: %+v", socket)
	}
}

func TestDockerIntegrationRunsAsNonRootWithoutCapabilitiesOnAReadOnlyRootFilesystem(t *testing.T) {
	e := newDockerEnv(t)
	v := e.resolve(t, "hardening")
	if uid := strings.TrimSpace(run(t, v, "id", "-u").Stdout); uid != "1000" {
		t.Fatalf("uid = %q, want 1000", uid)
	}
	caps := run(t, v, "grep", "CapEff", "/proc/self/status")
	if !regexp.MustCompile(`CapEff:\s+0+\s*$`).MatchString(caps.Stdout) {
		t.Fatalf("effective capabilities = %q, want none", caps.Stdout)
	}
	for _, path := range []string{"/etc/aec-test", "/usr/local/bin/aec-test"} {
		if res := run(t, v, "touch", path); !notZero(res.ExitCode) {
			t.Fatalf("touch %s succeeded on a read-only root filesystem", path)
		}
	}
	// The helper is root-owned: the sandbox user cannot tamper with it.
	if res := run(t, v, "sh", "-c", "echo x >> /tmp/.aec/helper.js"); !notZero(res.ExitCode) {
		t.Fatalf("the sandbox user could write to the helper")
	}
}

func TestDockerIntegrationCreatesTheContainerWithResourceLimitsAndHardeningFlags(t *testing.T) {
	e := newDockerEnv(t)
	e.resolve(t, "hardening")
	cfg := hostConfig(t, e.prefix, e.container())
	mib := float64(2048 * 1024 * 1024)
	for key, want := range map[string]any{
		"Memory": mib, "MemorySwap": mib, "NanoCpus": 2e9, "PidsLimit": float64(256),
		"NetworkMode": "none", "ReadonlyRootfs": true, "Init": true, "Privileged": false,
	} {
		if cfg[key] != want {
			t.Errorf("HostConfig.%s = %v, want %v", key, cfg[key], want)
		}
	}
	if got := fmt.Sprint(cfg["CapDrop"]); got != "[ALL]" {
		t.Errorf("HostConfig.CapDrop = %v, want [ALL]", got)
	}
	if got := fmt.Sprint(cfg["SecurityOpt"]); got != "[no-new-privileges]" {
		t.Errorf("HostConfig.SecurityOpt = %v, want [no-new-privileges]", got)
	}
	// Only the workspace volume is mounted: no host folders, no Docker socket.
	out, err := ownedDocker(t, e.prefix, e.container(), "container", "inspect", "--format", "{{json .Mounts}}")
	if err != nil {
		t.Fatal(err)
	}
	var mounts []struct{ Type, Destination string }
	if err := json.Unmarshal([]byte(out), &mounts); err != nil {
		t.Fatalf("decode Mounts %q: %v", out, err)
	}
	if len(mounts) != 1 || mounts[0].Type != "volume" || mounts[0].Destination != "/workspace" {
		t.Errorf("mounts = %+v, want only the workspace volume", mounts)
	}
}

func TestDockerIntegrationBlocksSymlinkEscapesAnAgentCreatesWithExec(t *testing.T) {
	e := newDockerEnv(t)
	v := e.resolve(t, "hardening")
	for _, args := range [][]string{
		{"ln", "-sfn", "/etc", "etc-link"},
		{"ln", "-sfn", "/workspace/agents/someone-else", "peer-link"},
		{"ln", "-sfn", "/tmp/nowhere/new-file", "dangling"},
	} {
		if res := run(t, v, args...); res.ExitCode == nil || *res.ExitCode != 0 {
			t.Fatalf("%v = %+v, want exit 0", args, res)
		}
	}
	ctx := context.Background()
	_, err := v.ReadFile(ctx, "etc-link/passwd", sandboxhub.ReadOptions{})
	wantCode(t, err, kernel.SandboxCodePathEscape)
	_, err = v.WriteFile(ctx, "peer-link/planted.txt", "x", sandboxhub.WriteOptions{})
	wantCode(t, err, kernel.SandboxCodePathEscape)
	_, err = v.WriteFile(ctx, "dangling", "x", sandboxhub.WriteOptions{})
	wantCode(t, err, kernel.SandboxCodePathEscape)
	_, err = v.Exec(ctx, sandboxhub.ExecRequest{Command: []string{"pwd"}, Cwd: "etc-link", TimeoutMs: 5_000, MaxOutputBytes: 1_000}, sandboxhub.ExecOptions{})
	wantCode(t, err, kernel.SandboxCodePathEscape)
	_, err = v.List(ctx, "peer-link", sandboxhub.ListOptions{})
	wantCode(t, err, kernel.SandboxCodePathEscape)
}

func TestDockerIntegrationDoesNotPassHostEnvironmentVariablesItWasNotAskedTo(t *testing.T) {
	secret := "server-secret-" + randomHex(t, 6)
	t.Setenv(serverSecretEnv, secret)
	e := newDockerEnv(t)
	v := e.resolve(t, "hardening")
	res := runWith(t, v, sandboxhub.ExecRequest{Command: []string{"printenv", serverSecretEnv}, TimeoutMs: 5_000, MaxOutputBytes: 1_000})
	if !notZero(res.ExitCode) || strings.Contains(res.Stdout, secret) {
		t.Fatalf("printenv = %+v, want a failure and no secret", res)
	}
}

// -- Lifecycle --

func TestDockerIntegrationKeepsFilesAcrossAFullStopAndStartThenDeletesThemWithTheAgent(t *testing.T) {
	image := requireDocker(t)
	prefix := newPrefix(t)
	container := prefix + "-shared"
	newHub := func() *sandboxhub.SandboxHub {
		hub := newTestHub(t, newTestProvider(t, prefix, image), image)
		disposeHub(t, hub)
		return hub
	}
	ctx := context.Background()

	first := newHub()
	env := resolveOn(t, first, "keeper")
	if _, err := env.WriteFile(ctx, "notes/keep.md", "persisted", sandboxhub.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := first.Dispose(ctx); err != nil { // removes the container, keeps the volume
		t.Fatal(err)
	}
	if _, err := ownedDocker(t, prefix, container, "container", "inspect"); err == nil {
		t.Fatalf("the container %s still exists after the hub was disposed", container)
	}

	second := newHub()
	again := resolveOn(t, second, "keeper")
	read, err := again.ReadFile(ctx, "notes/keep.md", sandboxhub.ReadOptions{})
	if err != nil || read.Content != "persisted" {
		t.Fatalf("ReadFile after a restart = %+v, %v; want persisted", read, err)
	}
	if err := second.ReleaseAgentData(ctx, "keeper"); err != nil {
		t.Fatal(err)
	}
	third := resolveOn(t, second, "keeper")
	if stat, err := third.Stat(ctx, "notes/keep.md"); err != nil || stat != nil {
		t.Fatalf("Stat after the agent's data was released = %+v, %v; want nil", stat, err)
	}
	if err := second.Dispose(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestDockerIntegrationRemovesAStoppedAgentsSharedFolderWithoutStartingTheSandbox(t *testing.T) {
	image := requireDocker(t)
	prefix := newPrefix(t)
	newHub := func() *sandboxhub.SandboxHub {
		hub := newTestHub(t, newTestProvider(t, prefix, image), image)
		disposeHub(t, hub)
		return hub
	}
	ctx := context.Background()

	first := newHub()
	if _, err := resolveOn(t, first, "gone").WriteFile(ctx, "x.txt", "x", sandboxhub.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := first.Dispose(ctx); err != nil {
		t.Fatal(err)
	}

	second := newHub()
	// The shared sandbox is not running: the hub goes through provider.RemoveData.
	if err := second.ReleaseAgentData(ctx, "gone"); err != nil {
		t.Fatalf("ReleaseAgentData: %v", err)
	}
	if _, err := ownedDocker(t, prefix, prefix+"-shared", "container", "inspect"); err == nil {
		t.Fatalf("releasing a stopped agent's data started the sandbox container")
	}
	env := resolveOn(t, second, "gone")
	if stat, err := env.Stat(ctx, "x.txt"); err != nil || stat != nil {
		t.Fatalf("Stat after RemoveData = %+v, %v; want nil", stat, err)
	}
	if err := second.Dispose(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestDockerIntegrationRecoversWhenTheContainerWasStoppedBehindItsBack(t *testing.T) {
	e := newDockerEnv(t)
	env := e.resolve(t, "recover")
	ctx := context.Background()
	if _, err := env.WriteFile(ctx, "before.txt", "still here", sandboxhub.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := ownedDocker(t, e.prefix, e.container(), "stop", "-t", "0"); err != nil {
		t.Fatalf("docker stop: %v", err)
	}
	read, err := env.ReadFile(ctx, "before.txt", sandboxhub.ReadOptions{})
	if err != nil || read.Content != "still here" {
		t.Fatalf("ReadFile after the container was stopped = %+v, %v", read, err)
	}
	if res := run(t, env, "echo", "back"); res.Stdout != "back\n" {
		t.Fatalf("exec after recovery = %+v, want back", res)
	}
}

func TestDockerIntegrationGivesAnOwnSandboxTheWholeWorkspaceAndDeletesItsVolumeWithTheAgent(t *testing.T) {
	e := newDockerEnv(t)
	// The spec a SandboxManager would build for this profile (the manager is Phase 10):
	// limits clamped by the hub policy (cpus 1, pids 128), memory 256 MB.
	memory := 256
	spec, err := e.hub.BuildOwnSpec(sandboxhub.OwnConfig{Limits: &sandboxhub.LimitsOverride{MemoryMb: &memory}})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), createWait)
	defer cancel()
	sb, err := e.provider.Create(ctx, spec, sandboxhub.CreateContext{ID: "docker:own:owner", Tenancy: sandboxhub.TenancyOwn, AgentID: "owner"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	disposed := false
	t.Cleanup(func() {
		if !disposed {
			_ = sb.Dispose(context.Background(), true)
		}
	})
	env, err := sb.ForAgent(ctx, "owner")
	if err != nil {
		t.Fatal(err)
	}
	if env.Traits().Tenancy != sandboxhub.TenancyOwn || env.WorkingDir() != "/workspace" || !slices.Equal(env.Roots(), []string{"/workspace"}) {
		t.Fatalf("own view = tenancy %q, working dir %q, roots %v", env.Traits().Tenancy, env.WorkingDir(), env.Roots())
	}
	if _, err := env.WriteFile(ctx, "mine.txt", "own data", sandboxhub.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	list, err := env.List(ctx, ".", sandboxhub.ListOptions{})
	if err != nil || len(list.Entries) != 1 || list.Entries[0].Name != "mine.txt" {
		t.Fatalf("List = %+v, %v; want only mine.txt", list, err)
	}
	// Profile limits, clamped by the hub policy (cpus 1, pids 128).
	owned := e.prefix + "-own-owner"
	cfg := hostConfig(t, e.prefix, owned)
	if cfg["Memory"] != float64(256*1024*1024) || cfg["NanoCpus"] != 1e9 || cfg["PidsLimit"] != float64(128) {
		t.Errorf("own container limits = memory %v, nanocpus %v, pids %v; want 256 MiB, 1e9, 128", cfg["Memory"], cfg["NanoCpus"], cfg["PidsLimit"])
	}

	disposed = true
	if err := sb.Dispose(ctx, true); err != nil {
		t.Fatal(err)
	}
	if _, err := ownedDocker(t, e.prefix, owned, "volume", "inspect"); err == nil {
		t.Fatalf("the volume %s still exists after Dispose(removeData)", owned)
	}
}

func TestDockerIntegrationReportsHealthAndFailsClosedAfterDispose(t *testing.T) {
	image := requireDocker(t)
	prefix := newPrefix(t)
	p := newTestProvider(t, prefix, image)
	ctx, cancel := context.WithTimeout(context.Background(), createWait)
	defer cancel()
	sb, err := p.Create(ctx, sandboxhub.Spec{Kind: sandboxhub.KindDocker, Image: image}, sandboxhub.CreateContext{ID: "docker:health", Tenancy: sandboxhub.TenancyShared})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if got := sb.Health(ctx); got != sandboxhub.HealthReady {
		t.Fatalf("health = %q, want ready", got)
	}
	env, err := sb.ForAgent(ctx, "h1")
	if err != nil {
		t.Fatal(err)
	}
	if err := sb.Dispose(ctx, true); err != nil {
		t.Fatal(err)
	}
	if got := sb.Health(ctx); got != sandboxhub.HealthUnavailable {
		t.Fatalf("health after dispose = %q, want unavailable", got)
	}
	_, err = env.ReadFile(ctx, "x", sandboxhub.ReadOptions{})
	wantCode(t, err, kernel.SandboxCodeEnvironmentUnavailable)
}
