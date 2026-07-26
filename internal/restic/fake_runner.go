package restic

import (
	"context"
	"sync"
	"time"
)

// FakeCall records one Run invocation.
type FakeCall struct {
	Argv []string
	Env  []string
}

// FakeRunner is a test double that records argv/env and returns scripted results.
type FakeRunner struct {
	mu      sync.Mutex
	Calls   []FakeCall
	Handler func(req RunRequest) (*Result, error)
	// DefaultResult used when Handler is nil.
	DefaultResult *Result
	DefaultErr    error
}

// NewFakeRunner creates an empty fake runner.
func NewFakeRunner() *FakeRunner {
	return &FakeRunner{}
}

func (f *FakeRunner) Run(ctx context.Context, req RunRequest) (*Result, error) {
	if err := AssertArgvAllowed(req.Argv); err != nil {
		return nil, err
	}
	f.mu.Lock()
	f.Calls = append(f.Calls, FakeCall{
		Argv: append([]string(nil), req.Argv...),
		Env:  append([]string(nil), req.Env...),
	})
	handler := f.Handler
	defRes := f.DefaultResult
	defErr := f.DefaultErr
	f.mu.Unlock()

	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	default:
	}

	if handler != nil {
		return handler(req)
	}
	if defRes == nil {
		defRes = &Result{ExitCode: 0, Stdout: []byte("[]"), Duration: time.Millisecond}
	}
	return defRes, defErr
}

// LastCall returns the most recent call or nil.
func (f *FakeRunner) LastCall() *FakeCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.Calls) == 0 {
		return nil
	}
	c := f.Calls[len(f.Calls)-1]
	return &c
}

// Reset clears recorded calls.
func (f *FakeRunner) Reset() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Calls = nil
}
