#!/usr/bin/env bash
# Release check for the core/go module (Phase 12, REL-04, CONTEXT RM-1 and RM-3).
#
# What it verifies: that a clean module, with nothing but the network, can
# `go get github.com/Phong95/aec-agent-go@<version>`
# and build a program that imports the root, kernel, llm, persistence, conformance
# and aecmock packages, that `go list -m` reports exactly that version, and that
# aec.Version of the tagged code equals the tag's version (a stale constant fails).
#
# It is verification only. It never tags, never pushes and never writes to the
# project repository. The tag is the maintainer's step (see core/go/RELEASE.md).
#
# The module is served by the public mirror repository github.com/Phong95/aec-agent-go,
# which the go-sdk-mirror workflow fills from the monorepo's core/go. The fetch goes
# straight to GitHub: GOPRIVATE keeps the module path away from the public proxy and
# the checksum database, and GOPROXY=direct skips the proxy, so a cached proxy copy
# can never mask a broken tag. Nothing secret is stored in or read by this script.
#
# Usage:
#   releasecheck.sh [vX.Y.Z]               default v0.1.0; run it after
#                                          `git push origin core/go/<version>` and
#                                          the mirror workflow has pushed <version>
#   releasecheck.sh --self-test [vX.Y.Z]   prove the script and the tag rule without a
#                                          real tag, against a throwaway git origin
#
# In the monorepo the tag is path-prefixed (core/go/vX.Y.Z); the mirror workflow
# republishes it as a bare vX.Y.Z, because the module sits at the mirror's root. A
# path-prefixed tag on the mirror does not version the module, and `go get` reports
# "unknown revision vX.Y.Z".
#
# --self-test builds the origin inside its own mktemp directory from the working
# tree's core/go laid out as the mirror's root, commits and tags there (an isolated
# git config, never the project's), points github.com/Phong95/aec-agent-go at that
# directory through an insteadOf rule, and runs the same check three times: an
# annotated bare <version> tag must resolve and build, a path-prefixed
# core/go/<version> tag must fail with the hint, and a tag
# whose aec.Version does not equal the version must fail. Every
# git call goes through tgit, which refuses any directory outside the temp
# directory, and nothing is ever pushed. The public dependencies of core/go still
# come from the network, through a throwaway module cache.
#
# Exit status: 0 when the check passes, 1 when it fails, 2 on a usage error or a
# refused git write.
set -euo pipefail

MODULE_PATH="github.com/Phong95/aec-agent-go"
REPO_URL_PREFIX="https://github.com/Phong95/aec-agent-go"
DEFAULT_VERSION="v0.1.0"

# The module directory is this script's ../.. ; keep the POSIX form for cp and the
# Windows form (when available, Git Bash) for anything a native Go or git reads.
MODULE_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"

usage() {
  echo "usage: releasecheck.sh [vX.Y.Z] | --self-test [vX.Y.Z]" >&2
  exit 2
}

SELF_TEST=0
VERSION=""
for arg in "$@"; do
  case "$arg" in
    --self-test) SELF_TEST=1 ;;
    v[0-9]*) VERSION="$arg" ;;
    *) usage ;;
  esac
done
VERSION="${VERSION:-$DEFAULT_VERSION}"

# A dependency needing a newer Go must fail here, not auto-download a toolchain.
export GOTOOLCHAIN=local

WORK_DIR="$(mktemp -d)"
trap 'rm -rf "$WORK_DIR"' EXIT

# native prints a directory in the form a native Go or git expects (Windows form
# under Git Bash, the plain path elsewhere).
native() {
  (cd "$1" && { pwd -W 2>/dev/null || pwd; })
}

hint() {
  echo "releasecheck: hint: the mirror github.com/Phong95/aec-agent-go must carry the bare tag $VERSION; push core/go/$VERSION (annotated, on the merge commit on main) to the monorepo and check that the go-sdk-mirror workflow republished it" >&2
}

