# Releasing core/go

`github.com/Phong95/aec-agent-go` is developed in `core/go` of the private monorepo `github.com/Phong95/aec-agent` and published through the public, read-only mirror `github.com/Phong95/aec-agent-go`, which has the module at its root. This has three consequences:

- the `go-sdk-mirror` workflow (`.github/workflows/go-sdk-mirror.yml` in the monorepo) copies `core/go` to the mirror's `main` as one `Sync from Phong95/aec-agent@<sha>` commit on every push to `main` that touches `core/go`. No monorepo history is published. It needs the monorepo secret `GO_SDK_MIRROR_TOKEN`: a fine-grained token limited to `Phong95/aec-agent-go` with Contents read and write;
- the maintainer tags the monorepo with the path-prefixed `core/go/vX.Y.Z`, and the workflow republishes it on the mirror as the bare `vX.Y.Z` users fetch. A published version is immutable: the workflow refuses a version the mirror already has;
- the mirror's `.gitattributes` is generated from the monorepo's `core/go/` rules, so edit the root `.gitattributes` and never the mirror.

Committing, merging and tagging are the maintainer's job. `scripts/releasecheck/releasecheck.sh` only checks a tag after it is pushed; it never creates one.

## Checklist

1. **Bump the version.** Set `aec.Version` in `aec.go` to `X.Y.Z` in the commit the tag will point at.

2. **Run the pre-flight checks locally.** `make -C core/go preflight` runs all of them; each must be green:

   ```sh
   go -C core/go mod tidy -diff
   go -C core/go vet ./...
   go -C core/go test -count=1 ./...
   go -C core/go test -run TestAPISnapshot .              # root API matches tests/testdata/api/aec.txt
   bash core/go/scripts/racecheck/racecheck.sh              # -race in a golang:1.25 container
   bash core/go/scripts/releasecheck/releasecheck.sh --self-test
   ```

   - **Go `stable` leg of CI:** to reproduce it, run `RACECHECK_IMAGE=golang:1.27 bash core/go/scripts/racecheck/racecheck.sh` and `GOTOOLCHAIN=go1.27.1 go -C core/go test -count=1 ./...`. Use the Go version the latest CI log names.
   - **API changes:** if the root API changed on purpose, refresh the snapshot with `go -C core/go test -run TestAPISnapshot -update .` and review the diff in `tests/testdata/api/aec.txt`.
   - **`--self-test`:** it builds a throwaway git origin in a temp directory and checks that an annotated `core/go/v0.1.0` tag resolves while a bare `v0.1.0` fails. It needs network access for the public dependencies.

3. **Check that CI is green on the PR.** All of these must pass:
   - `core-go` on Go 1.25.x and on `stable`, including the external-consumer steps;
   - `core-go-windows`;
   - `core-go-docker`.

   golangci-lint runs at v2.14.0 in CI. A finding that shows up only in CI usually comes from a different local lint version.

4. **Commit everything the tag needs in one PR:**
   - `core/go`, with `go.mod`, `go.sum` and `testdata/`;
   - `.gitattributes`: its `-text` and `eol=lf` rules keep the embedded files, the fixtures and the `tools/` scripts byte-stable on a Windows checkout;
   - `.github/workflows/ci.yml` and `.github/workflows/go-sdk-mirror.yml`.

5. **Merge the PR to `main`.**

6. **Tag the merge commit on `main`.** Use an annotated, path-prefixed tag:

   ```sh
   git tag -a core/go/vX.Y.Z -m "core/go vX.Y.Z" <merge commit>
   git push origin core/go/vX.Y.Z
   ```

   Never tag a branch commit that is not merged: it would publish code `main` does not have. The mirror workflow refuses such a tag.

   Then wait for the `Go SDK mirror` run of the tag to finish: it pushes `vX.Y.Z` to `github.com/Phong95/aec-agent-go`.

7. **Verify the pushed tag** (or `make -C core/go releasecheck VERSION=vX.Y.Z`):

   ```sh
   bash core/go/scripts/releasecheck/releasecheck.sh vX.Y.Z
   ```

   - **Expected output:** `releasecheck: ok vX.Y.Z`.
   - **What it does:** it creates a clean temporary module and fetches the mirror's tag with `GOPRIVATE=github.com/Phong95/aec-agent-go`, `GOPROXY=direct` and a throwaway module cache. It then builds a program that imports the root, `kernel`, `llm`, `persistence`, `conformance` and `aecmock`. It requires `go list -m` to report the version, and `aec.Version` to match it.
   - **Credentials:** none: the mirror is public. It writes no token anywhere.
   - **pkg.go.dev:** the first `go get` through the public proxy lists the version there. To trigger it, run `GOPROXY=https://proxy.golang.org go list -m github.com/Phong95/aec-agent-go@vX.Y.Z`.

8. **Optional live check:** `RUN_LIVE_TESTS=1 go -C core/go test ./components/toolhub/builtin/web -run Live -v`. It is opt-in because DuckDuckGo has no API and rate-limits.

## Versioning

v0.x carries no API compatibility guarantee: any minor release may change the exported API, and `tests/testdata/api/aec.txt` shows exactly what changed. `server/go` pins exact versions (`require github.com/Phong95/aec-agent-go vX.Y.Z`). Read the API snapshot diff before you upgrade.

## If `releasecheck.sh` fails

- **`unknown revision vX.Y.Z`:** the mirror has no `vX.Y.Z`. Either `core/go/vX.Y.Z` was not pushed to the monorepo, or its `Go SDK mirror` run failed. Read that run's log: a tag not on `main`, a missing `GO_SDK_MIRROR_TOKEN`, or a version that already exists stops it.
- **A credentials or `terminal prompts disabled` error:** the mirror is not public, or its name changed. The module must be readable at `https://github.com/Phong95/aec-agent-go` without credentials.
- **`aec.Version is "..." in vX.Y.Z`:** the tagged commit carries an older `aec.Version`. Bump it on `main` and tag the next patch version.
- **The tag exists but the build fails:** the tagged commit is not the merge commit on `main`, or `go.sum` was not committed. Fix it on `main`, delete nothing that was published, and tag the next patch version.
