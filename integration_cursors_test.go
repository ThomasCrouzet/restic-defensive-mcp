//go:build integration

package main_test

// Failure modes covered by this executable E2E test:
//
//   - Paged browse/search cannot be fully traversed.
//   - Cursors duplicate, omit, or reorder visible entries.
//   - Hidden entries consume the visible page budget.
//   - Cursors survive snapshot removal, changed filters, tampering, or reuse.
//   - Scoped `latest` resolves again on a later page.
//   - Snapshot-list continuation shifts or drops entries after a new backup.

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

type cursorSnapshotPage struct {
	Snapshots  []restic.Snapshot `json:"snapshots"`
	Count      int               `json:"count"`
	Total      int               `json:"total"`
	Offset     int               `json:"offset"`
	Limit      int               `json:"limit"`
	NextCursor string            `json:"next_cursor"`
	Truncated  bool              `json:"-"`
}

type cursorBrowseData struct {
	Path       string        `json:"path"`
	Nodes      []restic.Node `json:"nodes"`
	Count      int           `json:"count"`
	Total      int           `json:"total"`
	Recursive  bool          `json:"recursive"`
	Incomplete bool          `json:"incomplete"`
	NextCursor string        `json:"next_cursor"`
	Truncated  bool          `json:"-"`
}

type cursorFindData struct {
	Groups     []restic.FindGroup `json:"groups"`
	Count      int                `json:"count"`
	Total      int                `json:"total"`
	Incomplete bool               `json:"incomplete"`
	NextCursor string             `json:"next_cursor"`
	Truncated  bool               `json:"-"`
}

