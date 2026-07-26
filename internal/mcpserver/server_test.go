package mcpserver_test

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/ThomasCrouzet/restic-defensive-mcp/internal/audit"
	"github.com/ThomasCrouzet/restic-defensive-mcp/internal/config"
	"github.com/ThomasCrouzet/restic-defensive-mcp/internal/mcpserver"
	"github.com/ThomasCrouzet/restic-defensive-mcp/internal/repositories"
	"github.com/ThomasCrouzet/restic-defensive-mcp/internal/restic"
)

func TestToolListExactSevenNoMutation(t *testing.T) {
	deps := testDeps(t, nil)
	srv := mcpserver.NewServer(deps)

	ctx := context.Background()
	// Use in-memory transport pair via client connecting to server
	t1, t2 := mcp.NewInMemoryTransports()
	go func() { _ = srv.Run(ctx, t1) }()

	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1.0"}, nil)
	session, err := client.Connect(ctx, t2, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()

	var names []string
	for tool, err := range session.Tools(ctx, nil) {
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, tool.Name)
	}
	if len(names) != 7 {
		t.Fatalf("expected 7 tools, got %d: %v", len(names), names)
	}
	forbidden := []string{"backup", "restore", "forget", "prune", "unlock", "run_restic", "execute", "raw_command"}
	for _, n := range names {
		for _, f := range forbidden {
			if strings.Contains(strings.ToLower(n), f) {
				t.Fatalf("mutation-like tool present: %s", n)
			}
		}
	}
	for _, want := range mcpserver.ToolNames() {
		found := false
		for _, n := range names {
			if n == want {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("missing tool %s in %v", want, names)
		}
	}
}

func TestCapabilitiesAndListRepos(t *testing.T) {
	deps := testDeps(t, nil)
	srv := mcpserver.NewServer(deps)
	ctx := context.Background()
	t1, t2 := mcp.NewInMemoryTransports()
	go func() { _ = srv.Run(ctx, t1) }()
	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1.0"}, nil)
	session, err := client.Connect(ctx, t2, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()

	res, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "restic_capabilities", Arguments: map[string]any{}})
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("capabilities error: %+v", res)
	}
	text := contentText(res)
	if strings.Contains(text, "password") && strings.Contains(text, "/run/secrets") {
		t.Fatal("secrets path leaked")
	}
	if !strings.Contains(text, "local-test") {
		t.Fatalf("expected repo id: %s", text)
	}

	res, err = session.CallTool(ctx, &mcp.CallToolParams{Name: "list_repositories", Arguments: map[string]any{}})
	if err != nil || res.IsError {
		t.Fatalf("list_repositories: %v %+v", err, res)
	}
	if strings.Contains(contentText(res), "s3:") || strings.Contains(contentText(res), "password") {
		t.Fatal("url or secret leaked")
	}
}

