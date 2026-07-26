//go:build integration

package main_test

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/ThomasCrouzet/restic-defensive-mcp/internal/audit"
	"github.com/ThomasCrouzet/restic-defensive-mcp/internal/config"
	"github.com/ThomasCrouzet/restic-defensive-mcp/internal/mcpserver"
	"github.com/ThomasCrouzet/restic-defensive-mcp/internal/repositories"
	"github.com/ThomasCrouzet/restic-defensive-mcp/internal/restic"
	"github.com/ThomasCrouzet/restic-defensive-mcp/internal/testrepo"
)

func TestIntegrationRealRestic(t *testing.T) {
	if _, err := exec.LookPath("restic"); err != nil {
		t.Skip("restic not available")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	fix, err := testrepo.Create(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	defer fix.Close()

	host, _ := os.Hostname()
	dataPath := fix.DataDir1
	// Allow both data roots
	cfg := &config.Config{
		Version: 1,
		Limits: config.Limits{
			CommandTimeout:          60 * time.Second,
			ExpensiveCommandTimeout: 2 * time.Minute,
			MaxSnapshots:            50,
			MaxNodes:                200,
			MaxFindResults:          100,
			MaxOutputBytes:          4 << 20,
			MaxConcurrentPerRepo:    1,
			MaxConcurrentGlobal:     2,
		},
		Repositories: []config.Repository{{
			ID:             "tmp-local",
			Label:          "Integration",
			RepositoryFile: fix.RepositoryFile,
			PasswordFile:   fix.PasswordFile,
			CacheDir:       fix.CacheDir,
			AllowedHosts:   []string{host},
			AllowedTags:    []string{"daily", "weekly", "test"},
			AllowedPaths:   []string{fix.DataDir1, fix.DataDir2},
		}},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}

	runner, err := restic.NewExecRunner("")
	if err != nil {
		t.Fatal(err)
	}
	reg, err := repositories.New(cfg, runner, audit.New(os.Stderr))
	if err != nil {
		t.Fatal(err)
	}

	srv := mcpserver.NewServer(mcpserver.Deps{
		Registry:      reg,
		ResticVersion: "integration",
		Audit:         audit.New(os.Stderr),
		ServerVersion: "integration",
	})

	t1, t2 := mcp.NewInMemoryTransports()
	go func() { _ = srv.Run(ctx, t1) }()
	client := mcp.NewClient(&mcp.Implementation{Name: "integration", Version: "1.0"}, nil)
	session, err := client.Connect(ctx, t2, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()

	// tools/list
	var tools []string
	for tool, err := range session.Tools(ctx, nil) {
		if err != nil {
			t.Fatal(err)
		}
		tools = append(tools, tool.Name)
	}
	if len(tools) != 7 {
		t.Fatalf("tools: %v", tools)
	}
	for _, bad := range []string{"backup", "restore", "forget", "prune"} {
		for _, n := range tools {
			if strings.Contains(n, bad) {
				t.Fatalf("mutation tool %s", n)
			}
		}
	}

	call := func(name string, args map[string]any) string {
		t.Helper()
		res, err := session.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		text := toolText(res)
		if res.IsError {
			t.Fatalf("%s error: %s", name, text)
		}
		return text
	}

	caps := call("restic_capabilities", map[string]any{})
	if !strings.Contains(caps, "tmp-local") {
		t.Fatalf("caps: %s", caps)
	}

	repos := call("list_repositories", map[string]any{})
	if strings.Contains(repos, fix.RepoDir) || strings.Contains(repos, "test-password") {
		t.Fatal("leaked path or password")
	}

	snaps := call("list_snapshots", map[string]any{
		"repository_id": "tmp-local",
		"host":          host,
	})
	if !strings.Contains(snaps, `"count"`) {
		t.Fatalf("snaps: %s", snaps)
	}
	var snapEnv struct {
		Data struct {
			Snapshots []struct {
				ID    string   `json:"id"`
				Paths []string `json:"paths"`
			} `json:"snapshots"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(snaps), &snapEnv); err != nil {
		t.Fatal(err)
	}
	if len(snapEnv.Data.Snapshots) < 2 {
		t.Fatalf("want >=2 snapshots, got %d: %s", len(snapEnv.Data.Snapshots), snaps)
	}
	sid := snapEnv.Data.Snapshots[0].ID
	browsePath := dataPath
	if len(snapEnv.Data.Snapshots[0].Paths) > 0 {
		browsePath = snapEnv.Data.Snapshots[0].Paths[0]
	}

	_ = call("get_snapshot", map[string]any{
		"repository_id": "tmp-local",
		"snapshot_id":   sid,
	})

	browse := call("browse_snapshot", map[string]any{
		"repository_id": "tmp-local",
		"snapshot_id":   sid,
		"path":          browsePath,
	})
	if !strings.Contains(browse, "hello.txt") && !strings.Contains(browse, "nodes") && !strings.Contains(browse, "extra.log") {
		t.Fatalf("browse: %s", browse)
	}

	// denied path
	res, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name: "browse_snapshot",
		Arguments: map[string]any{
			"repository_id": "tmp-local",
			"snapshot_id":   sid,
			"path":          "/etc",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError {
		t.Fatal("expected path deny")
	}

	find := call("find_files", map[string]any{
		"repository_id": "tmp-local",
		"pattern":       "*.txt",
		"snapshot_id":   sid,
		"path":          browsePath,
	})
	if !strings.Contains(find, "groups") {
		t.Fatalf("find: %s", find)
	}

	stats := call("repository_stats", map[string]any{
		"repository_id": "tmp-local",
		"mode":          "restore-size",
		"host":          host,
	})
	if !strings.Contains(stats, "total_size") && !strings.Contains(stats, "stats") {
		t.Fatalf("stats: %s", stats)
	}

	// Confirm cache dir may exist (local effect)
	if fi, err := os.Stat(fix.CacheDir); err == nil && !fi.IsDir() {
		t.Fatal("cache_dir should be dir if present")
	}

	t.Logf("integration OK: tools=%v snapshots=%d data=%s", tools, len(snapEnv.Data.Snapshots), dataPath)
}

func toolText(res *mcp.CallToolResult) string {
	var b strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			b.WriteString(tc.Text)
		}
	}
	return b.String()
}
