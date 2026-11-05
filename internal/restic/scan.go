package restic

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"path"
	"time"

	apperr "github.com/ThomasCrouzet/restic-defensive-mcp/internal/errors"
	"github.com/ThomasCrouzet/restic-defensive-mcp/internal/policy"
	"github.com/ThomasCrouzet/restic-defensive-mcp/internal/redaction"
)

// ScanLimits bounds an incremental ls or find scan. MaxBytes caps the raw
// stdout bytes consumed from restic. MaxDecodedEntries caps the number of
// decoded JSON records retained as working state. Both are hard ceilings.
type ScanLimits struct {
	MaxBytes          int
	MaxDecodedEntries int
}

// ScanStats records bounded facts about one incremental scan. Hidden entry
// counts are deliberately not part of this type: exposing them would leak
// information about entries outside the caller's allowlist.
type ScanStats struct {
	RawBytes        int64 `json:"raw_bytes"`
	DecodedEntries  int   `json:"decoded_entries"`
	AcceptedEntries int   `json:"accepted_entries"`
	PageEntries     int   `json:"page_entries"`
	Complete        bool  `json:"complete"`
	More            bool  `json:"more"`
	StoppedEarly    bool  `json:"stopped_early"`
	BudgetExhausted bool  `json:"budget_exhausted"`
}

// LSScanResult is the bounded page produced from one restic ls stream.
type LSScanResult struct {
	Nodes []Node
	Stats ScanStats
}

// FindScanResult is the bounded page produced from one restic find stream.
type FindScanResult struct {
	Groups []FindGroup
	Stats  ScanStats
}

var errScanBudget = errors.New("scan byte budget exhausted")

// budgetReader caps the bytes an incremental decoder may consume. Once the
// ceiling is crossed it returns errScanBudget instead of silently truncating a
// JSON document into a syntactically complete value.
type budgetReader struct {
	r        io.Reader
	limit    int64
	consumed int64
	exceeded bool
}

func newBudgetReader(r io.Reader, limit int) *budgetReader {
	return &budgetReader{r: r, limit: int64(limit)}
}

func (b *budgetReader) Read(p []byte) (int, error) {
	if b.exceeded {
		return 0, errScanBudget
	}
	if b.limit > 0 && b.consumed >= b.limit {
		// Probe one byte to distinguish an exact-size document from one that
		// continues past the bound.
		var probe [1]byte
		n, err := b.r.Read(probe[:])
		if n > 0 {
			b.exceeded = true
			return 0, errScanBudget
		}
		return 0, err
	}
	if b.limit > 0 {
		remaining := b.limit - b.consumed
		if int64(len(p)) > remaining {
			p = p[:remaining]
		}
	}
	n, err := b.r.Read(p)
	b.consumed += int64(n)
	return n, err
}

func scanLimitsOrDefault(limits ScanLimits) ScanLimits {
	if limits.MaxBytes <= 0 {
		limits.MaxBytes = 16 << 20
	}
	if limits.MaxDecodedEntries <= 0 {
		limits.MaxDecodedEntries = 50000
	}
	return limits
}

