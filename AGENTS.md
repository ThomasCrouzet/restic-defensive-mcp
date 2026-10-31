# restic-defensive-mcp

## What it is

Structurally read-only stdio MCP server for inspecting restic repositories.
It exposes seven tools for snapshots, file metadata, path search, and statistics.
Callers pass a configured repository ID, never a repository URL.
Version 0.1.0, MIT, public repository, maintained.

## Stack

- Go 1.26.0 module directive; CI builds with Go 1.27.3 and smoke builds 1.26.10.
- restic 0.17.1 or later on `PATH` or via `restic_binary`; CI covers 0.17.1 and 0.19.1.
- Modules: `github.com/modelcontextprotocol/go-sdk` v1.8.0, `gopkg.in/yaml.v3` v3.0.1. No database or sidecar service.
- Transport is MCP over stdio. stdout carries JSON-RPC; logs and audit events go to stderr.

## Verified commands

Proven with `make -n`; cross-checked in `docs/testing.md`. Run from the repository root.

- `make build` writes `bin/restic-defensive-mcp` with `CGO_ENABLED=0`.
- `make test`, `make race`, `make vet`, `make fmt`, `make clean`.
- `make lint` runs golangci-lint v2.15.0 via `go run`; the first run needs network access.
- `make fuzz-smoke` runs six fuzz targets, five seconds each by default.
- `make integration` needs a real restic 0.17.1+ binary on `PATH`. It covers in-memory and stdio MCP boundaries against a temporary repository with two backups and no file restore.
- `make demo` runs only `TestExecutableStdioEndToEnd`; it needs restic.
- `GOMAXPROCS=2 go test -p 1 -race -count=1 ./...` is the full local regression check; no restic, no secrets.
- `./bin/restic-defensive-mcp --version` prints the build version.
- `./bin/restic-defensive-mcp --config <path>` starts the server; needs a config, restic, and readable secret files.

The integration fixture is local and temporary.

## Layout

- `cmd/restic-defensive-mcp/main.go`: flags, boot probes, stdio server start.
- `internal/restic/`: argv builders, exec runner, JSON parsers, backend classification.
- `internal/policy/`: host, tag, and path allowlists.
- `internal/repositories/`: sealed registry, child environment, concurrency slots.
- `internal/mcpserver/`: the seven MCP tools and the response envelope.
- `internal/config/`: YAML loading, runtime limits, secret file checks.
- `internal/audit/` and `internal/redaction/`: stderr audit events and secret scrubbing.
- `internal/testrepo/`: the only code allowed to run `restic init` or `restic backup`; tests only.
- `docs/`: tool contract, testing, releasing, restic compatibility.
- Generated: `bin/` and `dist/` are gitignored; do not edit or commit them.

## Invariants

- Keep the production subcommand set closed to `version`, `snapshots`, `ls`, `find`, `stats`; `AssertArgvAllowed` rechecks every argv.
- Never add `restore`, `forget`, `prune`, `unlock`, `check`, or a free-form argv tool; there is no mutation path.
- Never add a shell. Build argv slices and call restic directly; a shell would reopen injection.
- Production code must not call `init` or `backup`; those live only in `internal/testrepo`.
- Build the child environment from scratch; inherited `RESTIC_*` or cloud credentials would leak secrets.
- Accept only allowlisted env keys; `AWS_PROFILE` and `AWS_SHARED_CREDENTIALS_FILE` stay rejected because profiles can execute helpers.
- Take repository location and credentials from config, not MCP arguments; callers send only `repository_id`.
- Keep the repository location sealed at boot; editing `repository_file` must not retarget a running server, so restart after changes.
- Keep the password path fixed and file-based; restic rereads it per command, so rotation needs no restart.
- Require secret files to be regular, non-symlink, and mode 0600 or stricter on Unix; looser files expose the password.
- Keep path checks lexical and absolute, rejecting `..`, backslashes, and control characters; no host path or symlink resolution.
- Treat limits as hard bounds; values outside the allowed range fail at boot instead of being clamped.
- Reject over-capacity calls with `resource_busy`; queuing would turn concurrency into unbounded latency and memory.
- Never return file contents, repository URLs, or secret paths; sanitize metadata and redact child stderr before it reaches clients.
- Keep audit events free of full paths and secrets; log policy denials before any restic call.
- Keep `repository_stats` as a size and count query, never as an integrity check or a `check --read-data` claim.
- Never aggregate statistics across snapshots with paths outside `allowed_paths`; `repository_stats` drops them and fails if none remain.
- Keep empty allowlists meaning no restriction, with the boot `empty_allowlist` warning; silence would hide exposure.
- Accept only 64-hex or an 8+ character prefix for snapshot IDs; `latest` needs filters and `find`, `ls`, `stats` reject it.

