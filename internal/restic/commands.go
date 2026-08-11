package restic

import (
	"fmt"
	"strings"
	"unicode"

	"github.com/ThomasCrouzet/restic-defensive-mcp/internal/policy"
)

// Allowed production subcommands. Anything else is forbidden in the production runner.
const (
	CmdVersion   = "version"
	CmdSnapshots = "snapshots"
	CmdLS        = "ls"
	CmdFind      = "find"
	CmdStats     = "stats"
)

// allowedSubcommands is the closed set of restic subcommands production may
// build. It is private so another package cannot extend it at runtime.
var allowedSubcommands = map[string]struct{}{
	CmdVersion:   {},
	CmdSnapshots: {},
	CmdLS:        {},
	CmdFind:      {},
	CmdStats:     {},
}

var allowedCommandNames = []string{CmdVersion, CmdSnapshots, CmdLS, CmdFind, CmdStats}

// forbiddenSubcommands are explicitly never constructed by production code.
var forbiddenSubcommands = []string{
	"backup", "restore", "forget", "prune", "unlock", "repair", "recover",
	"rewrite", "tag", "key", "init", "copy", "migrate", "check", "dump",
	"cat", "cache", "generate", "options", "self-update", "mount",
	"rebuild-index",
}

// AllowedCommands returns a copy of the closed production command set.
func AllowedCommands() []string {
	return append([]string(nil), allowedCommandNames...)
}

// ForbiddenCommands returns a copy of the explicitly denied command set.
func ForbiddenCommands() []string {
	return append([]string(nil), forbiddenSubcommands...)
}

// CostClass describes operational cost of a tool/command.
type CostClass string

const (
	CostLight     CostClass = "light"
	CostModerate  CostClass = "moderate"
	CostExpensive CostClass = "expensive"
)

// SnapshotIDPattern is hex snapshot ids (full 64 or prefix >= 8).
func ValidSnapshotID(id string) bool {
	if id == "latest" {
		return true
	}
	if len(id) < 8 || len(id) > 64 {
		return false
	}
	for _, c := range id {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') && (c < 'A' || c > 'F') {
			return false
		}
	}
	return true
}

// BuildVersionArgv builds: restic version --json
func BuildVersionArgv() []string {
	return []string{CmdVersion, "--json"}
}

// SnapshotsOpts configures the snapshots command.
type SnapshotsOpts struct {
	Hosts []string
	Tags  []string
	Paths []string
	// SnapshotIDs optional explicit ids
	SnapshotIDs []string
}

// BuildSnapshotsArgv builds a locked argv for `restic snapshots`.
func BuildSnapshotsArgv(opts SnapshotsOpts) ([]string, error) {
	argv := []string{CmdSnapshots, "--json"}
	for _, h := range opts.Hosts {
		if h == "" || strings.IndexFunc(h, unicode.IsControl) >= 0 {
			return nil, fmt.Errorf("invalid host")
		}
		argv = append(argv, "--host", h)
	}
	for _, t := range opts.Tags {
		if t == "" || strings.IndexFunc(t, unicode.IsControl) >= 0 {
			return nil, fmt.Errorf("invalid tag")
		}
		argv = append(argv, "--tag", t)
	}
	for _, p := range opts.Paths {
		cleaned, err := policy.CleanPath(p)
		if err != nil {
			return nil, fmt.Errorf("invalid path")
		}
		argv = append(argv, "--path", cleaned)
	}
	for _, id := range opts.SnapshotIDs {
		if !ValidSnapshotID(id) || id == "latest" {
			return nil, fmt.Errorf("invalid snapshot id")
		}
		argv = append(argv, id)
	}
	return argv, nil
}

// LSOpts configures restic ls.
type LSOpts struct {
	SnapshotID string
	// Dirs are absolute paths within the snapshot to list.
	Dirs      []string
	Recursive bool
}

// BuildLSArgv builds argv for `restic ls`.
func BuildLSArgv(opts LSOpts) ([]string, error) {
	if !ValidSnapshotID(opts.SnapshotID) || opts.SnapshotID == "latest" {
		return nil, fmt.Errorf("invalid snapshot id")
	}
	argv := []string{CmdLS, "--json", opts.SnapshotID}
	if opts.Recursive {
		argv = append(argv, "--recursive")
	}
	for _, d := range opts.Dirs {
		cleaned, err := policy.CleanPath(d)
		if err != nil {
			return nil, fmt.Errorf("ls dir must be absolute")
		}
		argv = append(argv, cleaned)
	}
	return argv, nil
}

// FindOpts configures restic find. Pattern is a single glob-like string, not regex.
type FindOpts struct {
	Pattern     string
	SnapshotIDs []string
	IgnoreCase  bool
}

// BuildFindArgv builds argv for `restic find`.
func BuildFindArgv(opts FindOpts) ([]string, error) {
	if opts.Pattern == "" {
		return nil, fmt.Errorf("pattern required")
	}
	if len(opts.Pattern) > 256 {
		return nil, fmt.Errorf("pattern too long")
	}
	// Reject characters that suggest regex or shell metacharacters beyond simple globs.
	if strings.ContainsAny(opts.Pattern, "(){}|+^$") ||
		strings.IndexFunc(opts.Pattern, unicode.IsControl) >= 0 {
		return nil, fmt.Errorf("pattern contains unsupported characters")
	}
	// Patterns must never look like CLI flags: restic would parse them as global options
	// (e.g. --repo=, -r, --password-file=) if they appear as free-form tokens.
	if err := validateFindPattern(opts.Pattern); err != nil {
		return nil, err
	}
	argv := []string{CmdFind, "--json"}
	if opts.IgnoreCase {
		argv = append(argv, "--ignore-case")
	}
	for _, snapshotID := range opts.SnapshotIDs {
		if !ValidSnapshotID(snapshotID) || snapshotID == "latest" {
			// find uses --snapshot; latest is not documented the same way: reject latest.
			if snapshotID == "latest" {
				return nil, fmt.Errorf("latest is not supported for find; pass a concrete snapshot id")
			}
			if !ValidSnapshotID(snapshotID) {
				return nil, fmt.Errorf("invalid snapshot id")
			}
		}
		argv = append(argv, "--snapshot", snapshotID)
	}
	// End-of-flags marker so restic cannot treat the pattern as a global option
	// even if validation is later relaxed.
	argv = append(argv, "--", opts.Pattern)
	return argv, nil
}

