//go:build integration

package main_test

// Failure modes covered by these executable E2E tests:
//
//   - A disposable REST or S3-compatible service cannot be started, cannot be
//     reached through restic, or leaves a container behind.
//   - Remote snapshots, browsing, search, or statistics bypass host/tag/path
//     policy filtering.
//   - Backend authentication denial, a missing repository, invalid TLS, a slow
//     response, an interrupted response, or cancellation returns raw service
//     diagnostics, URLs, credentials, or secret-file paths.
//   - A timeout or cancellation leaks the repository concurrency slot, leaves a
//     normal cleanup lock behind, or prevents later inspections.
//   - Lock contention returns a misleading success or unsafe backend detail.
//   - Repository data or disposable services survive a successful or failed run.
//
// The tests drive the real executable over stdio. Only internal/testrepo runs
// init/backup/check, and only synthetic credentials reach the local services.

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/ThomasCrouzet/restic-defensive-mcp/internal/testrepo"
)

const remoteTestsEnv = "RESTIC_MCP_REMOTE_TESTS"

func TestExecutableRemoteRESTBackend(t *testing.T) {
	requireRemoteBackends(t)
	artifact, ctx, binary := startRemoteArtifact(t)

	serviceRoot := t.TempDir()
	service, err := testrepo.StartREST(ctx, serviceRoot)
	if err != nil {
		t.Fatalf("start disposable REST service: %v", err)
	}
	artifact.environment["rest_service"] = service.Version
	artifact.environment["rest_service_image"] = service.Image
	t.Cleanup(func() {
		if err := service.Stop(context.Background()); err != nil {
			t.Errorf("stop REST service: %v", err)
		}
	})

	resticBinary := artifact.environment["restic_binary"]
	fixture, err := testrepo.CreateRemote(ctx, resticBinary, service.RepositoryURL("repo"), service.RESTEnv())
	if err != nil {
		t.Fatalf("prepare remote fixture: %v", err)
	}
	artifact.fixture = fixture.Describe()
	t.Cleanup(func() {
		if err := fixture.Close(); err != nil {
			t.Errorf("close remote fixture: %v", err)
		}
	})

	configPath := filepath.Join(fixture.Root, "rest-success.yaml")
	if err := fixture.WriteRemoteConfig(configPath, resticBinary, remotePolicy(fixture, "", 30*time.Second)); err != nil {
		t.Fatal(err)
	}
	server := startRemoteServer(t, ctx, binary, configPath)
	defer server.Close(t)
	selectedID := assertRemoteSuccess(t, ctx, artifact, server.session, fixture, "rest")

	proxy, err := testrepo.StartProxy("http://" + service.Endpoint)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(proxy.Close)
	forbidden := forbiddenRemoteValues(fixture, service,
		"synthetic-invalid-rest-password", "synthetic-invalid-rest-user")

	// Missing repository: the service is reachable and authenticated, but the
	// prefix has no restic config. restic returns exit code 10.
	missingConfig := writeRemoteFailureConfig(t, fixture, resticBinary, "rest-missing",
		service.RepositoryURL("missing"), service.RESTEnv(), 30*time.Second)
	assertRemoteError(t, ctx, artifact, binary, missingConfig, forbidden, "repository_unavailable")
	artifact.check("REST missing repository returns repository_unavailable")

	// Service-level authentication denial. restic maps an HTTP 401 to a generic
	// failure, so the server must not echo the backend response body or URL.
	wrongREST := []string{
		"RESTIC_REST_USERNAME=synthetic-invalid-rest-user",
		"RESTIC_REST_PASSWORD=synthetic-invalid-rest-password",
	}
	deniedConfig := writeRemoteFailureConfig(t, fixture, resticBinary, "rest-denied",
		service.RepositoryURL("repo"), wrongREST, 5*time.Second)
	assertRemoteError(t, ctx, artifact, binary, deniedConfig, forbidden, "internal_error", "timeout")
	artifact.check("REST invalid credentials return a safe bounded error")

	// Self-signed TLS is rejected by restic. No custom CA is accepted from
	// callers or from the scrubbed child environment.
	tlsConfig := writeRemoteFailureConfig(t, fixture, resticBinary, "rest-tls",
		proxyTLSRepositoryURL(service, proxy, "repo"), service.RESTEnv(), 5*time.Second)
	assertRemoteError(t, ctx, artifact, binary, tlsConfig, forbidden, "internal_error", "timeout")
	artifact.check("REST invalid TLS returns a safe bounded error")

	// A delayed response must hit the configured command timeout and then free
	// the repository slot for the next request on the same server process.
	proxy.SetRules(testrepo.ProxyRule{Delay: 6 * time.Second})
	slowConfig := writeRemoteFailureConfig(t, fixture, resticBinary, "rest-slow",
		proxyRepositoryURL(service, proxy, "repo"), service.RESTEnv(), 3*time.Second)
	assertRemoteTimeoutRecovers(t, ctx, artifact, binary, slowConfig, forbidden, proxy,
		"REST slow response", "timeout")

	// An interrupted response is retried by restic. The bounded command timeout
	// must terminate it without leaking diagnostics or retaining the slot.
	proxy.SetRules(testrepo.ProxyRule{Interrupt: true})
	interruptedConfig := writeRemoteFailureConfig(t, fixture, resticBinary, "rest-interrupted",
		proxyRepositoryURL(service, proxy, "repo"), service.RESTEnv(), 3*time.Second)
	assertRemoteTimeoutRecovers(t, ctx, artifact, binary, interruptedConfig, forbidden, proxy,
		"REST interrupted response", "timeout", "internal_error")

	// Cancellation while restic waits on a data response. The proxy stalls only
	// GET/HEAD data reads, so restic can still remove its lock on cancellation.
	proxy.SetRules(testrepo.ProxyRule{
		Methods:      []string{"GET", "HEAD"},
		PathContains: "/data/",
		Stall:        true,
	})
	cancelConfig := writeRemoteFailureConfig(t, fixture, resticBinary, "rest-cancel",
		proxyRepositoryURL(service, proxy, "repo"), service.RESTEnv(), 10*time.Second)
	assertRemoteCancellation(t, ctx, artifact, binary, fixture, service, proxy, cancelConfig, selectedID)
	proxy.SetRules()
	artifact.check("REST cancellation cleaned up the lock and repository slot")

	// Exclusive lock contention from a read-only restic check. The check runs
	// through a delayed proxy to keep the lock observable.
	proxy.SetRules(testrepo.ProxyRule{Methods: []string{"GET", "HEAD"}, Delay: 5 * time.Second})
	holder, err := fixture.HoldExclusiveLock(ctx, proxyRepositoryURL(service, proxy, "repo"), service.RESTEnv())
	if err != nil {
		t.Fatalf("start exclusive lock holder: %v", err)
	}
	defer func() { _ = holder.Release() }()
	assertRemoteLockContention(t, ctx, artifact, binary, resticBinary, fixture, service, holder)
	proxy.SetRules()
	artifact.check("REST lock contention returned repository_locked and recovered")

	server.Close(t)
	assertNoForbiddenText(t, "REST server stderr", server.stderrText(), forbidden)
	assertRemoteCleanup(t, ctx, service, serviceRoot, fixture)
}

