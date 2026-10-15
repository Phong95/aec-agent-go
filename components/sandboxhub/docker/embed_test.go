package docker

import (
	"bytes"
	"strings"
	"testing"

	"github.com/Phong95/aec-agent-go/internal/jsonx"
	"github.com/Phong95/aec-agent-go/internal/wiretest"
)

// regenerate is named in every failure so a drifted copy tells how to refresh it.
const regenerate = "keep the helper and dockerfile strings of core/go/tests/testdata/golden/docker-helper.json " +
	"byte for byte equal to core/go/components/sandboxhub/docker/embed/helper.js and embed/Dockerfile: edit them together"

type helperFixture struct {
	HelperPath string `json:"helperPath"`
	Image      string `json:"image"`
	Helper     string `json:"helper"`
	Dockerfile string `json:"dockerfile"`
}

func loadHelperFixture(t *testing.T) helperFixture {
	t.Helper()
	var f helperFixture
	if err := jsonx.UnmarshalStrict(wiretest.Load(t, "docker-helper.json"), &f); err != nil {
		t.Fatalf("decode docker-helper.json: %v", err)
	}
	return f
}

// TestEmbeddedHelperMatchesGolden is the CI hash check of success criterion 3: the embedded
// helper must be byte-identical to the recorded golden helper. A drift of the recorded
// source fails wiregen --check, a drift of the embedded copy fails this test.
func TestEmbeddedHelperMatchesGolden(t *testing.T) {
	f := loadHelperFixture(t)
	if !bytes.Equal([]byte(helperSource), []byte(f.Helper)) {
		t.Fatalf("embed/helper.js (%d bytes) differs from the TS HELPER_SOURCE (%d bytes); %s", len(helperSource), len(f.Helper), regenerate)
	}
	if strings.Contains(helperSource, "\r") {
		t.Errorf("embed/helper.js contains a carriage return; it must stay LF (.gitattributes marks it -text)")
	}
	if helperPath != f.HelperPath {
		t.Errorf("helperPath = %q, the TS HELPER_PATH is %q", helperPath, f.HelperPath)
	}
	if helperSource == "" {
		t.Errorf("embed/helper.js is empty")
	}
}

func TestEmbeddedDockerfileMatchesGolden(t *testing.T) {
	f := loadHelperFixture(t)
	if !bytes.Equal([]byte(SandboxDockerfile()), []byte(f.Dockerfile)) {
		t.Fatalf("embed/Dockerfile (%d bytes) differs from the TS AEC_SANDBOX_DOCKERFILE (%d bytes); %s", len(SandboxDockerfile()), len(f.Dockerfile), regenerate)
	}
	if strings.Contains(SandboxDockerfile(), "\r") {
		t.Errorf("embed/Dockerfile contains a carriage return; it must stay LF (.gitattributes marks it -text)")
	}
	if SandboxImage != f.Image {
		t.Errorf("SandboxImage = %q, the TS AEC_SANDBOX_IMAGE is %q", SandboxImage, f.Image)
	}
}
