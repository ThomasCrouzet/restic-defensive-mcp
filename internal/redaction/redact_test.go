package redaction

import (
	"strings"
	"testing"
)

func TestSanitizeControlChars(t *testing.T) {
	in := "hello\x1b[31mred\x00world\n"
	out := SanitizeString(in)
	if containsCtrl(out) && !containsOnlyAllowed(out) {
		t.Fatalf("still has bad controls: %q", out)
	}
	if out == in {
		t.Fatal("expected change")
	}
}

func TestRedactSecrets(t *testing.T) {
	in := "password: supersecret AWS_SECRET_ACCESS_KEY=abc123"
	out := RedactSecrets(in)
	if strings.Contains(out, "supersecret") || strings.Contains(out, "abc123") {
		t.Fatalf("not redacted: %q", out)
	}
}

func TestRedactValues(t *testing.T) {
	out := RedactValues(
		"repository rest:https://user:pass@example.test/backups failed",
		"rest:https://user:pass@example.test/backups",
		"user:pass",
	)
	if strings.Contains(out, "example.test") || strings.Contains(out, "user:pass") {
		t.Fatalf("exact sensitive value leaked: %s", out)
	}
}

func TestClip(t *testing.T) {
	if Clip("abcdef", 4) != "a..." {
		t.Fatalf("got %q", Clip("abcdef", 4))
	}
	if Clip("ééééé", 4) != "é..." {
		t.Fatalf("unicode clip: %q", Clip("ééééé", 4))
	}
	if Clip("éé", 2) != "éé" {
		t.Fatalf("exact rune limit: %q", Clip("éé", 2))
	}
}

func FuzzSanitize(f *testing.F) {
	f.Add("normal")
	f.Add("\x1b[0mpassword=x")
	f.Fuzz(func(t *testing.T, s string) {
		_ = SanitizeAndRedact(s)
	})
}

func containsCtrl(s string) bool {
	for _, r := range s {
		if r < 32 && r != '\t' && r != '\n' {
			return true
		}
	}
	return false
}

func containsOnlyAllowed(s string) bool {
	for _, r := range s {
		if r == '\t' || r == '\n' || r >= 32 {
			continue
		}
		return false
	}
	return true
}
