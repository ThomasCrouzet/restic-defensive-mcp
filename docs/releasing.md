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
