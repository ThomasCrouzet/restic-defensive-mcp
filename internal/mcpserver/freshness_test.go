package mcpserver

import (
	"testing"
	"time"

	"github.com/ThomasCrouzet/restic-defensive-mcp/internal/policy"
	"github.com/ThomasCrouzet/restic-defensive-mcp/internal/repositories"
	"github.com/ThomasCrouzet/restic-defensive-mcp/internal/restic"
)

func TestFreshnessCadenceBoundaryAndFutureClamp(t *testing.T) {
	observed := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	entry := &repositories.Entry{
		Hosts: policy.NewHostPolicy(nil),
		Tags:  policy.NewTagPolicy(nil),
		Paths: policy.PathPolicy{},
	}
	atBoundary := restic.Snapshot{
		ID: "aaaaaaaa", Hostname: "host", Time: observed.Add(-time.Hour),
	}
	future := restic.Snapshot{
		ID: "bbbbbbbb", Hostname: "future", Time: observed.Add(time.Hour),
	}
	data := buildFreshnessAt(entry, []restic.Snapshot{atBoundary, future}, nil, freshnessScope{}, freshnessGroupHost, time.Hour, false, observed)
	if data.Status != "within_cadence" {
		t.Fatalf("status = %s, want within_cadence", data.Status)
	}
	for _, group := range data.Groups {
		if group.Overdue == nil {
			t.Fatalf("missing overdue judgment: %+v", group)
		}
		if group.Hostname == "host" && *group.Overdue {
			t.Fatalf("age exactly at cadence must not be overdue: %+v", group)
		}
		if group.Hostname == "future" && (group.AgeSeconds != 0 || *group.Overdue) {
			t.Fatalf("future snapshot age must clamp to zero and not be overdue: %+v", group)
		}
	}
}

func TestFreshnessWithoutCadenceHasNoJudgment(t *testing.T) {
	observed := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	entry := &repositories.Entry{
		Hosts: policy.NewHostPolicy(nil),
		Tags:  policy.NewTagPolicy(nil),
		Paths: policy.PathPolicy{},
	}
	data := buildFreshnessAt(entry, []restic.Snapshot{{
		ID: "aaaaaaaa", Hostname: "host", Time: observed.Add(-48 * time.Hour),
	}}, nil, freshnessScope{}, freshnessGroupHost, 0, false, observed)
	if data.Status != "recency_only" || data.Groups[0].Overdue != nil {
		t.Fatalf("freshness = %+v", data)
	}
}

func TestFreshnessHostTagGroupingAndEmptyVisibility(t *testing.T) {
	observed := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	entry := &repositories.Entry{
		Hosts: policy.NewHostPolicy(nil),
		Tags:  policy.NewTagPolicy(nil),
		Paths: policy.PathPolicy{},
	}
	snapshots := []restic.Snapshot{
		{ID: "aaaaaaaa", Hostname: "host", Tags: []string{"b", "a"}, Time: observed.Add(-time.Hour)},
		{ID: "bbbbbbbb", Hostname: "host", Tags: []string{"c"}, Time: observed.Add(-2 * time.Hour)},
	}
	data := buildFreshnessAt(entry, snapshots, nil, freshnessScope{}, freshnessGroupHostTag, 0, false, observed)
	if len(data.Groups) != 2 || data.Groups[0].Key != "host|a,b" {
		t.Fatalf("groups = %+v", data.Groups)
	}
	empty := buildFreshnessAt(entry, nil, nil, freshnessScope{}, freshnessGroupHost, 0, false, observed)
	if empty.Status != "no_visible_snapshots" || !empty.NoVisibleSnapshots {
		t.Fatalf("empty freshness = %+v", empty)
	}
}

func TestFreshnessPathScopeRestriction(t *testing.T) {
	root := &repositories.Entry{Paths: policy.PathPolicy{Allowed: []string{"/"}}}
	if pathScopeRestricted(root) {
		t.Fatal("root allowlist must not be reported as hidden scope")
	}
	narrow := &repositories.Entry{Paths: policy.PathPolicy{Allowed: []string{"/srv/data"}}}
	if !pathScopeRestricted(narrow) {
		t.Fatal("narrow path allowlist must be reported as hidden scope")
	}
}
