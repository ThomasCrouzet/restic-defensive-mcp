package testrepo

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sync"
	"time"
)

// LockHolder runs a read-only restic check that holds an exclusive repository
// lock. It exists only for integration tests that need deterministic lock
// contention; production never builds the check command.
type LockHolder struct {
	cmd      *exec.Cmd
	finished chan struct{}
	stderr   bytes.Buffer
	waitErr  error
	stopOnce sync.Once
	stopErr  error
}

// HoldExclusiveLock starts "restic check --read-data" against location and
// returns once the process is running. The caller waits for the backend lock
// object before issuing a competing inspection. Release removes the lock via
// restic's normal interrupt cleanup.
func (f *Fixture) HoldExclusiveLock(ctx context.Context, location string, extraEnv []string) (*LockHolder, error) {
	cmd := exec.Command(f.binary, "check", "--read-data")
	cmd.Env = f.envFor(location, extraEnv)
	holder := &LockHolder{cmd: cmd, finished: make(chan struct{})}
	cmd.Stdout = &holder.stderr
	cmd.Stderr = &holder.stderr
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	go func() {
		holder.waitErr = cmd.Wait()
		close(holder.finished)
	}()
	go func() {
		select {
		case <-ctx.Done():
			_ = holder.Release()
		case <-holder.finished:
		}
	}()
	return holder, nil
}

// Stderr returns captured restic output for diagnostics. Synthetic credentials
// must never be written by the test to this buffer.
func (h *LockHolder) Stderr() string {
	return h.stderr.String()
}

// WaitErr reports the restic exit after the holder finished.
func (h *LockHolder) WaitErr() error {
	select {
	case <-h.finished:
		return h.waitErr
	default:
		return nil
	}
}

// Release interrupts the lock holder and waits for restic to remove its lock.
// It falls back to killing the process after a bounded grace period.
func (h *LockHolder) Release() error {
	h.stopOnce.Do(func() {
		if h.cmd.Process == nil {
			h.stopErr = fmt.Errorf("lock holder was not started")
			return
		}
		if err := h.cmd.Process.Signal(os.Interrupt); err != nil && !errors.Is(err, os.ErrProcessDone) {
			_ = h.cmd.Process.Kill()
		}
		select {
		case <-h.finished:
			// Interrupting restic returns a non-zero exit; the lock cleanup is
			// what matters, and callers verify it through the backend state.
		case <-time.After(10 * time.Second):
			_ = h.cmd.Process.Kill()
			select {
			case <-h.finished:
			case <-time.After(5 * time.Second):
				h.stopErr = fmt.Errorf("lock holder did not exit after kill")
			}
		}
	})
	return h.stopErr
}
