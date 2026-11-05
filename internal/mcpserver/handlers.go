package mcpserver

import (
	"context"
	"sort"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	apperr "github.com/ThomasCrouzet/restic-defensive-mcp/internal/errors"
	"github.com/ThomasCrouzet/restic-defensive-mcp/internal/policy"
	"github.com/ThomasCrouzet/restic-defensive-mcp/internal/redaction"
	"github.com/ThomasCrouzet/restic-defensive-mcp/internal/repositories"
	"github.com/ThomasCrouzet/restic-defensive-mcp/internal/restic"
)

func makeCapabilities(deps Deps) func(context.Context, *mcp.CallToolRequest, emptyIn) (*mcp.CallToolResult, any, error) {
	return func(ctx context.Context, _ *mcp.CallToolRequest, _ emptyIn) (*mcp.CallToolResult, any, error) {
		lim := deps.Registry.Limits()
		data := map[string]any{
			"server_version":            deps.ServerVersion,
			"restic_version":            deps.ResticVersion,
			"repository_ids":            deps.Registry.IDs(),
			"supported_backends":        []string{"local", "s3", "b2", "rest"},
			"unsupported_backends":      []string{"sftp", "rclone", "azure", "gs", "swift", "other"},
			"allowed_restic_commands":   restic.AllowedCommands(),
			"forbidden_restic_commands": restic.ForbiddenCommands(),
			"tools":                     ToolNames(),
			"cost_classes":              []string{"light", "moderate", "expensive"},
			"limits": map[string]any{
				"command_timeout_sec":           int(lim.CommandTimeout.Seconds()),
				"expensive_command_timeout_sec": int(lim.ExpensiveCommandTimeout.Seconds()),
				"max_snapshots":                 lim.MaxSnapshots,
				"max_nodes":                     lim.MaxNodes,
				"max_find_results":              lim.MaxFindResults,
				"max_output_bytes":              lim.MaxOutputBytes,
				"max_concurrent_per_repo":       lim.MaxConcurrentPerRepo,
				"max_concurrent_global":         lim.MaxConcurrentGlobal,
			},
			"warnings": []string{
				"This server is structurally read-only: no backup, restore, forget, prune, unlock, or repair tools.",
				"Restic may still create or update a local cache directory and may take repository locks during inspection.",
				"repository_stats is not a full integrity check; it does not run restic check --read-data.",
				"File names and paths from snapshots are sensitive; results are bounded and sanitized.",
				"Callers cannot supply repository URLs, backends, or secret paths.",
			},
		}
		return okResult(restic.CostLight, data, false)
	}
}

func makeListRepositories(deps Deps) func(context.Context, *mcp.CallToolRequest, listReposIn) (*mcp.CallToolResult, any, error) {
	return func(ctx context.Context, _ *mcp.CallToolRequest, _ listReposIn) (*mcp.CallToolResult, any, error) {
		return okResult(restic.CostLight, deps.Registry.List(), false)
	}
}

