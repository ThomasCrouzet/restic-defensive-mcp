package mcpserver

import (
	"time"

	apperr "github.com/ThomasCrouzet/restic-defensive-mcp/internal/errors"
)

// timeWindow is a half-open [after, before) interval on snapshot creation time.
// It never filters file modification times.
type timeWindow struct {
	after  *time.Time
	before *time.Time
}

func parseTimeWindow(after, before string) (timeWindow, error) {
	window := timeWindow{}
	if after != "" {
		parsed, err := time.Parse(time.RFC3339Nano, after)
		if err != nil {
			return timeWindow{}, apperr.New(apperr.InvalidArgument, "after must be an RFC 3339 timestamp with a UTC offset")
		}
		parsed = parsed.UTC()
		window.after = &parsed
	}
	if before != "" {
		parsed, err := time.Parse(time.RFC3339Nano, before)
		if err != nil {
			return timeWindow{}, apperr.New(apperr.InvalidArgument, "before must be an RFC 3339 timestamp with a UTC offset")
		}
		parsed = parsed.UTC()
		window.before = &parsed
	}
	if window.after != nil && window.before != nil && !window.after.Before(*window.before) {
		return timeWindow{}, apperr.New(apperr.InvalidArgument, "after must be earlier than before; the interval is [after, before)")
	}
	return window, nil
}

func (w timeWindow) contains(instant time.Time) bool {
	if w.after != nil && instant.Before(*w.after) {
		return false
	}
	if w.before != nil && !instant.Before(*w.before) {
		return false
	}
	return true
}

func (w timeWindow) normalized() (string, string) {
	after := ""
	before := ""
	if w.after != nil {
		after = w.after.Format(time.RFC3339Nano)
	}
	if w.before != nil {
		before = w.before.Format(time.RFC3339Nano)
	}
	return after, before
}
