//go:build integration

package main_test

// Failure modes covered by this executable E2E test:
//
//   - An allowlisted parent root fails to select snapshots rooted below it.
//   - A child search scope hides a visible concrete snapshot.
//   - Parent/child, equal, disjoint, nested, and root scopes disagree.
//   - Mixed allowlists broaden or narrow snapshot selection incorrectly.
//
// The test runs the real MCP executable over stdio against a real restic
// repository prepared only by internal/testrepo.

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/ThomasCrouzet/restic-defensive-mcp/internal/restic"
	"github.com/ThomasCrouzet/restic-defensive-mcp/internal/testrepo"
)

func TestExecutableFindSnapshotRootSelection(t *testing.T) {
	artifact := newE2EArtifact(t)
	t.Cleanup(func() { artifact.finish(t, "") })

	resticBinary, err := exec.LookPath("restic")
	if err != nil {
		t.Fatalf("integration E2E requires restic: %v", err)
	}
	artifact.environment["restic_binary"] = resticBinary
	artifact.environment["restic_version"] = commandText(t, resticBinary, "version")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	t.Cleanup(cancel)

	binary, expectedServerVersion, binarySource := selectE2EBinary(t, ctx)
	serverVersion := assertVersionFlag(t, ctx, artifact, binary, expectedServerVersion)
	artifact.environment["server_binary"] = binary
	artifact.environment["server_binary_source"] = binarySource
	artifact.environment["server_version"] = serverVersion

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

	// Discover which concrete snapshot covers each data root.
	snapshots := listSnapshotsWithPaths(t, ctx, artifact, binary, fixture, resticBinary, []string{fixture.Root}, "")
	if len(snapshots) != 2 {
		t.Fatalf("fixture snapshots = %d, want 2", len(snapshots))
	}
	byRoot := make(map[string]restic.Snapshot, len(snapshots))
	for _, snapshot := range snapshots {
		if len(snapshot.Paths) != 1 {
			t.Fatalf("unexpected snapshot paths: %+v", snapshot)
		}
		byRoot[snapshot.Paths[0]] = snapshot
	}
	for _, root := range []string{fixture.DataDir1, fixture.DataDir2} {
		if _, ok := byRoot[root]; !ok {
			t.Fatalf("no snapshot rooted at %s: %+v", root, byRoot)
		}
	}

	countMatches := func(find e2eFindData) int {
		total := 0
		for _, group := range find.Groups {
			total += len(group.Matches)
		}
		return total
	}
	checkRoots := func(t *testing.T, name string, find e2eFindData, wantCount int, wantPrefixes ...string) {
		t.Helper()
		if find.Count != wantCount || countMatches(find) != wantCount {
			t.Fatalf("%s: find count=%d matches=%d, want %d: %+v", name, find.Count, countMatches(find), wantCount, find)
		}
		for _, group := range find.Groups {
			for _, match := range group.Matches {
				if !slices.ContainsFunc(wantPrefixes, func(prefix string) bool {
					return match.Path == prefix || strings.HasPrefix(match.Path, prefix+"/")
				}) {
					t.Fatalf("%s: match %q escaped expected roots %v", name, match.Path, wantPrefixes)
				}
			}
		}
	}

	t.Run("parent allowlist selects child snapshot roots", func(t *testing.T) {
		configPath := filepath.Join(fixture.Root, "roots-parent.yaml")
		if err := fixture.WriteConfigForPaths(configPath, resticBinary, []string{fixture.Root}); err != nil {
			t.Fatal(err)
		}
		session := connectFixtureServer(t, ctx, binary, configPath)

		page := callSnapshots(t, ctx, session, artifact, map[string]any{
			"repository_id": "tmp-local",
		})
		if page.Total != 2 {
			t.Fatalf("list_snapshots = %+v, want both child snapshots", page)
		}

		find := callFind(t, ctx, session, artifact, map[string]any{
			"repository_id": "tmp-local",
			"pattern":       "hello.txt",
		})
		checkRoots(t, "parent implicit scope", find, 2, fixture.DataDir1, fixture.DataDir2)
		artifact.check("parent allowlist with implicit child roots")
	})

	t.Run("parent allowlist scoped to a child root", func(t *testing.T) {
		configPath := filepath.Join(fixture.Root, "roots-parent-child.yaml")
		if err := fixture.WriteConfigForPaths(configPath, resticBinary, []string{fixture.Root}); err != nil {
			t.Fatal(err)
		}
		session := connectFixtureServer(t, ctx, binary, configPath)

		page := callSnapshots(t, ctx, session, artifact, map[string]any{
			"repository_id": "tmp-local",
			"path":          fixture.DataDir1,
		})
		if page.Total != 1 || len(page.Snapshots) != 1 || len(page.Snapshots[0].Paths) != 1 ||
			page.Snapshots[0].Paths[0] != fixture.DataDir1 {
			t.Fatalf("list_snapshots path filter = %+v", page)
		}

		find := callFind(t, ctx, session, artifact, map[string]any{
			"repository_id": "tmp-local",
			"pattern":       "hello.txt",
			"path":          fixture.DataDir1,
		})
		checkRoots(t, "parent allowlist child scope", find, 1, fixture.DataDir1)
		artifact.check("parent allowlist scoped to child root")
	})

	t.Run("child allowlist hides disjoint snapshot root", func(t *testing.T) {
		configPath := filepath.Join(fixture.Root, "roots-child.yaml")
		if err := fixture.WriteConfigForPaths(configPath, resticBinary, []string{fixture.DataDir1}); err != nil {
			t.Fatal(err)
		}
		session := connectFixtureServer(t, ctx, binary, configPath)

		page := callSnapshots(t, ctx, session, artifact, map[string]any{
			"repository_id": "tmp-local",
		})
		if page.Total != 1 || page.Snapshots[0].Paths[0] != fixture.DataDir1 {
			t.Fatalf("list_snapshots = %+v, want only the child snapshot", page)
		}

		find := callFind(t, ctx, session, artifact, map[string]any{
			"repository_id": "tmp-local",
			"pattern":       "hello.txt",
		})
		checkRoots(t, "child implicit scope", find, 1, fixture.DataDir1)
		if slices.ContainsFunc(find.Groups, func(group restic.FindGroup) bool {
			return snapshotPrefixMatches(byRoot[fixture.DataDir2].ID, group.Snapshot)
		}) {
			t.Fatalf("disjoint snapshot appeared in find result: %+v", find)
		}
		artifact.check("child allowlist hides disjoint snapshot root")
	})

	t.Run("equal root and concrete snapshot id", func(t *testing.T) {
		configPath := filepath.Join(fixture.Root, "roots-equal.yaml")
		if err := fixture.WriteConfigForPaths(configPath, resticBinary, []string{fixture.DataDir2}); err != nil {
			t.Fatal(err)
		}
		session := connectFixtureServer(t, ctx, binary, configPath)

		find := callFind(t, ctx, session, artifact, map[string]any{
			"repository_id": "tmp-local",
			"pattern":       "hello.txt",
			"snapshot_id":   byRoot[fixture.DataDir2].ID,
		})
		checkRoots(t, "equal root concrete id", find, 1, fixture.DataDir2)
		artifact.check("equal root with visible concrete snapshot id")
	})

	t.Run("nested overlapping allowlists", func(t *testing.T) {
		configPath := filepath.Join(fixture.Root, "roots-nested.yaml")
		if err := fixture.WriteConfigForPaths(configPath, resticBinary, []string{fixture.Root, fixture.DataDir1}); err != nil {
			t.Fatal(err)
		}
		session := connectFixtureServer(t, ctx, binary, configPath)

		page := callSnapshots(t, ctx, session, artifact, map[string]any{
			"repository_id": "tmp-local",
		})
		if page.Total != 2 {
			t.Fatalf("nested allowlist snapshots = %+v, want both", page)
		}
		find := callFind(t, ctx, session, artifact, map[string]any{
			"repository_id": "tmp-local",
			"pattern":       "hello.txt",
			"path":          fixture.DataDir1,
		})
		checkRoots(t, "nested allowlist", find, 1, fixture.DataDir1)
		artifact.check("nested overlapping allowlists")
	})

	t.Run("root allowlist", func(t *testing.T) {
		configPath := filepath.Join(fixture.Root, "roots-slash.yaml")
		if err := fixture.WriteConfigForPaths(configPath, resticBinary, []string{"/"}); err != nil {
			t.Fatal(err)
		}
		session := connectFixtureServer(t, ctx, binary, configPath)

		page := callSnapshots(t, ctx, session, artifact, map[string]any{
			"repository_id": "tmp-local",
		})
		if page.Total != 2 {
			t.Fatalf("root allowlist snapshots = %+v, want both", page)
		}
		find := callFind(t, ctx, session, artifact, map[string]any{
			"repository_id": "tmp-local",
			"pattern":       "hello.txt",
		})
		checkRoots(t, "root allowlist", find, 2, fixture.DataDir1, fixture.DataDir2)
		artifact.check("root allowlist semantics")
	})
}