func makeListSnapshots(deps Deps) func(context.Context, *mcp.CallToolRequest, listSnapshotsIn) (*mcp.CallToolResult, any, error) {
	const tool = "list_snapshots"
	return func(ctx context.Context, _ *mcp.CallToolRequest, in listSnapshotsIn) (*mcp.CallToolResult, any, error) {
		e, err := requireRepo(deps, in.RepositoryID)
		if err != nil {
			return failTool(deps, tool, in.RepositoryID, err)
		}
		lim := deps.Registry.Limits()
		limit := in.Limit
		if limit < 0 {
			return failTool(deps, tool, in.RepositoryID, apperr.New(apperr.InvalidArgument, "limit must be >= 0"))
		}
		if limit == 0 || limit > lim.MaxSnapshots {
			limit = lim.MaxSnapshots
		}
		if in.Offset < 0 {
			return failTool(deps, tool, in.RepositoryID, apperr.New(apperr.InvalidArgument, "offset must be >= 0"))
		}
		window, err := parseTimeWindow(in.After, in.Before)
		if err != nil {
			return failTool(deps, tool, in.RepositoryID, err)
		}
		groupBy, err := validateFreshnessGroupBy(in.FreshnessGroupBy)
		if err != nil {
			return failTool(deps, tool, in.RepositoryID, err)
		}
		cadence, err := parseExpectedCadence(in.ExpectedCadence)
		if err != nil {
			return failTool(deps, tool, in.RepositoryID, err)
		}
		if in.Cursor != "" && in.IncludeFreshness {
			return failTool(deps, tool, in.RepositoryID, apperr.New(
				apperr.InvalidArgument,
				"include_freshness is computed from the first-page selection and cannot be combined with a cursor",
			))
		}

		if in.Cursor != "" {
			if in.Offset != 0 || in.Host != "" || len(in.Tags) > 0 || in.Path != "" ||
				in.After != "" || in.Before != "" {
				return failTool(deps, tool, in.RepositoryID, apperr.New(
					apperr.InvalidArgument,
					"cursor carries the original filters; pass only repository_id, cursor, and optionally limit",
				))
			}
			state, err := deps.cursors.take(in.Cursor, tool, in.RepositoryID)
			if err != nil {
				return failTool(deps, tool, in.RepositoryID, err)
			}
			filtered, more, err := listSnapshotsForCursor(ctx, deps, e, in.RepositoryID, state, limit)
			if err != nil {
				return failTool(deps, tool, in.RepositoryID, err)
			}
			total := state.Total
			page := filtered
			start := state.Offset
			end := start + len(page)
			truncated := more
			out := map[string]any{
				"snapshots": page,
				"count":     len(page),
				"total":     total,
				"offset":    start,
				"limit":     limit,
				"window":    windowOutput(state.After, state.Before),
			}
			if truncated {
				next, cursorErr := deps.cursors.issue(cursorState{
					Tool:       tool,
					Repository: in.RepositoryID,
					Host:       state.Host,
					Tags:       state.Tags,
					Path:       state.Path,
					After:      state.After,
					Before:     state.Before,
					LastTime:   page[len(page)-1].Time,
					LastID:     page[len(page)-1].ID,
					Offset:     end,
					Total:      total,
				})
				if cursorErr != nil {
					return errTool(cursorErr), nil, nil
				}
				out["next_cursor"] = next
			}
			return okResult(restic.CostLight, out, truncated)
		}

		opts := restic.SnapshotsOpts{}
		requestedPath := ""
		normalizedAfter, normalizedBefore := window.normalized()
		state := cursorState{
			Tool:       tool,
			Repository: in.RepositoryID,
			After:      normalizedAfter,
			Before:     normalizedBefore,
		}
		if in.Host != "" {
			if err := e.Hosts.Check(in.Host); err != nil {
				return failTool(deps, tool, in.RepositoryID, err)
			}
			opts.Hosts = []string{in.Host}
			state.Host = in.Host
		} else if len(e.Hosts.Allowed) == 1 {
			// When exactly one host is allowlisted, apply it to avoid leaking others.
			opts.Hosts = []string{e.Hosts.Allowed[0]}
			state.Host = e.Hosts.Allowed[0]
		}
		if len(in.Tags) > 0 {
			if err := e.Tags.CheckEach(in.Tags); err != nil {
				return failTool(deps, tool, in.RepositoryID, err)
			}
			opts.Tags = in.Tags
			state.Tags = append([]string(nil), in.Tags...)
		}
		if in.Path != "" {
			p, err := e.Paths.Check(in.Path)
			if err != nil {
				return failTool(deps, tool, in.RepositoryID, err)
			}
			requestedPath = p
			state.Path = p
		}

		argv, err := restic.BuildSnapshotsArgv(opts)
		if err != nil {
			return failTool(deps, tool, in.RepositoryID, apperr.New(apperr.InvalidArgument, err.Error()))
		}
		res, err := deps.Registry.Run(ctx, in.RepositoryID, argv, false)
		if err != nil {
			return errTool(err), nil, nil
		}
		snaps, err := restic.ParseSnapshots(res.Stdout)
		if err != nil {
			return errTool(err), nil, nil
		}
		// Server-side visibility filter (single policy implementation).
		filtered := make([]restic.Snapshot, 0, len(snaps))
		partialByID := make(map[string]bool, len(snaps))
		for _, s := range snaps {
			if !window.contains(s.Time) || !policyVisible(e, s) || !matchesSnapshotFilters(s, in.Host, in.Tags, requestedPath) {
				continue
			}
			partialByID[s.ID] = !statsSnapshotAllowed(e, s)
			filtered = append(filtered, publicSnapshot(e, s))
		}
		total := len(filtered)
		start := in.Offset
		if start > total {
			start = total
		}
		end := start + limit
		if end > total {
			end = total
		}
		page := filtered[start:end]
		// truncated means more pages exist beyond this offset/limit window.
		truncated := end < total
		out := map[string]any{
			"snapshots": page,
			"count":     len(page),
			"total":     total,
			"offset":    start,
			"limit":     limit,
			"window":    windowOutput(state.After, state.Before),
		}
		if in.IncludeFreshness {
			scope := freshnessScope{
				Hosts:  append([]string(nil), e.Hosts.Allowed...),
				Tags:   append([]string(nil), e.Tags.Allowed...),
				Paths:  append([]string(nil), e.Paths.Allowed...),
				Window: windowOutput(state.After, state.Before),
			}
			if state.Host != "" {
				scope.Hosts = []string{state.Host}
			}
			if len(state.Tags) > 0 {
				scope.Tags = append([]string(nil), state.Tags...)
			}
			if state.Path != "" {
				scope.Paths = []string{state.Path}
			}
			freshness := buildFreshness(e, filtered, partialByID, scope, groupBy, cadence, false)
			out["freshness"] = freshness
		}
		if truncated && len(page) > 0 {
			next, cursorErr := deps.cursors.issue(cursorState{
				Tool:       tool,
				Repository: in.RepositoryID,
				Host:       state.Host,
				Tags:       state.Tags,
				Path:       state.Path,
				After:      state.After,
				Before:     state.Before,
				LastTime:   page[len(page)-1].Time,
				LastID:     page[len(page)-1].ID,
				Offset:     end,
				Total:      total,
			})
			if cursorErr != nil {
				return errTool(cursorErr), nil, nil
			}
			out["next_cursor"] = next
		}
		return okResult(restic.CostLight, out, truncated)
	}
}

