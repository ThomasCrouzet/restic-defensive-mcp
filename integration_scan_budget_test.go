//go:build integration

package main_test

// Failure modes covered by this executable E2E test:
//
//   - A large search with many hidden early entries delays or blocks the page.
//   - Hidden matches consume the visible page budget or leak into results.
//   - Exhausted scan or response budgets are reported as complete successes.
//   - Incremental continuation duplicates or omits visible matches.
//
// The fixture is disposable and local; no reports or transcripts are published.

import (
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/ThomasCrouzet/restic-defensive-mcp/internal/restic"
	"github.com/ThomasCrouzet/restic-defensive-mcp/internal/testrepo"
)

type scanFindPage struct {
	Groups          []restic.FindGroup `json:"groups"`
	Count           int                `json:"count"`
	Total           int                `json:"total"`
	Incomplete      bool               `json:"incomplete"`
	ScanComplete    bool               `json:"scan_complete"`
	BudgetExhausted bool               `json:"budget_exhausted"`
	NextCursor      string             `json:"next_cursor"`
	Truncated       bool               `json:"-"`
}

func TestExecutableIncrementalScanBudget(t *testing.T) {
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

	hiddenDir := filepath.Join(fixture.Root, "large-hidden")
	visibleDir := filepath.Join(fixture.Root, "large-visible")
	const perDir = 200
	for i := 0; i < perDir; i++ {
		name := fmt.Sprintf("entry-%04d.txt", i)
		if err := fixture.WriteFile(hiddenDir, name, []byte("hidden\n")); err != nil {
			t.Fatal(err)
		}
		if err := fixture.WriteFile(visibleDir, name, []byte("visible\n")); err != nil {
			t.Fatal(err)
		}
	}
	if err := fixture.AddSnapshot(ctx, fixture.Host, []string{"daily", "test"}, hiddenDir, visibleDir); err != nil {
		t.Fatal(err)
	}

	wideConfig := filepath.Join(fixture.Root, "scan-wide.yaml")
	if err := fixture.WriteConfigForPaths(wideConfig, resticBinary, []string{"/"}); err != nil {
		t.Fatal(err)
	}
	wide := connectFixtureServer(t, ctx, binary, wideConfig)
	wideSnapshots := callSnapshots(t, ctx, wide, artifact, map[string]any{
		"repository_id": "tmp-local",
		"limit":         200,
	})
	var mixedID string
	for _, snapshot := range wideSnapshots.Snapshots {
		if len(snapshot.Paths) == 2 &&
			slices.Contains(snapshot.Paths, hiddenDir) &&
			slices.Contains(snapshot.Paths, visibleDir) {
			mixedID = snapshot.ID
		}
	}
	if mixedID == "" {
		t.Fatalf("mixed scan snapshot not found in %+v", wideSnapshots.Snapshots)
	}

	narrowConfig := filepath.Join(fixture.Root, "scan-narrow.yaml")
	if err := fixture.WriteConfigWithPolicy(narrowConfig, resticBinary, testrepo.ConfigPolicy{
		AllowedHosts:      []string{fixture.Host},
		AllowedTags:       []string{"daily", "weekly", "test"},
		AllowedPaths:      []string{visibleDir},
		MaxFindResults:    500,
		MaxNodes:          500,
		MaxDecodedEntries: 50000,
	}); err != nil {
		t.Fatal(err)
	}
	narrow := connectFixtureServer(t, ctx, binary, narrowConfig)

	// A small page must arrive with visible matches even though every hidden
	// path sorts before the allowed root.
	first := callFindScan(t, ctx, narrow, artifact, map[string]any{
		"repository_id": "tmp-local",
		"pattern":       "*.txt",
		"snapshot_id":   mixedID,
		"path":          visibleDir,
		"limit":         10,
	})
	if first.Count != 10 || first.NextCursor == "" || !first.Truncated {
		t.Fatalf("first bounded page = %+v truncated=%v", first, first.Truncated)
	}
	if first.Incomplete || first.BudgetExhausted {
		t.Fatalf("first page should not report budget exhaustion: %+v", first)
	}
	if first.Total != 0 && first.Total != perDir {
		t.Fatalf("first page total = %d, want 0 (unknown) or %d", first.Total, perDir)
	}
	for _, group := range first.Groups {
		for _, match := range group.Matches {
			if !pathWithin(visibleDir, match.Path) {
				t.Fatalf("hidden match leaked through the page: %s", match.Path)
			}
		}
	}
	artifact.check("bounded page filters hidden entries before the page budget")

	// Traverse the complete visible result set with a larger page.
	seen := make(map[string]bool)
	for _, group := range first.Groups {
		for _, match := range group.Matches {
			seen[match.Path] = true
		}
	}
	cursor := first.NextCursor
	pages := 1
	for cursor != "" {
		page := callFindScan(t, ctx, narrow, artifact, map[string]any{
			"repository_id": "tmp-local",
			"cursor":        cursor,
			"limit":         50,
		})
		if page.Incomplete || page.BudgetExhausted {
			t.Fatalf("continuation page unexpectedly incomplete: %+v", page)
		}
		for _, group := range page.Groups {
			for _, match := range group.Matches {
				if !pathWithin(visibleDir, match.Path) {
					t.Fatalf("hidden match leaked through continuation: %s", match.Path)
				}
				if seen[match.Path] {
					t.Fatalf("continuation duplicated %s", match.Path)
				}
				seen[match.Path] = true
			}
		}
		cursor = page.NextCursor
		pages++
	}
	if len(seen) != perDir {
		t.Fatalf("continuation saw %d visible matches, want %d across %d pages", len(seen), perDir, pages)
	}
	artifact.check("incremental continuation reaches every visible match exactly once")

	// A public response limit is a hard failure, not a truncated success.
	responseConfig := filepath.Join(fixture.Root, "scan-response-limit.yaml")
	if err := fixture.WriteConfigWithPolicy(responseConfig, resticBinary, testrepo.ConfigPolicy{
		AllowedHosts:     []string{fixture.Host},
		AllowedTags:      []string{"daily", "weekly", "test"},
		AllowedPaths:     []string{visibleDir},
		MaxFindResults:   500,
		MaxResponseBytes: 16384,
	}); err != nil {
		t.Fatal(err)
	}
	responseLimited := connectFixtureServer(t, ctx, binary, responseConfig)
	responseErr := callE2EToolError(t, ctx, responseLimited, artifact, "find_files", map[string]any{
		"repository_id": "tmp-local",
		"pattern":       "*.txt",
		"snapshot_id":   mixedID,
		"path":          visibleDir,
		"limit":         300,
	})
	if responseErr["code"] != "response_limit_exceeded" {
		t.Fatalf("oversized public response = %v, want response_limit_exceeded", responseErr)
	}
	artifact.check("public response ceiling rejects oversized pages")

	// A decoded-entry budget must be reported as incomplete, not success.
	budgetConfig := filepath.Join(fixture.Root, "scan-decoded-limit.yaml")
	if err := fixture.WriteConfigWithPolicy(budgetConfig, resticBinary, testrepo.ConfigPolicy{
		AllowedHosts:      []string{fixture.Host},
		AllowedTags:       []string{"daily", "weekly", "test"},
		AllowedPaths:      []string{visibleDir},
		MaxFindResults:    500,
		MaxDecodedEntries: 5,
	}); err != nil {
		t.Fatal(err)
	}
	budgetLimited := connectFixtureServer(t, ctx, binary, budgetConfig)
	budgetPage := callFindScan(t, ctx, budgetLimited, artifact, map[string]any{
		"repository_id": "tmp-local",
		"pattern":       "*.txt",
		"snapshot_id":   mixedID,
		"path":          visibleDir,
		"limit":         10,
	})
	if !budgetPage.Incomplete || !budgetPage.BudgetExhausted || !budgetPage.Truncated {
		t.Fatalf("exhausted decoded budget = %+v truncated=%v", budgetPage, budgetPage.Truncated)
	}
	if budgetPage.Count >= 10 {
		t.Fatalf("decoded budget did not bound the page: %+v", budgetPage)
	}
	artifact.check("exhausted decoded budget is reported as incomplete")
}

func callFindScan(
	t *testing.T,
	ctx context.Context,
	session *mcp.ClientSession,
	artifact *e2eArtifact,
	args map[string]any,
) scanFindPage {
	t.Helper()
	envelope := callE2ETool(t, ctx, session, artifact, "find_files", args)
	var page scanFindPage
	unmarshalData(t, envelope, &page)
	page.Truncated = envelope.Truncated
	return page
}
