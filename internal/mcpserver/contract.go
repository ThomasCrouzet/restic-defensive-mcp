package mcpserver

import (
	"encoding/json"
	"fmt"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/ThomasCrouzet/restic-defensive-mcp/internal/audit"
	apperr "github.com/ThomasCrouzet/restic-defensive-mcp/internal/errors"
	"github.com/ThomasCrouzet/restic-defensive-mcp/internal/redaction"
	"github.com/ThomasCrouzet/restic-defensive-mcp/internal/repositories"
	"github.com/ThomasCrouzet/restic-defensive-mcp/internal/restic"
)

// toolEnvelope is the typed success envelope advertised in every output schema.
type toolEnvelope[T any] struct {
	Cost      restic.CostClass `json:"cost" jsonschema:"success cost class"`
	Truncated bool             `json:"truncated,omitempty" jsonschema:"more results exist or the bounded scan is incomplete"`
	Data      T                `json:"data"`
	Warnings  []string         `json:"warnings,omitempty" jsonschema:"operator-facing response warnings"`
}

type windowData struct {
	After   *string `json:"after" jsonschema:"normalized inclusive lower bound, or null"`
	Before  *string `json:"before" jsonschema:"normalized exclusive upper bound, or null"`
	Bounded bool    `json:"bounded" jsonschema:"true when at least one bound is set"`
}

type capabilitiesLimits struct {
	CommandTimeoutSec          int `json:"command_timeout_sec"`
	ExpensiveCommandTimeoutSec int `json:"expensive_command_timeout_sec"`
	MaxSnapshots               int `json:"max_snapshots"`
	MaxNodes                   int `json:"max_nodes"`
	MaxFindResults             int `json:"max_find_results"`
	MaxOutputBytes             int `json:"max_output_bytes"`
	MaxConcurrentPerRepo       int `json:"max_concurrent_per_repo"`
	MaxConcurrentGlobal        int `json:"max_concurrent_global"`
}

type capabilitiesData struct {
	ServerVersion           string             `json:"server_version"`
	ResticVersion           string             `json:"restic_version"`
	RepositoryIDs           []string           `json:"repository_ids"`
	SupportedBackends       []string           `json:"supported_backends"`
	UnsupportedBackends     []string           `json:"unsupported_backends"`
	AllowedResticCommands   []string           `json:"allowed_restic_commands"`
	ForbiddenResticCommands []string           `json:"forbidden_restic_commands"`
	Tools                   []string           `json:"tools"`
	CostClasses             []string           `json:"cost_classes"`
	Limits                  capabilitiesLimits `json:"limits"`
	Warnings                []string           `json:"warnings"`
}

type listSnapshotsData struct {
	Snapshots  []restic.Snapshot `json:"snapshots"`
	Count      int               `json:"count"`
	Total      int               `json:"total"`
	Offset     int               `json:"offset"`
	Limit      int               `json:"limit"`
	Window     windowData        `json:"window"`
	NextCursor string            `json:"next_cursor,omitempty"`
}

type browseData struct {
	Path       string        `json:"path"`
	Nodes      []restic.Node `json:"nodes"`
	Count      int           `json:"count"`
	Total      int           `json:"total"`
	Recursive  bool          `json:"recursive"`
	Incomplete bool          `json:"incomplete"`
	NextCursor string        `json:"next_cursor,omitempty"`
}

type findData struct {
	Groups     []restic.FindGroup `json:"groups"`
	Count      int                `json:"count"`
	Total      *int               `json:"total,omitempty"`
	Incomplete bool               `json:"incomplete"`
	Window     windowData         `json:"window"`
	NextCursor string             `json:"next_cursor,omitempty"`
}

type statsData struct {
	Mode              string             `json:"mode"`
	Stats             restic.StatsResult `json:"stats"`
	Method            string             `json:"method"`
	Notes             []string           `json:"notes"`
	SelectedSnapshots int                `json:"selected_snapshots"`
	ExcludedSnapshots int                `json:"excluded_snapshots"`
	Window            windowData         `json:"window"`
}

type getSnapshotData = restic.Snapshot
type listRepositoriesData = []repositories.PublicInfo

