# Remote backend validation

This project inspects pre-existing restic repositories. Remote backend support
therefore has two separate questions:

1. Does configuration loading, credential allowlisting, redaction, and
   process isolation behave correctly without contacting a backend?
2. Does restic interoperability work against the named service, including its
   authentication, TLS, lock, and network-failure behavior?

The automated suite answers the second question only for the disposable REST
and S3-compatible services below. It does not claim native Backblaze B2
validation. No cloud account is required for the automated jobs.

## Disposable services

Services are pinned by OCI manifest digest. Docker verifies the digest during
pull, which binds the run to reviewed image content. The tests use only synthetic
credentials and temporary data under `TMPDIR`.

| Service | Version | Image manifest | Purpose |
|---|---|---|---|
| restic REST server | 0.14.0 | `docker.io/restic/rest-server@sha256:d2aff06f47eb38637dff580c3e6bce4af98f386c396a25d32eb6727ec96214a5` | Plain HTTP REST backend with htpasswd authentication |
| Versity S3 Gateway | v1.10.0 | `docker.io/versity/versitygw@sha256:89cad13cc981986ee284cbafea4613f9533ce6fd5f37a2c96c4c4fb8adb3a316` | S3-compatible gateway with a POSIX filesystem backend |

The gateway runs as the invoking user and writes into a temporary bind mount.
Cleanup force-removes the container and then removes the fixture tree. Tests
assert that the container is gone and the temporary directories no longer exist
after both successful and failed runs.

## Compatibility matrix

| Backend | Automated scenarios with real restic and executable MCP over stdio | Residual service-specific limitations |
|---|---|---|
| REST | Snapshot listing, metadata, browse, find, and statistics with host/tag/path filtering; hidden tag and path denials; missing repository (exit 10); invalid htpasswd credentials; self-signed TLS rejection; delayed response timeout; interrupted response; cancellation with lock cleanup and slot recovery; exclusive-lock contention; container and data cleanup | rest-server 0.14.0 over plain HTTP for success cases. The production server exposes no CA override, so successful TLS is not automated; the TLS case is a self-signed rejection. One htpasswd user and one repository prefix per run. Docker on Linux only. |
| S3-compatible | The same success and policy scenarios; missing repository prefix (exit 10); access denial from a wrong secret key; self-signed TLS rejection; delayed response timeout; cancellation with lock cleanup and slot recovery; exclusive-lock contention; container and data cleanup | Versity S3 Gateway with a single bucket and path-style addressing. No multipart/large-object, bucket-policy, IAM-policy, lifecycle, versioning, object-lock, or multi-region cases. Restic retries 403 and 404 responses; a short command timeout may therefore surface `timeout` or `internal_error` rather than an authentication-specific code. This is AWS S3 protocol compatibility, not a claim about every S3 vendor. |
| Backblaze B2 | Configuration acceptance, environment allowlisting, and credential redaction only. No B2 endpoint is contacted. | Native B2 service validation is **not automated**. It needs an explicitly configured test account and bucket; see [B2 validation levels](#b2-validation-levels). |

The remote workflow uses restic **0.19.1**. The ordinary local integration job
also runs restic **0.17.1** and **0.19.1**; it does not start remote services.

## Safe failure mapping

Failures are converted to the documented MCP error envelope. Backend response
bodies, repository URLs, credential values, and secret-file paths are not
returned to clients or written to audit logs.

| Condition | Typical restic result | MCP code |
|---|---|---|
| Missing repository or missing repository prefix | exit 10 | `repository_unavailable` |
| Wrong repository password | exit 12 | `authentication_failed` |
| Exclusive repository lock | exit 11 | `repository_locked` |
| REST authentication denial, TLS verification failure, interrupted response | exit 1 | `internal_error` |
| Backend denial or missing bucket retried until the command deadline | killed at deadline | `timeout` |
| Client cancellation | process cancelled | `cancelled` |

A missing S3 bucket is retried by restic rather than reported as exit 10. The
suite asserts the deterministic missing-prefix case and documents the retry
behavior instead of turning it into a vendor-specific claim.

## B2 validation levels

B2 appears in three distinct levels. Do not collapse them into a single
"B2 supported" claim.

| Level | Status | Evidence |
|---|---|---|
| Configuration acceptance | Automated | `TestExecutableB2ConfigurationBoundary` loads a `b2:` location with synthetic `B2_ACCOUNT_ID` and `B2_ACCOUNT_KEY`, starts the executable, and lists the repository as `b2` without contacting Backblaze. |
| Local failure checks | Automated | Configuration validation, secret-file checks, `env_files` key allowlisting, and audit/protocol redaction are covered by the normal and remote suites. These checks do not establish B2 protocol compatibility. |
| Actual service validation | Manual, opt-in, never required by CI | Requires an explicitly configured disposable B2 bucket and application key. Follow the operator procedure below. |

### Manual B2 procedure

Run this only with a dedicated, non-production bucket and a key limited to it.
Never commit credentials or paste them into CI configuration.

1. Create a disposable B2 bucket outside this server, using restic directly to
   create a test repository and one synthetic snapshot. This server never creates
   repositories or backups.
2. Create private `0600` files containing the repository location, password, and
   an `env_files` entry with `B2_ACCOUNT_ID` and `B2_ACCOUNT_KEY`.
3. Point a local configuration at those files, run
   `--validate-config`, then start the server and call `list_snapshots`,
   `browse_snapshot`, `find_files`, and `repository_stats`.
4. Confirm that responses and stderr contain no location, key ID, application
   key, or secret-file path, and that a wrong key produces only a bounded safe
   error.
5. Delete the disposable bucket, key, and local secret files.

Record the service, restic version, commands, and results locally. Do not upload
credentials, transcripts, or reports to GitHub.

## Running the automated suite

Docker and restic 0.17.1 or later are required. The suite is opt-in so the
ordinary integration run stays fast:

```bash
RESTIC_MCP_REMOTE_TESTS=1 make integration-remote
```

or directly:

```bash
RESTIC_MCP_REMOTE_TESTS=1 \
  go test -tags=integration -count=1 -timeout 12m \
  -run '^(TestExecutableRemoteRESTBackend|TestExecutableRemoteS3Backend|TestExecutableB2ConfigurationBoundary)$' \
  -v .
```

If Docker is unavailable, the tests skip with an explicit reason outside CI.
In CI the workflow fails instead of skipping.

The `.github/workflows/remote-backends.yml` job runs only when Go source,
`go.mod`, `go.sum`, `Makefile`, or that workflow changes. Documentation-only
changes do not start it. Test artifacts stay in `RUNNER_TEMP` and are never
uploaded. Set `RESTIC_MCP_ARTIFACT_DIR` to keep the local protocol transcript,
run metadata, stderr log, and checksums outside the repository.