// ScanLS incrementally decodes restic ls --json lines. The accept callback runs
// before page counting, so hidden entries never consume the visible page budget
// and are not retained. Scanning stops as soon as one authorized entry beyond
// the requested page proves another page exists.
func ScanLS(r io.Reader, limits ScanLimits, skip, limit int, accept func(Node) bool) (LSScanResult, error) {
	limits = scanLimitsOrDefault(limits)
	if skip < 0 {
		skip = 0
	}
	if limit < 0 {
		limit = 0
	}
	reader := newBudgetReader(r, limits.MaxBytes)
	sc := bufio.NewScanner(reader)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)

	var result LSScanResult
	remainingSkip := skip
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		if result.Stats.DecodedEntries >= limits.MaxDecodedEntries {
			result.Stats.RawBytes = reader.consumed
			result.Stats.BudgetExhausted = true
			result.Stats.StoppedEarly = true
			result.Stats.More = pageMayHaveMore(result.Stats.PageEntries, limit)
			return result, nil
		}
		var msg rawLSMessage
		if err := json.Unmarshal(line, &msg); err != nil {
			if reader.exceeded {
				result.Stats.RawBytes = reader.consumed
				result.Stats.BudgetExhausted = true
				result.Stats.StoppedEarly = true
				result.Stats.More = pageMayHaveMore(result.Stats.PageEntries, limit)
				return result, nil
			}
			return result, apperr.New(apperr.ProtocolError, "invalid ls JSON line").WithCause(err)
		}
		mt := msg.MessageType
		if mt == "" {
			mt = msg.StructType
		}
		if mt != "node" {
			continue
		}
		result.Stats.DecodedEntries++
		if msg.Path == "" {
			return result, apperr.New(apperr.ProtocolError, "ls node missing path")
		}
		cleanedPath, err := policy.CleanPath(msg.Path)
		if err != nil {
			return result, apperr.New(apperr.ProtocolError, "ls node has invalid path").WithCause(err)
		}
		node := Node{
			Name:    redaction.SanitizeString(msg.Name),
			Type:    redaction.SanitizeString(msg.Type),
			Path:    cleanedPath,
			Size:    msg.Size,
			Mode:    redaction.SanitizeString(msg.Permissions),
			ModTime: msg.Mtime.UTC(),
		}
		if node.Path == "" {
			continue
		}
		if !accept(node) {
			continue
		}
		result.Stats.AcceptedEntries++
		if remainingSkip > 0 {
			remainingSkip--
			continue
		}
		if len(result.Nodes) < limit {
			result.Nodes = append(result.Nodes, node)
			result.Stats.PageEntries = len(result.Nodes)
			continue
		}
		result.Stats.More = true
		result.Stats.StoppedEarly = true
		result.Stats.RawBytes = reader.consumed
		return result, nil
	}
	result.Stats.RawBytes = reader.consumed
	if err := sc.Err(); err != nil {
		if reader.exceeded || errors.Is(err, errScanBudget) {
			result.Stats.BudgetExhausted = true
			result.Stats.StoppedEarly = true
			result.Stats.More = pageMayHaveMore(result.Stats.PageEntries, limit)
			return result, nil
		}
		return result, apperr.New(apperr.ProtocolError, "failed reading ls output").WithCause(err)
	}
	result.Stats.Complete = true
	return result, nil
}

