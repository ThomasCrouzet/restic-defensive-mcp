//go:build integration

package main_test

// Failure modes covered by this executable E2E test:
//
//   - The built command can reject valid flags or return incorrect exit codes.
//   - Startup can mix operational logs with the stdout JSON-RPC stream.
//   - The stdio transport can fail initialization, tool discovery, or shutdown.
//   - The executable can expose a tool outside the closed seven-tool surface.
//   - Tool responses can be empty, malformed, unbounded, or leak secret material.
//   - Blocked AWS profile keys can enable external credential helpers.
//   - S3 startup can fall back to ambient credentials when static keys are incomplete.
//   - Pagination and an authorized empty result can report incorrect counts.
//   - A policy denial can run restic or return the wrong structured error code.
//   - Unknown repository audit events can expose attacker-controlled identifiers.
//   - Repository location changes can retarget a running server.
//   - Password file changes can fail to affect later restic child processes.
//   - Inspection tools can mutate the fixture snapshot set.
//   - A shutdown can leave the server process running.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/ThomasCrouzet/restic-defensive-mcp/internal/restic"
	"github.com/ThomasCrouzet/restic-defensive-mcp/internal/testrepo"
)

const e2eBinaryVersion = "integration-e2e"

var expectedToolNames = []string{
	"browse_snapshot",
	"find_files",
	"get_snapshot",
	"list_repositories",
	"list_snapshots",
	"repository_stats",
	"restic_capabilities",
}

type e2eEnvelope struct {
	Cost      string          `json:"cost"`
	Truncated bool            `json:"truncated,omitempty"`
	Data      json.RawMessage `json:"data"`
}

type e2eSnapshotPage struct {
	Snapshots []restic.Snapshot `json:"snapshots"`
	Count     int               `json:"count"`
	Total     int               `json:"total"`
	Offset    int               `json:"offset"`
	Limit     int               `json:"limit"`
}

type e2eBrowseData struct {
	Path      string        `json:"path"`
	Nodes     []restic.Node `json:"nodes"`
	Count     int           `json:"count"`
	Recursive bool          `json:"recursive"`
}

type e2eFindData struct {
	Groups []restic.FindGroup `json:"groups"`
	Count  int                `json:"count"`
}

type e2eStatsData struct {
	Mode              string             `json:"mode"`
	Stats             restic.StatsResult `json:"stats"`
	SelectedSnapshots int                `json:"selected_snapshots"`
}

