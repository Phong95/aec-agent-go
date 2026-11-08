package conformance_test

import (
	"testing"

	"github.com/Phong95/aec-agent-go/components/sandboxhub"
	"github.com/Phong95/aec-agent-go/conformance"
	"github.com/Phong95/aec-agent-go/conformance/internal/check"
)

// TestSandboxViewScoped (A-216) runs the SandboxView suite on a view scoped to session
// folders: scoped to the agent's own folder it must behave exactly like the view itself.
func TestSandboxViewScoped(t *testing.T) {
	conformance.SandboxView(t, func(t *testing.T) conformance.SandboxViewFixture {
		t.Helper()
		f := inMemoryFixture(t)
		scoped, err := sandboxhub.Scope(f.View, []string{f.View.WorkingDir()})
		check.NoErr(t, err)
		f.View = scoped
		return f
	})
}
