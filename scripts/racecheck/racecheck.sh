#!/usr/bin/env bash
# Local race-detector runner (Phase 12, REL-04, CONTEXT RL-2).
#
# Why this exists: the race detector needs cgo, and this Windows host's TDM-GCC
# cannot build runtime/cgo, so `go test -race` does not work natively here. Local
# race runs happen in a Linux container (golang:1.25, gcc, CGO_ENABLED=1) instead.
# The core-go job in .github/workflows/ci.yml stays the authority; this script only
# lets a developer see the same result before pushing.
#
# Usage:
#   racecheck.sh [go test flags and packages]   default: -count=1 ./...
#   racecheck.sh -count=5 -shuffle=on -timeout 40m ./...
#   racecheck.sh --self-test                    prove the detector can see a race
#   RACECHECK_IMAGE=golang:1.27 racecheck.sh    the run that reproduces the `stable` leg
#                                               of the core-go job (see Environment)
#
# What it runs: `go test -race <your arguments>` on a copy of core/go.
#
# Environment:
#   RACECHECK_IMAGE          container image (default golang:1.25). Use golang:1.27 to
#                            reproduce the `stable` leg of the core-go job: setup-go resolved
#                            go1.27.1 in CI run 37125492889 (gap G-12-1). `stable` moves, so
#                            the image tag should follow the version CI resolves. The shared
#                            aec-go-build cache volume is safe across Go releases (the Go
#                            build cache is keyed by toolchain).
#   RACECHECK_LOG            file that receives the full output (default: a mktemp file, path printed)
#
# Typical wall time on a warm cache (Docker Desktop, Windows host): about 30 s for the
# single CI-equivalent pass, about 100 s for `-count=5 -shuffle=on ./...`; the slowest
# packages are sandbox/host, conformance and llm/openaisub (60 to 80 s at count=5).
# `-shuffle=on` seeds are only printed for failing packages (or with -v); to replay a
# failure run `racecheck.sh -run '<Test>' -shuffle=<seed> -count=1 ./<pkg>`.
#
# What it never does: it never mounts the Docker socket, never sets AEC_DOCKER_TESTS
# or RUN_LIVE_TESTS in the container (so the Docker-backed and live-API tests skip),
# never writes to the repository (the tree is bind-mounted read-only and copied to
# /work inside the container), and never touches your other containers (the run is
# `docker run --rm`). The named volumes aec-go-mod and aec-go-build only cache
# modules and build output between runs.
#
# Exit status: non-zero when go test failed, or when the log holds a
# "WARNING: DATA RACE", a goleak "found unexpected goroutines" report or a
# "--- FAIL" line. "racecheck: ok" is printed only otherwise.
set -euo pipefail

# --- inside the container -------------------------------------------------
# The script runs itself in the container (from the read-only /src mount), so the
# container logic lives in one file and needs no shell quoting.
if [ "${RACECHECK_INSIDE:-}" = "1" ]; then
  mode="run"
  if [ "${1:-}" = "--self-test" ]; then
    mode="self-test"
    shift
  fi

  if [ "$mode" = "self-test" ]; then
    # A throwaway module in the container's own temp dir: never /work, never the host.
    probe="$(mktemp -d)"
    cd "$probe"
    go mod init racecheckprobe >/dev/null 2>&1
    printf '%s\n' \
      'package probe' \
      '' \
      'import (' \
      '	"sync"' \
      '	"testing"' \
      ')' \
      '' \
      '// TestRace has two goroutines write one variable with no synchronisation.' \
      'func TestRace(t *testing.T) {' \
      '	var x int' \
      '	var wg sync.WaitGroup' \
      '	wg.Add(2)' \
      '	go func() { defer wg.Done(); x++ }()' \
      '	go func() { defer wg.Done(); x++ }()' \
      '	wg.Wait()' \
      '	_ = x' \
      '}' > probe_test.go
    echo "== go version"
    go version
    echo "== CGO_ENABLED=$(go env CGO_ENABLED)"
    set +e
    out="$(go test -race -count=1 ./... 2>&1)"
    rc=$?
    set -e
    printf '%s\n' "$out"
    if [ "$rc" -ne 0 ] && printf '%s' "$out" | grep -q 'WARNING: DATA RACE'; then
      echo "racecheck: self-test ok"
      exit 0
    fi
    echo "racecheck: self-test FAILED: the probe race was not detected (exit $rc); a race run that cannot see a race proves nothing" >&2
    exit 1
  fi

  cp -a /src/. /work
  cd /work
  echo "== go version"
  go version
  echo "== CGO_ENABLED=$(go env CGO_ENABLED)"

  if [ "$#" -eq 0 ]; then
    set -- -count=1 ./...
  fi

  echo "== go test -race $*"
  set +e
  go test -race "$@"
  root_rc=$?
  set -e

  echo "RACECHECK_STATUS root=$root_rc"
  if [ "$root_rc" -ne 0 ]; then
    exit 1
  fi
  exit 0
fi

# --- on the host ----------------------------------------------------------
# The module directory is this script's ../.. ; print it in Windows form when
# available (Git Bash) because docker needs a native path in -v.
MODULE_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && { pwd -W 2>/dev/null || pwd; })"
IMAGE="${RACECHECK_IMAGE:-golang:1.25}"

LOG="${RACECHECK_LOG:-$(mktemp)}"
echo "racecheck: log file: $LOG"
: > "$LOG"

# MSYS_NO_PATHCONV stops Git Bash rewriting /src, /work and the volume targets.
# GOTOOLCHAIN=local: a dependency needing a newer Go must fail, not auto-download.
set +e
MSYS_NO_PATHCONV=1 docker run --rm \
  -v "${MODULE_DIR}:/src:ro" \
  -v aec-go-mod:/go/pkg/mod \
  -v aec-go-build:/root/.cache/go-build \
  -w /work \
  -e GOTOOLCHAIN=local \
  -e RACECHECK_INSIDE=1 \
  "$IMAGE" \
  bash /src/scripts/racecheck/racecheck.sh "$@" 2>&1 | tee "$LOG"
docker_rc="${PIPESTATUS[0]}"
set -e

if [ "${1:-}" = "--self-test" ]; then
  # The inside run already decided; its exit status is the verdict.
  exit "$docker_rc"
fi

count() { grep -c -e "$1" "$LOG" || true; }
races="$(count 'WARNING: DATA RACE')"
leaks="$(count 'found unexpected goroutines')"
fails="$(count '--- FAIL')"

echo
echo "racecheck: summary"
echo "  WARNING: DATA RACE:         $races"
echo "  unexpected goroutines:      $leaks"
echo "  --- FAIL lines:             $fails"
echo "  FAIL package lines:"
grep -E '^FAIL' "$LOG" | sed 's/^/    /' || true
echo "  shuffle seeds:"
grep -E -- '-test\.shuffle' "$LOG" | sort -u | sed 's/^/    /' || true
grep -E '^RACECHECK_STATUS' "$LOG" | sed 's/^/  /' || true

if [ "$docker_rc" -ne 0 ] || [ "$races" -ne 0 ] || [ "$leaks" -ne 0 ] || [ "$fails" -ne 0 ]; then
  echo "racecheck: FAILED (exit $docker_rc)" >&2
  exit 1
fi
echo "racecheck: ok"