func TestUnknownRepository(t *testing.T) {
	deps := testDeps(t, nil)
	srv := mcpserver.NewServer(deps)
	ctx := context.Background()
	t1, t2 := mcp.NewInMemoryTransports()
	go func() { _ = srv.Run(ctx, t1) }()
	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1.0"}, nil)
	session, err := client.Connect(ctx, t2, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()

	res, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name: "list_snapshots",
		Arguments: map[string]any{
			"repository_id": "no-such-repo",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError {
		t.Fatal("expected error")
	}
	if !strings.Contains(contentText(res), "repository_not_found") {
		t.Fatalf("got %s", contentText(res))
	}
}

func TestPathNotAllowed(t *testing.T) {
	fake := restic.NewFakeRunner()
	fake.Handler = func(req restic.RunRequest) (*restic.Result, error) {
		return &restic.Result{Stdout: []byte("[]"), ExitCode: 0}, nil
	}
	deps := testDeps(t, fake)
	srv := mcpserver.NewServer(deps)
	ctx := context.Background()
	t1, t2 := mcp.NewInMemoryTransports()
	go func() { _ = srv.Run(ctx, t1) }()
	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1.0"}, nil)
	session, err := client.Connect(ctx, t2, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()

	res, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name: "browse_snapshot",
		Arguments: map[string]any{
			"repository_id": "local-test",
			"snapshot_id":   "abcdef0123456789",
			"path":          "/etc/passwd",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError {
		t.Fatal("expected not_allowed")
	}
	if !strings.Contains(contentText(res), "not_allowed") {
		t.Fatalf("got %s", contentText(res))
	}
	// Ensure restic was never invoked for denied path
	if fake.LastCall() != nil {
		t.Fatal("restic should not run for denied path")
	}
}

func TestNegativeLimitsRejectedBeforeRestic(t *testing.T) {
	cases := []struct {
		tool string
		args map[string]any
	}{
		{
			tool: "list_snapshots",
			args: map[string]any{
				"repository_id": "local-test",
				"limit":         -1,
			},
		},
		{
			tool: "browse_snapshot",
			args: map[string]any{
				"repository_id": "local-test",
				"snapshot_id":   "abcdef0123456789",
				"path":          "/data",
				"limit":         -1,
			},
		},
		{
			tool: "find_files",
			args: map[string]any{
				"repository_id": "local-test",
				"pattern":       "*.txt",
				"limit":         -1,
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.tool, func(t *testing.T) {
			fake := restic.NewFakeRunner()
			fake.Handler = func(req restic.RunRequest) (*restic.Result, error) {
				t.Fatalf("restic must not run for a negative limit: %v", req.Argv)
				return nil, nil
			}
			res := callTestTool(t, testDeps(t, fake), tc.tool, tc.args)
			if !res.IsError || !strings.Contains(contentText(res), "invalid_argument") {
				t.Fatalf("expected invalid_argument, got %s", contentText(res))
			}
			if fake.LastCall() != nil {
				t.Fatal("restic must not run")
			}
		})
	}
}

func TestListSnapshotsUsesFake(t *testing.T) {
	raw := `[{"time":"2024-06-01T12:00:00Z","hostname":"testhost","username":"root","parent":"pppppppppppppppppppppppppppppppppppppppppppppppppppppppppppppppp","tree":"tttttttttttttttttttttttttttttttttttttttttttttttttttttttttttttttt","paths":["/data"],"tags":["daily"],"id":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}]`
	fake := restic.NewFakeRunner()
	fake.Handler = func(req restic.RunRequest) (*restic.Result, error) {
		if req.Argv[0] != "snapshots" {
			t.Fatalf("unexpected cmd %v", req.Argv)
		}
		// Ensure no password in env values as command args
		for _, a := range req.Argv {
			if strings.Contains(strings.ToLower(a), "password") {
				t.Fatalf("password in argv: %v", req.Argv)
			}
		}
		return &restic.Result{Stdout: []byte(raw), ExitCode: 0}, nil
	}
	deps := testDeps(t, fake)
	srv := mcpserver.NewServer(deps)
	ctx := context.Background()
	t1, t2 := mcp.NewInMemoryTransports()
	go func() { _ = srv.Run(ctx, t1) }()
	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1.0"}, nil)
	session, err := client.Connect(ctx, t2, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()

	res, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name: "list_snapshots",
		Arguments: map[string]any{
			"repository_id": "local-test",
			"host":          "testhost",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("error: %s", contentText(res))
	}
	text := contentText(res)
	if !strings.Contains(text, "aaaaaaaa") {
		t.Fatalf("missing snapshot: %s", text)
	}
	// L4: username, parent tree hashes must not reach the client.
	if strings.Contains(text, `"username"`) && strings.Contains(text, "root") {
		t.Fatalf("username leaked: %s", text)
	}
	if strings.Contains(text, "pppppppp") || strings.Contains(text, "tttttttt") {
		t.Fatalf("parent/tree hashes leaked: %s", text)
	}
	var env map[string]any
	if err := json.Unmarshal([]byte(text), &env); err != nil {
		t.Fatal(err)
	}
	if env["cost"] != "light" {
		t.Fatalf("cost %v", env["cost"])
	}
}

func TestListSnapshotsMatchesNestedAllowedPathLocally(t *testing.T) {
	const snapshots = `[{
		"time":"2024-06-01T12:00:00Z",
		"hostname":"testhost",
		"paths":["/data"],
		"tags":["daily"],
		"summary":{"total_files_processed":42,"total_bytes_processed":999},
		"id":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	}]`
	fake := restic.NewFakeRunner()
	fake.Handler = func(req restic.RunRequest) (*restic.Result, error) {
		for _, arg := range req.Argv {
			if arg == "--path" {
				t.Fatalf("nested path filtering must be enforced locally: %v", req.Argv)
			}
		}
		return &restic.Result{Stdout: []byte(snapshots), ExitCode: 0}, nil
	}
	res := callTestTool(
		t,
		testDepsWithPaths(t, fake, []string{"/data/visible"}),
		"list_snapshots",
		map[string]any{
			"repository_id": "local-test",
			"path":          "/data/visible",
		},
	)
	if res.IsError {
		t.Fatalf("unexpected error: %s", contentText(res))
	}
	var env struct {
		Data struct {
			Snapshots []struct {
				Paths []string `json:"paths"`
			} `json:"snapshots"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(contentText(res)), &env); err != nil {
		t.Fatal(err)
	}
	if len(env.Data.Snapshots) != 1 ||
		len(env.Data.Snapshots[0].Paths) != 1 ||
		env.Data.Snapshots[0].Paths[0] != "/data/visible" {
		t.Fatalf("unexpected visible path projection: %s", contentText(res))
	}
	if strings.Contains(contentText(res), `"summary"`) || strings.Contains(contentText(res), `"total_bytes_processed"`) {
		t.Fatalf("whole-snapshot summary leaked through a partial path view: %s", contentText(res))
	}
}

func TestGetSnapshotFullIDUsesSnapshotArgv(t *testing.T) {
	const fullID = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	raw := `[{"time":"2024-06-01T12:00:00Z","hostname":"testhost","paths":["/data"],"tags":["daily"],"id":"` + fullID + `"}]`
	fake := restic.NewFakeRunner()
	fake.Handler = func(req restic.RunRequest) (*restic.Result, error) {
		if req.Argv[0] != "snapshots" {
			t.Fatalf("cmd %v", req.Argv)
		}
		// Full 64-hex id must be passed as a restic snapshot argument.
		found := false
		for _, a := range req.Argv {
			if a == fullID {
				found = true
			}
		}
		if !found {
			t.Fatalf("expected full snapshot id in argv, got %v", req.Argv)
		}
		return &restic.Result{Stdout: []byte(raw), ExitCode: 0}, nil
	}
	deps := testDeps(t, fake)
	srv := mcpserver.NewServer(deps)
	ctx := context.Background()
	t1, t2 := mcp.NewInMemoryTransports()
	go func() { _ = srv.Run(ctx, t1) }()
	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1.0"}, nil)
	session, err := client.Connect(ctx, t2, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()

	res, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name: "get_snapshot",
		Arguments: map[string]any{
			"repository_id": "local-test",
			"snapshot_id":   fullID,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("%s", contentText(res))
	}
	if !strings.Contains(contentText(res), fullID[:16]) {
		t.Fatalf("missing id: %s", contentText(res))
	}
}

func TestGetSnapshotAmbiguousPrefix(t *testing.T) {
	raw := `[
		{"time":"2024-06-01T12:00:00Z","hostname":"testhost","paths":["/data"],"tags":["daily"],"id":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
		{"time":"2024-06-02T12:00:00Z","hostname":"testhost","paths":["/data"],"tags":["daily"],"id":"aaaaaaaabbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"}
	]`
	fake := restic.NewFakeRunner()
	fake.Handler = func(req restic.RunRequest) (*restic.Result, error) {
		// Prefix path must not pass a partial id as SnapshotIDs-only optimization incorrectly.
		for _, a := range req.Argv {
			if a == "aaaaaaaa" {
				t.Fatalf("short prefix should not be sole restic snapshot arg without list disambiguation: %v", req.Argv)
			}
		}
		return &restic.Result{Stdout: []byte(raw), ExitCode: 0}, nil
	}
	deps := testDeps(t, fake)
	srv := mcpserver.NewServer(deps)
	ctx := context.Background()
	t1, t2 := mcp.NewInMemoryTransports()
	go func() { _ = srv.Run(ctx, t1) }()
	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1.0"}, nil)
	session, err := client.Connect(ctx, t2, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()

	res, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name: "get_snapshot",
		Arguments: map[string]any{
			"repository_id": "local-test",
			"snapshot_id":   "aaaaaaaa",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError {
		t.Fatal("expected ambiguous_snapshot")
	}
	if !strings.Contains(contentText(res), "ambiguous_snapshot") {
		t.Fatalf("got %s", contentText(res))
	}
}

func TestFindFilesCountAfterPathFilter(t *testing.T) {
	// One match under allowlist (/data), one outside (/etc) — count must be 1 after filter.
	raw := `[{"hits":2,"snapshot":"abcdef01","matches":[
		{"path":"/data/hello.txt","name":"hello.txt","type":"file","size":1,"mtime":"2024-01-01T00:00:00Z"},
		{"path":"/etc/shadow","name":"shadow","type":"file","size":1,"mtime":"2024-01-01T00:00:00Z"}
	]}]`
	fake := restic.NewFakeRunner()
	fake.Handler = func(req restic.RunRequest) (*restic.Result, error) {
		switch req.Argv[0] {
		case restic.CmdSnapshots:
			return &restic.Result{Stdout: []byte(testSnapshotJSON), ExitCode: 0}, nil
		case restic.CmdFind:
			return &restic.Result{Stdout: []byte(raw), ExitCode: 0}, nil
		default:
			t.Fatalf("unexpected command: %v", req.Argv)
			return nil, nil
		}
	}
	deps := testDeps(t, fake)
	srv := mcpserver.NewServer(deps)
	ctx := context.Background()
	t1, t2 := mcp.NewInMemoryTransports()
	go func() { _ = srv.Run(ctx, t1) }()
	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1.0"}, nil)
	session, err := client.Connect(ctx, t2, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()

	res, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name: "find_files",
		Arguments: map[string]any{
			"repository_id": "local-test",
			"pattern":       "*",
			"snapshot_id":   "abcdef0123456789",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("%s", contentText(res))
	}
	text := contentText(res)
	var env struct {
		Data struct {
			Count int `json:"count"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(text), &env); err != nil {
		t.Fatal(err)
	}
	if env.Data.Count != 1 {
		t.Fatalf("want count=1 after allowlist filter, got %d: %s", env.Data.Count, text)
	}
	if strings.Contains(text, "/etc/shadow") {
		t.Fatal("disallowed path leaked")
	}
	if !strings.Contains(text, "/data/hello.txt") {
		t.Fatalf("allowed path missing: %s", text)
	}
}

func TestFindFilesRequiresPathForMultipleAllowedRoots(t *testing.T) {
	fake := restic.NewFakeRunner()
	fake.Handler = func(req restic.RunRequest) (*restic.Result, error) {
		t.Fatalf("restic must not run when path scope is ambiguous: %v", req.Argv)
		return nil, nil
	}
	deps := testDepsWithPaths(t, fake, []string{"/srv/data", "/etc/app"})
	srv := mcpserver.NewServer(deps)
	ctx := context.Background()
	t1, t2 := mcp.NewInMemoryTransports()
	go func() { _ = srv.Run(ctx, t1) }()
	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1.0"}, nil)
	session, err := client.Connect(ctx, t2, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()

	res, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name: "find_files",
		Arguments: map[string]any{
			"repository_id": "local-test",
			"pattern":       "*.log",
			"snapshot_id":   "abcdef0123456789",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError || !strings.Contains(contentText(res), "path is required") {
		t.Fatalf("expected path requirement, got %s", contentText(res))
	}
	if fake.LastCall() != nil {
		t.Fatal("restic must not run")
	}
}

// TestFindFilesTruncatedOnlyWhenVisiblePageFull: pre-filter parse can hit the limit
// with mixed allowlisted/disallowed hits; truncated must be false when visible count < limit.
func TestFindFilesTruncatedOnlyWhenVisiblePageFull(t *testing.T) {
	// limit=2: three matches so ParseFind sets truncated=true after taking the first two.
	// One of those two is allowlisted → visible=1 < limit → truncated must clear.
	raw := `[{"hits":3,"snapshot":"abcdef01","matches":[
		{"path":"/etc/a","name":"a","type":"file","size":1,"mtime":"2024-01-01T00:00:00Z"},
		{"path":"/data/ok.txt","name":"ok.txt","type":"file","size":1,"mtime":"2024-01-01T00:00:00Z"},
		{"path":"/etc/b","name":"b","type":"file","size":1,"mtime":"2024-01-01T00:00:00Z"}
	]}]`
	fake := restic.NewFakeRunner()
	fake.Handler = func(req restic.RunRequest) (*restic.Result, error) {
		if req.Argv[0] == restic.CmdSnapshots {
			return &restic.Result{Stdout: []byte(testSnapshotJSON), ExitCode: 0}, nil
		}
		return &restic.Result{Stdout: []byte(raw), ExitCode: 0}, nil
	}
	deps := testDepsWithLimits(t, fake, 2, []string{"/data"})
	srv := mcpserver.NewServer(deps)
	ctx := context.Background()
	t1, t2 := mcp.NewInMemoryTransports()
	go func() { _ = srv.Run(ctx, t1) }()
	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1.0"}, nil)
	session, err := client.Connect(ctx, t2, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()

	res, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name: "find_files",
		Arguments: map[string]any{
			"repository_id": "local-test",
			"pattern":       "*",
			"snapshot_id":   "abcdef0123456789",
			"limit":         2,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("%s", contentText(res))
	}
	text := contentText(res)
	var env struct {
		Truncated bool `json:"truncated"`
		Data      struct {
			Count int `json:"count"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(text), &env); err != nil {
		t.Fatal(err)
	}
	if env.Data.Count != 1 {
		t.Fatalf("want visible count=1, got %d: %s", env.Data.Count, text)
	}
	if env.Truncated {
		t.Fatalf("truncated must be false when visible count < limit after filter: %s", text)
	}
}

// TestFindFilesTruncatedWhenVisiblePageFull keeps truncated=true when the allowlisted
// page is full and parse hit its cap (all matches under allowlist).
func TestFindFilesTruncatedWhenVisiblePageFull(t *testing.T) {
	raw := `[{"hits":3,"snapshot":"abcdef01","matches":[
		{"path":"/data/a","name":"a","type":"file","size":1,"mtime":"2024-01-01T00:00:00Z"},
		{"path":"/data/b","name":"b","type":"file","size":1,"mtime":"2024-01-01T00:00:00Z"},
		{"path":"/data/c","name":"c","type":"file","size":1,"mtime":"2024-01-01T00:00:00Z"}
	]}]`
	fake := restic.NewFakeRunner()
	fake.Handler = func(req restic.RunRequest) (*restic.Result, error) {
		if req.Argv[0] == restic.CmdSnapshots {
			return &restic.Result{Stdout: []byte(testSnapshotJSON), ExitCode: 0}, nil
		}
		return &restic.Result{Stdout: []byte(raw), ExitCode: 0}, nil
	}
	deps := testDepsWithLimits(t, fake, 2, []string{"/data"})
	srv := mcpserver.NewServer(deps)
	ctx := context.Background()
	t1, t2 := mcp.NewInMemoryTransports()
	go func() { _ = srv.Run(ctx, t1) }()
	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1.0"}, nil)
	session, err := client.Connect(ctx, t2, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()

	res, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name: "find_files",
		Arguments: map[string]any{
			"repository_id": "local-test",
			"pattern":       "*",
			"snapshot_id":   "abcdef0123456789",
			"limit":         2,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("%s", contentText(res))
	}
	text := contentText(res)
	var env struct {
		Truncated bool `json:"truncated"`
		Data      struct {
			Count int `json:"count"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(text), &env); err != nil {
		t.Fatal(err)
	}
	if env.Data.Count != 2 {
		t.Fatalf("want count=2 (page full), got %d: %s", env.Data.Count, text)
	}
	if !env.Truncated {
		t.Fatalf("truncated must be true when visible page is full and parse capped: %s", text)
	}
}

func TestPathDenialIsAudited(t *testing.T) {
	var auditBuf bytes.Buffer
	fake := restic.NewFakeRunner()
	fake.Handler = func(req restic.RunRequest) (*restic.Result, error) {
		t.Fatal("restic must not run on path denial")
		return nil, nil
	}
	deps := testDeps(t, fake)
	deps.Audit = audit.New(&auditBuf)
	// Rebuild registry audit is separate; denial audit uses deps.Audit in mcpserver.
	srv := mcpserver.NewServer(deps)
	ctx := context.Background()
	t1, t2 := mcp.NewInMemoryTransports()
	go func() { _ = srv.Run(ctx, t1) }()
	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1.0"}, nil)
	session, err := client.Connect(ctx, t2, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()

	res, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name: "browse_snapshot",
		Arguments: map[string]any{
			"repository_id": "local-test",
			"snapshot_id":   "abcdef0123456789",
			"path":          "/etc/passwd",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError {
		t.Fatal("expected not_allowed")
	}
	log := auditBuf.String()
	if !strings.Contains(log, `"action":"tool_rejected"`) {
		t.Fatalf("expected tool_rejected audit, got %q", log)
	}
	if !strings.Contains(log, `"status":"denied"`) {
		t.Fatalf("expected denied status, got %q", log)
	}
	if !strings.Contains(log, "browse_snapshot") {
		t.Fatalf("expected tool name, got %q", log)
	}
	if fake.LastCall() != nil {
		t.Fatal("restic should not run")
	}
}

func TestBrowseSnapshotRejectsInvisibleConcreteSnapshot(t *testing.T) {
	const hidden = `[{
		"time":"2024-06-01T12:00:00Z",
		"hostname":"testhost",
		"paths":["/data"],
		"tags":["private"],
		"id":"abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789"
	}]`
	fake := restic.NewFakeRunner()
	fake.Handler = func(req restic.RunRequest) (*restic.Result, error) {
		if req.Argv[0] != restic.CmdSnapshots {
			t.Fatalf("content command must not run for an invisible snapshot: %v", req.Argv)
		}
		return &restic.Result{Stdout: []byte(hidden), ExitCode: 0}, nil
	}
	res := callTestTool(t, testDeps(t, fake), "browse_snapshot", map[string]any{
		"repository_id": "local-test",
		"snapshot_id":   "abcdef0123456789",
		"path":          "/data",
	})
	if !res.IsError || !strings.Contains(contentText(res), "snapshot_not_found") {
		t.Fatalf("expected hidden snapshot rejection, got %s", contentText(res))
	}
}

func TestBrowseSnapshotAppliesLimitAfterPathPolicy(t *testing.T) {
	const listing = `{"message_type":"node","name":"hidden","type":"file","path":"/etc/hidden","size":1}
{"message_type":"node","name":"one","type":"file","path":"/data/one","size":1}
{"message_type":"node","name":"two","type":"file","path":"/data/two","size":1}
`
	fake := restic.NewFakeRunner()
	fake.Handler = func(req restic.RunRequest) (*restic.Result, error) {
		switch req.Argv[0] {
		case restic.CmdSnapshots:
			return &restic.Result{Stdout: []byte(testSnapshotJSON), ExitCode: 0}, nil
		case restic.CmdLS:
			return &restic.Result{Stdout: []byte(listing), ExitCode: 0}, nil
		default:
			t.Fatalf("unexpected command: %v", req.Argv)
			return nil, nil
		}
	}
	res := callTestTool(t, testDeps(t, fake), "browse_snapshot", map[string]any{
		"repository_id": "local-test",
		"snapshot_id":   "abcdef0123456789",
		"path":          "/data",
		"limit":         1,
	})
	if res.IsError {
		t.Fatalf("unexpected error: %s", contentText(res))
	}
	var env struct {
		Truncated bool `json:"truncated"`
		Data      struct {
			Count int `json:"count"`
			Nodes []struct {
				Path string `json:"path"`
			} `json:"nodes"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(contentText(res)), &env); err != nil {
		t.Fatal(err)
	}
	if env.Data.Count != 1 || len(env.Data.Nodes) != 1 || env.Data.Nodes[0].Path != "/data/one" {
		t.Fatalf("authorized node was hidden by pre-filter limiting: %s", contentText(res))
	}
	if !env.Truncated {
		t.Fatalf("additional authorized node must set truncated: %s", contentText(res))
	}
}

func TestFindFilesSelectsOnlyVisibleSnapshots(t *testing.T) {
	const snapshots = `[
		{
			"time":"2024-06-02T12:00:00Z",
			"hostname":"testhost",
			"paths":["/data"],
			"tags":["daily"],
			"id":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
		},
		{
			"time":"2024-06-01T12:00:00Z",
			"hostname":"testhost",
			"paths":["/data"],
			"tags":["private"],
			"id":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
		}
	]`
	fake := restic.NewFakeRunner()
	fake.Handler = func(req restic.RunRequest) (*restic.Result, error) {
		switch req.Argv[0] {
		case restic.CmdSnapshots:
			return &restic.Result{Stdout: []byte(snapshots), ExitCode: 0}, nil
		case restic.CmdFind:
			joined := strings.Join(req.Argv, " ")
			if !strings.Contains(joined, strings.Repeat("a", 64)) {
				t.Fatalf("visible snapshot missing from argv: %v", req.Argv)
			}
			if strings.Contains(joined, strings.Repeat("b", 64)) {
				t.Fatalf("invisible snapshot reached find argv: %v", req.Argv)
			}
			return &restic.Result{Stdout: []byte(`[]`), ExitCode: 0}, nil
		default:
			t.Fatalf("unexpected command: %v", req.Argv)
			return nil, nil
		}
	}
	res := callTestTool(t, testDeps(t, fake), "find_files", map[string]any{
		"repository_id": "local-test",
		"pattern":       "*.txt",
		"path":          "/data",
	})
	if res.IsError {
		t.Fatalf("%s", contentText(res))
	}
}

func TestRepositoryStatsRejectsPartiallyAllowedSnapshot(t *testing.T) {
	const partial = `[{
		"time":"2024-06-01T12:00:00Z",
		"hostname":"testhost",
		"paths":["/data","/etc"],
		"tags":["daily"],
		"id":"abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789"
	}]`
	fake := restic.NewFakeRunner()
	fake.Handler = func(req restic.RunRequest) (*restic.Result, error) {
		if req.Argv[0] != restic.CmdSnapshots {
			t.Fatalf("stats must not run for a partially allowed snapshot: %v", req.Argv)
		}
		return &restic.Result{Stdout: []byte(partial), ExitCode: 0}, nil
	}
	res := callTestTool(t, testDeps(t, fake), "repository_stats", map[string]any{
		"repository_id": "local-test",
		"snapshot_id":   "abcdef0123456789",
	})
	if !res.IsError || !strings.Contains(contentText(res), "not_allowed") {
		t.Fatalf("expected not_allowed, got %s", contentText(res))
	}
}

func TestHostileFilenameSanitized(t *testing.T) {
	// Secret-looking names must be redacted field-by-field without corrupting JSON.
	raw := "{\"message_type\":\"node\",\"name\":\"\\u001b[31mpassword=supersecret\\u0000\",\"type\":\"file\",\"path\":\"/data/password=supersecret\",\"size\":1,\"permissions\":\"-rw-------\",\"mtime\":\"2024-01-01T00:00:00Z\"}\n"
	fake := restic.NewFakeRunner()
	fake.Handler = func(req restic.RunRequest) (*restic.Result, error) {
		if req.Argv[0] == restic.CmdSnapshots {
			return &restic.Result{Stdout: []byte(testSnapshotJSON), ExitCode: 0}, nil
		}
		return &restic.Result{Stdout: []byte(raw), ExitCode: 0}, nil
	}
	deps := testDeps(t, fake)
	srv := mcpserver.NewServer(deps)
	ctx := context.Background()
	t1, t2 := mcp.NewInMemoryTransports()
	go func() { _ = srv.Run(ctx, t1) }()
	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1.0"}, nil)
	session, err := client.Connect(ctx, t2, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()

	res, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name: "browse_snapshot",
		Arguments: map[string]any{
			"repository_id": "local-test",
			"snapshot_id":   "abcdef0123456789",
			"path":          "/data",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("%s", contentText(res))
	}
	text := contentText(res)
	if strings.Contains(text, "\x1b") || strings.Contains(text, "\x00") {
		t.Fatal("control chars leaked")
	}
	if strings.Contains(text, "supersecret") {
		t.Fatal("secret-looking value leaked")
	}
	var decoded map[string]any
	if err := json.Unmarshal([]byte(text), &decoded); err != nil {
		t.Fatalf("response is not valid JSON after redaction: %v\n%s", err, text)
	}
}

func testDeps(t *testing.T, runner restic.Runner) mcpserver.Deps {
	t.Helper()
	return testDepsWithLimits(t, runner, 200, []string{"/data"})
}

func testDepsWithPaths(t *testing.T, runner restic.Runner, paths []string) mcpserver.Deps {
	t.Helper()
	return testDepsWithLimits(t, runner, 200, paths)
}

func testDepsWithLimits(t *testing.T, runner restic.Runner, maxFind int, paths []string) mcpserver.Deps {
	t.Helper()
	dir := t.TempDir()
	repoFile := filepath.Join(dir, "repo")
	passFile := filepath.Join(dir, "pass")
	if err := os.WriteFile(repoFile, []byte("/tmp/fake-restic-repo\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(passFile, []byte("test-pass\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{
		Version: 1,
		Limits: config.Limits{
			CommandTimeout:          10 * time.Second,
			ExpensiveCommandTimeout: 30 * time.Second,
			MaxSnapshots:            100,
			MaxNodes:                500,
			MaxFindResults:          maxFind,
			MaxOutputBytes:          1 << 20,
			MaxConcurrentPerRepo:    1,
			MaxConcurrentGlobal:     2,
		},
		Repositories: []config.Repository{{
			ID:             "local-test",
			Label:          "Test",
			RepositoryFile: repoFile,
			PasswordFile:   passFile,
			AllowedHosts:   []string{"testhost"},
			AllowedTags:    []string{"daily", "weekly", "test"},
			AllowedPaths:   append([]string(nil), paths...),
		}},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	if runner == nil {
		runner = restic.NewFakeRunner()
	}
	reg, err := repositories.New(cfg, runner, audit.New(os.Stderr))
	if err != nil {
		t.Fatal(err)
	}
	return mcpserver.Deps{
		Registry:      reg,
		ResticVersion: "0.19.1",
		Audit:         audit.New(os.Stderr),
		ServerVersion: "test",
	}
}

func contentText(res *mcp.CallToolResult) string {
	var b strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			b.WriteString(tc.Text)
		}
	}
	return b.String()
}

func callTestTool(t *testing.T, deps mcpserver.Deps, name string, arguments map[string]any) *mcp.CallToolResult {
	t.Helper()
	srv := mcpserver.NewServer(deps)
	ctx := context.Background()
	t1, t2 := mcp.NewInMemoryTransports()
	go func() { _ = srv.Run(ctx, t1) }()
	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1.0"}, nil)
	session, err := client.Connect(ctx, t2, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	res, err := session.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: arguments})
	if err != nil {
		t.Fatal(err)
	}
	return res
}

const testSnapshotJSON = `[{
	"time":"2024-06-01T12:00:00Z",
	"hostname":"testhost",
	"paths":["/data"],
	"tags":["daily"],
	"id":"abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789"
}]`
