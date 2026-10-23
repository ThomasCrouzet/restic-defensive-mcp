package repositories_test

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ThomasCrouzet/restic-defensive-mcp/internal/audit"
	"github.com/ThomasCrouzet/restic-defensive-mcp/internal/config"
	apperr "github.com/ThomasCrouzet/restic-defensive-mcp/internal/errors"
	"github.com/ThomasCrouzet/restic-defensive-mcp/internal/repositories"
	"github.com/ThomasCrouzet/restic-defensive-mcp/internal/restic"
)

func TestRegistryNewSealsAndListsPublicInfo(t *testing.T) {
	cfg, _ := testConfig(t, "/tmp/sealed-repo-location")
	fake := restic.NewFakeRunner()
	reg, err := repositories.New(cfg, fake, audit.New(ioDiscard{}))
	if err != nil {
		t.Fatal(err)
	}
	ids := reg.IDs()
	if len(ids) != 1 || ids[0] != "local-test" {
		t.Fatalf("ids: %v", ids)
	}
	list := reg.List()
	if len(list) != 1 {
		t.Fatalf("list: %+v", list)
	}
	if list[0].Backend != "local" {
		t.Fatalf("backend: %s", list[0].Backend)
	}
	raw, err := json.Marshal(list)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "/tmp/sealed-repo-location") || strings.Contains(string(raw), "test-pass") {
		t.Fatalf("leaked secret material: %s", raw)
	}
	if _, err := reg.Get("nope"); !apperr.IsCode(err, apperr.RepositoryNotFound) {
		t.Fatalf("want repository_not_found, got %v", err)
	}
}

