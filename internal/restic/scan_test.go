package restic

import (
	"strings"
	"testing"

	apperr "github.com/ThomasCrouzet/restic-defensive-mcp/internal/errors"
)

func lsLine(path string) string {
	return `{"message_type":"node","name":"` + path[strings.LastIndex(path, "/")+1:] +
		`","type":"file","path":"` + path + `","size":1,"permissions":"-rw-------","mtime":"2024-01-01T00:00:00Z"}` + "\n"
}

func TestScanLSAppliesVisibilityBeforePageLimit(t *testing.T) {
	var input strings.Builder
	for i := 0; i < 20; i++ {
		input.WriteString(lsLine("/hidden/secret-" + string(rune('a'+i)) + ".txt"))
	}
	input.WriteString(lsLine("/data/one.txt"))
	input.WriteString(lsLine("/data/two.txt"))
	input.WriteString(lsLine("/data/three.txt"))

	result, err := ScanLS(strings.NewReader(input.String()), ScanLimits{MaxBytes: 1 << 20, MaxDecodedEntries: 1000}, 0, 2, func(node Node) bool {
		return strings.HasPrefix(node.Path, "/data/")
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Nodes) != 2 {
		t.Fatalf("page nodes = %d, want 2", len(result.Nodes))
	}
	if result.Nodes[0].Path != "/data/one.txt" || result.Nodes[1].Path != "/data/two.txt" {
		t.Fatalf("hidden entries consumed page budget: %+v", result.Nodes)
	}
	if !result.Stats.More || !result.Stats.StoppedEarly {
		t.Fatalf("page boundary not detected: %+v", result.Stats)
	}
	if result.Stats.AcceptedEntries != 3 {
		t.Fatalf("accepted entries = %d, want 3", result.Stats.AcceptedEntries)
	}
}

func TestScanLSContinuesFromVisibleOffset(t *testing.T) {
	var input strings.Builder
	for i := 0; i < 5; i++ {
		input.WriteString(lsLine("/hidden/" + string(rune('a'+i))))
	}
	for i := 0; i < 5; i++ {
		input.WriteString(lsLine("/data/" + string(rune('a'+i))))
	}
	result, err := ScanLS(strings.NewReader(input.String()), ScanLimits{}, 2, 2, func(node Node) bool {
		return strings.HasPrefix(node.Path, "/data/")
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Nodes) != 2 || result.Nodes[0].Path != "/data/c" || result.Nodes[1].Path != "/data/d" {
		t.Fatalf("unexpected offset page: %+v", result.Nodes)
	}
	if !result.Stats.More {
		t.Fatal("expected another page")
	}
}

func TestScanLSStopsReadingAtPageBoundary(t *testing.T) {
	var input strings.Builder
	for i := 0; i < 4000; i++ {
		input.WriteString(lsLine("/data/file-" + strings.Repeat("x", i%40) + "-" + string(rune('a'+i%26))))
	}
	all := input.String()
	result, err := ScanLS(strings.NewReader(all), ScanLimits{MaxBytes: len(all), MaxDecodedEntries: 100000}, 0, 2, func(Node) bool { return true })
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Nodes) != 2 || !result.Stats.More {
		t.Fatalf("unexpected page: nodes=%d stats=%+v", len(result.Nodes), result.Stats)
	}
	if result.Stats.RawBytes >= int64(len(all)) {
		t.Fatalf("scan read the full stream (%d bytes) instead of stopping at the boundary", result.Stats.RawBytes)
	}
}

func TestScanLSByteBudgetIsIncompleteNotSuccess(t *testing.T) {
	var input strings.Builder
	for i := 0; i < 200; i++ {
		input.WriteString(lsLine("/data/file-" + strings.Repeat("y", 100) + "-" + string(rune('a'+i%26))))
	}
	result, err := ScanLS(strings.NewReader(input.String()), ScanLimits{MaxBytes: 512, MaxDecodedEntries: 100000}, 0, 10, func(Node) bool { return true })
	if err != nil {
		t.Fatal(err)
	}
	if !result.Stats.BudgetExhausted || result.Stats.Complete {
		t.Fatalf("byte budget must be reported as incomplete: %+v", result.Stats)
	}
	if len(result.Nodes) >= 10 {
		t.Fatalf("bounded scan retained too many nodes: %d", len(result.Nodes))
	}
}

func TestScanLSDecodedEntryBudgetBoundsWorkingState(t *testing.T) {
	var input strings.Builder
	for i := 0; i < 100; i++ {
		input.WriteString(lsLine("/data/file-" + string(rune('a'+i%26))))
	}
	result, err := ScanLS(strings.NewReader(input.String()), ScanLimits{MaxBytes: 1 << 20, MaxDecodedEntries: 3}, 0, 10, func(Node) bool { return true })
	if err != nil {
		t.Fatal(err)
	}
	if !result.Stats.BudgetExhausted {
		t.Fatalf("decoded entry budget not reported: %+v", result.Stats)
	}
	if result.Stats.DecodedEntries > 3 {
		t.Fatalf("decoded entries exceed budget: %d", result.Stats.DecodedEntries)
	}
}

func TestScanLSRejectsMalformedLine(t *testing.T) {
	_, err := ScanLS(strings.NewReader(lsLine("/data/ok")+"{not-json}\n"), ScanLimits{}, 0, 10, func(Node) bool { return true })
	if !apperr.IsCode(err, apperr.ProtocolError) {
		t.Fatalf("malformed line error = %v, want protocol_error", err)
	}
}

func TestScanFindFiltersBeforePageLimit(t *testing.T) {
	input := `[
		{"hits":20,"snapshot":"abcdef0123456789","matches":[
			{"path":"/hidden/a","type":"file","size":1},
			{"path":"/hidden/b","type":"file","size":1}
		]},
		{"hits":3,"snapshot":"abcdef0123456789","matches":[
			{"path":"/data/one.txt","type":"file","size":1},
			{"path":"/data/two.txt","type":"file","size":1},
			{"path":"/data/three.txt","type":"file","size":1}
		]}
	]`
	result, err := ScanFind(strings.NewReader(input), ScanLimits{}, 0, 2, func(_ string, match FindMatch) bool {
		return strings.HasPrefix(match.Path, "/data/")
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Stats.PageEntries != 2 || len(result.Groups) != 1 || len(result.Groups[0].Matches) != 2 {
		t.Fatalf("unexpected page: %+v", result)
	}
	if !result.Stats.More || !result.Stats.StoppedEarly {
		t.Fatalf("page boundary not detected: %+v", result.Stats)
	}
	if result.Groups[0].Matches[0].Path != "/data/one.txt" {
		t.Fatalf("hidden match consumed page budget: %+v", result.Groups)
	}
}

func TestScanFindReportsCompleteTotal(t *testing.T) {
	input := `[{"hits":3,"snapshot":"abcdef0123456789","matches":[
		{"path":"/data/a","type":"file","size":1},
		{"path":"/data/b","type":"file","size":1},
		{"path":"/data/c","type":"file","size":1}
	]}]`
	result, err := ScanFind(strings.NewReader(input), ScanLimits{}, 0, 10, func(string, FindMatch) bool { return true })
	if err != nil {
		t.Fatal(err)
	}
	if !result.Stats.Complete || result.Stats.AcceptedEntries != 3 || result.Stats.PageEntries != 3 {
		t.Fatalf("complete scan stats: %+v", result.Stats)
	}
}

func TestScanFindHandlesMatchesBeforeSnapshot(t *testing.T) {
	input := `[{"hits":2,"matches":[
		{"path":"/data/a","type":"file","size":1},
		{"path":"/data/b","type":"file","size":1}
	],"snapshot":"abcdef0123456789"}]`
	result, err := ScanFind(strings.NewReader(input), ScanLimits{}, 0, 10, func(string, FindMatch) bool { return true })
	if err != nil {
		t.Fatal(err)
	}
	if !result.Stats.Complete || result.Stats.AcceptedEntries != 2 || len(result.Groups) != 1 {
		t.Fatalf("reordered group not parsed: %+v", result)
	}
}

func TestScanFindRejectsTruncatedJSON(t *testing.T) {
	_, err := ScanFind(strings.NewReader(`[{"hits":1,"snapshot":"abcdef0123456789","matches":[`), ScanLimits{}, 0, 10, func(string, FindMatch) bool { return true })
	if !apperr.IsCode(err, apperr.ProtocolError) {
		t.Fatalf("truncated find error = %v, want protocol_error", err)
	}
}

func TestScanFindDecodedEntryBudgetBoundsHostileGroup(t *testing.T) {
	var matches strings.Builder
	for i := 0; i < 500; i++ {
		if i > 0 {
			matches.WriteString(",")
		}
		matches.WriteString(`{"path":"/data/f`)
		matches.WriteString(strings.Repeat("z", 100))
		matches.WriteString(`","type":"file","size":1}`)
	}
	input := `[{"hits":500,"snapshot":"abcdef0123456789","matches":[` + matches.String() + `]}]`
	result, err := ScanFind(strings.NewReader(input), ScanLimits{MaxBytes: 1 << 20, MaxDecodedEntries: 4}, 0, 10, func(string, FindMatch) bool { return true })
	if err != nil {
		t.Fatal(err)
	}
	if !result.Stats.BudgetExhausted || result.Stats.Complete {
		t.Fatalf("hostile group not bounded: %+v", result.Stats)
	}
	if result.Stats.DecodedEntries > 5 {
		t.Fatalf("decoded entries exceed budget: %d", result.Stats.DecodedEntries)
	}
	if len(result.Groups) > 1 || len(result.Groups[0].Matches) > 4 {
		t.Fatalf("retained state exceeds page budget: %+v", result.Groups)
	}
}

func TestScanFindByteBudgetIsIncomplete(t *testing.T) {
	var input strings.Builder
	input.WriteString(`[{"hits":1,"snapshot":"abcdef0123456789","matches":[`)
	for i := 0; i < 200; i++ {
		if i > 0 {
			input.WriteString(",")
		}
		input.WriteString(`{"path":"/data/`)
		input.WriteString(strings.Repeat("a", 100))
		input.WriteString(`","type":"file","size":1}`)
	}
	input.WriteString(`]}]`)
	result, err := ScanFind(strings.NewReader(input.String()), ScanLimits{MaxBytes: 512, MaxDecodedEntries: 100000}, 0, 10, func(string, FindMatch) bool { return true })
	if err != nil {
		t.Fatal(err)
	}
	if !result.Stats.BudgetExhausted || result.Stats.Complete {
		t.Fatalf("find byte budget must be incomplete: %+v", result.Stats)
	}
}