func TestExecutableRemoteS3Backend(t *testing.T) {
	requireRemoteBackends(t)
	artifact, ctx, binary := startRemoteArtifact(t)

	serviceRoot := t.TempDir()
	service, err := testrepo.StartS3(ctx, serviceRoot, "testbucket")
	if err != nil {
		t.Fatalf("start disposable S3 service: %v", err)
	}
	artifact.environment["s3_service"] = service.Version
	artifact.environment["s3_service_image"] = service.Image
	t.Cleanup(func() {
		if err := service.Stop(context.Background()); err != nil {
			t.Errorf("stop S3 service: %v", err)
		}
	})

	resticBinary := artifact.environment["restic_binary"]
	fixture, err := testrepo.CreateRemote(ctx, resticBinary, service.RepositoryURL("repo"), service.S3Env())
	if err != nil {
		t.Fatalf("prepare remote fixture: %v", err)
	}
	artifact.fixture = fixture.Describe()
	t.Cleanup(func() {
		if err := fixture.Close(); err != nil {
			t.Errorf("close remote fixture: %v", err)
		}
	})

	configPath := filepath.Join(fixture.Root, "s3-success.yaml")
	if err := fixture.WriteRemoteConfig(configPath, resticBinary, remotePolicy(fixture, "", 30*time.Second)); err != nil {
		t.Fatal(err)
	}
	server := startRemoteServer(t, ctx, binary, configPath)
	defer server.Close(t)
	selectedID := assertRemoteSuccess(t, ctx, artifact, server.session, fixture, "s3")

	proxy, err := testrepo.StartProxy("http://" + service.Endpoint)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(proxy.Close)
	forbidden := forbiddenRemoteValues(fixture, service, "synthetic-invalid-s3-secret")

	// Missing repository prefix in an existing bucket. A missing bucket is
	// retried by restic and is covered by the access-denial timeout case.
	missingConfig := writeRemoteFailureConfig(t, fixture, resticBinary, "s3-missing",
		service.RepositoryURL("missing"), service.S3Env(), 30*time.Second)
	assertRemoteError(t, ctx, artifact, binary, missingConfig, forbidden, "repository_unavailable")
	artifact.check("S3 missing repository returns repository_unavailable")

	// Backend access denial: restic retries a 403 signature error with backoff,
	// so a one-second command timeout bounds it as a timeout. The client never
	// sees the gateway response body or the wrong credential value.
	deniedConfig := writeRemoteFailureConfig(t, fixture, resticBinary, "s3-denied",
		service.RepositoryURL("repo"), service.S3EnvWithSecret("synthetic-invalid-s3-secret"), time.Second)
	assertRemoteError(t, ctx, artifact, binary, deniedConfig, forbidden, "timeout", "internal_error")
	artifact.check("S3 access denial is bounded and returns a safe error")

	tlsConfig := writeRemoteFailureConfig(t, fixture, resticBinary, "s3-tls",
		proxyTLSRepositoryURL(service, proxy, "repo"), service.S3Env(), 5*time.Second)
	assertRemoteError(t, ctx, artifact, binary, tlsConfig, forbidden, "internal_error", "timeout")
	artifact.check("S3 invalid TLS returns a safe bounded error")

	proxy.SetRules(testrepo.ProxyRule{Delay: 6 * time.Second})
	slowConfig := writeRemoteFailureConfig(t, fixture, resticBinary, "s3-slow",
		proxyRepositoryURL(service, proxy, "repo"), service.S3Env(), 3*time.Second)
	assertRemoteTimeoutRecovers(t, ctx, artifact, binary, slowConfig, forbidden, proxy,
		"S3 slow response", "timeout")

	proxy.SetRules(testrepo.ProxyRule{
		Methods:      []string{"GET", "HEAD"},
		PathContains: "/data/",
		Stall:        true,
	})
	cancelConfig := writeRemoteFailureConfig(t, fixture, resticBinary, "s3-cancel",
		proxyRepositoryURL(service, proxy, "repo"), service.S3Env(), 10*time.Second)
	assertRemoteCancellation(t, ctx, artifact, binary, fixture, service, proxy, cancelConfig, selectedID)
	proxy.SetRules()
	artifact.check("S3 cancellation cleaned up the lock and repository slot")

	proxy.SetRules(testrepo.ProxyRule{Methods: []string{"GET", "HEAD"}, Delay: 5 * time.Second})
	holder, err := fixture.HoldExclusiveLock(ctx, proxyRepositoryURL(service, proxy, "repo"), service.S3Env())
	if err != nil {
		t.Fatalf("start exclusive lock holder: %v", err)
	}
	defer func() { _ = holder.Release() }()
	assertRemoteLockContention(t, ctx, artifact, binary, resticBinary, fixture, service, holder)
	proxy.SetRules()
	artifact.check("S3 lock contention returned repository_locked and recovered")

	server.Close(t)
	assertNoForbiddenText(t, "S3 server stderr", server.stderrText(), forbidden)
	assertRemoteCleanup(t, ctx, service, serviceRoot, fixture)
}

