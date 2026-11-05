package mcpserver

import (
	"sort"
	"strings"
	"time"

	apperr "github.com/ThomasCrouzet/restic-defensive-mcp/internal/errors"
	"github.com/ThomasCrouzet/restic-defensive-mcp/internal/repositories"
	"github.com/ThomasCrouzet/restic-defensive-mcp/internal/restic"
)

const (
	freshnessGroupHost    = "host"
	freshnessGroupHostTag = "host_tag"
	minExpectedCadence    = time.Minute
	maxExpectedCadence    = 10 * 365 * 24 * time.Hour
)

func parseExpectedCadence(value string) (time.Duration, error) {
	if value == "" {
		return 0, nil
	}
	cadence, err := time.ParseDuration(value)
	if err != nil || cadence < minExpectedCadence || cadence > maxExpectedCadence {
		return 0, apperr.New(apperr.InvalidArgument, "expected_cadence must be a Go duration between 1m and 87600h")
	}
	return cadence, nil
}

func validateFreshnessGroupBy(value string) (string, error) {
	switch value {
	case "", freshnessGroupHost:
		return freshnessGroupHost, nil
	case freshnessGroupHostTag:
		return freshnessGroupHostTag, nil
	default:
		return "", apperr.New(apperr.InvalidArgument, "freshness_group_by must be host or host_tag")
	}
}

func buildFreshness(
	e *repositories.Entry,
	snapshots []restic.Snapshot,
	partialByID map[string]bool,
	scope freshnessScope,
	groupBy string,
	cadence time.Duration,
	incomplete bool,
) freshnessData {
	return buildFreshnessAt(e, snapshots, partialByID, scope, groupBy, cadence, incomplete, time.Now().UTC())
}

func buildFreshnessAt(
	e *repositories.Entry,
	snapshots []restic.Snapshot,
	partialByID map[string]bool,
	scope freshnessScope,
	groupBy string,
	cadence time.Duration,
	incomplete bool,
	observed time.Time,
) freshnessData {
	data := freshnessData{
		ObservedAt:         observed.Format(time.RFC3339Nano),
		GroupBy:            groupBy,
		Scope:              scope,
		NoVisibleSnapshots: len(snapshots) == 0,
		Incomplete:         incomplete,
		HiddenScope: freshnessHiddenScope{
			Hosts: len(e.Hosts.Allowed) > 0,
			Tags:  len(e.Tags.Allowed) > 0,
			Paths: pathScopeRestricted(e),
		},
		Notes: []string{
			"Freshness uses snapshot creation metadata only; it does not verify restorability, cryptographic integrity, or successful data verification.",
			"Visible scope is filtered by the repository allowlists and the requested window. Hidden counts are intentionally omitted.",
		},
	}
	if cadence > 0 {
		data.ExpectedCadence = cadence.String()
	}

	type groupAccumulator struct {
		group freshnessGroup
	}
	groups := make(map[string]*groupAccumulator)
	overdue := false
	partial := false
	for _, snapshot := range snapshots {
		key, tags := freshnessGroupKey(snapshot, groupBy)
		accumulator, ok := groups[key]
		if !ok {
			accumulator = &groupAccumulator{group: freshnessGroup{
				Key:                   key,
				Hostname:              snapshot.Hostname,
				Tags:                  tags,
				LatestSnapshotID:      snapshot.ID,
				LatestSnapshotTime:    snapshot.Time.UTC().Format(time.RFC3339Nano),
				PartialPathVisibility: partialByID[snapshot.ID],
			}}
			groups[key] = accumulator
		}
		accumulator.group.VisibleSnapshotCount++
		if partialByID[snapshot.ID] {
			partial = true
		}
	}
	for _, accumulator := range groups {
		group := &accumulator.group
		latest, err := time.Parse(time.RFC3339Nano, group.LatestSnapshotTime)
		if err != nil {
			continue
		}
		age := observed.Sub(latest)
		if age < 0 {
			age = 0
		}
		group.AgeSeconds = int64(age / time.Second)
		if cadence > 0 {
			isOverdue := age > cadence
			group.Overdue = &isOverdue
			group.ExpectedCadence = cadence.String()
			if isOverdue {
				overdue = true
			}
		}
		data.Groups = append(data.Groups, *group)
	}
	sort.Slice(data.Groups, func(i, j int) bool { return data.Groups[i].Key < data.Groups[j].Key })
	data.PartialPathVisibility = partial

	switch {
	case len(data.Groups) == 0:
		data.Status = "no_visible_snapshots"
		data.Notes = append(data.Notes, "\"No visible snapshots\" does not prove that no backups exist; hidden or unavailable scope may still exist.")
	case incomplete:
		data.Status = "incomplete"
	case cadence == 0:
		data.Status = "recency_only"
		data.Notes = append(data.Notes, "No expected cadence was supplied; recency is reported without a healthy or unhealthy judgment.")
	case overdue:
		data.Status = "overdue"
	default:
		data.Status = "within_cadence"
	}
	if partial {
		data.Notes = append(data.Notes, "Some visible snapshots expose only an allowlisted path intersection; whole-snapshot backup summaries remain suppressed.")
	}
	return data
}

func pathScopeRestricted(e *repositories.Entry) bool {
	if len(e.Paths.Allowed) == 0 {
		return false
	}
	for _, allowed := range e.Paths.Allowed {
		if allowed == "/" {
			return false
		}
	}
	return true
}

func freshnessGroupKey(snapshot restic.Snapshot, groupBy string) (string, []string) {
	tags := append([]string(nil), snapshot.Tags...)
	sort.Strings(tags)
	if groupBy == freshnessGroupHostTag {
		return snapshot.Hostname + "|" + strings.Join(tags, ","), tags
	}
	return snapshot.Hostname, tags
}
