package docker

import (
	"testing"

	"github.com/Phong95/aec-agent-go/components/sandboxhub"
	"github.com/Phong95/aec-agent-go/internal/servicecheck"
)

// TestServicesRealDocker (A-124 phase 2) runs the "services" suite against a real
// container: the helper's service operations and its connect mode. Opt-in like the
// other real-daemon tests.
func TestServicesRealDocker(t *testing.T) {
	e := newDockerEnv(t)
	n := 0
	servicecheck.Run(t, func(t *testing.T) sandboxhub.View {
		t.Helper()
		n++
		// A fresh agent folder per case, in the one shared container of this test.
		return e.resolve(t, "service-agent-"+string(rune('a'+n)))
	})
}