// TestExecutableB2ConfigurationBoundary records the B2 claim boundary without
// contacting Backblaze. It proves configuration acceptance and secret redaction
// only; it is not service-level B2 validation.
func TestExecutableB2ConfigurationBoundary(t *testing.T) {
	if os.Getenv(remoteTestsEnv) != "1" {
		t.Skipf("remote backend integration disabled; set %s=1", remoteTestsEnv)
	}
	artifact, ctx, binary := startRemoteArtifact(t)
	resticBinary := artifact.environment["restic_binary"]

	// Prepare the password, policy roots, and cache through the local fixture
	// boundary, then point only the configuration at B2. No B2 repository init
	// or tool call is attempted.
	fixture, err := testrepo.Create(ctx, resticBinary)
	if err != nil {
		t.Fatalf("prepare local configuration fixture: %v", err)
	}
	artifact.fixture = fixture.Describe()
	defer func() {
		if err := fixture.Close(); err != nil {
			t.Errorf("close B2 fixture: %v", err)
		}
	}()

	configPath := filepath.Join(fixture.Root, "b2.yaml")
	if err := fixture.WriteConfigForRepository(configPath, resticBinary, testrepo.SyntheticB2Location,
		testrepo.SyntheticB2Env(), remotePolicy(fixture, "", 30*time.Second)); err != nil {
		t.Fatal(err)
	}

	// Starting the server probes only the restic binary. It must accept the B2
	// location and B2_* keys without contacting a cloud account.
	server := startRemoteServer(t, ctx, binary, configPath)
	repos := callE2ETool(t, ctx, server.session, artifact, "list_repositories", map[string]any{})
	var listed []struct {
		ID      string `json:"id"`
		Backend string `json:"backend"`
	}
	unmarshalData(t, repos, &listed)
	if len(listed) != 1 || listed[0].ID != "tmp-local" || listed[0].Backend != "b2" {
		t.Fatalf("B2 repository listing = %+v", listed)
	}
	server.Close(t)

	// The public repository listing intentionally includes allowlisted roots, so
	// only backend locations and credential values must stay absent here.
	text := artifact.transcriptText() + server.stderrText()
	for _, value := range []string{
		testrepo.SyntheticB2AccountID,
		testrepo.SyntheticB2AccountKey,
		testrepo.SyntheticB2Location,
	} {
		if value != "" && strings.Contains(text, value) {
			t.Fatalf("B2 configuration boundary leaked forbidden value %q", value)
		}
	}
	artifact.check("B2 configuration accepted without network access")
	artifact.check("B2 synthetic credentials excluded from protocol and stderr")
}