// ScanFind incrementally decodes restic find --json. Group objects and their
// matches arrays are consumed token by token, so the retained state is the
// requested page rather than the complete result set. The accept callback runs
// before page counting. Scanning stops once one authorized match beyond the
// page is observed.
func ScanFind(r io.Reader, limits ScanLimits, skip, limit int, accept func(snapshot string, match FindMatch) bool) (FindScanResult, error) {
	limits = scanLimitsOrDefault(limits)
	if skip < 0 {
		skip = 0
	}
	if limit < 0 {
		limit = 0
	}
	reader := newBudgetReader(r, limits.MaxBytes)
	dec := json.NewDecoder(reader)

	result := FindScanResult{}
	tok, err := dec.Token()
	if err == io.EOF {
		result.Stats.RawBytes = reader.consumed
		result.Stats.Complete = true
		return result, nil
	}
	if err != nil {
		if isScanBudget(reader, err) {
			return finishFindBudget(result, reader, limit), nil
		}
		return result, apperr.New(apperr.ProtocolError, "invalid find JSON").WithCause(err)
	}
	if delim, ok := tok.(json.Delim); !ok || delim != '[' {
		return result, apperr.New(apperr.ProtocolError, "invalid find JSON")
	}

	remainingSkip := skip
	for dec.More() {
		if result.Stats.DecodedEntries >= limits.MaxDecodedEntries {
			return finishFindBudget(result, reader, limit), nil
		}
		tok, err := dec.Token()
		if err != nil {
			if isScanBudget(reader, err) {
				return finishFindBudget(result, reader, limit), nil
			}
			return result, apperr.New(apperr.ProtocolError, "invalid find group").WithCause(err)
		}
		if delim, ok := tok.(json.Delim); !ok || delim != '{' {
			return result, apperr.New(apperr.ProtocolError, "invalid find group")
		}
		result.Stats.DecodedEntries++
		group := FindGroup{}
		var pendingMatches json.RawMessage
		stopped := false

		for dec.More() {
			keyTok, err := dec.Token()
			if err != nil {
				if isScanBudget(reader, err) {
					markFindBudget(&result.Stats, len(group.Matches), limit)
					stopped = true
					break
				}
				return result, apperr.New(apperr.ProtocolError, "invalid find group").WithCause(err)
			}
			key, ok := keyTok.(string)
			if !ok {
				return result, apperr.New(apperr.ProtocolError, "invalid find group")
			}
			switch key {
			case "snapshot":
				var snapshot string
				if err := dec.Decode(&snapshot); err != nil {
					if isScanBudget(reader, err) {
						markFindBudget(&result.Stats, len(group.Matches), limit)
						stopped = true
						break
					}
					return result, apperr.New(apperr.ProtocolError, "invalid find group").WithCause(err)
				}
				if !ValidSnapshotID(snapshot) || snapshot == "latest" {
					return result, apperr.New(apperr.ProtocolError, "find group has invalid snapshot id")
				}
				group.Snapshot = snapshot
			case "hits":
				var hits uint64
				if err := dec.Decode(&hits); err != nil {
					if isScanBudget(reader, err) {
						markFindBudget(&result.Stats, len(group.Matches), limit)
						stopped = true
						break
					}
					return result, apperr.New(apperr.ProtocolError, "invalid find group").WithCause(err)
				}
			case "matches":
				if group.Snapshot == "" {
					if err := dec.Decode(&pendingMatches); err != nil {
						if isScanBudget(reader, err) {
							markFindBudget(&result.Stats, len(group.Matches), limit)
							stopped = true
							break
						}
						return result, apperr.New(apperr.ProtocolError, "invalid find matches").WithCause(err)
					}
					continue
				}
				stopped, err = scanFindMatchesDecoder(dec, reader, &group, limits, &result.Stats, &remainingSkip, limit, accept)
				if err != nil {
					return result, err
				}
			default:
				var discard json.RawMessage
				if err := dec.Decode(&discard); err != nil {
					if isScanBudget(reader, err) {
						markFindBudget(&result.Stats, len(group.Matches), limit)
						stopped = true
						break
					}
					return result, apperr.New(apperr.ProtocolError, "invalid find group").WithCause(err)
				}
			}
			if stopped {
				break
			}
		}
		if stopped {
			appendFindGroup(&result, group)
			result.Stats.RawBytes = reader.consumed
			result.Stats.StoppedEarly = true
			return result, nil
		}
		if _, err := dec.Token(); err != nil {
			if isScanBudget(reader, err) {
				markFindBudget(&result.Stats, len(group.Matches), limit)
				appendFindGroup(&result, group)
				result.Stats.RawBytes = reader.consumed
				result.Stats.StoppedEarly = true
				return result, nil
			}
			return result, apperr.New(apperr.ProtocolError, "invalid find group").WithCause(err)
		}
		if group.Snapshot == "" {
			return result, apperr.New(apperr.ProtocolError, "find group has invalid snapshot id")
		}
		matchesRaw := bytes.TrimSpace(pendingMatches)
		if len(matchesRaw) > 0 && !bytes.Equal(matchesRaw, []byte("null")) {
			pendingDecoder := json.NewDecoder(bytes.NewReader(matchesRaw))
			stopped, err = scanFindMatchesDecoder(pendingDecoder, reader, &group, limits, &result.Stats, &remainingSkip, limit, accept)
			if err != nil {
				return result, err
			}
			if stopped {
				appendFindGroup(&result, group)
				result.Stats.RawBytes = reader.consumed
				result.Stats.StoppedEarly = true
				return result, nil
			}
		}
		appendFindGroup(&result, group)
	}

	if _, err := dec.Token(); err != nil {
		if isScanBudget(reader, err) {
			return finishFindBudget(result, reader, limit), nil
		}
		return result, apperr.New(apperr.ProtocolError, "truncated find JSON").WithCause(err)
	}
	if _, err := dec.Token(); err != io.EOF {
		if err == nil {
			return result, apperr.New(apperr.ProtocolError, "trailing data after find JSON")
		}
		return result, apperr.New(apperr.ProtocolError, "trailing data after find JSON").WithCause(err)
	}
	result.Stats.RawBytes = reader.consumed
	result.Stats.Complete = true
	return result, nil
}

