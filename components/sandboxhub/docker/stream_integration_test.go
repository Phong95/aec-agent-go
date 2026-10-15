package docker

import (
	"testing"

	"github.com/Phong95/aec-agent-go/components/sandboxhub"
	"github.com/Phong95/aec-agent-go/internal/streamcheck"
)

// TestStreamRealDocker (A-124) runs the "fs.stream" suite against a real container:
// the helper's readraw and writeraw modes. Opt-in like the other real-daemon tests.
func TestStreamRealDocker(t *testing.T) {
	e := newDockerEnv(t)
	n := 0
	streamcheck.Run(t, func(t *testing.T) sandboxhub.View {
		t.Helper()
		n++
		// A fresh agent folder per case, in the one shared container of this test.
		return e.resolve(t, "stream-agent-"+string(rune('a'+n)))
	})
}
