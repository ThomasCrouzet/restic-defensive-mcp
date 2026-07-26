package restic

import (
	"net/url"
	"path/filepath"
	"strings"

	apperr "github.com/ThomasCrouzet/restic-defensive-mcp/internal/errors"
)

// BackendKind classifies a restic repository location without exposing the full URL.
type BackendKind string

const (
	BackendLocal  BackendKind = "local"
	BackendS3     BackendKind = "s3"
	BackendB2     BackendKind = "b2"
	BackendREST   BackendKind = "rest"
	BackendSFTP   BackendKind = "sftp"
	BackendRclone BackendKind = "rclone"
	BackendOther  BackendKind = "other"
)

// supportedBackends is the closed backend set for v0.1. It remains private so
// another package cannot extend the process-spawning boundary at runtime.
var supportedBackends = map[BackendKind]bool{
	BackendLocal: true,
	BackendS3:    true,
	BackendB2:    true,
	BackendREST:  true,
}

// ClassifyBackend inspects a repository location string.
func ClassifyBackend(location string) BackendKind {
	loc := strings.TrimSpace(location)
	lower := strings.ToLower(loc)

	switch {
	case strings.HasPrefix(lower, "sftp:"):
		return BackendSFTP
	case strings.HasPrefix(lower, "rclone:"):
		return BackendRclone
	case strings.HasPrefix(lower, "s3:"):
		return BackendS3
	case strings.HasPrefix(lower, "b2:"):
		return BackendB2
	case strings.HasPrefix(lower, "rest:"):
		return BackendREST
	case strings.HasPrefix(lower, "swift:"),
		strings.HasPrefix(lower, "azure:"),
		strings.HasPrefix(lower, "gs:"),
		strings.HasPrefix(lower, "rclone:"):
		return BackendOther
	}

	// Local repositories must use an absolute path so process working-directory
	// changes cannot retarget the sealed repository.
	if filepath.IsAbs(loc) {
		return BackendLocal
	}

	// URL-like without scheme prefix: try parse.
	if u, err := url.Parse(loc); err == nil && u.Scheme != "" {
		switch strings.ToLower(u.Scheme) {
		case "sftp":
			return BackendSFTP
		case "s3":
			return BackendS3
		case "b2":
			return BackendB2
		case "rest", "http", "https":
			// bare http(s) is not a restic backend without rest:
			if strings.ToLower(u.Scheme) == "rest" {
				return BackendREST
			}
			return BackendOther
		default:
			return BackendOther
		}
	}

	return BackendOther
}

// EnsureSupported returns an error if the backend is not allowed in v0.1.
func EnsureSupported(kind BackendKind) error {
	if supportedBackends[kind] {
		return nil
	}
	switch kind {
	case BackendSFTP:
		return apperr.New(apperr.UnsupportedBackend, "sftp backend is not supported in v0.1 (spawns ssh)")
	case BackendRclone:
		return apperr.New(apperr.UnsupportedBackend, "rclone backend is not supported in v0.1 (spawns rclone)")
	default:
		return apperr.New(apperr.UnsupportedBackend, "backend is not supported in v0.1")
	}
}
