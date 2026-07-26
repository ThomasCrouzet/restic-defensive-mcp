// Package mcpserver registers the read-only MCP tool surface.
package mcpserver

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/ThomasCrouzet/restic-defensive-mcp/internal/audit"
	apperr "github.com/ThomasCrouzet/restic-defensive-mcp/internal/errors"
	"github.com/ThomasCrouzet/restic-defensive-mcp/internal/policy"
	"github.com/ThomasCrouzet/restic-defensive-mcp/internal/redaction"
	"github.com/ThomasCrouzet/restic-defensive-mcp/internal/repositories"
	"github.com/ThomasCrouzet/restic-defensive-mcp/internal/restic"
)

// Version is set by main via ldflags or default.
var Version = "0.1.0-dev"

// Deps are injected into tool handlers.
type Deps struct {
	Registry      *repositories.Registry
	ResticVersion string
	Audit         *audit.Logger
	ServerVersion string
}

// toolNames is the closed list of MCP tools (max 7).
var toolNames = []string{
	"restic_capabilities",
	"list_repositories",
	"list_snapshots",
	"get_snapshot",
	"browse_snapshot",
	"find_files",
	"repository_stats",
}

// ToolNames returns a copy of the closed MCP tool list.
func ToolNames() []string {
	return append([]string(nil), toolNames...)
}

// NewServer builds an MCP server with all read-only tools registered.
func NewServer(deps Deps) *mcp.Server {
	if deps.ServerVersion == "" {
		deps.ServerVersion = Version
	}
	server := mcp.NewServer(&mcp.Implementation{
		Name:    "restic-defensive-mcp",
		Version: deps.ServerVersion,
	}, &mcp.ServerOptions{
		Instructions: "Read-only restic repository inspector. Repositories are preconfigured; callers may only pass repository_id. No backup, restore, forget, prune, unlock, or mutation tools exist.",
	})

	mcp.AddTool(server, &mcp.Tool{
		Name:        "restic_capabilities",
		Description: "Return server version, restic version, configured repository IDs, supported backends, allowed commands, cost classes, limits, and warnings. Cost: light.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true, IdempotentHint: true, DestructiveHint: boolPtr(false)},
	}, makeCapabilities(deps))

	mcp.AddTool(server, &mcp.Tool{
		Name:        "list_repositories",
		Description: "List preconfigured repository IDs, optional labels, backend kinds, and allowlist restrictions. Does not probe repositories. Cost: light.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true, IdempotentHint: true, DestructiveHint: boolPtr(false)},
	}, makeListRepositories(deps))

	mcp.AddTool(server, &mcp.Tool{
		Name:        "list_snapshots",
		Description: "List snapshots for a repository_id with optional host/tag/path filters (must be allowlisted). Bounded and paginated. Cost: light.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true, IdempotentHint: true, DestructiveHint: boolPtr(false)},
	}, makeListSnapshots(deps))

	mcp.AddTool(server, &mcp.Tool{
		Name:        "get_snapshot",
		Description: "Get metadata for one snapshot by full id or unambiguous prefix. latest requires host and/or tag and/or path filters. Cost: light.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true, IdempotentHint: true, DestructiveHint: boolPtr(false)},
	}, makeGetSnapshot(deps))

	mcp.AddTool(server, &mcp.Tool{
		Name:        "browse_snapshot",
		Description: "List directory entries inside a snapshot at an allowlisted path. Non-recursive by default, hard node limit. No file contents. Cost: moderate.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true, IdempotentHint: true, DestructiveHint: boolPtr(false)},
	}, makeBrowseSnapshot(deps))

	mcp.AddTool(server, &mcp.Tool{
		Name:        "find_files",
		Description: "Find files by a simple glob pattern within allowlisted scope. Not full regex. Bounded results. Cost: moderate to expensive.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true, IdempotentHint: true, DestructiveHint: boolPtr(false)},
	}, makeFindFiles(deps))

	mcp.AddTool(server, &mcp.Tool{
		Name:        "repository_stats",
		Description: "Return repository statistics for an allowlisted mode (restore-size, files-by-contents, raw-data, blobs-per-file). Metadata/counting only; not a full data integrity check. Cost: moderate to expensive.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true, IdempotentHint: true, DestructiveHint: boolPtr(false)},
	}, makeRepositoryStats(deps))

	return server
}

