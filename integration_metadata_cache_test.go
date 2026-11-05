//go:build integration

package main_test

// Failure modes covered by this executable E2E test:
//
//   - Cache hits are not distinguishable from fresh observations.
//   - A cached observation outlives its TTL after password rotation.
//   - Different selectors share a cache entry.
//   - Cache-off behavior becomes meaningless or hides its observation age.

import (
	"context"
	"encoding/json"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/ThomasCrouzet/restic-defensive-mcp/internal/testrepo"
)

type metadataCacheData struct {
	Enabled    bool   `json:"enabled"`
	Hit        bool   `json:"hit"`
	ObservedAt string `json:"observed_at"`
	AgeSeconds int64  `json:"age_seconds"`
	TTLSeconds int64  `json:"ttl_seconds"`
}

type cacheListEnvelope struct {
	Data struct {
		MetadataCache *metadataCacheData `json:"metadata_cache"`
		Total         int                `json:"total"`
	} `json:"data"`
	Warnings []string `json:"warnings"`
}

func TestExecutableMetadataCache(t *testing.T) {
	artifact := newE2EArtifact(t)
	t.Cleanup(func() { artifact.finish(t, "") })

	resticBinary, err := exec.LookPath("restic")
	if err != nil {
		t.Fatalf("integration E2E requires restic: %v", err)
	}
	artifact.environment["restic_binary"] = resticBinary
	artifact.environment["restic_version"] = commandText(t, resticBinary, "version")

	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
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

	offConfig := filepath.Join(fixture.Root, "cache-off.yaml")
	if err := fixture.WriteConfig(offConfig, resticBinary); err != nil {
		t.Fatal(err)
	}
	offSession := connectFixtureServer(t, ctx, binary, offConfig)
	off := callCacheEnvelope(t, ctx, offSession, artifact, map[string]any{"repository_id": "tmp-local"})
	if off.Data.MetadataCache != nil {
		t.Fatalf("cache-off response advertised cache metadata: %+v", off.Data.MetadataCache)
	}
	artifact.check("cache-off responses stay unmarked")

	cacheConfig := filepath.Join(fixture.Root, "cache-on.yaml")
	if err := fixture.WriteConfigWithPolicy(cacheConfig, resticBinary, testrepo.ConfigPolicy{
		AllowedHosts:         []string{fixture.Host},
		AllowedTags:          []string{"daily", "weekly", "test", "missing"},
		AllowedPaths:         []string{fixture.DataDir1, fixture.DataDir2},
		MetadataCacheTTL:     30 * time.Second,
		MetadataCacheMaxSize: 4,
	}); err != nil {
		t.Fatal(err)
	}
	session := connectFixtureServer(t, ctx, binary, cacheConfig)
	first := callCacheEnvelope(t, ctx, session, artifact, map[string]any{"repository_id": "tmp-local"})
	if first.Data.MetadataCache == nil || !first.Data.MetadataCache.Enabled || first.Data.MetadataCache.Hit ||
		first.Data.MetadataCache.TTLSeconds != 30 {
		t.Fatalf("first cache observation = %+v", first.Data.MetadataCache)
	}
	if _, err := time.Parse(time.RFC3339Nano, first.Data.MetadataCache.ObservedAt); err != nil {
		t.Fatalf("observed_at = %q: %v", first.Data.MetadataCache.ObservedAt, err)
	}
	second := callCacheEnvelope(t, ctx, session, artifact, map[string]any{"repository_id": "tmp-local"})
	if second.Data.MetadataCache == nil || !second.Data.MetadataCache.Hit ||
		second.Data.MetadataCache.AgeSeconds < 0 {
		t.Fatalf("second cache observation = %+v", second.Data.MetadataCache)
	}
	if !strings.Contains(strings.Join(second.Warnings, " "), "does not prove current authentication") {
		t.Fatalf("cache hit warning missing: %v", second.Warnings)
	}
	isolated := callCacheEnvelope(t, ctx, session, artifact, map[string]any{
		"repository_id": "tmp-local",
		"tags":          []string{"daily"},
	})
	if isolated.Data.MetadataCache == nil || isolated.Data.MetadataCache.Hit {
		t.Fatalf("different selector reused a cache entry: %+v", isolated.Data.MetadataCache)
	}
	artifact.check("cache hit, age, TTL, warning, and selector isolation")

	// Expiry is observable and a stale hit does not outlive the TTL after
	// password rotation.
	expiryConfig := filepath.Join(fixture.Root, "cache-expiry.yaml")
	if err := fixture.WriteConfigWithPolicy(expiryConfig, resticBinary, testrepo.ConfigPolicy{
		AllowedHosts:         []string{fixture.Host},
		AllowedTags:          []string{"daily", "weekly", "test", "missing"},
		AllowedPaths:         []string{fixture.DataDir1, fixture.DataDir2},
		MetadataCacheTTL:     time.Second,
		MetadataCacheMaxSize: 2,
	}); err != nil {
		t.Fatal(err)
	}
	expirySession := connectFixtureServer(t, ctx, binary, expiryConfig)
	callCacheEnvelope(t, ctx, expirySession, artifact, map[string]any{"repository_id": "tmp-local"})
	if err := fixture.SetPassword("synthetic-cache-rotated-password"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = fixture.RestorePassword() })
	stale := callCacheEnvelope(t, ctx, expirySession, artifact, map[string]any{"repository_id": "tmp-local"})
	if stale.Data.MetadataCache == nil || !stale.Data.MetadataCache.Hit {
		t.Fatalf("expected a stale cache hit before TTL expiry: %+v", stale.Data.MetadataCache)
	}
	time.Sleep(1200 * time.Millisecond)
	expired := callE2EToolError(t, ctx, expirySession, artifact, "list_snapshots", map[string]any{
		"repository_id": "tmp-local",
	})
	if expired["code"] != "authentication_failed" {
		t.Fatalf("expired cache observation did not reauthenticate: %v", expired)
	}
	if err := fixture.RestorePassword(); err != nil {
		t.Fatal(err)
	}
	artifact.check("cached metadata expires and does not mask password rotation")
}

func callCacheEnvelope(
	t *testing.T,
	ctx context.Context,
	session *mcp.ClientSession,
	artifact *e2eArtifact,
	args map[string]any,
) cacheListEnvelope {
	t.Helper()
	result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "list_snapshots", Arguments: args})
	if err != nil {
		t.Fatal(err)
	}
	text := toolText(result)
	artifact.record("list_snapshots", args, result.IsError, text)
	if result.IsError {
		t.Fatalf("list_snapshots: %s", text)
	}
	var cache cacheListEnvelope
	if err := json.Unmarshal([]byte(text), &cache); err != nil {
		t.Fatalf("list_snapshots JSON: %v: %s", err, text)
	}
	return cache
}
