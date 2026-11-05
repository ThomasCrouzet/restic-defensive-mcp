//go:build integration

package main_test

// Failure modes covered by this executable E2E test:
//
//   - A native POSIX-path fixture cannot traverse all seven tools.
//   - A configuration or executable path containing spaces is mishandled.
//   - Policy denial, find, browse, or statistics lose filtering on this fixture.
//
// The fixture uses only restic --stdin snapshots with POSIX filenames. It runs
// on every native platform and is the Windows workflow's positive scenario.

import (
	"context"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/ThomasCrouzet/restic-defensive-mcp/internal/testrepo"
)

func TestExecutableStdinFixture(t *testing.T) {
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

	fixture, err := testrepo.CreateStdinFixture(ctx, resticBinary)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := fixture.Close(); err != nil {
			t.Errorf("close fixture: %v", err)
		}
	})

	for _, file := range []struct {
		name     string
		contents string
	}{
		{name: "hello.txt", contents: "hello world\n"},
		{name: "with space.txt", contents: "space\n"},
		{name: "café.txt", contents: "unicode\n"},
	} {
		if err := fixture.AddStdinSnapshot(ctx, fixture.Host, []string{"test"}, file.name, []byte(file.contents)); err != nil {
			t.Fatal(err)
		}
	}

	configPath := filepath.Join(fixture.Root, "config with space.yaml")
	if err := fixture.WriteConfigWithPolicy(configPath, resticBinary, testrepo.ConfigPolicy{
		AllowedHosts: []string{fixture.Host},
		AllowedTags:  []string{"test"},
		AllowedPaths: []string{"/"},
	}); err != nil {
		t.Fatal(err)
	}
	session := connectFixtureServer(t, ctx, binary, configPath)

	tools := collectToolNames(t, ctx, session)
	if len(tools) != 7 {
		t.Fatalf("tools = %v, want seven", tools)
	}
	page := callSnapshotPage(t, ctx, session, artifact, map[string]any{
		"repository_id": "tmp-local",
		"limit":         20,
	})
	if page.Total != 3 || len(page.Snapshots) != 3 {
		t.Fatalf("snapshots = %+v", page)
	}
	snapshot := page.Snapshots[0]
	if len(snapshot.Paths) != 1 || snapshot.Paths[0][0] != '/' {
		t.Fatalf("stdin fixture did not use POSIX paths: %+v", snapshot.Paths)
	}
	got := getCursorSnapshot(t, ctx, session, artifact, snapshot.ID, nil)
	if got.ID != snapshot.ID {
		t.Fatalf("get_snapshot = %+v", got)
	}

	browse := callBrowseCursor(t, ctx, session, artifact, map[string]any{
		"repository_id": "tmp-local",
		"snapshot_id":   snapshot.ID,
		"path":          "/",
		"limit":         10,
	})
	if browse.Count != 1 || len(browse.Nodes) != 1 || browse.Nodes[0].Path != snapshot.Paths[0] {
		t.Fatalf("browse result = %+v", browse)
	}
	find := callFindCursor(t, ctx, session, artifact, map[string]any{
		"repository_id": "tmp-local",
		"pattern":       "*.txt",
		"path":          "/",
		"limit":         20,
	})
	if find.Count != 3 || len(find.Groups) != 3 {
		t.Fatalf("find result = %+v", find)
	}
	for _, group := range find.Groups {
		for _, match := range group.Matches {
			if match.Name == "" || match.Path[0] != '/' {
				t.Fatalf("find match lost POSIX name/path: %+v", match)
			}
		}
	}
	stats := callStats(t, ctx, session, artifact, map[string]any{
		"repository_id": "tmp-local",
		"mode":          "restore-size",
		"snapshot_id":   snapshot.ID,
	})
	if stats.SelectedSnapshots != 1 || stats.Stats.TotalFileCount == 0 {
		t.Fatalf("stats = %+v", stats)
	}
	denial := callE2EToolError(t, ctx, session, artifact, "browse_snapshot", map[string]any{
		"repository_id": "tmp-local",
		"snapshot_id":   snapshot.ID,
		"path":          `C:\Windows\System32`,
	})
	if denial["code"] != "invalid_argument" {
		t.Fatalf("Windows path rejection = %v", denial)
	}
	artifact.check("stdin POSIX fixture across all seven tools, spaces, and policy denial")
}