func boolPtr(b bool) *bool { return &b }

// --- shared response helpers ---

type envelope struct {
	Cost      restic.CostClass `json:"cost"`
	Truncated bool             `json:"truncated,omitempty"`
	Data      any              `json:"data"`
	Warnings  []string         `json:"warnings,omitempty"`
}

func okResult(cost restic.CostClass, data any, truncated bool, warnings ...string) (*mcp.CallToolResult, any, error) {
	env := envelope{Cost: cost, Data: data, Truncated: truncated, Warnings: warnings}
	raw, err := json.Marshal(env)
	if err != nil {
		return errTool(apperr.New(apperr.InternalError, "encode failed")), nil, nil
	}
	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: string(raw)}},
	}, env, nil
}

func errTool(err error) *mcp.CallToolResult {
	code := apperr.InternalError
	msg := "internal error"
	detail := ""
	if ae, ok := apperr.As(err); ok {
		code = ae.Code
		msg = ae.Message
		detail = ae.Detail
	} else if err != nil {
		msg = redaction.SanitizeAndRedact(err.Error())
		msg = redaction.Clip(msg, 200)
	}
	payload := map[string]string{
		"code":    string(code),
		"message": redaction.SanitizeAndRedact(msg),
	}
	if detail != "" {
		payload["detail"] = redaction.SanitizeAndRedact(detail)
	}
	b, _ := json.Marshal(payload)
	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: string(b)}},
		IsError: true,
	}
}

// auditPreResticRejection records policy denials and unknown repository_id values.
func auditPreResticRejection(deps Deps, tool, repoID string, err error) {
	if deps.Audit == nil || err == nil {
		return
	}
	ae, ok := apperr.As(err)
	if !ok {
		return
	}
	switch ae.Code {
	case apperr.NotAllowed, apperr.RepositoryNotFound:
	default:
		return
	}
	status := "error"
	if ae.Code == apperr.NotAllowed {
		status = "denied"
	}
	deps.Audit.Log(audit.Event{
		Action:       "tool_rejected",
		Tool:         tool,
		RepositoryID: repoID,
		Status:       status,
		ErrorCode:    string(ae.Code),
		Message:      ae.Message,
	})
}

// failTool audits pre-restic denials then returns a tool error result.
func failTool(deps Deps, tool, repoID string, err error) (*mcp.CallToolResult, any, error) {
	auditPreResticRejection(deps, tool, repoID, err)
	return errTool(err), nil, nil
}

func requireRepo(deps Deps, id string) (*repositories.Entry, error) {
	if id == "" {
		return nil, apperr.New(apperr.InvalidArgument, "repository_id is required")
	}
	return deps.Registry.Get(id)
}

// tool handler type aliases matching go-sdk AddTool signature with typed input.
// We use map-based inputs via json schema structs.

type emptyIn struct{}

type listReposIn struct{}

type listSnapshotsIn struct {
	RepositoryID string   `json:"repository_id" jsonschema:"preconfigured repository id"`
	Host         string   `json:"host,omitempty" jsonschema:"optional host filter (must be allowlisted)"`
	Tags         []string `json:"tags,omitempty" jsonschema:"optional tags (each must be allowlisted)"`
	Path         string   `json:"path,omitempty" jsonschema:"optional path filter (must be allowlisted)"`
	Limit        int      `json:"limit,omitempty" jsonschema:"max snapshots to return"`
	Offset       int      `json:"offset,omitempty" jsonschema:"pagination offset"`
}

type getSnapshotIn struct {
	RepositoryID string   `json:"repository_id" jsonschema:"preconfigured repository id"`
	SnapshotID   string   `json:"snapshot_id" jsonschema:"full id, prefix (>=8 hex), or latest"`
	Host         string   `json:"host,omitempty" jsonschema:"required with latest if host allowlist or to disambiguate"`
	Tags         []string `json:"tags,omitempty" jsonschema:"optional tags for latest disambiguation"`
	Path         string   `json:"path,omitempty" jsonschema:"optional path for latest disambiguation"`
}