func TestExecutableCursorTraversal(t *testing.T) {
	artifact := newE2EArtifact(t)
	t.Cleanup(func() { artifact.finish(t, "") })

	resticBinary, err := exec.LookPath("restic")
	if err != nil {
		t.Fatalf("integration E2E requires restic: %v", err)
	}
	artifact.environment["restic_binary"] = resticBinary
	artifact.environment["restic_version"] = commandText(t, resticBinary, "version")

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
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

	configPath := filepath.Join(fixture.Root, "cursors.yaml")
	if err := fixture.WriteConfig(configPath, resticBinary); err != nil {
		t.Fatal(err)
	}
	session := connectFixtureServer(t, ctx, binary, configPath)

	initial := callSnapshotPage(t, ctx, session, artifact, map[string]any{"repository_id": "tmp-local"})
	if initial.Total != 2 {
		t.Fatalf("initial snapshots = %d, want 2", initial.Total)
	}
	initialIDs := make([]string, 0, len(initial.Snapshots))
	byRoot := make(map[string]restic.Snapshot, len(initial.Snapshots))
	for _, snapshot := range initial.Snapshots {
		initialIDs = append(initialIDs, snapshot.ID)
		if len(snapshot.Paths) == 1 {
			byRoot[snapshot.Paths[0]] = snapshot
		}
	}
	slices.Sort(initialIDs)
	data1 := byRoot[fixture.DataDir1]
	if data1.ID == "" {
		t.Fatal("fixture has no DataDir1 snapshot")
	}

	// Snapshot-list continuation and single-use tokens.
	listFirst := callSnapshotPage(t, ctx, session, artifact, map[string]any{
		"repository_id": "tmp-local",
		"limit":         1,
	})
	if listFirst.Count != 1 || listFirst.Total != 2 || listFirst.NextCursor == "" || !listFirst.Truncated {
		t.Fatalf("first snapshot cursor page = %+v truncated=%v", listFirst, listFirst.Truncated)
	}
	listFirstCursor := listFirst.NextCursor
	listSecond := callSnapshotPage(t, ctx, session, artifact, map[string]any{
		"repository_id": "tmp-local",
		"limit":         1,
		"cursor":        listFirstCursor,
	})
	if listSecond.Count != 1 || listSecond.NextCursor != "" || listSecond.Truncated || listSecond.Offset != 1 {
		t.Fatalf("second snapshot cursor page = %+v truncated=%v", listSecond, listSecond.Truncated)
	}
	if listFirst.Snapshots[0].ID == listSecond.Snapshots[0].ID {
		t.Fatal("snapshot cursor returned a duplicate")
	}
	if !slices.Contains(initialIDs, listFirst.Snapshots[0].ID) ||
		!slices.Contains(initialIDs, listSecond.Snapshots[0].ID) {
		t.Fatal("snapshot cursor returned an unexpected snapshot")
	}
	reused := callE2EToolError(t, ctx, session, artifact, "list_snapshots", map[string]any{
		"repository_id": "tmp-local",
		"cursor":        listFirstCursor,
		"limit":         1,
	})
	if reused["code"] != "cursor_invalid" {
		t.Fatalf("reused list cursor = %v, want cursor_invalid", reused)
	}
	artifact.check("snapshot list cursor traverses the initial selection once")

	// Additions after the first page are not spliced into the continuation.
	if err := fixture.AddSnapshot(ctx, fixture.Host, []string{"test"}, fixture.DataDir2); err != nil {
		t.Fatal(err)
	}
	listBeforeAdd := callSnapshotPage(t, ctx, session, artifact, map[string]any{
		"repository_id": "tmp-local",
		"limit":         1,
	})
	if listBeforeAdd.Total != 3 || listBeforeAdd.NextCursor == "" {
		t.Fatalf("snapshot list before addition = %+v", listBeforeAdd)
	}
	addCursor := listBeforeAdd.NextCursor
	if err := fixture.AddSnapshot(ctx, fixture.Host, []string{"test"}, fixture.DataDir1); err != nil {
		t.Fatal(err)
	}
	listAfterAdd := callSnapshotPage(t, ctx, session, artifact, map[string]any{
		"repository_id": "tmp-local",
		"limit":         1,
		"cursor":        addCursor,
	})
	if listAfterAdd.Count != 1 || listAfterAdd.Total != 3 {
		t.Fatalf("snapshot cursor after addition = %+v", listAfterAdd)
	}
	if listAfterAdd.Snapshots[0].Time.After(listBeforeAdd.Snapshots[0].Time) {
		t.Fatalf("newer snapshot was spliced into an existing list cursor: %+v", listAfterAdd.Snapshots)
	}
	current := callSnapshotPage(t, ctx, session, artifact, map[string]any{
		"repository_id": "tmp-local",
		"limit":         200,
	})
	if current.Total != 4 {
		t.Fatalf("new snapshots not visible outside the old cursor: %d", current.Total)
	}
	artifact.check("snapshot cursor does not include later additions")

	// Changed filters and tampering.
	changedList := callE2EToolError(t, ctx, session, artifact, "list_snapshots", map[string]any{
		"repository_id": "tmp-local",
		"cursor":        listAfterAdd.NextCursor,
		"tags":          []string{"test"},
	})
	if changedList["code"] != "invalid_argument" {
		t.Fatalf("changed list filters with cursor = %v", changedList)
	}
	tampered := callE2EToolError(t, ctx, session, artifact, "list_snapshots", map[string]any{
		"repository_id": "tmp-local",
		"cursor":        "tampered-cursor",
	})
	if tampered["code"] != "cursor_invalid" {
		t.Fatalf("tampered list cursor = %v", tampered)
	}
	artifact.check("list cursor rejects changed filters and tampering")

	// Browse traversal below the visible node count.
	firstBrowse := callBrowseCursor(t, ctx, session, artifact, map[string]any{
		"repository_id": "tmp-local",
		"snapshot_id":   data1.ID,
		"path":          fixture.DataDir1,
		"recursive":     true,
		"limit":         2,
	})
	if firstBrowse.Count != 2 || firstBrowse.NextCursor == "" || !firstBrowse.Truncated || firstBrowse.Incomplete {
		t.Fatalf("first browse cursor page = %+v truncated=%v", firstBrowse, firstBrowse.Truncated)
	}
	browseFirstCursor := firstBrowse.NextCursor
	allBrowse := callBrowseCursor(t, ctx, session, artifact, map[string]any{
		"repository_id": "tmp-local",
		"snapshot_id":   data1.ID,
		"path":          fixture.DataDir1,
		"recursive":     true,
		"limit":         200,
	})
	seenNodes := make(map[string]bool)
	for _, node := range firstBrowse.Nodes {
		seenNodes[node.Path] = true
	}
	cursor := browseFirstCursor
	for cursor != "" {
		page := callBrowseCursor(t, ctx, session, artifact, map[string]any{
			"repository_id": "tmp-local",
			"cursor":        cursor,
			"limit":         2,
		})
		for _, node := range page.Nodes {
			if seenNodes[node.Path] {
				t.Fatalf("browse cursor duplicated %q", node.Path)
			}
			seenNodes[node.Path] = true
		}
		cursor = page.NextCursor
	}
	if len(seenNodes) != allBrowse.Count {
		t.Fatalf("browse cursor saw %d nodes, single page = %d", len(seenNodes), allBrowse.Count)
	}
	for _, node := range allBrowse.Nodes {
		if !seenNodes[node.Path] {
			t.Fatalf("browse cursor omitted %q", node.Path)
		}
	}
	artifact.check("browse cursor traverses every visible node exactly once")

	changedBrowse := callE2EToolError(t, ctx, session, artifact, "browse_snapshot", map[string]any{
		"repository_id": "tmp-local",
		"cursor":        browseFirstCursor,
		"path":          fixture.DataDir2,
	})
	if changedBrowse["code"] != "invalid_argument" {
		t.Fatalf("changed browse scope with cursor = %v", changedBrowse)
	}
	artifact.check("browse cursor rejects changed scope")

	// Find traversal with hidden entries filtered before paging.
	hiddenDir := filepath.Join(fixture.Root, "cursor-hidden")
	if err := fixture.WriteFile(hiddenDir, "secret.txt", []byte("hidden\n")); err != nil {
		t.Fatal(err)
	}
	if err := fixture.AddSnapshot(ctx, fixture.Host, []string{"test"}, fixture.DataDir1, hiddenDir); err != nil {
		t.Fatal(err)
	}
	// Re-discover the mixed multi-root snapshot from the unrestricted view.
	wideConfig := filepath.Join(fixture.Root, "cursors-wide.yaml")
	if err := fixture.WriteConfigForPaths(wideConfig, resticBinary, []string{"/"}); err != nil {
		t.Fatal(err)
	}
	wide := connectFixtureServer(t, ctx, binary, wideConfig)
	widePage := callSnapshotPage(t, ctx, wide, artifact, map[string]any{
		"repository_id": "tmp-local",
		"limit":         200,
	})
	var mixedID string
	for _, snapshot := range widePage.Snapshots {
		if len(snapshot.Paths) == 2 &&
			slices.Contains(snapshot.Paths, fixture.DataDir1) &&
			slices.Contains(snapshot.Paths, hiddenDir) {
			mixedID = snapshot.ID
		}
	}
	if mixedID == "" {
		t.Fatalf("mixed snapshot not found in %+v", widePage.Snapshots)
	}

	firstFind := callFindCursor(t, ctx, session, artifact, map[string]any{
		"repository_id": "tmp-local",
		"pattern":       "*.txt",
		"snapshot_id":   mixedID,
		"path":          fixture.DataDir1,
		"limit":         1,
	})
	if firstFind.Count != 1 || firstFind.NextCursor == "" || firstFind.Total != 3 ||
		!firstFind.Truncated || firstFind.Incomplete {
		t.Fatalf("first find cursor page = %+v truncated=%v", firstFind, firstFind.Truncated)
	}
	findFirstCursor := firstFind.NextCursor
	allFind := callFindCursor(t, ctx, session, artifact, map[string]any{
		"repository_id": "tmp-local",
		"pattern":       "*.txt",
		"snapshot_id":   mixedID,
		"path":          fixture.DataDir1,
		"limit":         200,
	})
	if allFind.Count != 3 || allFind.Total != 3 {
		t.Fatalf("single-page find = %+v", allFind)
	}
	seenMatches := make(map[string]bool)
	pageHits := 0
	for _, group := range firstFind.Groups {
		pageHits += int(group.Hits)
		for _, match := range group.Matches {
			if match.Name == "secret.txt" || match.Path == filepath.Join(hiddenDir, "secret.txt") {
				t.Fatalf("find cursor leaked a hidden match: %+v", match)
			}
			seenMatches[match.Path] = true
		}
	}
	if pageHits != firstFind.Count {
		t.Fatalf("find page hits = %d, count = %d", pageHits, firstFind.Count)
	}
	cursor = findFirstCursor
	lastTotal := firstFind.Total
	for cursor != "" {
		page := callFindCursor(t, ctx, session, artifact, map[string]any{
			"repository_id": "tmp-local",
			"cursor":        cursor,
			"limit":         1,
		})
		if page.Total != 0 && page.Total != lastTotal {
			t.Fatalf("find total changed between pages: %d then %d", lastTotal, page.Total)
		}
		hits := 0
		for _, group := range page.Groups {
			hits += int(group.Hits)
			for _, match := range group.Matches {
				if seenMatches[match.Path] {
					t.Fatalf("find cursor duplicated %q", match.Path)
				}
				seenMatches[match.Path] = true
			}
		}
		if hits != page.Count {
			t.Fatalf("find page hits = %d, count = %d", hits, page.Count)
		}
		cursor = page.NextCursor
	}
	if len(seenMatches) != allFind.Count {
		t.Fatalf("find cursor saw %d matches, single page = %d", len(seenMatches), allFind.Count)
	}
	for _, group := range allFind.Groups {
		for _, match := range group.Matches {
			if !seenMatches[match.Path] {
				t.Fatalf("find cursor omitted %q", match.Path)
			}
		}
	}
	artifact.check("find cursor traverses visible matches without hidden budget use")

	changedFind := callE2EToolError(t, ctx, session, artifact, "find_files", map[string]any{
		"repository_id": "tmp-local",
		"cursor":        findFirstCursor,
		"pattern":       "*.does-not-exist",
	})
	if changedFind["code"] != "invalid_argument" {
		t.Fatalf("changed find filter with cursor = %v", changedFind)
	}
	artifact.check("find cursor rejects changed filters")

	// A removed concrete snapshot makes a stored selection stale.
	if err := fixture.AddSnapshot(ctx, fixture.Host, []string{"test"}, fixture.DataDir1); err != nil {
		t.Fatal(err)
	}
	staleSnapshots := callSnapshotPage(t, ctx, session, artifact, map[string]any{
		"repository_id": "tmp-local",
		"path":          fixture.DataDir1,
		"limit":         200,
	})
	staleSnapshotID := staleSnapshots.Snapshots[0].ID
	firstStale := callFindCursor(t, ctx, session, artifact, map[string]any{
		"repository_id": "tmp-local",
		"pattern":       "*.txt",
		"snapshot_id":   staleSnapshotID,
		"path":          fixture.DataDir1,
		"limit":         1,
	})
	if firstStale.NextCursor == "" {
		t.Fatal("stale-selection scenario did not produce a cursor")
	}
	staleCursor := firstStale.NextCursor
	if err := fixture.ForgetSnapshot(ctx, staleSnapshotID); err != nil {
		t.Fatal(err)
	}
	stale := callE2EToolError(t, ctx, session, artifact, "find_files", map[string]any{
		"repository_id": "tmp-local",
		"cursor":        staleCursor,
		"limit":         1,
	})
	if stale["code"] != "stale_selection" {
		t.Fatalf("removed snapshot cursor = %v, want stale_selection", stale)
	}
	artifact.check("find cursor fails closed when the snapshot selection changes")

	// Scoped latest is resolved once; removing it makes later pages stale.
	if err := fixture.AddSnapshot(ctx, fixture.Host, []string{"test"}, fixture.DataDir2); err != nil {
		t.Fatal(err)
	}
	latest := getCursorSnapshot(t, ctx, session, artifact, "latest", map[string]any{
		"host": fixture.Host,
		"tags": []string{"test"},
		"path": fixture.DataDir2,
	})
	latestBrowse := callBrowseCursor(t, ctx, session, artifact, map[string]any{
		"repository_id": "tmp-local",
		"snapshot_id":   "latest",
		"host":          fixture.Host,
		"path":          fixture.DataDir2,
		"limit":         1,
	})
	if latestBrowse.NextCursor == "" {
		t.Fatal("latest browse did not produce a cursor")
	}
	latestCursor := latestBrowse.NextCursor
	if err := fixture.ForgetSnapshot(ctx, latest.ID); err != nil {
		t.Fatal(err)
	}
	latestStale := callE2EToolError(t, ctx, session, artifact, "browse_snapshot", map[string]any{
		"repository_id": "tmp-local",
		"cursor":        latestCursor,
		"limit":         1,
	})
	if latestStale["code"] != "stale_selection" {
		t.Fatalf("removed latest cursor = %v, want stale_selection", latestStale)
	}
	artifact.check("latest is fixed to the concrete snapshot for continuation")
}

