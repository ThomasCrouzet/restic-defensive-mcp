package repositories_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ThomasCrouzet/restic-defensive-mcp/internal/audit"
	"github.com/ThomasCrouzet/restic-defensive-mcp/internal/repositories"
	"github.com/ThomasCrouzet/restic-defensive-mcp/internal/restic"
)

func metadataFixture(t *testing.T, ttl time.Duration, max int) (*repositories.Registry, *restic.FakeRunner) {
	t.Helper()
	cfg, _ := testConfig(t, "/data/repo")
	cfg.Limits.MetadataCacheTTL = ttl
	cfg.Limits.MetadataCacheMaxEntries = max
	fake := restic.NewFakeRunner()
	reg, err := repositories.New(cfg, fake, audit.New(ioDiscard{}))
	if err != nil {
		t.Fatal(err)
	}
	return reg, fake
}

func TestRunMetadataCoalescesIdenticalInFlightQueries(t *testing.T) {
	reg, fake := metadataFixture(t, 0, 0)
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	var calls atomic.Int32
	fake.Handler = func(restic.RunRequest) (*restic.Result, error) {
		calls.Add(1)
		select {
		case started <- struct{}{}:
		default:
		}
		<-release
		return &restic.Result{Stdout: []byte("[]")}, nil
	}
	argv, err := restic.BuildSnapshotsArgv(restic.SnapshotsOpts{})
	if err != nil {
		t.Fatal(err)
	}
	const workers = 6
	var wg sync.WaitGroup
	results := make(chan error, workers)
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			result, observation, err := reg.RunMetadata(context.Background(), "local-test", argv)
			if err != nil {
				results <- err
				return
			}
			if result == nil || observation.Enabled {
				results <- errInvalidMetadataResult
				return
			}
			results <- nil
		}()
	}
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("metadata query did not start")
	}
	time.Sleep(50 * time.Millisecond)
	close(release)
	wg.Wait()
	close(results)
	for err := range results {
		if err != nil {
			t.Fatal(err)
		}
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("backend calls = %d, want 1 coalesced query", got)
	}
}

func TestRunMetadataCancellationDoesNotCancelOtherCallers(t *testing.T) {
	reg, fake := metadataFixture(t, 0, 0)
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	fake.Handler = func(restic.RunRequest) (*restic.Result, error) {
		select {
		case started <- struct{}{}:
		default:
		}
		<-release
		return &restic.Result{Stdout: []byte("[]")}, nil
	}
	argv, err := restic.BuildSnapshotsArgv(restic.SnapshotsOpts{})
	if err != nil {
		t.Fatal(err)
	}
	leaderDone := make(chan error, 1)
	go func() {
		_, _, err := reg.RunMetadata(context.Background(), "local-test", argv)
		leaderDone <- err
	}()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("metadata query did not start")
	}
	waiterCtx, cancelWaiter := context.WithCancel(context.Background())
	waiterDone := make(chan error, 1)
	go func() {
		_, _, err := reg.RunMetadata(waiterCtx, "local-test", argv)
		waiterDone <- err
	}()
	time.Sleep(20 * time.Millisecond)
	cancelWaiter()
	if err := <-waiterDone; err == nil {
		t.Fatal("cancelled waiter returned success")
	}
	close(release)
	if err := <-leaderDone; err != nil {
		t.Fatalf("leader was cancelled by another caller: %v", err)
	}
}

func TestRunMetadataCacheHitExpiryAndIsolation(t *testing.T) {
	reg, fake := metadataFixture(t, 1*time.Second, 2)
	argvEmpty, err := restic.BuildSnapshotsArgv(restic.SnapshotsOpts{})
	if err != nil {
		t.Fatal(err)
	}
	argvHost, err := restic.BuildSnapshotsArgv(restic.SnapshotsOpts{Hosts: []string{"testhost"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, first, err := reg.RunMetadata(context.Background(), "local-test", argvEmpty); err != nil || first.Hit || !first.Enabled {
		t.Fatalf("first observation = %+v err=%v", first, err)
	}
	if _, second, err := reg.RunMetadata(context.Background(), "local-test", argvEmpty); err != nil || !second.Hit {
		t.Fatalf("second observation = %+v err=%v", second, err)
	}
	if _, other, err := reg.RunMetadata(context.Background(), "local-test", argvHost); err != nil || other.Hit {
		t.Fatalf("different selector observation = %+v err=%v", other, err)
	}
	time.Sleep(1100 * time.Millisecond)
	if _, expired, err := reg.RunMetadata(context.Background(), "local-test", argvEmpty); err != nil || expired.Hit {
		t.Fatalf("expired observation = %+v err=%v", expired, err)
	}
	if got := len(fake.Calls); got != 3 {
		t.Fatalf("backend calls = %d, want 3 (miss, selector miss, expiry miss)", got)
	}
}

func TestRunMetadataCacheOffStillCoalescesSequentially(t *testing.T) {
	reg, fake := metadataFixture(t, 0, 0)
	argv, err := restic.BuildSnapshotsArgv(restic.SnapshotsOpts{})
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if _, observation, err := reg.RunMetadata(context.Background(), "local-test", argv); err != nil || observation.Enabled {
			t.Fatalf("observation = %+v err=%v", observation, err)
		}
	}
	if got := len(fake.Calls); got != 2 {
		t.Fatalf("cache-off backend calls = %d, want 2", got)
	}
}

var errInvalidMetadataResult = errors.New("invalid metadata result")
