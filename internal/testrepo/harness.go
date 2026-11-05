// Package testrepo prepares temporary local restic repositories for integration tests.
// It is the ONLY package allowed to run restic init/backup. Production code must not
// import this package's mutation helpers into the MCP server binary path except via tests.
package testrepo

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Fixture is a temporary restic repository with sample data.
type Fixture struct {
	Root           string
	RepoDir        string
	PasswordFile   string
	RepositoryFile string
	CacheDir       string
	DataDir1       string
	DataDir2       string
	Host           string
	password       string
	binary         string
}

// Description records the stable fixture recipe without secret values.
type Description struct {
	Host    string              `json:"host"`
	Backups []BackupDescription `json:"backups"`
}

// BackupDescription describes one test backup.
type BackupDescription struct {
	Root  string   `json:"root"`
	Files []string `json:"files"`
	Tags  []string `json:"tags"`
}

// S3Credentials defines synthetic static credentials for startup checks.
type S3Credentials struct {
	AccessKeyID     *string
	SecretAccessKey *string
}

// Create builds a temp dir, two data trees, inits a repo, and creates two snapshots.
func Create(ctx context.Context, resticBinary string) (*Fixture, error) {
	if resticBinary == "" {
		var err error
		resticBinary, err = exec.LookPath("restic")
		if err != nil {
			return nil, fmt.Errorf("restic not found: %w", err)
		}
	}
	root, err := os.MkdirTemp("", "restic-defensive-mcp-*")
	if err != nil {
		return nil, err
	}
	f := &Fixture{Root: root, binary: resticBinary}
	f.RepoDir = filepath.Join(root, "repo")
	f.CacheDir = filepath.Join(root, "cache")
	f.DataDir1 = filepath.Join(root, "data1")
	f.DataDir2 = filepath.Join(root, "data2")
	f.PasswordFile = filepath.Join(root, "password")
	f.RepositoryFile = filepath.Join(root, "repository")

	for _, d := range []string{f.RepoDir, f.CacheDir, f.DataDir1, f.DataDir2} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			_ = os.RemoveAll(root)
			return nil, err
		}
	}
	f.password = "test-password-not-secret"
	if err := f.SetPassword(f.password); err != nil {
		_ = f.Close()
		return nil, err
	}
	if err := os.WriteFile(f.RepositoryFile, []byte(f.RepoDir+"\n"), 0o600); err != nil {
		_ = f.Close()
		return nil, err
	}

	// Tree 1
	if err := os.WriteFile(filepath.Join(f.DataDir1, "hello.txt"), []byte("hello world\n"), 0o600); err != nil {
		_ = f.Close()
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(f.DataDir1, "with space.txt"), []byte("spaced name\n"), 0o600); err != nil {
		_ = f.Close()
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(f.DataDir1, "café.txt"), []byte("unicode name\n"), 0o600); err != nil {
		_ = f.Close()
		return nil, err
	}
	if err := os.MkdirAll(filepath.Join(f.DataDir1, "subdir"), 0o700); err != nil {
		_ = f.Close()
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(f.DataDir1, "subdir", "note.md"), []byte("# note\n"), 0o600); err != nil {
		_ = f.Close()
		return nil, err
	}
	// Tree 2 (second snapshot will add a file)
	if err := os.WriteFile(filepath.Join(f.DataDir2, "hello.txt"), []byte("hello world\n"), 0o600); err != nil {
		_ = f.Close()
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(f.DataDir2, "with space.txt"), []byte("spaced name\n"), 0o600); err != nil {
		_ = f.Close()
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(f.DataDir2, "café.txt"), []byte("unicode name\n"), 0o600); err != nil {
		_ = f.Close()
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(f.DataDir2, "extra.log"), []byte("log line\n"), 0o600); err != nil {
		_ = f.Close()
		return nil, err
	}

	if err := f.run(ctx, "init"); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("init: %w", err)
	}
	hostname, _ := os.Hostname()
	if hostname == "" {
		hostname = "testhost"
	}
	f.Host = hostname
	if err := f.run(ctx, "backup", "--host", hostname, "--tag", "daily", "--tag", "test", f.DataDir1); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("backup1: %w", err)
	}
	// Small delay so times differ
	time.Sleep(10 * time.Millisecond)
	if err := f.run(ctx, "backup", "--host", hostname, "--tag", "weekly", "--tag", "test", f.DataDir2); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("backup2: %w", err)
	}
	return f, nil
}

