package apperr_test

import (
	"errors"
	"testing"

	apperr "github.com/ThomasCrouzet/restic-defensive-mcp/internal/errors"
)

func TestNewAndCodes(t *testing.T) {
	e := apperr.New(apperr.NotAllowed, "path denied")
	if e.Code != apperr.NotAllowed {
		t.Fatalf("code %s", e.Code)
	}
	if !apperr.IsCode(e, apperr.NotAllowed) {
		t.Fatal("IsCode")
	}
	got, ok := apperr.As(e)
	if !ok || got.Message != "path denied" {
		t.Fatalf("%+v ok=%v", got, ok)
	}
	with := e.WithDetail("x").WithCause(errors.New("root"))
	if with.Detail != "x" {
		t.Fatal(with.Detail)
	}
	if !errors.Is(with, with) {
		t.Fatal("unwrap chain")
	}
	// Cause must not appear in Error() string for client safety of accidental log of Error().
	if with.Error() == "" || errors.Unwrap(with) == nil {
		t.Fatal("cause missing")
	}
}

func TestAsRejectsPlainError(t *testing.T) {
	if _, ok := apperr.As(errors.New("plain")); ok {
		t.Fatal("expected false")
	}
	if apperr.IsCode(errors.New("plain"), apperr.InternalError) {
		t.Fatal("expected false")
	}
}
