// Package policy enforces host, tag, and path allowlists.
package policy

import (
	"path"
	"strings"
	"unicode"

	apperr "github.com/ThomasCrouzet/restic-defensive-mcp/internal/errors"
)

const (
	maxPathBytes     = 4096
	maxHostBytes     = 253
	maxTagBytes      = 128
	maxRequestedTags = 32
)

// PathPolicy validates snapshot-relative absolute paths against an allowlist.
// Comparison is lexical only: no host filesystem resolution, no symlink follow.
type PathPolicy struct {
	// Allowed is a list of cleaned absolute paths (POSIX-style, leading '/').
	// Empty means no path restriction (still requires absolute cleaned paths).
	Allowed []string
}

// CleanPath lexically cleans a path and rejects empty, relative, or Windows drive forms.
// Paths inside restic snapshots use forward slashes.
func CleanPath(p string) (string, error) {
	if p == "" {
		return "", apperr.New(apperr.InvalidArgument, "path must not be empty")
	}
	if len(p) > maxPathBytes {
		return "", apperr.New(apperr.InvalidArgument, "path is too long")
	}
	// Reject null bytes and backslashes (restic uses '/').
	if strings.Contains(p, "\\") || strings.IndexFunc(p, unicode.IsControl) >= 0 {
		return "", apperr.New(apperr.InvalidArgument, "path contains illegal characters")
	}
	if !strings.HasPrefix(p, "/") {
		return "", apperr.New(apperr.InvalidArgument, "path must be absolute")
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == ".." {
			return "", apperr.New(apperr.InvalidArgument, "path must not contain parent segments")
		}
	}
	cleaned := path.Clean(p)
	if cleaned == "." || cleaned == "" {
		return "", apperr.New(apperr.InvalidArgument, "path is invalid after cleaning")
	}
	if !strings.HasPrefix(cleaned, "/") {
		return "", apperr.New(apperr.InvalidArgument, "path must remain absolute after cleaning")
	}
	return cleaned, nil
}

// IsAllowed reports whether cleaned path is under at least one allowed prefix.
// If Allowed is empty, any cleaned absolute path is allowed.
func (pp PathPolicy) IsAllowed(cleaned string) bool {
	if len(pp.Allowed) == 0 {
		return true
	}
	for _, a := range pp.Allowed {
		if pathUnder(cleaned, a) {
			return true
		}
	}
	return false
}

// Check returns nil if path is clean and allowed.
func (pp PathPolicy) Check(raw string) (string, error) {
	cleaned, err := CleanPath(raw)
	if err != nil {
		return "", err
	}
	if !pp.IsAllowed(cleaned) {
		return "", apperr.New(apperr.NotAllowed, "path is outside the configured allowlist")
	}
	return cleaned, nil
}

// FilterPaths returns the visible intersection between snapshot roots and the
// configured allowlist. If a snapshot root is broader than an allowed path,
// only the narrower allowed path is returned.
func (pp PathPolicy) FilterPaths(paths []string) []string {
	if len(paths) == 0 {
		return nil
	}
	seen := make(map[string]struct{})
	out := make([]string, 0, len(paths))
	for _, p := range paths {
		cleaned, err := CleanPath(p)
		if err != nil {
			continue
		}
		if len(pp.Allowed) == 0 {
			if _, duplicate := seen[cleaned]; duplicate {
				continue
			}
			seen[cleaned] = struct{}{}
			out = append(out, cleaned)
			continue
		}
		for _, allowed := range pp.Allowed {
			visible := ""
			switch {
			case pathUnder(cleaned, allowed):
				visible = cleaned
			case pathUnder(allowed, cleaned):
				visible = allowed
			}
			if visible == "" {
				continue
			}
			if _, duplicate := seen[visible]; duplicate {
				continue
			}
			seen[visible] = struct{}{}
			out = append(out, visible)
		}
	}
	return out
}

// pathUnder reports whether child is equal to parent or a descendant.
func pathUnder(child, parent string) bool {
	if child == parent {
		return true
	}
	if parent == "/" {
		return strings.HasPrefix(child, "/")
	}
	prefix := parent + "/"
	return strings.HasPrefix(child, prefix)
}

