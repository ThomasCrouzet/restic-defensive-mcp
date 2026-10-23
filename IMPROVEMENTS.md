# Improvement priorities

These items preserve the inspection-only contract in [SECURITY.md](SECURITY.md).

## P2: Extend executable policy scenarios

- Component: `internal/mcpserver/server.go`, `internal/policy/paths.go`.
- Benefit: demonstrate useful results under mixed visible and hidden snapshot roots.
- Completion: executable protocol scenarios cover nested allowlists and whole-snapshot statistics restrictions.
  Retained JSON results show which metadata remains visible.
- Existing evidence: executable tests cover pagination, empty matches, and path denial.
  Isolated policy tests cover mixed roots and ambiguous snapshot prefixes.

## P2: Exercise native Windows behavior

- Component: secret-file handling, process cancellation, and executable integration.
- Benefit: detect runtime differences that cross-compilation cannot find.
- Completion: a Windows runner executes the stdio suite and records local results.
  Its fixture uses private NTFS ACLs and confirms direct-process cancellation.

## P2: Exercise remote backend deployment boundaries

- Component: S3, B2, and REST backend integration.
- Benefit: detect authentication, TLS, and network differences from local repositories.
- Completion: disposable services provide controlled credentials and network failures.
  The executable returns safe errors without exposing repository locations or credentials.
