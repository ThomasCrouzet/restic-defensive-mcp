//go:build unix

package restic

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
	"time"

	apperr "github.com/ThomasCrouzet/restic-defensive-mcp/internal/errors"
)

// Failure modes retained by this isolated real-process suite:
//
//   - A stdout or stderr flood can continue until timeout after its buffer reaches the cap.
//   - A timeout or caller cancellation can leave the child process group running.
//   - A descendant can inherit output pipes and keep Run blocked after its parent exits.
//
// Real restic cannot trigger these failures reliably. The tests use disposable child
// processes and verify OS process behavior that cappedBuffer tests cannot observe.

const (
	runnerHelperModeEnv          = "RESTIC_RUNNER_HELPER_MODE"
	runnerHelperPIDFileEnv       = "RESTIC_RUNNER_HELPER_PID_FILE"
	runnerHelperChildPIDFileEnv  = "RESTIC_RUNNER_HELPER_CHILD_PID_FILE"
	runnerHelperStdoutFloodMode  = "stdout-flood"
	runnerHelperStderrFloodMode  = "stderr-flood"
	runnerHelperGroupMode        = "group"
	runnerHelperInheritedPipes   = "inherited-pipes"
	runnerHelperGrandchildMode   = "grandchild"
	runnerHelperPollInterval     = 10 * time.Millisecond
	runnerHelperStartupTimeout   = 3 * time.Second
	runnerHelperShutdownTimeout  = 3 * time.Second
	runnerHelperOperationTimeout = 8 * time.Second
)

func TestMain(m *testing.M) {
	if mode := os.Getenv(runnerHelperModeEnv); mode != "" {
		os.Exit(runRunnerProcessHelper(mode))
	}
	os.Exit(m.Run())
}

func TestExecRunnerStopsOutputFloodBeforeTimeout(t *testing.T) {
	cases := []struct {
		name string
		mode string
	}{
		{name: "stdout", mode: runnerHelperStdoutFloodMode},
		{name: "stderr", mode: runnerHelperStderrFloodMode},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			tempDir := t.TempDir()
			pidFile := filepath.Join(tempDir, "flood.pid")
			registerRunnerProcessCleanup(t, pidFile)

			runner := newSelfExecRunner(t)
			const maxOutputBytes = 32 << 10
			start := time.Now()
			result, err := runner.Run(context.Background(), RunRequest{
				Argv:           BuildVersionArgv(),
				Env:            runnerHelperEnv(testCase.mode, pidFile, ""),
				Timeout:        runnerHelperOperationTimeout,
				MaxOutputBytes: maxOutputBytes,
			})
			elapsed := time.Since(start)

			if !apperr.IsCode(err, apperr.OutputLimitExceeded) {
				t.Fatalf("Run error = %v, want %s", err, apperr.OutputLimitExceeded)
			}
			if result == nil {
				t.Fatal("Run returned no result")
			}
			if len(result.Stdout) > maxOutputBytes || len(result.Stderr) > maxOutputBytes {
				t.Fatalf("output lengths = (%d, %d), cap = %d", len(result.Stdout), len(result.Stderr), maxOutputBytes)
			}
			if testCase.mode == runnerHelperStdoutFloodMode && len(result.Stdout) != maxOutputBytes {
				t.Fatalf("stdout length = %d, want %d", len(result.Stdout), maxOutputBytes)
			}
			if testCase.mode == runnerHelperStderrFloodMode && len(result.Stderr) != maxOutputBytes {
				t.Fatalf("stderr length = %d, want %d", len(result.Stderr), maxOutputBytes)
			}
			if elapsed >= runnerHelperOperationTimeout/2 {
				t.Fatalf("Run took %s after output overflow; timeout was %s", elapsed, runnerHelperOperationTimeout)
			}

			pid := readRunnerHelperPID(t, pidFile, runnerHelperStartupTimeout)
			waitForRunnerProcessExit(t, pid, runnerHelperShutdownTimeout)
		})
	}
}

