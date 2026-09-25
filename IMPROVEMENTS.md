# Improvement priorities

These items preserve the inspection-only contract in [SECURITY.md](SECURITY.md).

## P1: Verify the complete executable path

- Component: `cmd/restic-defensive-mcp`, `internal/testrepo`.
- Benefit: detect startup, version-probe, and stdio integration failures.
- Completion: a child process lists and browses two temporary snapshots.
  The transcript proves that denied paths and ambiguous IDs expose no metadata.
  Repository writes remain confined to the existing test fixture helper.

## P1: Verify process termination under hostile output

- Component: `internal/restic/runner.go`, `process_unix.go`.
- Benefit: verify cancellation beyond fake runner responses.
- Completion: a disposable child fills stdout or stderr, or holds a pipe open.
  The check proves bounded output, bounded termination, and reusable concurrency slots.

## P2: Make credential rotation boundaries explicit

- Component: `internal/repositories/registry.go`, `internal/config`.
- Benefit: distinguish sealed repository and backend settings from password-file rotation.
- Completion: an executable fixture verifies accepted password rotation and rejects
  attempts to retarget the sealed repository. Documentation states the boundary.

## P2: Retain pagination and policy evidence

- Component: `internal/mcpserver/server.go`, `internal/policy/paths.go`.
- Benefit: demonstrate useful results under mixed visible and hidden snapshot roots.
- Completion: protocol scenarios cover page limits, nested allowlists, empty matches,
  and whole-snapshot statistics restrictions with retained JSON results.
