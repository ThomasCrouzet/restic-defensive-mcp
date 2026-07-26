# Security Policy

## Supported versions

| Version | Supported |
|---------|-----------|
| 0.1.x   | Yes       |

## Reporting a vulnerability

Please use GitHub private vulnerability reporting. If it is unavailable,
contact the maintainer through a private channel listed on their GitHub
profile. Do not open public issues for unfixed vulnerabilities that could
expose backup metadata.

## Threat model

`restic-defensive-mcp` is a local stdio MCP server. The MCP client process is
treated as potentially compromised, confused, or hostile.

### Assets

- Restic repository integrity (must not be mutated by this server)
- Repository passwords and backend credentials
- Snapshot metadata: hosts, tags, paths, file names, sizes, timestamps
- Local cache contents created by restic

### Trust boundaries

1. **MCP client (untrusted)** — tool arguments may be adversarial.
2. **YAML config + secret files (operator trusted)** — loaded only at boot.
3. **Restic binary (trusted dependency)** — resolved at boot, argv locked.
4. **Repository data (untrusted content)** — file names and JSON fields from
   the repository are treated as hostile input.
5. **Backends (partially trusted)** — slow or malicious backends can DoS;
   SFTP/rclone are rejected in v0.1 because they spawn external helpers.

### Guarantees (v0.1)

- No MCP tool can run `backup`, `restore`, `forget`, `prune`, `unlock`,
  `repair`, `init`, `dump`, `cat`, or any other mutating/content-extraction
  restic command.
- Callers cannot supply repository URLs, backends, password commands, or
  free-form CLI arguments.
- Repositories are a closed set declared in the config file; repository
  locations and backend credentials are snapshotted at boot.
- Host, tag, and path allowlists are enforced server-side.
- Content and statistics tools select snapshot IDs through the visibility
  policy before running `ls`, `find`, or `stats`.
- Child process environment is rebuilt from an allowlist; parent
  `RESTIC_*` and cloud credentials are not inherited.
- `RESTIC_PASSWORD_COMMAND` is never supported.
- Stdout is MCP JSON-RPC only; operational logs go to stderr.
- Outputs are size-bounded, sanitized for control characters, and redacted
  for common secret patterns.
- Child stderr also redacts exact sealed repository and credential values
  before it can enter a structured error.
- Reaching an output cap cancels the child immediately; it does not merely
  stop buffering while restic continues running.
- Audit logs avoid full file paths by default.
- Pre-restic policy denials (`not_allowed`) and unknown `repository_id` are
  audit-logged as `tool_rejected` events (no full paths).

### Non-guarantees / residual risk

- **Read-only means no intentional repository content mutation.** Restic may
  still create or update a **local cache** and may take **repository locks**
  during inspection commands. This is documented per operation class.
- File names and paths that the operator allowlists are visible to the MCP
  client. Treat the client as able to exfiltrate that metadata.
- **Empty allowlists mean no restriction** for that dimension. An empty
  `allowed_paths` list exposes all snapshot path metadata to the client.
  At boot the server emits `empty_allowlist` warnings on stderr for each
  unrestricted dimension; semantics stay unrestricted (warn, do not fail).
- Backend credential injection uses a scrubbed child env with `HOME=/var/empty`.
  `AWS_PROFILE` alone will not load `~/.aws/credentials`; use env key files or
  `AWS_SHARED_CREDENTIALS_FILE` with an absolute path.
- A compromised host process can still read the same secret files the server
  can read. This server reduces client blast radius; it does not replace OS
  isolation.
- JSON from restic is parsed defensively but a restic bug is out of scope
  for complete containment.
- `repository_stats` is **not** a full integrity check and never runs
  `restic check --read-data`.
- `repository_stats` covers entire snapshots, not a path subtree. When
  `allowed_paths` is configured, snapshots containing any root outside that
  allowlist are excluded (or rejected for an explicit snapshot request).
- Whole-snapshot backup summaries are omitted from snapshot metadata when only
  a narrower path intersection is visible.
- On Windows, Unix mode bits cannot validate secret-file ACLs. Operators must
  restrict configuration secrets with NTFS ACLs.

### Attack scenarios and mitigations

| Scenario | Mitigation |
|----------|------------|
| Client tries repository URL override | Not in schema; registry sealed at boot |
| Client tries path outside allowlist | `not_allowed` before restic runs; audited as `tool_rejected` |
| Client supplies an invisible snapshot ID | Resolved through host/tag/path policy before content access |
| `latest` selects wrong snapshot | Requires host/tag/path disambiguation |
| Hostile file name with ANSI/controls | Sanitized before MCP response |
| Huge restic output | Byte caps; `output_limit_exceeded` |
| Stderr contains password | Redaction + structured error codes |
| Env inheritance of credentials | Scrubbed child env |
| SFTP/rclone helper spawn | Backend rejected at registry build |
| DoS via expensive commands | Timeouts, concurrency=1 per repo, cost labels |
| Symlink secret files | Rejected at open; Unix uses `O_NOFOLLOW` and fd revalidation |
| Indirect mutation via argv | Closed subcommand set + AST test |
| Flag-shaped `find` pattern (`--repo=…`) | Pattern must not start with `-`; argv denylist; `--` end-of-flags |
| Multi-root `find` has ambiguous path scope | Caller must select one allowlisted path |
| Test harness `init`/`backup` | Isolated in `internal/testrepo`; not MCP tools |
| Timeout kill leaves Windows grandchildren | Direct process kill only; supported backends spawn no helpers |

### Local effects of inspection

| Command class | Lock | Cache | Network (remote backends) |
|---------------|------|-------|---------------------------|
| `version` | no | no | no |
| `snapshots` | may lock | may use | yes if remote |
| `ls` | may lock | may use | yes if remote |
| `find` | may lock | may use | yes if remote |
| `stats` | may lock | may use | yes if remote |

Operators who need zero lock interaction should schedule inspections outside
backup windows or use restic features appropriate to their deployment. v0.1
does not pass `--no-lock` by default so reads observe a consistent view when
locks are available.

### Backend support (v0.1)

| Backend | Status | Reason |
|---------|--------|--------|
| local | supported | absolute paths only; no external helper |
| s3 | supported | credentials via env file allowlist |
| b2 | supported | credentials via env file allowlist |
| rest | supported | credentials via env file allowlist |
| sftp | **unsupported** | spawns `ssh` |
| rclone | **unsupported** | spawns `rclone` |
| others | **unsupported** | not reviewed |

## What "read-only" does not mean

It does not mean:

- zero disk writes on the MCP host (cache)
- zero locks on the repository
- cryptographic verification of all pack data
- that the MCP client is denied all sensitive metadata

It does mean:

- no intentional mutation of backup contents or snapshot set via this server
- no free-form restic shell
- no content extraction (`dump`/`restore`)
