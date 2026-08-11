package restic

import (
	"bufio"
	"bytes"
	"encoding/json"
	"sort"
	"strings"
	"time"

	apperr "github.com/ThomasCrouzet/restic-defensive-mcp/internal/errors"
	"github.com/ThomasCrouzet/restic-defensive-mcp/internal/policy"
	"github.com/ThomasCrouzet/restic-defensive-mcp/internal/redaction"
)

// VersionInfo is restic version --json output.
type VersionInfo struct {
	MessageType string `json:"message_type"`
	Version     string `json:"version"`
	GoVersion   string `json:"go_version"`
	GoOS        string `json:"go_os"`
	GoArch      string `json:"go_arch"`
}

// Snapshot is a filtered snapshot metadata object.
type Snapshot struct {
	Time     time.Time `json:"time"`
	Parent   string    `json:"parent,omitempty"`
	Tree     string    `json:"tree,omitempty"`
	Paths    []string  `json:"paths"`
	Hostname string    `json:"hostname"`
	Username string    `json:"username,omitempty"`
	Tags     []string  `json:"tags,omitempty"`
	ID       string    `json:"id"`
	// ProgramVersion omitted from default client view if desired: keep for diagnostics.
	ProgramVersion string           `json:"program_version,omitempty"`
	Summary        *SnapshotSummary `json:"summary,omitempty"`
}

// SnapshotSummary is optional backup-time stats.
type SnapshotSummary struct {
	BackupStart         time.Time `json:"backup_start,omitempty"`
	BackupEnd           time.Time `json:"backup_end,omitempty"`
	FilesNew            uint64    `json:"files_new,omitempty"`
	FilesChanged        uint64    `json:"files_changed,omitempty"`
	FilesUnmodified     uint64    `json:"files_unmodified,omitempty"`
	DirsNew             uint64    `json:"dirs_new,omitempty"`
	DirsChanged         uint64    `json:"dirs_changed,omitempty"`
	DirsUnmodified      uint64    `json:"dirs_unmodified,omitempty"`
	DataAdded           uint64    `json:"data_added,omitempty"`
	DataAddedPacked     uint64    `json:"data_added_packed,omitempty"`
	TotalFilesProcessed uint64    `json:"total_files_processed,omitempty"`
	TotalBytesProcessed uint64    `json:"total_bytes_processed,omitempty"`
}

// rawSnapshot for flexible JSON decode.
type rawSnapshot struct {
	Time           time.Time        `json:"time"`
	Parent         string           `json:"parent"`
	Tree           string           `json:"tree"`
	Paths          []string         `json:"paths"`
	Hostname       string           `json:"hostname"`
	Username       string           `json:"username"`
	Tags           []string         `json:"tags"`
	ID             string           `json:"id"`
	ShortID        string           `json:"short_id"`
	ProgramVersion string           `json:"program_version"`
	Summary        *SnapshotSummary `json:"summary"`
}

// Node is a directory entry from restic ls (no file content).
type Node struct {
	Name    string    `json:"name"`
	Type    string    `json:"type"`
	Path    string    `json:"path"`
	Size    uint64    `json:"size,omitempty"`
	Mode    string    `json:"permissions,omitempty"`
	ModTime time.Time `json:"mtime,omitempty"`
}

type rawLSMessage struct {
	MessageType string    `json:"message_type"`
	StructType  string    `json:"struct_type"`
	Name        string    `json:"name"`
	Type        string    `json:"type"`
	Path        string    `json:"path"`
	Size        uint64    `json:"size"`
	Permissions string    `json:"permissions"`
	Mtime       time.Time `json:"mtime"`
	ID          string    `json:"id"`
}

// FindGroup is one snapshot's find hits.
type FindGroup struct {
	Hits     uint64      `json:"hits"`
	Snapshot string      `json:"snapshot"`
	Matches  []FindMatch `json:"matches"`
}

// FindMatch is a single find result (metadata only).
type FindMatch struct {
	Path  string    `json:"path"`
	Name  string    `json:"name"`
	Type  string    `json:"type"`
	Size  uint64    `json:"size,omitempty"`
	Mtime time.Time `json:"mtime,omitempty"`
}

// StatsResult is restic stats --json.
type StatsResult struct {
	TotalSize              uint64  `json:"total_size"`
	TotalFileCount         uint64  `json:"total_file_count"`
	TotalBlobCount         uint64  `json:"total_blob_count,omitempty"`
	SnapshotsCount         uint64  `json:"snapshots_count"`
	TotalUncompressedSize  uint64  `json:"total_uncompressed_size,omitempty"`
	CompressionRatio       float64 `json:"compression_ratio,omitempty"`
	CompressionProgress    float64 `json:"compression_progress,omitempty"`
	CompressionSpaceSaving float64 `json:"compression_space_saving,omitempty"`
}

