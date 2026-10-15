package docker

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// The Docker tests that talk to a real daemon are opt-in (CONTEXT C4, PITFALLS 11 and
// 14): they run only with AEC_DOCKER_TESTS=1 and a working Docker daemon, and in CI only
// on the Linux runner. They are written to be safe on a machine that has live
// aec-sandbox-* containers (the user's own):
//
//   - every test makes a fresh name prefix, "aec-test-" plus 6 lowercase hex characters
//     (newPrefix), and gives it to the provider as the name prefix, so every container
//     and volume it creates is named "<prefix>-...";
//   - cleanup lists containers and volumes by that prefix only and removes a name only
//     when it passes a second check in Go (cleanupPrefix);
//   - the few commands a test runs by hand (inspect, stop) go through ownedDocker, which
//     refuses a container name that is not the test's own;
//   - no test builds, tags, removes or prunes an image: the test image is pulled at
//     most (PullImages), and the default sandbox image (aec-sandbox:latest) is never
//     built, because every test names its own image.

const (
	// guardEnv is the opt-in switch.
	guardEnv = "AEC_DOCKER_TESTS"
	// imageEnv names the image of the tests; it must provide node (>= 18) and run as
	// uid 1000 (the node images do).
	imageEnv = "AEC_DOCKER_TEST_IMAGE"
	// defaultTestImage is a stock Node image, so no image build is needed.
	defaultTestImage = "node:22-bookworm-slim"
	// testPrefixStem starts every name a test creates.
	testPrefixStem = "aec-test-"
	// dockerCallWait bounds every docker command the helpers run by hand.
	dockerCallWait = 2 * time.Minute
)

// requireDocker skips the test unless the opt-in switch is set and the Docker daemon
// answers; it returns the image the test must use.
func requireDocker(t *testing.T) string {
	t.Helper()
	if os.Getenv(guardEnv) != "1" {
		t.Skipf("Docker tests are opt-in: set %s=1 (needs a Docker daemon with Linux containers)", guardEnv)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "docker", "version", "--format", "{{.Server.Version}}").Output() //nolint:gosec // a fixed, read-only command
	if err != nil || strings.TrimSpace(string(out)) == "" {
		t.Skipf("no Docker daemon answered `docker version` (%v)", err)
	}
	if image := os.Getenv(imageEnv); image != "" {
		return image
	}
	return defaultTestImage
}

// newPrefix returns a name prefix that no other run uses, aec-test-<6 hex>, and
// registers its cleanup. It is the only source of names a test may create.
func newPrefix(t *testing.T) string {
	t.Helper()
	prefix := testPrefixStem + randomHex(t, 3)
	t.Cleanup(func() { cleanupPrefix(t, prefix) })
	return prefix
}

// randomHex is n random bytes as lowercase hex.
func randomHex(t *testing.T, n int) string {
	t.Helper()
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatalf("random bytes: %v", err)
	}
	return hex.EncodeToString(b)
}

// ownedName reports whether a container or volume name belongs to the prefix: the name
// itself or "<prefix>-<anything>". A name like aec-test-123456x is not owned.
func ownedName(prefix, name string) bool {
	return strings.HasPrefix(prefix, testPrefixStem) && (name == prefix || strings.HasPrefix(name, prefix+"-"))
}

// dockerOut runs one docker command and returns its trimmed stdout and its error.
func dockerOut(args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), dockerCallWait)
	defer cancel()
	out, err := exec.CommandContext(ctx, "docker", args...).Output() //nolint:gosec // arguments are built by this file from owned names only
	return strings.TrimSpace(string(out)), err
}

// cleanupPrefix removes the containers and volumes of one prefix and nothing else: the
// daemon filter selects names that start with the prefix and every name is checked
// again here before it is removed.
func cleanupPrefix(t *testing.T, prefix string) {
	t.Helper()
	if !strings.HasPrefix(prefix, testPrefixStem) {
		t.Errorf("cleanup refused: %q is not a test prefix", prefix)
		return
	}
	containers, err := dockerOut("ps", "-a", "--filter", "name=^"+prefix, "--format", "{{.Names}}")
	if err != nil {
		t.Logf("cleanup: listing containers of %s: %v", prefix, err)
	}
	for _, name := range strings.Fields(containers) {
		if !ownedName(prefix, name) {
			t.Logf("cleanup: leaving %q alone (not owned by %s)", name, prefix)
			continue
		}
		if _, err := dockerOut("rm", "--force", name); err != nil {
			t.Errorf("cleanup: removing container %s: %v", name, err)
		}
	}
	volumes, err := dockerOut("volume", "ls", "-q", "--filter", "name=^"+prefix)
	if err != nil {
		t.Logf("cleanup: listing volumes of %s: %v", prefix, err)
	}
	for _, name := range strings.Fields(volumes) {
		if !ownedName(prefix, name) {
			t.Logf("cleanup: leaving %q alone (not owned by %s)", name, prefix)
			continue
		}
		if _, err := dockerOut("volume", "rm", "--force", name); err != nil {
			t.Errorf("cleanup: removing volume %s: %v", name, err)
		}
	}
}

// ownedDocker runs docker with a verb that names a container or volume of the test: it
// fails the test, before any docker call, when that name is not owned by the prefix.
func ownedDocker(t *testing.T, prefix, name string, verb ...string) (string, error) {
	t.Helper()
	if !ownedName(prefix, name) {
		t.Fatalf("refusing to run docker %v on %q: not owned by the test prefix %s", verb, name, prefix)
	}
	return dockerOut(append(verb, name)...)
}
