# Releasing

## Release contents

Each release contains four binary archives and `SHA256SUMS`:

- Linux amd64
- Linux arm64
- macOS arm64
- Windows amd64

Restic is a separate dependency. The release does not contain a restic binary.
The build injects the release tag's version into `--version` and MCP capabilities.
Each archive includes license texts and third-party notices.

## Reproducibility gate

The release workflow calls `.github/workflows/reproducible.yml` before publishing.
That job builds the same commit twice in two detached worktrees with different
absolute checkout paths and separate Go build caches. It then compares every
platform archive and `SHA256SUMS` byte for byte. Any mismatch fails the job and
names the affected artifact, so publication does not start.

The comparison job also runs on pull requests that change packaging scripts,
Go module inputs, the command or internal packages, or the release workflows.
It runs the existing extracted-binary validation separately; identical bytes do
not prove that the executable runs correctly.

The controlled envelope is recorded in the job log and in local output:

- Source revision and `SOURCE_DATE_EPOCH` from the commit time.
- Go toolchain version, `go.sum` digest, and fixed build flags
  (`-buildvcs=true`, `-trimpath`, `-s -w`, `CGO_ENABLED=0`, version injection).
- Python version and compile/runtime zlib versions used by the archive writer.
- Separate build caches and different absolute checkout paths.

Reproduce locally:

```bash
revision="$(git rev-parse HEAD)"
epoch="$(git show -s --format=%ct "$revision")"
python3 scripts/verify_reproducible_builds.py \
  --revision "$revision" \
  --version 0.0.0 \
  --source-date-epoch "$epoch"
```

Byte identity is promised only inside this envelope. A different Go patch
release, module contents, Python/zlib build, compression implementation, or
build host is outside the comparison. Signature or attestation metadata added
later must stay outside the byte-for-byte artifact comparison. Keep logs and
diagnostic output local; no reproducibility report or trace is uploaded.

## Prepare the release

1. Update `CHANGELOG.md` with the release version and date.
2. Update compatibility and security documentation for behavior changes.
3. Run the required checks in [Testing](testing.md).
4. Keep logs and repeatable evidence outside the repository.
5. Commit the changes on a branch.
6. Open a pull request against `main`.
7. Wait for the required `ci` and `secrets` checks.
8. Merge the pull request.
9. Wait for the CI push run on the resulting `main` commit.

## Publish the release

Create an annotated `vMAJOR.MINOR.PATCH` tag on the tested commit.
For example:

```bash
git fetch origin main
git tag -a v0.1.0 origin/main -m 'Release v0.1.0'
git push origin v0.1.0
```

The tag starts the CI workflow. Its release job requires successful `ci` and `secrets` jobs.
The release workflow rejects tags whose commits are outside `main`.
The workflow builds the archives, calculates checksums, and publishes the release.
Release notes come from `CHANGELOG.md`.
Reports and test traces remain outside GitHub release assets.

## Confirm publication

1. Examine the release workflow result.
2. Make sure that the release tag identifies the tested commit.
3. Download the release assets.
4. Compare each archive's SHA-256 checksum with `SHA256SUMS`.
5. Extract the native archive. Run its binary with `--version`.
6. Run the executable integration suite with the extracted binary:

```bash
RESTIC_MCP_BINARY=/absolute/path/to/restic-defensive-mcp \
  GOMAXPROCS=2 GOFLAGS=-p=1 \
  go test -tags=integration -run TestExecutableStdioEndToEnd -count=1 -timeout 5m -v .
```

If publication fails, examine the failed step before another run.
Do not move or replace a published tag.
Use a new patch version for corrections to published source or binaries.