// WriteConfig writes a complete server configuration for this fixture.
func (f *Fixture) WriteConfig(path, resticBinary string) error {
	return f.WriteConfigForPaths(path, resticBinary, []string{f.DataDir1, f.DataDir2})
}

// WriteConfigForPaths writes a fixture configuration with explicit allowed paths.
// Hosts and tags keep the fixture defaults; callers use this to exercise path
// policy intersections without changing repository contents.
func (f *Fixture) WriteConfigForPaths(path, resticBinary string, allowedPaths []string) error {
	return f.writeConfig(path, resticBinary, f.RepositoryFile, "", allowedPaths)
}

// WriteConfigWithBlockedEnvKey writes a config that must fail during startup.
func (f *Fixture) WriteConfigWithBlockedEnvKey(path, resticBinary, key string) error {
	envPath := filepath.Join(f.Root, "blocked-env-"+key)
	value := "synthetic-profile"
	if key == "AWS_SHARED_CREDENTIALS_FILE" {
		credentialsPath := filepath.Join(f.Root, "synthetic-credentials")
		credentials := "[default]\ncredential_process = /usr/bin/false\n"
		if err := os.WriteFile(credentialsPath, []byte(credentials), 0o600); err != nil {
			return err
		}
		value = credentialsPath
	}
	if err := os.WriteFile(envPath, []byte(key+"="+value+"\n"), 0o600); err != nil {
		return err
	}
	return f.writeConfig(path, resticBinary, f.RepositoryFile, envPath, []string{f.DataDir1, f.DataDir2})
}

// WriteS3Config writes a synthetic S3 configuration without contacting a backend.
func (f *Fixture) WriteS3Config(path, resticBinary string, credentials S3Credentials) error {
	repositoryPath := filepath.Join(f.Root, "synthetic-s3-repository")
	if err := os.WriteFile(repositoryPath, []byte("s3:https://s3.example.invalid/test-bucket\n"), 0o600); err != nil {
		return err
	}
	var entries []string
	if credentials.AccessKeyID != nil {
		entries = append(entries, "AWS_ACCESS_KEY_ID="+*credentials.AccessKeyID)
	}
	if credentials.SecretAccessKey != nil {
		entries = append(entries, "AWS_SECRET_ACCESS_KEY="+*credentials.SecretAccessKey)
	}
	envPath := ""
	if len(entries) > 0 {
		sort.Strings(entries)
		envPath = filepath.Join(f.Root, "synthetic-s3-env")
		if err := os.WriteFile(envPath, []byte(strings.Join(entries, "\n")+"\n"), 0o600); err != nil {
			return err
		}
	}
	return f.writeConfig(path, resticBinary, repositoryPath, envPath, []string{f.DataDir1, f.DataDir2})
}

func (f *Fixture) writeConfig(path, resticBinary, repositoryPath, envPath string, allowedPaths []string) error {
	envFiles := ""
	if envPath != "" {
		envFiles = fmt.Sprintf("    env_files: [%q]\n", envPath)
	}
	quotedPaths := make([]string, 0, len(allowedPaths))
	for _, allowed := range allowedPaths {
		quotedPaths = append(quotedPaths, strconv.Quote(allowed))
	}
	content := fmt.Sprintf(`version: 1
restic_binary: %q
limits:
  command_timeout: 60s
  expensive_command_timeout: 2m
  max_snapshots: 50
  max_nodes: 200
  max_find_results: 100
  max_output_bytes: 4194304
  max_concurrent_per_repo: 1
  max_concurrent_global: 2
repositories:
  - id: tmp-local
    label: Integration
    repository_file: %q
    password_file: %q
    cache_dir: %q
    allowed_hosts: [%q]
    allowed_tags: [daily, weekly, test, missing]
    allowed_paths: [%s]
%s`, resticBinary, repositoryPath, f.PasswordFile, f.CacheDir, f.Host, strings.Join(quotedPaths, ", "), envFiles)
	return os.WriteFile(path, []byte(content), 0o600)
}

