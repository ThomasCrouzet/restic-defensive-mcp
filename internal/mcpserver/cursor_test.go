package mcpserver

import (
	"testing"
	"time"

	apperr "github.com/ThomasCrouzet/restic-defensive-mcp/internal/errors"
)

func TestCursorStoreRoundTripAndSingleUse(t *testing.T) {
	store := newCursorStore()
	state := cursorState{
		Tool:        "browse_snapshot",
		Repository:  "repo",
		Path:        "/srv/data",
		SnapshotIDs: []string{"aaaaaaaa"},
		Offset:      25,
	}
	token, err := store.issue(state)
	if err != nil {
		t.Fatal(err)
	}
	got, err := store.take(token, "browse_snapshot", "repo")
	if err != nil {
		t.Fatal(err)
	}
	if got.Offset != 25 || got.Path != "/srv/data" || len(got.SnapshotIDs) != 1 {
		t.Fatalf("state = %+v", got)
	}
	if _, err := store.take(token, "browse_snapshot", "repo"); !apperr.IsCode(err, apperr.CursorInvalid) {
		t.Fatalf("second take = %v, want cursor_invalid", err)
	}
}

func TestCursorStoreRejectsExpiredTamperedAndMismatched(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	store := newCursorStore()
	store.now = func() time.Time { return now }
	store.ttl = time.Minute
	token, err := store.issue(cursorState{Tool: "find_files", Repository: "repo"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.take("not-a-real-token", "find_files", "repo"); !apperr.IsCode(err, apperr.CursorInvalid) {
		t.Fatalf("tampered token = %v, want cursor_invalid", err)
	}
	if _, err := store.take(token, "browse_snapshot", "repo"); !apperr.IsCode(err, apperr.CursorInvalid) {
		t.Fatalf("wrong tool = %v, want cursor_invalid", err)
	}

	expiring, err := store.issue(cursorState{Tool: "find_files", Repository: "repo"})
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(2 * time.Minute)
	if _, err := store.take(expiring, "find_files", "repo"); !apperr.IsCode(err, apperr.CursorInvalid) {
		t.Fatalf("expired token = %v, want cursor_invalid", err)
	}
}

func TestCursorStoreBoundsEntries(t *testing.T) {
	store := newCursorStore()
	store.max = 2
	first, err := store.issue(cursorState{Tool: "find_files", Repository: "repo"})
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(time.Millisecond)
	if _, err := store.issue(cursorState{Tool: "find_files", Repository: "repo"}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(time.Millisecond)
	if _, err := store.issue(cursorState{Tool: "find_files", Repository: "repo"}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.take(first, "find_files", "repo"); !apperr.IsCode(err, apperr.CursorInvalid) {
		t.Fatalf("evicted token = %v, want cursor_invalid", err)
	}
	if len(store.entries) != 2 {
		t.Fatalf("entries = %d, want 2", len(store.entries))
	}
}
