//go:build integration

package main_test

// Failure modes covered by this executable E2E test:
//
//   - A mixed-root snapshot leaks a hidden root or whole-snapshot summary.
//   - A snapshot root broader than the allowlist exposes hidden siblings.
//   - An allowlisted parent hides a legitimate snapshot of a child directory.
//   - Hidden hosts, hidden tags, or disjoint roots appear in any tool.
//   - Statistics aggregate a partially visible snapshot or its summary.
//   - Scoped `latest` resolves outside the visible snapshot set.
//
// The test drives the real executable over stdio against a real restic
// repository prepared only by internal/testrepo.

import (
	"context"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/ThomasCrouzet/restic-defensive-mcp/internal/restic"
	"github.com/ThomasCrouzet/restic-defensive-mcp/internal/testrepo"
)

type visibilityStats struct {
	Mode              string             `json:"mode"`
	Stats             restic.StatsResult `json:"stats"`
	SelectedSnapshots int                `json:"selected_snapshots"`
	ExcludedSnapshots int                `json:"excluded_snapshots"`
}

func TestExecutableMixedVisibilityPolicies(t *testing.T) {
	artifact := newE2EArtifact(t)
	t.Cleanup(func() { artifact.finish(t, "") })

	resticBinary, err := exec.LookPath("restic")
	if err != nil {
		t.Fatalf("integration E2E requires restic: %v", err)
	}
	artifact.environment["restic_binary"] = resticBinary
	artifact.environment["restic_version"] = commandText(t, resticBinary, "version")

	ctx, cancel := context.WithTimeout(context.Background(), 7*time.Minute)
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

	hiddenDir := filepath.Join(fixture.Root, "hidden-root")
	partialDir := filepath.Join(fixture.Root, "partial-root")
	partialVisible := filepath.Join(partialDir, "visible")
	parentAllowed := filepath.Join(fixture.Root, "allowed-parent")
	childDir := filepath.Join(parentAllowed, "child")
	broadDir := filepath.Join(fixture.Root, "disjoint-broad")
	for _, file := range []struct {
		dir  string
		name string
		data string
	}{
		{hiddenDir, "secret.txt", "hidden root content\n"},
		{partialDir, "visible/ok.txt", "visible content\n"},
		{partialDir, "hidden/secret.txt", "hidden sibling content\n"},
		{childDir, "child.txt", "child content\n"},
		{broadDir, "secret.txt", "disjoint content\n"},
	} {
		if err := fixture.WriteFile(file.dir, file.name, []byte(file.data)); err != nil {
			t.Fatal(err)
		}
	}

	if err := fixture.AddSnapshot(ctx, fixture.Host, []string{"test"}, fixture.DataDir1, hiddenDir); err != nil {
		t.Fatal(err)
	}
	if err := fixture.AddSnapshot(ctx, fixture.Host, []string{"test"}, partialDir); err != nil {
		t.Fatal(err)
	}
	if err := fixture.AddSnapshot(ctx, fixture.Host, []string{"test"}, childDir); err != nil {
		t.Fatal(err)
	}
	if err := fixture.AddSnapshot(ctx, "hidden-host", []string{"test"}, fixture.DataDir2); err != nil {
		t.Fatal(err)
	}
	if err := fixture.AddSnapshot(ctx, fixture.Host, []string{"hidden-tag"}, fixture.DataDir1); err != nil {
		t.Fatal(err)
	}
	if err := fixture.AddSnapshot(ctx, fixture.Host, []string{"test"}, broadDir); err != nil {
		t.Fatal(err)
	}

	wideConfig := filepath.Join(fixture.Root, "visibility-wide.yaml")
	if err := fixture.WriteConfigWithPolicy(wideConfig, resticBinary, testrepo.ConfigPolicy{
		AllowedPaths: []string{"/"},
	}); err != nil {
		t.Fatal(err)
	}
	wide := connectFixtureServer(t, ctx, binary, wideConfig)
	widePage := callSnapshots(t, ctx, wide, artifact, map[string]any{"repository_id": "tmp-local"})
	if widePage.Total != 8 {
		t.Fatalf("wide fixture snapshots = %d, want 8", widePage.Total)
	}
	ids := snapshotIDsByShape(t, widePage.Snapshots, fixture, hiddenDir, partialDir, childDir, broadDir)
	artifact.check("wide fixture identity discovery")

	restrictedConfig := filepath.Join(fixture.Root, "visibility-restricted.yaml")
	if err := fixture.WriteConfigWithPolicy(restrictedConfig, resticBinary, testrepo.ConfigPolicy{
		AllowedHosts: []string{fixture.Host},
		AllowedTags:  []string{"daily", "weekly", "test"},
		AllowedPaths: []string{fixture.DataDir1, fixture.DataDir2, parentAllowed, partialVisible},
	}); err != nil {
		t.Fatal(err)
	}
	session := connectFixtureServer(t, ctx, binary, restrictedConfig)

	page := callSnapshots(t, ctx, session, artifact, map[string]any{"repository_id": "tmp-local"})
	if page.Total != 5 {
		t.Fatalf("restricted visible snapshots = %d, want 5: %+v", page.Total, page.Snapshots)
	}
	visibleIDs := make(map[string]bool, len(page.Snapshots))
	for _, snapshot := range page.Snapshots {
		visibleIDs[snapshot.ID] = true
	}
	for _, want := range []string{ids.data1, ids.data2, ids.mixed, ids.partial, ids.child} {
		if !visibleIDs[want] {
			t.Fatalf("visible snapshots omit %s: %+v", want, visibleIDs)
		}
	}
	for _, hidden := range []string{ids.hiddenHost, ids.hiddenTag, ids.disjoint} {
		if visibleIDs[hidden] {
			t.Fatalf("hidden snapshot %s appeared in listing", hidden)
		}
	}
	artifact.check("mixed, partial, and child snapshots visible; hidden host/tag/disjoint roots excluded")

	mixed := callSnapshot(t, ctx, session, artifact, ids.mixed)
	if !slices.Equal(mixed.Paths, []string{fixture.DataDir1}) || mixed.Summary != nil {
		t.Fatalf("mixed snapshot projection leaked hidden root or summary: %+v", mixed)
	}
	partial := callSnapshot(t, ctx, session, artifact, ids.partial)
	if !slices.Equal(partial.Paths, []string{partialVisible}) || partial.Summary != nil {
		t.Fatalf("partial snapshot projection = %+v, want only %s and no summary", partial.Paths, partialVisible)
	}
	eligible := callSnapshot(t, ctx, session, artifact, ids.data1)
	if eligible.Summary == nil {
		t.Fatalf("fully visible snapshot lost its whole-snapshot summary: %+v", eligible)
	}
	artifact.check("authorized metadata retained and hidden roots/summaries suppressed")

	for name, hiddenID := range map[string]string{
		"hidden host":   ids.hiddenHost,
		"hidden tag":    ids.hiddenTag,
		"disjoint root": ids.disjoint,
	} {
		payload := callE2EToolError(t, ctx, session, artifact, "get_snapshot", map[string]any{
			"repository_id": "tmp-local",
			"snapshot_id":   hiddenID,
		})
		if payload["code"] != "snapshot_not_found" {
			t.Fatalf("%s snapshot error = %v, want snapshot_not_found", name, payload)
		}
	}
	artifact.check("invisible concrete snapshot ids fail closed")

	for _, arguments := range []map[string]any{
		{"repository_id": "tmp-local", "host": "hidden-host"},
		{"repository_id": "tmp-local", "tags": []string{"hidden-tag"}},
	} {
		payload := callE2EToolError(t, ctx, session, artifact, "list_snapshots", arguments)
		if payload["code"] != "not_allowed" {
			t.Fatalf("hidden selector error = %v, want not_allowed", payload)
		}
	}
	artifact.check("hidden host and tag filters are denied")

	deniedBrowse := callE2EToolError(t, ctx, session, artifact, "browse_snapshot", map[string]any{
		"repository_id": "tmp-local",
		"snapshot_id":   ids.mixed,
		"path":          hiddenDir,
	})
	if deniedBrowse["code"] != "not_allowed" {
		t.Fatalf("hidden browse error = %v", deniedBrowse)
	}
	browse := callBrowse(t, ctx, session, artifact, map[string]any{
		"repository_id": "tmp-local",
		"snapshot_id":   ids.mixed,
		"path":          fixture.DataDir1,
	})
	if browse.Count == 0 || slices.ContainsFunc(browse.Nodes, func(node restic.Node) bool {
		return node.Name == "secret.txt" || node.Path == hiddenDir
	}) {
		t.Fatalf("visible browse leaked a hidden node: %+v", browse)
	}
	artifact.check("browse hides denied roots and returns authorized nodes")

	hiddenFind := callFind(t, ctx, session, artifact, map[string]any{
		"repository_id": "tmp-local",
		"pattern":       "secret.txt",
		"snapshot_id":   ids.mixed,
		"path":          fixture.DataDir1,
	})
	if hiddenFind.Count != 0 || len(hiddenFind.Groups) != 0 {
		t.Fatalf("find leaked a hidden root match: %+v", hiddenFind)
	}
	partialFind := callFind(t, ctx, session, artifact, map[string]any{
		"repository_id": "tmp-local",
		"pattern":       "*.txt",
		"path":          partialVisible,
	})
	if partialFind.Count != 1 || len(partialFind.Groups) != 1 ||
		partialFind.Groups[0].Matches[0].Path != filepath.Join(partialVisible, "ok.txt") {
		t.Fatalf("partial-root find = %+v, want only the visible file", partialFind)
	}
	artifact.check("find applies path policy before visible results")

	stats := callStats(t, ctx, session, artifact, map[string]any{
		"repository_id": "tmp-local",
		"mode":          "restore-size",
		"host":          fixture.Host,
	})
	if stats.SelectedSnapshots != 3 || stats.ExcludedSnapshots != 2 || stats.Stats.TotalFileCount == 0 {
		t.Fatalf("mixed statistics = %+v, want 3 eligible and 2 excluded", stats)
	}
	for name, snapshotID := range map[string]string{"mixed": ids.mixed, "partial": ids.partial} {
		payload := callE2EToolError(t, ctx, session, artifact, "repository_stats", map[string]any{
			"repository_id": "tmp-local",
			"mode":          "restore-size",
			"snapshot_id":   snapshotID,
		})
		if payload["code"] != "not_allowed" {
			t.Fatalf("%s statistics error = %v, want not_allowed", name, payload)
		}
	}
	artifact.check("statistics exclude or reject partially visible snapshots")

	latestPartial := callSnapshot(t, ctx, session, artifact, "latest", map[string]any{
		"host": fixture.Host,
		"tags": []string{"test"},
		"path": partialVisible,
	})
	if latestPartial.ID != ids.partial {
		t.Fatalf("scoped latest for partial root = %s, want %s", latestPartial.ID, ids.partial)
	}
	latestData1 := callSnapshot(t, ctx, session, artifact, "latest", map[string]any{
		"host": fixture.Host,
		"tags": []string{"test"},
		"path": fixture.DataDir1,
	})
	if latestData1.ID != ids.mixed {
		t.Fatalf("scoped latest for DataDir1 = %s, want %s", latestData1.ID, ids.mixed)
	}
	artifact.check("scoped latest stays inside the visible selection")

	nestedConfig := filepath.Join(fixture.Root, "visibility-nested.yaml")
	if err := fixture.WriteConfigWithPolicy(nestedConfig, resticBinary, testrepo.ConfigPolicy{
		AllowedHosts: []string{fixture.Host},
		AllowedTags:  []string{"daily", "weekly", "test"},
		AllowedPaths: []string{parentAllowed, childDir, fixture.DataDir1},
	}); err != nil {
		t.Fatal(err)
	}
	nested := connectFixtureServer(t, ctx, binary, nestedConfig)
	nestedPage := callSnapshots(t, ctx, nested, artifact, map[string]any{"repository_id": "tmp-local"})
	if nestedPage.Total != 3 {
		t.Fatalf("nested allowlist visible snapshots = %d, want 3: %+v", nestedPage.Total, nestedPage.Snapshots)
	}
	nestedFind := callFind(t, ctx, nested, artifact, map[string]any{
		"repository_id": "tmp-local",
		"pattern":       "child.txt",
		"path":          childDir,
	})
	if nestedFind.Count != 1 || nestedFind.Groups[0].Matches[0].Path != filepath.Join(childDir, "child.txt") {
		t.Fatalf("nested allowlist find = %+v", nestedFind)
	}
	artifact.check("nested overlapping allowlists and parent/child roots")

	rootConfig := filepath.Join(fixture.Root, "visibility-root.yaml")
	if err := fixture.WriteConfigWithPolicy(rootConfig, resticBinary, testrepo.ConfigPolicy{
		AllowedPaths: []string{"/"},
	}); err != nil {
		t.Fatal(err)
	}
	rootSession := connectFixtureServer(t, ctx, binary, rootConfig)
	rootPage := callSnapshots(t, ctx, rootSession, artifact, map[string]any{"repository_id": "tmp-local"})
	if rootPage.Total != 8 {
		t.Fatalf("root allowlist snapshots = %d, want 8", rootPage.Total)
	}
	rootStats := callStats(t, ctx, rootSession, artifact, map[string]any{
		"repository_id": "tmp-local",
		"mode":          "restore-size",
	})
	if rootStats.SelectedSnapshots != 8 || rootStats.ExcludedSnapshots != 0 {
		t.Fatalf("root allowlist statistics = %+v, want all 8 eligible", rootStats)
	}
	artifact.check("root allowlist makes all roots visible and eligible")
}