type browseSnapshotIn struct {
	RepositoryID string `json:"repository_id" jsonschema:"preconfigured repository id"`
	SnapshotID   string `json:"snapshot_id" jsonschema:"full id, unambiguous prefix, or latest"`
	Path         string `json:"path" jsonschema:"absolute path inside snapshot (must be allowlisted)"`
	Recursive    bool   `json:"recursive,omitempty" jsonschema:"default false"`
	Limit        int    `json:"limit,omitempty" jsonschema:"max nodes"`
	Host         string `json:"host,omitempty" jsonschema:"optional host filter"`
}

type findFilesIn struct {
	RepositoryID string   `json:"repository_id" jsonschema:"preconfigured repository id"`
	Pattern      string   `json:"pattern" jsonschema:"simple glob pattern (not regex)"`
	Path         string   `json:"path,omitempty" jsonschema:"optional path scope (must be allowlisted)"`
	SnapshotID   string   `json:"snapshot_id,omitempty" jsonschema:"optional concrete snapshot id"`
	Host         string   `json:"host,omitempty" jsonschema:"optional host filter"`
	Tags         []string `json:"tags,omitempty" jsonschema:"optional tags"`
	Limit        int      `json:"limit,omitempty" jsonschema:"max matches"`
	IgnoreCase   bool     `json:"ignore_case,omitempty"`
}

