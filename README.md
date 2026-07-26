# restic-defensive-mcp

Structurally read-only [MCP](https://modelcontextprotocol.io) server (stdio) for
inspecting [restic](https://restic.net) repositories through an untrusted MCP
client **without** exposing a shell, free-form CLI, or mutation path.

> Are my backups present and recent, is their metadata readable, and does a
> snapshot contain the files I expect?

## Why not expose `restic` directly?

Restic is a powerful backup tool. An unrestricted caller can:

- delete retention history (`forget` / `prune`)
- overwrite or extract data (`backup` / `restore` / `dump`)
- unlock or rewrite repository state
- accept arbitrary repository URLs and password commands

Thin wrappers that expose "run restic with these args" or per-call repository
overrides recreate that blast radius inside MCP.

This project is different by construction:

1. Repositories are **declared and sealed at process start**
2. MCP surface is **exclusively read-only** (no feature flag for writes)
3. Only a **compiled-in subset** of restic subcommands can be built
4. **No shell** — `exec.CommandContext` with fixed argv only
5. Hosts, tags, paths, and result sizes are **bounded**
6. Secrets come from **permission-checked files**, never `RESTIC_PASSWORD_COMMAND`
7. Errors are **structured and redacted**
8. Every tool reports an explicit **cost class** (`light` / `moderate` / `expensive`)

## Requirements

- Go 1.25+ (build)
- restic **0.17.1+** on `PATH` (or `restic_binary` in config); tested with 0.19.x
- An MCP client that supports local stdio servers

## Install (from source)

```bash
git clone https://github.com/ThomasCrouzet/restic-defensive-mcp.git
cd restic-defensive-mcp
make build
./bin/restic-defensive-mcp --version
```

## Quick start

1. Create password and repository location files (regular files, no symlinks).
   Avoid placing the password in shell history:

```bash
secret_dir="${XDG_CONFIG_HOME:-$HOME/.config}/restic-defensive-mcp"
install -d -m 700 "$secret_dir"
install -m 600 /dev/null "$secret_dir/repository"
printf '%s\n' '/path/to/your/repo' > "$secret_dir/repository"
install -m 600 /dev/null "$secret_dir/password"
"${EDITOR:-vi}" "$secret_dir/password"
```

2. Copy and edit `config.example.yaml`, including the two file paths above.

3. Run the freshly built binary:

```bash
./bin/restic-defensive-mcp --config /path/to/config.yaml
```

4. Point your MCP host at the binary with `--config` (stdio transport only).

Example MCP host snippet (illustrative):

```json
{
  "mcpServers": {
    "restic-defensive": {
      "command": "/absolute/path/to/restic-defensive-mcp",
      "args": ["--config", "/etc/restic-defensive-mcp/config.yaml"]
    }
  }
}
```

## Tools (exactly seven)

| Tool | Cost | Purpose |
|------|------|---------|
| `restic_capabilities` | light | Versions, limits, backends, warnings |
| `list_repositories` | light | Configured IDs and allowlists (no URLs) |
| `list_snapshots` | light | Bounded snapshot list |
| `get_snapshot` | light | One snapshot metadata |
| `browse_snapshot` | moderate | Directory listing inside allowlisted path |
| `find_files` | moderate–expensive | Simple glob find (not arbitrary regex) |
| `repository_stats` | moderate–expensive | Size/count stats for allowlisted modes |

There is **no** `run_restic`, `backup`, `restore`, `forget`, `prune`, `unlock`,
`check --read-data`, or repository URL argument.

Full request/response shapes: [docs/tool-contract.md](docs/tool-contract.md).

## Configuration

See [config.example.yaml](config.example.yaml). Principles:

- MCP callers pass only `repository_id`
- Prefer `repository_file` + `password_file` over inline secrets
- Repository locations and backend credentials are loaded and sealed at boot;
  changing their source files does not retarget a running server
- `allowed_hosts` / `allowed_tags` / `allowed_paths` are **enforcement**, not cosmetics
- Empty allowlists mean **no restriction** for that dimension; the server logs a
  boot-time warning (`empty_allowlist`) so operators notice
- Unknown YAML keys and multiple YAML documents are rejected
- Secret files must be regular, non-symlink files; Unix requires mode `0600`
  or stricter, while Windows deployments must enforce equivalent NTFS ACLs
- `find_files` requires an explicit `path` when multiple roots are allowlisted
- Runtime limits: zeros become defaults; values outside absolute min/max are
  **rejected** (not silently clamped). Defaults are the recommended starting
  point; values may be raised only up to those ceilings

## Supported backends (v0.1)

| Backend | Supported |
|---------|-----------|
| local filesystem | yes (absolute paths only) |
| S3 | yes (credentials via allowlisted env files) |
| Backblaze B2 | yes |
| REST server | yes |
| SFTP | **no** (spawns `ssh`) |
| rclone | **no** (spawns `rclone`) |
| other | **no** |

Compatibility notes: [docs/restic-compatibility.md](docs/restic-compatibility.md).

## Local effects: cache and locks

Inspection is **not** a pure no-op on disk or on the repository lock table:

- restic may create or update a **local cache** (`cache_dir` or default)
- restic may take a **repository lock** during snapshots/ls/find/stats

This server never intentionally mutates backup contents or snapshot sets.
It also never runs full data verification (`restic check --read-data`).
`repository_stats` answers size/count questions only.

Details: [SECURITY.md](SECURITY.md).

## Privacy limits

Snapshot file names, paths, hosts, tags, sizes, and timestamps are **sensitive**.
This server:

- never returns file **contents**
- never returns repository URLs or secret file paths
- bounds listings and find results
- sanitizes control characters in names
- keeps audit logs free of full paths by default

## Demo (local temporary repository)

```bash
make demo
```

Runs the end-to-end harness against a throwaway restic repository created by
`internal/testrepo`. It exercises every MCP tool, proves path denial and the
absence of mutation tools, then removes the fixture. Repository operations stay
local; normal Go module downloads may still occur on a fresh checkout.

## Development

```bash
make fmt
make lint
make test          # unit tests
make race          # race detector
make fuzz-smoke    # short run of every fuzz target
make integration   # real restic temp repo (requires restic)
make vet
```

## Dependencies

| Module | Why |
|--------|-----|
| `github.com/modelcontextprotocol/go-sdk` | Official MCP Go SDK (stdio server/client) |
| `gopkg.in/yaml.v3` | Config file parsing |

Restic itself is an external binary, not a Go module.

## Non-goals

- Backup orchestration or scheduling
- Restore / forget / prune / unlock / repair
- Generic restic CLI proxy
- Autorestic config import
- Remote multi-tenant API
- Full `restic check --read-data` as a casual tool
- Telemetry or auto-update

## License

MIT — see [LICENSE](LICENSE).
