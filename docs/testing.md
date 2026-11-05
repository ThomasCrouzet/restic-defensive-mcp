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
The race detector requires a compatible C compiler and CGO support.

## Fuzzing

Normal tests always run each fuzz target's deterministic seed corpus. Pull-request
validation does not start random exploration, so unrelated changes keep the fast
path.

Three lanes exist:

- **Deterministic seeds:** `make test`, `make race`, and normal CI runs replay the
  checked-in seeds.
- **Manual exploration:** set the CI `fuzz` workflow input, or run
  `make fuzz-smoke` locally. `FUZZTIME` changes the per-target duration:
  `FUZZTIME=2m make fuzz-smoke`.
- **Automatic exploration:** `.github/workflows/fuzz.yml` runs weekly on a
  schedule and through manual dispatch. It runs one matrix job per target with a
  default 60-second per-target budget, a 15-minute job timeout, `GOMAXPROCS=2`,
  and at most two concurrent target jobs. Scheduled runs do not repeat push or
  pull-request validation.

The scheduled job records the source revision, Go version, package, target,
per-target budget, GOMAXPROCS, job timeout, and parallelism in its ordinary log.
A failure fails the workflow step. Go prints the failing target and input in the
job log; reproduce it locally with the same `go test ... -fuzz ...` command.
Minimize a meaningful discovery into a reviewed, non-sensitive regression seed.
Do not upload fuzz reports, corpus archives, traces, or transcripts to GitHub.

## Integration boundaries

The integration suite uses a temporary local restic repository with two backups.
Only `internal/testrepo` creates repositories and backups.
The fixture includes two data roots, small text files, and different snapshot tags.
Cleanup removes the temporary repository and its files.

The suite covers two boundaries:

- In-memory MCP calls use real restic commands.
- Executable tests build the server, load YAML, and exchange MCP messages through stdio.

Executable tests cover mixed and nested visibility policies with real
repositories: mixed visible/hidden roots, snapshot roots broader than the
allowlist, allowlisted parents broader than backed-up children, nested and
overlapping allowlists, `/`, disjoint roots, hidden hosts and tags, scoped
`latest`, and whole-snapshot statistics restrictions.
They also cover startup, version output, all seven tools, and nonempty metadata results.
They also cover path denial, pagination, empty results, credential restrictions, and the separation of stdout and stderr.
Configuration diagnostics are exercised through the executable for valid,
invalid, permission-failing, unreachable, and machine-readable results, including
the guarantee that local validation does not contact a configured backend.
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