## Pitfalls

- `make integration` fails on a missing binary: the suite runs real restic. Install 0.17.1+ or set `restic_binary`; `make test` alone does not need restic.
- Boot exits 2 with `unsupported_restic_version`: the version probe found an older restic; upgrade before debugging anything else.
- Boot exits 2 with secret file errors: the file is a symlink, not regular, empty, oversized, or group/world readable. Use a plain 0600 file.
- `empty_allowlist` warning at boot: an empty list means no restriction for that dimension. Set explicit hosts, tags, and paths.
- `resource_busy` under load: one command per repository and four globally by default, with no queue. Retry after a delay.
- `latest` rejected: `find`, `ls`, and `stats` need concrete IDs; `latest` elsewhere needs host, tag, or path filters.
- Repository unchanged after editing `repository_file`: the registry sealed it at boot; restart the server.
- Authentication changes right after editing the password file: restic reads it per command. This is intended.
- `./bin/restic-defensive-mcp` fails with "Exec format error": the gitignored `bin/` holds a stale macOS arm64 binary. Run `make build`.
- `go` 1.27.3 is on `PATH` here (2026-10-30); `go build`, `go vet` and `go test ./...` pass.

## Conventions

- Write code, comments, docs, and commits in English.
- Read `CONTRIBUTING.md` and `SECURITY.md` before source changes.
- Branch from `main`; keep commits atomic and messages imperative, for example "Add path allowlist fuzz".
- Open a pull request against `main` with tests for behavior changes.
- Run gofmt, lint, race, and integration before requesting review.
- Security-sensitive changes (argv, child environment, backend classification, path policy, secret files) need documented failure modes, E2E checks, and an explicit PR note. Never weaken an allowlist to make a test pass.
- Test policy: never write unit tests after the code; prefer E2E as the sole mechanism and verify through observable results.
- Every E2E run yields a repeatable artifact: command, source revision, environment, fixtures, results, and the working diff identity when dirty.
- Inspect assertions, fixtures, mocks, and skips before deleting a test; disabled E2E tests and simulated boundaries are not coverage.
- Document failure modes first; keep an isolated test only for a concrete failure E2E cannot detect.
- No tests for coverage percentages, type contracts, dependency behavior, or mocked call sequences alone.
- Keep isolated checks for policy projection, ambiguous snapshots, command injection, environment sealing, output limits, audit redaction, and malformed responses.
- The fixture removes its temporary repository after the run; save the log, restic version, fixture, and run metadata outside the repository.
- CI `ci` job: gofmt, lint, race, integration on restic 0.17.1 and 0.19.1, govulncheck, `go mod tidy` diff, four cross-builds.
- CI `secrets` job runs gitleaks over the full history with redacted output.
- Release: tag `v*` on a commit in `main` after `ci` and `secrets` pass; follow `docs/releasing.md` and `scripts/build_release.py`.
- Never upload test reports, traces, or SARIF files to GitHub; publish only archives, `SHA256SUMS`, license texts, and notices.