func TestExecutableStdioEndToEnd(t *testing.T) {
	artifact := newE2EArtifact(t)
	var serverStderr bytes.Buffer
	t.Cleanup(func() {
		artifact.finish(t, serverStderr.String())
	})

	resticBinary, err := exec.LookPath("restic")
	if err != nil {
		t.Fatalf("integration E2E requires restic: %v", err)
	}
	artifact.environment["restic_binary"] = resticBinary
	artifact.environment["restic_version"] = commandText(t, resticBinary, "version")

	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	t.Cleanup(cancel)

	binary, expectedServerVersion, binarySource := selectE2EBinary(t, ctx)
	serverVersion := assertVersionFlag(t, ctx, artifact, binary, expectedServerVersion)
	artifact.environment["server_binary"] = binary
	artifact.environment["server_binary_sha256"] = fileSHA256(t, binary)
	artifact.environment["server_binary_source"] = binarySource
	artifact.environment["server_version"] = serverVersion
	assertMissingConfig(t, ctx, artifact, binary)
	assertInvalidConfig(t, ctx, artifact, binary)

	fixture, err := testrepo.Create(ctx, resticBinary)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := fixture.Close(); err != nil {
			t.Errorf("close fixture: %v", err)
		}
	})
	artifact.fixture = fixture.Describe()

	beforeIDs, err := fixture.SnapshotIDs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(beforeIDs) != 2 {
		t.Fatalf("fixture snapshot count = %d, want 2", len(beforeIDs))
	}
	artifact.beforeSnapshotIDs = append([]string(nil), beforeIDs...)

	missingBinaryConfig := filepath.Join(fixture.Root, "missing-binary.yaml")
	if err := fixture.WriteConfig(missingBinaryConfig, filepath.Join(fixture.Root, "missing-restic")); err != nil {
		t.Fatal(err)
	}
	assertMissingResticBinary(t, ctx, artifact, binary, missingBinaryConfig)
	for _, key := range []string{"AWS_PROFILE", "AWS_SHARED_CREDENTIALS_FILE"} {
		blockedConfig := filepath.Join(fixture.Root, "blocked-"+key+".yaml")
		if err := fixture.WriteConfigWithBlockedEnvKey(blockedConfig, resticBinary, key); err != nil {
			t.Fatal(err)
		}
		assertBlockedEnvConfig(t, ctx, artifact, binary, blockedConfig, key)
	}
	accessKey := "synthetic-access-key"
	secretKey := "synthetic-secret-key"
	emptyValue := ""
	s3Cases := []struct {
		name        string
		credentials testrepo.S3Credentials
		wantExit    int
	}{
		{name: "missing-pair", wantExit: 2},
		{name: "access-key-only", credentials: testrepo.S3Credentials{AccessKeyID: &accessKey}, wantExit: 2},
		{name: "secret-key-only", credentials: testrepo.S3Credentials{SecretAccessKey: &secretKey}, wantExit: 2},
		{name: "empty-access-key", credentials: testrepo.S3Credentials{AccessKeyID: &emptyValue, SecretAccessKey: &secretKey}, wantExit: 2},
		{name: "empty-secret-key", credentials: testrepo.S3Credentials{AccessKeyID: &accessKey, SecretAccessKey: &emptyValue}, wantExit: 2},
		{name: "complete-pair", credentials: testrepo.S3Credentials{AccessKeyID: &accessKey, SecretAccessKey: &secretKey}, wantExit: 0},
	}
	for _, testCase := range s3Cases {
		s3Config := filepath.Join(fixture.Root, "s3-"+testCase.name+".yaml")
		if err := fixture.WriteS3Config(s3Config, resticBinary, testCase.credentials); err != nil {
			t.Fatal(err)
		}
		assertS3Startup(t, ctx, artifact, binary, s3Config, testCase.name, testCase.wantExit)
	}

	configPath := filepath.Join(fixture.Root, "server.yaml")
	if err := fixture.WriteConfig(configPath, resticBinary); err != nil {
		t.Fatal(err)
	}

	cmd := exec.CommandContext(ctx, binary, "--config", configPath)
	cmd.Env = replaceEnvironment(os.Environ(), map[string]string{
		"RESTIC_REPOSITORY":     filepath.Join(fixture.Root, "attacker-repository"),
		"RESTIC_PASSWORD":       "synthetic-parent-password",
		"AWS_SECRET_ACCESS_KEY": "synthetic-parent-cloud-key",
	})
	cmd.Stderr = &serverStderr
	transport := &mcp.CommandTransport{Command: cmd, TerminateDuration: 3 * time.Second}
	client := mcp.NewClient(&mcp.Implementation{Name: "executable-e2e", Version: "1.0"}, nil)
	session, err := client.Connect(ctx, transport, nil)
	if err != nil {
		t.Fatalf("connect to executable: %v", err)
	}
	closed := false
	t.Cleanup(func() {
		if !closed {
			_ = session.Close()
		}
	})

	actualTools := collectToolNames(t, ctx, session)
	artifact.record("tools/list", nil, false, mustJSON(t, actualTools))
	if !slices.Equal(actualTools, expectedToolNames) {
		t.Fatalf("tool names = %v, want %v", actualTools, expectedToolNames)
	}
	artifact.check("exact seven-tool surface")

	capabilities := callE2ETool(t, ctx, session, artifact, "restic_capabilities", map[string]any{})
	var capabilityData struct {
		ServerVersion   string   `json:"server_version"`
		ResticVersion   string   `json:"restic_version"`
		RepositoryIDs   []string `json:"repository_ids"`
		Tools           []string `json:"tools"`
		AllowedCommands []string `json:"allowed_restic_commands"`
	}
	unmarshalData(t, capabilities, &capabilityData)
	sort.Strings(capabilityData.Tools)
	if capabilityData.ServerVersion != serverVersion || capabilityData.ResticVersion == "" {
		t.Fatalf("unexpected capabilities: %+v", capabilityData)
	}
	if !slices.Equal(capabilityData.RepositoryIDs, []string{"tmp-local"}) ||
		!slices.Equal(capabilityData.Tools, expectedToolNames) {
		t.Fatalf("unexpected capability surface: %+v", capabilityData)
	}
	if !sameStrings(capabilityData.AllowedCommands, []string{"find", "ls", "snapshots", "stats", "version"}) {
		t.Fatalf("allowed commands = %v", capabilityData.AllowedCommands)
	}
	artifact.check("capabilities")

	repositories := callE2ETool(t, ctx, session, artifact, "list_repositories", map[string]any{})
	var repositoryData []struct {
		ID      string `json:"id"`
		Backend string `json:"backend"`
	}
	unmarshalData(t, repositories, &repositoryData)
	if len(repositoryData) != 1 || repositoryData[0].ID != "tmp-local" || repositoryData[0].Backend != "local" {
		t.Fatalf("repositories = %+v", repositoryData)
	}
	artifact.check("repository listing")

	pageOneEnvelope := callE2ETool(t, ctx, session, artifact, "list_snapshots", map[string]any{
		"repository_id": "tmp-local",
		"limit":         1,
		"offset":        0,
	})
	var pageOne e2eSnapshotPage
	unmarshalData(t, pageOneEnvelope, &pageOne)
	if pageOne.Count != 1 || pageOne.Total != 2 || pageOne.Offset != 0 || pageOne.Limit != 1 || !pageOneEnvelope.Truncated {
		t.Fatalf("first snapshot page = %+v, truncated=%v", pageOne, pageOneEnvelope.Truncated)
	}

	pageTwoEnvelope := callE2ETool(t, ctx, session, artifact, "list_snapshots", map[string]any{
		"repository_id": "tmp-local",
		"limit":         1,
		"offset":        1,
	})
	var pageTwo e2eSnapshotPage
	unmarshalData(t, pageTwoEnvelope, &pageTwo)
	if pageTwo.Count != 1 || pageTwo.Total != 2 || pageTwo.Offset != 1 || pageTwoEnvelope.Truncated {
		t.Fatalf("second snapshot page = %+v, truncated=%v", pageTwo, pageTwoEnvelope.Truncated)
	}
	if pageOne.Snapshots[0].ID == pageTwo.Snapshots[0].ID {
		t.Fatal("snapshot pages returned the same snapshot")
	}
	artifact.check("snapshot pagination")

	emptySnapshots := callE2ETool(t, ctx, session, artifact, "list_snapshots", map[string]any{
		"repository_id": "tmp-local",
		"tags":          []string{"missing"},
	})
	var emptyPage e2eSnapshotPage
	unmarshalData(t, emptySnapshots, &emptyPage)
	if emptyPage.Count != 0 || emptyPage.Total != 0 || len(emptyPage.Snapshots) != 0 || emptySnapshots.Truncated {
		t.Fatalf("empty snapshot page = %+v, truncated=%v", emptyPage, emptySnapshots.Truncated)
	}
	artifact.check("authorized empty snapshot result")

	selected := pageOne.Snapshots[0]
	if len(selected.Paths) != 1 || selected.ID == "" {
		t.Fatalf("selected snapshot = %+v", selected)
	}
	selectedPath := selected.Paths[0]

	getSnapshot := callE2ETool(t, ctx, session, artifact, "get_snapshot", map[string]any{
		"repository_id": "tmp-local",
		"snapshot_id":   selected.ID,
	})
	var snapshot restic.Snapshot
	unmarshalData(t, getSnapshot, &snapshot)
	if snapshot.ID != selected.ID || !slices.Equal(snapshot.Paths, selected.Paths) {
		t.Fatalf("get_snapshot = %+v, selected = %+v", snapshot, selected)
	}
	artifact.check("snapshot metadata")

	browseEnvelope := callE2ETool(t, ctx, session, artifact, "browse_snapshot", map[string]any{
		"repository_id": "tmp-local",
		"snapshot_id":   selected.ID,
		"path":          selectedPath,
		"limit":         100,
	})
	var browse e2eBrowseData
	unmarshalData(t, browseEnvelope, &browse)
	assertBrowseResult(t, fixture, selectedPath, browse)
	artifact.check("observable browse result")

	browseLimitedEnvelope := callE2ETool(t, ctx, session, artifact, "browse_snapshot", map[string]any{
		"repository_id": "tmp-local",
		"snapshot_id":   selected.ID,
		"path":          selectedPath,
		"limit":         1,
	})
	var browseLimited e2eBrowseData
	unmarshalData(t, browseLimitedEnvelope, &browseLimited)
	if browseLimited.Count != 1 || len(browseLimited.Nodes) != 1 || !browseLimitedEnvelope.Truncated {
		t.Fatalf("limited browse = %+v, truncated=%v", browseLimited, browseLimitedEnvelope.Truncated)
	}
	artifact.check("browse limit")

	findEnvelope := callE2ETool(t, ctx, session, artifact, "find_files", map[string]any{
		"repository_id": "tmp-local",
		"pattern":       "*.txt",
		"snapshot_id":   selected.ID,
		"path":          selectedPath,
	})
	var find e2eFindData
	unmarshalData(t, findEnvelope, &find)
	if find.Count != 1 || len(find.Groups) != 1 || len(find.Groups[0].Matches) != 1 {
		t.Fatalf("find result = %+v", find)
	}
	findPath := find.Groups[0].Matches[0].Path
	if filepath.Base(findPath) != "hello.txt" || !strings.HasPrefix(findPath, selectedPath) {
		t.Fatalf("find path = %q, want hello.txt under %q", findPath, selectedPath)
	}
	artifact.check("observable find result")

	emptyFindEnvelope := callE2ETool(t, ctx, session, artifact, "find_files", map[string]any{
		"repository_id": "tmp-local",
		"pattern":       "*.does-not-exist",
		"snapshot_id":   selected.ID,
		"path":          selectedPath,
	})
	var emptyFind e2eFindData
	unmarshalData(t, emptyFindEnvelope, &emptyFind)
	if emptyFind.Count != 0 || len(emptyFind.Groups) != 0 || emptyFindEnvelope.Truncated {
		t.Fatalf("empty find result = %+v, truncated=%v", emptyFind, emptyFindEnvelope.Truncated)
	}
	artifact.check("authorized empty find result")

	statsEnvelope := callE2ETool(t, ctx, session, artifact, "repository_stats", map[string]any{
		"repository_id": "tmp-local",
		"mode":          "restore-size",
		"snapshot_id":   selected.ID,
	})
	var stats e2eStatsData
	unmarshalData(t, statsEnvelope, &stats)
	if stats.Mode != "restore-size" || stats.SelectedSnapshots != 1 ||
		stats.Stats.TotalFileCount == 0 || stats.Stats.TotalSize == 0 {
		t.Fatalf("stats result = %+v", stats)
	}
	artifact.check("observable statistics")

	denial := callE2EToolError(t, ctx, session, artifact, "browse_snapshot", map[string]any{
		"repository_id": "tmp-local",
		"snapshot_id":   selected.ID,
		"path":          "/etc",
	})
	if denial["code"] != "not_allowed" {
		t.Fatalf("policy denial = %v", denial)
	}
	artifact.check("structured policy denial")
	unknownRepositoryID := "/Users/victim/private/path"
	unknownRepository := callE2EToolError(t, ctx, session, artifact, "list_snapshots", map[string]any{
		"repository_id": unknownRepositoryID,
	})
	if unknownRepository["code"] != "repository_not_found" {
		t.Fatalf("unknown repository error = %v", unknownRepository)
	}
	artifact.check("unknown repository audit redaction")

	if err := fixture.SetRepositoryLocation(filepath.Join(fixture.Root, "missing-repository")); err != nil {
		t.Fatal(err)
	}
	sealedLocation := callE2ETool(t, ctx, session, artifact, "list_snapshots", map[string]any{
		"repository_id": "tmp-local",
	})
	var sealedPage e2eSnapshotPage
	unmarshalData(t, sealedLocation, &sealedPage)
	if sealedPage.Total != 2 {
		t.Fatalf("sealed repository returned %+v", sealedPage)
	}
	if err := fixture.SetRepositoryLocation(fixture.RepoDir); err != nil {
		t.Fatal(err)
	}
	artifact.check("sealed repository location")

	if err := fixture.SetPassword("synthetic-invalid-rotated-password"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = fixture.RestorePassword() })
	passwordError := callE2EToolError(t, ctx, session, artifact, "list_snapshots", map[string]any{
		"repository_id": "tmp-local",
	})
	if passwordError["code"] != "authentication_failed" {
		t.Fatalf("password rotation error = %v", passwordError)
	}
	if err := fixture.RestorePassword(); err != nil {
		t.Fatal(err)
	}
	restoredPassword := callE2ETool(t, ctx, session, artifact, "list_snapshots", map[string]any{
		"repository_id": "tmp-local",
	})
	var restoredPage e2eSnapshotPage
	unmarshalData(t, restoredPassword, &restoredPage)
	if restoredPage.Total != 2 {
		t.Fatalf("restored password returned %+v", restoredPage)
	}
	artifact.check("password file rotation")

	protocolText := artifact.transcriptText()
	for _, forbidden := range []string{
		fixture.RepoDir,
		fixture.RepositoryFile,
		fixture.PasswordFile,
		"test-password-not-secret",
		"synthetic-parent-password",
		"synthetic-parent-cloud-key",
		"hello world",
		"# note",
		"log line",
	} {
		if strings.Contains(protocolText, forbidden) {
			t.Fatalf("protocol transcript leaked forbidden value %q", forbidden)
		}
	}
	artifact.check("protocol secret and content exclusion")

	if err := session.Close(); err != nil {
		t.Fatalf("close executable session: %v", err)
	}
	closed = true
	if cmd.ProcessState == nil || !cmd.ProcessState.Exited() || !cmd.ProcessState.Success() {
		t.Fatalf("server process did not exit cleanly: %v", cmd.ProcessState)
	}
	artifact.check("clean executable shutdown")

	stderrText := serverStderr.String()
	if !strings.Contains(stderrText, `"msg":"starting"`) ||
		!strings.Contains(stderrText, `"action":"tool_rejected"`) ||
		!strings.Contains(stderrText, `"error_code":"not_allowed"`) ||
		!strings.Contains(stderrText, `"error_code":"repository_not_found"`) {
		t.Fatalf("stderr lacks startup or denial evidence: %s", stderrText)
	}
	for _, forbidden := range []string{
		`"jsonrpc"`,
		fixture.RepoDir,
		fixture.RepositoryFile,
		fixture.PasswordFile,
		"test-password-not-secret",
		"synthetic-invalid-rotated-password",
		"synthetic-parent-password",
		"synthetic-parent-cloud-key",
		"/etc",
		unknownRepositoryID,
	} {
		if strings.Contains(stderrText, forbidden) {
			t.Fatalf("stderr leaked forbidden value %q", forbidden)
		}
	}
	artifact.check("stderr and stdout separation")

	afterIDs, err := fixture.SnapshotIDs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	artifact.afterSnapshotIDs = append([]string(nil), afterIDs...)
	if !slices.Equal(beforeIDs, afterIDs) {
		t.Fatalf("snapshot IDs changed: before=%v after=%v", beforeIDs, afterIDs)
	}
	artifact.check("snapshot set immutability")
}

