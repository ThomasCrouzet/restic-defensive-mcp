# Changelog

All notable changes to this project are documented in this file.

## [0.1.0]: 2026-10-23

### Added

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

- Reject AWS profiles and shared credential files that can execute credential helpers.
- Require static S3 credentials to prevent fallback to host instance credentials.
- Reject saturated command capacity with `resource_busy` instead of queuing requests.
- Omit unknown repository identifiers from audit events.
- Keep Unix process-group cancellation active after the group leader exits.
- Terminate descendants when inherited output pipes exceed the wait limit.
- Reject statistics responses that omit required size data.
- Omit child stderr from generic errors to prevent backend location disclosure.

### Changed

- Require Go 1.26 or later. Use a current patch release of a supported Go version.
- Build CI and release binaries with Go 1.27.3.
- Update lint, vulnerability scanning, and cache tooling.
- Preserve external Go build flags and enable the race detector's CGO requirements.
- Document password rotation, credential restrictions, test boundaries, and release gates.