func requireRemoteBackends(t *testing.T) {
	t.Helper()
	if os.Getenv(remoteTestsEnv) != "1" {
		t.Skipf("remote backend integration disabled; set %s=1", remoteTestsEnv)
	}
	if _, err := exec.LookPath("restic"); err != nil {
		t.Fatalf("remote backend integration requires restic: %v", err)
	}
	if _, err := exec.LookPath("docker"); err != nil {
		if os.Getenv("CI") != "" {
			t.Fatalf("remote backend integration requires Docker in CI: %v", err)
		}
		t.Skipf("remote backend integration requires Docker: %v", err)
	}
	if output, err := exec.Command("docker", "info").CombinedOutput(); err != nil {
		if os.Getenv("CI") != "" {
			t.Fatalf("Docker daemon unavailable in CI: %v: %s", err, output)
		}
		t.Skipf("Docker daemon unavailable: %v", err)
	}
}

func startRemoteArtifact(t *testing.T) (*e2eArtifact, context.Context, string) {
	t.Helper()
	artifact := newE2EArtifact(t)
	t.Cleanup(func() { artifact.finish(t, "") })
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	t.Cleanup(cancel)

	resticBinary, err := exec.LookPath("restic")
	if err != nil {
		t.Fatalf("remote E2E requires restic: %v", err)
	}
	artifact.environment["restic_binary"] = resticBinary
	artifact.environment["restic_version"] = commandText(t, resticBinary, "version")
	artifact.environment["docker_version"] = commandText(t, "docker", "version", "--format", "{{.Server.Version}}")

	binary, expectedVersion, source := selectE2EBinary(t, ctx)
	serverVersion := assertVersionFlag(t, ctx, artifact, binary, expectedVersion)
	artifact.environment["server_binary"] = binary
	artifact.environment["server_binary_source"] = source
	artifact.environment["server_version"] = serverVersion
	return artifact, ctx, binary
}

func remotePolicy(fixture *testrepo.Fixture, cacheDir string, commandTimeout time.Duration) testrepo.ConfigPolicy {
	if commandTimeout < time.Second {
		commandTimeout = time.Second
	}
	expensive := 60 * time.Second
	if commandTimeout > expensive {
		expensive = commandTimeout
	}
	return testrepo.ConfigPolicy{
		AllowedHosts:            []string{fixture.Host},
		AllowedTags:             []string{"daily", "test"},
		AllowedPaths:            []string{fixture.DataDir1},
		CacheDir:                cacheDir,
		CommandTimeout:          commandTimeout,
		ExpensiveCommandTimeout: expensive,
	}
}