# run_check resolves $MODULE_PATH@$1 from a clean module. $2 is a fresh scratch
# directory that holds the module and its own module cache, so a cached copy from
# an earlier run can never mask a broken tag. $3 (optional) is a git config file
# that redirects the repository, used by the self-test only. It prints the go
# output; on failure it prints the hint and returns 1 (never relies on set -e,
# because callers use it in an || list).
run_check() {
  local version="$1" dir="$2" gitcfg="${3:-}"
  mkdir -p "$dir/mod" "$dir/gomodcache"
  local cache
  cache="$(native "$dir/gomodcache")"
  (
    cd "$dir/mod"
    export GOPRIVATE=github.com/Phong95/aec-agent-go
    export GOPROXY=direct
    export GOFLAGS="-mod=mod -modcacherw"
    export GOMODCACHE="$cache"
    export GIT_TERMINAL_PROMPT=0
    if [ -n "$gitcfg" ]; then
      export GIT_CONFIG_GLOBAL="$gitcfg"
      export GIT_CONFIG_NOSYSTEM=1
    fi

    go mod init example.com/releasecheck || exit 1
    # go mod init writes the local toolchain version; the floor claim is 1.25.0.
    go mod edit -go=1.25.0 || exit 1

    printf '%s\n' \
      'package main' \
      '' \
      'import (' \
      '	"fmt"' \
      '' \
      '	aec "github.com/Phong95/aec-agent-go"' \
      '	"github.com/Phong95/aec-agent-go/aecmock"' \
      '	"github.com/Phong95/aec-agent-go/conformance"' \
      '	"github.com/Phong95/aec-agent-go/kernel"' \
      '	"github.com/Phong95/aec-agent-go/components/llmhub"' \
      '	"github.com/Phong95/aec-agent-go/components/persistence"' \
      ')' \
      '' \
      'func main() {' \
      '	_ = kernel.NewID()' \
      '	_ = llmhub.ListProviders()' \
      '	_ = persistence.NewPersistenceHub()' \
      '	_ = conformance.SessionStore' \
      '	_ = aecmock.NewScriptedProvider' \
      '	fmt.Println(aec.Version)' \
      '}' > main.go

    # `go mod download` takes the module path exactly, so a missing bare tag on
    # the mirror fails with "unknown revision <version>". `go get` alone could
    # report it less directly, through the package it fails to find.
    go mod download "$MODULE_PATH@$version" || exit 1
    go get "$MODULE_PATH@$version" || exit 1
    go mod tidy || exit 1
    go build ./... || exit 1

    got="$(go list -m -f '{{.Version}}' "$MODULE_PATH")" || exit 1
    if [ "$got" != "$version" ]; then
      echo "releasecheck: go list -m reports $got, want $version" >&2
      exit 1
    fi

    # The Version constant of the code the tag points at must equal the tag's
    # version without the leading v (RELEASE.md steps 3 and 8). A failing run or an
    # empty value is a failure too: the comparison must never be skipped silently.
    sdk_version="$(go run .)" || {
      echo "releasecheck: go run of the probe program failed, so aec.Version could not be read" >&2
      exit 1
    }
    if [ "$sdk_version" != "${version#v}" ]; then
      echo "releasecheck: aec.Version is \"$sdk_version\" in $version, want \"${version#v}\"; bump aec.Version in the commit the tag points at" >&2
      exit 1
    fi
  ) && return 0
  hint
  return 1
}

if [ "$SELF_TEST" -eq 0 ]; then
  run_check "$VERSION" "$WORK_DIR/check"
  echo "releasecheck: ok $VERSION"
  exit 0
fi

# ---- self-test ---------------------------------------------------------------

