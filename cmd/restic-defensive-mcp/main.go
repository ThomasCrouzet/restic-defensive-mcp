// Command restic-defensive-mcp is a structurally read-only MCP server for inspecting restic repositories.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/ThomasCrouzet/restic-defensive-mcp/internal/audit"
	"github.com/ThomasCrouzet/restic-defensive-mcp/internal/config"
	"github.com/ThomasCrouzet/restic-defensive-mcp/internal/mcpserver"
	"github.com/ThomasCrouzet/restic-defensive-mcp/internal/redaction"
	"github.com/ThomasCrouzet/restic-defensive-mcp/internal/repositories"
	"github.com/ThomasCrouzet/restic-defensive-mcp/internal/restic"
)

// version is overridden via -ldflags "-X main.version=..."
var version = "0.1.0-dev"

func main() {
	os.Exit(run())
}

func run() int {
	configPath := flag.String("config", "", "path to YAML configuration file (required)")
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()

	if *showVersion {
		_, _ = fmt.Fprintf(os.Stdout, "restic-defensive-mcp %s\n", version)
		return 0
	}
	if *configPath == "" {
		fmt.Fprintf(os.Stderr, "error: --config is required\n")
		flag.Usage()
		return 2
	}

	// Logging only on stderr; stdout is reserved for MCP JSON-RPC.
	logHandler := slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo})
	slog.SetDefault(slog.New(logHandler))

	cfg, err := config.Load(*configPath)
	if err != nil {
		slog.Error("config_load_failed", "err", redaction.SanitizeAndRedact(err.Error()))
		return 2
	}
	// Operator foot-gun: empty allowlists mean no restriction for that dimension.
	for _, w := range cfg.EmptyAllowlistWarnings() {
		slog.Warn("empty_allowlist", "msg", w)
	}

	runner, err := restic.NewExecRunner(cfg.ResticBinary)
	if err != nil {
		slog.Error("restic_binary", "err", redaction.SanitizeAndRedact(err.Error()))
		return 2
	}

	resticVer, err := probeResticVersion(context.Background(), runner)
	if err != nil {
		slog.Error("restic_version", "err", redaction.SanitizeAndRedact(err.Error()))
		return 2
	}
	if restic.CompareVersion(resticVer, restic.MinResticVersion) < 0 {
		slog.Error("unsupported_restic_version", "version", resticVer, "min", restic.MinResticVersion)
		return 2
	}

	auditor := audit.New(os.Stderr)
	reg, err := repositories.New(cfg, runner, auditor)
	if err != nil {
		slog.Error("registry", "err", redaction.SanitizeAndRedact(err.Error()))
		return 2
	}

	mcpserver.Version = version
	srv := mcpserver.NewServer(mcpserver.Deps{
		Registry:      reg,
		ResticVersion: resticVer,
		Audit:         auditor,
		ServerVersion: version,
	})

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	slog.Info("starting",
		"version", version,
		"restic", resticVer,
		"repositories", len(reg.IDs()),
		"config", filepath.Base(*configPath),
	)

	if err := srv.Run(ctx, &mcp.StdioTransport{}); err != nil && ctx.Err() == nil {
		slog.Error("server_exit", "err", redaction.SanitizeAndRedact(err.Error()))
		return 1
	}
	return 0
}

func probeResticVersion(ctx context.Context, runner restic.Runner) (string, error) {
	env := []string{"PATH=/usr/bin:/bin", "LANG=C", "LC_ALL=C", "HOME=/var/empty"}
	res, err := runner.Run(ctx, restic.RunRequest{
		Argv:           restic.BuildVersionArgv(),
		Env:            env,
		Timeout:        15 * time.Second,
		MaxOutputBytes: 64 << 10,
	})
	if err != nil {
		return "", err
	}
	vi, err := restic.ParseVersion(res.Stdout)
	if err != nil {
		return "", err
	}
	return vi.Version, nil
}