func listSnapshotsForCursor(
	ctx context.Context,
	deps Deps,
	e *repositories.Entry,
	repoID string,
	state cursorState,
	limit int,
) ([]restic.Snapshot, bool, error) {
	window, err := parseTimeWindow(state.After, state.Before)
	if err != nil {
		return nil, false, apperr.New(apperr.CursorInvalid, "cursor carries an invalid time window")
	}
	opts := restic.SnapshotsOpts{}
	if state.Host != "" {
		if err := e.Hosts.Check(state.Host); err != nil {
			return nil, false, err
		}
		opts.Hosts = []string{state.Host}
	} else if len(e.Hosts.Allowed) == 1 {
		opts.Hosts = []string{e.Hosts.Allowed[0]}
	}
	if len(state.Tags) > 0 {
		if err := e.Tags.CheckEach(state.Tags); err != nil {
			return nil, false, err
		}
		opts.Tags = state.Tags
	}
	argv, err := restic.BuildSnapshotsArgv(opts)
	if err != nil {
		return nil, false, apperr.New(apperr.InvalidArgument, err.Error())
	}
	res, err := deps.Registry.Run(ctx, repoID, argv, false)
	if err != nil {
		return nil, false, err
	}
	snaps, err := restic.ParseSnapshots(res.Stdout)
	if err != nil {
		return nil, false, err
	}
	out := make([]restic.Snapshot, 0, limit+1)
	more := false
	for _, snapshot := range snaps {
		if !window.contains(snapshot.Time) ||
			!policyVisible(e, snapshot) ||
			!matchesSnapshotFilters(snapshot, state.Host, state.Tags, state.Path) {
			continue
		}
		if !afterSnapshotMarker(snapshot, state.LastTime, state.LastID) {
			continue
		}
		if len(out) >= limit {
			more = true
			break
		}
		out = append(out, publicSnapshot(e, snapshot))
	}
	return out, more, nil
}

func afterSnapshotMarker(snapshot restic.Snapshot, lastTime time.Time, lastID string) bool {
	if lastTime.IsZero() {
		return true
	}
	if snapshot.Time.Before(lastTime) {
		return true
	}
	return snapshot.Time.Equal(lastTime) && snapshot.ID > lastID
}

