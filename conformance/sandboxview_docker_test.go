package conformance_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Phong95/aec-agent-go/components/sandboxhub"
	"github.com/Phong95/aec-agent-go/components/sandboxhub/docker"
	"github.com/Phong95/aec-agent-go/conformance"
)

// TestSandboxViewDocker runs the SandboxView suite on a real Docker sandbox, through the
// hub (a shared sandbox resolved via sandboxhub.SandboxHub): one shared sandbox for the whole run, a fresh agent per subtest, and the agent's folder
// removed afterwards. It is opt-in (AEC_DOCKER_TESTS=1 and a Docker daemon, CONTEXT C4)
// and never touches a container it did not create: the provider gets a name prefix
// "aec-test-<6 hex>" that belongs to this run alone, and cleanup removes only names with
// that prefix (the rules of sandbox/docker/guard_test.go, duplicated here because test
// helpers cannot cross packages).
func TestSandboxViewDocker(t *testing.T) {
	image := requireDocker(t)
	prefix := newDockerPrefix(t)
	container := prefix + "-shared"
	build := false
	provider, err := docker.New(docker.Options{NamePrefix: prefix, DefaultImage: image, BuildDefaultImage: &build})
	if err != nil {
		t.Fatal(err)
	}
	hub := sandboxhub.NewSandboxHub()
	if err := hub.Register(provider); err != nil {
		t.Fatal(err)
	}
	if err := hub.ConfigureShared(sandboxhub.Spec{Kind: sandboxhub.KindDocker, Image: image}); err != nil {
		t.Fatal(err)
	}
	cpus, memory, pids := 1.0, 512, 128
	hub.SetPolicy(sandboxhub.PolicyUpdate{MaxLimits: &sandboxhub.LimitsOverride{Cpus: &cpus, MemoryMb: &memory, Pids: &pids}})
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		if err := hub.Dispose(ctx); err != nil {
			t.Logf("dispose the hub: %v", err)
		}
	})

	var counter atomic.Int64
	conformance.SandboxView(t, func(t *testing.T) conformance.SandboxViewFixture {
		agentID := fmt.Sprintf("conf-%d", counter.Add(1))
		ctx, cancel := context.WithTimeout(t.Context(), 10*time.Minute)
		defer cancel()
		view, err := hub.Resolve(ctx, agentID, sandboxhub.Selection{})
		if err != nil {
			t.Fatalf("resolve %s: %v", agentID, err)
		}
		t.Cleanup(func() {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			if err := hub.ReleaseAgentData(ctx, agentID); err != nil {
				t.Errorf("release the data of %s: %v", agentID, err)
			}
		})
		return conformance.SandboxViewFixture{
			View: view,
			// Symlink and PlantFile run `docker exec -u <user>` in the test's own
			// container.
			Symlink: func(link, target string) error {
				return execInContainer(prefix, container, "", `mkdir -p "$(dirname "$1")" && ln -sfn "$2" "$1"`, link, target)
			},
			PlantFile: func(path, content string) error {
				return execInContainer(prefix, container, content, `mkdir -p "$(dirname "$1")" && cat > "$1"`, path)
			},
		}
	})
}

// -- The guard and the prefix rules (see sandbox/docker/guard_test.go) --

const (
	dockerGuardEnv     = "AEC_DOCKER_TESTS"
	dockerImageEnv     = "AEC_DOCKER_TEST_IMAGE"
	dockerTestImage    = "node:22-bookworm-slim"
	dockerPrefixStem   = "aec-test-"
	dockerTestUser     = "1000:1000"
	dockerCommandLimit = 2 * time.Minute
)

// requireDocker skips unless AEC_DOCKER_TESTS=1 and the daemon answers; it returns the
// test image.
func requireDocker(t *testing.T) string {
	t.Helper()
	if os.Getenv(dockerGuardEnv) != "1" {
		t.Skipf("Docker tests are opt-in: set %s=1 (needs a Docker daemon with Linux containers)", dockerGuardEnv)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "docker", "version", "--format", "{{.Server.Version}}").Output() //nolint:gosec // a fixed, read-only command
	if err != nil || strings.TrimSpace(string(out)) == "" {
		t.Skipf("no Docker daemon answered `docker version` (%v)", err)
	}
	if image := os.Getenv(dockerImageEnv); image != "" {
		return image
	}
	return dockerTestImage
}

// newDockerPrefix returns aec-test-<6 hex> and registers its prefix-only cleanup.
func newDockerPrefix(t *testing.T) string {
	t.Helper()
	var b [3]byte
	if _, err := rand.Read(b[:]); err != nil {
		t.Fatalf("random prefix: %v", err)
	}
	prefix := dockerPrefixStem + hex.EncodeToString(b[:])
	t.Cleanup(func() { cleanupDockerPrefix(t, prefix) })
	return prefix
}

func ownedByPrefix(prefix, name string) bool {
	return strings.HasPrefix(prefix, dockerPrefixStem) && (name == prefix || strings.HasPrefix(name, prefix+"-"))
}

func dockerOutput(stdin string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), dockerCommandLimit)
	defer cancel()
	cmd := exec.CommandContext(ctx, "docker", args...) //nolint:gosec // arguments are built from owned names only
	cmd.Stdin = strings.NewReader(stdin)
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// cleanupDockerPrefix removes the containers and volumes whose names start with the
// prefix, after a second check in Go, and nothing else.
func cleanupDockerPrefix(t *testing.T, prefix string) {
	t.Helper()
	if !strings.HasPrefix(prefix, dockerPrefixStem) {
		t.Errorf("cleanup refused: %q is not a test prefix", prefix)
		return
	}
	containers, err := dockerOutput("", "ps", "-a", "--filter", "name=^"+prefix, "--format", "{{.Names}}")
	if err != nil {
		t.Logf("cleanup: listing containers of %s: %v (%s)", prefix, err, containers)
	}
	for _, name := range strings.Fields(containers) {
		if !ownedByPrefix(prefix, name) {
			continue
		}
		if out, err := dockerOutput("", "rm", "--force", name); err != nil {
			t.Errorf("cleanup: removing container %s: %v (%s)", name, err, out)
		}
	}
	volumes, err := dockerOutput("", "volume", "ls", "-q", "--filter", "name=^"+prefix)
	if err != nil {
		t.Logf("cleanup: listing volumes of %s: %v (%s)", prefix, err, volumes)
	}
	for _, name := range strings.Fields(volumes) {
		if !ownedByPrefix(prefix, name) {
			continue
		}
		if out, err := dockerOutput("", "volume", "rm", "--force", name); err != nil {
			t.Errorf("cleanup: removing volume %s: %v (%s)", name, err, out)
		}
	}
}

// execInContainer is `docker exec -u <user> -i <container> sh -c <script> sh <args>` on
// the test's own container; it refuses any other container name.
func execInContainer(prefix, container, stdin, script string, args ...string) error {
	if !ownedByPrefix(prefix, container) {
		return fmt.Errorf("refusing to run docker exec on %q: not owned by the test prefix %s", container, prefix)
	}
	full := append([]string{"exec", "-i", "-u", dockerTestUser, container, "sh", "-c", script, "sh"}, args...)
	if out, err := dockerOutput(stdin, full...); err != nil {
		return fmt.Errorf("docker exec in %s: %w (%s)", container, err, out)
	}
	return nil
}