func TestExecRunnerTimeoutKillsProcessGroup(t *testing.T) {
	tempDir := t.TempDir()
	pidFile := filepath.Join(tempDir, "parent.pid")
	childPIDFile := filepath.Join(tempDir, "child.pid")
	registerRunnerProcessCleanup(t, pidFile, childPIDFile)

	runner := newSelfExecRunner(t)
	const timeout = 500 * time.Millisecond
	start := time.Now()
	result, err := runner.Run(context.Background(), RunRequest{
		Argv:           BuildVersionArgv(),
		Env:            runnerHelperEnv(runnerHelperGroupMode, pidFile, childPIDFile),
		Timeout:        timeout,
		MaxOutputBytes: 1 << 20,
	})
	elapsed := time.Since(start)

	if !apperr.IsCode(err, apperr.Timeout) {
		t.Fatalf("Run error = %v, want %s", err, apperr.Timeout)
	}
	if result == nil {
		t.Fatal("Run returned no result")
	}
	if elapsed >= runnerHelperShutdownTimeout {
		t.Fatalf("Run took %s after a %s timeout", elapsed, timeout)
	}

	parentPID := readRunnerHelperPID(t, pidFile, runnerHelperStartupTimeout)
	childPID := readRunnerHelperPID(t, childPIDFile, runnerHelperStartupTimeout)
	waitForRunnerProcessExit(t, parentPID, runnerHelperShutdownTimeout)
	waitForRunnerProcessExit(t, childPID, runnerHelperShutdownTimeout)
}

func TestExecRunnerInheritedPipesDoNotOutliveWaitDelay(t *testing.T) {
	tempDir := t.TempDir()
	pidFile := filepath.Join(tempDir, "parent.pid")
	childPIDFile := filepath.Join(tempDir, "child.pid")
	registerRunnerProcessCleanup(t, pidFile, childPIDFile)

	runner := newSelfExecRunner(t)
	start := time.Now()
	result, err := runner.Run(context.Background(), RunRequest{
		Argv:           BuildVersionArgv(),
		Env:            runnerHelperEnv(runnerHelperInheritedPipes, pidFile, childPIDFile),
		Timeout:        runnerHelperOperationTimeout,
		MaxOutputBytes: 1 << 20,
	})
	elapsed := time.Since(start)

	if !apperr.IsCode(err, apperr.InternalError) || !errors.Is(err, exec.ErrWaitDelay) {
		t.Fatalf("Run error = %v, want internal error caused by exec.ErrWaitDelay", err)
	}
	if result == nil {
		t.Fatal("Run returned no result")
	}
	if elapsed >= runnerHelperOperationTimeout/2 {
		t.Fatalf("Run took %s with inherited pipes; timeout was %s", elapsed, runnerHelperOperationTimeout)
	}

	parentPID := readRunnerHelperPID(t, pidFile, runnerHelperStartupTimeout)
	childPID := readRunnerHelperPID(t, childPIDFile, runnerHelperStartupTimeout)
	waitForRunnerProcessExit(t, parentPID, runnerHelperShutdownTimeout)
	waitForRunnerProcessExit(t, childPID, runnerHelperShutdownTimeout)
}

func TestExecRunnerCallerCancellationClosesInheritedPipes(t *testing.T) {
	tempDir := t.TempDir()
	pidFile := filepath.Join(tempDir, "parent.pid")
	childPIDFile := filepath.Join(tempDir, "child.pid")
	registerRunnerProcessCleanup(t, pidFile, childPIDFile)

	runner := newSelfExecRunner(t)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	type runOutcome struct {
		result *Result
		err    error
	}
	outcomes := make(chan runOutcome, 1)
	go func() {
		result, err := runner.Run(ctx, RunRequest{
			Argv:           BuildVersionArgv(),
			Env:            runnerHelperEnv(runnerHelperInheritedPipes, pidFile, childPIDFile),
			Timeout:        runnerHelperOperationTimeout,
			MaxOutputBytes: 1 << 20,
		})
		outcomes <- runOutcome{result: result, err: err}
	}()

	parentPID := readRunnerHelperPID(t, pidFile, runnerHelperStartupTimeout)
	childPID := readRunnerHelperPID(t, childPIDFile, runnerHelperStartupTimeout)
	waitForRunnerProcessExit(t, parentPID, runnerHelperStartupTimeout)

	start := time.Now()
	cancel()
	select {
	case outcome := <-outcomes:
		if !apperr.IsCode(outcome.err, apperr.Cancelled) {
			t.Fatalf("Run error = %v, want %s", outcome.err, apperr.Cancelled)
		}
		if outcome.result == nil {
			t.Fatal("Run returned no result")
		}
	case <-time.After(runnerHelperShutdownTimeout):
		t.Fatal("Run remained blocked on inherited output pipes")
	}
	if elapsed := time.Since(start); elapsed >= runnerHelperShutdownTimeout {
		t.Fatalf("Run took %s after caller cancellation", elapsed)
	}
	waitForRunnerProcessExit(t, childPID, runnerHelperShutdownTimeout)
}