func selectE2EBinary(t *testing.T, ctx context.Context) (string, string, string) {
	t.Helper()
	provided := os.Getenv("RESTIC_MCP_BINARY")
	if provided != "" {
		absolute, err := filepath.Abs(provided)
		if err != nil {
			t.Fatalf("resolve RESTIC_MCP_BINARY: %v", err)
		}
		info, err := os.Stat(absolute)
		if err != nil || !info.Mode().IsRegular() {
			t.Fatalf("RESTIC_MCP_BINARY is not a regular file: %s", absolute)
		}
		return absolute, "", "provided"
	}
	return buildE2EBinary(t, ctx), e2eBinaryVersion, "built"
}

func buildE2EBinary(t *testing.T, ctx context.Context) string {
	t.Helper()
	binary := filepath.Join(t.TempDir(), "restic-defensive-mcp")
	cmd := exec.CommandContext(ctx, "go", "build", "-trimpath", "-ldflags", "-X main.version="+e2eBinaryVersion, "-o", binary, "./cmd/restic-defensive-mcp")
	goFlags := strings.TrimSpace(os.Getenv("GOFLAGS") + " -p=1")
	cmd.Env = replaceEnvironment(os.Environ(), map[string]string{
		"CGO_ENABLED": "0",
		"GOMAXPROCS":  "2",
		"GOFLAGS":     goFlags,
	})
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build executable: %v\n%s", err, output)
	}
	return binary
}