// ParseVersion parses restic version --json.
func ParseVersion(data []byte) (*VersionInfo, error) {
	data = bytes.TrimSpace(data)
	if len(data) == 0 {
		return nil, apperr.New(apperr.ProtocolError, "empty version output")
	}
	var v VersionInfo
	if err := json.Unmarshal(data, &v); err != nil {
		return nil, apperr.New(apperr.ProtocolError, "invalid version JSON").WithCause(err)
	}
	if v.Version == "" {
		return nil, apperr.New(apperr.ProtocolError, "version field missing")
	}
	v.Version = redaction.SanitizeString(v.Version)
	return &v, nil
}

// ParseSnapshots parses restic snapshots --json array.
func ParseSnapshots(data []byte) ([]Snapshot, error) {
	data = bytes.TrimSpace(data)
	if len(data) == 0 {
		return nil, nil
	}
	var raw []rawSnapshot
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, apperr.New(apperr.ProtocolError, "invalid snapshots JSON").WithCause(err)
	}
	out := make([]Snapshot, 0, len(raw))
	for _, r := range raw {
		s, err := normalizeSnapshot(r)
		if err != nil {
			return nil, apperr.New(apperr.ProtocolError, "invalid snapshot entry").WithCause(err)
		}
		out = append(out, s)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Time.Equal(out[j].Time) {
			return out[i].ID < out[j].ID
		}
		return out[i].Time.After(out[j].Time) // newest first
	})
	return out, nil
}

func normalizeSnapshot(r rawSnapshot) (Snapshot, error) {
	id := r.ID
	if id == "" {
		id = r.ShortID
	}
	if id == "" {
		return Snapshot{}, apperr.New(apperr.ProtocolError, "snapshot missing id")
	}
	if redaction.SanitizeString(id) != id || !ValidSnapshotID(id) || id == "latest" {
		return Snapshot{}, apperr.New(apperr.ProtocolError, "snapshot has invalid id")
	}
	if len(r.Hostname) > 253 || redaction.SanitizeString(r.Hostname) != r.Hostname {
		return Snapshot{}, apperr.New(apperr.ProtocolError, "snapshot has invalid hostname")
	}
	if len(r.Paths) > 256 {
		return Snapshot{}, apperr.New(apperr.ProtocolError, "snapshot has too many paths")
	}
	paths := make([]string, 0, len(r.Paths))
	for _, p := range r.Paths {
		cleaned, err := policy.CleanPath(p)
		if err != nil {
			return Snapshot{}, apperr.New(apperr.ProtocolError, "snapshot has invalid path").WithCause(err)
		}
		paths = append(paths, cleaned)
	}
	tags := make([]string, 0, len(r.Tags))
	if len(r.Tags) > 256 {
		return Snapshot{}, apperr.New(apperr.ProtocolError, "snapshot has too many tags")
	}
	for _, t := range r.Tags {
		if t == "" || len(t) > 128 || redaction.SanitizeString(t) != t {
			return Snapshot{}, apperr.New(apperr.ProtocolError, "snapshot has invalid tag")
		}
		tags = append(tags, t)
	}
	sort.Strings(tags)
	t := r.Time.UTC()
	var sum *SnapshotSummary
	if r.Summary != nil {
		cp := *r.Summary
		if !cp.BackupStart.IsZero() {
			cp.BackupStart = cp.BackupStart.UTC()
		}
		if !cp.BackupEnd.IsZero() {
			cp.BackupEnd = cp.BackupEnd.UTC()
		}
		sum = &cp
	}
	return Snapshot{
		Time:           t,
		Parent:         redaction.SanitizeString(r.Parent),
		Tree:           redaction.SanitizeString(r.Tree),
		Paths:          paths,
		Hostname:       r.Hostname,
		Username:       redaction.SanitizeString(r.Username),
		Tags:           tags,
		ID:             id,
		ProgramVersion: redaction.SanitizeString(r.ProgramVersion),
		Summary:        sum,
	}, nil
}