type visibilityIDs struct {
	data1      string
	data2      string
	mixed      string
	partial    string
	child      string
	hiddenHost string
	hiddenTag  string
	disjoint   string
}

func snapshotIDsByShape(
	t *testing.T,
	snapshots []restic.Snapshot,
	fixture *testrepo.Fixture,
	hiddenDir, partialDir, childDir, broadDir string,
) visibilityIDs {
	t.Helper()
	ids := visibilityIDs{}
	for _, snapshot := range snapshots {
		switch {
		case snapshot.Hostname == "hidden-host":
			ids.hiddenHost = snapshot.ID
		case slices.Contains(snapshot.Tags, "hidden-tag"):
			ids.hiddenTag = snapshot.ID
		case len(snapshot.Paths) == 2 && slices.Contains(snapshot.Paths, fixture.DataDir1) &&
			slices.Contains(snapshot.Paths, hiddenDir):
			ids.mixed = snapshot.ID
		case slices.Equal(snapshot.Paths, []string{partialDir}):
			ids.partial = snapshot.ID
		case slices.Equal(snapshot.Paths, []string{childDir}):
			ids.child = snapshot.ID
		case slices.Equal(snapshot.Paths, []string{broadDir}):
			ids.disjoint = snapshot.ID
		case slices.Equal(snapshot.Paths, []string{fixture.DataDir1}) && slices.Contains(snapshot.Tags, "daily"):
			ids.data1 = snapshot.ID
		case slices.Equal(snapshot.Paths, []string{fixture.DataDir2}) && slices.Contains(snapshot.Tags, "weekly"):
			ids.data2 = snapshot.ID
		}
	}
	for name, value := range map[string]string{
		"data1": ids.data1, "data2": ids.data2, "mixed": ids.mixed, "partial": ids.partial,
		"child": ids.child, "hidden host": ids.hiddenHost, "hidden tag": ids.hiddenTag, "disjoint": ids.disjoint,
	} {
		if value == "" {
			t.Fatalf("fixture discovery missed %s", name)
		}
	}
	return ids
}

