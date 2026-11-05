package mcpserver

import (
	"context"
	"strings"

	apperr "github.com/ThomasCrouzet/restic-defensive-mcp/internal/errors"
	"github.com/ThomasCrouzet/restic-defensive-mcp/internal/policy"
	"github.com/ThomasCrouzet/restic-defensive-mcp/internal/redaction"
	"github.com/ThomasCrouzet/restic-defensive-mcp/internal/repositories"
	"github.com/ThomasCrouzet/restic-defensive-mcp/internal/restic"
)

func policyVisible(e *repositories.Entry, s restic.Snapshot) bool {
	return policy.SnapshotVisible(s.Hostname, s.Tags, s.Paths, e.Hosts, e.Tags, e.Paths)
}

func publicSnapshot(e *repositories.Entry, s restic.Snapshot) restic.Snapshot {
	if !statsSnapshotAllowed(e, s) {
		// Backup summaries describe the entire snapshot. Suppress them when the
		// path policy exposes only an intersection of broader snapshot roots.
		s.Summary = nil
	}
	s.Paths = e.Paths.FilterPaths(s.Paths)
	for i := range s.Paths {
		s.Paths[i] = redaction.SanitizeAndRedact(s.Paths[i])
	}
	s.Tags = e.Tags.FilterTags(s.Tags)
	for i := range s.Tags {
		s.Tags[i] = redaction.SanitizeAndRedact(s.Tags[i])
	}
	s.Hostname = redaction.SanitizeAndRedact(s.Hostname)
	s.ProgramVersion = redaction.SanitizeAndRedact(s.ProgramVersion)
	s.Username = ""
	s.Parent = ""
	s.Tree = ""
	return s
}

func visibleSnapshots(
	ctx context.Context,
	deps Deps,
	e *repositories.Entry,
	repoID string,
	selector string,
	host string,
	tags []string,
	requestPath string,
	window timeWindow,
) ([]restic.Snapshot, error) {
	opts := restic.SnapshotsOpts{}
	if host != "" {
		if err := e.Hosts.Check(host); err != nil {
			return nil, err
		}
		opts.Hosts = []string{host}
	} else if len(e.Hosts.Allowed) == 1 {
		opts.Hosts = []string{e.Hosts.Allowed[0]}
	}
	if len(tags) > 0 {
		if err := e.Tags.CheckEach(tags); err != nil {
			return nil, err
		}
		opts.Tags = append([]string(nil), tags...)
	}
	if requestPath != "" {
		cleaned, err := e.Paths.Check(requestPath)
		if err != nil {
			return nil, err
		}
		requestPath = cleaned
	}
	if selector != "" && selector != "latest" && len(selector) == 64 {
		opts.SnapshotIDs = []string{selector}
	}

	argv, err := restic.BuildSnapshotsArgv(opts)
	if err != nil {
		return nil, apperr.New(apperr.InvalidArgument, err.Error())
	}
	res, err := deps.Registry.Run(ctx, repoID, argv, false)
	if err != nil {
		return nil, err
	}
	snapshots, err := restic.ParseSnapshots(res.Stdout)
	if err != nil {
		return nil, err
	}

	visible := make([]restic.Snapshot, 0, len(snapshots))
	for _, snapshot := range snapshots {
		if !policyVisible(e, snapshot) {
			continue
		}
		if !window.contains(snapshot.Time) || !matchesSnapshotFilters(snapshot, host, tags, requestPath) {
			continue
		}
		if selector != "" && selector != "latest" && !snapshotIDMatches(snapshot.ID, selector) {
			continue
		}
		visible = append(visible, snapshot)
	}
	return visible, nil
}

