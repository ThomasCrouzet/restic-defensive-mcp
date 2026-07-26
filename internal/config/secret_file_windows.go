//go:build windows

package config

import (
	"io"
	"os"

	apperr "github.com/ThomasCrouzet/restic-defensive-mcp/internal/errors"
)

// readSecretFile opens and reads a secret file after OpenSecretFile already
// rejected symlinks via Lstat. Windows has no portable O_NOFOLLOW equivalent
// used here; re-Stat on the fd still bounds size/mode races after open.
func readSecretFile(path string, maxBytes int64) ([]byte, error) {
	f, err := os.Open(path)
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
	data, err := io.ReadAll(io.LimitReader(f, maxBytes+1))
	if err != nil {
		return nil, apperr.New(apperr.ConfigError, "cannot read secret file").WithCause(err)
	}
	if int64(len(data)) > maxBytes {
		return nil, apperr.New(apperr.ConfigError, "secret file size out of bounds")
	}
	return data, nil
}

// Windows permission bits do not represent the file's ACL. Operators must
// restrict secret files with NTFS ACLs.
func secretFilePermissionsOK(os.FileMode) bool {
	return true
}
