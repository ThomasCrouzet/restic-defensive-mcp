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

Generic restic failures return `internal_error` without child stderr details.
Backend errors can contain repository locations even when they contain no password.

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
| `resource_busy` | Repository or global command capacity is full; retry after a delay |
| `repository_unavailable` | Restic exit 10 (repository does not exist) |
| `timeout` | Command deadline exceeded |
| `cancelled` | Caller cancelled the request (context cancelled) |
| `output_limit_exceeded` | Stdout/stderr cap |
| `protocol_error` | Unexpected restic JSON |
| `cursor_invalid` | Cursor is malformed, expired, already used, or belongs to another request |
| `stale_selection` | A snapshot frozen in a cursor was removed or is no longer visible |
| `internal_error` | Other failure |
| `config_error` | Boot/config only |

The server rejects excess commands immediately when a concurrency limit is full.
It does not keep an unbounded command queue.
Clients can retry `resource_busy` errors with a delay and a limited retry count.

## Time selectors

`after` and `before` filter snapshot creation time (`Snapshot.Time`), not file
modification time. They accept RFC 3339 timestamps with an explicit offset,
normalize to UTC, and use the half-open interval `[after, before)`. Reversed,
equal, or malformed bounds return `invalid_argument` before any restic command.
A window narrows an oversized `find_files` or `repository_stats` selection; it
never changes statistics modes or aggregates independent batches.

## Schema and validation

Every tool advertises an `outputSchema` in `tools/list` and returns the same
envelope as MCP `structuredContent` plus a JSON text block for older clients.
The envelope requires `cost` and `data`; `truncated`, `warnings`, continuation
fields, and optional result fields are omitted when absent.

The SDK validates JSON argument types, unknown required fields, and bounds
declared by the input schema before the handler runs. Those framework failures
are MCP tool errors and do not use the `{code, message, detail}` handler
envelope. Policy, window, cursor, repository, and restic failures keep the
documented handler envelope and error codes. Optional result fields such as
`total` and `next_cursor` are explicitly absent rather than guessed.

## Freshness summary

`list_snapshots` can return an opt-in `freshness` object. It uses only the
snapshot metadata already authorized for that request; it adds no backend probe
and does not change `list_repositories` or `restic_capabilities`.

The summary includes the observation time, effective host/tag/path scope,
requested window, group-by mode, and the latest visible snapshot per group with
its timestamp, age, visible count, and optional expected cadence. `status` is one
of `no_visible_snapshots`, `recency_only`, `within_cadence`, `overdue`, or
`incomplete`. `overdue` is `null` when no cadence was supplied; age exactly at
the cadence is not overdue. `hidden_scope` reports restricted dimensions as
booleans without hidden counts or identifiers. `partial_path_visibility` and the
per-group flag mark snapshots visible only through an allowlisted intersection;
their whole-snapshot summaries remain suppressed.

When metadata caching is enabled, `list_snapshots` also returns
`metadata_cache: {enabled, hit, observed_at, age_seconds, ttl_seconds}`. A cache
hit adds a warning and never proves current authentication or backend
reachability. Selection caching for find/browse/statistics is internal; those
tools still run fresh concrete inspection commands and reapply authorization.

A missing visible snapshot is not evidence that no backup exists. Freshness
never proves restorability, cryptographic integrity, backup coverage, or a
successful data verification. Repository connectivity errors remain ordinary
tool errors rather than freshness results.

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
| `path` | string | absolute, allowlisted; selects snapshots whose roots overlap it in either direction |
| `limit` | int | capped by config |
| `offset` | int | legacy pagination offset |
| `after` | string | optional RFC 3339 inclusive lower bound on snapshot creation time |
| `before` | string | optional RFC 3339 exclusive upper bound on snapshot creation time |
| `cursor` | string | opaque continuation; carries the original filters |
| `include_freshness` | bool | opt-in metadata-only latest-per-group summary |
| `expected_cadence` | string | optional Go duration (1m to 87600h) for overdue judgment |
| `freshness_group_by` | string | `host` (default) or `host_tag` |
| `metadata_cache` | object | present when caching is enabled; includes hit, observation time, age, and TTL |

