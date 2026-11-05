//go:build integration

package main_test

// Failure modes covered by this executable E2E test:
//
//   - Cancelling an inspection leaves a restic repository lock behind.
//   - Cancellation does not release the repository concurrency slot.
//   - The server reports success or an unrelated error after cancellation.
//
// The test uses a real restic repository and the real MCP executable over
// stdio. A sufficiently large snapshot keeps the raw-data statistics command
// running while the test observes an acquired lock and cancels the call.

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/ThomasCrouzet/restic-defensive-mcp/internal/testrepo"
)

const cancellationBlobBytes = 64 << 20

func TestExecutableCancellationReleasesRepositoryLock(t *testing.T) {
	artifact := newE2EArtifact(t)
	var serverStderr bytes.Buffer
	t.Cleanup(func() { artifact.finish(t, serverStderr.String()) })

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

	blob := make([]byte, cancellationBlobBytes)
	if _, err := rand.Read(blob); err != nil {
		t.Fatal(err)
	}
	if err := fixture.WriteFile(fixture.DataDir2, "cancellation-blob.bin", blob); err != nil {
		t.Fatal(err)
	}
	if err := fixture.AddSnapshot(ctx, fixture.Host, []string{"test"}, fixture.DataDir2); err != nil {
		t.Fatal(err)
	}

	configPath := filepath.Join(fixture.Root, "cancellation.yaml")
	if err := fixture.WriteConfig(configPath, resticBinary); err != nil {
		t.Fatal(err)
	}

	cmd := exec.CommandContext(ctx, binary, "--config", configPath)
	cmd.Env = replaceEnvironment(os.Environ(), nil)
	cmd.Stderr = &serverStderr
	transport := &mcp.CommandTransport{Command: cmd, TerminateDuration: 3 * time.Second}
	client := mcp.NewClient(&mcp.Implementation{Name: "cancellation-e2e", Version: "1.0"}, nil)
	session, err := client.Connect(ctx, transport, nil)
	if err != nil {
		t.Fatalf("connect to executable: %v\nstderr: %s", err, serverStderr.String())
	}
	closed := false
	t.Cleanup(func() {
		if !closed {
			_ = session.Close()
		}
	})
	t.Cleanup(func() {
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			if cmd.ProcessState != nil && cmd.ProcessState.Exited() {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Errorf("server process did not exit: %v\nstderr: %s", cmd.ProcessState, serverStderr.String())
	})

	callCtx, callCancel := context.WithCancel(ctx)
	defer callCancel()
	type callOutcome struct {
		result *mcp.CallToolResult
		err    error
	}
	outcomes := make(chan callOutcome, 1)
	go func() {
		result, err := session.CallTool(callCtx, &mcp.CallToolParams{
			Name: "repository_stats",
			Arguments: map[string]any{
				"repository_id": "tmp-local",
				"mode":          "raw-data",
				"tags":          []string{"test"},
			},
		})
		outcomes <- callOutcome{result: result, err: err}
	}()

	lockPath := filepath.Join(fixture.RepoDir, "locks")
	if err := waitForRepositoryLock(t, lockPath, 30*time.Second); err != nil {
		t.Fatal(err)
	}
	artifact.check("repository lock observed during inspection")

	start := time.Now()
	callCancel()
	select {
	case outcome := <-outcomes:
		if outcome.err != nil {
			if !strings.Contains(strings.ToLower(outcome.err.Error()), "cancel") {
				t.Fatalf("statistics cancellation error = %v, want cancelled", outcome.err)
			}
		} else if outcome.result == nil || !outcome.result.IsError {
			t.Fatalf("statistics call returned success after cancellation: %+v", outcome.result)
		}
	case <-time.After(gracefulStopWindowBound):
		t.Fatal("statistics call did not return after cancellation")
	}
	if elapsed := time.Since(start); elapsed >= gracefulStopWindowBound {
		t.Fatalf("cancellation took %s, want under %s", elapsed, gracefulStopWindowBound)
	}
	artifact.check("cancellation returned a bounded cancelled error")

	if err := waitForRepositoryLockRelease(t, lockPath, 10*time.Second); err != nil {
		t.Fatal(err)
	}
	artifact.check("restic removed its lock during graceful cleanup")

	// The same repository must remain usable, proving the concurrency slot and
	// server state were released after cancellation. The client observes its own
	// cancellation immediately, so allow the server handler to finish cleanup.
	deadline := time.Now().Add(10 * time.Second)
	var snapshots e2eSnapshotPage
	var lastCapacityError string
	for time.Now().Before(deadline) {
		result, callErr := session.CallTool(ctx, &mcp.CallToolParams{
			Name: "list_snapshots",
			Arguments: map[string]any{
				"repository_id": "tmp-local",
			},
		})
		if callErr == nil && result != nil && !result.IsError {
			var envelope e2eEnvelope
			if err := json.Unmarshal([]byte(toolText(result)), &envelope); err == nil {
				if err := json.Unmarshal(envelope.Data, &snapshots); err == nil && snapshots.Total == 3 {
					break
				}
			}
		}
		if result != nil {
			lastCapacityError = toolText(result)
		} else if callErr != nil {
			lastCapacityError = callErr.Error()
		}
		time.Sleep(50 * time.Millisecond)
	}
	if snapshots.Total != 3 {
		t.Fatalf("repository capacity was not released after cancellation: %+v last=%s", snapshots, lastCapacityError)
	}
	artifact.check("repository capacity usable after cancellation")

	if err := session.Close(); err != nil {
		t.Fatalf("close session: %v", err)
	}
	closed = true
	if cmd.ProcessState == nil || !cmd.ProcessState.Exited() || !cmd.ProcessState.Success() {
		t.Fatalf("server process did not exit cleanly: %v", cmd.ProcessState)
	}
	artifact.check("clean shutdown after cancellation")
}

const gracefulStopWindowBound = 5 * time.Second

func waitForRepositoryLock(t *testing.T, lockPath string, timeout time.Duration) error {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		entries, err := os.ReadDir(lockPath)
		if err == nil {
			for _, entry := range entries {
				if !entry.IsDir() {
					return nil
				}
			}
		}
		time.Sleep(2 * time.Millisecond)
	}
	return &lockWaitError{path: lockPath, wantPresent: true}
}

func waitForRepositoryLockRelease(t *testing.T, lockPath string, timeout time.Duration) error {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		entries, err := os.ReadDir(lockPath)
		if err != nil || len(entries) == 0 {
			return nil
		}
		time.Sleep(10 * time.Millisecond)
	}
	return &lockWaitError{path: lockPath, wantPresent: false}
}

type lockWaitError struct {
	path        string
	wantPresent bool
}

func (e *lockWaitError) Error() string {
	if e.wantPresent {
		return "repository lock was not acquired before the deadline"
	}
	return "repository lock was not released before the deadline"
}
