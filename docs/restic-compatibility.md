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

Tested during development with **restic 0.19.1**.

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
single requested path locally. When a repository exposes multiple allowed
roots, the caller must choose one explicitly.

## Process lifecycle (timeouts)

- **Unix:** restic is started in its own process group (`Setpgid`); timeouts
  send `SIGKILL` to the group.
- **Windows:** restic is started with `CREATE_NEW_PROCESS_GROUP`; timeouts
  kill the direct restic process only (no full job-object tree walk in v0.1).
  Supported backends do not spawn `ssh`/`rclone` helpers.
- Reaching the stdout or stderr byte cap cancels the child immediately.

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

Child `HOME` is set to `/var/empty`. Do not rely on `AWS_PROFILE` loading credentials from
the operator home directory; pass keys (or `AWS_SHARED_CREDENTIALS_FILE` pointing at an
absolute path) via `env_files`.

On Unix, secret files are opened with `O_NOFOLLOW`, checked on the open file
descriptor, and must be mode `0600` or stricter. Windows builds revalidate file
type and size, but ACL enforcement remains the operator's responsibility.