func makeGetSnapshot(deps Deps) func(context.Context, *mcp.CallToolRequest, getSnapshotIn) (*mcp.CallToolResult, any, error) {
	const tool = "get_snapshot"
	return func(ctx context.Context, _ *mcp.CallToolRequest, in getSnapshotIn) (*mcp.CallToolResult, any, error) {
		e, err := requireRepo(deps, in.RepositoryID)
		if err != nil {
			return failTool(deps, tool, in.RepositoryID, err)
		}
		if in.SnapshotID == "" {
			return failTool(deps, tool, in.RepositoryID, apperr.New(apperr.InvalidArgument, "snapshot_id is required"))
		}
		if !restic.ValidSnapshotID(in.SnapshotID) {
			return failTool(deps, tool, in.RepositoryID, apperr.New(apperr.InvalidArgument, "invalid snapshot_id"))
		}
		window, err := parseTimeWindow(in.After, in.Before)
		if err != nil {
			return failTool(deps, tool, in.RepositoryID, err)
		}
		if in.SnapshotID == "latest" {
			if err := validateLatestScope(e, in.Host, in.Tags, in.Path); err != nil {
				return failTool(deps, tool, in.RepositoryID, err)
			}
		}
		snapshot, err := resolveVisibleSnapshot(
			ctx, deps, e, in.RepositoryID, in.SnapshotID, in.Host, in.Tags, in.Path, window,
		)
		if err != nil {
			return failTool(deps, tool, in.RepositoryID, err)
		}
		return okResult(restic.CostLight, publicSnapshot(e, snapshot), false)
	}
}

func makeBrowseSnapshot(deps Deps) func(context.Context, *mcp.CallToolRequest, browseSnapshotIn) (*mcp.CallToolResult, any, error) {
	const tool = "browse_snapshot"
	return func(ctx context.Context, _ *mcp.CallToolRequest, in browseSnapshotIn) (*mcp.CallToolResult, any, error) {
		e, err := requireRepo(deps, in.RepositoryID)
		if err != nil {
			return failTool(deps, tool, in.RepositoryID, err)
		}
		lim := deps.Registry.Limits()
		limit := in.Limit
		if limit < 0 {
			return failTool(deps, tool, in.RepositoryID, apperr.New(apperr.InvalidArgument, "limit must be >= 0"))
		}
		if limit == 0 || limit > lim.MaxNodes {
			limit = lim.MaxNodes
		}
		window, err := parseTimeWindow(in.After, in.Before)
		if err != nil {
			return failTool(deps, tool, in.RepositoryID, err)
		}

		var state cursorState
		if in.Cursor != "" {
			if in.SnapshotID != "" || in.Path != "" || in.Host != "" || in.Recursive ||
				in.After != "" || in.Before != "" {
				return failTool(deps, tool, in.RepositoryID, apperr.New(
					apperr.InvalidArgument,
					"cursor carries the original scope; pass only repository_id, cursor, and optionally limit",
				))
			}
			state, err = deps.cursors.take(in.Cursor, tool, in.RepositoryID)
			if err != nil {
				return failTool(deps, tool, in.RepositoryID, err)
			}
		} else {
			if !restic.ValidSnapshotID(in.SnapshotID) {
				return failTool(deps, tool, in.RepositoryID, apperr.New(apperr.InvalidArgument, "invalid snapshot_id"))
			}
			path, pathErr := e.Paths.Check(in.Path)
			if pathErr != nil {
				return failTool(deps, tool, in.RepositoryID, pathErr)
			}
			if in.SnapshotID == "latest" {
				if err := validateLatestScope(e, in.Host, nil, path); err != nil {
					return failTool(deps, tool, in.RepositoryID, err)
				}
			}
			snapshot, snapshotErr := resolveVisibleSnapshot(
				ctx, deps, e, in.RepositoryID, in.SnapshotID, in.Host, nil, path, window,
			)
			if snapshotErr != nil {
				return failTool(deps, tool, in.RepositoryID, snapshotErr)
			}
			state = cursorState{
				Tool:        tool,
				Repository:  in.RepositoryID,
				Path:        path,
				Recursive:   in.Recursive,
				SnapshotIDs: []string{snapshot.ID},
			}
		}

		// Authorization is rechecked on every page from the concrete snapshot
		// selection, never from a cursor-provided path or host.
		snapshots, err := snapshotsByIDs(ctx, deps, e, in.RepositoryID, state.SnapshotIDs)
		if err != nil {
			return failTool(deps, tool, in.RepositoryID, err)
		}
		snapshot := snapshots[0]
		if _, err := e.Paths.Check(state.Path); err != nil {
			return failTool(deps, tool, in.RepositoryID, err)
		}

		nodes, parseTruncated, err := browseVisibleNodes(
			ctx, deps, e, in.RepositoryID, snapshot.ID, state.Path, state.Recursive,
		)
		if err != nil {
			return errTool(err), nil, nil
		}
		total := len(nodes)
		start := state.Offset
		if start > total {
			start = total
		}
		end := start + limit
		if end > total {
			end = total
		}
		page := nodes[start:end]
		truncated := end < total || parseTruncated
		out := map[string]any{
			"path":       state.Path,
			"nodes":      page,
			"count":      len(page),
			"total":      total,
			"recursive":  state.Recursive,
			"incomplete": parseTruncated,
		}
		if end < total {
			next, cursorErr := deps.cursors.issue(cursorState{
				Tool:        tool,
				Repository:  in.RepositoryID,
				Path:        state.Path,
				Recursive:   state.Recursive,
				SnapshotIDs: state.SnapshotIDs,
				Offset:      end,
			})
			if cursorErr != nil {
				return errTool(cursorErr), nil, nil
			}
			out["next_cursor"] = next
		}
		return okResult(restic.CostModerate, out, truncated)
	}
}

