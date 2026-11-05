package mcpserver

import (
	"crypto/rand"
	"encoding/base64"
	"sync"
	"time"

	apperr "github.com/ThomasCrouzet/restic-defensive-mcp/internal/errors"
)

const (
	cursorTokenBytes   = 24
	cursorMaxEntries   = 256
	cursorDefaultTTL   = 5 * time.Minute
	cursorStateVersion = 1
)

// cursorState is the bounded continuation state kept in memory. It never
// stores hidden metadata, credentials, repository locations, or file contents.
type cursorState struct {
	Version     int
	Tool        string
	Repository  string
	Host        string
	Tags        []string
	Path        string
	After       string
	Before      string
	Pattern     string
	IgnoreCase  bool
	Recursive   bool
	SnapshotIDs []string
	Offset      int
	LastTime    time.Time
	LastID      string
	Total       int
	Expires     time.Time
	Created     time.Time
}

type cursorStore struct {
	mu      sync.Mutex
	entries map[string]cursorState
	max     int
	ttl     time.Duration
	now     func() time.Time
}

func newCursorStore() *cursorStore {
	return &cursorStore{
		entries: make(map[string]cursorState),
		max:     cursorMaxEntries,
		ttl:     cursorDefaultTTL,
		now:     time.Now,
	}
}

func (s *cursorStore) issue(state cursorState) (string, error) {
	token, err := newCursorToken()
	if err != nil {
		return "", apperr.New(apperr.InternalError, "failed to create continuation cursor").WithCause(err)
	}
	now := s.now().UTC()
	state.Version = cursorStateVersion
	state.Created = now
	state.Expires = now.Add(s.ttl)
	state.SnapshotIDs = append([]string(nil), state.SnapshotIDs...)
	state.Tags = append([]string(nil), state.Tags...)

	s.mu.Lock()
	defer s.mu.Unlock()
	s.pruneLocked(now)
	if len(s.entries) >= s.max {
		s.evictOldestLocked()
	}
	s.entries[token] = state
	return token, nil
}

func (s *cursorStore) take(token, tool, repositoryID string) (cursorState, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	state, ok := s.entries[token]
	if !ok {
		return cursorState{}, apperr.New(apperr.CursorInvalid, "cursor is invalid or already used")
	}
	// Consume the token so a continuation cannot fork into duplicate pages.
	delete(s.entries, token)
	now := s.now().UTC()
	if !now.Before(state.Expires) {
		return cursorState{}, apperr.New(apperr.CursorInvalid, "cursor has expired")
	}
	if state.Version != cursorStateVersion || state.Tool != tool || state.Repository != repositoryID {
		return cursorState{}, apperr.New(apperr.CursorInvalid, "cursor does not match this repository or tool")
	}
	return state, nil
}

func (s *cursorStore) pruneLocked(now time.Time) {
	for token, state := range s.entries {
		if !now.Before(state.Expires) {
			delete(s.entries, token)
		}
	}
}

func (s *cursorStore) evictOldestLocked() {
	var oldestToken string
	var oldestTime time.Time
	for token, state := range s.entries {
		if oldestToken == "" || state.Created.Before(oldestTime) {
			oldestToken = token
			oldestTime = state.Created
		}
	}
	if oldestToken != "" {
		delete(s.entries, oldestToken)
	}
}

func newCursorToken() (string, error) {
	raw := make([]byte, cursorTokenBytes)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}