Output: `{snapshots, count, total, offset, limit, next_cursor?}` sorted newest first (UTC).
`truncated` is true when more results exist beyond the current offset/limit page.
When a cursor is supplied, pass only `repository_id`, `cursor`, and optionally
`limit`; combining a cursor with filters or `offset` returns `invalid_argument`.
`include_freshness` cannot be combined with a cursor because the summary
describes the first-page selection.
Cursors are single-use, expire after five minutes, and bind the repository,
filters, and policy. Snapshot additions after the first page are not spliced into
the same traversal. Restarting the server invalidates outstanding cursors.
Snapshot objects omit `username`, `parent`, and `tree` (not needed for inspection).
If the path policy exposes only a narrower intersection of a broader snapshot
root, the whole-snapshot backup `summary` is omitted to avoid aggregate leakage.

### `get_snapshot`

| Field | Type | Notes |
|-------|------|-------|
| `repository_id` | string | required |
| `snapshot_id` | string | full id, hex prefix ≥8, or `latest` |
| `host`/`tags`/`path` | | required disambiguators for `latest` in multi-snapshot repos |
| `after`/`before` | string | optional snapshot-creation window, `[after, before)` |

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
| `after`/`before` | string | optional snapshot-creation window, `[after, before)` |
| `cursor` | string | opaque continuation; carries the original scope |

Output nodes: `name`, `type`, `path`, `size?`, `permissions?`, `mtime?`.
No file contents. For `latest`, the required path scopes selection and the
concrete snapshot is fixed before the first cursor is issued; `host` can narrow
it further. With a cursor, pass only `repository_id`, `cursor`, and optionally
`limit`.
Output also includes `total` (visible nodes in the bounded scan) and
`incomplete` (the restic output reached its cap). `truncated` is true when
another page exists or the scan is incomplete; `next_cursor` exists only while
another visible page can be requested. Hidden nodes are filtered before the page
offset, so they never consume the visible page budget. Each page re-runs `restic
ls`; this is continuation, not backend pagination.

### `find_files`

| Field | Type | Notes |
|-------|------|-------|
| `repository_id` | string | required |
| `pattern` | string | simple glob; rejects regex metacharacters and flag-shaped patterns starting with `-` |
| `path` | string | optional with zero/one allowed root; required with multiple roots |
| `snapshot_id` | string | concrete id only (not `latest`) |
| `host`/`tags` | | policy checked |
| `limit` | int | page size, capped by `max_find_results` |
| `ignore_case` | bool | |
| `after`/`before` | string | optional snapshot-creation window, `[after, before)`; not file modification time |
| `cursor` | string | opaque continuation; carries the original filters |

Output: `{groups, count, total?, incomplete, next_cursor?}` where `count` is the
number of visible matches in this page. Each group's `hits` counts only that
group's matches in this page; a group can span pages. `total` is published only
after the underlying authorized scan completed, never for an incomplete scan.
Each match includes `name`, derived from the validated POSIX snapshot path with
`path.Base` when restic omits it; `name` therefore uses the same spelling as
`browse_snapshot`, including spaces and Unicode.
With a cursor, pass only `repository_id`, `cursor`, and optionally `limit`.
Hidden matches are filtered before the page offset. `truncated` is true when
another visible page exists or the scan is incomplete. Cursors are single-use,
expire after five minutes, and return `stale_selection` if a frozen snapshot was
removed. Each page re-runs `restic find`; this is not backend pagination.
The server first selects visible snapshot IDs by root intersection, then filters
and limits matches inside the requested scope. With one configured root, an
omitted `path` uses that root automatically.
Snapshot selection accepts both parent/child directions: a scope rooted at an
allowlisted parent includes snapshots of child directories, and a child scope
still selects a visible concrete snapshot whose root is that child.
Multiple allowed roots require an explicit path so every search has one clear,
locally enforced scope.

### `repository_stats`

| Field | Type | Notes |
|-------|------|-------|
| `repository_id` | string | required |
| `mode` | string | `restore-size` (default), `files-by-contents`, `raw-data`, `blobs-per-file` |
| `snapshot_id` | string | optional |
| `host`/`tags`/`path` | | snapshot selectors; `path` is not a subtree statistics scope |
| `after`/`before` | string | optional snapshot-creation window, `[after, before)` |

Output includes `selected_snapshots`, `excluded_snapshots`, and `notes`.
Statistics cover entire selected snapshots and are not a full integrity check.
When path policy is configured, only snapshots whose every root is allowlisted
are eligible; this prevents aggregate size/count leakage from broader roots.