func assertVersionFlag(
	t *testing.T,
	ctx context.Context,
	artifact *e2eArtifact,
	binary string,
	expectedVersion string,
) string {
	t.Helper()
	cmd := exec.CommandContext(ctx, binary, "--version")
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("version flag: %v", err)
	}
	line := strings.TrimSuffix(stdout.String(), "\n")
	const prefix = "restic-defensive-mcp "
	version := strings.TrimPrefix(line, prefix)
	if line == version || version == "" || stderr.Len() != 0 {
		t.Fatalf("version stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
	if expectedVersion != "" && version != expectedVersion {
		t.Fatalf("version = %q, want %q", version, expectedVersion)
	}
	artifact.check("version flag")
	return version
}

func assertMissingConfig(t *testing.T, ctx context.Context, artifact *e2eArtifact, binary string) {
	t.Helper()
	stdout, stderr := runExpectExit(t, ctx, 2, binary)
	if stdout != "" || !strings.Contains(stderr, "--config is required") {
		t.Fatalf("missing config stdout=%q stderr=%q", stdout, stderr)
	}
	artifact.check("missing config exit code")
}

func assertInvalidConfig(t *testing.T, ctx context.Context, artifact *e2eArtifact, binary string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "invalid.yaml")
	if err := testrepo.WriteInvalidConfig(path); err != nil {
		t.Fatal(err)
	}
	stdout, stderr := runExpectExit(t, ctx, 2, binary, "--config", path)
	if stdout != "" || !strings.Contains(stderr, `"msg":"config_load_failed"`) {
		t.Fatalf("invalid config stdout=%q stderr=%q", stdout, stderr)
	}
	artifact.check("invalid config exit code")
}

