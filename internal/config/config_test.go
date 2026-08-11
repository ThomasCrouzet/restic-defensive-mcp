package config

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestParseValid(t *testing.T) {
	dir := t.TempDir()
	repoFile := filepath.Join(dir, "repo")
	passFile := filepath.Join(dir, "pass")
	mustWrite(t, repoFile, "/tmp/fake-repo\n", 0o600)
	mustWrite(t, passFile, "secret\n", 0o600)

	yaml := `
version: 1
limits:
  command_timeout: 20s
  expensive_command_timeout: 2m
  max_snapshots: 50
repositories:
  - id: local-test
    repository_file: ` + repoFile + `
    password_file: ` + passFile + `
    allowed_hosts: [host1]
    allowed_tags: [daily]
    allowed_paths: [/data]
`
	cfg, err := Parse([]byte(yaml))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Limits.CommandTimeout != 20*time.Second {
		t.Fatalf("timeout: %v", cfg.Limits.CommandTimeout)
	}
	if len(cfg.Repositories) != 1 || cfg.Repositories[0].ID != "local-test" {
		t.Fatalf("repos: %+v", cfg.Repositories)
	}
	if cfg.Repositories[0].AllowedPaths[0] != "/data" {
		t.Fatalf("paths: %v", cfg.Repositories[0].AllowedPaths)
	}
}

func TestRejectBadVersion(t *testing.T) {
	_, err := Parse([]byte(`version: 99
repositories:
  - id: x
    repository_file: /a
    password_file: /b
`))
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestRejectDuplicateID(t *testing.T) {
	dir := t.TempDir()
	rf := filepath.Join(dir, "r")
	pf := filepath.Join(dir, "p")
	mustWrite(t, rf, "/tmp/r\n", 0o600)
	mustWrite(t, pf, "p\n", 0o600)
	yaml := `
version: 1
repositories:
  - id: same
    repository_file: ` + rf + `
    password_file: ` + pf + `
  - id: same
    repository_file: ` + rf + `
    password_file: ` + pf + `
`
	_, err := Parse([]byte(yaml))
	if err == nil {
		t.Fatal("expected duplicate id error")
	}
}

func TestRejectRelativeSecretPath(t *testing.T) {
	yaml := `
version: 1
repositories:
  - id: bad
    repository_file: relative/path
    password_file: /abs/pass
`
	_, err := Parse([]byte(yaml))
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestRejectOversizedRepositoryLists(t *testing.T) {
	base := Repository{
		ID:             "too-large",
		RepositoryFile: "/abs/repository",
		PasswordFile:   "/abs/password",
	}
	cfg := &Config{Version: 1, Repositories: []Repository{base}}
	cfg.Repositories[0].AllowedTags = make([]string, maxAllowlistEntries+1)
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected oversized allowlist rejection")
	}

	cfg = &Config{Version: 1, Repositories: []Repository{base}}
	cfg.Repositories[0].EnvFiles = make([]string, maxEnvFiles+1)
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected oversized env_files rejection")
	}
}

func TestSecretPathParentSegmentValidation(t *testing.T) {
	if err := validateSecretFilePath(filepath.Join(t.TempDir(), "file..name"), "test"); err != nil {
		t.Fatalf("dots inside a file name must be allowed: %v", err)
	}
	parentPath := t.TempDir() + string(os.PathSeparator) + ".." + string(os.PathSeparator) + "secret"
	if err := validateSecretFilePath(parentPath, "test"); err == nil {
		t.Fatal("expected parent segment rejection")
	}
}

func TestRejectUnknownConfigField(t *testing.T) {
	_, err := Parse([]byte(`
version: 1
unexpected: true
	repositories: []
`))
	if err == nil {
		t.Fatalf("expected unknown-field error, got %v", err)
	}
}

func TestRejectMultipleYAMLDocuments(t *testing.T) {
	_, err := Parse([]byte(`
version: 1
repositories: []
---
version: 1
repositories: []
`))
	if err == nil || !strings.Contains(err.Error(), "multiple YAML documents") {
		t.Fatalf("expected multiple-document error, got %v", err)
	}
}

func TestOpenSecretFileRejectsSymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "real")
	link := filepath.Join(dir, "link")
	mustWrite(t, target, "secret\n", 0o600)
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	_, err := OpenSecretFile(link)
	if err == nil {
		t.Fatal("expected symlink rejection")
	}
}

func TestOpenSecretFileRejectsPermissiveMode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows permissions are enforced through ACLs, not Unix mode bits")
	}
	dir := t.TempDir()
	p := filepath.Join(dir, "wide")
	mustWrite(t, p, "secret\n", 0o644)
	if err := os.Chmod(p, 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := OpenSecretFile(p)
	if err == nil {
		t.Fatal("expected permission rejection")
	}
}

func TestOpenSecretFileOK(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "ok")
	mustWrite(t, p, "secret\n", 0o600)
	data, err := OpenSecretFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "secret\n" {
		t.Fatalf("got %q", data)
	}
}

func TestReadRepositoryLocationRejectsMultipleLines(t *testing.T) {
	p := filepath.Join(t.TempDir(), "repo")
	mustWrite(t, p, "/srv/repo\nsftp:user@host:/repo\n", 0o600)
	if _, err := ReadRepositoryLocation(p); err == nil {
		t.Fatal("expected multi-line repository location rejection")
	}
}

func TestEmptyAllowlistWarnings(t *testing.T) {
	cfg := &Config{
		Repositories: []Repository{
			{ID: "wide-open", AllowedHosts: nil, AllowedTags: nil, AllowedPaths: nil},
			{ID: "tight", AllowedHosts: []string{"h"}, AllowedTags: []string{"t"}, AllowedPaths: []string{"/data"}},
			{ID: "paths-only", AllowedHosts: []string{"h"}, AllowedTags: []string{"t"}, AllowedPaths: nil},
		},
	}
	warns := cfg.EmptyAllowlistWarnings()
	if len(warns) != 4 {
		// wide-open: 3 warnings; paths-only: 1 → 4
		t.Fatalf("want 4 warnings, got %d: %v", len(warns), warns)
	}
	joined := strings.Join(warns, "\n")
	if !strings.Contains(joined, `repository "wide-open"`) || !strings.Contains(joined, "allowed_paths is empty") {
		t.Fatalf("unexpected warnings: %v", warns)
	}
	if strings.Contains(joined, `"tight"`) {
		t.Fatalf("tight repo should not warn: %v", warns)
	}
	// Empty config
	if len((*Config)(nil).EmptyAllowlistWarnings()) != 0 {
		t.Fatal("nil config")
	}
}

func TestFuzzParseSmoke(t *testing.T) {
	// Ensure fuzz target compiles; actual fuzz runs in CI with -fuzztime.
	f := func(data []byte) {
		_, _ = Parse(data)
	}
	f([]byte("version: 1\n"))
	f([]byte("{not yaml"))
	f(nil)
}

func FuzzParse(f *testing.F) {
	f.Add([]byte("version: 1\nrepositories: []\n"))
	f.Add([]byte("version: 1\nrepositories:\n  - id: a\n    repository_file: /x\n    password_file: /y\n"))
	f.Fuzz(func(t *testing.T, data []byte) {
		_, _ = Parse(data)
	})
}

func mustWrite(t *testing.T, path, content string, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
}
