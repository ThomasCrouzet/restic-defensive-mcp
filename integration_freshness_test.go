//go:build integration

package main_test

// Failure modes covered by this executable E2E test:
//
//   - Freshness invents a healthy/unhealthy judgment without a cadence.
//   - Boundary ages are classified incorrectly or the observation time is absent.
//   - Hidden hosts/tags/paths or partial path visibility are not explicit.
//   - "No visible snapshots" becomes a claim that no backups exist.
//   - Discovery tools gain backend access or freshness bypasses a cursor.
//
// The summary is opt-in on list_snapshots and uses only snapshot metadata that
// the same request was already authorized to read.

import (
	"context"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/ThomasCrouzet/restic-defensive-mcp/internal/testrepo"
)

type freshnessGroup struct {
	Key                   string   `json:"key"`
	Hostname              string   `json:"hostname"`
	Tags                  []string `json:"tags"`
	LatestSnapshotID      string   `json:"latest_snapshot_id"`
	LatestSnapshotTime    string   `json:"latest_snapshot_time"`
	AgeSeconds            int64    `json:"age_seconds"`
	ExpectedCadence       string   `json:"expected_cadence"`
	Overdue               *bool    `json:"overdue"`
	PartialPathVisibility bool     `json:"partial_path_visibility"`
	VisibleSnapshotCount  int      `json:"visible_snapshot_count"`
}

type freshnessSummary struct {
	ObservedAt            string `json:"observed_at"`
	Status                string `json:"status"`
	GroupBy               string `json:"group_by"`
	ExpectedCadence       string `json:"expected_cadence"`
	NoVisibleSnapshots    bool   `json:"no_visible_snapshots"`
	PartialPathVisibility bool   `json:"partial_path_visibility"`
	Incomplete            bool   `json:"incomplete"`
	HiddenScope           struct {
		Hosts bool `json:"hosts"`
		Tags  bool `json:"tags"`
		Paths bool `json:"paths"`
	} `json:"hidden_scope"`
	Scope struct {
		Hosts  []string `json:"hosts"`
		Tags   []string `json:"tags"`
		Paths  []string `json:"paths"`
		Window struct {
			After   *string `json:"after"`
			Before  *string `json:"before"`
			Bounded bool    `json:"bounded"`
		} `json:"window"`
	} `json:"scope"`
	Groups []freshnessGroup `json:"groups"`
	Notes  []string         `json:"notes"`
}

type freshnessPage struct {
	Snapshots []struct {
		ID      string `json:"id"`
		Summary any    `json:"summary"`
	} `json:"snapshots"`
	Freshness *freshnessSummary `json:"freshness"`
}

