// Package audit writes structured, non-sensitive operational events to stderr.
package audit

import (
	"encoding/json"
	"io"
	"os"
	"sync"
	"time"

	"github.com/ThomasCrouzet/restic-defensive-mcp/internal/redaction"
)

// Event is a single audit record. It must never contain secrets, full file paths,
// repository URLs, or raw restic argv that embeds secrets.
type Event struct {
	Time         time.Time `json:"time"`
	Action       string    `json:"action"`
	RepositoryID string    `json:"repository_id,omitempty"`
	Tool         string    `json:"tool,omitempty"`
	Cost         string    `json:"cost,omitempty"`
	Status       string    `json:"status"` // ok | error | denied
	ErrorCode    string    `json:"error_code,omitempty"`
	DurationMS   int64     `json:"duration_ms,omitempty"`
	Message      string    `json:"message,omitempty"`
}

// Logger writes JSON lines to an io.Writer (typically stderr).
type Logger struct {
	mu  sync.Mutex
	out io.Writer
}

// New creates a logger writing to w. If w is nil, os.Stderr is used.
func New(w io.Writer) *Logger {
	if w == nil {
		w = os.Stderr
	}
	return &Logger{out: w}
}

// Log writes one event. Failures are silently ignored to avoid breaking tools.
func (l *Logger) Log(ev Event) {
	if l == nil {
		return
	}
	ev.Time = time.Now().UTC()
	ev.Action = redaction.Clip(redaction.SanitizeString(ev.Action), 64)
	ev.RepositoryID = redaction.Clip(redaction.SanitizeAndRedact(ev.RepositoryID), 128)
	ev.Tool = redaction.Clip(redaction.SanitizeString(ev.Tool), 64)
	ev.Cost = redaction.Clip(redaction.SanitizeString(ev.Cost), 32)
	ev.Status = redaction.Clip(redaction.SanitizeString(ev.Status), 32)
	ev.ErrorCode = redaction.Clip(redaction.SanitizeString(ev.ErrorCode), 64)
	ev.Message = redaction.Clip(redaction.SanitizeAndRedact(ev.Message), 512)

	b, err := json.Marshal(ev)
	if err != nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	_, _ = l.out.Write(append(b, '\n'))
}
