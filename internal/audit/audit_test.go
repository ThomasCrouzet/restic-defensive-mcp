package audit_test

import (
	"bytes"
	"strings"
	"testing"

	"github.com/ThomasCrouzet/restic-defensive-mcp/internal/audit"
)

func TestLogSanitizesAndWritesJSONLine(t *testing.T) {
	var buf bytes.Buffer
	l := audit.New(&buf)
	l.Log(audit.Event{
		Action:       "tool_rejected",
		Tool:         "browse_snapshot",
		RepositoryID: "local-test",
		Status:       "denied",
		ErrorCode:    "not_allowed",
		Message:      "password=supersecret path denial",
	})
	out := buf.String()
	if !strings.HasSuffix(out, "\n") {
		t.Fatalf("expected newline-terminated JSON: %q", out)
	}
	if !strings.Contains(out, `"action":"tool_rejected"`) {
		t.Fatalf("missing action: %s", out)
	}
	if strings.Contains(out, "supersecret") {
		t.Fatalf("secret not redacted: %s", out)
	}
	if !strings.Contains(out, "[REDACTED]") && !strings.Contains(out, "password") {
		// RedactSecrets should replace password=... patterns
		t.Fatalf("expected redaction marker or stripped secret: %s", out)
	}
}

func TestNilLoggerSafe(t *testing.T) {
	var l *audit.Logger
	l.Log(audit.Event{Action: "x", Status: "ok"}) // must not panic
}

func TestLogBoundsUntrustedFields(t *testing.T) {
	var buf bytes.Buffer
	audit.New(&buf).Log(audit.Event{
		Action:       "tool_rejected",
		RepositoryID: strings.Repeat("x", 10_000),
		Status:       strings.Repeat("y", 10_000),
		Message:      strings.Repeat("z", 10_000),
	})
	if buf.Len() > 1_000 {
		t.Fatalf("audit event was not bounded: %d bytes", buf.Len())
	}
}