func outputSchemaFor[T any]() any {
	schema, err := jsonschema.For[toolEnvelope[T]](nil)
	if err != nil {
		panic(fmt.Sprintf("build output schema: %v", err))
	}
	return schema
}

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
	auditRepoID := repoID
	if ae.Code == apperr.NotAllowed {
		status = "denied"
	} else {
		// Unknown identifiers are client-controlled and can contain sensitive paths.
		auditRepoID = ""
	}
	deps.Audit.Log(audit.Event{
		Action:       "tool_rejected",
		Tool:         tool,
		RepositoryID: auditRepoID,
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
	After        string   `json:"after,omitempty" jsonschema:"optional RFC 3339 inclusive lower bound on snapshot creation time"`
	Before       string   `json:"before,omitempty" jsonschema:"optional RFC 3339 exclusive upper bound on snapshot creation time"`
	Cursor       string   `json:"cursor,omitempty" jsonschema:"opaque continuation cursor; pass only repository_id, cursor, and optionally limit"`
}

type getSnapshotIn struct {
	RepositoryID string   `json:"repository_id" jsonschema:"preconfigured repository id"`
	SnapshotID   string   `json:"snapshot_id" jsonschema:"full id, prefix (>=8 hex), or latest"`
	Host         string   `json:"host,omitempty" jsonschema:"required with latest if host allowlist or to disambiguate"`
	Tags         []string `json:"tags,omitempty" jsonschema:"optional tags for latest disambiguation"`
	Path         string   `json:"path,omitempty" jsonschema:"optional path for latest disambiguation"`
	After        string   `json:"after,omitempty" jsonschema:"optional RFC 3339 inclusive lower bound on snapshot creation time"`
	Before       string   `json:"before,omitempty" jsonschema:"optional RFC 3339 exclusive upper bound on snapshot creation time"`
}

type browseSnapshotIn struct {
	RepositoryID string `json:"repository_id" jsonschema:"preconfigured repository id"`
	SnapshotID   string `json:"snapshot_id,omitempty" jsonschema:"full id, unambiguous prefix, or latest; omitted when cursor is supplied"`
	Path         string `json:"path,omitempty" jsonschema:"absolute path inside snapshot (must be allowlisted); omitted when cursor is supplied"`
	Recursive    bool   `json:"recursive,omitempty" jsonschema:"default false"`
	Limit        int    `json:"limit,omitempty" jsonschema:"max nodes"`
	Host         string `json:"host,omitempty" jsonschema:"optional host filter"`
	After        string `json:"after,omitempty" jsonschema:"optional RFC 3339 inclusive lower bound on snapshot creation time"`
	Before       string `json:"before,omitempty" jsonschema:"optional RFC 3339 exclusive upper bound on snapshot creation time"`
	Cursor       string `json:"cursor,omitempty" jsonschema:"opaque continuation cursor; pass only repository_id, cursor, and optionally limit"`
}

type findFilesIn struct {
	RepositoryID string   `json:"repository_id" jsonschema:"preconfigured repository id"`
	Pattern      string   `json:"pattern,omitempty" jsonschema:"simple glob pattern (not regex); omitted when cursor is supplied"`
	Path         string   `json:"path,omitempty" jsonschema:"optional path scope (must be allowlisted)"`
	SnapshotID   string   `json:"snapshot_id,omitempty" jsonschema:"optional concrete snapshot id"`
	Host         string   `json:"host,omitempty" jsonschema:"optional host filter"`
	Tags         []string `json:"tags,omitempty" jsonschema:"optional tags"`
	Limit        int      `json:"limit,omitempty" jsonschema:"max matches"`
	IgnoreCase   bool     `json:"ignore_case,omitempty"`
	After        string   `json:"after,omitempty" jsonschema:"optional RFC 3339 inclusive lower bound on snapshot creation time; not a file mtime filter"`
	Before       string   `json:"before,omitempty" jsonschema:"optional RFC 3339 exclusive upper bound on snapshot creation time; not a file mtime filter"`
	Cursor       string   `json:"cursor,omitempty" jsonschema:"opaque continuation cursor; pass only repository_id, cursor, and optionally limit"`
}

type repositoryStatsIn struct {
	RepositoryID string   `json:"repository_id" jsonschema:"preconfigured repository id"`
	Mode         string   `json:"mode,omitempty" jsonschema:"restore-size|files-by-contents|raw-data|blobs-per-file"`
	SnapshotID   string   `json:"snapshot_id,omitempty" jsonschema:"optional snapshot id or latest"`
	Host         string   `json:"host,omitempty"`
	Tags         []string `json:"tags,omitempty"`
	Path         string   `json:"path,omitempty"`
	After        string   `json:"after,omitempty" jsonschema:"optional RFC 3339 inclusive lower bound on snapshot creation time"`
	Before       string   `json:"before,omitempty" jsonschema:"optional RFC 3339 exclusive upper bound on snapshot creation time"`
}

func windowOutput(after, before string) windowData {
	out := windowData{Bounded: after != "" || before != ""}
	if after != "" {
		out.After = &after
	}
	if before != "" {
		out.Before = &before
	}
	return out
}
