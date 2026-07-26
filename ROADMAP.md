# Roadmap

## v0.1 (current)

Structurally read-only inspection:

- Sealed repository registry
- Seven MCP tools, no mutations
- Local / S3 / B2 / REST backends
- Bounds, redaction, audit, cost classes
- Real restic integration tests on temporary repos

## After v0.1 (reporting and compatibility)

Possible improvements that stay read-only:

- Richer pagination cursors
- Optional metadata-only health summary with explicit "what was not verified"
- Broader restic version matrix in CI
- Stronger singleflight/caching for identical light queries (no sensitive persistence by default)
- Windows path policy hardening if demand appears
- Optional `diff_snapshots` as a replacement for an existing tool slot (still max seven), with hard limits

## Explicitly out of scope for this binary

Mutation capabilities (`backup`, `restore`, `forget`, `prune`, `unlock`,
`repair`, key management, etc.) will **not** be added behind feature flags in
this project.

If a write-capable companion is ever designed, it must be a **separate binary
and separate decision**, with its own threat model. It will not appear as an
automatic next step on this roadmap.