func TestExecutableFreshnessSummary(t *testing.T) {
	artifact := newE2EArtifact(t)
	t.Cleanup(func() { artifact.finish(t, "") })

	resticBinary, err := exec.LookPath("restic")
	if err != nil {
		t.Fatalf("integration E2E requires restic: %v", err)
	}
	artifact.environment["restic_binary"] = resticBinary
	artifact.environment["restic_version"] = commandText(t, resticBinary, "version")

	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
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

	if err := fixture.AddSnapshotAt(ctx, "old-host", []string{"test"}, time.Now().UTC().Add(-2*time.Hour), fixture.DataDir2); err != nil {
		t.Fatal(err)
	}

	configPath := filepath.Join(fixture.Root, "freshness.yaml")
	if err := fixture.WriteConfigWithPolicy(configPath, resticBinary, testrepo.ConfigPolicy{
		AllowedHosts: []string{fixture.Host, "old-host"},
		AllowedTags:  []string{"daily", "weekly", "test"},
		AllowedPaths: []string{fixture.DataDir1, fixture.DataDir2},
	}); err != nil {
		t.Fatal(err)
	}
	session := connectFixtureServer(t, ctx, binary, configPath)

	page := callFreshness(t, ctx, session, artifact, map[string]any{
		"repository_id":     "tmp-local",
		"limit":             200,
		"include_freshness": true,
		"expected_cadence":  "1h",
	})
	if page.Freshness == nil {
		t.Fatal("freshness summary missing")
	}
	freshness := page.Freshness
	if freshness.Status != "overdue" || freshness.ExpectedCadence != "1h0m0s" ||
		freshness.NoVisibleSnapshots || freshness.Incomplete ||
		!freshness.HiddenScope.Hosts || !freshness.HiddenScope.Tags || !freshness.HiddenScope.Paths {
		t.Fatalf("freshness = %+v", freshness)
	}
	if _, err := time.Parse(time.RFC3339Nano, freshness.ObservedAt); err != nil {
		t.Fatalf("observed_at = %q: %v", freshness.ObservedAt, err)
	}
	if len(freshness.Groups) != 2 {
		t.Fatalf("freshness groups = %+v", freshness.Groups)
	}
	for _, group := range freshness.Groups {
		if group.Overdue == nil {
			t.Fatalf("group without overdue judgment: %+v", group)
		}
		if group.Hostname == "old-host" && !*group.Overdue {
			t.Fatalf("old-host not overdue: %+v", group)
		}
		if group.Hostname == fixture.Host && *group.Overdue {
			t.Fatalf("recent host marked overdue: %+v", group)
		}
	}
	artifact.check("overdue and recent groups with explicit observation time")

	noCadence := callFreshness(t, ctx, session, artifact, map[string]any{
		"repository_id":     "tmp-local",
		"limit":             200,
		"include_freshness": true,
	})
	if noCadence.Freshness == nil || noCadence.Freshness.Status != "recency_only" {
		t.Fatalf("no-cadence freshness = %+v", noCadence.Freshness)
	}
	for _, group := range noCadence.Freshness.Groups {
		if group.Overdue != nil {
			t.Fatalf("group invented an overdue judgment without cadence: %+v", group)
		}
	}
	artifact.check("recency without an expected cadence has no health judgment")

	tagged := callFreshness(t, ctx, session, artifact, map[string]any{
		"repository_id":      "tmp-local",
		"limit":              200,
		"include_freshness":  true,
		"freshness_group_by": "host_tag",
	})
	if tagged.Freshness == nil || tagged.Freshness.GroupBy != "host_tag" || len(tagged.Freshness.Groups) < 3 {
		t.Fatalf("host_tag freshness = %+v", tagged.Freshness)
	}
	artifact.check("host and host_tag grouping")

	for name, args := range map[string]map[string]any{
		"invalid cadence": {
			"repository_id": "tmp-local", "include_freshness": true, "expected_cadence": "soon",
		},
		"too small cadence": {
			"repository_id": "tmp-local", "include_freshness": true, "expected_cadence": "1ns",
		},
		"invalid group": {
			"repository_id": "tmp-local", "include_freshness": true, "freshness_group_by": "path",
		},
	} {
		payload := callE2EToolError(t, ctx, session, artifact, "list_snapshots", args)
		if payload["code"] != "invalid_argument" {
			t.Fatalf("%s error = %v", name, payload)
		}
	}
	artifact.check("freshness inputs are bounded and validated")

	// Partial path visibility is explicit and does not add summaries.
	partialConfig := filepath.Join(fixture.Root, "freshness-partial.yaml")
	if err := fixture.WriteConfigForPaths(partialConfig, resticBinary, []string{filepath.Join(fixture.DataDir1, "subdir")}); err != nil {
		t.Fatal(err)
	}
	partialSession := connectFixtureServer(t, ctx, binary, partialConfig)
	partial := callFreshness(t, ctx, partialSession, artifact, map[string]any{
		"repository_id":     "tmp-local",
		"limit":             200,
		"include_freshness": true,
	})
	if partial.Freshness == nil || !partial.Freshness.PartialPathVisibility {
		t.Fatalf("partial visibility freshness = %+v", partial.Freshness)
	}
	if len(partial.Snapshots) != 1 || partial.Snapshots[0].Summary != nil {
		t.Fatalf("partial snapshot exposed a whole-snapshot summary: %+v", partial.Snapshots)
	}
	artifact.check("partial path visibility and summary suppression stay explicit")

	// No visible snapshots is not a claim that no backups exist.
	emptyConfig := filepath.Join(fixture.Root, "freshness-empty.yaml")
	if err := fixture.WriteConfigForPaths(emptyConfig, resticBinary, []string{filepath.Join(fixture.Root, "not-backed-up")}); err != nil {
		t.Fatal(err)
	}
	emptySession := connectFixtureServer(t, ctx, binary, emptyConfig)
	empty := callFreshness(t, ctx, emptySession, artifact, map[string]any{
		"repository_id":     "tmp-local",
		"limit":             200,
		"include_freshness": true,
	})
	if empty.Freshness == nil || empty.Freshness.Status != "no_visible_snapshots" ||
		len(empty.Freshness.Groups) != 0 || !strings.Contains(strings.Join(empty.Freshness.Notes, " "), "does not prove") {
		t.Fatalf("empty freshness = %+v", empty.Freshness)
	}
	artifact.check("no visible snapshots stays a visibility statement")

	// An unrestricted policy reports no hidden scope booleans.
	openConfig := filepath.Join(fixture.Root, "freshness-open.yaml")
	if err := fixture.WriteConfigWithPolicy(openConfig, resticBinary, testrepo.ConfigPolicy{
		AllowedPaths: []string{"/"},
	}); err != nil {
		t.Fatal(err)
	}
	openSession := connectFixtureServer(t, ctx, binary, openConfig)
	open := callFreshness(t, ctx, openSession, artifact, map[string]any{
		"repository_id":     "tmp-local",
		"limit":             200,
		"include_freshness": true,
	})
	if open.Freshness == nil || open.Freshness.HiddenScope.Hosts || open.Freshness.HiddenScope.Tags || open.Freshness.HiddenScope.Paths {
		t.Fatalf("unrestricted hidden scope = %+v", open.Freshness)
	}
	artifact.check("hidden-scope flags match effective restrictions")

	// Freshness cannot be requested with a cursor because it describes the
	// first-page selection.
	cursorPage := callSnapshotPage(t, ctx, session, artifact, map[string]any{
		"repository_id": "tmp-local",
		"limit":         1,
	})
	if cursorPage.NextCursor == "" {
		t.Fatal("freshness cursor scenario did not produce a cursor")
	}
	cursorFreshness := callE2EToolError(t, ctx, session, artifact, "list_snapshots", map[string]any{
		"repository_id":     "tmp-local",
		"cursor":            cursorPage.NextCursor,
		"include_freshness": true,
	})
	if cursorFreshness["code"] != "invalid_argument" {
		t.Fatalf("cursor freshness error = %v", cursorFreshness)
	}
	artifact.check("freshness cannot be mixed with a cursor")
}

func callFreshness(
	t *testing.T,
	ctx context.Context,
	session *mcp.ClientSession,
	artifact *e2eArtifact,
	args map[string]any,
) freshnessPage {
	t.Helper()
	envelope := callE2ETool(t, ctx, session, artifact, "list_snapshots", args)
	var page freshnessPage
	unmarshalData(t, envelope, &page)
	return page
}
