package restic

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	apperr "github.com/ThomasCrouzet/restic-defensive-mcp/internal/errors"
	"github.com/ThomasCrouzet/restic-defensive-mcp/internal/redaction"
)

// Result is a completed restic process outcome.
type Result struct {
	Stdout   []byte
	Stderr   []byte
	ExitCode int
	Duration time.Duration
}

// RunRequest describes one restic invocation.
type RunRequest struct {
	// Argv is the subcommand and flags only (no binary path). Built by Build* helpers.
	Argv []string
	// Env is the complete child environment (already scrubbed + injected).
	Env []string
	// Timeout bounds the command.
	Timeout time.Duration
	// MaxOutputBytes caps stdout and stderr independently.
	MaxOutputBytes int
	// WorkDir optional; usually empty.
	WorkDir string
}

// Runner executes restic. Production uses ExecRunner; tests inject FakeRunner.
type Runner interface {
	Run(ctx context.Context, req RunRequest) (*Result, error)
}

// ExecRunner runs the real restic binary via os/exec without a shell.
type ExecRunner struct {
	binary string
}

// NewExecRunner resolves the restic binary to an absolute path at boot.
func NewExecRunner(binary string) (*ExecRunner, error) {
	path, err := resolveBinary(binary)
	if err != nil {
		return nil, err
	}
	return &ExecRunner{binary: path}, nil
}

func resolveBinary(binary string) (string, error) {
	if binary == "" {
		binary = "restic"
	}
	if strings.Contains(binary, string(os.PathSeparator)) || filepath.IsAbs(binary) {
		abs, err := filepath.Abs(binary)
		if err != nil {
			return "", apperr.New(apperr.ResticNotFound, "cannot resolve restic binary").WithCause(err)
		}
		info, err := os.Stat(abs)
		if err != nil || info.IsDir() {
			return "", apperr.New(apperr.ResticNotFound, "restic binary not found")
		}
		return abs, nil
	}
	path, err := exec.LookPath(binary)
	if err != nil {
		return "", apperr.New(apperr.ResticNotFound, "restic binary not found in PATH").WithCause(err)
	}
	return path, nil
}

// Run executes restic with a hard timeout and output caps. Never uses a shell.
func (r *ExecRunner) Run(ctx context.Context, req RunRequest) (*Result, error) {
	if err := AssertArgvAllowed(req.Argv); err != nil {
		return nil, apperr.New(apperr.InternalError, "refusing to run disallowed argv").WithCause(err)
	}
	if req.Timeout <= 0 {
		req.Timeout = 30 * time.Second
	}
	if req.MaxOutputBytes <= 0 {
		req.MaxOutputBytes = 8 << 20
	}

	runCtx, cancel := context.WithTimeout(ctx, req.Timeout)
	defer cancel()

	cmd := exec.CommandContext(runCtx, r.binary, req.Argv...)
	cmd.Env = req.Env
	if req.WorkDir != "" {
		cmd.Dir = req.WorkDir
	}
	configureProcess(cmd)
	cmd.Cancel = func() error {
		return killProcessGroup(cmd)
	}
	// Bound waits for inherited stdout/stderr descriptors after cancellation.
	cmd.WaitDelay = 2 * time.Second

	var stdout, stderr cappedBuffer
	stdout.limit = req.MaxOutputBytes
	stderr.limit = req.MaxOutputBytes
	// Stop the process as soon as either stream reaches its cap. Without this,
	// memory stays bounded but a noisy child can continue running until timeout.
	stdout.onExceeded = cancel
	stderr.onExceeded = cancel
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	start := time.Now()
	err := cmd.Run()
	dur := time.Since(start)
	safeStderr := redactChildStderr(stderr.Bytes(), req.Env)

	res := &Result{
		Stdout:   stdout.Bytes(),
		Stderr:   safeStderr,
		Duration: dur,
	}

	if stdout.exceeded || stderr.exceeded {
		return res, apperr.New(apperr.OutputLimitExceeded, "restic output exceeded configured limit")
	}

	if runCtx.Err() == context.DeadlineExceeded {
		return res, apperr.New(apperr.Timeout, "restic command timed out")
	}
	if ctx.Err() != nil {
		return res, apperr.New(apperr.Cancelled, "request cancelled")
	}

	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			res.ExitCode = ee.ExitCode()
			return res, MapExitCode(res.ExitCode, safeStderr)
		}
		return res, apperr.New(apperr.InternalError, "failed to execute restic").WithCause(err)
	}
	res.ExitCode = 0
	return res, nil
}

