//go:build integration

package main_test

// Failure modes covered by this executable E2E test:
//
//   - Time selectors are not applied to snapshot selection.
//   - Boundary instants are included or excluded on the wrong side.
//   - Offset-bearing timestamps are compared as wall-clock text.
//   - Invalid, reversed, or empty windows run restic or return success.
//   - Hidden snapshots reappear through a time filter.
//   - Oversized selections lose the narrowing guidance.
//   - Statistics or find silently change their snapshot selection semantics.

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/ThomasCrouzet/restic-defensive-mcp/internal/restic"
	"github.com/ThomasCrouzet/restic-defensive-mcp/internal/testrepo"
)

type windowStats struct {
	Mode              string             `json:"mode"`
	Stats             restic.StatsResult `json:"stats"`
	SelectedSnapshots int                `json:"selected_snapshots"`
	ExcludedSnapshots int                `json:"excluded_snapshots"`
}

type timeWindowOutput struct {
	After   *string `json:"after"`
	Before  *string `json:"before"`
	Bounded bool    `json:"bounded"`
}

type windowContainer struct {
	Window timeWindowOutput `json:"window"`
}

func TestExecutableTimeWindowSelectors(t *testing.T) {
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

	configPath := filepath.Join(fixture.Root, "windows.yaml")
	if err := fixture.WriteConfig(configPath, resticBinary); err != nil {
		t.Fatal(err)
	}
	session := connectFixtureServer(t, ctx, binary, configPath)

	all := callSnapshotPage(t, ctx, session, artifact, map[string]any{"repository_id": "tmp-local"})
	if all.Total != 2 || len(all.Snapshots) != 2 {
		t.Fatalf("fixture snapshots = %+v", all)
	}
	newer := all.Snapshots[0]
	older := all.Snapshots[1]
	if !newer.Time.After(older.Time) {
		t.Fatalf("fixture times are not ordered: newer=%s older=%s", newer.Time, older.Time)
	}

	olderOffset := older.Time.In(time.FixedZone("plus0530", 5*3600+30*60)).Format(time.RFC3339Nano)
	newerOffset := newer.Time.In(time.FixedZone("minus0200", -2*3600)).Format(time.RFC3339Nano)

	page, window := callListWindow(t, ctx, session, artifact, map[string]any{
		"repository_id": "tmp-local",
		"after":         olderOffset,
		"before":        newerOffset,
	})
	if page.Total != 1 || len(page.Snapshots) != 1 || page.Snapshots[0].ID != older.ID {
		t.Fatalf("bounded list = %+v", page)
	}
	if !window.Bounded || window.After == nil || *window.After != older.Time.UTC().Format(time.RFC3339Nano) ||
		window.Before == nil || *window.Before != newer.Time.UTC().Format(time.RFC3339Nano) {
		t.Fatalf("normalized window = %+v", window)
	}
	artifact.check("offset timestamps normalize to UTC and select [after, before)")

	afterPage, _ := callListWindow(t, ctx, session, artifact, map[string]any{
		"repository_id": "tmp-local",
		"after":         newer.Time.Format(time.RFC3339Nano),
	})
	if afterPage.Total != 1 || afterPage.Snapshots[0].ID != newer.ID {
		t.Fatalf("after-only list = %+v", afterPage)
	}
	beforePage, _ := callListWindow(t, ctx, session, artifact, map[string]any{
		"repository_id": "tmp-local",
		"before":        older.Time.Format(time.RFC3339Nano),
	})
	if beforePage.Total != 0 {
		t.Fatalf("before-boundary list = %+v, want empty", beforePage)
	}
	artifact.check("after is inclusive and before is exclusive")

	for name, args := range map[string]map[string]any{
		"invalid": {
			"repository_id": "tmp-local",
			"after":         "not-a-time",
		},
		"reversed": {
			"repository_id": "tmp-local",
			"after":         newer.Time.Format(time.RFC3339Nano),
			"before":        older.Time.Format(time.RFC3339Nano),
		},
		"empty": {
			"repository_id": "tmp-local",
			"after":         older.Time.Format(time.RFC3339Nano),
			"before":        older.Time.Format(time.RFC3339Nano),
		},
	} {
		payload := callE2EToolError(t, ctx, session, artifact, "list_snapshots", args)
		if payload["code"] != "invalid_argument" {
			t.Fatalf("%s window error = %v, want invalid_argument", name, payload)
		}
	}
	artifact.check("invalid, reversed, and empty windows fail without inspection")

	invisible := callE2EToolError(t, ctx, session, artifact, "get_snapshot", map[string]any{
		"repository_id": "tmp-local",
		"snapshot_id":   older.ID,
		"before":        older.Time.Format(time.RFC3339Nano),
	})
	if invisible["code"] != "snapshot_not_found" {
		t.Fatalf("out-of-window concrete snapshot = %v", invisible)
	}
	artifact.check("concrete snapshot outside the window fails closed")

	findEnvelope := callE2ETool(t, ctx, session, artifact, "find_files", map[string]any{
		"repository_id": "tmp-local",
		"pattern":       "*.txt",
		"path":          older.Paths[0],
		"after":         older.Time.Format(time.RFC3339Nano),
		"before":        newer.Time.Format(time.RFC3339Nano),
	})
	var find cursorFindData
	unmarshalData(t, findEnvelope, &find)
	var findContainer windowContainer
	unmarshalData(t, findEnvelope, &findContainer)
	if find.Count != 3 || len(find.Groups) != 1 || !findContainer.Window.Bounded {
		t.Fatalf("time-window find = %+v window=%+v", find, findContainer.Window)
	}
	artifact.check("find selects concrete snapshots before the authorized scan")

	statsEnvelope := callE2ETool(t, ctx, session, artifact, "repository_stats", map[string]any{
		"repository_id": "tmp-local",
		"mode":          "restore-size",
		"after":         older.Time.Format(time.RFC3339Nano),
		"before":        newer.Time.Format(time.RFC3339Nano),
	})
	var stats windowStats
	unmarshalData(t, statsEnvelope, &stats)
	var statsContainer windowContainer
	unmarshalData(t, statsEnvelope, &statsContainer)
	if stats.SelectedSnapshots != 1 || stats.Stats.TotalFileCount == 0 || !statsContainer.Window.Bounded {
		t.Fatalf("time-window stats = %+v window=%+v", stats, statsContainer.Window)
	}
	artifact.check("statistics applies the window before selecting snapshots")

	// Hidden snapshots remain hidden even when the window includes them.
	hiddenConfig := filepath.Join(fixture.Root, "windows-hidden.yaml")
	if err := fixture.WriteConfigForPaths(hiddenConfig, resticBinary, []string{fixture.DataDir1}); err != nil {
		t.Fatal(err)
	}
	hiddenSession := connectFixtureServer(t, ctx, binary, hiddenConfig)
	hiddenPage, _ := callListWindow(t, ctx, hiddenSession, artifact, map[string]any{
		"repository_id": "tmp-local",
		"after":         older.Time.Format(time.RFC3339Nano),
	})
	if hiddenPage.Total != 1 || hiddenPage.Snapshots[0].Paths[0] != fixture.DataDir1 {
		t.Fatalf("hidden snapshot crossed the window filter: %+v", hiddenPage)
	}
	artifact.check("time windows do not bypass path policy")

	// An over-broad selection still reports how to narrow it.
	maxConfig := filepath.Join(fixture.Root, "windows-max.yaml")
	if err := fixture.WriteConfigForPaths(maxConfig, resticBinary, []string{"/"}); err != nil {
		t.Fatal(err)
	}
	rewriteMaxSnapshots(t, maxConfig, 1)
	maxSession := connectFixtureServer(t, ctx, binary, maxConfig)
	overBroad := callE2EToolError(t, ctx, maxSession, artifact, "find_files", map[string]any{
		"repository_id": "tmp-local",
		"pattern":       "*.txt",
		"after":         older.Time.Format(time.RFC3339Nano),
	})
	if overBroad["code"] != "invalid_argument" ||
		!strings.Contains(overBroad["message"], "after") || !strings.Contains(overBroad["message"], "before") {
		t.Fatalf("over-broad window error = %v", overBroad)
	}
	artifact.check("oversized selection explains time-window narrowing")

	// Cursors carry the original window and reject a changed one.
	cursorConfig := filepath.Join(fixture.Root, "windows-cursor.yaml")
	if err := fixture.WriteConfigForPaths(cursorConfig, resticBinary, []string{"/"}); err != nil {
		t.Fatal(err)
	}
	cursorSession := connectFixtureServer(t, ctx, binary, cursorConfig)
	cursorPage, _ := callListWindow(t, ctx, cursorSession, artifact, map[string]any{
		"repository_id": "tmp-local",
		"after":         older.Time.Format(time.RFC3339Nano),
		"limit":         1,
	})
	if cursorPage.NextCursor == "" {
		t.Fatalf("windowed list did not produce a cursor: %+v", cursorPage)
	}
	changedWindow := callE2EToolError(t, ctx, cursorSession, artifact, "list_snapshots", map[string]any{
		"repository_id": "tmp-local",
		"cursor":        cursorPage.NextCursor,
		"before":        newer.Time.Format(time.RFC3339Nano),
	})
	if changedWindow["code"] != "invalid_argument" {
		t.Fatalf("changed window with cursor = %v", changedWindow)
	}
	artifact.check("cursors reject a changed time window")
}

func callListWindow(
	t *testing.T,
	ctx context.Context,
	session *mcp.ClientSession,
	artifact *e2eArtifact,
	args map[string]any,
) (cursorSnapshotPage, timeWindowOutput) {
	t.Helper()
	envelope := callE2ETool(t, ctx, session, artifact, "list_snapshots", args)
	var page cursorSnapshotPage
	unmarshalData(t, envelope, &page)
	page.Truncated = envelope.Truncated
	var container windowContainer
	unmarshalData(t, envelope, &container)
	return page, container.Window
}

func rewriteMaxSnapshots(t *testing.T, path string, value int) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	updated := strings.Replace(string(data), "max_snapshots: 50", "max_snapshots: "+strconv.Itoa(value), 1)
	if updated == string(data) {
		t.Fatalf("fixture config did not contain the expected max_snapshots value")
	}
	if err := os.WriteFile(path, []byte(updated), 0o600); err != nil {
		t.Fatal(err)
	}
}