func browseVisibleNodes(
	ctx context.Context,
	deps Deps,
	e *repositories.Entry,
	repoID, snapshotID, path string,
	recursive bool,
) ([]restic.Node, bool, error) {
	argv, err := restic.BuildLSArgv(restic.LSOpts{
		SnapshotID: snapshotID,
		Dirs:       []string{path},
		Recursive:  recursive,
	})
	if err != nil {
		return nil, false, apperr.New(apperr.InvalidArgument, err.Error())
	}
	res, err := deps.Registry.Run(ctx, repoID, argv, true)
	if err != nil {
		return nil, false, err
	}
	lim := deps.Registry.Limits()
	nodes, parseTruncated, err := restic.ParseLS(res.Stdout, lim.MaxOutputBytes)
	if err != nil {
		return nil, false, err
	}
	filtered := make([]restic.Node, 0, len(nodes))
	for _, n := range nodes {
		cleaned, cleanErr := policy.CleanPath(n.Path)
		if cleanErr != nil || !e.Paths.IsAllowed(cleaned) || !pathIsUnderAllow(cleaned, path) {
			continue
		}
		n.Path = redaction.SanitizeAndRedact(cleaned)
		n.Name = redaction.SanitizeAndRedact(n.Name)
		n.Type = redaction.SanitizeAndRedact(n.Type)
		n.Mode = redaction.SanitizeAndRedact(n.Mode)
		filtered = append(filtered, n)
	}
	return filtered, parseTruncated, nil
}