func isScanBudget(reader *budgetReader, err error) bool {
	return reader.exceeded || errors.Is(err, errScanBudget)
}

func markFindBudget(stats *ScanStats, extra, limit int) {
	stats.BudgetExhausted = true
	stats.More = pageMayHaveMore(stats.PageEntries+extra, limit)
}

func appendFindGroup(result *FindScanResult, group FindGroup) {
	if len(group.Matches) == 0 {
		return
	}
	group.Hits = uint64(len(group.Matches))
	result.Groups = append(result.Groups, group)
	result.Stats.PageEntries += len(group.Matches)
}

func scanFindMatchesDecoder(
	dec *json.Decoder,
	reader *budgetReader,
	group *FindGroup,
	limits ScanLimits,
	stats *ScanStats,
	remainingSkip *int,
	limit int,
	accept func(snapshot string, match FindMatch) bool,
) (bool, error) {
	tok, err := dec.Token()
	if err != nil {
		if isScanBudget(reader, err) {
			markFindBudget(stats, len(group.Matches), limit)
			return true, nil
		}
		return false, apperr.New(apperr.ProtocolError, "invalid find matches").WithCause(err)
	}
	if delim, ok := tok.(json.Delim); !ok || delim != '[' {
		return false, apperr.New(apperr.ProtocolError, "invalid find matches")
	}
	for dec.More() {
		if stats.DecodedEntries >= limits.MaxDecodedEntries {
			stats.BudgetExhausted = true
			stats.More = pageMayHaveMore(stats.PageEntries+len(group.Matches), limit)
			return true, nil
		}
		var rawMatch struct {
			Path  string    `json:"path"`
			Name  string    `json:"name"`
			Type  string    `json:"type"`
			Size  uint64    `json:"size"`
			Mtime time.Time `json:"mtime"`
		}
		if err := dec.Decode(&rawMatch); err != nil {
			if isScanBudget(reader, err) {
				stats.BudgetExhausted = true
				stats.More = pageMayHaveMore(stats.PageEntries+len(group.Matches), limit)
				return true, nil
			}
			return false, apperr.New(apperr.ProtocolError, "invalid find match").WithCause(err)
		}
		stats.DecodedEntries++
		if rawMatch.Path == "" {
			return false, apperr.New(apperr.ProtocolError, "find match missing path")
		}
		cleanedPath, err := policy.CleanPath(rawMatch.Path)
		if err != nil {
			return false, apperr.New(apperr.ProtocolError, "find match has invalid path").WithCause(err)
		}
		name := rawMatch.Name
		if name == "" {
			name = path.Base(cleanedPath)
		}
		match := FindMatch{
			Path:  cleanedPath,
			Name:  redaction.SanitizeString(name),
			Type:  redaction.SanitizeString(rawMatch.Type),
			Size:  rawMatch.Size,
			Mtime: rawMatch.Mtime.UTC(),
		}
		if !accept(group.Snapshot, match) {
			continue
		}
		stats.AcceptedEntries++
		if *remainingSkip > 0 {
			*remainingSkip = *remainingSkip - 1
			continue
		}
		if len(group.Matches) < limit {
			group.Matches = append(group.Matches, match)
			continue
		}
		stats.More = true
		return true, nil
	}
	if _, err := dec.Token(); err != nil {
		if isScanBudget(reader, err) {
			markFindBudget(stats, len(group.Matches), limit)
			return true, nil
		}
		return false, apperr.New(apperr.ProtocolError, "invalid find matches").WithCause(err)
	}
	return false, nil
}

func pageMayHaveMore(pageEntries, limit int) bool {
	return limit > 0 && pageEntries >= limit
}

func finishFindBudget(result FindScanResult, reader *budgetReader, limit int) FindScanResult {
	result.Stats.RawBytes = reader.consumed
	result.Stats.BudgetExhausted = true
	result.Stats.StoppedEarly = true
	result.Stats.More = pageMayHaveMore(result.Stats.PageEntries, limit)
	return result
}