func matchesSnapshotFilters(snapshot restic.Snapshot, host string, tags []string, requestPath string) bool {
	if host != "" && snapshot.Hostname != host {
		return false
	}
	for _, requestedTag := range tags {
		found := false
		for _, snapshotTag := range snapshot.Tags {
			if snapshotTag == requestedTag {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	if requestPath == "" {
		return true
	}
	// A requested scope and a snapshot root overlap when either contains the
	// other. Both directions are required: a search rooted at an allowlisted
	// parent must select snapshots of its child directories, while a search in
	// a child scope must still select a snapshot whose root is that child.
	for _, snapshotPath := range snapshot.Paths {
		cleaned, err := policy.CleanPath(snapshotPath)
		if err == nil && pathsOverlap(requestPath, cleaned) {
			return true
		}
	}
	return false
}

func pathsOverlap(a, b string) bool {
	return pathIsUnderAllow(a, b) || pathIsUnderAllow(b, a)
}

func resolveVisibleSnapshot(
	ctx context.Context,
	deps Deps,
	e *repositories.Entry,
	repoID string,
	selector string,
	host string,
	tags []string,
	requestPath string,
	window timeWindow,
) (restic.Snapshot, error) {
	snapshots, err := visibleSnapshots(ctx, deps, e, repoID, selector, host, tags, requestPath, window)
	if err != nil {
		return restic.Snapshot{}, err
	}
	if len(snapshots) == 0 {
		return restic.Snapshot{}, apperr.New(apperr.SnapshotNotFound, "snapshot not found")
	}
	if selector != "latest" && len(snapshots) > 1 {
		return restic.Snapshot{}, apperr.New(apperr.AmbiguousSnapshot, "snapshot id prefix matches multiple snapshots")
	}
	return snapshots[0], nil
}

func snapshotIDMatches(snapshotID, selector string) bool {
	if len(selector) > len(snapshotID) {
		return false
	}
	return strings.EqualFold(snapshotID[:len(selector)], selector)
}

func validateLatestScope(e *repositories.Entry, host string, tags []string, requestPath string) error {
	if host == "" && len(tags) == 0 && requestPath == "" && len(e.Hosts.Allowed) != 1 {
		return apperr.New(apperr.InvalidArgument, "latest requires a host, tag, or path filter")
	}
	return nil
}

func snapshotsByIDs(
	ctx context.Context,
	deps Deps,
	e *repositories.Entry,
	repoID string,
	ids []string,
) ([]restic.Snapshot, error) {
	if len(ids) == 0 {
		return nil, apperr.New(apperr.StaleSelection, "snapshot selection is empty or stale")
	}
	argv, err := restic.BuildSnapshotsArgv(restic.SnapshotsOpts{SnapshotIDs: ids})
	if err != nil {
		return nil, apperr.New(apperr.InternalError, "invalid stored snapshot selection").WithCause(err)
	}
	res, err := deps.Registry.Run(ctx, repoID, argv, false)
	if err != nil {
		return nil, err
	}
	snapshots, err := restic.ParseSnapshots(res.Stdout)
	if err != nil {
		return nil, err
	}
	byID := make(map[string]restic.Snapshot, len(snapshots))
	for _, snapshot := range snapshots {
		byID[snapshot.ID] = snapshot
	}
	out := make([]restic.Snapshot, 0, len(ids))
	for _, id := range ids {
		snapshot, ok := byID[id]
		if !ok || !policyVisible(e, snapshot) {
			return nil, apperr.New(apperr.StaleSelection, "snapshot selection changed; restart pagination")
		}
		out = append(out, snapshot)
	}
	return out, nil
}

func pathIsUnderAllow(child, parent string) bool {
	if child == parent {
		return true
	}
	if parent == "/" {
		return len(child) > 0 && child[0] == '/'
	}
	return len(child) > len(parent) && child[:len(parent)] == parent && child[len(parent)] == '/'
}

func statsSnapshotAllowed(e *repositories.Entry, snapshot restic.Snapshot) bool {
	if len(e.Paths.Allowed) == 0 {
		return true
	}
	if len(snapshot.Paths) == 0 {
		return false
	}
	for _, rawPath := range snapshot.Paths {
		cleaned, err := policy.CleanPath(rawPath)
		if err != nil || !e.Paths.IsAllowed(cleaned) {
			return false
		}
	}
	return true
}
