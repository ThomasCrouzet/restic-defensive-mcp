# Scan and response budgets

`browse_snapshot` and `find_files` stream restic `ls`/`find` JSON instead of
buffering a complete result. The scanner keeps only the requested page plus a
small amount of framing state. Hidden entries are rejected by policy before page
counting, so they cannot consume the visible page budget or appear in counts.

## Separate limits

| Key | Default | Absolute range | Applies to |
|---|---|---|---|
| `command_timeout` | `30s` | `1s`–`2m` | Ordinary commands (`version`, `snapshots`) |
| `expensive_command_timeout` | `5m` | `10s`–`15m` | `ls`, `find`, `stats`; also the scan elapsed-time ceiling |
| `max_concurrent_per_repo` | `1` | `1`–`4` | Active commands per repository |
| `max_concurrent_global` | `4` | `1`–`16` | Active commands across repositories |
| `max_output_bytes` | `8388608` (8 MiB) | `65536`–`33554432` | Retained stdout/stderr for buffered commands and stderr for scans |
| `max_scan_bytes` | `16777216` (16 MiB) | `65536`–`67108864` | Raw `ls`/`find` stdout consumed by one incremental scan |
| `max_decoded_entries` | `50000` | `1`–`1000000` | Decoded JSON records (nodes, groups, matches) visited by one scan |
| `max_response_bytes` | `2097152` (2 MiB) | `16384`–`16777216` | Encoded public JSON returned to the client |

Values outside the absolute range fail at boot with `config_error`; they are
never silently clamped. `max_nodes` and `max_find_results` remain the public
page-size ceilings and are applied after visibility filtering.

`max_output_bytes` still bounds stderr for streaming scans and the complete
stdout of buffered commands. A scan's raw input is bounded separately by
`max_scan_bytes`, so raising the public page or scan budget does not raise every
other command's memory bound. Existing configurations keep working: every new
key defaults independently.

## Behaviour

- When the visible page is full and one more authorized entry is observed, the
  scan stops and restic is cancelled through the existing graceful process-group
  path. The response is a normal page with `next_cursor`.
- A raw-byte or decoded-entry ceiling stops the scan with `incomplete: true`,
  `budget_exhausted: true`, `truncated: true`, and a warning. It is never
  reported as a complete scan.
- `scan_complete` is `true` only when the authorized stream ended normally.
  `total` is published only for a complete scan; an early page-boundary stop
  leaves `total` absent rather than guessing.
- Malformed or unexpectedly truncated JSON returns `protocol_error`. Timeouts,
  cancellation, authentication failures, locked repositories, and restic exit
  codes keep their existing structured errors and are never converted to an
  empty successful page.
- An encoded response larger than `max_response_bytes` returns
  `response_limit_exceeded`. Lower `limit`, narrow the path/pattern, or raise the
  ceiling within its absolute range.

## Migration guidance

1. Existing `version: 1` files need no change. The new keys are optional.
2. If large searches now report `incomplete: true` with `budget_exhausted`,
   first narrow the path or pattern, then raise `max_scan_bytes` or
   `max_decoded_entries` within their absolute ranges.
3. `max_scan_bytes` is the scan input ceiling; `max_output_bytes` still caps
   stderr and non-streaming command output. Do not lower `max_output_bytes`
   below the stderr volume your restic backend emits on success.
4. `max_response_bytes` is a hard public payload ceiling. A client that needs
   larger pages must request a smaller `limit` and continue with `next_cursor`.
5. Restart the server after changing limits. The repository registry seals
   limits at boot.

## Reproducible local baseline

The benchmark below builds a synthetic 10,000-record stream and requests a
50-row page. It reports elapsed time, raw bytes consumed before the page
boundary, allocated working memory, and allocations. A retained page is bounded
by page size; it does not grow with the synthetic stream.

```bash
TMPDIR=/srv/dev/projects/.opencode-tmp \
GOTMPDIR=/srv/dev/projects/.opencode-tmp \
go test ./internal/restic -run '^$' -bench 'BenchmarkScan' -benchmem -count=3
```

Representative local result (Linux amd64, AMD Ryzen 7 5800H, Go 1.27.3):

| Scan | Page rows | Raw bytes read | Allocated per page | Allocations | Elapsed |
|---|---|---|---|---|---|
| `ls` | 50 | 65,536 | ~102 KiB | ~738 | ~0.83 ms |
| `find` | 50 | 8,295 | ~34 KiB | ~555 | ~0.46 ms |

`find` reads only the matches needed to establish the page because its group's
`matches` array is decoded token by token. `ls` reads one scanner buffer
(64 KiB) before the boundary is known. Both numbers are far below the 16 MiB
default scan ceiling for a 10,000-record stream.

Process peak RSS was sampled from `/proc/<pid>/status` (`VmHWM`) while running
each benchmark for 20,000 iterations: about 16 MiB for `find` and 32 MiB for
`ls`, including the Go test runtime and the synthetic input buffer. The
retained page state itself is the `Allocated per page` column.

## Disposable large-directory E2E fixture

`TestExecutableIncrementalScanBudget` (integration tag) builds a temporary
repository with 200 hidden and 200 visible files under lexically adjacent roots,
adds one mixed snapshot, and runs the real server executable. It verifies:

- a 10-match page returns only visible paths even though every hidden path sorts
  first;
- continuation pages reach all 200 visible matches exactly once;
- `response_limit_exceeded` is an error rather than a truncated success;
- an exhausted decoded-entry budget is reported with `incomplete`,
  `budget_exhausted`, and `truncated`.

Run it with:

```bash
TMPDIR=/srv/dev/projects/.opencode-tmp \
GOTMPDIR=/srv/dev/projects/.opencode-tmp \
GOMAXPROCS=2 GOFLAGS=-p=1 \
go test -tags=integration -count=1 -timeout 8m \
  -run '^TestExecutableIncrementalScanBudget$' -v .
```

The fixture removes its temporary repository after the run. Keep any local logs
outside the repository; do not upload reports, traces, or transcripts.
