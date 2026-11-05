package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/ThomasCrouzet/restic-defensive-mcp/internal/config"
	apperr "github.com/ThomasCrouzet/restic-defensive-mcp/internal/errors"
	"github.com/ThomasCrouzet/restic-defensive-mcp/internal/redaction"
	"github.com/ThomasCrouzet/restic-defensive-mcp/internal/repositories"
	"github.com/ThomasCrouzet/restic-defensive-mcp/internal/restic"
)

const (
	diagnosticExitOK           = 0
	diagnosticExitInvalid      = 2
	diagnosticExitUnreachable  = 3
	diagnosticModeValidate     = "validate-config"
	diagnosticModeConnectivity = "diagnose"
)

// diagnosticReport is the bounded, machine-readable result of a local
// configuration check or an explicitly requested connectivity check.
type diagnosticReport struct {
	Valid           bool               `json:"valid"`
	Mode            string             `json:"mode"`
	ConfigFile      string             `json:"config_file,omitempty"`
	ConfigVersion   int                `json:"config_version,omitempty"`
	ResticVersion   string             `json:"restic_version,omitempty"`
	ResticSupported bool               `json:"restic_supported"`
	Limits          map[string]any     `json:"limits,omitempty"`
	Repositories    []repositoryReport `json:"repositories,omitempty"`
	Warnings        []string           `json:"warnings,omitempty"`
	Errors          []string           `json:"errors,omitempty"`
}

type repositoryReport struct {
	ID           string              `json:"id"`
	Label        string              `json:"label,omitempty"`
	Backend      string              `json:"backend,omitempty"`
	AllowedHosts []string            `json:"allowed_hosts,omitempty"`
	AllowedTags  []string            `json:"allowed_tags,omitempty"`
	AllowedPaths []string            `json:"allowed_paths,omitempty"`
	Unrestricted restrictionReport   `json:"unrestricted"`
	SecretsValid bool                `json:"secrets_valid"`
	Connectivity *connectivityReport `json:"connectivity,omitempty"`
}

type restrictionReport struct {
	Hosts bool `json:"hosts"`
	Tags  bool `json:"tags"`
	Paths bool `json:"paths"`
}

type connectivityReport struct {
	OK         bool   `json:"ok"`
	ErrorCode  string `json:"error_code,omitempty"`
	Error      string `json:"error,omitempty"`
	DurationMS int64  `json:"duration_ms"`
}

func runConfigDiagnostic(configPath string, checkConnectivity, jsonOutput bool) int {
	report := diagnosticReport{
		Mode:            diagnosticModeValidate,
		ResticSupported: false,
	}
	if checkConnectivity {
		report.Mode = diagnosticModeConnectivity
	}
	if configPath == "" {
		report.Errors = append(report.Errors, "config path is required")
		emitDiagnostic(report, jsonOutput)
		return diagnosticExitInvalid
	}
	report.ConfigFile = filepath.Base(configPath)

	cfg, err := config.Load(configPath)
	if err != nil {
		report.Errors = append(report.Errors, safeDiagnosticError(err))
		emitDiagnostic(report, jsonOutput)
		return diagnosticExitInvalid
	}
	report.ConfigVersion = cfg.Version
	report.Limits = diagnosticLimits(cfg.Limits)
	report.Warnings = append(report.Warnings, cfg.EmptyAllowlistWarnings()...)

	var runner restic.Runner
	execRunner, err := restic.NewExecRunner(cfg.ResticBinary)
	if err != nil {
		report.Errors = append(report.Errors, "restic binary unavailable: "+safeDiagnosticError(err))
	} else {
		runner = execRunner
		version, versionErr := probeResticVersion(context.Background(), runner)
		if versionErr != nil {
			report.Errors = append(report.Errors, "restic version unavailable: "+safeDiagnosticError(versionErr))
		} else {
			report.ResticVersion = version
			report.ResticSupported = restic.CompareVersion(version, restic.MinResticVersion) >= 0
			if !report.ResticSupported {
				report.Errors = append(report.Errors, fmt.Sprintf(
					"restic %s is below the supported minimum %s",
					version, restic.MinResticVersion,
				))
			}
		}
	}

	registry, registryErr := repositories.New(cfg, runner, nil)
	if registryErr != nil {
		report.Errors = append(report.Errors, "repository configuration invalid: "+safeDiagnosticError(registryErr))
	}
	report.Repositories = diagnosticRepositories(cfg, registry, registryErr == nil)

	if checkConnectivity {
		if registry == nil || runner == nil {
			report.Errors = append(report.Errors, "connectivity check requires valid configuration and a supported restic binary")
		} else {
			connectivityOK := true
			for i := range report.Repositories {
				repositoryID := report.Repositories[i].ID
				connectivity := checkRepositoryConnectivity(context.Background(), registry, repositoryID)
				report.Repositories[i].Connectivity = &connectivity
				if !connectivity.OK {
					connectivityOK = false
				}
			}
			report.Valid = len(report.Errors) == 0 && connectivityOK
			emitDiagnostic(report, jsonOutput)
			if !report.Valid {
				if len(report.Errors) > 0 {
					return diagnosticExitInvalid
				}
				return diagnosticExitUnreachable
			}
			return diagnosticExitOK
		}
	}

	report.Valid = len(report.Errors) == 0
	emitDiagnostic(report, jsonOutput)
	if !report.Valid {
		return diagnosticExitInvalid
	}
	return diagnosticExitOK
}