// WriteInvalidConfig writes malformed YAML for executable startup checks.
func WriteInvalidConfig(path string) error {
	return os.WriteFile(path, []byte("version: [\n"), 0o600)
}

// WriteFile writes one file under a fixture data directory.
func (f *Fixture) WriteFile(dataDir, relative string, contents []byte) error {
	cleaned := filepath.Clean(relative)
	if cleaned == "." || strings.HasPrefix(cleaned, ".."+string(os.PathSeparator)) || cleaned == ".." || filepath.IsAbs(cleaned) {
		return fmt.Errorf("fixture file path must stay inside %s", dataDir)
	}
	path := filepath.Join(dataDir, cleaned)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return os.WriteFile(path, contents, 0o600)
}

// AddSnapshot creates one additional backup of the named fixture paths.
func (f *Fixture) AddSnapshot(ctx context.Context, host string, tags []string, paths ...string) error {
	args := []string{"backup"}
	if host != "" {
		args = append(args, "--host", host)
	}
	for _, tag := range tags {
		args = append(args, "--tag", tag)
	}
	args = append(args, paths...)
	return f.run(ctx, args...)
}

// SetRepositoryLocation replaces the repository location source file.
func (f *Fixture) SetRepositoryLocation(location string) error {
	return os.WriteFile(f.RepositoryFile, []byte(location+"\n"), 0o600)
}

// SetPassword replaces the synthetic repository password file.
func (f *Fixture) SetPassword(password string) error {
	return os.WriteFile(f.PasswordFile, []byte(password+"\n"), 0o600)
}

// RestorePassword restores the password used to create the fixture.
func (f *Fixture) RestorePassword() error {
	return f.SetPassword(f.password)
}

// SnapshotIDs returns the current snapshot IDs in sorted order.
func (f *Fixture) SnapshotIDs(ctx context.Context) ([]string, error) {
	cmd := exec.CommandContext(ctx, f.binary, "snapshots", "--json")
	cmd.Env = f.env()
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("snapshots: %v: %s", err, stderr.String())
	}
	var snapshots []struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &snapshots); err != nil {
		return nil, fmt.Errorf("parse snapshots: %w", err)
	}
	ids := make([]string, 0, len(snapshots))
	for _, snapshot := range snapshots {
		if snapshot.ID == "" {
			return nil, fmt.Errorf("parse snapshots: empty snapshot id")
		}
		ids = append(ids, snapshot.ID)
	}
	sort.Strings(ids)
	return ids, nil
}

// Describe returns the stable fixture recipe.
func (f *Fixture) Describe() Description {
	return Description{Host: f.Host, Backups: []BackupDescription{
		{
			Root:  "data1",
			Files: []string{"café.txt", "hello.txt", "subdir/note.md", "with space.txt"},
			Tags:  []string{"daily", "test"},
		},
		{
			Root:  "data2",
			Files: []string{"café.txt", "extra.log", "hello.txt", "with space.txt"},
			Tags:  []string{"weekly", "test"},
		},
	}}
}

func (f *Fixture) run(ctx context.Context, args ...string) error {
	cmd := exec.CommandContext(ctx, f.binary, args...)
	cmd.Env = f.env()
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	cmd.Stdout = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%v: %s", err, stderr.String())
	}
	return nil
}

func (f *Fixture) env() []string {
	return []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + f.Root,
		"RESTIC_REPOSITORY_FILE=" + f.RepositoryFile,
		"RESTIC_PASSWORD_FILE=" + f.PasswordFile,
		"RESTIC_CACHE_DIR=" + f.CacheDir,
		"LANG=C",
	}
}

// Close removes the temporary directory.
func (f *Fixture) Close() error {
	if f == nil || f.Root == "" {
		return nil
	}
	err := os.RemoveAll(f.Root)
	f.Root = ""
	return err
}