func assertMissingResticBinary(t *testing.T, ctx context.Context, artifact *e2eArtifact, binary, configPath string) {
	t.Helper()
	stdout, stderr := runExpectExit(t, ctx, 2, binary, "--config", configPath)
	if stdout != "" || !strings.Contains(stderr, `"msg":"restic_binary"`) {
		t.Fatalf("missing restic stdout=%q stderr=%q", stdout, stderr)
	}
	artifact.check("missing restic binary exit code")
}

func assertBlockedEnvConfig(
	t *testing.T,
	ctx context.Context,
	artifact *e2eArtifact,
	binary string,
	configPath string,
	key string,
) {
	t.Helper()
	stdout, stderr := runExpectExit(t, ctx, 2, binary, "--config", configPath)
	if stdout != "" || !strings.Contains(stderr, `"msg":"registry"`) || strings.Contains(stderr, "credential_process") {
		t.Fatalf("blocked %s stdout=%q stderr=%q", key, stdout, stderr)
	}
	artifact.check("blocked env key " + key)
}

func assertS3Startup(
	t *testing.T,
	ctx context.Context,
	artifact *e2eArtifact,
	binary string,
	configPath string,
	name string,
	wantExit int,
) {
	t.Helper()
	stdout, stderr := runExpectExit(t, ctx, wantExit, binary, "--config", configPath)
	if stdout != "" {
		t.Fatalf("S3 %s wrote stdout: %q", name, stdout)
	}
	if wantExit == 0 && !strings.Contains(stderr, `"msg":"starting"`) {
		t.Fatalf("S3 %s did not start: %q", name, stderr)
	}
	if wantExit != 0 && !strings.Contains(stderr, `"msg":"registry"`) {
		t.Fatalf("S3 %s did not fail in registry setup: %q", name, stderr)
	}
	for _, secret := range []string{"synthetic-access-key", "synthetic-secret-key"} {
		if strings.Contains(stderr, secret) {
			t.Fatalf("S3 %s leaked a synthetic credential", name)
		}
	}
	artifact.check("S3 static credential boundary " + name)
}