type repositoryStatsIn struct {
	RepositoryID string   `json:"repository_id" jsonschema:"preconfigured repository id"`
	Mode         string   `json:"mode,omitempty" jsonschema:"restore-size|files-by-contents|raw-data|blobs-per-file"`
	SnapshotID   string   `json:"snapshot_id,omitempty" jsonschema:"optional snapshot id or latest"`
	Host         string   `json:"host,omitempty"`
	Tags         []string `json:"tags,omitempty"`
	Path         string   `json:"path,omitempty"`
}

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

		opts := restic.SnapshotsOpts{}
		requestedPath := ""
		if in.Host != "" {
			if err := e.Hosts.Check(in.Host); err != nil {
				return failTool(deps, tool, in.RepositoryID, err)
			}
			opts.Hosts = []string{in.Host}
		} else if len(e.Hosts.Allowed) == 1 {
			// When exactly one host is allowlisted, apply it to avoid leaking others.
			opts.Hosts = []string{e.Hosts.Allowed[0]}
		}
		if len(in.Tags) > 0 {
			if err := e.Tags.CheckEach(in.Tags); err != nil {
				return failTool(deps, tool, in.RepositoryID, err)
			}
			opts.Tags = in.Tags
		}
		if in.Path != "" {
			p, err := e.Paths.Check(in.Path)
			if err != nil {
				return failTool(deps, tool, in.RepositoryID, err)
			}
			requestedPath = p
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
		for _, s := range snaps {
			if !policyVisible(e, s) || !matchesSnapshotFilters(s, in.Host, in.Tags, requestedPath) {
				continue
			}
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
		}
		return okResult(restic.CostLight, out, truncated)
	}
}

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
		if !matchesSnapshotFilters(snapshot, host, tags, requestPath) {
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
	for _, snapshotPath := range snapshot.Paths {
		cleaned, err := policy.CleanPath(snapshotPath)
		if err == nil && pathIsUnderAllow(requestPath, cleaned) {
			return true
		}
	}
	return false
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
) (restic.Snapshot, error) {
	snapshots, err := visibleSnapshots(ctx, deps, e, repoID, selector, host, tags, requestPath)
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
		if in.SnapshotID == "latest" {
			if err := validateLatestScope(e, in.Host, in.Tags, in.Path); err != nil {
				return failTool(deps, tool, in.RepositoryID, err)
			}
		}
		snapshot, err := resolveVisibleSnapshot(
			ctx, deps, e, in.RepositoryID, in.SnapshotID, in.Host, in.Tags, in.Path,
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
		if !restic.ValidSnapshotID(in.SnapshotID) {
			return failTool(deps, tool, in.RepositoryID, apperr.New(apperr.InvalidArgument, "invalid snapshot_id"))
		}
		path, err := e.Paths.Check(in.Path)
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
		if in.SnapshotID == "latest" {
			if err := validateLatestScope(e, in.Host, nil, path); err != nil {
				return failTool(deps, tool, in.RepositoryID, err)
			}
		}
		snapshot, err := resolveVisibleSnapshot(
			ctx, deps, e, in.RepositoryID, in.SnapshotID, in.Host, nil, path,
		)
		if err != nil {
			return failTool(deps, tool, in.RepositoryID, err)
		}

		lsOpts := restic.LSOpts{
			SnapshotID: snapshot.ID,
			Dirs:       []string{path},
			Recursive:  in.Recursive,
		}
		argv, err := restic.BuildLSArgv(lsOpts)
		if err != nil {
			return failTool(deps, tool, in.RepositoryID, apperr.New(apperr.InvalidArgument, err.Error()))
		}
		res, err := deps.Registry.Run(ctx, in.RepositoryID, argv, true)
		if err != nil {
			return errTool(err), nil, nil
		}
		// Parse the complete byte-bounded response, then enforce visibility and
		// the client limit. Limiting before policy filtering could let hostile
		// out-of-scope entries hide later authorized entries.
		nodes, parseTruncated, err := restic.ParseLS(res.Stdout, lim.MaxOutputBytes)
		if err != nil {
			return errTool(err), nil, nil
		}
		filtered := make([]restic.Node, 0, min(len(nodes), limit))
		truncated := parseTruncated
		for _, n := range nodes {
			cleaned, cleanErr := policy.CleanPath(n.Path)
			if cleanErr != nil || !e.Paths.IsAllowed(cleaned) || !pathIsUnderAllow(cleaned, path) {
				continue
			}
			if len(filtered) >= limit {
				truncated = true
				continue
			}
			n.Path = redaction.SanitizeAndRedact(cleaned)
			n.Name = redaction.SanitizeAndRedact(n.Name)
			n.Type = redaction.SanitizeAndRedact(n.Type)
			n.Mode = redaction.SanitizeAndRedact(n.Mode)
			filtered = append(filtered, n)
		}
		out := map[string]any{
			"path":      path,
			"nodes":     filtered,
			"count":     len(filtered),
			"recursive": in.Recursive,
		}
		return okResult(restic.CostModerate, out, truncated)
	}
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

func makeFindFiles(deps Deps) func(context.Context, *mcp.CallToolRequest, findFilesIn) (*mcp.CallToolResult, any, error) {
	const tool = "find_files"
	return func(ctx context.Context, _ *mcp.CallToolRequest, in findFilesIn) (*mcp.CallToolResult, any, error) {
		e, err := requireRepo(deps, in.RepositoryID)
		if err != nil {
			return failTool(deps, tool, in.RepositoryID, err)
		}
		if in.Pattern == "" {
			return failTool(deps, tool, in.RepositoryID, apperr.New(apperr.InvalidArgument, "pattern is required"))
		}
		lim := deps.Registry.Limits()
		limit := in.Limit
		if limit < 0 {
			return failTool(deps, tool, in.RepositoryID, apperr.New(apperr.InvalidArgument, "limit must be >= 0"))
		}
		if limit == 0 || limit > lim.MaxFindResults {
			limit = lim.MaxFindResults
		}
		opts := restic.FindOpts{
			Pattern:    in.Pattern,
			IgnoreCase: in.IgnoreCase,
		}
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
			p, err := e.Paths.Check(scopePath)
			if err != nil {
				return failTool(deps, tool, in.RepositoryID, err)
			}
			scopePath = p
		}
		// Reject unsafe patterns before probing repository metadata.
		if _, err := restic.BuildFindArgv(opts); err != nil {
			return failTool(deps, tool, in.RepositoryID, apperr.New(apperr.InvalidArgument, err.Error()))
		}

		var snapshots []restic.Snapshot
		if in.SnapshotID != "" {
			if !restic.ValidSnapshotID(in.SnapshotID) || in.SnapshotID == "latest" {
				return failTool(deps, tool, in.RepositoryID, apperr.New(apperr.InvalidArgument, "snapshot_id must be a concrete id"))
			}
			snapshot, err := resolveVisibleSnapshot(
				ctx, deps, e, in.RepositoryID, in.SnapshotID, in.Host, in.Tags, scopePath,
			)
			if err != nil {
				return failTool(deps, tool, in.RepositoryID, err)
			}
			snapshots = []restic.Snapshot{snapshot}
		} else {
			snapshots, err = visibleSnapshots(
				ctx, deps, e, in.RepositoryID, "", in.Host, in.Tags, scopePath,
			)
			if err != nil {
				return failTool(deps, tool, in.RepositoryID, err)
			}
			if len(snapshots) > lim.MaxSnapshots {
				return failTool(
					deps,
					tool,
					in.RepositoryID,
					apperr.New(apperr.InvalidArgument, "query matches too many snapshots; add a host, tag, path, or snapshot_id filter"),
				)
			}
		}
		if len(snapshots) == 0 {
			cost := restic.CostExpensive
			if in.SnapshotID != "" {
				cost = restic.CostModerate
			}
			return okResult(cost, map[string]any{"groups": []restic.FindGroup{}, "count": 0}, false)
		}
		opts.SnapshotIDs = make([]string, 0, len(snapshots))
		for _, snapshot := range snapshots {
			opts.SnapshotIDs = append(opts.SnapshotIDs, snapshot.ID)
		}

		argv, err := restic.BuildFindArgv(opts)
		if err != nil {
			return failTool(deps, tool, in.RepositoryID, apperr.New(apperr.InvalidArgument, err.Error()))
		}
		res, err := deps.Registry.Run(ctx, in.RepositoryID, argv, true)
		if err != nil {
			return errTool(err), nil, nil
		}
		// Parse all matches present in the already byte-bounded process output,
		// then apply path policy before enforcing the client-visible result limit.
		groups, _, _, err := restic.ParseFind(res.Stdout, lim.MaxOutputBytes)
		if err != nil {
			return errTool(err), nil, nil
		}
		visible := 0
		truncated := false
		kept := make([]restic.FindGroup, 0, len(groups))
		for _, g := range groups {
			if !findGroupAuthorized(g.Snapshot, snapshots) {
				continue
			}
			m := make([]restic.FindMatch, 0, len(g.Matches))
			for _, match := range g.Matches {
				cleaned, cleanErr := policy.CleanPath(match.Path)
				if cleanErr != nil || !e.Paths.IsAllowed(cleaned) {
					continue
				}
				if scopePath != "" && !pathIsUnderAllow(cleaned, scopePath) {
					continue
				}
				if visible >= limit {
					truncated = true
					continue
				}
				match.Path = redaction.SanitizeAndRedact(cleaned)
				match.Name = redaction.SanitizeAndRedact(match.Name)
				match.Type = redaction.SanitizeAndRedact(match.Type)
				m = append(m, match)
				visible++
			}
			if len(m) == 0 {
				continue
			}
			g.Matches = m
			g.Hits = uint64(len(m))
			g.Snapshot = redaction.SanitizeString(g.Snapshot)
			kept = append(kept, g)
		}
		out := map[string]any{
			"groups": kept,
			"count":  visible,
		}
		cost := restic.CostModerate
		if in.SnapshotID == "" {
			cost = restic.CostExpensive
		}
		return okResult(cost, out, truncated)
	}
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
		if in.SnapshotID == "latest" {
			if err := validateLatestScope(e, in.Host, in.Tags, in.Path); err != nil {
				return failTool(deps, tool, in.RepositoryID, err)
			}
		}

		var snapshots []restic.Snapshot
		if in.SnapshotID != "" {
			snapshot, err := resolveVisibleSnapshot(
				ctx, deps, e, in.RepositoryID, in.SnapshotID, in.Host, in.Tags, in.Path,
			)
			if err != nil {
				return failTool(deps, tool, in.RepositoryID, err)
			}
			snapshots = []restic.Snapshot{snapshot}
		} else {
			snapshots, err = visibleSnapshots(
				ctx, deps, e, in.RepositoryID, "", in.Host, in.Tags, in.Path,
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
				apperr.New(apperr.InvalidArgument, "query matches too many snapshots; add a host, tag, path, or snapshot_id filter"),
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
		}
		return okResult(cost, out, false)
	}
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
