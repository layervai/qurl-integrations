package internal

import "strings"

// sanitizeLogValue neutralizes CR/LF (go/log-injection). It preserves diagnostic context while preventing a
// Slack- or API-controlled value from forging a second plain-text log entry.
// Production uses the shared JSON slog handler, which also escapes control
// bytes; this remains a defense-in-depth boundary if the handler changes.
func sanitizeLogValue(value string) string {
	value = strings.ReplaceAll(value, "\r", `\r`)
	return strings.ReplaceAll(value, "\n", `\n`)
}