func callSnapshotPage(
	t *testing.T,
	ctx context.Context,
	session *mcp.ClientSession,
	artifact *e2eArtifact,
	args map[string]any,
) cursorSnapshotPage {
	t.Helper()
	envelope := callE2ETool(t, ctx, session, artifact, "list_snapshots", args)
	var page cursorSnapshotPage
	unmarshalData(t, envelope, &page)
	page.Truncated = envelope.Truncated
	return page
}

func callBrowseCursor(
	t *testing.T,
	ctx context.Context,
	session *mcp.ClientSession,
	artifact *e2eArtifact,
	args map[string]any,
) cursorBrowseData {
	t.Helper()
	envelope := callE2ETool(t, ctx, session, artifact, "browse_snapshot", args)
	var page cursorBrowseData
	unmarshalData(t, envelope, &page)
	page.Truncated = envelope.Truncated
	return page
}

func callFindCursor(
	t *testing.T,
	ctx context.Context,
	session *mcp.ClientSession,
	artifact *e2eArtifact,
	args map[string]any,
) cursorFindData {
	t.Helper()
	envelope := callE2ETool(t, ctx, session, artifact, "find_files", args)
	var page cursorFindData
	unmarshalData(t, envelope, &page)
	page.Truncated = envelope.Truncated
	return page
}

func getCursorSnapshot(
	t *testing.T,
	ctx context.Context,
	session *mcp.ClientSession,
	artifact *e2eArtifact,
	snapshotID string,
	extra map[string]any,
) restic.Snapshot {
	t.Helper()
	args := map[string]any{
		"repository_id": "tmp-local",
		"snapshot_id":   snapshotID,
	}
	for key, value := range extra {
		args[key] = value
	}
	envelope := callE2ETool(t, ctx, session, artifact, "get_snapshot", args)
	var snapshot restic.Snapshot
	unmarshalData(t, envelope, &snapshot)
	return snapshot
}
