# restic-defensive-mcp

Inspect [restic](https://restic.net) snapshots through a **read-only stdio**
[MCP](https://modelcontextprotocol.io) server. List backups, browse file metadata,
find paths, and get repository statistics.

![Configured repository IDs and path policies constrain five fixed restic commands. Results contain metadata, not file contents.](docs/diagrams/inspection-flow.svg)

Repository locations remain fixed after startup. The runner accepts only
`version`, `snapshots`, `ls`, `find`, and `stats` with controlled arguments.
It uses no shell. Policies restrict hosts, tags, paths, and result sizes.
Errors remove secrets, and each tool reports its cost class.

## Requirements

- Go 1.25+ (build)
- restic **0.17.1+** on `PATH`, or an explicit `restic_binary` path. CI uses 0.17.1.
- An MCP client that supports local stdio servers

## Install (from source)

```bash
git clone https://github.com/ThomasCrouzet/restic-defensive-mcp.git
cd restic-defensive-mcp
make build
./bin/restic-defensive-mcp --version
```

## Quick start

1. Create regular password and repository location files outside the repository.
   Do not use symlinks. Enter the password through an editor:

```bash
secret_dir="${XDG_CONFIG_HOME:-$HOME/.config}/restic-defensive-mcp"
install -d -m 700 "$secret_dir"
install -m 600 /dev/null "$secret_dir/repository"
printf '%s\n' '/path/to/your/repo' > "$secret_dir/repository"
install -m 600 /dev/null "$secret_dir/password"
"${EDITOR:-vi}" "$secret_dir/password"
```

2. Copy [config.example.yaml](config.example.yaml) outside the repository.
3. Set its repository file, password file, and host/tag/path allowlists.

4. Start the binary:

```bash
./bin/restic-defensive-mcp --config /path/to/config.yaml
```

5. Register the absolute binary path and `--config` argument in your MCP host.

Example MCP client configuration:

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
| `get_snapshot` | light | Metadata for one snapshot |
| `browse_snapshot` | moderate | Directory listing inside allowlisted path |
| `find_files` | moderate or expensive | Simple glob search |
| `repository_stats` | moderate or expensive | Size/count statistics for permitted modes |

There is **no** `run_restic`, `backup`, `restore`, `forget`, `prune`, `unlock`,
`check --read-data`, or repository URL argument.

Full request/response shapes: [docs/tool-contract.md](docs/tool-contract.md).

## Configuration

See [config.example.yaml](config.example.yaml) for all settings:

- MCP callers pass only `repository_id`
- Prefer `repository_file` + `password_file` over inline secrets
- Repository locations and backend credentials are loaded and sealed at boot;
  changing their source files does not retarget a running server
- `allowed_hosts`, `allowed_tags`, and `allowed_paths` enforce access restrictions
- Empty allowlists mean **no restriction** for that dimension; the server logs a
  boot-time warning (`empty_allowlist`) so operators notice
- Unknown YAML keys and multiple YAML documents are rejected
- Secret files must be regular, non-symlink files; Unix requires mode `0600`
  or stricter, while Windows deployments must enforce equivalent NTFS ACLs
- `find_files` requires an explicit `path` when multiple roots are allowlisted
- Runtime limits: zeros become defaults; values outside absolute min/max are
  **rejected** (not silently clamped). Defaults are the recommended starting
  point; values may be raised only up to those ceilings

## Supported backends

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

Read-only inspection can have these local and repository effects:

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

The demo uses in-memory MCP and real restic commands against a temporary repository.
`internal/testrepo` creates two backups and removes the fixture after the run.
The suite calls all seven tools and checks path denial and the absence of mutation tools.
It does not start the MCP executable or restore file contents.
Go can download modules on a new checkout.

## Development

```bash
make fmt
make lint
make test          # local regression tests
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

## Documentation

- [Tool contract](docs/tool-contract.md): inputs, outputs, policies, and costs.
- [restic compatibility](docs/restic-compatibility.md): commands and backend limits.
- [Security](SECURITY.md): secret files, metadata privacy, and remaining risks.
- [Contributing](CONTRIBUTING.md) and [Changelog](CHANGELOG.md).

## License

MIT, see [LICENSE](LICENSE).