// validateFindPattern rejects flag-shaped or otherwise unsafe find patterns.
func validateFindPattern(pattern string) error {
	if pattern == "" {
		return fmt.Errorf("pattern required")
	}
	// Leading dash is always a flag form in restic/CLI parsing.
	if strings.HasPrefix(pattern, "-") {
		return fmt.Errorf("pattern must not start with '-' (flag-shaped patterns are rejected)")
	}
	if strings.IndexFunc(pattern, unicode.IsControl) >= 0 {
		return fmt.Errorf("pattern contains unsupported characters")
	}
	return nil
}

// StatsMode is an allowlisted restic stats --mode value.
type StatsMode string

const (
	StatsRestoreSize     StatsMode = "restore-size"
	StatsFilesByContents StatsMode = "files-by-contents"
	StatsRawData         StatsMode = "raw-data"
	// blobs-per-file is expensive; still allowlisted but marked expensive.
	StatsBlobsPerFile StatsMode = "blobs-per-file"
)

// ValidStatsMode reports whether mode is allowlisted.
func ValidStatsMode(m string) bool {
	switch StatsMode(m) {
	case StatsRestoreSize, StatsFilesByContents, StatsRawData, StatsBlobsPerFile:
		return true
	default:
		return false
	}
}

// StatsOpts configures restic stats.
type StatsOpts struct {
	Mode        StatsMode
	SnapshotIDs []string
}

// BuildStatsArgv builds argv for `restic stats`.
func BuildStatsArgv(opts StatsOpts) ([]string, error) {
	mode := opts.Mode
	if mode == "" {
		mode = StatsRestoreSize
	}
	if !ValidStatsMode(string(mode)) {
		return nil, fmt.Errorf("invalid stats mode")
	}
	argv := []string{CmdStats, "--json", "--mode", string(mode)}
	for _, id := range opts.SnapshotIDs {
		if !ValidSnapshotID(id) || id == "latest" {
			return nil, fmt.Errorf("invalid snapshot id")
		}
		argv = append(argv, id)
	}
	return argv, nil
}

// deniedGlobalFlags are restic global options that must never appear in production argv
// (exact token match). Callers inject repository/password only via scrubbed child env.
var deniedGlobalFlags = map[string]struct{}{
	"--password-command":     {},
	"-p":                     {},
	"--password":             {},
	"--password-file":        {},
	"--insecure-tls":         {},
	"--insecure-no-password": {},
	"--option":               {},
	"-o":                     {},
	"--cacert":               {},
	"--tls-client-cert":      {},
	"--repo":                 {},
	"-r":                     {},
	"--repository":           {},
	"--repository-file":      {},
	"--cache-dir":            {},
	"--no-lock":              {},
	"--key-hint":             {},
	// find-only modes that escalate beyond simple path globs
	"--blob": {},
	"--pack": {},
	"--tree": {},
}

// deniedGlobalFlagPrefixes catch --flag=value forms of the same options.
var deniedGlobalFlagPrefixes = []string{
	"--password-command=",
	"--password=",
	"--password-file=",
	"--insecure-tls=",
	"--insecure-no-password=",
	"--option=",
	"--cacert=",
	"--tls-client-cert=",
	"--repo=",
	"--repository=",
	"--repository-file=",
	"--cache-dir=",
	"--no-lock=",
	"--key-hint=",
	"-r=",
	"-p=",
	"-o=",
}

// AssertArgvAllowed fails if argv[0] is not an allowed subcommand or if any
// forbidden global flag token appears anywhere in argv.
func AssertArgvAllowed(argv []string) error {
	if len(argv) == 0 {
		return fmt.Errorf("empty argv")
	}
	sub := argv[0]
	if _, ok := allowedSubcommands[sub]; !ok {
		return fmt.Errorf("subcommand not allowed: %s", sub)
	}
	for i := 0; i < len(argv); i++ {
		a := argv[i]
		if a == "" || strings.IndexFunc(a, unicode.IsControl) >= 0 {
			return fmt.Errorf("invalid argv token")
		}
		// Allow the end-of-flags marker and the single positional pattern that follows it.
		if a == "--" {
			// Tokens after "--" are positional only; still reject controls / empty.
			for _, pos := range argv[i+1:] {
				if pos == "" || strings.IndexFunc(pos, unicode.IsControl) >= 0 {
					return fmt.Errorf("invalid positional argument after --")
				}
				// Positional patterns must not be flag-shaped either (defense in depth).
				if strings.HasPrefix(pos, "-") {
					return fmt.Errorf("positional argument must not look like a flag")
				}
			}
			return nil
		}
		if _, bad := deniedGlobalFlags[a]; bad {
			return fmt.Errorf("global flag not allowed: %s", a)
		}
		for _, p := range deniedGlobalFlagPrefixes {
			if strings.HasPrefix(a, p) {
				return fmt.Errorf("global flag not allowed")
			}
		}
	}
	return nil
}