func newSelfExecRunner(t *testing.T) *ExecRunner {
	t.Helper()
	runner, err := NewExecRunner(os.Args[0])
	if err != nil {
		t.Fatal(err)
	}
	return runner
}

func runnerHelperEnv(mode, pidFile, childPIDFile string) []string {
	env := []string{
		runnerHelperModeEnv + "=" + mode,
		runnerHelperPIDFileEnv + "=" + pidFile,
	}
	if childPIDFile != "" {
		env = append(env, runnerHelperChildPIDFileEnv+"="+childPIDFile)
	}
	return env
}

func runRunnerProcessHelper(mode string) int {
	pidFile := os.Getenv(runnerHelperPIDFileEnv)
	if err := writeRunnerHelperPID(pidFile); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, err)
		return 2
	}

	switch mode {
	case runnerHelperStdoutFloodMode, runnerHelperStderrFloodMode:
		chunk := bytes.Repeat([]byte("x"), 4096)
		for {
			if mode == runnerHelperStdoutFloodMode {
				_, _ = os.Stdout.Write(chunk)
			} else {
				_, _ = os.Stderr.Write(chunk)
			}
		}
	case runnerHelperGroupMode, runnerHelperInheritedPipes:
		if err := startRunnerHelperGrandchild(os.Getenv(runnerHelperChildPIDFileEnv)); err != nil {
			_, _ = fmt.Fprintln(os.Stderr, err)
			return 3
		}
		if mode == runnerHelperInheritedPipes {
			return 0
		}
		for {
			time.Sleep(time.Hour)
		}
	case runnerHelperGrandchildMode:
		for {
			time.Sleep(time.Hour)
		}
	default:
		_, _ = fmt.Fprintf(os.Stderr, "unknown helper mode %q\n", mode)
		return 4
	}
}

func startRunnerHelperGrandchild(pidFile string) error {
	cmd := exec.Command(os.Args[0], BuildVersionArgv()...)
	cmd.Env = runnerHelperEnv(runnerHelperGrandchildMode, pidFile, "")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start grandchild: %w", err)
	}
	deadline := time.Now().Add(runnerHelperStartupTimeout)
	for time.Now().Before(deadline) {
		if _, err := readRunnerHelperPIDFile(pidFile); err == nil {
			return nil
		}
		time.Sleep(runnerHelperPollInterval)
	}
	_ = cmd.Process.Kill()
	return fmt.Errorf("grandchild did not become ready")
}

func writeRunnerHelperPID(path string) error {
	if path == "" {
		return fmt.Errorf("helper PID file is not configured")
	}
	return os.WriteFile(path, []byte(strconv.Itoa(os.Getpid())), 0o600)
}

func readRunnerHelperPID(t *testing.T, path string, timeout time.Duration) int {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		pid, err := readRunnerHelperPIDFile(path)
		if err == nil {
			return pid
		}
		lastErr = err
		time.Sleep(runnerHelperPollInterval)
	}
	t.Fatalf("read helper PID from %s: %v", filepath.Base(path), lastErr)
	return 0
}

func readRunnerHelperPIDFile(path string) (int, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	pid, err := strconv.Atoi(string(raw))
	if err != nil || pid <= 1 {
		return 0, fmt.Errorf("invalid helper PID %q", raw)
	}
	return pid, nil
}

func waitForRunnerProcessExit(t *testing.T, pid int, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if !runnerProcessExists(pid) {
			return
		}
		time.Sleep(runnerHelperPollInterval)
	}
	t.Fatalf("helper process %d did not exit", pid)
}

func runnerProcessExists(pid int) bool {
	err := syscall.Kill(pid, 0)
	if err != nil && !errors.Is(err, syscall.EPERM) {
		return false
	}
	stat, statErr := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if statErr == nil {
		end := bytes.LastIndex(stat, []byte(") "))
		if end >= 0 && len(stat) > end+2 && stat[end+2] == 'Z' {
			return false
		}
	}
	return true
}

func registerRunnerProcessCleanup(t *testing.T, pidFiles ...string) {
	t.Helper()
	t.Cleanup(func() {
		for _, path := range pidFiles {
			pid, err := readRunnerHelperPIDFile(path)
			if err != nil || pid == os.Getpid() {
				continue
			}
			_ = syscall.Kill(pid, syscall.SIGKILL)
			_ = syscall.Kill(-pid, syscall.SIGKILL)
		}
	})
}