func callSnapshot(
	t *testing.T,
	ctx context.Context,
	session *mcp.ClientSession,
	artifact *e2eArtifact,
	snapshotID string,
	extra ...map[string]any,
) restic.Snapshot {
	t.Helper()
	args := map[string]any{
		"repository_id": "tmp-local",
		"snapshot_id":   snapshotID,
	}
	if len(extra) > 0 {
		for key, value := range extra[0] {
			args[key] = value
		}
	}
	envelope := callE2ETool(t, ctx, session, artifact, "get_snapshot", args)
	var snapshot restic.Snapshot
	unmarshalData(t, envelope, &snapshot)
	return snapshot
}

func callBrowse(
	t *testing.T,
	ctx context.Context,
	session *mcp.ClientSession,
	artifact *e2eArtifact,
	args map[string]any,
) e2eBrowseData {
	t.Helper()
	envelope := callE2ETool(t, ctx, session, artifact, "browse_snapshot", args)
	var browse e2eBrowseData
	unmarshalData(t, envelope, &browse)
	return browse
}

func callStats(
	t *testing.T,
	ctx context.Context,
	session *mcp.ClientSession,
	artifact *e2eArtifact,
	args map[string]any,
) visibilityStats {
	t.Helper()
	envelope := callE2ETool(t, ctx, session, artifact, "repository_stats", args)
	var stats visibilityStats
	unmarshalData(t, envelope, &stats)
	return stats
}
