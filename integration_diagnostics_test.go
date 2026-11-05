//go:build integration

package main_test

// Failure modes covered by this executable E2E test:
//
//   - Local validation contacts a backend or mutates a repository.
//   - Diagnostic exit codes differ for invalid config, unreachable repository,
//     and successful validation.
//   - Diagnostic output exposes secret files, repository locations, or values.
//   - Empty allowlists are hidden instead of reported as unrestricted.
//   - Machine-readable and human-readable output disagree.

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ThomasCrouzet/restic-defensive-mcp/internal/testrepo"
)

type diagnosticJSON struct {
	Valid           bool   `json:"valid"`
	Mode            string `json:"mode"`
	ConfigFile      string `json:"config_file"`
	ResticVersion   string `json:"restic_version"`
	ResticSupported bool   `json:"restic_supported"`
	Repositories    []struct {
		ID           string `json:"id"`
		Backend      string `json:"backend"`
		SecretsValid bool   `json:"secrets_valid"`
		Unrestricted struct {
			Hosts bool `json:"hosts"`
			Tags  bool `json:"tags"`
			Paths bool `json:"paths"`
		} `json:"unrestricted"`
		Connectivity *struct {
			OK        bool   `json:"ok"`
			ErrorCode string `json:"error_code"`
		} `json:"connectivity"`
	} `json:"repositories"`
	Warnings []string `json:"warnings"`
	Errors   []string `json:"errors"`
}