// HostPolicy allowlists snapshot hostnames.
type HostPolicy struct {
	Allowed []string // empty = any host
	set     map[string]struct{}
}

// NewHostPolicy builds a host policy (case-sensitive exact match).
func NewHostPolicy(allowed []string) HostPolicy {
	hp := HostPolicy{Allowed: append([]string(nil), allowed...)}
	if len(allowed) > 0 {
		hp.set = make(map[string]struct{}, len(allowed))
		for _, h := range allowed {
			hp.set[h] = struct{}{}
		}
	}
	return hp
}

// Check returns nil if host is allowed. Empty host is rejected when allowlist is set.
func (hp HostPolicy) Check(host string) error {
	if host != "" && (len(host) > maxHostBytes || strings.IndexFunc(host, unicode.IsControl) >= 0) {
		return apperr.New(apperr.InvalidArgument, "host is invalid")
	}
	if len(hp.set) == 0 {
		return nil
	}
	if host == "" {
		return apperr.New(apperr.NotAllowed, "host filter is required by repository policy")
	}
	if _, ok := hp.set[host]; !ok {
		return apperr.New(apperr.NotAllowed, "host is outside the configured allowlist")
	}
	return nil
}

// Allows reports whether host is permitted (empty host allowed only when no allowlist).
func (hp HostPolicy) Allows(host string) bool {
	return hp.Check(host) == nil
}

// TagPolicy allowlists snapshot tags.
type TagPolicy struct {
	Allowed []string
	set     map[string]struct{}
}

// NewTagPolicy builds a tag policy.
func NewTagPolicy(allowed []string) TagPolicy {
	tp := TagPolicy{Allowed: append([]string(nil), allowed...)}
	if len(allowed) > 0 {
		tp.set = make(map[string]struct{}, len(allowed))
		for _, t := range allowed {
			tp.set[t] = struct{}{}
		}
	}
	return tp
}

// CheckEach verifies every requested tag is allowlisted.
func (tp TagPolicy) CheckEach(tags []string) error {
	if len(tags) > maxRequestedTags {
		return apperr.New(apperr.InvalidArgument, "too many tag filters")
	}
	for _, t := range tags {
		if t == "" || len(t) > maxTagBytes || strings.IndexFunc(t, unicode.IsControl) >= 0 {
			return apperr.New(apperr.InvalidArgument, "tag is invalid")
		}
		if len(tp.set) > 0 {
			if _, ok := tp.set[t]; ok {
				continue
			}
			return apperr.New(apperr.NotAllowed, "tag is outside the configured allowlist")
		}
	}
	return nil
}

// FilterTags keeps only allowlisted tags from a snapshot's tags.
func (tp TagPolicy) FilterTags(tags []string) []string {
	if len(tags) == 0 {
		return nil
	}
	if len(tp.set) == 0 {
		return append([]string(nil), tags...)
	}
	out := make([]string, 0, len(tags))
	for _, t := range tags {
		if _, ok := tp.set[t]; ok {
			out = append(out, t)
		}
	}
	return out
}

// SnapshotVisible reports whether a snapshot may be shown given host/tag/path policies.
// A snapshot is visible if:
//   - host is allowed (or no host allowlist)
//   - if tag allowlist is set, at least one snapshot tag is allowlisted (empty tags → not visible)
//   - if path allowlist is set, at least one snapshot path is under an allowed prefix
func SnapshotVisible(host string, tags, paths []string, hosts HostPolicy, tagp TagPolicy, pathsPol PathPolicy) bool {
	if !hosts.Allows(host) {
		return false
	}
	if len(tagp.set) > 0 {
		matched := false
		for _, t := range tags {
			if _, ok := tagp.set[t]; ok {
				matched = true
				break
			}
		}
		if !matched {
			return false
		}
	}
	if len(pathsPol.Allowed) > 0 {
		if len(pathsPol.FilterPaths(paths)) == 0 {
			return false
		}
	}
	return true
}
