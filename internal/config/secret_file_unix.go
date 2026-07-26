//go:build unix

package config

import (
	"io"
	"os"
	"syscall"

	apperr "github.com/ThomasCrouzet/restic-defensive-mcp/internal/errors"
)

// readSecretFile opens path without following symlinks (O_NOFOLLOW), re-validates
// the open file descriptor, and reads at most maxBytes.
func readSecretFile(path string, maxBytes int64) ([]byte, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, apperr.New(apperr.ConfigError, "cannot open secret file").WithCause(err)
	}
	defer func() { _ = f.Close() }()

	fi, err := f.Stat()
	if err != nil {
		return nil, apperr.New(apperr.ConfigError, "cannot stat secret file").WithCause(err)
	}
	if !fi.Mode().IsRegular() {
		return nil, apperr.New(apperr.ConfigError, "secret file must be a regular file")
	}
	if fi.Size() <= 0 || fi.Size() > maxBytes {
		return nil, apperr.New(apperr.ConfigError, "secret file size out of bounds")
	}
	if !secretFilePermissionsOK(fi.Mode()) {
		return nil, apperr.New(apperr.ConfigError, "secret file permissions must be 0600 or stricter")
	}

	// Bound the read even if the file grows after Stat.
	data, err := io.ReadAll(io.LimitReader(f, maxBytes+1))
	if err != nil {
		return nil, apperr.New(apperr.ConfigError, "cannot read secret file").WithCause(err)
	}
	if int64(len(data)) > maxBytes {
		return nil, apperr.New(apperr.ConfigError, "secret file size out of bounds")
	}
	return data, nil
}

func secretFilePermissionsOK(mode os.FileMode) bool {
	return mode.Perm()&0o077 == 0
}