func TestExecutableConfigDiagnostics(t *testing.T) {
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

	configPath := filepath.Join(fixture.Root, "diagnostics.yaml")
	if err := fixture.WriteConfig(configPath, resticBinary); err != nil {
		t.Fatal(err)
	}
	forbidden := []string{
		fixture.RepoDir,
		fixture.RepositoryFile,
		fixture.PasswordFile,
		fixture.CacheDir,
		"test-password-not-secret",
	}

	stdout, stderr := runExpectExit(t, ctx, 0, binary, "--config", configPath, "--validate-config", "--json")
	assertNoDiagnosticLeak(t, stdout+stderr, forbidden)
	var report diagnosticJSON
	if err := json.Unmarshal([]byte(stdout), &report); err != nil {
		t.Fatalf("validate-config JSON: %v: %s", err, stdout)
	}
	if !report.Valid || report.Mode != "validate-config" || !report.ResticSupported ||
		report.ResticVersion == "" || report.ConfigFile != filepath.Base(configPath) {
		t.Fatalf("validate-config report = %+v", report)
	}
	if len(report.Repositories) != 1 || report.Repositories[0].ID != "tmp-local" ||
		report.Repositories[0].Backend != "local" || !report.Repositories[0].SecretsValid {
		t.Fatalf("validate-config repositories = %+v", report.Repositories)
	}
	if report.Repositories[0].Unrestricted.Hosts || report.Repositories[0].Unrestricted.Tags ||
		report.Repositories[0].Unrestricted.Paths {
		t.Fatalf("configured allowlists reported as unrestricted: %+v", report.Repositories[0])
	}
	if len(report.Errors) != 0 {
		t.Fatalf("valid config errors = %v", report.Errors)
	}
	artifact.check("local validation JSON result")

	humanOut, humanErr := runExpectExit(t, ctx, 0, binary, "--config", configPath, "--validate-config")
	assertNoDiagnosticLeak(t, humanOut+humanErr, forbidden)
	for _, want := range []string{"configuration: valid", "repository tmp-local", "restic:"} {
		if !strings.Contains(humanOut, want) {
			t.Fatalf("human validation output omits %q: %s", want, humanOut)
		}
	}
	artifact.check("local validation human result")

	invalidPath := filepath.Join(fixture.Root, "invalid-diagnostics.yaml")
	if err := testrepo.WriteInvalidConfig(invalidPath); err != nil {
		t.Fatal(err)
	}
	invalidOut, invalidErr := runExpectExit(t, ctx, 2, binary, "--config", invalidPath, "--validate-config", "--json")
	assertNoDiagnosticLeak(t, invalidOut+invalidErr, forbidden)
	var invalidReport diagnosticJSON
	if err := json.Unmarshal([]byte(invalidOut), &invalidReport); err != nil {
		t.Fatalf("invalid config JSON: %v: %s", err, invalidOut)
	}
	if invalidReport.Valid || len(invalidReport.Errors) == 0 {
		t.Fatalf("invalid config report = %+v", invalidReport)
	}
	artifact.check("invalid config exit code and result")

	permissionsConfig := filepath.Join(fixture.Root, "permissions-diagnostics.yaml")
	if err := fixture.WriteConfig(permissionsConfig, resticBinary); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(fixture.PasswordFile, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(fixture.PasswordFile, 0o600) })
	permissionOut, permissionErr := runExpectExit(t, ctx, 2, binary, "--config", permissionsConfig, "--validate-config", "--json")
	assertNoDiagnosticLeak(t, permissionOut+permissionErr, forbidden)
	if !strings.Contains(permissionOut, "secret file") {
		t.Fatalf("permission failure is not explained generically: %s", permissionOut)
	}
	if err := os.Chmod(fixture.PasswordFile, 0o600); err != nil {
		t.Fatal(err)
	}
	artifact.check("secret permission failure without location disclosure")

	accessKey := "synthetic-diagnostic-access"
	secretKey := "synthetic-diagnostic-secret"
	s3Config := filepath.Join(fixture.Root, "diagnostics-s3.yaml")
	if err := fixture.WriteS3Config(s3Config, resticBinary, testrepo.S3Credentials{
		AccessKeyID:     &accessKey,
		SecretAccessKey: &secretKey,
	}); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	s3Out, s3Err := runExpectExit(t, ctx, 0, binary, "--config", s3Config, "--validate-config", "--json")
	s3Elapsed := time.Since(start)
	assertNoDiagnosticLeak(t, s3Out+s3Err, append(forbidden, accessKey, secretKey))
	if s3Elapsed >= 5*time.Second {
		t.Fatalf("local S3 validation took %s; it must not contact the backend", s3Elapsed)
	}
	var s3Report diagnosticJSON
	if err := json.Unmarshal([]byte(s3Out), &s3Report); err != nil {
		t.Fatalf("S3 validation JSON: %v: %s", err, s3Out)
	}
	if !s3Report.Valid || len(s3Report.Repositories) != 1 || s3Report.Repositories[0].Backend != "s3" {
		t.Fatalf("S3 validation report = %+v", s3Report)
	}
	artifact.check("local validation performs no backend contact")

	diagnoseOut, diagnoseErr := runExpectExit(t, ctx, 0, binary, "--config", configPath, "--diagnose", "--json")
	assertNoDiagnosticLeak(t, diagnoseOut+diagnoseErr, forbidden)
	var diagnoseReport diagnosticJSON
	if err := json.Unmarshal([]byte(diagnoseOut), &diagnoseReport); err != nil {
		t.Fatalf("diagnose JSON: %v: %s", err, diagnoseOut)
	}
	if !diagnoseReport.Valid || diagnoseReport.Mode != "diagnose" || len(diagnoseReport.Repositories) != 1 ||
		diagnoseReport.Repositories[0].Connectivity == nil || !diagnoseReport.Repositories[0].Connectivity.OK {
		t.Fatalf("diagnose report = %+v", diagnoseReport)
	}
	artifact.check("explicit connectivity check succeeds")

	if err := fixture.SetRepositoryLocation(filepath.Join(fixture.Root, "missing-diagnostic-repository")); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = fixture.SetRepositoryLocation(fixture.RepoDir) })
	unreachableOut, unreachableErr := runExpectExit(t, ctx, 3, binary, "--config", configPath, "--diagnose", "--json")
	assertNoDiagnosticLeak(t, unreachableOut+unreachableErr, forbidden)
	var unreachableReport diagnosticJSON
	if err := json.Unmarshal([]byte(unreachableOut), &unreachableReport); err != nil {
		t.Fatalf("unreachable JSON: %v: %s", err, unreachableOut)
	}
	if unreachableReport.Valid || len(unreachableReport.Repositories) != 1 ||
		unreachableReport.Repositories[0].Connectivity == nil ||
		unreachableReport.Repositories[0].Connectivity.OK ||
		unreachableReport.Repositories[0].Connectivity.ErrorCode != "repository_unavailable" {
		t.Fatalf("unreachable report = %+v", unreachableReport)
	}
	artifact.check("connectivity failure exit code and safe error")
	if err := fixture.SetRepositoryLocation(fixture.RepoDir); err != nil {
		t.Fatal(err)
	}

	modeErrOut, _ := runExpectExit(t, ctx, 2, binary, "--config", configPath, "--json")
	if strings.Contains(modeErrOut, "jsonrpc") {
		t.Fatalf("diagnostic mode misuse wrote protocol data: %s", modeErrOut)
	}
	artifact.check("diagnostic flag validation")
}

func assertNoDiagnosticLeak(t *testing.T, output string, forbidden []string) {
	t.Helper()
	for _, value := range forbidden {
		if value != "" && strings.Contains(output, value) {
			t.Fatalf("diagnostic output leaked forbidden value %q: %s", value, output)
		}
	}
}