func TestRegistryRunAuditsAndUsesChildEnv(t *testing.T) {
	cfg, _ := testConfig(t, "/data/repo")
	var auditBuf bytes.Buffer
	fake := restic.NewFakeRunner()
	fake.Handler = func(req restic.RunRequest) (*restic.Result, error) {
		joined := strings.Join(req.Env, "\n")
		if !strings.Contains(joined, "RESTIC_REPOSITORY=/data/repo") {
			t.Fatalf("missing sealed repository location in env: %v", req.Env)
		}
		if strings.Contains(joined, "RESTIC_REPOSITORY_FILE=") {
			t.Fatalf("mutable repository file leaked into child env: %v", req.Env)
		}
		for _, e := range req.Env {
			if strings.HasPrefix(e, "RESTIC_PASSWORD=") {
				t.Fatal("inline password env forbidden")
			}
		}
		if req.Argv[0] != "snapshots" {
			t.Fatalf("argv: %v", req.Argv)
		}
		return &restic.Result{Stdout: []byte("[]"), ExitCode: 0}, nil
	}
	reg, err := repositories.New(cfg, fake, audit.New(&auditBuf))
	if err != nil {
		t.Fatal(err)
	}
	argv, err := restic.BuildSnapshotsArgv(restic.SnapshotsOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = reg.Run(context.Background(), "local-test", argv, false); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(auditBuf.String(), `"action":"restic_run"`) {
		t.Fatalf("audit missing restic_run: %s", auditBuf.String())
	}
	if !strings.Contains(auditBuf.String(), `"status":"ok"`) {
		t.Fatalf("audit status: %s", auditBuf.String())
	}
}

func TestRegistryRepositoryLocationIsSealedAtBoot(t *testing.T) {
	cfg, _ := testConfig(t, "/data/original")
	fake := restic.NewFakeRunner()
	fake.Handler = func(req restic.RunRequest) (*restic.Result, error) {
		joined := strings.Join(req.Env, "\n")
		if !strings.Contains(joined, "RESTIC_REPOSITORY=/data/original") {
			t.Fatalf("repository location changed after boot: %v", req.Env)
		}
		if strings.Contains(joined, "sftp:") {
			t.Fatalf("post-boot repository retarget reached child env: %v", req.Env)
		}
		return &restic.Result{Stdout: []byte("[]"), ExitCode: 0}, nil
	}
	reg, err := repositories.New(cfg, fake, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfg.Repositories[0].RepositoryFile, []byte("sftp:user@host:/repo\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	argv, err := restic.BuildSnapshotsArgv(restic.SnapshotsOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reg.Run(context.Background(), "local-test", argv, false); err != nil {
		t.Fatal(err)
	}
}

func TestRegistryRejectsForbiddenArgv(t *testing.T) {
	cfg, _ := testConfig(t, "/data/repo")
	reg, err := repositories.New(cfg, restic.NewFakeRunner(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = reg.Run(context.Background(), "local-test", []string{"backup"}, false); err == nil {
		t.Fatal("expected forbidden argv rejection")
	}
}

func TestRegistryRejectsUnsupportedBackend(t *testing.T) {
	cfg, _ := testConfig(t, "sftp:user@host:/repo")
	_, err := repositories.New(cfg, restic.NewFakeRunner(), nil)
	if err == nil || !apperr.IsCode(err, apperr.UnsupportedBackend) {
		t.Fatalf("want unsupported_backend, got %v", err)
	}
}

func TestRegistryRequiresStaticS3Credentials(t *testing.T) {
	tests := []struct {
		name    string
		envFile string
		wantErr bool
	}{
		{name: "missing", wantErr: true},
		{name: "access key only", envFile: "AWS_ACCESS_KEY_ID=test-id\n", wantErr: true},
		{name: "secret key only", envFile: "AWS_SECRET_ACCESS_KEY=test-key\n", wantErr: true},
		{name: "empty access key", envFile: "AWS_ACCESS_KEY_ID=\nAWS_SECRET_ACCESS_KEY=test-key\n", wantErr: true},
		{name: "empty secret key", envFile: "AWS_ACCESS_KEY_ID=test-id\nAWS_SECRET_ACCESS_KEY=\n", wantErr: true},
		{
			name:    "static pair",
			envFile: "AWS_ACCESS_KEY_ID=test-id\nAWS_SECRET_ACCESS_KEY=test-key\n",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, dir := testConfig(t, "s3:s3.example.test/bucket")
			if tt.envFile != "" {
				envPath := filepath.Join(dir, "backend.env")
				if err := os.WriteFile(envPath, []byte(tt.envFile), 0o600); err != nil {
					t.Fatal(err)
				}
				cfg.Repositories[0].EnvFiles = []string{envPath}
			}
			if err := cfg.Validate(); err != nil {
				t.Fatal(err)
			}

			_, err := repositories.New(cfg, restic.NewFakeRunner(), nil)
			if tt.wantErr {
				if !apperr.IsCode(err, apperr.ConfigError) {
					t.Fatalf("want config_error, got %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("valid static credential pair was rejected: %v", err)
			}
		})
	}
}

func TestRegistryRunRejectsRepositorySaturationAndReusesSlot(t *testing.T) {
	cfg, _ := testConfig(t, "/data/repo")
	started := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseRunner := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(releaseRunner)
	var calls atomic.Int32
	fake := restic.NewFakeRunner()
	fake.Handler = func(restic.RunRequest) (*restic.Result, error) {
		if calls.Add(1) == 1 {
			close(started)
			<-release
		}
		return &restic.Result{Stdout: []byte("[]"), ExitCode: 0}, nil
	}
	var auditBuf bytes.Buffer
	reg, err := repositories.New(cfg, fake, audit.New(&auditBuf))
	if err != nil {
		t.Fatal(err)
	}
	argv, err := restic.BuildSnapshotsArgv(restic.SnapshotsOpts{})
	if err != nil {
		t.Fatal(err)
	}

	firstDone := make(chan error, 1)
	go func() {
		_, runErr := reg.Run(context.Background(), "local-test", argv, false)
		firstDone <- runErr
	}()
	<-started

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := reg.Run(ctx, "local-test", argv, false); errorCode(err) != "resource_busy" {
		t.Fatalf("want resource_busy, got %v", err)
	}
	if !strings.Contains(auditBuf.String(), `"error_code":"resource_busy"`) ||
		!strings.Contains(auditBuf.String(), `"status":"error"`) {
		t.Fatalf("busy run was not audited as an error: %s", auditBuf.String())
	}

	releaseRunner()
	if err := <-firstDone; err != nil {
		t.Fatal(err)
	}
	if _, err := reg.Run(context.Background(), "local-test", argv, false); err != nil {
		t.Fatalf("repository slot was not reusable: %v", err)
	}
}

func TestRegistryRunRejectsGlobalSaturationAndReleasesRepositorySlot(t *testing.T) {
	cfg, _ := testConfig(t, "/data/repo")
	second := cfg.Repositories[0]
	second.ID = "local-two"
	cfg.Repositories = append(cfg.Repositories, second)
	cfg.Limits.MaxConcurrentGlobal = 1
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}

	started := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseRunner := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(releaseRunner)
	var calls atomic.Int32
	fake := restic.NewFakeRunner()
	fake.Handler = func(restic.RunRequest) (*restic.Result, error) {
		if calls.Add(1) == 1 {
			close(started)
			<-release
		}
		return &restic.Result{Stdout: []byte("[]"), ExitCode: 0}, nil
	}
	reg, err := repositories.New(cfg, fake, nil)
	if err != nil {
		t.Fatal(err)
	}
	argv, err := restic.BuildSnapshotsArgv(restic.SnapshotsOpts{})
	if err != nil {
		t.Fatal(err)
	}

	firstDone := make(chan error, 1)
	go func() {
		_, runErr := reg.Run(context.Background(), "local-test", argv, false)
		firstDone <- runErr
	}()
	<-started
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := reg.Run(ctx, "local-two", argv, false); errorCode(err) != "resource_busy" {
		t.Fatalf("want resource_busy, got %v", err)
	}

	releaseRunner()
	if err := <-firstDone; err != nil {
		t.Fatal(err)
	}
	if _, err := reg.Run(context.Background(), "local-two", argv, false); err != nil {
		t.Fatalf("repository slot leaked after global saturation: %v", err)
	}
}

func TestRegistryRunCancelledContextDoesNotConsumeSlot(t *testing.T) {
	cfg, _ := testConfig(t, "/data/repo")
	fake := restic.NewFakeRunner()
	fake.Handler = func(restic.RunRequest) (*restic.Result, error) {
		return &restic.Result{Stdout: []byte("[]"), ExitCode: 0}, nil
	}
	reg, err := repositories.New(cfg, fake, nil)
	if err != nil {
		t.Fatal(err)
	}
	argv, err := restic.BuildSnapshotsArgv(restic.SnapshotsOpts{})
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := reg.Run(ctx, "local-test", argv, false); !apperr.IsCode(err, apperr.Cancelled) {
		t.Fatalf("want cancelled, got %v", err)
	}
	if fake.LastCall() != nil {
		t.Fatal("cancelled request reached the runner")
	}
	if _, err := reg.Run(context.Background(), "local-test", argv, false); err != nil {
		t.Fatalf("cancelled request consumed a slot: %v", err)
	}
}

func errorCode(err error) string {
	ae, ok := apperr.As(err)
	if !ok {
		return ""
	}
	return string(ae.Code)
}

func testConfig(t *testing.T, repoLocation string) (*config.Config, string) {
	t.Helper()
	dir := t.TempDir()
	repoFile := filepath.Join(dir, "repo")
	passFile := filepath.Join(dir, "pass")
	if err := os.WriteFile(repoFile, []byte(repoLocation+"\n"), 0o600); err != nil {
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
			MaxSnapshots:            50,
			MaxNodes:                100,
			MaxFindResults:          50,
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
			AllowedTags:    []string{"daily"},
			AllowedPaths:   []string{"/data"},
		}},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	return cfg, dir
}

type ioDiscard struct{}

func (ioDiscard) Write(p []byte) (int, error) { return len(p), nil }
