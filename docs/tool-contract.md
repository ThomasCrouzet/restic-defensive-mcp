# Tool contract (v0.1)

All successful tool responses are a JSON object:

```json
{
  "cost": "light|moderate|expensive",
  "truncated": false,
  "data": {},
  "warnings": []
}
```

Strings originating in repository metadata are sanitized and redacted before
encoding. Snapshot IDs used by content tools are resolved through repository
host, tag, and path policies before the underlying restic command runs.

Errors set MCP `isError` and return:

```json
{
  "code": "not_allowed",
  "message": "human readable",
  "detail": "optional non-sensitive detail"
}
```

## Error codes

| Code | Meaning |
|------|---------|
| `invalid_argument` | Bad or missing input |
| `repository_not_found` | Unknown `repository_id` |
| `snapshot_not_found` | No matching snapshot |
| `ambiguous_snapshot` | Prefix matched multiple ids |
| `not_allowed` | Host/tag/path policy denial |
| `restic_not_found` | Binary missing at boot |
| `unsupported_restic_version` | Below minimum |
| `unsupported_backend` | SFTP/rclone/other |
| `authentication_failed` | Restic exit 12 |
| `repository_locked` | Restic exit 11 |
| `repository_unavailable` | Restic exit 10 (repository does not exist) |
| `timeout` | Command deadline exceeded |
| `cancelled` | Caller cancelled the request (context cancelled) |
| `output_limit_exceeded` | Stdout/stderr cap |
| `protocol_error` | Unexpected restic JSON |
| `internal_error` | Other failure |
| `config_error` | Boot/config only |

## Tools

### `restic_capabilities`

Input: none.

Output `data`: server version, restic version, repository ids, backends,
allowed/forbidden commands, tool names, limits, warnings.
Never includes URLs or secret paths.

### `list_repositories`

Input: none.

Output `data`: array of `{id, label?, backend, allowed_hosts, allowed_tags, allowed_paths}`.

### `list_snapshots`

| Field | Type | Notes |
|-------|------|-------|
| `repository_id` | string | required |
| `host` | string | must be allowlisted if set |
| `tags` | string[] | each must be allowlisted |
| `path` | string | absolute, allowlisted |
| `limit` | int | capped by config |
| `offset` | int | pagination |

Output: `{snapshots, count, total, offset, limit}` sorted newest first (UTC).
`truncated` is true when more results exist beyond the current offset/limit page.
Snapshot objects omit `username`, `parent`, and `tree` (not needed for inspection).
If the path policy exposes only a narrower intersection of a broader snapshot
root, the whole-snapshot backup `summary` is omitted to avoid aggregate leakage.

### `get_snapshot`

| Field | Type | Notes |
|-------|------|-------|
| `repository_id` | string | required |
| `snapshot_id` | string | full id, hex prefix ≥8, or `latest` |
| `host`/`tags`/`path` | | required disambiguators for `latest` in multi-snapshot repos |

Full 64-hex snapshot ids are resolved with a targeted metadata query. Shorter
hex prefixes are matched server-side so `ambiguous_snapshot` is preserved.
Invisible snapshots are reported as `snapshot_not_found`.

### `browse_snapshot`

| Field | Type | Notes |
|-------|------|-------|
| `repository_id` | string | required |
| `snapshot_id` | string | full id, prefix, or `latest` |
| `path` | string | required, allowlisted absolute path |
| `recursive` | bool | default false |
| `limit` | int | capped by `max_nodes` |
| `host` | string | optional policy-checked filter |

Output nodes: `name`, `type`, `path`, `size?`, `permissions?`, `mtime?`.
No file contents. For `latest`, the required path scopes selection; `host` can
narrow it further.

### `find_files`

| Field | Type | Notes |
|-------|------|-------|
| `repository_id` | string | required |
| `pattern` | string | simple glob; rejects regex metacharacters and flag-shaped patterns starting with `-` |
| `path` | string | optional with zero/one allowed root; required with multiple roots |
| `snapshot_id` | string | concrete id only (not `latest`) |
| `host`/`tags` | | policy checked |
| `limit` | int | capped |
| `ignore_case` | bool | |

Output: `{groups, count}` where `count` is the number of matches **after** path
allowlist filtering (capped by `limit` / `max_find_results`).
`truncated` is true only when additional visible matches were omitted.
The server first selects visible snapshot IDs, then filters and limits matches.
With one configured root, an omitted `path` uses that root automatically.
Multiple allowed roots require an explicit path so every search has one clear,
locally enforced scope.

### `repository_stats`

| Field | Type | Notes |
|-------|------|-------|
| `repository_id` | string | required |
| `mode` | string | `restore-size` (default), `files-by-contents`, `raw-data`, `blobs-per-file` |
| `snapshot_id` | string | optional |
| `host`/`tags`/`path` | | snapshot selectors; `path` is not a subtree statistics scope |

Output includes `selected_snapshots`, `excluded_snapshots`, and `notes`.
Statistics cover entire selected snapshots and are not a full integrity check.
When path policy is configured, only snapshots whose every root is allowlisted
are eligible; this prevents aggregate size/count leakage from broader roots.