func makeFindFiles(deps Deps) func(context.Context, *mcp.CallToolRequest, findFilesIn) (*mcp.CallToolResult, any, error) {
	const tool = "find_files"
	return func(ctx context.Context, _ *mcp.CallToolRequest, in findFilesIn) (*mcp.CallToolResult, any, error) {
		e, err := requireRepo(deps, in.RepositoryID)
		if err != nil {
			return failTool(deps, tool, in.RepositoryID, err)
		}
		lim := deps.Registry.Limits()
		limit := in.Limit
		if limit < 0 {
			return failTool(deps, tool, in.RepositoryID, apperr.New(apperr.InvalidArgument, "limit must be >= 0"))
		}
		if limit == 0 || limit > lim.MaxFindResults {
			limit = lim.MaxFindResults
		}

		var state cursorState
		var snapshots []restic.Snapshot
		if in.Cursor != "" {
			if in.Pattern != "" || in.Path != "" || in.SnapshotID != "" || in.Host != "" ||
				len(in.Tags) > 0 || in.IgnoreCase || in.After != "" || in.Before != "" {
				return failTool(deps, tool, in.RepositoryID, apperr.New(
					apperr.InvalidArgument,
					"cursor carries the original filters; pass only repository_id, cursor, and optionally limit",
				))
			}
			state, err = deps.cursors.take(in.Cursor, tool, in.RepositoryID)
			if err != nil {
				return failTool(deps, tool, in.RepositoryID, err)
			}
			snapshots, err = snapshotsByIDs(ctx, deps, e, in.RepositoryID, state.SnapshotIDs)
			if err != nil {
				return failTool(deps, tool, in.RepositoryID, err)
			}
		} else {
			if in.Pattern == "" {
				return failTool(deps, tool, in.RepositoryID, apperr.New(apperr.InvalidArgument, "pattern is required"))
			}
			window, windowErr := parseTimeWindow(in.After, in.Before)
			if windowErr != nil {
				return failTool(deps, tool, in.RepositoryID, windowErr)
			}
			normalizedAfter, normalizedBefore := window.normalized()
			scopePath := in.Path
			if scopePath == "" {
				switch len(e.Paths.Allowed) {
				case 1:
					scopePath = e.Paths.Allowed[0]
				case 0:
					// The repository has no path restriction.
				default:
					return failTool(
						deps,
						tool,
						in.RepositoryID,
						apperr.New(apperr.InvalidArgument, "path is required when multiple allowed_paths are configured"),
					)
				}
			}
			if scopePath != "" {
				p, pathErr := e.Paths.Check(scopePath)
				if pathErr != nil {
					return failTool(deps, tool, in.RepositoryID, pathErr)
				}
				scopePath = p
			}
			state = cursorState{
				Tool:       tool,
				Repository: in.RepositoryID,
				Path:       scopePath,
				After:      normalizedAfter,
				Before:     normalizedBefore,
				Pattern:    in.Pattern,
				IgnoreCase: in.IgnoreCase,
			}
			// Reject unsafe patterns before probing repository metadata.
			if _, err := restic.BuildFindArgv(restic.FindOpts{Pattern: in.Pattern, IgnoreCase: in.IgnoreCase}); err != nil {
				return failTool(deps, tool, in.RepositoryID, apperr.New(apperr.InvalidArgument, err.Error()))
			}

			if in.SnapshotID != "" {
				if !restic.ValidSnapshotID(in.SnapshotID) || in.SnapshotID == "latest" {
					return failTool(deps, tool, in.RepositoryID, apperr.New(apperr.InvalidArgument, "snapshot_id must be a concrete id"))
				}
				snapshot, snapshotErr := resolveVisibleSnapshot(
					ctx, deps, e, in.RepositoryID, in.SnapshotID, in.Host, in.Tags, scopePath, window,
				)
				if snapshotErr != nil {
					return failTool(deps, tool, in.RepositoryID, snapshotErr)
				}
				snapshots = []restic.Snapshot{snapshot}
			} else {
				snapshots, err = visibleSnapshots(
					ctx, deps, e, in.RepositoryID, "", in.Host, in.Tags, scopePath, window,
				)
				if err != nil {
					return failTool(deps, tool, in.RepositoryID, err)
				}
				if len(snapshots) > lim.MaxSnapshots {
					return failTool(
						deps,
						tool,
						in.RepositoryID,
						apperr.New(apperr.InvalidArgument, "query matches too many snapshots; add a host, tag, path, after, before, or snapshot_id filter"),
					)
				}
			}
			state.SnapshotIDs = make([]string, 0, len(snapshots))
			for _, snapshot := range snapshots {
				state.SnapshotIDs = append(state.SnapshotIDs, snapshot.ID)
			}
		}

		if len(snapshots) == 0 {
			cost := restic.CostExpensive
			return okResult(cost, map[string]any{"groups": []restic.FindGroup{}, "count": 0, "total": 0}, false)
		}

		argv, err := restic.BuildFindArgv(restic.FindOpts{
			Pattern:     state.Pattern,
			IgnoreCase:  state.IgnoreCase,
			SnapshotIDs: state.SnapshotIDs,
		})
		if err != nil {
			return failTool(deps, tool, in.RepositoryID, apperr.New(apperr.InvalidArgument, err.Error()))
		}
		res, err := deps.Registry.Run(ctx, in.RepositoryID, argv, true)
		if err != nil {
			return errTool(err), nil, nil
		}
		groups, _, parseTruncated, err := restic.ParseFind(res.Stdout, lim.MaxOutputBytes)
		if err != nil {
			return errTool(err), nil, nil
		}
		page, visibleCount, totalVisible, err := paginateFindGroups(groups, snapshots, e, state.Path, state.Offset, limit)
		if err != nil {
			return errTool(err), nil, nil
		}
		out := map[string]any{
			"groups":     page,
			"count":      visibleCount,
			"incomplete": parseTruncated,
			"window":     windowOutput(state.After, state.Before),
		}
		if !parseTruncated {
			out["total"] = totalVisible
		}
		truncated := parseTruncated || state.Offset+visibleCount < totalVisible
		if !parseTruncated && state.Offset+visibleCount < totalVisible {
			next, cursorErr := deps.cursors.issue(cursorState{
				Tool:        tool,
				Repository:  in.RepositoryID,
				Path:        state.Path,
				Pattern:     state.Pattern,
				IgnoreCase:  state.IgnoreCase,
				SnapshotIDs: state.SnapshotIDs,
				Offset:      state.Offset + visibleCount,
			})
			if cursorErr != nil {
				return errTool(cursorErr), nil, nil
			}
			out["next_cursor"] = next
		}
		cost := restic.CostExpensive
		if len(state.SnapshotIDs) == 1 {
			cost = restic.CostModerate
		}
		return okResult(cost, out, truncated)
	}
}

