package mcpserver

import (
	"testing"
	"time"

	apperr "github.com/ThomasCrouzet/restic-defensive-mcp/internal/errors"
)

func TestTimeWindowBoundariesAndOffsets(t *testing.T) {
	after := "2026-01-01T00:00:00+05:30"
	before := "2026-01-01T00:00:00-02:00"
	window, err := parseTimeWindow(after, before)
	if err != nil {
		t.Fatal(err)
	}
	afterUTC, _ := time.Parse(time.RFC3339, after)
	beforeUTC, _ := time.Parse(time.RFC3339, before)
	if window.contains(afterUTC.Add(-time.Nanosecond)) {
		t.Fatal("instant before after was included")
	}
	if !window.contains(afterUTC) {
		t.Fatal("after boundary must be inclusive")
	}
	if !window.contains(beforeUTC.Add(-time.Nanosecond)) {
		t.Fatal("instant before before was excluded")
	}
	if window.contains(beforeUTC) {
		t.Fatal("before boundary must be exclusive")
	}
}

func TestTimeWindowRejectsInvalidReversedAndEmptyIntervals(t *testing.T) {
	cases := []struct{ after, before string }{
		{after: "yesterday"},
		{before: "2026-01-01"},
		{after: "2026-01-02T00:00:00Z", before: "2026-01-01T00:00:00Z"},
		{after: "2026-01-01T00:00:00Z", before: "2026-01-01T00:00:00Z"},
	}
	for _, testCase := range cases {
		if _, err := parseTimeWindow(testCase.after, testCase.before); !apperr.IsCode(err, apperr.InvalidArgument) {
			t.Fatalf("after=%q before=%q error = %v, want invalid_argument", testCase.after, testCase.before, err)
		}
	}
}

func TestTimeWindowNormalization(t *testing.T) {
	window, err := parseTimeWindow("2026-01-01T05:30:00+05:30", "")
	if err != nil {
		t.Fatal(err)
	}
	after, before := window.normalized()
	if after != "2026-01-01T00:00:00Z" || before != "" {
		t.Fatalf("normalized = %q, %q", after, before)
	}
}
