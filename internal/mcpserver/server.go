package mcpserver

import (
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/ThomasCrouzet/restic-defensive-mcp/internal/audit"
	"github.com/ThomasCrouzet/restic-defensive-mcp/internal/repositories"
)

// Version is set by main via ldflags or default.
var Version = "0.1.0-dev"

// Deps are injected into tool handlers.
type Deps struct {
	Registry      *repositories.Registry
	ResticVersion string
	Audit         *audit.Logger
	ServerVersion string
	cursors       *cursorStore
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
	deps.cursors = newCursorStore()
	server := mcp.NewServer(&mcp.Implementation{
		Name:    "restic-defensive-mcp",
		Version: deps.ServerVersion,
	}, &mcp.ServerOptions{
		Instructions: "Read-only restic repository inspector. Repositories are preconfigured; callers may only pass repository_id. No backup, restore, forget, prune, unlock, or mutation tools exist.",
	})

	mcp.AddTool(server, &mcp.Tool{
		Name:         "restic_capabilities",
		Description:  "Return server version, restic version, configured repository IDs, supported backends, allowed commands, cost classes, limits, and warnings. Cost: light.",
		Annotations:  &mcp.ToolAnnotations{ReadOnlyHint: true, IdempotentHint: true, DestructiveHint: boolPtr(false)},
		OutputSchema: outputSchemaFor[capabilitiesData](),
	}, makeCapabilities(deps))

	mcp.AddTool(server, &mcp.Tool{
		Name:         "list_repositories",
		Description:  "List preconfigured repository IDs, optional labels, backend kinds, and allowlist restrictions. Does not probe repositories. Cost: light.",
		Annotations:  &mcp.ToolAnnotations{ReadOnlyHint: true, IdempotentHint: true, DestructiveHint: boolPtr(false)},
		OutputSchema: outputSchemaFor[listRepositoriesData](),
	}, makeListRepositories(deps))

	mcp.AddTool(server, &mcp.Tool{
		Name:         "list_snapshots",
		Description:  "List snapshots for a repository_id with optional host/tag/path filters (must be allowlisted). Bounded; supports offset or an opaque continuation cursor. Cost: light.",
		Annotations:  &mcp.ToolAnnotations{ReadOnlyHint: true, IdempotentHint: true, DestructiveHint: boolPtr(false)},
		OutputSchema: outputSchemaFor[listSnapshotsData](),
	}, makeListSnapshots(deps))

	mcp.AddTool(server, &mcp.Tool{
		Name:         "get_snapshot",
		Description:  "Get metadata for one snapshot by full id or unambiguous prefix. latest requires host and/or tag and/or path filters. Cost: light.",
		Annotations:  &mcp.ToolAnnotations{ReadOnlyHint: true, IdempotentHint: true, DestructiveHint: boolPtr(false)},
		OutputSchema: outputSchemaFor[getSnapshotData](),
	}, makeGetSnapshot(deps))

	mcp.AddTool(server, &mcp.Tool{
		Name:         "browse_snapshot",
		Description:  "List directory entries inside a snapshot at an allowlisted path. Non-recursive by default, hard node limit, opaque continuation cursor. No file contents. Cost: moderate.",
		Annotations:  &mcp.ToolAnnotations{ReadOnlyHint: true, IdempotentHint: true, DestructiveHint: boolPtr(false)},
		OutputSchema: outputSchemaFor[browseData](),
	}, makeBrowseSnapshot(deps))

	mcp.AddTool(server, &mcp.Tool{
		Name:         "find_files",
		Description:  "Find files by a simple glob pattern within allowlisted scope. Not full regex. Bounded results with an opaque continuation cursor. Cost: moderate to expensive.",
		Annotations:  &mcp.ToolAnnotations{ReadOnlyHint: true, IdempotentHint: true, DestructiveHint: boolPtr(false)},
		OutputSchema: outputSchemaFor[findData](),
	}, makeFindFiles(deps))

	mcp.AddTool(server, &mcp.Tool{
		Name:         "repository_stats",
		Description:  "Return repository statistics for an allowlisted mode (restore-size, files-by-contents, raw-data, blobs-per-file). Metadata/counting only; not a full data integrity check. Cost: moderate to expensive.",
		Annotations:  &mcp.ToolAnnotations{ReadOnlyHint: true, IdempotentHint: true, DestructiveHint: boolPtr(false)},
		OutputSchema: outputSchemaFor[statsData](),
	}, makeRepositoryStats(deps))

	return server
}

func boolPtr(b bool) *bool { return &b }
