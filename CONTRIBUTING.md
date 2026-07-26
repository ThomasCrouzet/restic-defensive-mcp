# Contributing

Thanks for considering a contribution.

## Rules

- Public repository language is **English** (code, comments, docs, commits).
- Do not add telemetry, auto-update, or mutation tools to the MCP surface.
- Do not introduce a free-form restic argv escape hatch.
- New dependencies need a one-line justification in the README table.
- Do not commit secrets, real repository URLs, or live backup paths.

## Development setup

```bash
go version   # Go 1.25+
restic version
make fmt
make lint
make test
make race
make integration   # if restic is installed
```

## Pull requests

1. Fork and branch from `main`.
2. Keep commits atomic and messages imperative ("Add path allowlist fuzz").
3. Include tests for behavior changes.
4. Run `gofmt`, `go vet`, `go test ./...`, and `go test -race ./...`.
5. Update docs when tool contracts or security properties change.

## Security-sensitive changes

Changes to argv construction, environment building, backend classification,
path policy, or secret file handling require:

- unit tests
- an explicit note in the PR description
- no weakening of allowlists "to make a test pass"

Report vulnerabilities per [SECURITY.md](SECURITY.md).

## Code layout

```
cmd/restic-defensive-mcp/   # process entry
internal/config/            # YAML + secret files
internal/restic/            # argv, runner, parsers
internal/repositories/      # sealed registry
internal/policy/            # host/tag/path rules
internal/mcpserver/         # MCP tools
internal/testrepo/          # init/backup harness for tests only
internal/redaction/
internal/audit/
```

Production code must not call restic `init` or `backup`. Those exist only in
`internal/testrepo`.
