package docker

import (
	_ "embed" // go:embed of the helper and the Dockerfile
)

// helperSource and dockerfileSource are written from the recorded helper and
// Dockerfile dump (tests/testdata/golden/docker-helper.json) and a test compares them
// byte for byte; never edit them by hand, and never convert their line endings
// (.gitattributes marks them -text). To refresh them after a helper change, run node
// core/go/tools/wiregen/wiregen.mjs and write the two strings of the dump to the files
// (see the failure text of TestEmbeddedHelperMatchesGolden).
//
//go:embed embed/helper.js
var helperSource string

//go:embed embed/Dockerfile
var dockerfileSource string

// SandboxImage is the default image of Docker sandboxes: Debian slim with Node
// (required, the sandbox helper runs on it), Python, git and ripgrep. The provider
// builds it from SandboxDockerfile the first time it is missing.
const SandboxImage = "aec-sandbox:latest"

// helperPath is where the helper lives inside the container.
const helperPath = "/tmp/.aec/helper.js"

// SandboxDockerfile returns the Dockerfile of SandboxImage, which has no build
// context:
//
//	docker build -t aec-sandbox:latest - < Dockerfile
//
// The node user (uid 1000) owns /workspace; containers run as it.
func SandboxDockerfile() string { return dockerfileSource }