# tgit is the only way this script runs git. It refuses any directory that is not
# inside this run's own temp directory (exit 2), and it uses an isolated config.
tgit() {
  local dir="$1"
  shift
  case "$dir" in
    *..*) echo "releasecheck: refusing git in $dir" >&2; exit 2 ;;
    "$WORK_DIR"/*) ;;
    *) echo "releasecheck: refusing git outside the temp directory: $dir" >&2; exit 2 ;;
  esac
  GIT_CONFIG_GLOBAL="$WORK_DIR/gitconfig-base" GIT_CONFIG_NOSYSTEM=1 \
    git -C "$dir" -c user.name=releasecheck -c user.email=releasecheck@invalid "$@"
}

# The base config is isolated from the user's: no autocrlf conversion (the embedded
# files are byte-compared), no signing, no credential helper.
printf '%s\n' \
  '[core]' \
  '	autocrlf = false' \
  '	longpaths = true' \
  '[commit]' \
  '	gpgsign = false' \
  '[tag]' \
  '	gpgsign = false' \
  '[safe]' \
  '	directory = *' \
  '[init]' \
  '	defaultBranch = main' > "$WORK_DIR/gitconfig-base"

# make_origin <name> <tag> <sdk-version> builds a throwaway origin whose only content
# is the working tree's core/go at the root, as the mirror lays it out, committed once
# and tagged with <tag> (annotated).
# The snapshot's aec.Version constant is set to <sdk-version> first, so the self-test
# controls what the Version check sees whatever the working tree says (the edit is to
# the copy inside the temp directory only).
make_origin() {
  local name="$1" tag="$2" sdk="$3" origin="$WORK_DIR/origin-$1"
  mkdir -p "$origin"
  cp -R "$MODULE_DIR/." "$origin/"
  sed -i.bak "s/^const Version = \".*\"/const Version = \"$sdk\"/" "$origin/aec.go"
  rm -f "$origin/aec.go.bak"
  if ! grep -q "^const Version = \"$sdk\"" "$origin/aec.go"; then
    echo "releasecheck: self-test: could not set aec.Version in the snapshot" >&2
    exit 1
  fi
  tgit "$origin" init -q
  tgit "$origin" add -A
  tgit "$origin" commit -q -m "core/go snapshot for the release self-test"
  tgit "$origin" tag -a "$tag" -m "$tag"
  # The insteadOf rule sends https://github.com/Phong95/aec-agent-go to this origin.
  local url
  url="$(native "$origin")"
  case "$url" in
    /*) url="file://$url" ;;
    *) url="file:///$url" ;;
  esac
  cp "$WORK_DIR/gitconfig-base" "$WORK_DIR/gitconfig-$name"
  printf '%s\n' \
    "[url \"$url\"]" \
    "	insteadOf = $REPO_URL_PREFIX" >> "$WORK_DIR/gitconfig-$name"
}

echo "releasecheck: self-test: annotated tag $VERSION"
make_origin ok "$VERSION" "${VERSION#v}"
out_ok="$WORK_DIR/out-ok.txt"
if ! run_check "$VERSION" "$WORK_DIR/run-ok" "$WORK_DIR/gitconfig-ok" > "$out_ok" 2>&1; then
  cat "$out_ok" >&2
  echo "releasecheck: self-test failed: the annotated $VERSION tag did not resolve and build" >&2
  exit 1
fi
echo "releasecheck: ok $VERSION"

echo "releasecheck: self-test: path-prefixed tag core/go/$VERSION must fail"
make_origin prefixed "core/go/$VERSION" "${VERSION#v}"
out_prefixed="$WORK_DIR/out-prefixed.txt"
if run_check "$VERSION" "$WORK_DIR/run-prefixed" "$WORK_DIR/gitconfig-prefixed" > "$out_prefixed" 2>&1; then
  cat "$out_prefixed" >&2
  echo "releasecheck: self-test failed: a path-prefixed core/go/$VERSION tag was accepted on the mirror" >&2
  exit 1
fi
if ! grep -q "unknown revision $VERSION" "$out_prefixed"; then
  cat "$out_prefixed" >&2
  echo "releasecheck: self-test failed: the path-prefixed-tag failure does not say 'unknown revision $VERSION'" >&2
  exit 1
fi
if ! grep -q "must carry the bare tag $VERSION" "$out_prefixed"; then
  cat "$out_prefixed" >&2
  echo "releasecheck: self-test failed: the path-prefixed-tag failure lacks the bare tag hint" >&2
  exit 1
fi
grep "unknown revision" "$out_prefixed" | head -n 1
echo "releasecheck: path-prefixed tag failed as expected"

echo "releasecheck: self-test: a stale aec.Version must fail"
make_origin stale "$VERSION" "0.0.0-stale"
out_stale="$WORK_DIR/out-stale.txt"
if run_check "$VERSION" "$WORK_DIR/run-stale" "$WORK_DIR/gitconfig-stale" > "$out_stale" 2>&1; then
  cat "$out_stale" >&2
  echo "releasecheck: self-test failed: a tag whose aec.Version is stale was accepted" >&2
  exit 1
fi
if ! grep -q 'aec.Version is "0.0.0-stale"' "$out_stale"; then
  cat "$out_stale" >&2
  echo "releasecheck: self-test failed: the stale-version failure does not name aec.Version" >&2
  exit 1
fi
grep 'aec.Version is' "$out_stale" | head -n 1
echo "releasecheck: stale aec.Version failed as expected"

echo "releasecheck: self-test ok"