// cappedBuffer collects output up to a limit.
type cappedBuffer struct {
	buf        bytes.Buffer
	limit      int
	exceeded   bool
	onExceeded func()
	mu         sync.Mutex
}

func (c *cappedBuffer) Write(p []byte) (int, error) {
	c.mu.Lock()
	if c.exceeded {
		c.mu.Unlock()
		return len(p), nil
	}
	remain := c.limit - c.buf.Len()
	if remain <= 0 {
		c.exceeded = true
		onExceeded := c.onExceeded
		c.mu.Unlock()
		if onExceeded != nil {
			onExceeded()
		}
		return len(p), nil
	}
	if len(p) > remain {
		_, _ = c.buf.Write(p[:remain])
		c.exceeded = true
		onExceeded := c.onExceeded
		c.mu.Unlock()
		if onExceeded != nil {
			onExceeded()
		}
		return len(p), nil
	}
	n, err := c.buf.Write(p)
	c.mu.Unlock()
	return n, err
}

func (c *cappedBuffer) Bytes() []byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]byte(nil), c.buf.Bytes()...)
}

// MapExitCode converts restic exit codes to structured errors.
// stderr is redacted before inclusion in messages.
func MapExitCode(code int, stderr []byte) error {
	msg := redaction.SanitizeAndRedact(string(stderr))
	msg = redaction.Clip(msg, 400)
	switch code {
	case 0:
		return nil
	case 10:
		return apperr.New(apperr.RepositoryUnavailable, "repository does not exist")
	case 11:
		return apperr.New(apperr.RepositoryLocked, "repository is locked")
	case 12:
		return apperr.New(apperr.AuthenticationFailed, "authentication failed")
	case 130:
		return apperr.New(apperr.Cancelled, "restic command cancelled")
	default:
		e := apperr.New(apperr.InternalError, "restic command failed")
		if msg != "" {
			e = e.WithDetail(msg)
		}
		return e
	}
}

func redactChildStderr(stderr []byte, env []string) []byte {
	values := make([]string, 0, len(env))
	for _, entry := range env {
		key, value, ok := strings.Cut(entry, "=")
		if !ok || value == "" {
			continue
		}
		switch key {
		case "RESTIC_REPOSITORY",
			"RESTIC_PASSWORD_FILE",
			"RESTIC_CACHE_DIR",
			"AWS_ACCESS_KEY_ID",
			"AWS_SECRET_ACCESS_KEY",
			"AWS_SESSION_TOKEN",
			"AWS_SHARED_CREDENTIALS_FILE",
			"B2_ACCOUNT_ID",
			"B2_ACCOUNT_KEY",
			"RESTIC_REST_USERNAME",
			"RESTIC_REST_PASSWORD":
			values = append(values, value)
		}
	}
	safe := redaction.SanitizeString(string(stderr))
	safe = redaction.RedactValues(safe, values...)
	safe = redaction.RedactSecrets(safe)
	return []byte(safe)
}