func listSnapshotsWithPaths(
	t *testing.T,
	ctx context.Context,
	artifact *e2eArtifact,
	binary string,
	fixture *testrepo.Fixture,
	resticBinary string,
	allowedPaths []string,
	requestPath string,
) []restic.Snapshot {
	t.Helper()
	configPath := filepath.Join(t.TempDir(), "snapshot-roots.yaml")
	if err := fixture.WriteConfigForPaths(configPath, resticBinary, allowedPaths); err != nil {
		t.Fatal(err)
	}
	session := connectFixtureServer(t, ctx, binary, configPath)
	args := map[string]any{"repository_id": "tmp-local"}
	if requestPath != "" {
		args["path"] = requestPath
	}
	return callSnapshots(t, ctx, session, artifact, args).Snapshots
}

func snapshotPrefixMatches(full, candidate string) bool {
	if candidate == "" || len(candidate) > len(full) {
		return false
	}
	return strings.EqualFold(full[:len(candidate)], candidate)
}

func connectFixtureServer(t *testing.T, ctx context.Context, binary, configPath string) *mcp.ClientSession {
	t.Helper()
	cmd := exec.CommandContext(ctx, binary, "--config", configPath)
	cmd.Env = replaceEnvironment(os.Environ(), nil)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	transport := &mcp.CommandTransport{Command: cmd, TerminateDuration: 3 * time.Second}
	client := mcp.NewClient(&mcp.Implementation{Name: "find-roots-e2e", Version: "1.0"}, nil)
	session, err := client.Connect(ctx, transport, nil)
	if err != nil {
		t.Fatalf("connect to executable: %v\nstderr: %s", err, stderr.String())
	}
	t.Cleanup(func() {
		if err := session.Close(); err != nil {
			t.Errorf("close session: %v", err)
		}
		if cmd.ProcessState == nil || !cmd.ProcessState.Exited() {
			t.Errorf("server process did not exit: %v\nstderr: %s", cmd.ProcessState, stderr.String())
		}
	})
	return session
}

func callSnapshots(
	t *testing.T,
	ctx context.Context,
	session *mcp.ClientSession,
	artifact *e2eArtifact,
	args map[string]any,
) e2eSnapshotPage {
	t.Helper()
	envelope := callE2ETool(t, ctx, session, artifact, "list_snapshots", args)
	var page e2eSnapshotPage
	unmarshalData(t, envelope, &page)
	return page
}

func callFind(
	t *testing.T,
	ctx context.Context,
	session *mcp.ClientSession,
	artifact *e2eArtifact,
	args map[string]any,
) e2eFindData {
	t.Helper()
	envelope := callE2ETool(t, ctx, session, artifact, "find_files", args)
	var find e2eFindData
	unmarshalData(t, envelope, &find)
	return find
}