func diagnosticRepositories(cfg *config.Config, registry *repositories.Registry, secretsValid bool) []repositoryReport {
	out := make([]repositoryReport, 0, len(cfg.Repositories))
	if registry != nil {
		for _, info := range registry.List() {
			out = append(out, repositoryReport{
				ID:           info.ID,
				Label:        info.Label,
				Backend:      info.Backend,
				AllowedHosts: info.AllowedHosts,
				AllowedTags:  info.AllowedTags,
				AllowedPaths: info.AllowedPaths,
				Unrestricted: restrictionReport{
					Hosts: len(info.AllowedHosts) == 0,
					Tags:  len(info.AllowedTags) == 0,
					Paths: len(info.AllowedPaths) == 0,
				},
				SecretsValid: secretsValid,
			})
		}
		return out
	}
	for _, repository := range cfg.Repositories {
		out = append(out, repositoryReport{
			ID:           repository.ID,
			Label:        repository.Label,
			AllowedHosts: append([]string(nil), repository.AllowedHosts...),
			AllowedTags:  append([]string(nil), repository.AllowedTags...),
			AllowedPaths: append([]string(nil), repository.AllowedPaths...),
			Unrestricted: restrictionReport{
				Hosts: len(repository.AllowedHosts) == 0,
				Tags:  len(repository.AllowedTags) == 0,
				Paths: len(repository.AllowedPaths) == 0,
			},
			SecretsValid: false,
		})
	}
	return out
}

func diagnosticLimits(limits config.Limits) map[string]any {
	return map[string]any{
		"command_timeout_sec":           int(limits.CommandTimeout.Seconds()),
		"expensive_command_timeout_sec": int(limits.ExpensiveCommandTimeout.Seconds()),
		"max_snapshots":                 limits.MaxSnapshots,
		"max_nodes":                     limits.MaxNodes,
		"max_find_results":              limits.MaxFindResults,
		"max_output_bytes":              limits.MaxOutputBytes,
		"max_concurrent_per_repo":       limits.MaxConcurrentPerRepo,
		"max_concurrent_global":         limits.MaxConcurrentGlobal,
	}
}

func checkRepositoryConnectivity(ctx context.Context, registry *repositories.Registry, repositoryID string) connectivityReport {
	start := time.Now()
	argv, err := restic.BuildSnapshotsArgv(restic.SnapshotsOpts{})
	if err != nil {
		return connectivityReport{OK: false, ErrorCode: string(apperr.InternalError), Error: safeDiagnosticError(err)}
	}
	res, err := registry.Run(ctx, repositoryID, argv, false)
	report := connectivityReport{
		OK:         err == nil,
		DurationMS: time.Since(start).Milliseconds(),
	}
	if err != nil {
		report.ErrorCode = diagnosticErrorCode(err)
		report.Error = safeDiagnosticError(err)
		return report
	}
	if _, parseErr := restic.ParseSnapshots(res.Stdout); parseErr != nil {
		report.OK = false
		report.ErrorCode = diagnosticErrorCode(parseErr)
		report.Error = safeDiagnosticError(parseErr)
	}
	return report
}

func diagnosticErrorCode(err error) string {
	if ae, ok := apperr.As(err); ok {
		return string(ae.Code)
	}
	return string(apperr.InternalError)
}

func safeDiagnosticError(err error) string {
	if err == nil {
		return ""
	}
	message := redaction.SanitizeAndRedact(err.Error())
	return redaction.Clip(message, 300)
}

func emitDiagnostic(report diagnosticReport, jsonOutput bool) {
	if jsonOutput {
		encoder := json.NewEncoder(os.Stdout)
		encoder.SetIndent("", "  ")
		_ = encoder.Encode(report)
		return
	}
	status := "valid"
	if !report.Valid {
		status = "invalid"
	}
	_, _ = fmt.Fprintf(os.Stdout, "configuration: %s\n", status)
	if report.ConfigFile != "" {
		_, _ = fmt.Fprintf(os.Stdout, "config_file: %s\n", report.ConfigFile)
	}
	if report.ResticVersion != "" {
		supported := "unsupported"
		if report.ResticSupported {
			supported = "supported"
		}
		_, _ = fmt.Fprintf(os.Stdout, "restic: %s (%s)\n", report.ResticVersion, supported)
	}
	for _, repository := range report.Repositories {
		_, _ = fmt.Fprintf(os.Stdout, "repository %s: backend=%s\n", repository.ID, repository.Backend)
		_, _ = fmt.Fprintf(os.Stdout, "  secrets: %s\n", yesNo(repository.SecretsValid))
		_, _ = fmt.Fprintf(os.Stdout, "  restrictions: hosts=%s tags=%s paths=%s\n",
			restrictionLabel(repository.Unrestricted.Hosts),
			restrictionLabel(repository.Unrestricted.Tags),
			restrictionLabel(repository.Unrestricted.Paths),
		)
		if repository.Connectivity != nil {
			if repository.Connectivity.OK {
				_, _ = fmt.Fprintf(os.Stdout, "  connectivity: ok (%d ms)\n", repository.Connectivity.DurationMS)
			} else {
				_, _ = fmt.Fprintf(os.Stdout, "  connectivity: failed code=%s (%d ms)\n",
					repository.Connectivity.ErrorCode, repository.Connectivity.DurationMS)
			}
		}
	}
	for _, warning := range report.Warnings {
		_, _ = fmt.Fprintf(os.Stdout, "warning: %s\n", warning)
	}
	for _, message := range report.Errors {
		_, _ = fmt.Fprintf(os.Stdout, "error: %s\n", message)
	}
}

func yesNo(value bool) string {
	if value {
		return "valid"
	}
	return "invalid"
}

func restrictionLabel(unrestricted bool) string {
	if unrestricted {
		return "unrestricted"
	}
	return "restricted"
}
