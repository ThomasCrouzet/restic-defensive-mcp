//go:build integration && windows

package main_test

// Windows-only fixture checks. The server does not read NTFS ACLs itself, so
// the test fixture creates and verifies the private ACL that operators must
// apply to real secret files.

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestWindowsPrivateFixtureACL(t *testing.T) {
	dir := t.TempDir()
	secret := filepath.Join(dir, "password file")
	if err := os.WriteFile(secret, []byte("synthetic-password\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	secureWindowsFile(t, secret)
	output := verifyWindowsACL(t, secret)
	if !strings.Contains(output, "(F)") {
		t.Fatalf("private ACL does not grant full control: %s", output)
	}
	if strings.Contains(output, "(I)") {
		t.Fatalf("private ACL still contains inherited entries: %s", output)
	}
}

func secureWindowsFile(t *testing.T, path string) {
	t.Helper()
	user := os.Getenv("USERNAME")
	if user == "" {
		output, err := exec.Command("whoami").Output()
		if err != nil {
			t.Fatalf("resolve Windows user: %v", err)
		}
		user = strings.TrimSpace(string(output))
	}
	cmd := exec.Command("icacls", path, "/inheritance:r", "/grant:r", user+":F")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("icacls secure %s: %v: %s", filepath.Base(path), err, output)
	}
}

func verifyWindowsACL(t *testing.T, path string) string {
	t.Helper()
	output, err := exec.Command("icacls", path).CombinedOutput()
	if err != nil {
		t.Fatalf("icacls verify %s: %v: %s", filepath.Base(path), err, output)
	}
	return string(output)
}
