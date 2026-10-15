package artifact_test

import (
	"testing"

	"github.com/Phong95/aec-agent-go/components/artifacthub/artifact"
	"github.com/Phong95/aec-agent-go/internal/clonecheck"
)

// Artifact.Clone must share nothing with the original.
func TestArtifactCloneIsDeep(t *testing.T) {
	sample := artifact.Artifact{
		ID: "art-1", AgentID: "a1", Name: "report.md", Type: "text/markdown", Description: "d",
		Tags: []string{"x", "y"}, SessionID: "s1", Version: 2, SandboxPath: "artifacts/report.md",
		SandboxID: "docker:shared", Bytes: 10, SHA256: "abc", CreatedAt: "2026-03-01T00:00:00.000Z", UpdatedAt: "2026-03-02T00:00:00.000Z",
	}
	clonecheck.AssertDeep(t, sample, artifact.Artifact.Clone)
}

// Version.Clone must share nothing with the original; a pointer or slice field
// added to Version or Origin without a Clone line fails here.
func TestVersionCloneIsDeep(t *testing.T) {
	sample := artifact.Version{
		ArtifactID: "art-1", Version: 2, Bytes: 10, SHA256: "abc", CreatedAt: "2026-03-01T00:00:00.000Z", Note: "n",
		Origin: artifact.Origin{AgentID: "a1", SessionID: "s1", Tool: "save_artifact", SandboxPath: "artifacts/report.md", SandboxID: "docker:shared"},
	}
	clonecheck.AssertDeep(t, sample, artifact.Version.Clone)
}
