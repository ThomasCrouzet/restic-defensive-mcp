# Restic compatibility

## Minimum version

**0.17.1**: required for stable repository, lock, and authentication exit
codes:

| Code | Meaning |
|------|---------|
| 0 | success |
| 1 | generic failure |
| 10 | repository does not exist |
| 11 | lock failure |
| 12 | wrong password |
| 130 | cancelled |

CI runs the Linux integration suite with **restic 0.17.1** and **restic 0.19.1**.
The suite starts the real MCP executable and uses a temporary local repository.
Remote backends require deployment-specific credentials and network checks.

A native **macOS arm64** job validates the executable on `macos-14` with restic
0.19.1. It records the macOS version, architecture, Go version, and restic
version; requires an arm64 runner and binary; runs the executable stdio suite;
and exercises native cancellation plus repository lock cleanup. During release,
the job builds the intended macOS archive, verifies its version output, and runs
the same suite against the extracted archive. This is native runtime evidence,
separate from cross-compilation and earlier manual runs.

## JSON formats used

| Command | Format | Parser |
|---------|--------|--------|
| `version --json` | single object | `ParseVersion` |
| `snapshots --json` | single array | `ParseSnapshots` |
| `ls --json` | JSON lines (`snapshot` + `node`) | `ParseLS` |
| `find --json` | single array of hit groups | `ParseFind` |
| `stats --json` | single object | `ParseStats` |

Unknown restic JSON fields are ignored. Malformed entries and missing required
fields fail with `protocol_error` instead of being silently omitted. Hostile
strings are sanitized before returning to MCP clients.

## Production subcommands

Allowed: `version`, `snapshots`, `ls`, `find`, `stats`.

Explicitly never built by production code: `backup`, `restore`, `forget`,
`prune`, `unlock`, `repair`, `recover`, `rewrite`, `tag`, `key`, `init`,
`copy`, `migrate`, `check`, `dump`, `cat`, `cache`, `mount`, `generate`,
`self-update`, and others.

Local repository locations must be absolute paths. Relative paths and `file:`
URLs are rejected so the process working directory cannot retarget a registry
entry.

## Flags never passed

Production argv is built only by `Build*Argv` helpers and checked by
`AssertArgvAllowed`. Callers cannot inject repository or password flags.

Denied global flags include (exact and `flag=value` forms where applicable):

- `--password-command` / `-p` / `--password` / `--password-file`
- `--insecure-tls` / `--insecure-no-password`
- free-form `-o` / `--option`
- `-r` / `--repo` / `--repository` / `--repository-file`
- `--cache-dir` / `--no-lock` / `--key-hint` / `--cacert` / `--tls-client-cert`
- find modes `--blob` / `--pack` / `--tree`

Repository location and password come only from the scrubbed child env
(`RESTIC_REPOSITORY`, `RESTIC_PASSWORD_FILE`). The location is read from
`repository_file` and snapshotted at boot, preventing post-start retargeting.

`find_files` patterns must not start with `-` (flag-shaped tokens rejected).
The pattern is passed after a `--` end-of-flags marker.

Repeated restic `--path` filters use intersection semantics. `find_files`
therefore selects policy-visible snapshots by concrete ID and enforces its
single requested path locally. A requested scope and a snapshot root overlap
when either contains the other, so an allowlisted parent selects child
snapshots while a child scope still selects its concrete snapshot. When a
repository exposes multiple allowed roots, the caller must choose one
explicitly.

## Snapshot time windows

`after` and `before` accept RFC 3339 timestamps with an explicit offset and form
a half-open `[after, before)` interval on restic snapshot creation time. The
server normalizes them to UTC and filters snapshots before invoking `find` or
`stats`. They are not translated into restic's file-time `--oldest`/`--newest`
find flags.

## Metadata coalescing and cache

Identical in-flight snapshot-metadata commands are coalesced, so concurrent
callers share one approved `snapshots` invocation without changing the capacity
policy for distinct commands. An optional in-memory cache is disabled by default;
when enabled it is short-lived (`1s` to `5m`), entry-bounded, keyed by repository
and normalized selectors, and reports hit status, observation time, age, and TTL.
Password rotation and removed snapshots are observed only after expiry. A cache
hit does not prove current authentication or backend reachability. Content tools
still execute fresh `ls`, `find`, or `stats` commands and reapply policy.

## Continuation cursors

`list_snapshots`, `browse_snapshot`, and `find_files` can return an opaque
`next_cursor`. Cursors are bounded in-memory state, expire after five minutes,
are single-use, and bind the repository, original filters, and sealed policy.
Browsing and searching re-run the approved inspection command for each page;
restic has no backend pagination for these formats. Hidden entries are filtered
before advancing the visible page offset. A removed snapshot produces
`stale_selection`; a malformed, reused, expired, or mismatched cursor produces
`cursor_invalid`. Restarting the server invalidates outstanding cursors.

## Process lifecycle (timeouts)

- **Unix:** restic is started in its own process group (`Setpgid`). Caller
  cancellation, deadlines, output caps, and server shutdown send `SIGTERM` to
  the group, allow a two-second cleanup window, then send `SIGKILL` to the
  whole group. The runner waits at most three seconds for inherited pipes.
  Forced termination can leave a stale inspection lock; this server never runs
  `unlock`, so an operator may need restic's documented unlock procedure.
- **Windows:** restic is started with `CREATE_NEW_PROCESS_GROUP`; cancellation
  kills the direct restic process only (no full job-object tree walk in v0.1).
  Supported backends do not spawn `ssh`/`rclone` helpers.
- Reaching the stdout or stderr byte cap starts the same graceful cleanup; it
  does not keep buffering output.
- On Unix, cancellation remains active after the group leader exits.
  The runner also terminates remaining group members when inherited output pipes exceed the wait limit.
- Cancellation, deadline, and output-cap classifications are retained even if
  restic exits cleanly during the graceful cleanup window.

## Locks and cache

v0.1 does **not** default to `--no-lock`. Inspection commands may lock and may
read/write the restic local cache. Configure `cache_dir` per repository for
isolation.

## Password and repository injection

Child environment always uses:

- `RESTIC_REPOSITORY` (value sealed from `repository_file` at boot)
- `RESTIC_PASSWORD_FILE`
- optional `RESTIC_CACHE_DIR`
- optional allowlisted backend credential keys from `env_files`

Parent process environment is not inherited for `RESTIC_*` or cloud secrets.

The registry seals repository locations and backend environment values at startup.
Changes to `repository_file` or `env_files` require a server restart.
The password file path stays fixed, but restic reads the current password for each command.
Keep replacement password files regular, private, and free of symlinks.

Child `HOME` is set to `/var/empty`.
Supply S3 credentials through `AWS_ACCESS_KEY_ID`, `AWS_SECRET_ACCESS_KEY`, and optional `AWS_SESSION_TOKEN` in private `env_files`.
S3 configuration requires a nonempty key pair. Instance-role credentials and anonymous access are unsupported.
The server rejects `AWS_PROFILE` and `AWS_SHARED_CREDENTIALS_FILE`.
Shared credential profiles can execute helpers through `credential_process`, which violates the restricted process contract.

On Unix, secret files are opened with `O_NOFOLLOW`, checked on the open file
descriptor, and must be mode `0600` or stricter. Windows builds revalidate file
type and size, but ACL enforcement remains the operator's responsibility.