// BuildChildEnv constructs a minimal environment for a repository.
// It does not inherit RESTIC_* or cloud credentials from the parent process.
func BuildChildEnv(opts ChildEnvOpts) ([]string, error) {
	// Base: PATH and locale only from parent (needed to find nothing — binary is absolute).
	// Still provide a minimal PATH and system roots for TLS.
	env := []string{
		"PATH=/usr/bin:/bin:/usr/sbin:/sbin",
		"LANG=C",
		"LC_ALL=C",
		"HOME=" + opts.HomeDir,
	}
	if opts.HomeDir == "" {
		// Isolate HOME so restic does not read ~/.restic or user ssh config unexpectedly.
		env[3] = "HOME=/var/empty"
	}

	// The repository location is snapshotted from repository_file at boot so
	// later file changes cannot retarget the sealed registry. The password
	// remains file-based to support credential rotation.
	if opts.Repository == "" || opts.PasswordFile == "" {
		return nil, apperr.New(apperr.InternalError, "repository and password_file required for child env")
	}
	env = append(env,
		"RESTIC_REPOSITORY="+opts.Repository,
		"RESTIC_PASSWORD_FILE="+opts.PasswordFile,
	)
	if opts.CacheDir != "" {
		env = append(env, "RESTIC_CACHE_DIR="+opts.CacheDir)
	}

	// Optional backend credential files (KEY=VALUE lines). Allowlist keys only.
	seenExtra := make(map[string]struct{}, len(opts.ExtraEnv))
	for _, kv := range opts.ExtraEnv {
		key, val, ok := strings.Cut(kv, "=")
		if !ok || key == "" {
			return nil, apperr.New(apperr.ConfigError, "invalid env entry")
		}
		if !allowedEnvKey(key) {
			return nil, apperr.New(apperr.ConfigError, "env key not allowlisted").WithDetail(key)
		}
		if _, duplicate := seenExtra[key]; duplicate {
			return nil, apperr.New(apperr.ConfigError, "duplicate env key").WithDetail(key)
		}
		if strings.ContainsRune(val, 0) {
			return nil, apperr.New(apperr.ConfigError, "env value contains a null byte").WithDetail(key)
		}
		seenExtra[key] = struct{}{}
		// Never allow overriding restic password/repo via extra env.
		env = append(env, key+"="+val)
	}

	// Explicitly ensure dangerous vars are not present (belt and suspenders).
	// We rebuild from scratch so parent RESTIC_PASSWORD etc. are not inherited.
	return env, nil
}

// ChildEnvOpts configures BuildChildEnv.
type ChildEnvOpts struct {
	Repository   string
	PasswordFile string
	CacheDir     string
	HomeDir      string
	// ExtraEnv is KEY=VALUE pairs already validated.
	ExtraEnv []string
}

var allowedEnvKeys = map[string]struct{}{
	// AWS / S3
	"AWS_ACCESS_KEY_ID": {}, "AWS_SECRET_ACCESS_KEY": {}, "AWS_SESSION_TOKEN": {},
	"AWS_DEFAULT_REGION": {}, "AWS_REGION": {}, "AWS_PROFILE": {},
	"AWS_SHARED_CREDENTIALS_FILE": {},
	// B2
	"B2_ACCOUNT_ID": {}, "B2_ACCOUNT_KEY": {},
	// REST server
	"RESTIC_REST_USERNAME": {}, "RESTIC_REST_PASSWORD": {},
	// TLS optional custom CA via file path only — we do not pass --cacert; skip for v0.1
	"TMPDIR": {}, "TMP": {}, "TEMP": {},
}

func allowedEnvKey(k string) bool {
	_, ok := allowedEnvKeys[k]
	return ok
}

// LoadEnvFile reads KEY=VALUE lines from a secret file (same constraints as secrets).
func LoadEnvFile(path string, open func(string) ([]byte, error)) ([]string, error) {
	data, err := open(path)
	if err != nil {
		return nil, err
	}
	var out []string
	lines := strings.Split(string(data), "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, val, ok := strings.Cut(line, "=")
		if !ok {
			return nil, apperr.New(apperr.ConfigError, "env file line must be KEY=VALUE")
		}
		key = strings.TrimSpace(key)
		if !allowedEnvKey(key) {
			return nil, apperr.New(apperr.ConfigError, "env file contains non-allowlisted key")
		}
		// Reject restic password command style
		if strings.HasPrefix(key, "RESTIC_") && key != "RESTIC_REST_USERNAME" && key != "RESTIC_REST_PASSWORD" {
			return nil, apperr.New(apperr.ConfigError, "env file must not set RESTIC_* repository secrets")
		}
		out = append(out, key+"="+val)
	}
	return out, nil
}

// Ensure FakeRunner can be used when tests need io.Writer compatibility.
var _ io.Writer = (*cappedBuffer)(nil)
