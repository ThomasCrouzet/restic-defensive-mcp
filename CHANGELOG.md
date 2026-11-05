# Changelog

All notable changes to this project are documented in this file.

## [Unreleased]

### Added

- Publish typed output schemas for all seven tools, return matching structured
  content alongside the text compatibility envelope, and split the MCP server
  into registration, contract, selection, and handler components.

- Add `--validate-config` for local configuration diagnostics and `--diagnose`
  for explicitly requested bounded repository connectivity checks. Both support
  human-readable and `--json` output without exposing secrets or backend URLs.

### Changed

- Add native macOS arm64 runtime validation for the executable stdio and
  cancellation suites, including release-archive validation.

- Verify release archives by building the same revision twice with separate
  worktrees and build caches, then comparing every archive and `SHA256SUMS`
  byte for byte before publication.

- Add a separate weekly bounded fuzz exploration workflow with per-target and
  whole-job budgets while keeping deterministic seeds in normal CI.

### Fixed

- Cancel Unix restic inspections with a graceful `SIGTERM`, a bounded cleanup
  window, and process-group `SIGKILL` fallback so normal lock cleanup can run.
- Populate `find_files` match names from validated POSIX snapshot paths when
  restic omits the upstream `name` field.
- Select snapshots when an allowlisted parent contains snapshot roots, including
  parent/child, equal, nested, and `/` scopes, without hiding visible concrete
  snapshot IDs from `find_files`.

## [0.1.0]: 2026-10-23

### Added

- Add bounded `after`/`before` snapshot-creation windows to listing, retrieval,
  browsing, search, and statistics; intervals are normalized to UTC and use
  `[after, before)`.

- Add bounded, single-use continuation cursors for snapshot listing, directory
  browsing, and file searches. Hidden entries are filtered before paging, and
  removed snapshot selections fail with `stale_selection`.

- Structurally read-only stdio MCP server with seven inspection tools.
- Sealed YAML repository registry with host, tag, and path policies.
- Local, S3, B2, and REST backend support; SFTP and rclone are rejected.
- Scrubbed child environments, file-based secrets, timeouts, concurrency
  limits, process-group cancellation, and bounded output.
- Snapshot authorization before content or statistics access.
- Structured errors, field-safe redaction, and non-sensitive audit events.
- Executable stdio tests with real restic and local protocol evidence.
- Regression, race, fuzz, process lifecycle, and multi-platform build checks.
- Compatibility checks for restic 0.17.1 and 0.19.1.
- Release archives for Linux amd64/arm64, macOS arm64, and Windows amd64.
- SHA-256 checksums and complete dependency license notices in release deliverables.

### Fixed

- Cancel Unix restic inspections with a graceful `SIGTERM`, a bounded cleanup
  window, and process-group `SIGKILL` fallback so normal lock cleanup can run.
- Reject AWS profiles and shared credential files that can execute credential helpers.
- Require static S3 credentials to prevent fallback to host instance credentials.
- Reject saturated command capacity with `resource_busy` instead of queuing requests.
- Omit unknown repository identifiers from audit events.
- Keep Unix process-group cancellation active after the group leader exits.
- Terminate descendants when inherited output pipes exceed the wait limit.
- Reject statistics responses that omit required size data.
- Omit child stderr from generic errors to prevent backend location disclosure.

### Changed

- Expand executable coverage for mixed and nested visibility policies,
  including hidden hosts/tags and whole-snapshot statistics restrictions.

- Require Go 1.26 or later. Use a current patch release of a supported Go version.
- Build CI and release binaries with Go 1.27.3.
- Update lint, vulnerability scanning, and cache tooling.
- Preserve external Go build flags and enable the race detector's CGO requirements.
- Document password rotation, credential restrictions, test boundaries, and release gates.
