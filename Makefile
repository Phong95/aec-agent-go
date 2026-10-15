# Makefile for the AEC Agent Go SDK (core/go).
#
# Run from core/go: `make` (or `make help`) lists the targets. The targets wrap the
# same commands CI and RELEASE.md run, so `make ci` before a push reproduces the
# core-go job locally (minus the Windows and Docker jobs).
#
# The recipes are shell-agnostic: on Windows, make runs them through cmd.exe when
# it finds no sh on the PATH (as from PowerShell), so environment variables are set
# with target-specific `export` instead of a `VAR=x cmd` prefix, and nothing needs
# grep, awk, tail or rm. The scripts/*.sh scripts need bash: on Windows that is Git
# Bash (not the WSL bash.exe in System32); override with BASH=<path>.

.DEFAULT_GOAL := help
empty :=

GO            ?= go
GOLANGCI_LINT ?= golangci-lint
PKGS          ?= ./...
# Extra flags for the test targets, e.g. make test TESTFLAGS=-v
TESTFLAGS     ?=
# The tag to verify with releasecheck, e.g. make releasecheck VERSION=v0.1.0
VERSION       ?= v0.1.0
COVERPROFILE  ?= coverage.out

ifeq ($(OS),Windows_NT)
BASH ?= "C:/Program Files/Git/bin/bash.exe"
else
BASH ?= bash
endif

.PHONY: help
help:
	$(info Usage: make <target> [PKGS=./...] [TESTFLAGS=...])
	$(info )
	$(info Everyday)
	$(info $(empty)  build          Build every package)
	$(info $(empty)  test           Run every test; TESTFLAGS adds flags)
	$(info $(empty)  race           Run every test under -race; needs cgo and a C compiler)
	$(info $(empty)  examples       Run the godoc examples)
	$(info $(empty)  vet            go vet for the host OS and for windows)
	$(info $(empty)  lint           golangci-lint; CI pins v2.14.0)
	$(info $(empty)  fmt            Format with the linter formatters: gofumpt, goimports)
	$(info $(empty)  tidy           go mod tidy)
	$(info $(empty)  tidy-check     Fail if go.mod or go.sum is not tidy)
	$(info $(empty)  cover          Test with coverage; the last line is the total)
	$(info $(empty)  cover-html     Open the coverage report in a browser)
	$(info )
	$(info Snapshots and golden data)
	$(info $(empty)  api            Refresh tests/testdata/api/aec.txt after an intended root API change)
	$(info $(empty)  api-check      Fail if the root API differs from tests/testdata/api/aec.txt)
	$(info $(empty)  written        Refresh the Go-written records in tests/testdata/written)
	$(info )
	$(info Opt-in suites)
	$(info $(empty)  racecheck      -race in a golang:1.25 container; needs Docker and bash)
	$(info $(empty)  docker-test    Docker sandbox tests against a real daemon)
	$(info $(empty)  live-test      Live web tests; DuckDuckGo rate-limits)
	$(info $(empty)  smoke          A clean module can require the SDK and build; needs bash)
	$(info )
	$(info CI and release)
	$(info $(empty)  check          Quick pre-push check: tidy-check build vet lint test)
	$(info $(empty)  ci             What the core-go CI job runs: adds smoke and race)
	$(info $(empty)  preflight      RELEASE.md pre-flight checks)
	$(info $(empty)  releasecheck   Verify a pushed tag: make releasecheck VERSION=vX.Y.Z)
	$(info $(empty)  clean          Clear the test cache)
	@:

# ---------------------------------------------------------------------------
# Everyday
# ---------------------------------------------------------------------------

.PHONY: build
build:
	$(GO) build $(PKGS)

.PHONY: test
test:
	$(GO) test -count=1 $(TESTFLAGS) $(PKGS)

.PHONY: race
race: export CGO_ENABLED = 1
race:
	$(GO) test -race -count=1 $(TESTFLAGS) $(PKGS)

.PHONY: examples
examples:
	$(GO) test -count=1 -run Example $(PKGS)

.PHONY: vet vet-host vet-windows
vet: vet-host vet-windows

vet-host:
	$(GO) vet $(PKGS)

vet-windows: export GOOS = windows
vet-windows:
	$(GO) vet $(PKGS)

.PHONY: lint
lint:
	$(GOLANGCI_LINT) run $(PKGS)

.PHONY: fmt
fmt:
	$(GOLANGCI_LINT) fmt $(PKGS)

.PHONY: tidy
tidy:
	$(GO) mod tidy

.PHONY: tidy-check
tidy-check:
	$(GO) mod tidy -diff

.PHONY: cover
cover:
	$(GO) test -count=1 -coverprofile=$(COVERPROFILE) $(PKGS)
	$(GO) tool cover -func=$(COVERPROFILE)

.PHONY: cover-html
cover-html: cover
	$(GO) tool cover -html=$(COVERPROFILE)

# ---------------------------------------------------------------------------
# Snapshots and golden data
# ---------------------------------------------------------------------------

.PHONY: api
api:
	$(GO) test -count=1 -run TestAPISnapshot -update .

.PHONY: api-check
api-check:
	$(GO) test -count=1 -run TestAPISnapshot .

.PHONY: written
written: export AEC_WIRE_UPDATE = 1
written:
	$(GO) test -count=1 -run TestGoWritten ./components/persistence/

# ---------------------------------------------------------------------------
# Opt-in suites
# ---------------------------------------------------------------------------

.PHONY: racecheck
racecheck:
	$(BASH) scripts/racecheck/racecheck.sh $(TESTFLAGS)

.PHONY: docker-test
docker-test: export AEC_DOCKER_TESTS = 1
docker-test:
	$(GO) test -count=1 -timeout 30m -v -run "TestDockerIntegration|TestSandboxViewDocker" ./components/sandboxhub/docker/... ./conformance/...

.PHONY: live-test
live-test: export RUN_LIVE_TESTS = 1
live-test:
	$(GO) test -count=1 -v -run Live ./components/toolhub/builtin/web

.PHONY: smoke
smoke:
	$(BASH) scripts/smoke/smoke.sh

# ---------------------------------------------------------------------------
# CI and release
# ---------------------------------------------------------------------------

.PHONY: check
check: tidy-check build vet lint test

.PHONY: ci
ci: tidy-check build vet lint smoke race

.PHONY: preflight
preflight: tidy-check vet test api-check racecheck
	$(BASH) scripts/releasecheck/releasecheck.sh --self-test

.PHONY: releasecheck
releasecheck:
	$(BASH) scripts/releasecheck/releasecheck.sh $(VERSION)

.PHONY: clean
clean:
	$(GO) clean -testcache