func runExpectExit(t *testing.T, ctx context.Context, exitCode int, name string, args ...string) (string, string) {
	t.Helper()
	cmd := exec.CommandContext(ctx, name, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	var actual int
	if err == nil {
		actual = 0
	} else if exitErr, ok := err.(*exec.ExitError); ok {
		actual = exitErr.ExitCode()
	} else {
		t.Fatalf("run %s: %v", name, err)
	}
	if actual != exitCode {
		t.Fatalf("exit code = %d, want %d; stderr=%q", actual, exitCode, stderr.String())
	}
	return stdout.String(), stderr.String()
}

func collectToolNames(t *testing.T, ctx context.Context, session *mcp.ClientSession) []string {
	t.Helper()
	var names []string
	for tool, err := range session.Tools(ctx, nil) {
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, tool.Name)
	}
	sort.Strings(names)
	return names
}

func callE2ETool(
	t *testing.T,
	ctx context.Context,
	session *mcp.ClientSession,
	artifact *e2eArtifact,
	name string,
	arguments map[string]any,
) e2eEnvelope {
	t.Helper()
	result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: arguments})
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	text := toolText(result)
	artifact.record(name, arguments, result.IsError, text)
	if result.IsError {
		t.Fatalf("%s: %s", name, text)
	}
	var envelope e2eEnvelope
	if err := json.Unmarshal([]byte(text), &envelope); err != nil {
		t.Fatalf("%s returned invalid JSON: %v: %s", name, err, text)
	}
	if envelope.Cost == "" || len(envelope.Data) == 0 {
		t.Fatalf("%s returned an incomplete envelope: %s", name, text)
	}
	return envelope
}

func callE2EToolError(
	t *testing.T,
	ctx context.Context,
	session *mcp.ClientSession,
	artifact *e2eArtifact,
	name string,
	arguments map[string]any,
) map[string]string {
	t.Helper()
	result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: arguments})
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	text := toolText(result)
	artifact.record(name, arguments, result.IsError, text)
	if !result.IsError {
		t.Fatalf("%s returned success, want error: %s", name, text)
	}
	var payload map[string]string
	if err := json.Unmarshal([]byte(text), &payload); err != nil {
		t.Fatalf("%s returned invalid error JSON: %v: %s", name, err, text)
	}
	return payload
}

func unmarshalData(t *testing.T, envelope e2eEnvelope, target any) {
	t.Helper()
	if err := json.Unmarshal(envelope.Data, target); err != nil {
		t.Fatalf("decode response data: %v: %s", err, envelope.Data)
	}
}

func assertBrowseResult(t *testing.T, fixture *testrepo.Fixture, selectedPath string, browse e2eBrowseData) {
	t.Helper()
	if browse.Path != selectedPath || browse.Count != len(browse.Nodes) || browse.Count < 2 {
		t.Fatalf("browse result = %+v", browse)
	}
	names := make([]string, 0, len(browse.Nodes))
	for _, node := range browse.Nodes {
		if !strings.HasPrefix(node.Path, selectedPath) {
			t.Fatalf("browse path escaped scope: %+v", node)
		}
		names = append(names, node.Name)
	}
	if !slices.Contains(names, "hello.txt") {
		t.Fatalf("browse names omit hello.txt: %v", names)
	}
	if selectedPath == fixture.DataDir1 && !slices.Contains(names, "subdir") {
		t.Fatalf("data1 browse names omit subdir: %v", names)
	}
	if selectedPath == fixture.DataDir2 && !slices.Contains(names, "extra.log") {
		t.Fatalf("data2 browse names omit extra.log: %v", names)
	}
}

func sameStrings(actual, expected []string) bool {
	a := append([]string(nil), actual...)
	b := append([]string(nil), expected...)
	sort.Strings(a)
	sort.Strings(b)
	return slices.Equal(a, b)
}

