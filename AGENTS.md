# Project instructions

This Go MCP server provides restricted, read-only access to configured restic repositories.
Read `CONTRIBUTING.md` and `SECURITY.md` before source changes.
Keep command allowlists, sealed repository locations, path policies, and secret-file restrictions.
Only `internal/testrepo` can initialize temporary repositories or create test backups.

## Testing policy

- Never write unit tests after you write code.
- Highly prefer E2E tests as the sole testing mechanism.
- Use E2E tests to verify complex features through observable results.
- At the end of each E2E run, produce a verifiable and repeatable artifact.
- Record the command, source revision, environment, fixtures, and results with the artifact.
- Include the working diff identity when the source has uncommitted changes.
- Examine assertions, fixtures, mocks, and skips before removing a test.
- Do not treat disabled E2E tests or simulated boundaries as equivalent coverage.
- If isolation is necessary, first document all identified failure modes. Then write the tests and implementation.
- Keep an isolated test only for a concrete failure that E2E tests cannot detect.
- Do not add tests for coverage percentages, type contracts, dependency behavior, or mocked call sequences alone.

Run `GOMAXPROCS=2 go test -p 1 -race -count=1 ./...` for local regression checks.
Run `GOMAXPROCS=2 GOFLAGS=-p=1 make integration` with a local restic binary.
The integration suite uses in-memory MCP and the MCP executable over stdio with real restic commands.
Its temporary repository contains two backups. The suite does not restore file contents.
Keep isolated checks for policy projection, ambiguous snapshots, command injection, environment sealing, output limits, audit redaction, and malformed responses.
The fixture removes its temporary repository after the run.
Save the integration log, restic version, fixture description, and required run metadata outside the repository.
The executable suite retains local protocol evidence outside the repository.
See `docs/testing.md` for artifact settings and test boundaries.
Never upload reports, traces, or SARIF files to GitHub. Keep release deliverables separate.
