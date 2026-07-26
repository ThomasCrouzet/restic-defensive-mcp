// Package apperr defines structured application errors returned to MCP clients.
package apperr

import (
	"errors"
	"fmt"
)

// Code is a stable machine-readable error code.
type Code string

const (
	InvalidArgument       Code = "invalid_argument"
	RepositoryNotFound    Code = "repository_not_found"
	SnapshotNotFound      Code = "snapshot_not_found"
	AmbiguousSnapshot     Code = "ambiguous_snapshot"
	NotAllowed            Code = "not_allowed"
	ResticNotFound        Code = "restic_not_found"
	UnsupportedResticVer  Code = "unsupported_restic_version"
	UnsupportedBackend    Code = "unsupported_backend"
	AuthenticationFailed  Code = "authentication_failed"
	RepositoryLocked      Code = "repository_locked"
	Timeout               Code = "timeout"
	Cancelled             Code = "cancelled"
	OutputLimitExceeded   Code = "output_limit_exceeded"
	ProtocolError         Code = "protocol_error"
	InternalError         Code = "internal_error"
	ConfigError           Code = "config_error"
	RepositoryUnavailable Code = "repository_unavailable"
)

// Error is a structured application error safe for MCP clients.
type Error struct {
	Code    Code   `json:"code"`
	Message string `json:"message"`
	Detail  string `json:"detail,omitempty"`
	cause   error
}

func (e *Error) Error() string {
	if e.Detail != "" {
		return fmt.Sprintf("%s: %s (%s)", e.Code, e.Message, e.Detail)
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

func (e *Error) Unwrap() error { return e.cause }

// New builds a structured error.
func New(code Code, message string) *Error {
	return &Error{Code: code, Message: message}
}

// WithDetail attaches a non-sensitive detail string.
func (e *Error) WithDetail(detail string) *Error {
	cp := *e
	cp.Detail = detail
	return &cp
}

// WithCause attaches an underlying cause (never serialized to clients).
func (e *Error) WithCause(err error) *Error {
	cp := *e
	cp.cause = err
	return &cp
}

// IsCode reports whether err is an *Error with the given code.
func IsCode(err error, code Code) bool {
	var e *Error
	if errors.As(err, &e) {
		return e.Code == code
	}
	return false
}

// As extracts an *Error if present.
func As(err error) (*Error, bool) {
	var e *Error
	if errors.As(err, &e) {
		return e, true
	}
	return nil, false
}
