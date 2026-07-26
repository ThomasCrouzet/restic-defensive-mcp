// Package redaction neutralizes control characters and strips sensitive patterns.
package redaction

import (
	"regexp"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"
)

const replacement = "\uFFFD"

// Control and terminal escape sequences that must never reach callers or logs.
var (
	ansiEscape = regexp.MustCompile(`\x1b\[[0-9;?]*[a-zA-Z]|\x1b\][^\x07]*(?:\x07|\x1b\\)|\x1b.`)
	// Common secret-bearing fragments that may appear in restic stderr.
	secretPatterns = []*regexp.Regexp{
		regexp.MustCompile(`(?i)(password|passwd|secret|token|access[_-]?key|secret[_-]?access[_-]?key)\s*[:=]\s*\S+`),
		regexp.MustCompile(`(?i)AWS_SECRET_ACCESS_KEY=\S+`),
		regexp.MustCompile(`(?i)B2_ACCOUNT_KEY=\S+`),
		regexp.MustCompile(`(?i)RESTIC_PASSWORD=\S+`),
		regexp.MustCompile(`(?i)Authorization:\s*\S+`),
	}
)

// SanitizeString removes control characters (except tab/newline), strips ANSI,
// and replaces invalid UTF-8. It does not truncate.
func SanitizeString(s string) string {
	if s == "" {
		return s
	}
	s = ansiEscape.ReplaceAllString(s, "")
	if !utf8.ValidString(s) {
		s = strings.ToValidUTF8(s, replacement)
	}
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch {
		case r == '\t' || r == '\n':
			b.WriteRune(r)
		case r == '\r':
			// drop bare CR
		case unicode.IsControl(r):
			b.WriteString(replacement)
		case r == unicode.ReplacementChar:
			b.WriteString(replacement)
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// RedactSecrets replaces known secret-bearing patterns with [REDACTED].
func RedactSecrets(s string) string {
	out := s
	for _, re := range secretPatterns {
		out = re.ReplaceAllString(out, "[REDACTED]")
	}
	return out
}

// RedactValues removes exact sensitive values, longest first so overlapping
// values cannot leave a suffix behind.
func RedactValues(s string, values ...string) string {
	filtered := make([]string, 0, len(values))
	for _, value := range values {
		value = SanitizeString(value)
		if value != "" {
			filtered = append(filtered, value)
		}
	}
	sort.Slice(filtered, func(i, j int) bool {
		return len(filtered[i]) > len(filtered[j])
	})
	out := s
	for _, value := range filtered {
		out = strings.ReplaceAll(out, value, "[REDACTED]")
	}
	return out
}

// SanitizeAndRedact applies both sanitization and secret redaction.
func SanitizeAndRedact(s string) string {
	return RedactSecrets(SanitizeString(s))
}

// Clip bounds a string to max runes, appending an ellipsis marker when truncated.
func Clip(s string, max int) string {
	if max <= 0 {
		return ""
	}
	ellipsisCut := max - 3
	if max < 3 {
		ellipsisCut = max
	}
	runes := 0
	cut := 0
	for byteIndex := range s {
		if runes == ellipsisCut {
			cut = byteIndex
		}
		if runes == max {
			if max < 3 {
				return s[:byteIndex]
			}
			return s[:cut] + "..."
		}
		runes++
	}
	return s
}
