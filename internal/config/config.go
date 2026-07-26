// Package config loads and validates the YAML configuration file.
package config

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode"

	"gopkg.in/yaml.v3"

	apperr "github.com/ThomasCrouzet/restic-defensive-mcp/internal/errors"
	"github.com/ThomasCrouzet/restic-defensive-mcp/internal/policy"
)

const (
	// SchemaVersion is the only supported config version.
	SchemaVersion = 1

	defaultCommandTimeout          = 30 * time.Second
	defaultExpensiveCommandTimeout = 5 * time.Minute
	defaultMaxSnapshots            = 100
	defaultMaxNodes                = 500
	defaultMaxFindResults          = 200
	defaultMaxOutputBytes          = 8 << 20 // 8 MiB
	defaultMaxConcurrentPerRepo    = 1
	defaultMaxConcurrentGlobal     = 4

	minCommandTimeout   = 1 * time.Second
	maxCommandTimeout   = 2 * time.Minute
	minExpensiveTimeout = 10 * time.Second
	maxExpensiveTimeout = 15 * time.Minute
	minMaxSnapshots     = 1
	maxMaxSnapshots     = 1000
	minMaxNodes         = 1
	maxMaxNodes         = 5000
	minMaxFindResults   = 1
	maxMaxFindResults   = 2000
	minMaxOutputBytes   = 64 << 10 // 64 KiB
	maxMaxOutputBytes   = 32 << 20 // 32 MiB
	maxSecretFileBytes  = 4 << 10  // 4 KiB
	maxRepositories     = 32
	maxAllowlistEntries = 256
	maxEnvFiles         = 16
)

