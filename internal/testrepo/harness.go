// Package testrepo prepares temporary local restic repositories for integration tests.
// It is the ONLY package allowed to run restic init/backup. Production code must not
// import this package's mutation helpers into the MCP server binary path except via tests.
package testrepo

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
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
	binary         string
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
	if err := os.WriteFile(f.PasswordFile, []byte("test-password-not-secret\n"), 0o600); err != nil {
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

func (f *Fixture) run(ctx context.Context, args ...string) error {
	cmd := exec.CommandContext(ctx, f.binary, args...)
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + f.Root,
		"RESTIC_REPOSITORY_FILE=" + f.RepositoryFile,
		"RESTIC_PASSWORD_FILE=" + f.PasswordFile,
		"RESTIC_CACHE_DIR=" + f.CacheDir,
		"LANG=C",
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	cmd.Stdout = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%v: %s", err, stderr.String())
	}
	return nil
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