func paginateFindGroups(
	groups []restic.FindGroup,
	snapshots []restic.Snapshot,
	e *repositories.Entry,
	scopePath string,
	offset, limit int,
) ([]restic.FindGroup, int, int, error) {
	type orderedGroup struct {
		snapshot string
		matches  []restic.FindMatch
	}
	ordered := make([]orderedGroup, 0, len(groups))
	totalVisible := 0
	for _, g := range groups {
		if !findGroupAuthorized(g.Snapshot, snapshots) {
			continue
		}
		matches := make([]restic.FindMatch, 0, len(g.Matches))
		for _, match := range g.Matches {
			cleaned, cleanErr := policy.CleanPath(match.Path)
			if cleanErr != nil || !e.Paths.IsAllowed(cleaned) {
				continue
			}
			if scopePath != "" && !pathIsUnderAllow(cleaned, scopePath) {
				continue
			}
			match.Path = redaction.SanitizeAndRedact(cleaned)
			match.Name = redaction.SanitizeAndRedact(match.Name)
			match.Type = redaction.SanitizeAndRedact(match.Type)
			matches = append(matches, match)
		}
		if len(matches) == 0 {
			continue
		}
		sort.SliceStable(matches, func(i, j int) bool { return matches[i].Path < matches[j].Path })
		ordered = append(ordered, orderedGroup{snapshot: g.Snapshot, matches: matches})
		totalVisible += len(matches)
	}
	sort.SliceStable(ordered, func(i, j int) bool { return ordered[i].snapshot < ordered[j].snapshot })
	if offset > totalVisible {
		offset = totalVisible
	}
	remaining := totalVisible - offset
	pageLimit := limit
	if pageLimit > remaining {
		pageLimit = remaining
	}
	page := make([]restic.FindGroup, 0, len(ordered))
	skipped := 0
	collected := 0
	for _, group := range ordered {
		if collected >= pageLimit {
			break
		}
		pageMatches := make([]restic.FindMatch, 0, len(group.matches))
		for _, match := range group.matches {
			if skipped < offset {
				skipped++
				continue
			}
			if collected >= pageLimit {
				break
			}
			pageMatches = append(pageMatches, match)
			collected++
		}
		if len(pageMatches) == 0 {
			continue
		}
		snapshot := redaction.SanitizeString(group.snapshot)
		page = append(page, restic.FindGroup{
			Hits:     uint64(len(pageMatches)),
			Snapshot: snapshot,
			Matches:  pageMatches,
		})
	}
	return page, collected, totalVisible, nil
}

func findGroupAuthorized(groupID string, snapshots []restic.Snapshot) bool {
	if !restic.ValidSnapshotID(groupID) || groupID == "latest" {
		return false
	}
	matches := 0
	for _, snapshot := range snapshots {
		if snapshotIDMatches(snapshot.ID, groupID) {
			matches++
		}
	}
	return matches == 1
}