func replaceEnvironment(base []string, replacements map[string]string) []string {
	out := make([]string, 0, len(base)+len(replacements))
	for _, entry := range base {
		key, _, ok := strings.Cut(entry, "=")
		if _, replace := replacements[key]; ok && replace {
			continue
		}
		out = append(out, entry)
	}
	keys := make([]string, 0, len(replacements))
	for key := range replacements {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		out = append(out, key+"="+replacements[key])
	}
	return out
}

func commandText(t *testing.T, name string, args ...string) string {
	t.Helper()
	output, err := exec.Command(name, args...).CombinedOutput()
	if err != nil {
		t.Fatalf("run %s: %v: %s", name, err, output)
	}
	return strings.TrimSpace(string(output))
}

func mustJSON(t *testing.T, value any) string {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func fileSHA256(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s for SHA-256: %v", path, err)
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

type e2eTranscriptEvent struct {
	Method    string         `json:"method"`
	Arguments map[string]any `json:"arguments,omitempty"`
	IsError   bool           `json:"is_error"`
	Response  string         `json:"response"`
}

type e2eFileIdentity struct {
	Path   string `json:"path"`
	Type   string `json:"type"`
	SHA256 string `json:"sha256"`
}

type e2eArtifactMetadata struct {
	SchemaVersion     int                  `json:"schema_version"`
	Command           string               `json:"command"`
	SourceRevision    string               `json:"source_revision"`
	TrackedDiffSHA256 string               `json:"tracked_working_diff_sha256"`
	UntrackedFiles    []e2eFileIdentity    `json:"untracked_files"`
	WorktreeStatus    string               `json:"worktree_status"`
	Environment       map[string]string    `json:"environment"`
	Fixture           testrepo.Description `json:"fixture"`
	BeforeSnapshotIDs []string             `json:"before_snapshot_ids,omitempty"`
	AfterSnapshotIDs  []string             `json:"after_snapshot_ids,omitempty"`
	Checks            []string             `json:"checks"`
	Result            string               `json:"result"`
	FinishedAt        string               `json:"finished_at"`
}

type e2eArtifact struct {
	dir               string
	command           string
	revision          string
	trackedDiffSHA256 string
	untrackedFiles    []e2eFileIdentity
	worktreeStatus    string
	environment       map[string]string
	fixture           testrepo.Description
	beforeSnapshotIDs []string
	afterSnapshotIDs  []string
	checks            []string
	transcript        []e2eTranscriptEvent
}

func newE2EArtifact(t *testing.T) *e2eArtifact {
	t.Helper()
	repositoryRoot := commandText(t, "git", "rev-parse", "--show-toplevel")
	base := os.Getenv("RESTIC_MCP_ARTIFACT_DIR")
	var dir string
	var err error
	if base == "" {
		dir, err = os.MkdirTemp("", "restic-defensive-mcp-e2e-artifact-")
	} else {
		base, err = resolvePathForContainment(base)
		if err == nil {
			resolvedRoot, resolveErr := resolvePathForContainment(repositoryRoot)
			if resolveErr != nil {
				err = resolveErr
			} else if pathWithin(resolvedRoot, base) {
				err = fmt.Errorf("RESTIC_MCP_ARTIFACT_DIR must be outside the repository")
			} else if mkdirErr := os.MkdirAll(base, 0o700); mkdirErr != nil {
				err = mkdirErr
			} else {
				dir, err = os.MkdirTemp(base, time.Now().UTC().Format("20060102T150405Z-")+"*")
			}
		}
	}
	if err != nil {
		t.Fatalf("create E2E artifact directory: %v", err)
	}
	t.Logf("E2E artifact directory: %s", dir)

	command := os.Getenv("RESTIC_MCP_E2E_COMMAND")
	if command == "" {
		command = strings.Join(os.Args, " ")
	}
	revision := commandText(t, "git", "rev-parse", "HEAD")
	trackedDiff, err := exec.Command("git", "diff", "--binary", "HEAD", "--").Output()
	if err != nil {
		t.Fatalf("read tracked working diff: %v", err)
	}
	diffSum := sha256.Sum256(trackedDiff)
	status := commandText(t, "git", "status", "--porcelain=v1", "--untracked-files=all")
	untracked := untrackedIdentities(t)

	return &e2eArtifact{
		dir:               dir,
		command:           command,
		revision:          revision,
		trackedDiffSHA256: hex.EncodeToString(diffSum[:]),
		untrackedFiles:    untracked,
		worktreeStatus:    status,
		environment: map[string]string{
			"go_arch":    runtime.GOARCH,
			"go_os":      runtime.GOOS,
			"go_runtime": runtime.Version(),
			"gomaxprocs": strconv.Itoa(runtime.GOMAXPROCS(0)),
			"goflags":    os.Getenv("GOFLAGS"),
		},
	}
}

func (a *e2eArtifact) check(name string) {
	a.checks = append(a.checks, name)
}

func (a *e2eArtifact) record(method string, arguments map[string]any, isError bool, response string) {
	a.transcript = append(a.transcript, e2eTranscriptEvent{
		Method:    method,
		Arguments: arguments,
		IsError:   isError,
		Response:  response,
	})
}

func (a *e2eArtifact) transcriptText() string {
	var builder strings.Builder
	for _, event := range a.transcript {
		builder.WriteString(event.Response)
		builder.WriteByte('\n')
	}
	return builder.String()
}

func (a *e2eArtifact) finish(t *testing.T, stderr string) {
	t.Helper()
	result := "passed"
	if t.Failed() {
		result = "failed"
	}
	metadata := e2eArtifactMetadata{
		SchemaVersion:     1,
		Command:           a.command,
		SourceRevision:    a.revision,
		TrackedDiffSHA256: a.trackedDiffSHA256,
		UntrackedFiles:    a.untrackedFiles,
		WorktreeStatus:    a.worktreeStatus,
		Environment:       a.environment,
		Fixture:           a.fixture,
		BeforeSnapshotIDs: a.beforeSnapshotIDs,
		AfterSnapshotIDs:  a.afterSnapshotIDs,
		Checks:            a.checks,
		Result:            result,
		FinishedAt:        time.Now().UTC().Format(time.RFC3339Nano),
	}
	metadataJSON, err := json.MarshalIndent(metadata, "", "  ")
	if err != nil {
		t.Errorf("encode E2E metadata: %v", err)
		return
	}
	metadataJSON = append(metadataJSON, '\n')

	var transcript bytes.Buffer
	encoder := json.NewEncoder(&transcript)
	for _, event := range a.transcript {
		if err := encoder.Encode(event); err != nil {
			t.Errorf("encode E2E transcript: %v", err)
			return
		}
	}
	files := []struct {
		name string
		data []byte
	}{
		{name: "metadata.json", data: metadataJSON},
		{name: "protocol.jsonl", data: transcript.Bytes()},
		{name: "stderr.log", data: []byte(stderr)},
	}
	var sums strings.Builder
	for _, file := range files {
		if err := os.WriteFile(filepath.Join(a.dir, file.name), file.data, 0o600); err != nil {
			t.Errorf("write E2E artifact %s: %v", file.name, err)
			return
		}
		sum := sha256.Sum256(file.data)
		fmt.Fprintf(&sums, "%x  %s\n", sum, file.name)
	}
	if err := os.WriteFile(filepath.Join(a.dir, "SHA256SUMS"), []byte(sums.String()), 0o600); err != nil {
		t.Errorf("write E2E checksums: %v", err)
	}
}

func untrackedIdentities(t *testing.T) []e2eFileIdentity {
	t.Helper()
	output, err := exec.Command("git", "ls-files", "--others", "--exclude-standard", "-z").Output()
	if err != nil {
		t.Fatalf("list untracked files: %v", err)
	}
	var identities []e2eFileIdentity
	for _, rawPath := range bytes.Split(output, []byte{0}) {
		if len(rawPath) == 0 {
			continue
		}
		path := string(rawPath)
		info, err := os.Lstat(path)
		if err != nil {
			t.Fatalf("inspect untracked file %s: %v", path, err)
		}
		fileType := "regular"
		var content []byte
		if info.Mode()&os.ModeSymlink != 0 {
			fileType = "symlink"
			target, readErr := os.Readlink(path)
			if readErr != nil {
				t.Fatalf("read untracked symlink %s: %v", path, readErr)
			}
			content = []byte(target)
		} else if info.Mode().IsRegular() {
			content, err = os.ReadFile(path)
			if err != nil {
				t.Fatalf("read untracked file %s: %v", path, err)
			}
		} else {
			fileType = info.Mode().Type().String()
		}
		sum := sha256.Sum256(content)
		identities = append(identities, e2eFileIdentity{
			Path: path, Type: fileType, SHA256: hex.EncodeToString(sum[:]),
		})
	}
	sort.Slice(identities, func(i, j int) bool { return identities[i].Path < identities[j].Path })
	return identities
}

func pathWithin(parent, child string) bool {
	relative, err := filepath.Rel(parent, child)
	if err != nil {
		return false
	}
	return relative == "." || (relative != ".." && !strings.HasPrefix(relative, ".."+string(os.PathSeparator)))
}

func resolvePathForContainment(path string) (string, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	current := absolute
	var suffix []string
	for {
		resolved, evalErr := filepath.EvalSymlinks(current)
		if evalErr == nil {
			for i := len(suffix) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, suffix[i])
			}
			return resolved, nil
		}
		if !os.IsNotExist(evalErr) {
			return "", evalErr
		}
		parent := filepath.Dir(current)
		if parent == current {
			return "", evalErr
		}
		suffix = append(suffix, filepath.Base(current))
		current = parent
	}
}
