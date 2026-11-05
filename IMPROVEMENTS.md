# Improvement priorities

These items preserve the inspection-only contract in [SECURITY.md](SECURITY.md).

## Completed: Extend executable policy scenarios ([#15](https://github.com/ThomasCrouzet/restic-defensive-mcp/issues/15))

- Component: `internal/testrepo`, executable stdio scenarios.
- Benefit: demonstrate useful results under mixed visible and hidden snapshot roots.
- Completion: `TestExecutableMixedVisibilityPolicies` covers mixed visible/hidden
  roots, a snapshot root broader than the allowlist, an allowlisted parent
  broader than a backed-up child, nested and overlapping allowlists, `/`,
  disjoint roots, hidden hosts and tags, scoped `latest`, and whole-snapshot
  statistics restrictions. Assertions inspect public responses and errors.
- Evidence: executable tests also cover pagination, empty matches, and path
  denial. Isolated policy tests retain mixed roots and ambiguous snapshot
  prefixes that executable fixtures cannot make deterministic.

## Completed: Exercise native Windows behavior ([#20](https://github.com/ThomasCrouzet/restic-defensive-mcp/issues/20))

- Component: secret-file handling, process cancellation, and executable integration.
- Benefit: detect runtime differences that cross-compilation cannot find.
- Completion: `native-windows.yml` runs a Windows runner with a checksum-verified
  restic, a POSIX-path stdin fixture, spaces in executable/config paths, and an
  `icacls` private-ACL check. Drive-root snapshot paths are canonicalized to
  POSIX form; UNC and drive-relative forms remain explicitly unsupported.
  Cancellation still kills the direct restic process only and may leave a lock;
  that platform limit is documented.

## P2: Exercise remote backend deployment boundaries

- Component: S3, B2, and REST backend integration.
- Benefit: detect authentication, TLS, and network differences from local repositories.
- Completion: disposable services provide controlled credentials and network failures.
  The executable returns safe errors without exposing repository locations or credentials.