func makeRepositoryStats(deps Deps) func(context.Context, *mcp.CallToolRequest, repositoryStatsIn) (*mcp.CallToolResult, any, error) {
	const tool = "repository_stats"
	return func(ctx context.Context, _ *mcp.CallToolRequest, in repositoryStatsIn) (*mcp.CallToolResult, any, error) {
		e, err := requireRepo(deps, in.RepositoryID)
		if err != nil {
			return failTool(deps, tool, in.RepositoryID, err)
		}
		mode := in.Mode
		if mode == "" {
			mode = string(restic.StatsRestoreSize)
		}
		if !restic.ValidStatsMode(mode) {
			return failTool(deps, tool, in.RepositoryID, apperr.New(apperr.InvalidArgument, "invalid stats mode"))
		}
		if in.SnapshotID != "" && !restic.ValidSnapshotID(in.SnapshotID) {
			return failTool(deps, tool, in.RepositoryID, apperr.New(apperr.InvalidArgument, "invalid snapshot_id"))
		}
		window, err := parseTimeWindow(in.After, in.Before)
		if err != nil {
			return failTool(deps, tool, in.RepositoryID, err)
		}
		normalizedAfter, normalizedBefore := window.normalized()
		if in.SnapshotID == "latest" {
			if err := validateLatestScope(e, in.Host, in.Tags, in.Path); err != nil {
				return failTool(deps, tool, in.RepositoryID, err)
			}
		}

		var snapshots []restic.Snapshot
		if in.SnapshotID != "" {
			snapshot, err := resolveVisibleSnapshot(
				ctx, deps, e, in.RepositoryID, in.SnapshotID, in.Host, in.Tags, in.Path, window,
			)
			if err != nil {
				return failTool(deps, tool, in.RepositoryID, err)
			}
			snapshots = []restic.Snapshot{snapshot}
		} else {
			snapshots, err = visibleSnapshots(
				ctx, deps, e, in.RepositoryID, "", in.Host, in.Tags, in.Path, window,
			)
			if err != nil {
				return failTool(deps, tool, in.RepositoryID, err)
			}
		}

		eligible := make([]restic.Snapshot, 0, len(snapshots))
		excluded := 0
		for _, snapshot := range snapshots {
			if statsSnapshotAllowed(e, snapshot) {
				eligible = append(eligible, snapshot)
			} else {
				excluded++
			}
		}
		if len(eligible) == 0 {
			if excluded > 0 {
				return failTool(
					deps,
					tool,
					in.RepositoryID,
					apperr.New(apperr.NotAllowed, "repository_stats cannot safely aggregate snapshots containing paths outside allowed_paths"),
				)
			}
			return failTool(deps, tool, in.RepositoryID, apperr.New(apperr.SnapshotNotFound, "no visible snapshots match the requested filters"))
		}
		lim := deps.Registry.Limits()
		if len(eligible) > lim.MaxSnapshots {
			return failTool(
				deps,
				tool,
				in.RepositoryID,
				apperr.New(apperr.InvalidArgument, "query matches too many snapshots; add a host, tag, path, after, before, or snapshot_id filter"),
			)
		}

		opts := restic.StatsOpts{
			Mode:        restic.StatsMode(mode),
			SnapshotIDs: make([]string, 0, len(eligible)),
		}
		for _, snapshot := range eligible {
			opts.SnapshotIDs = append(opts.SnapshotIDs, snapshot.ID)
		}

		argv, err := restic.BuildStatsArgv(opts)
		if err != nil {
			return failTool(deps, tool, in.RepositoryID, apperr.New(apperr.InvalidArgument, err.Error()))
		}
		res, err := deps.Registry.Run(ctx, in.RepositoryID, argv, true)
		if err != nil {
			return errTool(err), nil, nil
		}
		stats, err := restic.ParseStats(res.Stdout)
		if err != nil {
			return errTool(err), nil, nil
		}
		cost := restic.CostModerate
		if mode == string(restic.StatsRawData) || mode == string(restic.StatsBlobsPerFile) {
			cost = restic.CostExpensive
		}
		out := map[string]any{
			"mode":   mode,
			"stats":  stats,
			"method": "restic stats --json --mode " + mode,
			"notes": []string{
				"This is a counting/size summary, not a full repository integrity check.",
				"Statistics cover entire selected snapshots, not a path subtree.",
				"Snapshots containing roots outside allowed_paths are excluded.",
				"Data blobs were not fully re-read for bitrot detection.",
				"Restic may use a local cache and may lock the repository during this operation.",
			},
			"selected_snapshots": len(eligible),
			"excluded_snapshots": excluded,
			"window":             windowOutput(normalizedAfter, normalizedBefore),
		}
		return okResult(cost, out, false)
	}
}
