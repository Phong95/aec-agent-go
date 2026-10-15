#!/usr/bin/env bash
# Consumer smoke test (FND-01): a clean module on Go 1.25 can require the SDK
# and go build.
#
# The module is published through the public mirror github.com/Phong95/aec-agent-go
# (see core/go/RELEASE.md), which only receives code after it is merged, so the
# working tree is wired in with a local replace. After a release, the manual check is
# go get github.com/Phong95/aec-agent-go@<version> from a clean module.
set -euo pipefail

# The module directory is this script's ../.. ; print it in Windows form when
# available (Git Bash) because the go tool needs a native path in -replace.
MODULE_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && { pwd -W 2>/dev/null || pwd; })"

# A dependency needing a newer Go must fail here, not auto-download a toolchain.
export GOTOOLCHAIN=local

WORK_DIR="$(mktemp -d)"
trap 'rm -rf "$WORK_DIR"' EXIT
cd "$WORK_DIR"

go mod init example.com/smoke
go mod edit -go=1.25.0
go mod edit \
  -require=github.com/Phong95/aec-agent-go@v0.0.0 \
  -replace=github.com/Phong95/aec-agent-go="$MODULE_DIR"

# The consumer also imports llm and blank-imports llm/all. Linking llm/all pulls in
# the Anthropic, OpenAI and AWS SDK modules, so building it here proves the whole
# vendor graph builds on the Go 1.25 floor (GOTOOLCHAIN=local, go 1.25.0 above). It
# also builds the entry points, aec.NewAgentCore and aec.CreateAgent, from a module outside
# core/go, and the built-in tools (workspace.NewTools, web.NewExtractTool and
# web.ExtractToolOptions), so they build on the Go 1.25 floor too.
printf '%s\n' \
  'package main' \
  '' \
  'import (' \
  '	aec "github.com/Phong95/aec-agent-go"' \
  '	"github.com/Phong95/aec-agent-go/kernel"' \
  '	"github.com/Phong95/aec-agent-go/components/llmhub"' \
  '	_ "github.com/Phong95/aec-agent-go/components/llmhub/providers/all"' \
  '	"github.com/Phong95/aec-agent-go/kernel/ordered"' \
  '	"github.com/Phong95/aec-agent-go/components/persistence"' \
  '	"github.com/Phong95/aec-agent-go/components/sandboxhub"' \
  '	"github.com/Phong95/aec-agent-go/components/toolhub/builtin/web"' \
  '	"github.com/Phong95/aec-agent-go/components/toolhub/builtin/workspace"' \
  ')' \
  '' \
  'func main() {' \
  '	_ = aec.Version' \
  '	_ = aec.NewAgentCore' \
  '	_ = aec.CreateAgent' \
  '	_ = workspace.NewTools' \
  '	_ = web.NewExtractTool' \
  '	_ = web.ExtractToolOptions{}' \
  '	_ = kernel.NewID()' \
  '	_ = llmhub.ListProviders()' \
  '	_ = ordered.NewObject()' \
  '	_ = persistence.NewPersistenceHub()' \
  '	_ = sandboxhub.NewSandboxHub()' \
  '}' > main.go

GOFLAGS=-mod=mod go build ./...

echo "smoke: ok"