var idPattern = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,63}$`)

// Config is the root configuration document.
type Config struct {
	Version      int          `yaml:"version"`
	ResticBinary string       `yaml:"restic_binary"`
	Limits       Limits       `yaml:"limits"`
	Repositories []Repository `yaml:"repositories"`
}

// Limits bounds runtime behaviour. Zero values become defaults; non-zero values
// outside absolute min/max are rejected (not silently clamped). Values may be
// raised above defaults only up to those ceilings.
type Limits struct {
	CommandTimeout          time.Duration `yaml:"command_timeout"`
	ExpensiveCommandTimeout time.Duration `yaml:"expensive_command_timeout"`
	MaxSnapshots            int           `yaml:"max_snapshots"`
	MaxNodes                int           `yaml:"max_nodes"`
	MaxFindResults          int           `yaml:"max_find_results"`
	MaxOutputBytes          int           `yaml:"max_output_bytes"`
	MaxConcurrentPerRepo    int           `yaml:"max_concurrent_per_repo"`
	MaxConcurrentGlobal     int           `yaml:"max_concurrent_global"`
}

// Repository is one pre-declared restic repository. Callers may only reference ID.
type Repository struct {
	ID             string   `yaml:"id"`
	Label          string   `yaml:"label"`
	RepositoryFile string   `yaml:"repository_file"`
	PasswordFile   string   `yaml:"password_file"`
	CacheDir       string   `yaml:"cache_dir"`
	AllowedHosts   []string `yaml:"allowed_hosts"`
	AllowedTags    []string `yaml:"allowed_tags"`
	AllowedPaths   []string `yaml:"allowed_paths"`
	// EnvFiles lists optional key=value files for backend credentials (AWS_*, B2_*, etc.).
	// Never RESTIC_PASSWORD or RESTIC_REPOSITORY.
	EnvFiles []string `yaml:"env_files"`
}

// Load reads and validates a YAML config from path.
func Load(path string) (*Config, error) {
	if path == "" {
		return nil, apperr.New(apperr.ConfigError, "config path is required")
	}
	// Clean for open only; do not follow into surprising locations beyond OS open.
	clean := filepath.Clean(path)
	info, err := os.Lstat(clean)
	if err != nil {
		return nil, apperr.New(apperr.ConfigError, "cannot read config file").WithCause(err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return nil, apperr.New(apperr.ConfigError, "config path must not be a symlink")
	}
	if !info.Mode().IsRegular() {
		return nil, apperr.New(apperr.ConfigError, "config path must be a regular file")
	}
	if info.Size() > 1<<20 {
		return nil, apperr.New(apperr.ConfigError, "config file too large")
	}
	data, err := os.ReadFile(clean)
	if err != nil {
		return nil, apperr.New(apperr.ConfigError, "cannot read config file").WithCause(err)
	}
	return Parse(data)
}

// Parse unmarshals and validates YAML bytes.
func Parse(data []byte) (*Config, error) {
	var raw Config
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&raw); err != nil {
		return nil, apperr.New(apperr.ConfigError, "invalid YAML").WithCause(err)
	}
	var trailing any
	if err := dec.Decode(&trailing); err != io.EOF {
		if err == nil {
			return nil, apperr.New(apperr.ConfigError, "multiple YAML documents are not allowed")
		}
		return nil, apperr.New(apperr.ConfigError, "invalid trailing YAML").WithCause(err)
	}
	if err := raw.Validate(); err != nil {
		return nil, err
	}
	return &raw, nil
}

// Validate checks structural and policy constraints. Messages never embed secret contents.
func (c *Config) Validate() error {
	if c.Version != SchemaVersion {
		return apperr.New(apperr.ConfigError, fmt.Sprintf("unsupported config version %d (want %d)", c.Version, SchemaVersion))
	}
	c.applyLimitDefaults()
	if err := c.validateLimits(); err != nil {
		return err
	}
	if len(c.Repositories) == 0 {
		return apperr.New(apperr.ConfigError, "at least one repository is required")
	}
	if len(c.Repositories) > maxRepositories {
		return apperr.New(apperr.ConfigError, "too many repositories")
	}
	seen := make(map[string]struct{}, len(c.Repositories))
	for i := range c.Repositories {
		r := &c.Repositories[i]
		if err := validateRepository(r, i); err != nil {
			return err
		}
		if _, ok := seen[r.ID]; ok {
			return apperr.New(apperr.ConfigError, "duplicate repository id").WithDetail(r.ID)
		}
		seen[r.ID] = struct{}{}
	}
	return nil
}

func (c *Config) applyLimitDefaults() {
	if c.Limits.CommandTimeout == 0 {
		c.Limits.CommandTimeout = defaultCommandTimeout
	}
	if c.Limits.ExpensiveCommandTimeout == 0 {
		c.Limits.ExpensiveCommandTimeout = defaultExpensiveCommandTimeout
	}
	if c.Limits.MaxSnapshots == 0 {
		c.Limits.MaxSnapshots = defaultMaxSnapshots
	}
	if c.Limits.MaxNodes == 0 {
		c.Limits.MaxNodes = defaultMaxNodes
	}
	if c.Limits.MaxFindResults == 0 {
		c.Limits.MaxFindResults = defaultMaxFindResults
	}
	if c.Limits.MaxOutputBytes == 0 {
		c.Limits.MaxOutputBytes = defaultMaxOutputBytes
	}
	if c.Limits.MaxConcurrentPerRepo == 0 {
		c.Limits.MaxConcurrentPerRepo = defaultMaxConcurrentPerRepo
	}
	if c.Limits.MaxConcurrentGlobal == 0 {
		c.Limits.MaxConcurrentGlobal = defaultMaxConcurrentGlobal
	}
}

func (c *Config) validateLimits() error {
	l := &c.Limits
	if l.CommandTimeout < minCommandTimeout || l.CommandTimeout > maxCommandTimeout {
		return apperr.New(apperr.ConfigError, "limits.command_timeout out of range")
	}
	if l.ExpensiveCommandTimeout < minExpensiveTimeout || l.ExpensiveCommandTimeout > maxExpensiveTimeout {
		return apperr.New(apperr.ConfigError, "limits.expensive_command_timeout out of range")
	}
	if l.ExpensiveCommandTimeout < l.CommandTimeout {
		return apperr.New(apperr.ConfigError, "limits.expensive_command_timeout must be >= command_timeout")
	}
	if l.MaxSnapshots < minMaxSnapshots || l.MaxSnapshots > maxMaxSnapshots {
		return apperr.New(apperr.ConfigError, "limits.max_snapshots out of range")
	}
	if l.MaxNodes < minMaxNodes || l.MaxNodes > maxMaxNodes {
		return apperr.New(apperr.ConfigError, "limits.max_nodes out of range")
	}
	if l.MaxFindResults < minMaxFindResults || l.MaxFindResults > maxMaxFindResults {
		return apperr.New(apperr.ConfigError, "limits.max_find_results out of range")
	}
	if l.MaxOutputBytes < minMaxOutputBytes || l.MaxOutputBytes > maxMaxOutputBytes {
		return apperr.New(apperr.ConfigError, "limits.max_output_bytes out of range")
	}
	if l.MaxConcurrentPerRepo < 1 || l.MaxConcurrentPerRepo > 4 {
		return apperr.New(apperr.ConfigError, "limits.max_concurrent_per_repo out of range")
	}
	if l.MaxConcurrentGlobal < 1 || l.MaxConcurrentGlobal > 16 {
		return apperr.New(apperr.ConfigError, "limits.max_concurrent_global out of range")
	}
	return nil
}

func validateRepository(r *Repository, index int) error {
	loc := fmt.Sprintf("repositories[%d]", index)
	if !idPattern.MatchString(r.ID) {
		return apperr.New(apperr.ConfigError, loc+": invalid id (use [a-z][a-z0-9_-]{0,63})")
	}
	if r.RepositoryFile == "" {
		return apperr.New(apperr.ConfigError, loc+": repository_file is required")
	}
	if r.PasswordFile == "" {
		return apperr.New(apperr.ConfigError, loc+": password_file is required")
	}
	if err := validateSecretFilePath(r.RepositoryFile, loc+".repository_file"); err != nil {
		return err
	}
	r.RepositoryFile = filepath.Clean(r.RepositoryFile)
	if err := validateSecretFilePath(r.PasswordFile, loc+".password_file"); err != nil {
		return err
	}
	r.PasswordFile = filepath.Clean(r.PasswordFile)
	if len(r.AllowedHosts) > maxAllowlistEntries ||
		len(r.AllowedTags) > maxAllowlistEntries ||
		len(r.AllowedPaths) > maxAllowlistEntries {
		return apperr.New(apperr.ConfigError, loc+": allowlist has too many entries")
	}
	if len(r.EnvFiles) > maxEnvFiles {
		return apperr.New(apperr.ConfigError, loc+": too many env_files")
	}
	if r.CacheDir != "" {
		if !filepath.IsAbs(r.CacheDir) {
			return apperr.New(apperr.ConfigError, loc+": cache_dir must be absolute")
		}
		if hasParentPathSegment(r.CacheDir) || strings.IndexFunc(r.CacheDir, unicode.IsControl) >= 0 {
			return apperr.New(apperr.ConfigError, loc+": cache_dir is invalid")
		}
		r.CacheDir = filepath.Clean(r.CacheDir)
	}
	for i, p := range r.AllowedPaths {
		cleaned, err := policy.CleanPath(p)
		if err != nil {
			return apperr.New(apperr.ConfigError, fmt.Sprintf("%s.allowed_paths[%d]: invalid path", loc, i)).WithCause(err)
		}
		r.AllowedPaths[i] = cleaned
	}
	for i, h := range r.AllowedHosts {
		h = strings.TrimSpace(h)
		if h == "" || len(h) > 253 || strings.IndexFunc(h, unicode.IsControl) >= 0 {
			return apperr.New(apperr.ConfigError, fmt.Sprintf("%s.allowed_hosts[%d]: invalid host", loc, i))
		}
		r.AllowedHosts[i] = h
	}
	for i, t := range r.AllowedTags {
		t = strings.TrimSpace(t)
		if t == "" || len(t) > 128 || strings.IndexFunc(t, unicode.IsControl) >= 0 {
			return apperr.New(apperr.ConfigError, fmt.Sprintf("%s.allowed_tags[%d]: invalid tag", loc, i))
		}
		r.AllowedTags[i] = t
	}
	for i, ef := range r.EnvFiles {
		if err := validateSecretFilePath(ef, fmt.Sprintf("%s.env_files[%d]", loc, i)); err != nil {
			return err
		}
		r.EnvFiles[i] = filepath.Clean(ef)
	}
	if len(r.Label) > 128 || strings.IndexFunc(r.Label, unicode.IsControl) >= 0 {
		return apperr.New(apperr.ConfigError, loc+": invalid label")
	}
	return nil
}

func validateSecretFilePath(p, field string) error {
	if p == "" {
		return apperr.New(apperr.ConfigError, field+": path is required")
	}
	if !filepath.IsAbs(p) {
		return apperr.New(apperr.ConfigError, field+": path must be absolute")
	}
	if hasParentPathSegment(p) || strings.IndexFunc(p, unicode.IsControl) >= 0 {
		return apperr.New(apperr.ConfigError, field+": invalid path")
	}
	return nil
}

func hasParentPathSegment(p string) bool {
	for _, segment := range strings.FieldsFunc(p, func(r rune) bool {
		return r == '/' || r == '\\'
	}) {
		if segment == ".." {
			return true
		}
	}
	return false
}

// EmptyAllowlistWarnings returns operator-facing warnings when any repository
// leave hosts, tags, or paths unrestricted (empty list = no restriction).
// Semantics are unchanged; this is a boot-time foot-gun signal only.
func (c *Config) EmptyAllowlistWarnings() []string {
	if c == nil {
		return nil
	}
	var out []string
	for _, r := range c.Repositories {
		id := r.ID
		if id == "" {
			id = "?"
		}
		if len(r.AllowedHosts) == 0 {
			out = append(out, fmt.Sprintf("repository %q: allowed_hosts is empty (all snapshot hosts visible)", id))
		}
		if len(r.AllowedTags) == 0 {
			out = append(out, fmt.Sprintf("repository %q: allowed_tags is empty (no tag restriction on visibility filters)", id))
		}
		if len(r.AllowedPaths) == 0 {
			out = append(out, fmt.Sprintf("repository %q: allowed_paths is empty (all snapshot path metadata is visible to MCP clients)", id))
		}
	}
	return out
}

// OpenSecretFile validates and reads a secret file: regular, non-symlink, size-bounded,
// and preferably mode 0600 (or stricter) when the OS reports permissions.
// On Unix it opens with O_NOFOLLOW and re-checks metadata on the open fd to reduce
// symlink TOCTOU between Lstat and read.
func OpenSecretFile(path string) ([]byte, error) {
	if err := validateSecretFilePath(path, "secret"); err != nil {
		return nil, err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return nil, apperr.New(apperr.ConfigError, "secret file not accessible").WithCause(err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return nil, apperr.New(apperr.ConfigError, "secret file must not be a symlink")
	}
	if !info.Mode().IsRegular() {
		return nil, apperr.New(apperr.ConfigError, "secret file must be a regular file")
	}
	if info.Size() <= 0 || info.Size() > maxSecretFileBytes {
		return nil, apperr.New(apperr.ConfigError, "secret file size out of bounds")
	}
	// On Unix, reject world/group-readable files. Windows relies on filesystem ACLs.
	if !secretFilePermissionsOK(info.Mode()) {
		return nil, apperr.New(apperr.ConfigError, "secret file permissions must be 0600 or stricter")
	}

	data, err := readSecretFile(path, maxSecretFileBytes)
	if err != nil {
		return nil, err
	}
	return data, nil
}

// ReadRepositoryLocation reads and trims the repository URL/path from repository_file.
// The value is used only to classify the backend and is never returned to MCP clients.
func ReadRepositoryLocation(path string) (string, error) {
	data, err := OpenSecretFile(path)
	if err != nil {
		return "", err
	}
	loc := strings.TrimSpace(string(data))
	if loc == "" {
		return "", apperr.New(apperr.ConfigError, "repository_file is empty")
	}
	if len(loc) > 2048 {
		return "", apperr.New(apperr.ConfigError, "repository location too long")
	}
	if strings.IndexFunc(loc, unicode.IsControl) >= 0 {
		return "", apperr.New(apperr.ConfigError, "repository location contains control characters")
	}
	return loc, nil
}
