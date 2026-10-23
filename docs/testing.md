# Testing

## Required local checks

Install a supported Go toolchain and restic 0.17.1 or later.
Run these commands from the repository root:

```bash
GOMAXPROCS=2 go test -p 1 -race -count=1 ./...
GOMAXPROCS=2 GOFLAGS=-p=1 make integration
make lint
make vet
make build
```

Run `make fuzz-smoke` after parser, path policy, or redaction changes.
The CI workflow also provides a manual fuzz input.
The race detector requires a compatible C compiler and CGO support.

## Integration boundaries

The integration suite uses a temporary local restic repository with two backups.
Only `internal/testrepo` creates repositories and backups.
The fixture includes two data roots, small text files, and different snapshot tags.
Cleanup removes the temporary repository and its files.

The suite covers two boundaries:

- In-memory MCP calls use real restic commands.
- Executable tests build the server, load YAML, and exchange MCP messages through stdio.

Executable tests cover startup, version output, all seven tools, and nonempty metadata results.
They also cover path denial, pagination, empty results, credential restrictions, and the separation of stdout and stderr.
Repository location changes cannot retarget a running server.
Password replacement changes authentication results without changing the configured repository.
Snapshot IDs remain unchanged after inspection.

These tests do not restore file contents or exercise remote storage services.
Remote backend credentials, TLS, and network behavior require deployment-specific checks.
Cross-compilation proves that release targets build. It does not prove runtime behavior on every target system.

## Local evidence

Keep test reports outside the repository.
Each executable run records its artifact directory in the test log.
Set `RESTIC_MCP_ARTIFACT_DIR` to select a local evidence directory.
Keep the test log together with the protocol transcript and run metadata.
Set `RESTIC_MCP_BINARY` to use an existing binary instead of building the executable.
This option permits the same protocol checks against a downloaded release.

For a complete local log, use:

```bash
set -o pipefail
artifact_dir="$(mktemp -d "${TMPDIR:-/tmp}/restic-defensive-evidence.XXXXXX")"
RESTIC_MCP_ARTIFACT_DIR="$artifact_dir" \
  RESTIC_MCP_E2E_COMMAND='GOMAXPROCS=2 GOFLAGS=-p=1 make integration' \
  GOMAXPROCS=2 GOFLAGS=-p=1 make integration 2>&1 | tee "$artifact_dir/integration.log"
```

Required run metadata includes:

- The command and result.
- The source revision and working diff identity, including untracked source files.
- Go, restic, and operating system versions.
- Fixture contents, snapshot identities, and cleanup behavior.

Do not upload test reports, traces, or SARIF files to GitHub.
Release binaries, checksums, and license notices are separate release deliverables.

## Isolated failure checks

The normal test suite retains checks for failures that the real repository fixture cannot reliably produce:

- Ambiguous snapshot prefixes and mixed policy projections.
- Malformed restic responses and command injection.
- Sealed environments, secret-file restrictions, and audit redaction.
- Hostile child output, cancellation, and process termination.
- Saturated repository or global concurrency limits.

Record identified failure modes before isolated tests or implementation changes.
Write necessary tests before the implementation.
Do not add tests solely for coverage percentages, dependency behavior, or mocked call sequences.

## CI coverage

The required `ci` job runs formatting, lint, race, integration, vulnerability, module, and cross-compilation checks.
Integration runs use restic 0.17.1 and 0.19.1.
The `secrets` job scans Git history with redacted output.
The release job requires successful `ci` and `secrets` jobs for the tag.
The tagged commit must also belong to `main`.