func writeRemoteFailureConfig(
	t *testing.T,
	fixture *testrepo.Fixture,
	resticBinary, name, location string,
	credentials []string,
	commandTimeout time.Duration,
) string {
	t.Helper()
	cacheDir := filepath.Join(fixture.Root, "cache-"+name)
	if err := os.MkdirAll(cacheDir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(fixture.Root, name+".yaml")
	if err := fixture.WriteConfigForRepository(path, resticBinary, location, credentials,
		remotePolicy(fixture, cacheDir, commandTimeout)); err != nil {
		t.Fatal(err)
	}
	return path
}

func assertRemoteSuccess(
	t *testing.T,
	ctx context.Context,
	artifact *e2eArtifact,
	session *mcp.ClientSession,
	fixture *testrepo.Fixture,
	backend string,
) string {
	t.Helper()
	var repositories []struct {
		ID      string `json:"id"`
		Backend string `json:"backend"`
	}
	unmarshalData(t, callE2ETool(t, ctx, session, artifact, "list_repositories", map[string]any{}), &repositories)
	if len(repositories) != 1 || repositories[0].ID != "tmp-local" || repositories[0].Backend != backend {
		t.Fatalf("repository listing = %+v, want backend %q", repositories, backend)
	}
	artifact.check(backend + " repository listing")

	page := callSnapshots(t, ctx, session, artifact, map[string]any{"repository_id": "tmp-local"})
	if page.Total != 1 || page.Count != 1 || len(page.Snapshots) != 1 {
		t.Fatalf("%s visible snapshots = %+v, want exactly one", backend, page)
	}
	selected := page.Snapshots[0]
	if !slices.Equal(selected.Paths, []string{fixture.DataDir1}) {
		t.Fatalf("%s snapshot paths = %v, want filtered %s", backend, selected.Paths, fixture.DataDir1)
	}
	artifact.check(backend + " snapshot policy filtering")

	var snapshotSnapshot struct {
		ID    string   `json:"id"`
		Paths []string `json:"paths"`
	}
	unmarshalData(t, callE2ETool(t, ctx, session, artifact, "get_snapshot", map[string]any{
		"repository_id": "tmp-local",
		"snapshot_id":   selected.ID,
	}), &snapshotSnapshot)
	if snapshotSnapshot.ID != selected.ID || !slices.Equal(snapshotSnapshot.Paths, selected.Paths) {
		t.Fatalf("%s get_snapshot = %+v", backend, snapshotSnapshot)
	}

	var browse e2eBrowseData
	unmarshalData(t, callE2ETool(t, ctx, session, artifact, "browse_snapshot", map[string]any{
		"repository_id": "tmp-local",
		"snapshot_id":   selected.ID,
		"path":          fixture.DataDir1,
		"limit":         100,
	}), &browse)
	if browse.Path != fixture.DataDir1 || browse.Count < 2 || len(browse.Nodes) != browse.Count {
		t.Fatalf("%s browse = %+v", backend, browse)
	}
	names := make([]string, 0, len(browse.Nodes))
	for _, node := range browse.Nodes {
		if !strings.HasPrefix(node.Path, fixture.DataDir1) {
			t.Fatalf("%s browse escaped the policy root: %+v", backend, node)
		}
		names = append(names, node.Name)
	}
	if !slices.Contains(names, "hello.txt") {
		t.Fatalf("%s browse names omit hello.txt: %v", backend, names)
	}
	artifact.check(backend + " browse policy filtering")

	var find e2eFindData
	unmarshalData(t, callE2ETool(t, ctx, session, artifact, "find_files", map[string]any{
		"repository_id": "tmp-local",
		"pattern":       "*.txt",
		"snapshot_id":   selected.ID,
		"path":          fixture.DataDir1,
	}), &find)
	if find.Count != 3 || len(find.Groups) != 1 || len(find.Groups[0].Matches) != 3 {
		t.Fatalf("%s find = %+v", backend, find)
	}
	for _, match := range find.Groups[0].Matches {
		if !strings.HasPrefix(match.Path, fixture.DataDir1) {
			t.Fatalf("%s find escaped the policy root: %+v", backend, match)
		}
	}
	artifact.check(backend + " find policy filtering")

	var stats e2eStatsData
	unmarshalData(t, callE2ETool(t, ctx, session, artifact, "repository_stats", map[string]any{
		"repository_id": "tmp-local",
		"mode":          "restore-size",
		"snapshot_id":   selected.ID,
	}), &stats)
	if stats.SelectedSnapshots != 1 || stats.Stats.TotalFileCount == 0 || stats.Stats.TotalSize == 0 {
		t.Fatalf("%s stats = %+v", backend, stats)
	}
	artifact.check(backend + " statistics policy filtering")

	hiddenTags := callE2EToolError(t, ctx, session, artifact, "list_snapshots", map[string]any{
		"repository_id": "tmp-local",
		"tags":          []string{"weekly"},
	})
	if hiddenTags["code"] != "not_allowed" {
		t.Fatalf("%s hidden tag error = %v", backend, hiddenTags)
	}
	hiddenBrowse := callE2EToolError(t, ctx, session, artifact, "browse_snapshot", map[string]any{
		"repository_id": "tmp-local",
		"snapshot_id":   selected.ID,
		"path":          fixture.DataDir2,
	})
	if hiddenBrowse["code"] != "not_allowed" {
		t.Fatalf("%s hidden browse error = %v", backend, hiddenBrowse)
	}
	hiddenFind := callE2EToolError(t, ctx, session, artifact, "find_files", map[string]any{
		"repository_id": "tmp-local",
		"pattern":       "*.txt",
		"path":          fixture.DataDir2,
	})
	if hiddenFind["code"] != "not_allowed" {
		t.Fatalf("%s hidden find error = %v", backend, hiddenFind)
	}
	hiddenStats := callE2EToolError(t, ctx, session, artifact, "repository_stats", map[string]any{
		"repository_id": "tmp-local",
		"mode":          "restore-size",
		"path":          fixture.DataDir2,
	})
	if hiddenStats["code"] != "not_allowed" {
		t.Fatalf("%s hidden stats error = %v", backend, hiddenStats)
	}
	artifact.check(backend + " hidden path and tag denials")
	return selected.ID
}

func assertRemoteTimeoutRecovers(
	t *testing.T,
	ctx context.Context,
	artifact *e2eArtifact,
	binary, configPath string,
	forbidden []string,
	proxy *testrepo.Proxy,
	label string,
	wantCodes ...string,
) {
	t.Helper()
	server := startRemoteServer(t, ctx, binary, configPath)
	defer server.Close(t)
	start := time.Now()
	payload := callE2EToolError(t, ctx, server.session, artifact, "list_snapshots", map[string]any{
		"repository_id": "tmp-local",
	})
	elapsed := time.Since(start)
	code := payload["code"]
	if !slices.Contains(wantCodes, code) {
		t.Fatalf("%s error code = %q, want one of %v: %v", label, code, wantCodes, payload)
	}
	if elapsed > 10*time.Second {
		t.Fatalf("%s exceeded the bounded failure window: %s", label, elapsed)
	}
	assertNoForbiddenText(t, label+" error payload", payload["code"]+payload["message"]+payload["detail"], forbidden)

	// Restore a transparent proxy and reuse the same server process. A success
	// proves the failed command returned its repository concurrency slot.
	proxy.SetRules()
	waitForSnapshots(t, ctx, server.session, 1, 15*time.Second)
	server.Close(t)
	assertNoForbiddenText(t, label+" server stderr", server.stderrText(), forbidden)
	artifact.check(label + " failed safely and released repository capacity")
}

func assertRemoteError(
	t *testing.T,
	ctx context.Context,
	artifact *e2eArtifact,
	binary, configPath string,
	forbidden []string,
	wantCodes ...string,
) map[string]string {
	t.Helper()
	server := startRemoteServer(t, ctx, binary, configPath)
	defer server.Close(t)
	payload := callE2EToolError(t, ctx, server.session, artifact, "list_snapshots", map[string]any{
		"repository_id": "tmp-local",
	})
	server.Close(t)
	code := payload["code"]
	if !slices.Contains(wantCodes, code) {
		t.Fatalf("error code = %q, want one of %v: %v", code, wantCodes, payload)
	}
	assertNoForbiddenText(t, "MCP error payload", payload["code"]+payload["message"]+payload["detail"], forbidden)
	assertNoForbiddenText(t, "server stderr", server.stderrText(), forbidden)
	return payload
}

func assertNoForbiddenText(t *testing.T, label, text string, forbidden []string) {
	t.Helper()
	for _, value := range forbidden {
		if value != "" && strings.Contains(text, value) {
			t.Fatalf("%s leaked forbidden value %q: %s", label, value, text)
		}
	}
}

func assertRemoteCancellation(
	t *testing.T,
	ctx context.Context,
	artifact *e2eArtifact,
	binary string,
	fixture *testrepo.Fixture,
	service *testrepo.RemoteService,
	proxy *testrepo.Proxy,
	configPath, snapshotID string,
) {
	t.Helper()
	server := startRemoteServer(t, ctx, binary, configPath)
	defer server.Close(t)

	callCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	type outcome struct {
		result *mcp.CallToolResult
		err    error
	}
	outcomes := make(chan outcome, 1)
	go func() {
		result, err := server.session.CallTool(callCtx, &mcp.CallToolParams{
			Name: "repository_stats",
			Arguments: map[string]any{
				"repository_id": "tmp-local",
				"mode":          "raw-data",
				"snapshot_id":   snapshotID,
			},
		})
		outcomes <- outcome{result: result, err: err}
	}()

	lockDir := service.LockDir("repo")
	waitForLockState(t, lockDir, true, 20*time.Second)
	waitForProxyData(t, proxy, 20*time.Second)
	artifact.check("remote repository lock observed during inspection")

	start := time.Now()
	cancel()
	select {
	case outcome := <-outcomes:
		if outcome.err != nil {
			if !strings.Contains(strings.ToLower(outcome.err.Error()), "cancel") {
				t.Fatalf("cancellation error = %v, want cancelled", outcome.err)
			}
		} else if outcome.result == nil || !outcome.result.IsError {
			t.Fatalf("inspection returned success after cancellation: %+v", outcome.result)
		} else if text := toolText(outcome.result); !strings.Contains(text, `"cancelled"`) {
			t.Fatalf("cancellation payload = %s", text)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("inspection did not return after cancellation")
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("cancellation took %s", elapsed)
	}
	waitForLockState(t, lockDir, false, 15*time.Second)
	artifact.check("remote restic removed its lock during graceful cleanup")

	proxy.SetRules()
	waitForSnapshots(t, ctx, server.session, 1, 15*time.Second)
	server.Close(t)
	assertNoForbiddenText(t, "cancellation server stderr", server.stderrText(), forbiddenRemoteValues(fixture, service))
	artifact.check("remote repository capacity released after cancellation")
}

func assertRemoteLockContention(
	t *testing.T,
	ctx context.Context,
	artifact *e2eArtifact,
	binary, resticBinary string,
	fixture *testrepo.Fixture,
	service *testrepo.RemoteService,
	holder *testrepo.LockHolder,
) {
	t.Helper()
	lockDir := service.LockDir("repo")
	if err := waitForLockStateResult(lockDir, true, 40*time.Second); err != nil {
		_ = holder.Release()
		t.Fatalf("exclusive lock holder did not acquire a lock: %v; restic output: %s", err, holder.Stderr())
	}

	configPath := filepath.Join(fixture.Root, "lock-contention-"+service.Kind+".yaml")
	cacheDir := filepath.Join(fixture.Root, "cache-lock-"+service.Kind)
	if err := os.MkdirAll(cacheDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := fixture.WriteConfigForRepository(configPath, resticBinary, service.RepositoryURL("repo"),
		credentialsForService(service), remotePolicy(fixture, cacheDir, 30*time.Second)); err != nil {
		t.Fatal(err)
	}
	server := startRemoteServer(t, ctx, binary, configPath)
	defer server.Close(t)

	payload := callE2EToolError(t, ctx, server.session, artifact, "list_snapshots", map[string]any{
		"repository_id": "tmp-local",
	})
	if payload["code"] != "repository_locked" {
		t.Fatalf("lock contention error = %v, want repository_locked", payload)
	}
	if err := holder.Release(); err != nil {
		t.Fatalf("release lock holder: %v", err)
	}
	waitForLockState(t, lockDir, false, 20*time.Second)
	waitForSnapshots(t, ctx, server.session, 1, 15*time.Second)
	server.Close(t)
	assertNoForbiddenText(t, "lock contention server stderr", server.stderrText(), forbiddenRemoteValues(fixture, service))
}

func credentialsForService(service *testrepo.RemoteService) []string {
	if service.Kind == "s3" {
		return service.S3Env()
	}
	return service.RESTEnv()
}

func waitForLockState(t *testing.T, lockDir string, present bool, timeout time.Duration) {
	t.Helper()
	if err := waitForLockStateResult(lockDir, present, timeout); err != nil {
		t.Fatal(err)
	}
}

func waitForLockStateResult(lockDir string, present bool, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		entries, err := os.ReadDir(lockDir)
		hasLock := err == nil && len(entries) > 0
		if hasLock == present {
			return nil
		}
		time.Sleep(20 * time.Millisecond)
	}
	state := "appear"
	if !present {
		state = "disappear"
	}
	return &lockStateError{dir: lockDir, state: state, timeout: timeout}
}

type lockStateError struct {
	dir     string
	state   string
	timeout time.Duration
}

func (e *lockStateError) Error() string {
	return "repository lock did not " + e.state + " within " + e.timeout.String() + " in " + e.dir
}

func waitForProxyData(t *testing.T, proxy *testrepo.Proxy, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if proxy.RequestCount("/data/") > 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("inspection never reached the backend data path")
}

func waitForSnapshots(t *testing.T, ctx context.Context, session *mcp.ClientSession, want int, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last string
	for time.Now().Before(deadline) {
		result, err := session.CallTool(ctx, &mcp.CallToolParams{
			Name:      "list_snapshots",
			Arguments: map[string]any{"repository_id": "tmp-local"},
		})
		if err == nil && result != nil && !result.IsError {
			var envelope e2eEnvelope
			if jsonErr := json.Unmarshal([]byte(toolText(result)), &envelope); jsonErr == nil {
				var page e2eSnapshotPage
				if jsonErr := json.Unmarshal(envelope.Data, &page); jsonErr == nil && page.Total == want {
					return
				}
			}
		}
		if result != nil {
			last = toolText(result)
		} else if err != nil {
			last = err.Error()
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("repository did not return %d visible snapshots after recovery: last=%s", want, last)
}

func forbiddenRemoteValues(
	fixture *testrepo.Fixture,
	service *testrepo.RemoteService,
	extra ...string,
) []string {
	values := []string{
		fixture.RepositoryFile,
		fixture.PasswordFile,
		fixture.Root,
		testrepo.SyntheticRESTUsername,
		testrepo.SyntheticRESTPassword,
		testrepo.SyntheticS3AccessKey,
		testrepo.SyntheticS3SecretKey,
	}
	if service != nil {
		values = append(values,
			service.Endpoint,
			"http://"+service.Endpoint,
			"https://"+service.Endpoint,
		)
	}
	return append(values, extra...)
}

func proxyRepositoryURL(service *testrepo.RemoteService, proxy *testrepo.Proxy, prefix string) string {
	prefix = strings.Trim(prefix, "/")
	if service.Kind == "s3" {
		return "s3:" + proxy.URL + "/" + service.Bucket + "/" + prefix
	}
	return "rest:" + proxy.URL + "/" + prefix + "/"
}

func proxyTLSRepositoryURL(service *testrepo.RemoteService, proxy *testrepo.Proxy, prefix string) string {
	prefix = strings.Trim(prefix, "/")
	if service.Kind == "s3" {
		return "s3:" + proxy.TLSURL + "/" + service.Bucket + "/" + prefix
	}
	return "rest:" + proxy.TLSURL + "/" + prefix + "/"
}

func assertRemoteCleanup(
	t *testing.T,
	ctx context.Context,
	service *testrepo.RemoteService,
	serviceRoot string,
	fixture *testrepo.Fixture,
) {
	t.Helper()
	fixtureRoot := fixture.Root
	if err := service.Stop(ctx); err != nil {
		t.Fatalf("stop disposable service: %v", err)
	}
	if !service.ContainerGone(ctx) {
		t.Fatalf("disposable service container %s still exists", service.Container)
	}
	if err := os.RemoveAll(serviceRoot); err != nil {
		t.Fatalf("remove service data: %v", err)
	}
	if _, err := os.Stat(serviceRoot); !os.IsNotExist(err) {
		t.Fatalf("service data survived cleanup: %v", err)
	}
	if err := fixture.Close(); err != nil {
		t.Fatalf("close fixture: %v", err)
	}
	if _, err := os.Stat(fixtureRoot); !os.IsNotExist(err) {
		t.Fatalf("fixture data survived cleanup: %v", err)
	}
}

type remoteServer struct {
	session   *mcp.ClientSession
	cmd       *exec.Cmd
	stderr    bytes.Buffer
	closeOnce sync.Once
}

func startRemoteServer(t *testing.T, ctx context.Context, binary, configPath string) *remoteServer {
	t.Helper()
	cmd := exec.CommandContext(ctx, binary, "--config", configPath)
	cmd.Env = replaceEnvironment(os.Environ(), nil)
	server := &remoteServer{cmd: cmd}
	cmd.Stderr = &server.stderr
	transport := &mcp.CommandTransport{Command: cmd, TerminateDuration: 3 * time.Second}
	client := mcp.NewClient(&mcp.Implementation{Name: "remote-e2e", Version: "1.0"}, nil)
	session, err := client.Connect(ctx, transport, nil)
	if err != nil {
		t.Fatalf("connect to executable: %v\nstderr: %s", err, server.stderr.String())
	}
	server.session = session
	return server
}

func (s *remoteServer) stderrText() string {
	return s.stderr.String()
}

func (s *remoteServer) Close(t *testing.T) {
	t.Helper()
	s.closeOnce.Do(func() {
		if err := s.session.Close(); err != nil {
			t.Errorf("close executable session: %v", err)
		}
		deadline := time.Now().Add(5 * time.Second)
		for s.cmd.ProcessState == nil && time.Now().Before(deadline) {
			time.Sleep(10 * time.Millisecond)
		}
		if s.cmd.ProcessState == nil || !s.cmd.ProcessState.Exited() {
			t.Errorf("server process did not exit: %v\nstderr: %s", s.cmd.ProcessState, s.stderr.String())
		}
	})
}