// ParseLS parses restic ls --json JSON lines into nodes (skips snapshot header).
func ParseLS(data []byte, maxNodes int) (nodes []Node, truncated bool, err error) {
	if maxNodes <= 0 {
		maxNodes = 500
	}
	sc := bufio.NewScanner(bytes.NewReader(data))
	// Increase scanner buffer for long paths.
	buf := make([]byte, 0, 64*1024)
	sc.Buffer(buf, 1024*1024)

	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var msg rawLSMessage
		if err := json.Unmarshal(line, &msg); err != nil {
			return nodes, truncated, apperr.New(apperr.ProtocolError, "invalid ls JSON line").WithCause(err)
		}
		mt := msg.MessageType
		if mt == "" {
			mt = msg.StructType
		}
		if mt == "snapshot" {
			continue
		}
		if mt != "node" {
			continue
		}
		if msg.Path == "" {
			return nodes, truncated, apperr.New(apperr.ProtocolError, "ls node missing path")
		}
		cleanedPath, err := policy.CleanPath(msg.Path)
		if err != nil {
			return nodes, truncated, apperr.New(apperr.ProtocolError, "ls node has invalid path").WithCause(err)
		}
		n := Node{
			Name:    redaction.SanitizeString(msg.Name),
			Type:    redaction.SanitizeString(msg.Type),
			Path:    cleanedPath,
			Size:    msg.Size,
			Mode:    redaction.SanitizeString(msg.Permissions),
			ModTime: msg.Mtime.UTC(),
		}
		if n.Path == "" {
			continue
		}
		if len(nodes) >= maxNodes {
			truncated = true
			break
		}
		nodes = append(nodes, n)
	}
	if err := sc.Err(); err != nil {
		return nodes, truncated, apperr.New(apperr.ProtocolError, "failed reading ls output").WithCause(err)
	}
	sort.SliceStable(nodes, func(i, j int) bool {
		return nodes[i].Path < nodes[j].Path
	})
	return nodes, truncated, nil
}

// ParseFind parses restic find --json.
func ParseFind(data []byte, maxResults int) (groups []FindGroup, total int, truncated bool, err error) {
	if maxResults <= 0 {
		maxResults = 200
	}
	data = bytes.TrimSpace(data)
	if len(data) == 0 {
		return nil, 0, false, nil
	}
	var raw []struct {
		Hits     uint64 `json:"hits"`
		Snapshot string `json:"snapshot"`
		Matches  []struct {
			Path  string    `json:"path"`
			Name  string    `json:"name"`
			Type  string    `json:"type"`
			Size  uint64    `json:"size"`
			Mtime time.Time `json:"mtime"`
		} `json:"matches"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, 0, false, apperr.New(apperr.ProtocolError, "invalid find JSON").WithCause(err)
	}
	totalMatched := 0
	for _, g := range raw {
		if !ValidSnapshotID(g.Snapshot) || g.Snapshot == "latest" {
			return nil, 0, false, apperr.New(apperr.ProtocolError, "find group has invalid snapshot id")
		}
		fg := FindGroup{
			Hits:     g.Hits,
			Snapshot: redaction.SanitizeString(g.Snapshot),
		}
		for _, m := range g.Matches {
			if m.Path == "" {
				return nil, 0, false, apperr.New(apperr.ProtocolError, "find match missing path")
			}
			cleanedPath, err := policy.CleanPath(m.Path)
			if err != nil {
				return nil, 0, false, apperr.New(apperr.ProtocolError, "find match has invalid path").WithCause(err)
			}
			if totalMatched >= maxResults {
				truncated = true
				break
			}
			fg.Matches = append(fg.Matches, FindMatch{
				Path:  cleanedPath,
				Name:  redaction.SanitizeString(m.Name),
				Type:  redaction.SanitizeString(m.Type),
				Size:  m.Size,
				Mtime: m.Mtime.UTC(),
			})
			totalMatched++
		}
		if len(fg.Matches) > 0 || g.Hits > 0 {
			groups = append(groups, fg)
		}
		if truncated {
			break
		}
	}
	return groups, totalMatched, truncated, nil
}

// ParseStats parses restic stats --json.
func ParseStats(data []byte) (*StatsResult, error) {
	data = bytes.TrimSpace(data)
	if len(data) == 0 {
		return nil, apperr.New(apperr.ProtocolError, "empty stats output")
	}
	var s StatsResult
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, apperr.New(apperr.ProtocolError, "invalid stats JSON").WithCause(err)
	}
	return &s, nil
}

// CompareVersion returns -1, 0, 1 for a vs b (semver-ish major.minor.patch prefixes).
func CompareVersion(a, b string) int {
	pa := parseVer(a)
	pb := parseVer(b)
	for i := 0; i < 3; i++ {
		if pa[i] < pb[i] {
			return -1
		}
		if pa[i] > pb[i] {
			return 1
		}
	}
	return 0
}

func parseVer(v string) [3]int {
	v = strings.TrimPrefix(v, "v")
	// strip pre-release
	if i := strings.IndexAny(v, "-+"); i >= 0 {
		v = v[:i]
	}
	parts := strings.Split(v, ".")
	var out [3]int
	for i := 0; i < 3 && i < len(parts); i++ {
		n := 0
		for _, c := range parts[i] {
			if c < '0' || c > '9' {
				break
			}
			n = n*10 + int(c-'0')
		}
		out[i] = n
	}
	return out
}

// MinResticVersion is the minimum supported restic version.
const MinResticVersion = "0.17.1"
