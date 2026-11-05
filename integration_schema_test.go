//go:build integration

package main_test

// Failure modes covered by this executable E2E test:
//
//   - tools/list omits output schemas or advertises an unusable shape.
//   - Successful responses differ from their advertised typed contract.
//   - Structured content disagrees with the text compatibility representation.
//   - Framework-level input validation is confused with handler error envelopes.

import (
	"context"
	"encoding/json"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/ThomasCrouzet/restic-defensive-mcp/internal/testrepo"
)

func TestExecutableTypedOutputSchemas(t *testing.T) {
	artifact := newE2EArtifact(t)
	t.Cleanup(func() { artifact.finish(t, "") })

	resticBinary, err := exec.LookPath("restic")
	if err != nil {
		t.Fatalf("integration E2E requires restic: %v", err)
	}
	artifact.environment["restic_binary"] = resticBinary
	artifact.environment["restic_version"] = commandText(t, resticBinary, "version")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
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

	configPath := filepath.Join(fixture.Root, "schemas.yaml")
	if err := fixture.WriteConfig(configPath, resticBinary); err != nil {
		t.Fatal(err)
	}
	session := connectFixtureServer(t, ctx, binary, configPath)

	seenSchemas := make(map[string]bool)
	for tool, err := range session.Tools(ctx, nil) {
		if err != nil {
			t.Fatal(err)
		}
		schema, ok := tool.OutputSchema.(map[string]any)
		if !ok || len(schema) == 0 {
			t.Fatalf("%s output schema = %#v", tool.Name, tool.OutputSchema)
		}
		properties, ok := schema["properties"].(map[string]any)
		if !ok {
			t.Fatalf("%s output schema has no properties: %#v", tool.Name, schema)
		}
		for _, required := range []string{"cost", "data"} {
			if _, ok := properties[required]; !ok {
				t.Fatalf("%s output schema omits %q: %#v", tool.Name, required, schema)
			}
		}
		seenSchemas[tool.Name] = true
	}
	for _, name := range expectedToolNames {
		if !seenSchemas[name] {
			t.Fatalf("tools/list has no output schema for %s", name)
		}
	}
	artifact.check("all seven tools advertise typed output schemas")

	page := callSnapshotPage(t, ctx, session, artifact, map[string]any{"repository_id": "tmp-local"})
	if len(page.Snapshots) == 0 {
		t.Fatal("fixture has no snapshots")
	}
	snapshot := page.Snapshots[0]
	if len(snapshot.Paths) == 0 {
		t.Fatalf("snapshot has no paths: %+v", snapshot)
	}

	calls := []struct {
		name string
		args map[string]any
	}{
		{name: "restic_capabilities", args: map[string]any{}},
		{name: "list_repositories", args: map[string]any{}},
		{name: "list_snapshots", args: map[string]any{"repository_id": "tmp-local"}},
		{name: "get_snapshot", args: map[string]any{"repository_id": "tmp-local", "snapshot_id": snapshot.ID}},
		{name: "browse_snapshot", args: map[string]any{
			"repository_id": "tmp-local", "snapshot_id": snapshot.ID, "path": snapshot.Paths[0], "limit": 2,
		}},
		{name: "find_files", args: map[string]any{
			"repository_id": "tmp-local", "pattern": "*.txt", "snapshot_id": snapshot.ID, "path": snapshot.Paths[0],
		}},
		{name: "repository_stats", args: map[string]any{
			"repository_id": "tmp-local", "mode": "restore-size", "snapshot_id": snapshot.ID,
		}},
	}
	for _, call := range calls {
		result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: call.name, Arguments: call.args})
		if err != nil {
			t.Fatalf("%s: %v", call.name, err)
		}
		if result.IsError {
			t.Fatalf("%s failed: %s", call.name, toolText(result))
		}
		if result.StructuredContent == nil {
			t.Fatalf("%s has no structured content: %s", call.name, toolText(result))
		}
		structuredRaw, err := json.Marshal(result.StructuredContent)
		if err != nil {
			t.Fatalf("%s structured content: %v", call.name, err)
		}
		var structured map[string]any
		if err := json.Unmarshal(structuredRaw, &structured); err != nil {
			t.Fatalf("%s structured content: %v", call.name, err)
		}
		var text map[string]any
		if err := json.Unmarshal([]byte(toolText(result)), &text); err != nil {
			t.Fatalf("%s text content: %v", call.name, err)
		}
		if !sameJSONValue(structured, text) {
			t.Fatalf("%s structured content differs from text compatibility JSON:\nstructured=%v\ntext=%v", call.name, structured, text)
		}
		if _, ok := structured["data"]; !ok {
			t.Fatalf("%s structured content has no data field: %v", call.name, structured)
		}
	}
	artifact.check("successful responses conform to structured and text contracts")

	// Framework-level validation rejects wrong JSON types before a handler runs.
	invalid, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name: "list_snapshots",
		Arguments: map[string]any{
			"repository_id": "tmp-local",
			"limit":         "not-an-integer",
		},
	})
	if err != nil {
		t.Fatalf("framework validation returned a transport error: %v", err)
	}
	if !invalid.IsError || invalid.StructuredContent != nil {
		t.Fatalf("framework validation result = %+v", invalid)
	}
	artifact.check("framework input validation is distinct from handler errors")
}

func sameJSONValue(left, right any) bool {
	leftJSON, leftErr := json.Marshal(left)
	rightJSON, rightErr := json.Marshal(right)
	return leftErr == nil && rightErr == nil && string(leftJSON) == string(rightJSON)
}
